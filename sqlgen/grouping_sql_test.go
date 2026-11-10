package sqlgen_test

import (
	"testing"

	"github.com/doug-martin/goqu/v9"
	"github.com/doug-martin/goqu/v9/exp"
	"github.com/doug-martin/goqu/v9/internal/sb"
	"github.com/doug-martin/goqu/v9/sqlgen"
	"github.com/stretchr/testify/suite"
)

type groupingSQLGeneratorSuite struct {
	baseSQLGeneratorSuite
}

func (s *groupingSQLGeneratorSuite) assertCases(ssg sqlgen.SelectSQLGenerator, testCases ...selectTestCase) {
	for _, tc := range testCases {
		b := sb.NewSQLBuilder(tc.isPrepared)
		ssg.Generate(b, tc.clause)
		switch {
		case tc.err != "":
			s.assertErrorSQL(b, tc.err)
		case tc.isPrepared:
			s.assertPreparedSQL(b, tc.sql, tc.args)
		default:
			s.assertNotPreparedSQL(b, tc.sql)
		}
	}
}

func groupingClauses() exp.SelectClauses {
	return exp.NewSelectClauses().
		SetFrom(exp.NewColumnListExpression("t")).
		SetSelect(exp.NewColumnListExpression(
			exp.ParseIdentifier("a"),
			exp.ParseIdentifier("b"),
			exp.NewSQLFunctionExpression("COUNT", exp.Star()),
			exp.NewGroupingIDExpression("a", "b").As("gid"),
		)).
		SetGroupingSets(exp.NewGroupingSetsExpression(0,
			exp.NewGroupingSetExpression("a", "b"),
			exp.NewGroupingSetExpression("a"),
			exp.NewGroupingSetExpression(),
		))
}

func nativeOpts() *sqlgen.SQLDialectOptions {
	opts := sqlgen.DefaultDialectOptions()
	opts.SupportsGroupingSets = true
	return opts
}

func (s *groupingSQLGeneratorSuite) TestNativeGroupingSets() {
	s.assertCases(
		sqlgen.NewSelectSQLGenerator("test", nativeOpts()),
		selectTestCase{
			clause: groupingClauses(),
			sql:    `SELECT "a", "b", COUNT(*), GROUPING("a", "b") AS "gid" FROM "t" GROUP BY GROUPING SETS (("a", "b"), ("a"), ())`,
		},
		selectTestCase{
			clause: func() exp.SelectClauses {
				return groupingClauses().WhereAppend(exp.ParseIdentifier("x").Eq(1))
			}(),
			sql: `SELECT "a", "b", COUNT(*), GROUPING("a", "b") AS "gid" FROM "t" WHERE ("x" = ?) ` +
				`GROUP BY GROUPING SETS (("a", "b"), ("a"), ())`,
			isPrepared: true,
			args:       []interface{}{int64(1)},
		},
	)
}

func (s *groupingSQLGeneratorSuite) TestNativeGroupingSetsBudgetAndConflicts() {
	overBudget := exp.NewSelectClauses().
		SetFrom(exp.NewColumnListExpression("t")).
		SetSelect(exp.NewColumnListExpression(exp.ParseIdentifier("a"))).
		SetGroupingSets(exp.NewGroupingSetsExpression(2,
			exp.NewGroupingSetExpression("a"),
			exp.NewGroupingSetExpression("b"),
			exp.NewGroupingSetExpression(),
		))

	emptySets := exp.NewSelectClauses().
		SetFrom(exp.NewColumnListExpression("t")).
		SetSelect(exp.NewColumnListExpression(exp.ParseIdentifier("a"))).
		SetGroupingSets(exp.NewGroupingSetsExpression(0))

	withGroupBy := groupingClauses().SetGroupBy(exp.NewColumnListExpression("a"))

	s.assertCases(
		sqlgen.NewSelectSQLGenerator("test", nativeOpts()),
		selectTestCase{
			clause: overBudget,
			err:    "goqu: GROUPING SETS branch budget exceeded: 3 branches > 2, declare fewer sets or raise the budget",
		},
		selectTestCase{
			clause: emptySets,
			err:    "goqu: GROUPING SETS requires at least one grouping set",
		},
		selectTestCase{
			clause: withGroupBy,
			err:    "goqu: GROUPING SETS cannot be combined with a plain GROUP BY clause",
		},
	)
}

func (s *groupingSQLGeneratorSuite) TestLoweredGroupingSetsShape() {
	s.assertCases(
		sqlgen.NewSelectSQLGenerator("test", sqlgen.DefaultDialectOptions()),
		selectTestCase{
			clause: groupingClauses(),
			sql: `SELECT "a", "b", COUNT(*), 0 AS "gid" FROM "t" GROUP BY "a", "b"` +
				` UNION ALL SELECT "a", NULL, COUNT(*), 1 AS "gid" FROM "t" GROUP BY "a"` +
				` UNION ALL SELECT NULL, NULL, COUNT(*), 3 AS "gid" FROM "t"`,
		},
	)
}

func (s *groupingSQLGeneratorSuite) TestLoweredGroupingSetsTypedNull() {
	clauses := exp.NewSelectClauses().
		SetFrom(exp.NewColumnListExpression("t")).
		SetSelect(exp.NewColumnListExpression(
			exp.ParseIdentifier("a"),
			exp.ParseIdentifier("b"),
			exp.NewSQLFunctionExpression("SUM", exp.ParseIdentifier("v")),
		)).
		SetGroupingSets(exp.NewGroupingSetsExpression(0,
			exp.NewGroupingSetExpression(
				exp.NewGroupingKeyExpression("a", "INTEGER"),
				exp.NewGroupingKeyExpression("b", "TEXT"),
			),
			exp.NewGroupingSetExpression(exp.NewGroupingKeyExpression("a", "INTEGER")),
		))

	b := sb.NewSQLBuilder(false)
	sqlgen.NewSelectSQLGenerator("test", sqlgen.DefaultDialectOptions()).Generate(b, clauses)
	s.assertNotPreparedSQL(b,
		`SELECT "a", "b", SUM("v") FROM "t" GROUP BY "a", "b"`+
			` UNION ALL SELECT "a", CAST(NULL AS TEXT), SUM("v") FROM "t" GROUP BY "a"`,
	)
}

func (s *groupingSQLGeneratorSuite) TestLoweredGroupingSetsDuplicateSets() {
	clauses := exp.NewSelectClauses().
		SetFrom(exp.NewColumnListExpression("t")).
		SetSelect(exp.NewColumnListExpression(
			exp.ParseIdentifier("a"),
			exp.NewSQLFunctionExpression("COUNT", exp.Star()),
			exp.NewGroupingIDExpression("a").As("gid"),
		)).
		SetGroupingSets(exp.NewGroupingSetsExpression(0,
			exp.NewGroupingSetExpression("a"),
			exp.NewGroupingSetExpression("a"),
			exp.NewGroupingSetExpression(),
		))

	b := sb.NewSQLBuilder(false)
	sqlgen.NewSelectSQLGenerator("test", sqlgen.DefaultDialectOptions()).Generate(b, clauses)
	s.assertNotPreparedSQL(b,
		`SELECT "a", COUNT(*), 0 AS "gid" FROM "t" GROUP BY "a"`+
			` UNION ALL SELECT "a", COUNT(*), 0 AS "gid" FROM "t" GROUP BY "a"`+
			` UNION ALL SELECT NULL, COUNT(*), 1 AS "gid" FROM "t"`,
	)
}

func (s *groupingSQLGeneratorSuite) TestLoweredGroupingSetsRepeatedFilterArgs() {
	clauses := groupingClauses().WhereAppend(
		exp.ParseIdentifier("x").Eq(1),
		exp.ParseIdentifier("y").Eq("z"),
	)

	b := sb.NewSQLBuilder(true)
	sqlgen.NewSelectSQLGenerator("test", sqlgen.DefaultDialectOptions()).Generate(b, clauses)
	// The WHERE filter is shared by every branch, so its arguments repeat once per branch in branch order.
	s.assertPreparedSQL(b,
		`SELECT "a", "b", COUNT(*), 0 AS "gid" FROM "t" WHERE (("x" = ?) AND ("y" = ?)) GROUP BY "a", "b"`+
			` UNION ALL SELECT "a", NULL, COUNT(*), 1 AS "gid" FROM "t" WHERE (("x" = ?) AND ("y" = ?)) GROUP BY "a"`+
			` UNION ALL SELECT NULL, NULL, COUNT(*), 3 AS "gid" FROM "t" WHERE (("x" = ?) AND ("y" = ?))`,
		[]interface{}{int64(1), "z", int64(1), "z", int64(1), "z"},
	)
}

func (s *groupingSQLGeneratorSuite) TestLoweredGroupingSetsGlobalOrderLimit() {
	clauses := groupingClauses().
		SetOrder(exp.NewOrderedExpression(exp.ParseIdentifier("gid"), exp.AscDir, exp.NoNullsSortType)).
		SetLimit(uint(2)).
		SetOffset(1)

	b := sb.NewSQLBuilder(false)
	sqlgen.NewSelectSQLGenerator("test", sqlgen.DefaultDialectOptions()).Generate(b, clauses)
	s.assertNotPreparedSQL(b,
		`SELECT * FROM (`+
			`SELECT "a", "b", COUNT(*), 0 AS "gid" FROM "t" GROUP BY "a", "b"`+
			` UNION ALL SELECT "a", NULL, COUNT(*), 1 AS "gid" FROM "t" GROUP BY "a"`+
			` UNION ALL SELECT NULL, NULL, COUNT(*), 3 AS "gid" FROM "t"`+
			`) AS "goqu_grouping_sets" ORDER BY "gid" ASC LIMIT 2 OFFSET 1`,
	)
}

func (s *groupingSQLGeneratorSuite) TestLoweredGroupingSetsHaving() {
	clauses := groupingClauses().HavingAppend(
		exp.NewGroupingIDExpression("a", "b").Gt(0),
		exp.NewSQLFunctionExpression("COUNT", exp.Star()).Gt(1),
	)

	b := sb.NewSQLBuilder(false)
	sqlgen.NewSelectSQLGenerator("test", sqlgen.DefaultDialectOptions()).Generate(b, clauses)
	s.assertNotPreparedSQL(b,
		`SELECT "a", "b", COUNT(*), 0 AS "gid" FROM "t" GROUP BY "a", "b" HAVING ((0 > 0) AND (COUNT(*) > 1))`+
			` UNION ALL SELECT "a", NULL, COUNT(*), 1 AS "gid" FROM "t" GROUP BY "a" HAVING ((1 > 0) AND (COUNT(*) > 1))`+
			` UNION ALL SELECT NULL, NULL, COUNT(*), 3 AS "gid" FROM "t" HAVING ((3 > 0) AND (COUNT(*) > 1))`,
	)
}

func (s *groupingSQLGeneratorSuite) TestLoweredGroupingSetsJoinsAndCTE() {
	cte := exp.NewCommonTableExpression(false, "filtered", goqu.From("src").Select("a"))
	clauses := exp.NewSelectClauses().
		CommonTablesAppend(cte).
		SetFrom(exp.NewColumnListExpression("t")).
		JoinsAppend(exp.NewConditionedJoinExpression(
			exp.InnerJoinType,
			exp.ParseIdentifier("u"),
			exp.NewJoinOnCondition(exp.ParseIdentifier("t.id").Eq(exp.ParseIdentifier("u.id"))),
		)).
		SetSelect(exp.NewColumnListExpression(
			exp.ParseIdentifier("a"),
			exp.NewSQLFunctionExpression("COUNT", exp.Star()),
		)).
		SetGroupingSets(exp.NewGroupingSetsExpression(0,
			exp.NewGroupingSetExpression("a"),
			exp.NewGroupingSetExpression(),
		))

	b := sb.NewSQLBuilder(false)
	sqlgen.NewSelectSQLGenerator("test", sqlgen.DefaultDialectOptions()).Generate(b, clauses)
	s.assertNotPreparedSQL(b,
		`WITH filtered AS (SELECT "a" FROM "src") `+
			`SELECT "a", COUNT(*) FROM "t" INNER JOIN "u" ON ("t"."id" = "u"."id") GROUP BY "a"`+
			` UNION ALL SELECT NULL, COUNT(*) FROM "t" INNER JOIN "u" ON ("t"."id" = "u"."id")`,
	)
}

func (s *groupingSQLGeneratorSuite) TestLoweredGroupingSetsUnsupported() {
	base := func() exp.SelectClauses {
		return exp.NewSelectClauses().
			SetFrom(exp.NewColumnListExpression("t")).
			SetSelect(exp.NewColumnListExpression(
				exp.ParseIdentifier("a"),
				exp.NewSQLFunctionExpression("COUNT", exp.Star()),
			)).
			SetGroupingSets(exp.NewGroupingSetsExpression(0,
				exp.NewGroupingSetExpression("a"),
				exp.NewGroupingSetExpression(),
			))
	}

	selectWith := func(e exp.Expression) exp.SelectClauses {
		return base().SetSelect(exp.NewColumnListExpression(
			exp.ParseIdentifier("a"), e,
		))
	}

	subquerySource := base().SetFrom(exp.NewColumnListExpression(
		goqu.From("other").Select("a"),
	))
	compound := base().CompoundsAppend(exp.NewCompoundExpression(
		exp.UnionAllCompoundType,
		goqu.From("other").Select("a"),
	))

	cases := []struct {
		clause exp.SelectClauses
		err    string
	}{
		{
			clause: base().SetDistinct(exp.NewColumnListExpression()),
			err:    "goqu: DISTINCT and DISTINCT aggregates are not supported with GROUPING SETS",
		},
		{
			clause: selectWith(exp.NewSQLFunctionExpression("COUNT",
				exp.NewSQLFunctionExpression("DISTINCT", exp.ParseIdentifier("a")))),
			err: "goqu: DISTINCT and DISTINCT aggregates are not supported with GROUPING SETS",
		},
		{
			clause: selectWith(exp.NewSQLFunctionExpression("ROW_NUMBER").Over(
				exp.NewWindowExpression(nil, nil, nil, nil))),
			err: "goqu: window functions, WINDOW and QUALIFY clauses are not supported with GROUPING SETS lowering",
		},
		{
			clause: selectWith(exp.NewLiteralExpression("1 + 1")),
			err:    "goqu: custom literal expressions are not supported with GROUPING SETS lowering; select only whitelisted aggregates",
		},
		{
			clause: selectWith(exp.NewSQLFunctionExpression("LOWER", exp.ParseIdentifier("a"))),
			err:    `goqu: function "LOWER" is not a whitelisted pure aggregate and cannot be copied into GROUPING SETS branches`,
		},
		{
			clause: selectWith(exp.NewGroupingIDExpression("a", "b")),
			err:    "goqu: GROUPING arguments must be the GROUPING SETS keys in the same order",
		},
		{
			clause: selectWith(exp.ParseIdentifier("not_a_key")),
			err:    `goqu: column "not_a_key" is selected but is not a grouping key or a whitelisted aggregate`,
		},
		{
			clause: subquerySource,
			err:    "goqu: GROUPING SETS lowering can only read table sources; wrap subquery sources in a WITH clause and reference them by name",
		},
		{
			clause: compound,
			err:    "goqu: GROUPING SETS cannot be combined with UNION/INTERSECT compounds",
		},
		{
			clause: base().SetOrder(exp.NewOrderedExpression(
				exp.NewSQLFunctionExpression("COUNT", exp.Star()), exp.DescSortDir, exp.NoNullsSortType)),
			err: "goqu: ORDER BY with lowered GROUPING SETS can only reference output column names, alias the expression first",
		},
		{
			clause: base().WhereAppend(
				exp.NewSQLFunctionExpression("ABS", exp.ParseIdentifier("a")).Gt(1)),
			err: "goqu: function calls in WHERE/JOIN conditions are not supported with GROUPING SETS lowering: " +
				"the filter is re-evaluated for every branch and volatility would change the result",
		},
	}

	gen := sqlgen.NewSelectSQLGenerator("test", sqlgen.DefaultDialectOptions())
	for i, tc := range cases {
		b := sb.NewSQLBuilder(false)
		gen.Generate(b, tc.clause)
		s.EqualError(b.Error(), tc.err, "test case %d", i)
	}
}

func TestGroupingSQLGeneratorSuite(t *testing.T) {
	suite.Run(t, new(groupingSQLGeneratorSuite))
}
