package exp_test

import (
	"testing"

	"github.com/doug-martin/goqu/v9/exp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewGroupingSetExpression(t *testing.T) {
	set := exp.NewGroupingSetExpression("a", exp.NewIdentifierExpression("", "t", "b"))
	require.Len(t, set.Columns(), 2)
	assert.False(t, set.IsEmpty())
	first, ok := set.Columns()[0].(exp.IdentifierExpression)
	require.True(t, ok)
	assert.Equal(t, "a", first.GetCol())
	assert.Equal(t, "b", set.Columns()[1].(exp.IdentifierExpression).GetCol())

	empty := exp.NewGroupingSetExpression()
	assert.True(t, empty.IsEmpty())
	assert.Empty(t, empty.Columns())

	wrapped := exp.NewGroupingSetExpression(exp.NewGroupingKeyExpression("a", "INTEGER"))
	gk, ok := wrapped.Columns()[0].(exp.GroupingKeyExpression)
	require.True(t, ok)
	assert.Equal(t, "INTEGER", gk.NullType())
	assert.Equal(t, "a", gk.Key().(exp.IdentifierExpression).GetCol())

	assert.Panics(t, func() { exp.NewGroupingSetExpression(123) })
	assert.Panics(t, func() { exp.NewGroupingSetExpression(nil) })
}

func TestNewGroupingSetsExpressionKeysAndDuplicates(t *testing.T) {
	ab := exp.NewGroupingSetExpression("a", "b")
	a := exp.NewGroupingSetExpression("a")
	total := exp.NewGroupingSetExpression()
	// (a) is listed twice on purpose: duplicate sets must survive.
	gse := exp.NewGroupingSetsExpression(7, ab, a, total, a)

	assert.Equal(t, 4, len(gse.Sets()))
	assert.Equal(t, ab, gse.Sets()[0])

	// The key union keeps first-seen order.
	keys := gse.Keys().Columns()
	require.Len(t, keys, 2)
	assert.Equal(t, "a", keys[0].(exp.IdentifierExpression).GetCol())
	assert.Equal(t, "b", keys[1].(exp.IdentifierExpression).GetCol())

	assert.Equal(t, 7, gse.MaxBranches())
}

func TestNewGroupingSetsExpressionKeyOrderAcrossSets(t *testing.T) {
	gse := exp.NewGroupingSetsExpression(0,
		exp.NewGroupingSetExpression("b"),
		exp.NewGroupingSetExpression("a", "b"),
		exp.NewGroupingSetExpression("c"),
	)
	keys := gse.Keys().Columns()
	require.Len(t, keys, 3)
	assert.Equal(t, []string{"b", "a", "c"}, []string{
		keys[0].(exp.IdentifierExpression).GetCol().(string),
		keys[1].(exp.IdentifierExpression).GetCol().(string),
		keys[2].(exp.IdentifierExpression).GetCol().(string),
	})
	assert.Equal(t, 0, gse.MaxBranches())
}

func TestGroupingSetsClone(t *testing.T) {
	gse := exp.NewGroupingSetsExpression(3,
		exp.NewGroupingSetExpression("a"),
		exp.NewGroupingSetExpression(),
	)
	clone := gse.Clone().(exp.GroupingSetsExpression)
	assert.Equal(t, len(gse.Sets()), len(clone.Sets()))
	assert.Equal(t, gse.MaxBranches(), clone.MaxBranches())
	assert.Equal(t, len(gse.Keys().Columns()), len(clone.Keys().Columns()))
}

func TestNewGroupingIDExpression(t *testing.T) {
	gid := exp.NewGroupingIDExpression("a", "b")
	cols := gid.Columns().Columns()
	require.Len(t, cols, 2)
	assert.Equal(t, "a", cols[0].(exp.IdentifierExpression).GetCol())

	aliased := gid.As("g")
	assert.Equal(t, "g", aliased.GetAs().GetCol())
	_, isGID := aliased.Aliased().(exp.GroupingIDExpression)
	assert.True(t, isGID)

	boolean := gid.Eq(1)
	assert.Equal(t, exp.EqOp, boolean.Op())

	ordered := gid.Asc()
	assert.True(t, ordered.IsAsc())

	assert.NotPanics(t, func() {
		_ = gid.Gt(1)
		_ = gid.IsNotNull()
		_ = gid.In(1, 2)
		_ = gid.Between(exp.NewRangeVal(0, 3))
	})

	clone := gid.Clone().(exp.GroupingIDExpression)
	assert.Equal(t, len(gid.Columns().Columns()), len(clone.Columns().Columns()))
}

func TestGroupingKeyExpression(t *testing.T) {
	gk := exp.NewGroupingKeyExpression("a", "INTEGER")
	assert.Equal(t, "INTEGER", gk.NullType())
	assert.Equal(t, "a", gk.Key().(exp.IdentifierExpression).GetCol())

	plain := exp.NewGroupingKeyExpression("a", "")
	assert.Equal(t, "", plain.NullType())

	assert.Panics(t, func() { exp.NewGroupingKeyExpression("a", "INTEGER; DROP TABLE t") })
}

func TestSameGroupingKeyAndUnwrap(t *testing.T) {
	a := exp.ParseIdentifier("a")
	gk := exp.NewGroupingKeyExpression("a", "INTEGER")
	assert.True(t, exp.SameGroupingKey(a, gk))
	assert.Equal(t, a, exp.UnwrapGroupingKey(gk))
	assert.Equal(t, a, exp.UnwrapGroupingKey(a))

	b := exp.ParseIdentifier("b")
	assert.False(t, exp.SameGroupingKey(a, b))

	qualified := exp.NewIdentifierExpression("", "t", "a")
	assert.False(t, exp.SameGroupingKey(a, qualified))
	assert.True(t, exp.SameGroupingKey(qualified, exp.NewGroupingKeyExpression(qualified, "TEXT")))
}

func TestSelectClausesGroupingSets(t *testing.T) {
	sc := exp.NewSelectClauses()
	assert.Nil(t, sc.GroupingSets())

	gse := exp.NewGroupingSetsExpression(0, exp.NewGroupingSetExpression("a"))
	withSets := sc.SetGroupingSets(gse)
	assert.Nil(t, sc.GroupingSets(), "the original clauses must stay immutable")
	assert.Equal(t, gse, withSets.GroupingSets())

	cleared := withSets.ClearGroupingSets()
	assert.NotNil(t, withSets.GroupingSets())
	assert.Nil(t, cleared.GroupingSets())
}
