package exp

import (
	"database/sql/driver"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// Predicate optimization report reasons. They are stable strings so callers can branch on why a pass did not
// optimize a predicate.
const (
	// PredicateReasonApplied means at least one bounded rewrite was performed.
	PredicateReasonApplied = "applied"
	// PredicateReasonNoOpportunity means no duplicate pure atom or common pure factor was found.
	PredicateReasonNoOpportunity = "no_opportunity"
	// PredicateReasonEmpty means the input expression was nil or an empty expression list.
	PredicateReasonEmpty = "empty_predicate"
	// PredicateReasonNodeBudget means the input tree has more nodes than the budget allows.
	PredicateReasonNodeBudget = "node_budget_exceeded"
	// PredicateReasonCandidateBudget means the number of distinct factor candidates exceeded the budget.
	PredicateReasonCandidateBudget = "factor_candidate_budget_exceeded"
	// PredicateReasonGrowthBudget means the rewritten tree would grow beyond the allowed output growth.
	PredicateReasonGrowthBudget = "output_growth_budget_exceeded"
)

// Default budget values used by the SQL generators when SQLDialectOptions.OptimizePredicates is enabled. The rewrites
// only remove duplicated occurrences, so the optimized tree never has more nodes than the original one;
// DefaultMaxOutputGrowth being zero turns that invariant into an explicit safety check.
const (
	DefaultMaxPredicateNodes            = 256
	DefaultMaxPredicateFactorCandidates = 32
	DefaultMaxPredicateOutputGrowth     = 0
	// minSharedFactorMembers is the minimum number of opposite operator members that must contain a factor for it
	// to be extractable: a factor appearing once has nothing to be shared with.
	minSharedFactorMembers = 2
	// minRemainingMembers guarantees an affected member keeps at least one member besides the extracted factor, so
	// no vacuous TRUE/FALSE branch is ever synthesized.
	minRemainingMembers  = 2
	initialGroupCapacity = 3
)

// PredicateOptimizationBudget bounds a single OptimizePredicateExpression pass. A zero valued field is treated as the
// corresponding Default* constant, so an empty budget means "use all defaults".
type PredicateOptimizationBudget struct {
	// MaxNodes is the maximum number of expression nodes in the input tree. Larger trees are left untouched.
	MaxNodes int
	// MaxFactorCandidates is the maximum number of distinct common factor fingerprints inspected during the pass.
	// The tree is never expanded into DNF, so this stays proportional to the input node count.
	MaxFactorCandidates int
	// MaxOutputGrowth is the maximum allowed increase of the node count relative to the input tree.
	MaxOutputGrowth int
}

// DefaultPredicateOptimizationBudget returns the budget used by the built in SQL generators.
func DefaultPredicateOptimizationBudget() PredicateOptimizationBudget {
	return PredicateOptimizationBudget{
		MaxNodes:            DefaultMaxPredicateNodes,
		MaxFactorCandidates: DefaultMaxPredicateFactorCandidates,
		MaxOutputGrowth:     DefaultMaxPredicateOutputGrowth,
	}
}

func (b PredicateOptimizationBudget) withDefaults() PredicateOptimizationBudget {
	if b.MaxNodes <= 0 {
		b.MaxNodes = DefaultMaxPredicateNodes
	}
	if b.MaxFactorCandidates <= 0 {
		b.MaxFactorCandidates = DefaultMaxPredicateFactorCandidates
	}
	if b.MaxOutputGrowth < 0 {
		b.MaxOutputGrowth = DefaultMaxPredicateOutputGrowth
	}
	return b
}

// PredicateOptimizationReport describes what an optimization pass did. When Applied is false the original expression
// must be used unchanged; Reason explains why.
type PredicateOptimizationReport struct {
	Applied          bool
	Reason           string
	OriginalNodes    int
	OptimizedNodes   int
	FactorsExtracted int
	AtomsMerged      int
}

// OptimizePredicateExpression performs bounded common factor extraction and duplicate pure atom merging on a
// structured WHERE/HAVING predicate and returns a new expression tree.
//
// The rewrite only uses the boolean distributive laws, which hold for SQL three valued logic:
//
//	(A AND B) OR (A AND C)  -> A AND (B OR C)
//	(A OR B) AND (A OR C)   -> A OR (B AND C)
//
// A subtree is only moved or merged when it has a type aware fingerprint, i.e. it is built from identifier scopes and
// comparable literal values and contains no literal SQL, SQL function, driver.Valuer, subquery or other ordered
// opaque node. Opaque nodes keep their position, occurrence count and evaluation order, so the rewrite cannot change
// side effect counts or hide an evaluation error. No NOT/NULL simplification and no TRUE/FALSE constant folding is
// performed: operators are treated as opaque labels and only structurally identical subtrees are merged.
//
// The original tree is never mutated. On any budget violation the original expression is returned with Applied set
// to false.
func OptimizePredicateExpression(e Expression, budget PredicateOptimizationBudget) (Expression, PredicateOptimizationReport) {
	budget = budget.withDefaults()
	report := PredicateOptimizationReport{OriginalNodes: countPredicateNodes(e)}
	if e == nil {
		report.Reason = PredicateReasonEmpty
		return e, report
	}
	if report.OriginalNodes > budget.MaxNodes {
		report.Reason = PredicateReasonNodeBudget
		return e, report
	}
	optimizer := &predicateOptimizer{budget: budget}
	optimized, ok := optimizer.rewrite(e)
	if !ok {
		report.Reason = optimizer.reason
		return e, report
	}
	report.OptimizedNodes = countPredicateNodes(optimized)
	if report.OptimizedNodes > report.OriginalNodes+budget.MaxOutputGrowth {
		report.Reason = PredicateReasonGrowthBudget
		return e, report
	}
	report.FactorsExtracted = optimizer.factorsExtracted
	report.AtomsMerged = optimizer.atomsMerged
	if !optimizer.changed {
		report.Reason = PredicateReasonNoOpportunity
		return e, report
	}
	report.Applied = true
	report.Reason = PredicateReasonApplied
	return optimized, report
}

type predicateOptimizer struct {
	budget           PredicateOptimizationBudget
	reason           string
	changed          bool
	factorsExtracted int
	atomsMerged      int
	candidatesSeen   int
}

func (o *predicateOptimizer) abort(reason string) (Expression, bool) {
	o.reason = reason
	return nil, false
}

// rewrite rebuilds a node bottom up. Non list nodes are cloned (their value arguments are immutable, matching the
// existing Clone semantics) so the result never shares mutable structure with the input clauses.
//
// Structural rewrites (associative flattening, duplicate merging and factor extraction) only run on a list whose
// whole subtree is pure. A list containing an opaque node is rebuilt with the same shape and order after its
// children were optimized; the opaque list level itself is never reassociated or factored, so short circuit
// reachability and the evaluation count of every side effecting node stay identical.
func (o *predicateOptimizer) rewrite(e Expression) (Expression, bool) {
	el, ok := e.(ExpressionList)
	if !ok || el.IsEmpty() {
		return e.Clone(), true
	}
	_, fullyPure := fingerprintExpression(el)
	children := make([]Expression, 0, len(el.Expressions()))
	for _, child := range el.Expressions() {
		rewritten, ok := o.rewrite(child)
		if !ok {
			return nil, false
		}
		children = append(children, rewritten)
	}
	if !fullyPure {
		return NewExpressionList(el.Type(), children...), true
	}
	flattened, didFlatten := flattenSameType(el.Type(), children)
	children = flattened
	deduped, merged := dedupePureMembers(children)
	o.atomsMerged += merged
	result, ok := o.extractFactors(el.Type(), deduped)
	if !ok {
		return nil, false
	}
	if didFlatten || merged > 0 {
		o.changed = true
	}
	return result, true
}

// flattenSameType splices nested expression lists with the same operator into the parent. AND/OR are associative in
// three valued logic and the relative order of every node is preserved, so opaque barriers keep their ordering.
func flattenSameType(t ExpressionListType, children []Expression) ([]Expression, bool) {
	flattened := make([]Expression, 0, len(children))
	changed := false
	for _, child := range children {
		nested, ok := child.(ExpressionList)
		if ok && !nested.IsEmpty() && nested.Type() == t {
			flattened = append(flattened, nested.Expressions()...)
			changed = true
			continue
		}
		flattened = append(flattened, child)
	}
	return flattened, changed
}

// dedupePureMembers merges repeated fingerprint identical members of an associative list (x AND x = x, x OR x = x).
// Members without a fingerprint are opaque and are always kept, so two side effecting calls are never collapsed.
func dedupePureMembers(children []Expression) (deduped []Expression, merged int) {
	seen := make(map[string]struct{}, len(children))
	deduped = make([]Expression, 0, len(children))
	for _, child := range children {
		fp, ok := fingerprintExpression(child)
		if ok {
			if _, duplicate := seen[fp]; duplicate {
				merged++
				continue
			}
			seen[fp] = struct{}{}
		}
		deduped = append(deduped, child)
	}
	return deduped, merged
}

// extractFactors applies the distributive law to one list level and recurses so several nested factors can be
// extracted without ever expanding the tree into DNF. Only the affected members take part in the factored group;
// unaffected members stay at the current operator level so their reachability, evaluation order and side effect
// count are preserved.
func (o *predicateOptimizer) extractFactors(t ExpressionListType, children []Expression) (Expression, bool) {
	fp, affected, ok := o.chooseFactor(t, children)
	if !ok {
		return nil, false
	}
	if fp == "" {
		return NewExpressionList(t, children...), true
	}
	factor, ok := cloneFactorFromChildren(children, affected, fp)
	if !ok {
		return o.abort(PredicateReasonCandidateBudget)
	}
	opposite := oppositeExpressionListType(t)
	remainders := make([]Expression, 0, len(children))
	unaffected := make([]Expression, 0, len(children))
	for idx, child := range children {
		if _, isAffected := affected[idx]; !isAffected {
			// members without the factor must remain at the current operator level, never inside the factored group
			unaffected = append(unaffected, child)
			continue
		}
		sub := child.(ExpressionList)
		remaining := removeMemberByFingerprint(sub, fp)
		if len(remaining) == 1 {
			remainders = append(remainders, remaining[0])
		} else {
			remainders = append(remainders, NewExpressionList(opposite, remaining...))
		}
	}
	// Further factors may become extractable among the remainders of the affected members.
	inner, ok := o.extractFactors(t, remainders)
	if !ok {
		return nil, false
	}
	o.changed = true
	o.factorsExtracted++
	// (A AND B) OR (A AND C) becomes A AND (B OR C): the factor and the remainders are joined by the opposite
	// operator. Same operator nesting is flattened while building the group so the result is in normal form and a
	// second pass is a no-op.
	groupMembers := make([]Expression, 0, initialGroupCapacity)
	groupMembers = appendFlattenedMember(groupMembers, opposite, factor)
	groupMembers = appendFlattenedMember(groupMembers, opposite, inner)
	group := NewExpressionList(opposite, groupMembers...)
	if len(unaffected) == 0 {
		return group, true
	}
	// Keep every unaffected member at its original position and emit the factored group where the first affected
	// member stood, so an opaque node can never cross the group boundary.
	result := make([]Expression, 0, len(unaffected)+1)
	groupEmitted := false
	for idx, child := range children {
		if _, isAffected := affected[idx]; isAffected {
			if !groupEmitted {
				result = append(result, group)
				groupEmitted = true
			}
			continue
		}
		result = append(result, child)
	}
	return NewExpressionList(t, result...), true
}

// appendFlattenedMember splices same operator nested lists into their parent, relying on associativity while
// preserving node order.
func appendFlattenedMember(into []Expression, t ExpressionListType, e Expression) []Expression {
	if nested, ok := e.(ExpressionList); ok && !nested.IsEmpty() && nested.Type() == t {
		return append(into, nested.Expressions()...)
	}
	return append(into, e)
}

// chooseFactor returns the fingerprint of a pure factor shared by at least two opposite operator children, or an
// empty fingerprint when no factor can be extracted. A factor is only valid when every affected child keeps at least
// one other member, so the rewrite never synthesizes a vacuous TRUE/FALSE branch.
func (o *predicateOptimizer) chooseFactor(
	t ExpressionListType, children []Expression,
) (fp string, affected map[int]struct{}, ok bool) {
	opposite := oppositeExpressionListType(t)
	memberIndexes := map[string][]int{}
	for idx, child := range children {
		sub, ok := child.(ExpressionList)
		if !ok || sub.IsEmpty() || sub.Type() != opposite {
			continue
		}
		seenInMember := map[string]struct{}{}
		for _, member := range sub.Expressions() {
			fp, ok := fingerprintExpression(member)
			if !ok {
				continue
			}
			if _, duplicate := seenInMember[fp]; duplicate {
				continue
			}
			seenInMember[fp] = struct{}{}
			memberIndexes[fp] = append(memberIndexes[fp], idx)
		}
	}
	candidates := make([]string, 0, len(memberIndexes))
	for candidateFP, indexes := range memberIndexes {
		if len(indexes) >= minSharedFactorMembers {
			candidates = append(candidates, candidateFP)
		}
	}
	if len(candidates) == 0 {
		return "", nil, true
	}
	if o.candidatesSeen+len(candidates) > o.budget.MaxFactorCandidates {
		return o.abortFactorChoice()
	}
	o.candidatesSeen += len(candidates)
	sort.Strings(candidates)
	for _, fp := range candidates {
		affected := make(map[int]struct{}, len(memberIndexes[fp]))
		valid := true
		for _, idx := range memberIndexes[fp] {
			sub := children[idx].(ExpressionList)
			if len(sub.Expressions()) < minRemainingMembers {
				valid = false
				break
			}
			affected[idx] = struct{}{}
		}
		if valid {
			return fp, affected, true
		}
	}
	return "", nil, true
}

func (o *predicateOptimizer) abortFactorChoice() (fp string, affected map[int]struct{}, ok bool) {
	o.reason = PredicateReasonCandidateBudget
	return "", nil, false
}

func cloneFactorFromChildren(children []Expression, affected map[int]struct{}, fp string) (Expression, bool) {
	for idx := range children {
		if _, ok := affected[idx]; !ok {
			continue
		}
		sub := children[idx].(ExpressionList)
		for _, member := range sub.Expressions() {
			memberFP, ok := fingerprintExpression(member)
			if ok && memberFP == fp {
				return member.Clone(), true
			}
		}
	}
	return nil, false
}

// removeMemberByFingerprint removes one fingerprint matching member while preserving the order of the remaining
// members, including opaque ones.
func removeMemberByFingerprint(sub ExpressionList, fp string) []Expression {
	remaining := make([]Expression, 0, len(sub.Expressions())-1)
	removed := false
	for _, member := range sub.Expressions() {
		if !removed {
			memberFP, ok := fingerprintExpression(member)
			if ok && memberFP == fp {
				removed = true
				continue
			}
		}
		remaining = append(remaining, member)
	}
	return remaining
}

func oppositeExpressionListType(t ExpressionListType) ExpressionListType {
	if t == AndType {
		return OrType
	}
	return AndType
}

// fingerprint writer tokens. Every token is framed by a tag, its payload length and the payload, so arbitrary binary
// and string contents cannot collide with framing or with each other.
const (
	fpLengthSep    = ':'
	fpTokenNil     = 'n'
	fpTokenValue   = 'v'
	fpTokenString  = 's'
	fpTokenSlice   = 'l'
	fpTokenIdent   = 'I'
	fpTokenBoolean = 'B'
	fpTokenRange   = 'R'
	fpTokenList    = 'L'
	fpFloatBase    = 16
	fpFloatBitSize = 64
	fpBoolTrue     = "1"
	fpBoolFalse    = "0"
)

type fingerprintWriter struct {
	b strings.Builder
}

func (w *fingerprintWriter) token(tag byte, payload string) {
	w.b.WriteByte(tag)
	w.b.WriteString(strconv.Itoa(len(payload)))
	w.b.WriteByte(fpLengthSep)
	w.b.WriteString(payload)
}

func (w *fingerprintWriter) text(s string) {
	w.b.WriteString(strconv.Itoa(len(s)))
	w.b.WriteByte(fpLengthSep)
	w.b.WriteString(s)
}

func (w *fingerprintWriter) String() string {
	return w.b.String()
}

// finish wraps the payload accumulated in the writer into a single self delimiting token.
func (w *fingerprintWriter) finish(tag byte) string {
	payload := w.b.String()
	var out fingerprintWriter
	out.token(tag, payload)
	return out.String()
}

// fingerprintExpression returns the canonical identity of a pure, order independent expression. The second return
// value is false for opaque nodes (literal SQL, functions, subqueries, casts, cases, maps, ...), which must never be
// reordered or merged.
func fingerprintExpression(e Expression) (string, bool) {
	switch t := e.(type) {
	case IdentifierExpression:
		col, ok := t.GetCol().(string)
		if !ok {
			return "", false
		}
		w := &fingerprintWriter{}
		w.text(t.GetSchema())
		w.text(t.GetTable())
		w.text(col)
		return w.finish(fpTokenIdent), true
	case BooleanExpression:
		lhs, ok := fingerprintExpression(t.LHS())
		if !ok {
			return "", false
		}
		rhs, ok := fingerprintValue(t.RHS())
		if !ok {
			return "", false
		}
		w := &fingerprintWriter{}
		w.text(strconv.Itoa(int(t.Op())))
		w.b.WriteString(lhs)
		w.b.WriteString(rhs)
		return w.finish(fpTokenBoolean), true
	case RangeExpression:
		lhs, ok := fingerprintExpression(t.LHS())
		if !ok {
			return "", false
		}
		start, ok := fingerprintValue(t.RHS().Start())
		if !ok {
			return "", false
		}
		end, ok := fingerprintValue(t.RHS().End())
		if !ok {
			return "", false
		}
		w := &fingerprintWriter{}
		w.text(strconv.Itoa(int(t.Op())))
		w.b.WriteString(lhs)
		w.b.WriteString(start)
		w.b.WriteString(end)
		return w.finish(fpTokenRange), true
	case ExpressionList:
		w := &fingerprintWriter{}
		w.text(strconv.Itoa(int(t.Type())))
		for _, member := range t.Expressions() {
			memberFP, ok := fingerprintExpression(member)
			if !ok {
				return "", false
			}
			w.b.WriteString(memberFP)
		}
		return w.finish(fpTokenList), true
	default:
		return "", false
	}
}

// fingerprintValue returns the canonical identity of a comparable argument value. The exact dynamic Go type is part
// of the identity, so int64(1) and "1", []byte{1} and []int{1} or json.RawMessage never compare equal. driver.Valuers
// are never invoked and all non comparable kinds make the value opaque. No fmt.Sprintf key is produced for
// incomparable objects.
func fingerprintValue(v any) (string, bool) {
	if v == nil {
		w := &fingerprintWriter{}
		w.token(fpTokenNil, "")
		return w.String(), true
	}
	if _, ok := v.(driver.Valuer); ok {
		return "", false
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Float32, reflect.Float64:
		return fingerprintScalar(rv)
	case reflect.String:
		w := &fingerprintWriter{}
		w.text(rv.Type().String())
		w.text(rv.String())
		return w.finish(fpTokenString), true
	case reflect.Slice, reflect.Array:
		return fingerprintSequence(rv)
	default:
		return "", false
	}
}

func fingerprintScalar(rv reflect.Value) (string, bool) {
	w := &fingerprintWriter{}
	w.text(rv.Type().String())
	switch rv.Kind() {
	case reflect.Bool:
		if rv.Bool() {
			w.text(fpBoolTrue)
		} else {
			w.text(fpBoolFalse)
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		w.text(strconv.FormatInt(rv.Int(), 10))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		w.text(strconv.FormatUint(rv.Uint(), 10))
	case reflect.Float32:
		w.text(strconv.FormatUint(uint64(math.Float32bits(float32(rv.Float()))), fpFloatBase))
	case reflect.Float64:
		w.text(strconv.FormatUint(math.Float64bits(rv.Float()), fpFloatBase))
	default:
		return "", false
	}
	return w.finish(fpTokenValue), true
}

func fingerprintSequence(rv reflect.Value) (string, bool) {
	w := &fingerprintWriter{}
	w.text(rv.Type().String())
	w.text(strconv.Itoa(rv.Len()))
	for i := 0; i < rv.Len(); i++ {
		elementFP, ok := fingerprintValue(rv.Index(i).Interface())
		if !ok {
			return "", false
		}
		w.b.WriteString(elementFP)
	}
	return w.finish(fpTokenSlice), true
}

// countPredicateNodes gives a stable, bounded size metric for the budget checks. Unknown leaf expressions count as a
// single node and their non expression arguments are not reflected over, which keeps the walk linear.
func countPredicateNodes(e Expression) int {
	if e == nil {
		return 0
	}
	count := 1
	switch t := e.(type) {
	case ExpressionList:
		for _, child := range t.Expressions() {
			count += countPredicateNodes(child)
		}
	case ColumnListExpression:
		for _, child := range t.Columns() {
			count += countPredicateNodes(child)
		}
	case BooleanExpression:
		count += countPredicateNodes(t.LHS())
		count += countValueNodes(t.RHS())
	case RangeExpression:
		count += countPredicateNodes(t.LHS())
		count += countValueNodes(t.RHS().Start())
		count += countValueNodes(t.RHS().End())
	case AliasedExpression:
		count += countPredicateNodes(t.Aliased())
	case CastExpression:
		count += countPredicateNodes(t.Casted())
	case CollateExpression:
		count += countPredicateNodes(t.Collated())
	case BitwiseExpression:
		count += countPredicateNodes(t.LHS())
		count += countValueNodes(t.RHS())
	case OrderedExpression:
		count += countPredicateNodes(t.SortExpression())
	case SQLFunctionExpression:
		for _, arg := range t.Args() {
			count += countValueNodes(arg)
		}
	case LiteralExpression:
		for _, arg := range t.Args() {
			count += countValueNodes(arg)
		}
	}
	return count
}

func countValueNodes(v any) int {
	switch t := v.(type) {
	case Expression:
		return countPredicateNodes(t)
	case []any:
		count := 0
		for _, element := range t {
			count += countValueNodes(element)
		}
		return count
	case []Expression:
		count := 0
		for _, element := range t {
			count += countPredicateNodes(element)
		}
		return count
	default:
		return 0
	}
}
