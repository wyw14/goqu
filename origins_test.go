package goqu_test

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/doug-martin/goqu/v9"
	"github.com/doug-martin/goqu/v9/exp"
	"github.com/doug-martin/goqu/v9/internal/sb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "github.com/doug-martin/goqu/v9/dialect/mysql"
	_ "github.com/doug-martin/goqu/v9/dialect/postgres"
	_ "github.com/doug-martin/goqu/v9/dialect/sqlite3"
	_ "github.com/doug-martin/goqu/v9/dialect/sqlserver"
)

// ---- helpers -----------------------------------------------------------------------------------------------------

func originToken(sql string, s *goqu.OriginSpan) string {
	return sql[s.Start:s.End]
}

func spansOfKind(m *goqu.OriginMap, kind goqu.OriginKind) []goqu.OriginSpan {
	var out []goqu.OriginSpan
	for _, s := range m.Spans() {
		if s.Kind == kind {
			out = append(out, s)
		}
	}
	return out
}

func spanWithPath(m *goqu.OriginMap, want ...string) (goqu.OriginSpan, bool) {
	for _, s := range m.Spans() {
		if len(s.Path) == len(want) {
			match := true
			for i := range want {
				if s.Path[i] != want[i] {
					match = false
					break
				}
			}
			if match {
				return s, true
			}
		}
	}
	return goqu.OriginSpan{}, false
}

func pathContains(s *goqu.OriginSpan, segment string) bool {
	for _, p := range s.Path {
		if p == segment {
			return true
		}
	}
	return false
}

// assertInvariants checks span bookkeeping that must hold for every successful generation.
func assertInvariants(t *testing.T, sql string, m *goqu.OriginMap) {
	t.Helper()
	require.False(t, m.Incomplete(), "map should be complete")
	require.NotEmpty(t, m.Spans())
	for _, s := range m.Spans() {
		require.GreaterOrEqual(t, s.Start, 0)
		require.Greater(t, s.End, s.Start, "zero-length span: %+v", s)
		require.LessOrEqual(t, s.End, len(sql), "span exceeds SQL length: %+v sql=%q", s, sql)
		require.NotEmpty(t, s.Path, "every span belongs to a statement path: %+v", s)
	}
	// the root scope spans the whole statement
	root, ok := spanWithPath(m, "select")
	if !ok {
		for _, stmt := range []string{"insert", "update", "delete", "truncate"} {
			if root, ok = spanWithPath(m, stmt); ok {
				break
			}
		}
	}
	require.True(t, ok, "root scope span not found")
	require.Equal(t, 0, root.Start)
	require.Equal(t, len(sql), root.End)
	require.Equal(t, 0, root.Scope)
	require.Equal(t, goqu.OriginScope, root.Kind)
}

// ---- 1. ordinary ToSQL output and args stay identical ------------------------------------------------------------

func TestToSQLWithOrigins_MatchesPlainToSQL(t *testing.T) {
	pg := goqu.Dialect("postgres")
	cases := []struct {
		name string
		ds   interface {
			ToSQL() (string, []any, error)
			ToSQLWithOrigins() (string, []any, *goqu.OriginMap, error)
		}
	}{
		{"select interpolated", pg.From("t").Where(goqu.C("a").Eq(int64(1)))},
		{"select prepared", pg.From("t").Prepared(true).Where(goqu.C("a").Eq(int64(1)), goqu.C("b").Eq("x"))},
		{"insert prepared", pg.Insert("t").Prepared(true).Rows(goqu.Record{"a": int64(1)})},
		{"update", pg.Update("t").Set(goqu.Record{"a": int64(2)}).Where(goqu.C("id").Eq(int64(1)))},
		{"delete", pg.Delete("t").Where(goqu.C("id").Eq(int64(1)))},
		{"truncate", pg.Truncate("t")},
		{"mysql prepared", goqu.Dialect("mysql").From("t").Prepared(true).Where(goqu.C("a").Eq(int64(1)))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantSQL, wantArgs, err := tc.ds.ToSQL()
			require.NoError(t, err)
			gotSQL, gotArgs, m, err := tc.ds.ToSQLWithOrigins()
			require.NoError(t, err)
			assert.Equal(t, wantSQL, gotSQL)
			assert.Equal(t, wantArgs, gotArgs)
			assertInvariants(t, gotSQL, m)
		})
	}
}

// ---- 2. multi-byte identifiers and escaping keep byte-accurate spans ----------------------------------------------

func TestToSQLWithOrigins_MultibyteAndEscapedIdentifiers(t *testing.T) {
	d := goqu.Dialect("postgres")
	sql, _, m, err := d.From("用户表").
		Select(goqu.C("名前"), goqu.I(`a"b`)).
		Where(goqu.C("名前").Eq("x")).
		ToSQLWithOrigins()
	require.NoError(t, err)
	assertInvariants(t, sql, m)

	var multibyte, escaped goqu.OriginSpan
	foundMB, foundEscaped := false, false
	for _, s := range spansOfKind(m, goqu.OriginExpression) {
		token := originToken(sql, &s)
		switch token {
		case `"名前"`:
			multibyte, foundMB = s, true
			// 2 quotes + 2 runes * 3 bytes = 8 bytes while only 4 runes
			assert.Equal(t, 8, s.End-s.Start)
			assert.Equal(t, 4, len([]rune(token)))
		case `"a""b"`:
			escaped, foundEscaped = s, true
			assert.Equal(t, `"a""b"`, token)
		}
	}
	require.True(t, foundMB, "multi-byte identifier span not found")
	require.True(t, foundEscaped, "escaped identifier span not found")

	// a byte position inside the multi-byte column token resolves to that expression, not the enclosing clause
	off := multibyte.Start + 2 // first byte of 名
	deep, ok := m.At(off)
	require.True(t, ok)
	assert.Equal(t, multibyte.ExprID, deep.ExprID)

	// the escaped identifier round-trips through its own span
	escapedDeep, ok := m.At(escaped.Start + 1)
	require.True(t, ok)
	assert.Equal(t, escaped.ExprID, escapedDeep.ExprID)
}

// ---- 3. reused expression = same id + occurrences; equal but distinct nodes get distinct ids ---------------------

func TestToSQLWithOrigins_ReusedVsDistinctExpressions(t *testing.T) {
	d := goqu.Dialect("postgres")

	shared := goqu.I("a")
	sql, _, m, err := d.From("t").Select(shared, shared).ToSQLWithOrigins()
	require.NoError(t, err)
	var reused []goqu.OriginSpan
	for _, s := range spansOfKind(m, goqu.OriginExpression) {
		if originToken(sql, &s) == `"a"` {
			reused = append(reused, s)
		}
	}
	require.Len(t, reused, 2)
	assert.Equal(t, reused[0].ExprID, reused[1].ExprID, "reused node shares one identity")
	assert.Equal(t, 0, reused[0].Occurrence)
	assert.Equal(t, 1, reused[1].Occurrence)
	assert.NotEqual(t, reused[0].Start, reused[1].Start, "occurrences point at different byte positions")

	sql2, _, m2, err := d.From("t").Select(goqu.I("a"), goqu.I("a")).ToSQLWithOrigins()
	require.NoError(t, err)
	var distinct []goqu.OriginSpan
	for _, s := range spansOfKind(m2, goqu.OriginExpression) {
		if originToken(sql2, &s) == `"a"` {
			distinct = append(distinct, s)
		}
	}
	require.Len(t, distinct, 2)
	assert.NotEqual(t, distinct[0].ExprID, distinct[1].ExprID,
		"structurally identical but separately built nodes must not be attributed to the first one")
}

// ---- 4. long numbered placeholders keep exact spans when the number gains digits ---------------------------------

func TestToSQLWithOrigins_LongNumberedPlaceholders(t *testing.T) {
	d := goqu.Dialect("postgres")
	vals := make([]any, 12)
	for i := range vals {
		vals[i] = int64(i + 1)
	}
	sql, args, m, err := d.From("t").Prepared(true).Where(goqu.C("c").In(vals...)).ToSQLWithOrigins()
	require.NoError(t, err)
	assert.Len(t, args, 12)
	assert.Contains(t, sql, "$12")

	argSpans := spansOfKind(m, goqu.OriginArg)
	require.Len(t, argSpans, 12)
	for i, s := range argSpans {
		want := fmt.Sprintf("$%d", i+1)
		assert.Equal(t, want, originToken(sql, &s), "arg %d token", i+1)
		assert.Equal(t, i+1, s.ArgIndex)
		assert.Equal(t, len(want), s.End-s.Start)
	}
	// single digit vs double digit widths
	assert.Equal(t, 2, argSpans[0].End-argSpans[0].Start)
	assert.Equal(t, 3, argSpans[9].End-argSpans[9].Start)
	assert.Equal(t, 3, argSpans[11].End-argSpans[11].Start)
}

// ---- 5. subqueries / CTE / compound retain parent-child scopes and paths -----------------------------------------

func TestToSQLWithOrigins_NestedScopesAndReusedDataset(t *testing.T) {
	d := goqu.Dialect("postgres")
	inner := d.From("orders").Select(goqu.C("id")).Where(goqu.C("id").Eq(int64(1)))

	sql, _, m, err := d.From("users").
		With("recent", inner).
		Select(goqu.C("id")).
		Where(goqu.C("id").In(inner)).
		ToSQLWithOrigins()
	require.NoError(t, err)
	assertInvariants(t, sql, m)

	// CTE path: root -> with -> cte[0] -> subquery -> where
	cteWhere, ok := spanWithPath(m, "select", "with", "cte[0]", "subquery", "where")
	require.True(t, ok, "CTE subquery where span missing")
	assert.Equal(t, 1, cteWhere.Scope)
	assert.Equal(t, 0, cteWhere.ParentScope)

	// the same dataset reused in the IN predicate opens another, separate subquery scope
	inWhere, ok := spanWithPath(m, "select", "where")
	require.True(t, ok)
	subScopes := map[int]bool{}
	for _, s := range m.Spans() {
		if pathContains(&s, "subquery") {
			subScopes[s.Scope] = true
		}
	}
	assert.GreaterOrEqual(t, len(subScopes), 2, "CTE body and IN subquery need distinct scopes")
	assert.Equal(t, 0, inWhere.Scope, "outer WHERE stays in the root scope")

	// compound: each side keeps its own scope
	sql2, _, m2, err := d.From("a").Select(goqu.C("x")).
		UnionAll(d.From("b").Select(goqu.C("x"))).ToSQLWithOrigins()
	require.NoError(t, err)
	assertInvariants(t, sql2, m2)
	compound, ok := findCompoundScope(m2)
	require.True(t, ok, "compound[0] subquery scope missing")
	assert.Equal(t, 0, compound.ParentScope)
}

func findCompoundScope(m *goqu.OriginMap) (goqu.OriginSpan, bool) {
	for _, s := range m.Spans() {
		if pathContains(&s, "compound[0]") && pathContains(&s, "subquery") && s.Kind == goqu.OriginScope {
			return s, true
		}
	}
	return goqu.OriginSpan{}, false
}

// ---- 6. dialect placeholder formats ------------------------------------------------------------------------------

func TestToSQLWithOrigins_PlaceholderDialects(t *testing.T) {
	cases := []struct {
		dialect, wantToken string
	}{
		{"postgres", "$1"},
		{"mysql", "?"},
		{"sqlite3", "?"},
		{"sqlserver", "@p1"},
	}
	for _, tc := range cases {
		t.Run(tc.dialect, func(t *testing.T) {
			sql, _, m, err := goqu.Dialect(tc.dialect).From("t").Prepared(true).
				Where(goqu.C("a").Eq(int64(1))).ToSQLWithOrigins()
			require.NoError(t, err)
			args := spansOfKind(m, goqu.OriginArg)
			require.Len(t, args, 1)
			assert.Equal(t, tc.wantToken, originToken(sql, &args[0]))
			assert.Equal(t, 1, args[0].ArgIndex)
		})
	}
}

// ---- 7. literals are opaque but parameter sub-expressions stay visible; no parameter values leak -----------------

func TestToSQLWithOrigins_LiteralOpaqueWithNestedExpressions(t *testing.T) {
	d := goqu.Dialect("postgres")

	sql, _, m, err := d.From("t").Select(goqu.L("(? + 1)", goqu.C("x"))).ToSQLWithOrigins()
	require.NoError(t, err)
	assertInvariants(t, sql, m)

	var opaque, nested goqu.OriginSpan
	for _, s := range m.Spans() {
		if s.Opaque && s.Kind == goqu.OriginOpaque && strings.Contains(originToken(sql, &s), "+ 1") {
			opaque = s
		}
		if originToken(sql, &s) == `"x"` {
			nested = s
		}
	}
	require.NotNil(t, opaque.Path, "opaque literal span missing")
	assert.Equal(t, `("x" + 1)`, originToken(sql, &opaque))
	require.NotNil(t, nested.Path)
	assert.True(t, nested.Start > opaque.Start && nested.End < opaque.End, "nested expression sits inside the opaque span")

	// deepest position inside the identifier resolves to the expression, not to the opaque text
	deep, ok := m.At(nested.Start + 1)
	require.True(t, ok)
	assert.Equal(t, nested.ExprID, deep.ExprID)
	assert.False(t, deep.Opaque)
	// a position on literal-only text (the "+ 1)" region after the identifier) resolves to the opaque span
	plus := strings.Index(sql[opaque.Start:], "+") + opaque.Start
	deepText, ok := m.At(plus)
	require.True(t, ok)
	assert.True(t, deepText.Opaque)

	// prepared literal: argument occurrence is recorded inside the opaque span without its value
	const secret = "super-secret-value"
	sql2, args2, m2, err := d.From("t").Prepared(true).
		Where(goqu.L("name = ?", secret)).ToSQLWithOrigins()
	require.NoError(t, err)
	assert.Equal(t, secret, args2[0], "args still carry the value for the driver")
	assert.NotContains(t, sql2, secret)
	argSpans := spansOfKind(m2, goqu.OriginArg)
	require.Len(t, argSpans, 1)
	assert.True(t, argSpans[0].Start >= opaqueRangeStart(m2))
	// Rendering every recorded field must never expose the parameter value; spans only carry positions,
	// identities and clause paths.
	assert.NotContains(t, fmt.Sprintf("%+v", m2.Spans()), secret,
		"origin diagnostics must not contain parameter values")
}

func opaqueRangeStart(m *goqu.OriginMap) int {
	for _, s := range m.Spans() {
		if s.Opaque {
			return s.Start
		}
	}
	return 0
}

// ---- 8. unknown custom AppendableExpression is reported as opaque, never with fabricated nodes -------------------

type originTestAppendable struct{}

func (originTestAppendable) Clone() exp.Expression           { return originTestAppendable{} }
func (originTestAppendable) Expression() exp.Expression      { return originTestAppendable{} }
func (originTestAppendable) GetAs() exp.IdentifierExpression { return nil }
func (originTestAppendable) ReturnsColumns() bool            { return true }
func (originTestAppendable) AppendSQL(b sb.SQLBuilder)       { b.WriteStrings("SELECT 999") }

func TestToSQLWithOrigins_CustomAppendableIsOpaque(t *testing.T) {
	d := goqu.Dialect("postgres")
	sql, _, m, err := d.From("t").Where(originTestAppendable{}).ToSQLWithOrigins()
	require.NoError(t, err)
	assert.Contains(t, sql, "SELECT 999")

	var opaque goqu.OriginSpan
	found := false
	for _, s := range m.Spans() {
		if s.Kind == goqu.OriginOpaque && originToken(sql, &s) == "SELECT 999" {
			opaque, found = s, true
		}
	}
	require.True(t, found, "custom appendable must produce an opaque span over its output")
	for i, s := range m.Spans() {
		if i == indexOfSpan(m, &opaque) {
			continue
		}
		if s.Start >= opaque.Start && s.End <= opaque.End {
			t.Fatalf("no fabricated inner nodes allowed inside opaque range, got %+v", s)
		}
	}
}

func indexOfSpan(m *goqu.OriginMap, target *goqu.OriginSpan) int {
	for i, s := range m.Spans() {
		if s.Start == target.Start && s.End == target.End && s.Kind == target.Kind {
			return i
		}
	}
	return -1
}

// ---- 9. simulated database error byte positions hit the deepest known source --------------------------------------

func TestToSQLWithOrigins_DatabaseErrorPositionResolvesDeepest(t *testing.T) {
	d := goqu.Dialect("postgres")
	inner := d.From("o").Select(goqu.C("id")).Where(goqu.C("id").Eq(int64(1)))
	sql, _, m, err := d.From("u").Select(goqu.C("id")).
		Where(goqu.C("id").In(inner)).ToSQLWithOrigins()
	require.NoError(t, err)

	// collect every rendered "id" identifier token; the one in the subquery WHERE must be distinguishable
	type hit struct {
		span goqu.OriginSpan
	}
	var outer, innerHit hit
	for _, s := range spansOfKind(m, goqu.OriginExpression) {
		if originToken(sql, &s) != `"id"` {
			continue
		}
		if pathContains(&s, "subquery") {
			innerHit = hit{s}
		} else if pathContains(&s, "where") {
			outer = hit{s}
		}
	}
	require.NotNil(t, outer.span.Path)
	require.NotNil(t, innerHit.span.Path)
	assert.NotEqual(t, outer.span.Start, innerHit.span.Start)

	// simulate a 1-based database error position pointing at the inner id (convert to a 0-based byte offset)
	dbPos := innerHit.span.Start + 1
	resolved, ok := m.At(dbPos - 1)
	require.True(t, ok)
	assert.Equal(t, innerHit.span.ExprID, resolved.ExprID)
	assert.Equal(t, innerHit.span.Scope, resolved.Scope)
	assert.True(t, pathContains(&resolved, "subquery"), "must resolve to the nested source, not the first text match")

	// the outer error position resolves to the root scope
	resolvedOuter, ok := m.At(outer.span.Start)
	require.True(t, ok)
	assert.Equal(t, 0, resolvedOuter.Scope)
	assert.False(t, pathContains(&resolvedOuter, "subquery"))
}

// ---- 10. render failure: error + incomplete diagnostics, no executable half-SQL -----------------------------------

func TestToSQLWithOrigins_RenderFailureIsIncomplete(t *testing.T) {
	d := goqu.Dialect("postgres")
	sql, args, m, err := d.From("t").Select(goqu.I("")).ToSQLWithOrigins()
	require.Error(t, err)
	assert.Equal(t, "", sql, "half-finished SQL must not be exposed")
	assert.Empty(t, args)
	require.NotNil(t, m)
	assert.True(t, m.Incomplete())
	require.NotNil(t, m.Err())
	assert.Equal(t, err.Error(), m.Err().Error())
}

// ---- 11. join indexing and on-condition paths ---------------------------------------------------------------------

func TestToSQLWithOrigins_JoinPaths(t *testing.T) {
	d := goqu.Dialect("postgres")
	sql, _, m, err := d.From("a").
		InnerJoin(goqu.T("b"), goqu.On(goqu.C("a_id").Eq(goqu.I("b.id")))).
		LeftJoin(goqu.T("c"), goqu.On(goqu.C("c_id").Eq(goqu.I("c.id")))).
		ToSQLWithOrigins()
	require.NoError(t, err)
	assertInvariants(t, sql, m)

	on0, ok := spanWithPath(m, "select", "join[0]", "on")
	require.True(t, ok)
	assert.Contains(t, originToken(sql, &on0), "ON")
	on1, ok := spanWithPath(m, "select", "join[1]", "on")
	require.True(t, ok)
	assert.Contains(t, originToken(sql, &on1), "ON")
}

// ---- 12. insert rows and arguments --------------------------------------------------------------------------------

func TestToSQLWithOrigins_InsertRows(t *testing.T) {
	d := goqu.Dialect("postgres")
	sql, args, m, err := d.Insert("t").Prepared(true).
		Rows(goqu.Record{"a": int64(1)}, goqu.Record{"a": int64(2)}).ToSQLWithOrigins()
	require.NoError(t, err)
	assert.Len(t, args, 2)
	assertInvariants(t, sql, m)

	row0, ok := spanWithPath(m, "insert", "values", "row[0]")
	require.True(t, ok)
	row1, ok := spanWithPath(m, "insert", "values", "row[1]")
	require.True(t, ok)
	assert.Contains(t, originToken(sql, &row0), "$1")
	assert.Contains(t, originToken(sql, &row1), "$2")
	assert.Len(t, spansOfKind(m, goqu.OriginArg), 2)
}

// ---- 13. update / delete / truncate smoke paths -------------------------------------------------------------------

func TestToSQLWithOrigins_OtherStatements(t *testing.T) {
	d := goqu.Dialect("postgres")

	sql, _, m, err := d.Update("t").Set(goqu.Record{"a": int64(2)}).Where(goqu.C("id").Eq(int64(1))).ToSQLWithOrigins()
	require.NoError(t, err)
	assertInvariants(t, sql, m)
	_, ok := spanWithPath(m, "update", "set")
	require.True(t, ok)
	_, ok = spanWithPath(m, "update", "where")
	require.True(t, ok)

	sql, _, m, err = d.Delete("t").Where(goqu.C("id").Eq(int64(1))).ToSQLWithOrigins()
	require.NoError(t, err)
	assertInvariants(t, sql, m)
	_, ok = spanWithPath(m, "delete", "where")
	require.True(t, ok)

	sql, _, m, err = d.Truncate("t").ToSQLWithOrigins()
	require.NoError(t, err)
	assertInvariants(t, sql, m)
	_, ok = spanWithPath(m, "truncate", "truncate_keyword")
	require.True(t, ok)
}

// ---- 14. pooled builders and concurrency never mix records --------------------------------------------------------

func TestToSQLWithOrigins_ConcurrentAndPoolReuse(t *testing.T) {
	d := goqu.Dialect("postgres")
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				n := int64(g*100_000 + i)
				// alternate origin and plain generation to stress pool reuse
				sql, args, m, err := d.From("t").Prepared(true).
					Where(goqu.C("a").Eq(n)).ToSQLWithOrigins()
				if !assert.NoError(t, err) || !assert.Len(t, args, 1) {
					return
				}
				if !assert.False(t, m.Incomplete()) {
					return
				}
				// every span must slice into this statement and its root must cover exactly this SQL
				for _, s := range m.Spans() {
					if s.End > len(sql) || originToken(sql, &s) == "" {
						t.Errorf("foreign/empty span %+v in sql %q", s, sql)
						return
					}
				}
				root, ok := spanWithPath(m, "select")
				if !assert.True(t, ok) || !assert.Equal(t, len(sql), root.End) {
					return
				}
				argSpans := spansOfKind(m, goqu.OriginArg)
				if !assert.Len(t, argSpans, 1) || !assert.Equal(t, "$1", originToken(sql, &argSpans[0])) {
					return
				}

				plainSQL, _, err := d.From("t").Prepared(true).Where(goqu.C("a").Eq(n)).ToSQL()
				if !assert.NoError(t, err) || !assert.Equal(t, plainSQL, sql) {
					return
				}
			}
		}(g)
	}
	wg.Wait()
}
