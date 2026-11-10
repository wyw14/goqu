package sqlgen_test

import (
	"database/sql/driver"
	"testing"

	"github.com/doug-martin/goqu/v9"
	"github.com/doug-martin/goqu/v9/exp"
	"github.com/doug-martin/goqu/v9/internal/sb"
	"github.com/doug-martin/goqu/v9/sqlgen"
	"github.com/stretchr/testify/suite"
)

type renderCountingValuer struct {
	calls int
	value interface{}
}

func (r *renderCountingValuer) Value() (driver.Value, error) {
	r.calls++
	return r.value, nil
}

type predicateOptimizationSQLSuite struct {
	baseSQLGeneratorSuite
}

func TestPredicateOptimizationSQLSuite(t *testing.T) {
	suite.Run(t, new(predicateOptimizationSQLSuite))
}

func (s *predicateOptimizationSQLSuite) options(enabled bool) *sqlgen.SQLDialectOptions {
	opts := sqlgen.DefaultDialectOptions()
	opts.OptimizePredicates = enabled
	return opts
}

// sharedFactorPredicate is (A AND B) OR (A AND C) with a large repeated argument, the canonical factoring shape.
func (s *predicateOptimizationSQLSuite) sharedFactorPredicate() exp.Expression {
	repeated := "repeated-large-argument"
	return exp.NewExpressionList(exp.OrType,
		exp.NewExpressionList(exp.AndType, exp.NewIdentifierExpression("", "", "a").Eq(repeated),
			exp.NewIdentifierExpression("", "", "b").Gt(2)),
		exp.NewExpressionList(exp.AndType, exp.NewIdentifierExpression("", "", "a").Eq(repeated),
			exp.NewIdentifierExpression("", "", "c").Lt(9)),
	)
}

func (s *predicateOptimizationSQLSuite) renderSelect(
	opts *sqlgen.SQLDialectOptions, prepared bool, mutate func(exp.SelectClauses) exp.SelectClauses,
) (query string, args []interface{}, err error) {
	clauses := exp.NewSelectClauses().SetFrom(exp.NewColumnListExpression("t"))
	if mutate != nil {
		clauses = mutate(clauses)
	}
	builder := sb.NewSQLBuilder(prepared)
	sqlgen.NewSelectSQLGenerator("opt", opts).Generate(builder, clauses)
	return builder.ToSQL()
}

func (s *predicateOptimizationSQLSuite) TestSelectWhereFactoredPrepared() {
	predicate := s.sharedFactorPredicate()
	mutate := func(clauses exp.SelectClauses) exp.SelectClauses {
		return clauses.WhereAppend(predicate)
	}
	enabledSQL, enabledArgs, err := s.renderSelect(s.options(true), true, mutate)
	s.Require().NoError(err)
	s.Equal(
		`SELECT * FROM "t" WHERE (("a" = ?) AND (("b" > ?) OR ("c" < ?)))`,
		enabledSQL,
	)
	// arguments follow the new tree walk order; the repeated argument is bound once instead of twice
	s.Equal([]interface{}{"repeated-large-argument", int64(2), int64(9)}, enabledArgs)

	disabledSQL, disabledArgs, err := s.renderSelect(s.options(false), true, mutate)
	s.Require().NoError(err)
	s.Equal(
		`SELECT * FROM "t" WHERE ((("a" = ?) AND ("b" > ?)) OR (("a" = ?) AND ("c" < ?)))`,
		disabledSQL,
	)
	s.Len(disabledArgs, 4)
	s.Less(len(enabledSQL), len(disabledSQL))
	s.Less(len(enabledArgs), len(disabledArgs))
}

func (s *predicateOptimizationSQLSuite) TestSelectWhereFactoredInterpolated() {
	mutate := func(clauses exp.SelectClauses) exp.SelectClauses {
		return clauses.WhereAppend(s.sharedFactorPredicate())
	}
	enabledSQL, args, err := s.renderSelect(s.options(true), false, mutate)
	s.Require().NoError(err)
	s.Equal(
		`SELECT * FROM "t" WHERE (("a" = 'repeated-large-argument') AND (("b" > 2) OR ("c" < 9)))`,
		enabledSQL,
	)
	s.Empty(args)
}

func (s *predicateOptimizationSQLSuite) TestSelectHavingFactored() {
	predicate := s.sharedFactorPredicate()
	mutate := func(clauses exp.SelectClauses) exp.SelectClauses {
		return clauses.
			SetGroupBy(exp.NewColumnListExpression(exp.NewIdentifierExpression("", "", "g"))).
			HavingAppend(predicate)
	}
	enabledSQL, enabledArgs, err := s.renderSelect(s.options(true), true, mutate)
	s.Require().NoError(err)
	s.Equal(
		`SELECT * FROM "t" GROUP BY "g" HAVING (("a" = ?) AND (("b" > ?) OR ("c" < ?)))`,
		enabledSQL,
	)
	s.Len(enabledArgs, 3)

	disabledSQL, _, err := s.renderSelect(s.options(false), true, mutate)
	s.Require().NoError(err)
	s.Contains(disabledSQL, `HAVING ((("a" = ?) AND ("b" > ?)) OR (("a" = ?) AND ("c" < ?)))`)
}

func (s *predicateOptimizationSQLSuite) TestUpdateAndDeleteWhereFactored() {
	predicate := s.sharedFactorPredicate()
	updateClauses := exp.NewUpdateClauses().
		SetTable(exp.NewIdentifierExpression("", "t", "")).
		SetSetValues(exp.Record{"d": 1}).
		WhereAppend(predicate)
	updateBuilder := sb.NewSQLBuilder(true)
	sqlgen.NewUpdateSQLGenerator("opt", s.options(true)).Generate(updateBuilder, updateClauses)
	updateSQL, updateArgs, err := updateBuilder.ToSQL()
	s.Require().NoError(err)
	s.Equal(
		`UPDATE "t" SET "d"=? WHERE (("a" = ?) AND (("b" > ?) OR ("c" < ?)))`,
		updateSQL,
	)
	s.Len(updateArgs, 4)

	deleteClauses := exp.NewDeleteClauses().
		SetFrom(exp.NewIdentifierExpression("", "t", "")).
		WhereAppend(predicate)
	deleteBuilder := sb.NewSQLBuilder(true)
	sqlgen.NewDeleteSQLGenerator("opt", s.options(true)).Generate(deleteBuilder, deleteClauses)
	deleteSQL, deleteArgs, err := deleteBuilder.ToSQL()
	s.Require().NoError(err)
	s.Equal(
		`DELETE FROM "t" WHERE (("a" = ?) AND (("b" > ?) OR ("c" < ?)))`,
		deleteSQL,
	)
	s.Len(deleteArgs, 3)
}

func (s *predicateOptimizationSQLSuite) TestNodeBudgetExceededKeepsOriginalSQL() {
	// more distinct atoms than exp.DefaultMaxPredicateNodes, so the optimizer must leave the clause untouched
	atoms := make([]exp.Expression, 0, exp.DefaultMaxPredicateNodes+50)
	for i := 0; i < exp.DefaultMaxPredicateNodes+50; i++ {
		atoms = append(atoms, exp.NewIdentifierExpression("", "", "col").Eq(i))
	}
	mutate := func(clauses exp.SelectClauses) exp.SelectClauses {
		return clauses.WhereAppend(exp.NewExpressionList(exp.AndType, atoms...))
	}
	enabledSQL, enabledArgs, err := s.renderSelect(s.options(true), true, mutate)
	s.Require().NoError(err)
	disabledSQL, disabledArgs, err := s.renderSelect(s.options(false), true, mutate)
	s.Require().NoError(err)
	s.Equal(disabledSQL, enabledSQL)
	s.Equal(disabledArgs, enabledArgs)
}

func (s *predicateOptimizationSQLSuite) TestNoOpportunityRendersIdentically() {
	predicate := exp.NewExpressionList(exp.AndType,
		exp.NewIdentifierExpression("", "", "a").Eq(1),
		exp.NewLiteralExpression("RANDOM() > 0"),
	)
	mutate := func(clauses exp.SelectClauses) exp.SelectClauses {
		return clauses.WhereAppend(predicate)
	}
	enabledSQL, enabledArgs, err := s.renderSelect(s.options(true), true, mutate)
	s.Require().NoError(err)
	disabledSQL, disabledArgs, err := s.renderSelect(s.options(false), true, mutate)
	s.Require().NoError(err)
	s.Equal(disabledSQL, enabledSQL)
	s.Equal(disabledArgs, enabledArgs)
}

func (s *predicateOptimizationSQLSuite) TestValuerBlocksRewritesAndIsRenderedOnce() {
	goqu.RegisterDialect("predopt_valuer", s.options(true))
	valuer := &renderCountingValuer{value: 1}
	// the valuer makes the whole subtree opaque, so the optimizer must leave the clause structure byte identical
	predicate := goqu.Or(
		goqu.And(goqu.I("a").Eq(1), goqu.I("v").Eq(valuer)),
		goqu.And(goqu.I("a").Eq(1), goqu.I("c").Lt(9)),
	)
	enabledDS := goqu.Dialect("predopt_valuer").From("t").Where(predicate).Prepared(true)
	disabledDS := goqu.From("t").Where(predicate).Prepared(true)
	valuer.calls = 0
	enabledSQL, enabledArgs, err := enabledDS.ToSQL()
	s.Require().NoError(err)
	s.Equal(1, valuer.calls, "the opaque valuer must be rendered exactly once per statement")
	valuer.calls = 0
	disabledSQL, disabledArgs, err := disabledDS.ToSQL()
	s.Require().NoError(err)
	s.Equal(1, valuer.calls)
	s.Equal(disabledSQL, enabledSQL)
	s.Equal(disabledArgs, enabledArgs)
}
