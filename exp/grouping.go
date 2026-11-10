package exp

import (
	"fmt"
	"reflect"
	"regexp"
)

// safeNullTypePattern restricts the CAST target of a synthesized NULL to simple SQL type names so a user supplied
// type can never inject SQL fragments into the UNION ALL lowering.
var safeNullTypePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_ ]*$`)

type (
	// GroupingKeyExpression wraps a grouping key with an optional SQL type. The type is only used by the UNION ALL
	// lowering of a dialect without native GROUPING SETS: a branch that aggregates the key away selects
	// CAST(NULL AS <type>) instead of the key. Without a type a plain, type-agnostic NULL is emitted.
	GroupingKeyExpression interface {
		Expression
		// Key is the grouping column being wrapped.
		Key() Expression
		// NullType is the SQL type used for the NULL emitted when the key is aggregated away.
		// An empty string means a plain NULL.
		NullType() string
	}

	// GroupingSetExpression is one set of a GROUPING SETS clause. An empty set () is the grand total set.
	GroupingSetExpression interface {
		Expression
		// Columns returns the grouping keys of this set, in declaration order.
		Columns() []Expression
		// IsEmpty returns true for the grand total set ().
		IsEmpty() bool
	}

	// GroupingSetsExpression explicitly declares the grouping keys and the finite collection of grouping sets
	// rendered by a GROUP BY GROUPING SETS clause. Duplicate sets are preserved because GROUPING SETS repeats the
	// rows of a set as many times as it is listed.
	GroupingSetsExpression interface {
		Expression
		// Sets returns the grouping sets in declaration order.
		Sets() []GroupingSetExpression
		// Keys returns the union of every set's keys, in first-seen order.
		Keys() ColumnListExpression
		// MaxBranches is the maximum number of branches the UNION ALL lowering may generate for this expression.
		// Zero means the dialect default applies.
		MaxBranches() int
	}

	// GroupingIDExpression renders the SQL GROUPING(key, ...) bitmap. A dialect without native GROUPING SETS
	// replaces it, while lowering, with the constant bitmap of the branch being rendered.
	GroupingIDExpression interface {
		Expression
		Aliaseable
		Comparable
		Orderable
		Isable
		Inable
		Likeable
		Rangeable
		// Columns returns the grouping keys the bitmap is computed over, in order.
		Columns() ColumnListExpression
	}

	groupingKey struct {
		key      Expression
		nullType string
	}

	groupingSet struct {
		columns []Expression
	}

	groupingSets struct {
		sets        []GroupingSetExpression
		keys        ColumnListExpression
		maxBranches int
	}

	groupingID struct {
		columns ColumnListExpression
	}
)

// NewGroupingKeyExpression wraps a grouping key (string or IdentifierExpression) with the SQL type used when the
// key is aggregated away in a UNION ALL lowering. The type is validated when the SQL is generated so it can never
// contain SQL fragments.
func NewGroupingKeyExpression(col interface{}, nullType string) GroupingKeyExpression {
	key := normalizeGroupingColumn(col)
	if nullType != "" && !safeNullTypePattern.MatchString(nullType) {
		panic(fmt.Sprintf("Cannot use %q as a grouping key NULL type: only letters, digits, '_' and spaces are allowed", nullType))
	}
	return groupingKey{key: key, nullType: nullType}
}

func (gk groupingKey) Clone() Expression {
	return groupingKey{key: gk.key.Clone(), nullType: gk.nullType}
}

func (gk groupingKey) Expression() Expression { return gk }
func (gk groupingKey) Key() Expression        { return gk.key }
func (gk groupingKey) NullType() string       { return gk.nullType }

// NewGroupingSetExpression creates one grouping set. Pass no columns for the grand total set (). Strings are parsed
// as identifiers, IdentifierExpressions are kept as is and GroupingKeyExpression carries an optional NULL type.
func NewGroupingSetExpression(cols ...interface{}) GroupingSetExpression {
	columns := make([]Expression, 0, len(cols))
	for _, col := range cols {
		columns = append(columns, normalizeGroupingColumn(col))
	}
	return groupingSet{columns: columns}
}

func (gs groupingSet) Clone() Expression {
	columns := make([]Expression, 0, len(gs.columns))
	for _, col := range gs.columns {
		columns = append(columns, col.Clone())
	}
	return groupingSet{columns: columns}
}

func (gs groupingSet) Expression() Expression   { return gs }
func (gs groupingSet) Columns() []Expression     { return gs.columns }
func (gs groupingSet) IsEmpty() bool             { return len(gs.columns) == 0 }

// NewGroupingSetsExpression creates the finite grouping set collection. The union of the keys of all sets, in
// first-seen order, is available through Keys. Duplicate sets are kept on purpose. maxBranches of zero falls back to
// the dialect branch budget.
func NewGroupingSetsExpression(maxBranches int, sets ...GroupingSetExpression) GroupingSetsExpression {
	keyCols := []Expression{}
	for _, set := range sets {
		for _, col := range set.Columns() {
			key := UnwrapGroupingKey(col)
			found := false
			for _, existing := range keyCols {
				if SameGroupingKey(existing, key) {
					found = true
					break
				}
			}
			if !found {
				keyCols = append(keyCols, key)
			}
		}
	}
	keyArgs := make([]interface{}, len(keyCols))
	for i, key := range keyCols {
		keyArgs[i] = key
	}
	return groupingSets{
		sets:        append([]GroupingSetExpression(nil), sets...),
		keys:        NewColumnListExpression(keyArgs...),
		maxBranches: maxBranches,
	}
}

func (gs groupingSets) Clone() Expression {
	sets := make([]GroupingSetExpression, 0, len(gs.sets))
	for _, set := range gs.sets {
		sets = append(sets, set.Clone().(GroupingSetExpression))
	}
	return groupingSets{sets: sets, keys: gs.keys.Clone().(ColumnListExpression), maxBranches: gs.maxBranches}
}

func (gs groupingSets) Expression() Expression { return gs }
func (gs groupingSets) Sets() []GroupingSetExpression {
	return gs.sets
}
func (gs groupingSets) Keys() ColumnListExpression { return gs.keys }
func (gs groupingSets) MaxBranches() int           { return gs.maxBranches }

// NewGroupingIDExpression creates a GROUPING(key, ...) bitmap reference over the given grouping keys. The keys must
// be the ones declared by the enclosing GroupingSetsExpression when the statement is lowered.
func NewGroupingIDExpression(cols ...interface{}) GroupingIDExpression {
	return groupingID{columns: NewColumnListExpression(cols...)}
}

func (gi groupingID) Clone() Expression {
	return groupingID{columns: gi.columns.Clone().(ColumnListExpression)}
}

func (gi groupingID) Expression() Expression { return gi }
func (gi groupingID) Columns() ColumnListExpression {
	return gi.columns
}

func (gi groupingID) As(val interface{}) AliasedExpression { return NewAliasExpression(gi, val) }
func (gi groupingID) Eq(val interface{}) BooleanExpression  { return eq(gi, val) }
func (gi groupingID) Neq(val interface{}) BooleanExpression { return neq(gi, val) }
func (gi groupingID) Gt(val interface{}) BooleanExpression  { return gt(gi, val) }
func (gi groupingID) Gte(val interface{}) BooleanExpression { return gte(gi, val) }
func (gi groupingID) Lt(val interface{}) BooleanExpression  { return lt(gi, val) }
func (gi groupingID) Lte(val interface{}) BooleanExpression { return lte(gi, val) }
func (gi groupingID) Between(val RangeVal) RangeExpression  { return between(gi, val) }
func (gi groupingID) NotBetween(val RangeVal) RangeExpression {
	return notBetween(gi, val)
}
func (gi groupingID) Like(val interface{}) BooleanExpression    { return like(gi, val) }
func (gi groupingID) NotLike(val interface{}) BooleanExpression { return notLike(gi, val) }
func (gi groupingID) ILike(val interface{}) BooleanExpression   { return iLike(gi, val) }
func (gi groupingID) NotILike(val interface{}) BooleanExpression {
	return notILike(gi, val)
}
func (gi groupingID) RegexpLike(val interface{}) BooleanExpression {
	return regexpLike(gi, val)
}
func (gi groupingID) RegexpNotLike(val interface{}) BooleanExpression {
	return regexpNotLike(gi, val)
}
func (gi groupingID) RegexpILike(val interface{}) BooleanExpression {
	return regexpILike(gi, val)
}
func (gi groupingID) RegexpNotILike(val interface{}) BooleanExpression {
	return regexpNotILike(gi, val)
}
func (gi groupingID) In(vals ...interface{}) BooleanExpression { return in(gi, vals...) }
func (gi groupingID) NotIn(vals ...interface{}) BooleanExpression {
	return notIn(gi, vals...)
}
func (gi groupingID) Is(val interface{}) BooleanExpression    { return is(gi, val) }
func (gi groupingID) IsNot(val interface{}) BooleanExpression { return isNot(gi, val) }
func (gi groupingID) IsNull() BooleanExpression               { return is(gi, nil) }
func (gi groupingID) IsNotNull() BooleanExpression            { return isNot(gi, nil) }
func (gi groupingID) IsTrue() BooleanExpression                { return is(gi, true) }
func (gi groupingID) IsNotTrue() BooleanExpression             { return isNot(gi, true) }
func (gi groupingID) IsFalse() BooleanExpression               { return is(gi, false) }
func (gi groupingID) IsNotFalse() BooleanExpression            { return isNot(gi, false) }
func (gi groupingID) Asc() OrderedExpression                   { return asc(gi) }
func (gi groupingID) Desc() OrderedExpression                  { return desc(gi) }

// normalizeGroupingColumn turns a string or identifier into a grouping key expression.
func normalizeGroupingColumn(col interface{}) Expression {
	switch c := col.(type) {
	case nil:
		panic("Cannot create a grouping key from nil")
	case string:
		return ParseIdentifier(c)
	case IdentifierExpression:
		return c
	case GroupingKeyExpression:
		return c
	default:
		panic(fmt.Sprintf("Cannot create grouping key from %+v", col))
	}
}

// UnwrapGroupingKey returns the underlying key expression when e is a GroupingKeyExpression, and e itself otherwise.
func UnwrapGroupingKey(e Expression) Expression {
	if gk, ok := e.(GroupingKeyExpression); ok {
		return gk.Key()
	}
	return e
}

// SameGroupingKey reports whether two grouping key expressions reference the same column. GroupingKeyExpression
// wrappers are unwrapped first and identifiers are compared by their schema, table and column parts.
func SameGroupingKey(a, b Expression) bool {
	ai, aok := UnwrapGroupingKey(a).(IdentifierExpression)
	bi, bok := UnwrapGroupingKey(b).(IdentifierExpression)
	if !aok || !bok {
		return false
	}
	return ai.GetSchema() == bi.GetSchema() &&
		ai.GetTable() == bi.GetTable() &&
		reflect.DeepEqual(ai.GetCol(), bi.GetCol())
}
