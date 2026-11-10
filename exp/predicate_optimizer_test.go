package exp_test

import (
	"database/sql/driver"
	"encoding/json"
	"testing"

	"github.com/doug-martin/goqu/v9/exp"
	"github.com/stretchr/testify/suite"
)

type namedString string

type countingValuer struct {
	calls int
	value interface{}
}

func (c *countingValuer) Value() (driver.Value, error) {
	c.calls++
	return c.value, nil
}

type predicateOptimizerSuite struct {
	suite.Suite
}

func TestPredicateOptimizerSuite(t *testing.T) {
	suite.Run(t, new(predicateOptimizerSuite))
}

func (s *predicateOptimizerSuite) ident(name string) exp.IdentifierExpression {
	return exp.NewIdentifierExpression("", "", name)
}

func (s *predicateOptimizerSuite) andList(children ...exp.Expression) exp.ExpressionList {
	return exp.NewExpressionList(exp.AndType, children...)
}

func (s *predicateOptimizerSuite) orList(children ...exp.Expression) exp.ExpressionList {
	return exp.NewExpressionList(exp.OrType, children...)
}

// asList narrows an expression to an ExpressionList or fails the test.
func (s *predicateOptimizerSuite) asList(e exp.Expression) exp.ExpressionList {
	list, ok := e.(exp.ExpressionList)
	s.True(ok, "expected an ExpressionList, got %T", e)
	return list
}

func (s *predicateOptimizerSuite) optimize(e exp.Expression) (exp.Expression, exp.PredicateOptimizationReport) {
	return exp.OptimizePredicateExpression(e, exp.DefaultPredicateOptimizationBudget())
}

func (s *predicateOptimizerSuite) TestFingerprintDistinguishesExplicitTypes() {
	a := s.ident("a")
	cases := []struct {
		name string
		lhs  exp.BooleanExpression
		rhs  exp.BooleanExpression
	}{
		{"int64 versus string", a.Eq(int64(1)), a.Eq("1")},
		{"int32 versus int64", a.Eq(int32(1)), a.Eq(int64(1))},
		{"float32 versus float64", a.Eq(float32(1)), a.Eq(float64(1))},
		{"byte slice versus int slice", a.Eq([]byte{1}), a.Eq([]int{1})},
		{"byte slice versus json raw message", a.Eq([]byte{1}), a.Eq(json.RawMessage{1})},
		{"named string versus string", a.Eq(namedString("x")), a.Eq("x")},
		{"in list element order", a.In(1, 2), a.In(2, 1)},
		{"in slice versus in args", a.In([]int64{1, 2}), a.In(1, 2)},
		{"different operators", a.Eq(1), a.Neq(1)},
		{"different identifier scope",
			exp.NewIdentifierExpression("", "t1", "a").Eq(1),
			exp.NewIdentifierExpression("", "t2", "a").Eq(1)},
	}
	for _, tc := range cases {
		s.Run(tc.name, func() {
			_, report := s.optimize(s.andList(tc.lhs, tc.rhs))
			s.False(report.Applied, "case %s must not merge distinct identities: %+v", tc.name, report)
			s.Equal(exp.PredicateReasonNoOpportunity, report.Reason)
		})
	}
}

func (s *predicateOptimizerSuite) TestFingerprintMergesIdenticalValues() {
	a := s.ident("a")
	cases := []struct {
		name string
		lhs  exp.Expression
		rhs  exp.Expression
	}{
		{"identical int64", a.Eq(int64(1)), a.Eq(int64(1))},
		{"identical byte slices", a.Eq([]byte{1, 2}), a.Eq([]byte{1, 2})},
		{"identical in args", a.In(1, 2, 3), a.In(1, 2, 3)},
		{"identical nil comparison", a.Is(nil), a.Is(nil)},
		{"identical not operator", a.Neq(1), a.Neq(1)},
		{"identical range", a.Between(exp.NewRangeVal(1, 10)), a.Between(exp.NewRangeVal(1, 10))},
	}
	for _, tc := range cases {
		s.Run(tc.name, func() {
			_, report := s.optimize(s.andList(tc.lhs, tc.rhs))
			s.True(report.Applied, "case %s should merge: %+v", tc.name, report)
			s.Equal(1, report.AtomsMerged)
		})
	}
}

func (s *predicateOptimizerSuite) TestOpaqueBarriersAreNeverMerged() {
	a, b := s.ident("a"), s.ident("b")
	valuer := &countingValuer{value: 1}
	// Building the comparison invokes the existing construction time null check, which is allowed. The optimizer
	// itself must never call Value, so reset the counter once the atoms exist.
	valuerAtoms := s.andList(a.Eq(valuer), a.Eq(valuer))
	valuer.calls = 0
	cases := []struct {
		name string
		tree exp.Expression
	}{
		{"literal sql", s.andList(exp.NewLiteralExpression("? ", 1), exp.NewLiteralExpression("? ", 1))},
		{"sql function", s.andList(
			exp.NewSQLFunctionExpression("RANDOM").Eq(1), exp.NewSQLFunctionExpression("RANDOM").Eq(1))},
		{"driver valuer", valuerAtoms},
		{"ex map", s.andList(exp.Ex{"a": 1}, exp.Ex{"a": 1})},
		{"function lhs", s.andList(
			exp.NewSQLFunctionExpression("oc", a).Eq(1), exp.NewSQLFunctionExpression("oc", a).Eq(1))},
		{"cast lhs", s.andList(exp.NewCastExpression(a, "NUMERIC").Eq(1), exp.NewCastExpression(a, "NUMERIC").Eq(1))},
		{"subquery shaped literal", s.andList(
			exp.NewLiteralExpression("EXISTS (SELECT 1 WHERE x = ?)", b),
			exp.NewLiteralExpression("EXISTS (SELECT 1 WHERE x = ?)", b))},
	}
	for _, tc := range cases {
		s.Run(tc.name, func() {
			out, report := s.optimize(tc.tree)
			s.False(report.Applied, "opaque case %s must stay untouched: %+v", tc.name, report)
			s.Equal(exp.PredicateReasonNoOpportunity, report.Reason)
			s.Equal(tc.tree, out)
		})
	}
	s.Equal(0, valuer.calls, "driver.Valuer.Value must never be called by the optimizer")
}

func (s *predicateOptimizerSuite) TestFactorCommonAndFromOr() {
	a, b, c := s.ident("a"), s.ident("b"), s.ident("c")
	tree := s.orList(s.andList(a.Eq(1), b.Eq(2)), s.andList(a.Eq(1), c.Eq(3)))
	out, report := s.optimize(tree)
	s.True(report.Applied)
	s.Equal(1, report.FactorsExtracted)

	outer := s.asList(out)
	s.Equal(exp.AndType, outer.Type())
	s.Len(outer.Expressions(), 2)
	s.Equal(a.Eq(1), outer.Expressions()[0])
	inner := s.asList(outer.Expressions()[1])
	s.Equal(exp.OrType, inner.Type())
	s.Equal([]exp.Expression{b.Eq(2), c.Eq(3)}, inner.Expressions())
}

func (s *predicateOptimizerSuite) TestFactorCommonOrFromAnd() {
	a, b, c := s.ident("a"), s.ident("b"), s.ident("c")
	tree := s.andList(s.orList(a.Eq(1), b.Eq(2)), s.orList(a.Eq(1), c.Eq(3)))
	out, report := s.optimize(tree)
	s.True(report.Applied)
	s.Equal(1, report.FactorsExtracted)

	outer := s.asList(out)
	s.Equal(exp.OrType, outer.Type())
	s.Equal(a.Eq(1), outer.Expressions()[0])
	inner := s.asList(outer.Expressions()[1])
	s.Equal(exp.AndType, inner.Type())
	s.Equal([]exp.Expression{b.Eq(2), c.Eq(3)}, inner.Expressions())
}

func (s *predicateOptimizerSuite) TestNestedFactorExtractionWithoutDNF() {
	a, b, x, y := s.ident("a"), s.ident("b"), s.ident("x"), s.ident("y")
	tree := s.orList(
		s.andList(a.Eq(1), b.Eq(2), x.Eq(3)),
		s.andList(a.Eq(1), b.Eq(2), y.Eq(4)),
	)
	out, report := s.optimize(tree)
	s.True(report.Applied)
	s.Equal(2, report.FactorsExtracted)
	s.Less(report.OptimizedNodes, report.OriginalNodes)

	// running the optimizer again is a no-op, so the pass is idempotent
	again, second := s.optimize(out)
	s.False(second.Applied)
	s.Equal(exp.PredicateReasonNoOpportunity, second.Reason)
	s.Equal(out, again)
}

func (s *predicateOptimizerSuite) TestUnaffectedMembersStayAtOuterLevel() {
	a, b, c, d := s.ident("a"), s.ident("b"), s.ident("c"), s.ident("d")
	// OR(AND(A,B), AND(A,C), D) must become OR(AND(A, OR(B,C)), D): pulling D into the factored conjunction would
	// change the result whenever A is false.
	tree := s.orList(
		s.andList(a.Eq(1), b.Eq(2)),
		s.andList(a.Eq(1), c.Eq(3)),
		d.Eq(4),
	)
	out, report := s.optimize(tree)
	s.True(report.Applied)
	outer := s.asList(out)
	s.Equal(exp.OrType, outer.Type())
	s.Len(outer.Expressions(), 2)
	group := s.asList(outer.Expressions()[0])
	s.Equal(exp.AndType, group.Type())
	s.Equal(a.Eq(1), group.Expressions()[0])
	inner := s.asList(group.Expressions()[1])
	s.Equal(exp.OrType, inner.Type())
	s.Equal([]exp.Expression{b.Eq(2), c.Eq(3)}, inner.Expressions())
	s.Equal(d.Eq(4), outer.Expressions()[1])
}

func (s *predicateOptimizerSuite) TestPartialOverlapWithMultipleUnaffected() {
	a, b, c, d, e := s.ident("a"), s.ident("b"), s.ident("c"), s.ident("d"), s.ident("e")
	tree := s.orList(
		d.Eq(4),
		s.andList(a.Eq(1), b.Eq(2)),
		e.Eq(5),
		s.andList(a.Eq(1), c.Eq(3)),
	)
	out, report := s.optimize(tree)
	s.True(report.Applied)
	outer := s.asList(out)
	s.Equal(exp.OrType, outer.Type())
	// the group takes the slot of the first affected member; unaffected members keep their exact positions
	s.Len(outer.Expressions(), 3)
	s.Equal(d.Eq(4), outer.Expressions()[0])
	s.Equal(e.Eq(5), outer.Expressions()[2])
	group := s.asList(outer.Expressions()[1])
	s.Equal(exp.AndType, group.Type())
	s.Equal(a.Eq(1), group.Expressions()[0])
}

func (s *predicateOptimizerSuite) TestOpaqueSubtreeIsNotRestructured() {
	a, b := s.ident("a"), s.ident("b")
	opaque := exp.NewLiteralExpression("oc() = 1")
	// even though "a" is a shared pure factor, the opaque literal in this subtree forbids any structural rewrite,
	// because short circuit evaluation counts of the side effecting expression could otherwise change.
	tree := s.orList(
		s.andList(a.Eq(1), opaque),
		s.andList(a.Eq(1), b.Eq(2)),
	)
	out, report := s.optimize(tree)
	s.False(report.Applied)
	s.Equal(exp.PredicateReasonNoOpportunity, report.Reason)
	s.Equal(tree, out)
}

func (s *predicateOptimizerSuite) TestPureNestedSubtreeOptimizedBelowOpaqueLevel() {
	a, b, c, d := s.ident("a"), s.ident("b"), s.ident("c"), s.ident("d")
	opaque := exp.NewLiteralExpression("oc() = 1")
	// the outer AND contains an opaque member and stays untouched, but the pure OR child is factored internally
	tree := s.andList(
		opaque,
		s.orList(s.andList(a.Eq(1), b.Eq(2)), s.andList(a.Eq(1), c.Eq(3))),
		d.Eq(4),
	)
	out, report := s.optimize(tree)
	s.True(report.Applied)
	outer := s.asList(out)
	s.Equal(exp.AndType, outer.Type())
	s.Equal(opaque, outer.Expressions()[0])
	s.Equal(d.Eq(4), outer.Expressions()[2])
	group := s.asList(outer.Expressions()[1])
	s.Equal(exp.AndType, group.Type())
	s.Equal(a.Eq(1), group.Expressions()[0])
}

func (s *predicateOptimizerSuite) TestVacuousBranchIsNotSynthesized() {
	a, b := s.ident("a"), s.ident("b")
	// OR(AND(A), AND(A, B)): extracting A would leave an empty branch, so the pass must conservatively stop.
	tree := s.orList(s.andList(a.Eq(1)), s.andList(a.Eq(1), b.Eq(2)))
	out, report := s.optimize(tree)
	s.False(report.Applied)
	s.Equal(exp.PredicateReasonNoOpportunity, report.Reason)
	s.Equal(tree, out)
}

func (s *predicateOptimizerSuite) TestFlattenAssociativeLists() {
	a, b, c := s.ident("a"), s.ident("b"), s.ident("c")
	tree := s.andList(s.andList(a.Eq(1), b.Eq(2)), c.Eq(3))
	out, report := s.optimize(tree)
	s.True(report.Applied)
	outer := s.asList(out)
	s.Equal(exp.AndType, outer.Type())
	s.Len(outer.Expressions(), 3)
	s.Equal([]exp.Expression{a.Eq(1), b.Eq(2), c.Eq(3)}, outer.Expressions())
}

func (s *predicateOptimizerSuite) TestThreeValuedLogicOperatorsArePreserved() {
	a := s.ident("a")
	// idempotent merging is allowed, but operators must never be inverted or rewritten as Go booleans.
	tree := s.andList(a.Neq(1), a.Neq(1), a.Is(nil), a.Is(nil))
	out, report := s.optimize(tree)
	s.True(report.Applied)
	members := s.asList(out).Expressions()
	s.Len(members, 2)
	neq := members[0].(exp.BooleanExpression)
	isNull := members[1].(exp.BooleanExpression)
	s.Equal(exp.NeqOp, neq.Op())
	s.Equal(exp.IsOp, isNull.Op())
}

func (s *predicateOptimizerSuite) TestNoConstantFoldingAroundOpaqueTrue() {
	a := s.ident("a")
	// TRUE is rendered through opaque literal SQL, which the optimizer must not treat as a foldable constant.
	tree := s.orList(a.Eq(1), exp.NewLiteralExpression("TRUE"))
	out, report := s.optimize(tree)
	s.False(report.Applied)
	s.Equal(tree, out)
}

func (s *predicateOptimizerSuite) TestNodeBudgetFallsBackToOriginal() {
	a, b, c := s.ident("a"), s.ident("b"), s.ident("c")
	tree := s.orList(s.andList(a.Eq(1), b.Eq(2)), s.andList(a.Eq(1), c.Eq(3)))
	budget := exp.PredicateOptimizationBudget{MaxNodes: 1, MaxFactorCandidates: 1, MaxOutputGrowth: 0}
	out, report := exp.OptimizePredicateExpression(tree, budget)
	s.False(report.Applied)
	s.Equal(exp.PredicateReasonNodeBudget, report.Reason)
	s.Equal(tree, out)
}

func (s *predicateOptimizerSuite) TestCandidateBudgetFallsBackToOriginal() {
	a, b, x, y := s.ident("a"), s.ident("b"), s.ident("x"), s.ident("y")
	// one list level exposes two distinct shared factor fingerprints
	tree := s.orList(
		s.andList(a.Eq(1), b.Eq(2), x.Eq(3)),
		s.andList(a.Eq(1), b.Eq(2), y.Eq(4)),
	)
	budget := exp.PredicateOptimizationBudget{MaxFactorCandidates: 1}
	out, report := exp.OptimizePredicateExpression(tree, budget)
	s.False(report.Applied)
	s.Equal(exp.PredicateReasonCandidateBudget, report.Reason)
	s.Equal(tree, out)
}

func (s *predicateOptimizerSuite) TestInputTreeIsNotMutated() {
	a, b, c := s.ident("a"), s.ident("b"), s.ident("c")
	tree := s.orList(s.andList(a.Eq(1), b.Eq(2)), s.andList(a.Eq(1), c.Eq(3)))
	originalMembers := append([]exp.Expression(nil), tree.Expressions()...)
	out, report := s.optimize(tree)
	s.True(report.Applied)
	s.Equal(exp.AndType, s.asList(out).Type(), "the result must be a newly built tree")
	s.Equal(exp.OrType, tree.Type(), "the input clause tree must stay untouched")
	s.Equal(originalMembers, tree.Expressions(), "the input clause tree must stay untouched")
}

func (s *predicateOptimizerSuite) TestEmptyAndNilInput() {
	out, report := exp.OptimizePredicateExpression(nil, exp.DefaultPredicateOptimizationBudget())
	s.Nil(out)
	s.False(report.Applied)
	s.Equal(exp.PredicateReasonEmpty, report.Reason)

	empty := s.andList()
	_, report = s.optimize(empty)
	s.False(report.Applied)
}
