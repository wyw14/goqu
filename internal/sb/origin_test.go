package sb //nolint:testpackage // exercises unexported recorder state

import (
	"errors"
	"testing"
)

// Spans are byte ranges: multi-byte runes advance the recorder by their UTF-8 byte length, not by one.
func TestOriginRecorder_ByteOffsetsWithMultibyteRunes(t *testing.T) {
	b, m := NewSQLBuilderWithOrigins(false)
	PushFrame(b, FrameSpec{Segment: "root", Kind: OriginScope, OpensScope: true})
	b.WriteStrings("SELECT ") // 7 bytes
	PushFrame(b, FrameSpec{Segment: "col", Kind: OriginClause})
	b.WriteRunes('名', '前') // 2 runes, 6 UTF-8 bytes
	PopFrame(b)
	b.WriteRunes(' ')
	PopFrame(b)
	m.Finalize()

	spans := m.Spans()
	if len(spans) != 2 {
		t.Fatalf("expected 2 spans, got %d: %+v", len(spans), spans)
	}
	root, col := spans[0], spans[1]
	if root.Start != 0 || root.End != 14 {
		t.Fatalf("root span = [%d,%d), want [0,14)", root.Start, root.End)
	}
	if col.Start != 7 || col.End != 13 {
		t.Fatalf("col span = [%d,%d), want [7,13) (6 bytes for two 3-byte runes)", col.Start, col.End)
	}
	if got, ok := m.At(10); !ok || got.End != 13 {
		t.Fatalf("At(10) should resolve inside the col span, got %+v ok=%v", got, ok)
	}
}

// At must return the innermost span for an offset and reject offsets outside the statement.
func TestOriginRecorder_AtReturnsDeepestSpan(t *testing.T) {
	b, m := NewSQLBuilderWithOrigins(false)
	PushFrame(b, FrameSpec{Segment: "select", Kind: OriginScope, OpensScope: true})
	b.WriteStrings("WHERE ")
	PushFrame(b, FrameSpec{Segment: "where", Kind: OriginClause})
	b.WriteStrings("(")
	PushFrame(b, FrameSpec{Kind: OriginExpression, Identity: 1})
	b.WriteStrings("a")
	PopFrame(b)
	b.WriteStrings("=")
	PushFrame(b, FrameSpec{Kind: OriginArg, ArgIndex: 1})
	b.WriteStrings("$1")
	PopFrame(b)
	b.WriteStrings(")")
	PopFrame(b)
	PopFrame(b)
	m.Finalize()

	// "WHERE (a=$1)": 'a' is at byte 7 and the "$1" placeholder spans bytes [9,11).
	deep, ok := m.At(9) // inside "$1"
	if !ok {
		t.Fatal("expected a span at offset 9")
	}
	if deep.Kind != OriginArg || deep.ArgIndex != 1 {
		t.Fatalf("At(9) = %+v, want arg span #1", deep)
	}
	mid, ok := m.At(7) // inside "a"
	if !ok || mid.Kind != OriginExpression || mid.ExprID != 1 {
		t.Fatalf("At(7) = %+v ok=%v, want expression span", mid, ok)
	}
	if _, ok := m.At(99); ok {
		t.Fatal("out-of-range offset must not resolve to any span")
	}
}

// Reusing the same identity shares an ExprID and gets separate occurrences; distinct identities never alias.
func TestOriginRecorder_ExpressionIdentityAndOccurrences(t *testing.T) {
	b, m := NewSQLBuilderWithOrigins(false)
	PushFrame(b, FrameSpec{Segment: "select", Kind: OriginScope, OpensScope: true})

	PushFrame(b, FrameSpec{Kind: OriginExpression, Identity: 100})
	b.WriteStrings("a")
	PopFrame(b)
	PushFrame(b, FrameSpec{Kind: OriginExpression, Identity: 100})
	b.WriteStrings("a")
	PopFrame(b)
	PushFrame(b, FrameSpec{Kind: OriginExpression, Identity: 200})
	b.WriteStrings("a")
	PopFrame(b)

	PopFrame(b)
	m.Finalize()

	var exprs []OriginSpan
	for _, s := range m.Spans() {
		if s.Kind == OriginExpression {
			exprs = append(exprs, s)
		}
	}
	if len(exprs) != 3 {
		t.Fatalf("expected 3 expression spans, got %d: %+v", len(exprs), exprs)
	}
	first, second, third := exprs[0], exprs[1], exprs[2]
	if first.ExprID != second.ExprID || first.Occurrence != 0 || second.Occurrence != 1 {
		t.Fatalf("reused node: ids=%d/%d occurrences=%d/%d, want same id with occurrences 0,1",
			first.ExprID, second.ExprID, first.Occurrence, second.Occurrence)
	}
	if third.ExprID == first.ExprID {
		t.Fatalf("separate node identity %d must not alias reused node %d", third.ExprID, first.ExprID)
	}
}

// Scopes form a parent chain starting at 0, and spans record the scope they were rendered in.
func TestOriginRecorder_ScopeParentChain(t *testing.T) {
	b, m := NewSQLBuilderWithOrigins(false)
	PushFrame(b, FrameSpec{Segment: "select", Kind: OriginScope, OpensScope: true})
	b.WriteStrings("SELECT * FROM ")
	PushFrame(b, FrameSpec{Segment: "subquery", Kind: OriginScope, OpensScope: true})
	b.WriteStrings("SELECT 1")
	PushFrame(b, FrameSpec{Segment: "where", Kind: OriginClause})
	b.WriteStrings(" WHERE x")
	PopFrame(b)
	PopFrame(b)
	PopFrame(b)
	m.Finalize()

	var root, sub, where OriginSpan
	for _, s := range m.Spans() {
		switch {
		case len(s.Path) == 1 && s.Path[0] == "select":
			root = s
		case len(s.Path) == 2 && s.Path[1] == "subquery":
			sub = s
		case len(s.Path) == 3 && s.Path[2] == "where":
			where = s
		}
	}
	if root.Scope != 0 || root.ParentScope != 0 {
		t.Fatalf("root scope = %d/%d, want 0/0", root.Scope, root.ParentScope)
	}
	if sub.Scope != 1 || sub.ParentScope != 0 {
		t.Fatalf("subquery scope = %d/%d, want 1/0", sub.Scope, sub.ParentScope)
	}
	if where.Scope != 1 || where.ParentScope != 0 {
		t.Fatalf("where inside subquery scope = %d/%d, want 1/0", where.Scope, where.ParentScope)
	}
}

// Frames that write nothing produce no span, and incomplete maps keep their partial spans without pretending
// to be finished.
func TestOriginRecorder_ZeroLengthFramesAndIncomplete(t *testing.T) {
	b, m := NewSQLBuilderWithOrigins(false)
	PushFrame(b, FrameSpec{Segment: "select", Kind: OriginScope, OpensScope: true})
	PushFrame(b, FrameSpec{Segment: "empty", Kind: OriginClause})
	PopFrame(b)
	b.WriteStrings("SELECT 1")
	boom := errSentinel("boom")
	m.MarkIncomplete(boom)
	PopFrame(b)

	if !m.Incomplete() || !errors.Is(m.Err(), boom) {
		t.Fatalf("incomplete=%v err=%v, want marked incomplete with boom", m.Incomplete(), m.Err())
	}
	for _, s := range m.Spans() {
		if s.Start == s.End {
			t.Fatalf("zero-length span leaked: %+v", s)
		}
	}
}

// A released builder drops its recorder, so a later pooled generation cannot inherit old spans, while the
// previously returned map stays valid.
func TestOriginRecorder_ReleaseDetachesMap(t *testing.T) {
	b, m := NewSQLBuilderWithOrigins(false)
	PushFrame(b, FrameSpec{Segment: "select", Kind: OriginScope, OpensScope: true})
	b.WriteStrings("SELECT 1")
	PopFrame(b)
	m.Finalize()
	if len(m.Spans()) != 1 {
		t.Fatalf("expected 1 span, got %d", len(m.Spans()))
	}
	ReleaseSQLBuilder(b)

	next := NewSQLBuilder(false)
	if impl, ok := next.(*sqlBuilder); !ok || impl.rec != nil {
		t.Fatalf("pooled builder must start without a recorder, got %+v", next)
	}
	// Frames on an ordinary builder are no-ops.
	PushFrame(next, FrameSpec{Segment: "nope", Kind: OriginClause})
	PopFrame(next)

	if len(m.Spans()) != 1 || m.Spans()[0].End != 8 {
		t.Fatalf("returned map changed after release: %+v", m.Spans())
	}
	ReleaseSQLBuilder(next)
}

type errSentinel string

func (e errSentinel) Error() string { return string(e) }
