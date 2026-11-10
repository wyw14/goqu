package goqu_test

import (
	"testing"

	"github.com/doug-martin/goqu/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func groupingSetsDataset(dialect string) *goqu.SelectDataset {
	return goqu.Dialect(dialect).From("sales").
		Select(
			goqu.I("region"),
			goqu.I("product"),
			goqu.COUNT("*").As("row_count"),
			goqu.SUM("amount").As("amount_sum"),
			goqu.GroupingID("region", "product").As("gid"),
		).
		GroupingSets(goqu.GroupingSets(0,
			goqu.GroupingSet("region", "product"),
			goqu.GroupingSet("region"),
			goqu.GroupingSet(),
		))
}

func TestGroupingSetsPostgresNativeSQL(t *testing.T) {
	sql, args, err := groupingSetsDataset("postgres").ToSQL()
	require.NoError(t, err)
	assert.Equal(t,
		`SELECT "region", "product", COUNT(*) AS "row_count", SUM("amount") AS "amount_sum", `+
			`GROUPING("region", "product") AS "gid" FROM "sales" GROUP BY GROUPING SETS `+
			`(("region", "product"), ("region"), ())`,
		sql,
	)
	assert.Empty(t, args)

	// A filter argument is bound exactly once on the native statement.
	prepared, prepArgs, err := groupingSetsDataset("postgres").
		Where(goqu.I("region").Eq("east")).Prepared(true).ToSQL()
	require.NoError(t, err)
	assert.Equal(t,
		`SELECT "region", "product", COUNT(*) AS "row_count", SUM("amount") AS "amount_sum", `+
			`GROUPING("region", "product") AS "gid" FROM "sales" WHERE ("region" = $1) GROUP BY GROUPING SETS `+
			`(("region", "product"), ("region"), ())`,
		prepared,
	)
	assert.Equal(t, []interface{}{"east"}, prepArgs)
}

func TestGroupingSetsSQLiteLoweringSQL(t *testing.T) {
	sql, args, err := groupingSetsDataset("sqlite3").ToSQL()
	require.NoError(t, err)
	assert.Equal(t,
		"SELECT `region`, `product`, COUNT(*) AS `row_count`, SUM(`amount`) AS `amount_sum`, 0 AS `gid` "+
			"FROM `sales` GROUP BY `region`, `product`"+
			" UNION ALL SELECT `region`, NULL, COUNT(*) AS `row_count`, SUM(`amount`) AS `amount_sum`, 1 AS `gid` "+
			"FROM `sales` GROUP BY `region`"+
			" UNION ALL SELECT NULL, NULL, COUNT(*) AS `row_count`, SUM(`amount`) AS `amount_sum`, 3 AS `gid` "+
			"FROM `sales`",
		sql,
	)
	assert.Empty(t, args)
}

func TestGroupingSetsDatasetImmutability(t *testing.T) {
	original := goqu.Dialect("sqlite3").From("sales").Select(goqu.I("region"), goqu.COUNT("*"))
	withSets := original.GroupingSets(goqu.GroupingSets(0,
		goqu.GroupingSet("region"),
		goqu.GroupingSet(),
	))

	assert.Nil(t, original.GetClauses().GroupingSets(), "the source dataset must stay unchanged")
	require.NotNil(t, withSets.GetClauses().GroupingSets())

	originalSQL, _, err := original.ToSQL()
	require.NoError(t, err)
	assert.Equal(t, "SELECT `region`, COUNT(*) FROM `sales`", originalSQL)

	cleared := withSets.GroupingSets(nil)
	assert.Nil(t, cleared.GetClauses().GroupingSets())
	require.NotNil(t, withSets.GetClauses().GroupingSets(), "clearing must not mutate the receiver")
}

func TestGroupingSetsBudgetFailure(t *testing.T) {
	ds := goqu.Dialect("postgres").From("sales").
		Select(goqu.I("a"), goqu.COUNT("*")).
		GroupingSets(goqu.GroupingSets(2,
			goqu.GroupingSet("a"),
			goqu.GroupingSet("b"),
			goqu.GroupingSet(),
		))
	_, _, err := ds.ToSQL()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "GROUPING SETS branch budget exceeded: 3 branches > 2")

	// The same budget protects the UNION ALL lowering.
	_, _, err = goqu.Dialect("sqlite3").From("sales").
		Select(goqu.I("a"), goqu.COUNT("*")).
		GroupingSets(goqu.GroupingSets(2,
			goqu.GroupingSet("a"),
			goqu.GroupingSet("b"),
			goqu.GroupingSet(),
		)).ToSQL()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "GROUPING SETS branch budget exceeded: 3 branches > 2")
}

func TestGroupingSetsLoweringUnsupported(t *testing.T) {
	base := func() *goqu.SelectDataset {
		return goqu.Dialect("sqlite3").From("sales").
			Select(goqu.I("region"), goqu.COUNT("*")).
			GroupingSets(goqu.GroupingSets(0,
				goqu.GroupingSet("region"),
				goqu.GroupingSet(),
			))
	}

	_, _, err := base().Distinct().ToSQL()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "DISTINCT")

	_, _, err = base().Select(
		goqu.I("region"),
		goqu.Func("COUNT", goqu.DISTINCT("region")),
	).ToSQL()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "DISTINCT")

	_, _, err = base().Select(goqu.I("region"), goqu.ROW_NUMBER().Over(goqu.W())).ToSQL()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "window functions")

	_, _, err = base().Select(goqu.I("region"), goqu.L("1 + 1")).ToSQL()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "custom literal expressions")

	_, _, err = base().From(goqu.From("other").As("o")).ToSQL()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "table sources")
}
