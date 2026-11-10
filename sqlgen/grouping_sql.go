package sqlgen

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/doug-martin/goqu/v9/exp"
	"github.com/doug-martin/goqu/v9/internal/errors"
	"github.com/doug-martin/goqu/v9/internal/sb"
)

// groupingSetsSubqueryAlias is the alias of the wrapper derived table used to apply global ORDER BY/LIMIT/OFFSET
// to a lowered UNION ALL query.
const groupingSetsSubqueryAlias = "goqu_grouping_sets"

// pureAggregateWhitelist lists the only aggregate functions the UNION ALL lowering copies into every branch.
// Everything else is rejected so a function that is volatile, scalar or windowed is never silently re-evaluated.
var pureAggregateWhitelist = map[string]bool{
	"COUNT": true,
	"SUM":   true,
	"AVG":   true,
	"MIN":   true,
	"MAX":   true,
}

func ErrGroupingSetsWithGroupBy() error {
	return errors.New("GROUPING SETS cannot be combined with a plain GROUP BY clause")
}

func ErrEmptyGroupingSets() error {
	return errors.New("GROUPING SETS requires at least one grouping set")
}

func ErrGroupingSetBudgetExceeded(branches, budget int) error {
	return errors.New("GROUPING SETS branch budget exceeded: %d branches > %d, declare fewer sets or raise the budget", branches, budget)
}

func ErrGroupingSetsDistinct() error {
	return errors.New("DISTINCT and DISTINCT aggregates are not supported with GROUPING SETS")
}

func ErrGroupingSetsCompound() error {
	return errors.New("GROUPING SETS cannot be combined with UNION/INTERSECT compounds")
}

func ErrGroupingSetsWindow() error {
	return errors.New("window functions, WINDOW and QUALIFY clauses are not supported with GROUPING SETS lowering")
}

func ErrGroupingSetsLock() error {
	return errors.New("FOR UPDATE/SHARE locking clauses are not supported with GROUPING SETS")
}

func ErrGroupingSetsNoFrom() error {
	return errors.New("GROUPING SETS requires a FROM clause")
}

func ErrGroupingSetsSubquerySource() error {
	return errors.New(
		"GROUPING SETS lowering can only read table sources; wrap subquery sources in a WITH clause and reference them by name",
	)
}

func ErrGroupingSetsFunction(name string) error {
	return errors.New("function %q is not a whitelisted pure aggregate and cannot be copied into GROUPING SETS branches", name)
}

func ErrGroupingSetsLiteral() error {
	return errors.New("custom literal expressions are not supported with GROUPING SETS lowering; select only whitelisted aggregates")
}

func ErrGroupingSetsUnsupported(kind string) error {
	return errors.New("%s expressions are not supported with GROUPING SETS lowering", kind)
}

func ErrGroupingIDKeyMismatch() error {
	return errors.New("GROUPING arguments must be the GROUPING SETS keys in the same order")
}

func ErrGroupingSetsUnknownColumn(name string) error {
	return errors.New("column %q is selected but is not a grouping key or a whitelisted aggregate", name)
}

func ErrGroupingSetsOrder() error {
	return errors.New("ORDER BY with lowered GROUPING SETS can only reference output column names, alias the expression first")
}

func ErrGroupingSetsFilterFunction() error {
	return errors.New(
		"function calls in WHERE/JOIN conditions are not supported with GROUPING SETS lowering: " +
			"the filter is re-evaluated for every branch and volatility would change the result",
	)
}

// GroupByOrGroupingSetsSQL renders the GROUP BY fragment, dispatching to native GROUPING SETS syntax for dialects
// that support it and to the restricted UNION ALL lowering for the others.
func (ssg *selectSQLGenerator) GroupByOrGroupingSetsSQL(b sb.SQLBuilder, clauses exp.SelectClauses) {
	gse := clauses.GroupingSets()
	if gse == nil {
		ssg.GroupBySQL(b, clauses.GroupBy())
		return
	}
	if clauses.GroupBy() != nil && len(clauses.GroupBy().Columns()) > 0 {
		b.SetError(ErrGroupingSetsWithGroupBy())
		return
	}
	if len(gse.Sets()) == 0 {
		b.SetError(ErrEmptyGroupingSets())
		return
	}
	budget := gse.MaxBranches()
	if budget <= 0 {
		budget = ssg.DialectOptions().MaxGroupingSetBranches
	}
	if len(gse.Sets()) > budget {
		b.SetError(ErrGroupingSetBudgetExceeded(len(gse.Sets()), budget))
		return
	}
	if ssg.DialectOptions().SupportsGroupingSets {
		ssg.nativeGroupingSetsSQL(b, gse)
		return
	}
	ssg.loweredGroupingSetsSQL(b, clauses, gse)
}

// nativeGroupingSetsSQL renders GROUP BY GROUPING SETS (("a", "b"), ("a"), ()).
func (ssg *selectSQLGenerator) nativeGroupingSetsSQL(b sb.SQLBuilder, gse exp.GroupingSetsExpression) {
	opts := ssg.DialectOptions()
	b.Write(opts.GroupByFragment).Write(opts.GroupingSetsFragment)
	b.WriteRunes(opts.LeftParenRune)
	for i, set := range gse.Sets() {
		if i > 0 {
			b.WriteRunes(opts.CommaRune, opts.SpaceRune)
		}
		b.WriteRunes(opts.LeftParenRune)
		ssg.ExpressionSQLGenerator().Generate(b, newExpressionColumnList(set.Columns()))
		b.WriteRunes(opts.RightParenRune)
	}
	b.WriteRunes(opts.RightParenRune)
}

// loweredGroupingSetsSQL renders one aggregate SELECT per set joined with UNION ALL. Every branch keeps the same
// FROM/JOIN/WHERE input logic, the same output columns in the same order and a constant GROUPING bitmap, so data
// NULLs stay distinguishable from keys aggregated away.
func (ssg *selectSQLGenerator) loweredGroupingSetsSQL(
	b sb.SQLBuilder,
	clauses exp.SelectClauses,
	gse exp.GroupingSetsExpression,
) {
	if err := ssg.validateGroupingSetsClauses(clauses, gse); err != nil {
		b.SetError(err)
		return
	}
	keys := gse.Keys().Columns()
	needWrap := clauses.HasOrder() || clauses.HasLimit() || clauses.Offset() > 0

	// Hoist CTEs so they are declared once instead of being copied, and rejected, into every branch.
	if ctes := clauses.CommonTables(); len(ctes) > 0 {
		ssg.ExpressionSQLGenerator().Generate(b, ctes)
	}

	if needWrap {
		opts := ssg.DialectOptions()
		b.Write(opts.SelectClause).WriteRunes(opts.SpaceRune, opts.StarRune)
		b.Write(opts.FromFragment).WriteRunes(opts.SpaceRune, opts.LeftParenRune)
	}

	for i, set := range gse.Sets() {
		if i > 0 {
			b.Write(ssg.DialectOptions().UnionAllFragment)
		}
		present := groupingSetPresence(set, keys)
		bitmap := groupingBitmap(present)
		branch, err := ssg.buildGroupingSetBranch(clauses, gse, keys, set, present, bitmap)
		if err != nil {
			b.SetError(err)
			return
		}
		ssg.Generate(b, branch)
		if b.Error() != nil {
			return
		}
	}

	if needWrap {
		opts := ssg.DialectOptions()
		b.WriteRunes(opts.RightParenRune)
		b.Write(opts.AsFragment)
		ssg.ExpressionSQLGenerator().Generate(b, exp.NewIdentifierExpression("", "", groupingSetsSubqueryAlias))
		ssg.OrderSQL(b, clauses.Order())
		ssg.LimitSQL(b, clauses.Limit())
		ssg.OffsetSQL(b, clauses.Offset())
	}
}

// buildGroupingSetBranch creates the clauses of one UNION ALL branch: the shared input logic with its own GROUP BY,
// its own NULLed grouping keys, its bitmap constant and its per-set HAVING.
func (ssg *selectSQLGenerator) buildGroupingSetBranch(
	clauses exp.SelectClauses,
	gse exp.GroupingSetsExpression,
	keys []exp.Expression,
	set exp.GroupingSetExpression,
	present []bool,
	bitmap int,
) (exp.SelectClauses, error) {
	selectCols := clauses.Select().Columns()
	branchSelect := make([]exp.Expression, 0, len(selectCols))
	for _, col := range selectCols {
		transformed, err := ssg.transformGroupingExpression(col, gse, keys, present, bitmap)
		if err != nil {
			return nil, err
		}
		branchSelect = append(branchSelect, transformed)
	}

	branch := exp.NewSelectClauses().
		SetSelect(newExpressionColumnList(branchSelect)).
		SetFrom(clauses.From())
	for _, join := range clauses.Joins() {
		branch = branch.JoinsAppend(join)
	}
	if where := clauses.Where(); where != nil && !where.IsEmpty() {
		branch = branch.WhereAppend(where.Expressions()...)
	}
	if groupCols := groupingSetColumns(set); len(groupCols) > 0 {
		branch = branch.SetGroupBy(newExpressionColumnList(groupCols))
	}
	if having := clauses.Having(); having != nil && !having.IsEmpty() {
		transformedHaving := make([]exp.Expression, 0, len(having.Expressions()))
		for _, he := range having.Expressions() {
			transformed, err := ssg.transformGroupingExpression(he, gse, keys, present, bitmap)
			if err != nil {
				return nil, err
			}
			transformedHaving = append(transformedHaving, transformed)
		}
		branch = branch.HavingAppend(exp.NewExpressionList(having.Type(), transformedHaving...))
	}
	return branch, nil
}

// transformGroupingExpression rewrites one expression for a branch: GroupingID becomes the bitmap constant, missing
// keys become NULL and every other allowed node is rebuilt so identifiers and arguments keep their original order.
// transformContext carries the per-branch state shared while rewriting one expression tree.
type transformContext struct {
	gse     exp.GroupingSetsExpression
	keys    []exp.Expression
	present []bool
	bitmap  int
}

func (ssg *selectSQLGenerator) transformGroupingExpression(
	e exp.Expression,
	gse exp.GroupingSetsExpression,
	keys []exp.Expression,
	present []bool,
	bitmap int,
) (exp.Expression, error) {
	ctx := transformContext{gse: gse, keys: keys, present: present, bitmap: bitmap}
	return ssg.transformGroupingExpressionWith(e, ctx)
}

func (ssg *selectSQLGenerator) transformGroupingExpressionWith(e exp.Expression, ctx transformContext) (exp.Expression, error) {
	switch t := e.(type) {
	case exp.AliasedExpression:
		inner, err := ssg.transformGroupingExpressionWith(t.Aliased(), ctx)
		if err != nil {
			return nil, err
		}
		return exp.NewAliasExpression(inner, t.GetAs()), nil
	case exp.GroupingIDExpression:
		return exp.NewLiteralExpression(strconv.Itoa(ctx.bitmap)), nil
	case exp.GroupingKeyExpression:
		return ssg.missingOrKeptKey(t.Key(), ctx)
	case exp.IdentifierExpression:
		return ssg.missingOrKeptKey(t, ctx)
	case exp.SQLFunctionExpression:
		if isWhitelistedAggregate(t.Name()) {
			// Aggregates are copied verbatim, including COUNT(*): every branch runs the real aggregate over the
			// shared input, so grand totals on empty input keep their standard COUNT/SUM/AVG semantics.
			return t, nil
		}
		return nil, ErrGroupingSetsFunction(t.Name())
	case exp.LiteralExpression:
		return nil, ErrGroupingSetsLiteral()
	case exp.BooleanExpression:
		return ssg.transformBooleanGrouping(t, ctx)
	case exp.RangeExpression:
		return ssg.transformRangeGrouping(t, ctx)
	case exp.ExpressionList:
		return ssg.transformListGrouping(t, ctx)
	case exp.Ex, exp.ExOr:
		return ssg.transformMapGrouping(t, ctx)
	default:
		return nil, ErrGroupingSetsUnsupported(fmt.Sprintf("%T", e))
	}
}

func (ssg *selectSQLGenerator) transformBooleanGrouping(
	t exp.BooleanExpression,
	ctx transformContext,
) (exp.Expression, error) {
	lhs, err := ssg.transformGroupingExpressionWith(t.LHS(), ctx)
	if err != nil {
		return nil, err
	}
	rhs := t.RHS()
	if re, ok := rhs.(exp.Expression); ok {
		rhs, err = ssg.transformGroupingExpressionWith(re, ctx)
		if err != nil {
			return nil, err
		}
	}
	return exp.NewBooleanExpression(t.Op(), lhs, rhs), nil
}

func (ssg *selectSQLGenerator) transformRangeGrouping(t exp.RangeExpression, ctx transformContext) (exp.Expression, error) {
	lhs, err := ssg.transformGroupingExpressionWith(t.LHS(), ctx)
	if err != nil {
		return nil, err
	}
	start, err := ssg.transformRangeValue(t.RHS().Start(), ctx)
	if err != nil {
		return nil, err
	}
	end, err := ssg.transformRangeValue(t.RHS().End(), ctx)
	if err != nil {
		return nil, err
	}
	return exp.NewRangeExpression(t.Op(), lhs, exp.NewRangeVal(start, end)), nil
}

func (ssg *selectSQLGenerator) transformListGrouping(t exp.ExpressionList, ctx transformContext) (exp.Expression, error) {
	items := make([]exp.Expression, 0, len(t.Expressions()))
	for _, item := range t.Expressions() {
		transformed, err := ssg.transformGroupingExpressionWith(item, ctx)
		if err != nil {
			return nil, err
		}
		items = append(items, transformed)
	}
	return exp.NewExpressionList(t.Type(), items...), nil
}

func (ssg *selectSQLGenerator) transformMapGrouping(e exp.Expression, ctx transformContext) (exp.Expression, error) {
	list, err := expressionListFromMap(e)
	if err != nil {
		return nil, err
	}
	return ssg.transformGroupingExpressionWith(list, ctx)
}

func (ssg *selectSQLGenerator) transformRangeValue(val interface{}, ctx transformContext) (interface{}, error) {
	if e, ok := val.(exp.Expression); ok {
		return ssg.transformGroupingExpressionWith(e, ctx)
	}
	return val, nil
}

// missingOrKeptKey keeps a key that belongs to the current set or replaces it with the typed NULL of the branch.
func (ssg *selectSQLGenerator) missingOrKeptKey(key exp.Expression, ctx transformContext) (exp.Expression, error) {
	idx := indexOfGroupingKey(ctx.keys, key)
	if idx < 0 {
		return nil, ErrGroupingSetsUnknownColumn(describeKey(key))
	}
	if ctx.present[idx] {
		return key, nil
	}
	nullType := groupingKeyNullType(ctx.gse, ctx.keys[idx])
	if nullType == "" {
		return exp.NewLiteralExpression("NULL"), nil
	}
	return exp.NewLiteralExpression(fmt.Sprintf("CAST(NULL AS %s)", nullType)), nil
}

// validateGroupingSetsClauses enforces the restrictions that make the textual repetition of UNION ALL branches safe.
func (ssg *selectSQLGenerator) validateGroupingSetsClauses(clauses exp.SelectClauses, gse exp.GroupingSetsExpression) error {
	if err := validateGroupingSetClauseFlags(clauses); err != nil {
		return err
	}
	if err := ssg.validateGroupingSetSources(clauses); err != nil {
		return err
	}
	if err := validateGroupingSetProjections(clauses, gse); err != nil {
		return err
	}
	return validateGroupingSetOrder(clauses)
}

// validateGroupingSetClauseFlags rejects clauses that cannot be copied per branch or do not apply to a merged
// aggregate result.
func validateGroupingSetClauseFlags(clauses exp.SelectClauses) error {
	switch {
	case clauses.Distinct() != nil:
		return ErrGroupingSetsDistinct()
	case len(clauses.Compounds()) > 0:
		return ErrGroupingSetsCompound()
	case clauses.Lock() != nil:
		return ErrGroupingSetsLock()
	case len(clauses.Windows()) > 0 || clauses.Qualify() != nil:
		return ErrGroupingSetsWindow()
	case clauses.From() == nil || clauses.From().IsEmpty():
		return ErrGroupingSetsNoFrom()
	default:
		return nil
	}
}

// validateGroupingSetSources makes sure every input source is a table and the shared filter only contains pure
// expressions that are safe to re-evaluate per branch.
func (ssg *selectSQLGenerator) validateGroupingSetSources(clauses exp.SelectClauses) error {
	for _, src := range clauses.From().Columns() {
		if !isSimpleTableSource(src) {
			return ErrGroupingSetsSubquerySource()
		}
	}
	for _, join := range clauses.Joins() {
		if !isSimpleTableSource(join.Table()) {
			return ErrGroupingSetsSubquerySource()
		}
		if conditioned, ok := join.(exp.ConditionedJoinExpression); ok {
			if on, ok := conditioned.Condition().(exp.JoinOnCondition); ok && !on.IsEmpty() {
				if err := validateGroupingSetFilter(on.On()); err != nil {
					return err
				}
			}
		}
	}
	if where := clauses.Where(); where != nil && !where.IsEmpty() {
		return validateGroupingSetFilter(where)
	}
	return nil
}

// validateGroupingSetProjections whitelists the SELECT and HAVING expressions of every branch.
func validateGroupingSetProjections(clauses exp.SelectClauses, gse exp.GroupingSetsExpression) error {
	for _, col := range clauses.Select().Columns() {
		if err := validateGroupingSetSelectExpression(col, gse, false); err != nil {
			return err
		}
	}
	if having := clauses.Having(); having != nil && !having.IsEmpty() {
		for _, he := range having.Expressions() {
			if err := validateGroupingSetSelectExpression(he, gse, false); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateGroupingSetOrder requires global ORDER BY to reference output columns by name so it can be applied to
// the merged result.
func validateGroupingSetOrder(clauses exp.SelectClauses) error {
	if !clauses.HasOrder() {
		return nil
	}
	for _, ordered := range clauses.Order().Columns() {
		oe, ok := ordered.(exp.OrderedExpression)
		if !ok {
			return ErrGroupingSetsOrder()
		}
		if _, ok := oe.SortExpression().(exp.IdentifierExpression); !ok {
			return ErrGroupingSetsOrder()
		}
	}
	return nil
}

// validateGroupingSetSelectExpression whitelists grouping keys, GROUPING references and pure aggregates in the
// SELECT/HAVING expressions. insideAggregate is true while walking the arguments of a whitelisted aggregate.
func validateGroupingSetSelectExpression(e exp.Expression, gse exp.GroupingSetsExpression, insideAggregate bool) error {
	switch t := e.(type) {
	case exp.AliasedExpression:
		return validateGroupingSetSelectExpression(t.Aliased(), gse, insideAggregate)
	case exp.GroupingIDExpression:
		return validateGroupingIDColumns(t, gse)
	case exp.GroupingKeyExpression:
		return validateGroupingKey(t.Key(), gse, insideAggregate)
	case exp.IdentifierExpression:
		return validateGroupingKey(t, gse, insideAggregate)
	case exp.SQLWindowFunctionExpression:
		return ErrGroupingSetsWindow()
	case exp.SQLFunctionExpression:
		return validateAggregateFunction(t, gse)
	case exp.LiteralExpression:
		if insideAggregate && t.Literal() == "*" {
			return nil
		}
		return ErrGroupingSetsLiteral()
	case exp.BooleanExpression:
		return validateBooleanSelect(t, gse, insideAggregate)
	case exp.RangeExpression:
		return validateRangeSelect(t, gse, insideAggregate)
	case exp.ExpressionList:
		return validateExpressionListSelect(t, gse, insideAggregate)
	case exp.Ex, exp.ExOr:
		list, err := expressionListFromMap(t)
		if err != nil {
			return err
		}
		return validateGroupingSetSelectExpression(list, gse, insideAggregate)
	default:
		return ErrGroupingSetsUnsupported(fmt.Sprintf("%T", e))
	}
}

func validateGroupingKey(key exp.Expression, gse exp.GroupingSetsExpression, insideAggregate bool) error {
	if insideAggregate {
		return nil
	}
	return validateGroupingKeyReference(key, gse)
}

// validateAggregateFunction only accepts whitelisted pure aggregates and rejects DISTINCT and unsafe arguments.
func validateAggregateFunction(t exp.SQLFunctionExpression, gse exp.GroupingSetsExpression) error {
	name := strings.ToUpper(t.Name())
	if name == "DISTINCT" {
		return ErrGroupingSetsDistinct()
	}
	if !pureAggregateWhitelist[name] {
		return ErrGroupingSetsFunction(t.Name())
	}
	for _, arg := range t.Args() {
		if err := validateAggregateArgument(arg, gse); err != nil {
			return err
		}
	}
	return nil
}

func validateAggregateArgument(arg interface{}, gse exp.GroupingSetsExpression) error {
	ae, ok := arg.(exp.Expression)
	if !ok {
		return nil
	}
	if lit, isLit := ae.(exp.LiteralExpression); isLit {
		if lit.Literal() != "*" {
			return ErrGroupingSetsLiteral()
		}
		return nil
	}
	return validateGroupingSetSelectExpression(ae, gse, true)
}

func validateBooleanSelect(t exp.BooleanExpression, gse exp.GroupingSetsExpression, insideAggregate bool) error {
	if err := validateGroupingSetSelectExpression(t.LHS(), gse, insideAggregate); err != nil {
		return err
	}
	if re, ok := t.RHS().(exp.Expression); ok {
		return validateGroupingSetSelectExpression(re, gse, insideAggregate)
	}
	return nil
}

func validateRangeSelect(t exp.RangeExpression, gse exp.GroupingSetsExpression, insideAggregate bool) error {
	if err := validateGroupingSetSelectExpression(t.LHS(), gse, insideAggregate); err != nil {
		return err
	}
	for _, val := range []interface{}{t.RHS().Start(), t.RHS().End()} {
		if re, ok := val.(exp.Expression); ok {
			if err := validateGroupingSetSelectExpression(re, gse, insideAggregate); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateExpressionListSelect(t exp.ExpressionList, gse exp.GroupingSetsExpression, insideAggregate bool) error {
	for _, item := range t.Expressions() {
		if err := validateGroupingSetSelectExpression(item, gse, insideAggregate); err != nil {
			return err
		}
	}
	return nil
}

// validateGroupingSetFilter makes sure the input filter, which is copied into every branch, cannot contain function
// calls, custom literals or subqueries whose repeated evaluation would change semantics.
func validateGroupingSetFilter(e exp.Expression) error {
	switch t := e.(type) {
	case exp.IdentifierExpression, exp.GroupingKeyExpression:
		return nil
	case exp.AliasedExpression:
		return validateGroupingSetFilter(t.Aliased())
	case exp.SQLWindowFunctionExpression, exp.SQLFunctionExpression, exp.LiteralExpression,
		exp.AppendableExpression, exp.CaseExpression, exp.CastExpression, exp.ArrayExpression,
		exp.CollateExpression, exp.LateralExpression:
		return ErrGroupingSetsFilterFunction()
	case exp.BooleanExpression:
		return validateBooleanFilter(t)
	case exp.RangeExpression:
		return validateRangeFilter(t)
	case exp.ExpressionList:
		return validateExpressionListFilter(t)
	case exp.Ex, exp.ExOr:
		list, err := expressionListFromMap(t)
		if err != nil {
			return err
		}
		return validateGroupingSetFilter(list)
	default:
		return ErrGroupingSetsUnsupported(fmt.Sprintf("%T", e))
	}
}

func validateBooleanFilter(t exp.BooleanExpression) error {
	if err := validateGroupingSetFilter(t.LHS()); err != nil {
		return err
	}
	if re, ok := t.RHS().(exp.Expression); ok {
		return validateGroupingSetFilter(re)
	}
	return nil
}

func validateRangeFilter(t exp.RangeExpression) error {
	if err := validateGroupingSetFilter(t.LHS()); err != nil {
		return err
	}
	for _, val := range []interface{}{t.RHS().Start(), t.RHS().End()} {
		if re, ok := val.(exp.Expression); ok {
			if err := validateGroupingSetFilter(re); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateExpressionListFilter(t exp.ExpressionList) error {
	for _, item := range t.Expressions() {
		if err := validateGroupingSetFilter(item); err != nil {
			return err
		}
	}
	return nil
}

// expressionListFromMap expands an Ex/ExOr map into its AND/OR expression list.
func expressionListFromMap(e exp.Expression) (exp.ExpressionList, error) {
	switch t := e.(type) {
	case exp.Ex:
		return t.ToExpressions()
	case exp.ExOr:
		return t.ToExpressions()
	default:
		return nil, ErrGroupingSetsUnsupported(fmt.Sprintf("%T", e))
	}
}

func validateGroupingKeyReference(key exp.Expression, gse exp.GroupingSetsExpression) error {
	if indexOfGroupingKey(gse.Keys().Columns(), exp.UnwrapGroupingKey(key)) < 0 {
		return ErrGroupingSetsUnknownColumn(describeKey(key))
	}
	return nil
}

func validateGroupingIDColumns(gid exp.GroupingIDExpression, gse exp.GroupingSetsExpression) error {
	gidCols := gid.Columns().Columns()
	keys := gse.Keys().Columns()
	if len(gidCols) != len(keys) {
		return ErrGroupingIDKeyMismatch()
	}
	for i, col := range gidCols {
		if !exp.SameGroupingKey(col, keys[i]) {
			return ErrGroupingIDKeyMismatch()
		}
	}
	return nil
}

func isSimpleTableSource(e exp.Expression) bool {
	switch t := e.(type) {
	case exp.IdentifierExpression:
		return true
	case exp.AliasedExpression:
		_, ok := t.Aliased().(exp.IdentifierExpression)
		return ok
	default:
		return false
	}
}

func isWhitelistedAggregate(name string) bool {
	return pureAggregateWhitelist[strings.ToUpper(name)]
}

// newExpressionColumnList builds a ColumnListExpression from already normalized expressions.
func newExpressionColumnList(cols []exp.Expression) exp.ColumnListExpression {
	args := make([]interface{}, len(cols))
	for i, col := range cols {
		args[i] = col
	}
	return exp.NewColumnListExpression(args...)
}

func groupingSetColumns(set exp.GroupingSetExpression) []exp.Expression {
	cols := make([]exp.Expression, 0, len(set.Columns()))
	for _, col := range set.Columns() {
		cols = append(cols, exp.UnwrapGroupingKey(col))
	}
	return cols
}

func groupingSetPresence(set exp.GroupingSetExpression, keys []exp.Expression) []bool {
	present := make([]bool, len(keys))
	for _, col := range set.Columns() {
		if idx := indexOfGroupingKey(keys, exp.UnwrapGroupingKey(col)); idx >= 0 {
			present[idx] = true
		}
	}
	return present
}

func indexOfGroupingKey(keys []exp.Expression, key exp.Expression) int {
	for i, k := range keys {
		if exp.SameGroupingKey(k, key) {
			return i
		}
	}
	return -1
}

// groupingBitmap computes the SQL GROUPING bitmap: the first key is the most significant bit and a set bit means
// the key is aggregated away in the current branch.
func groupingBitmap(present []bool) int {
	bitmap := 0
	keyCount := len(present)
	for i := 0; i < keyCount; i++ {
		if !present[i] {
			bitmap |= 1 << (keyCount - 1 - i)
		}
	}
	return bitmap
}

func groupingKeyNullType(gse exp.GroupingSetsExpression, key exp.Expression) string {
	for _, set := range gse.Sets() {
		for _, col := range set.Columns() {
			if gk, ok := col.(exp.GroupingKeyExpression); ok && exp.SameGroupingKey(gk, key) {
				return gk.NullType()
			}
		}
	}
	return ""
}

func describeKey(e exp.Expression) string {
	if ident, ok := exp.UnwrapGroupingKey(e).(exp.IdentifierExpression); ok {
		if col, ok := ident.GetCol().(string); ok && col != "" {
			return col
		}
	}
	return fmt.Sprintf("%v", e)
}
