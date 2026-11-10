package sqlgen

import (
	"unsafe"

	"github.com/doug-martin/goqu/v9/exp"
	"github.com/doug-martin/goqu/v9/internal/sb"
)

// eface mirrors the runtime representation of an interface value. The data word is what identifies the boxed
// expression node while a statement is rendered.
type eface struct {
	typ unsafe.Pointer
	ptr unsafe.Pointer
}

// expressionDataPtr returns a stable identity for the boxed expression node during one generation.
//
// Go generators only ever receive expressions through interface values. Passing an existing interface value
// along copies the type/words pair but keeps the same data pointer, so one node reused at several render
// positions yields the same pointer; two separately constructed (even structurally identical) nodes were
// boxed independently and yield different pointers. The dataset clauses stay alive for the whole render, so
// the boxed storage is neither collected nor reused before generation finishes.
func expressionDataPtr(e any) uintptr {
	return uintptr((*eface)(unsafe.Pointer(&e)).ptr)
}

// pushClauseFrame opens a dataset clause frame (e.g. "where", "join[0]"). It is a no-op for ordinary builders.
func pushClauseFrame(b sb.SQLBuilder, segment string) {
	sb.PushFrame(b, sb.FrameSpec{Segment: segment, Kind: sb.OriginClause})
}

// pushScopeFrame opens a child (sub)statement scope (subquery, CTE body, compound side).
func pushScopeFrame(b sb.SQLBuilder, segment string) {
	sb.PushFrame(b, sb.FrameSpec{Segment: segment, Kind: sb.OriginScope, OpensScope: true})
}

// pushOpaqueFrame marks a byte range whose internal nodes cannot be reported. A LiteralExpression still gets
// an opaque expression frame from pushExpressionFrame; custom AppendableExpressions use this frame around
// their self-rendered output. No inner nodes are fabricated for opaque ranges.
func pushOpaqueFrame(b sb.SQLBuilder, segment string) {
	sb.PushFrame(b, sb.FrameSpec{Segment: segment, Kind: sb.OriginOpaque, Opaque: true})
}

// pushExpressionFrame opens a frame for one expression render. The frame carries the node's per-generation
// identity and its occurrence number; LiteralExpression frames are opaque, while parameter sub-expressions
// nested in the literal are still rendered through Generate and get their own (nested) frames.
func pushExpressionFrame(b sb.SQLBuilder, e exp.Expression) {
	_, isLiteral := e.(exp.LiteralExpression)
	kind := sb.OriginExpression
	if isLiteral {
		kind = sb.OriginOpaque
	}
	sb.PushFrame(b, sb.FrameSpec{
		Kind:     kind,
		Identity: expressionDataPtr(e),
		Opaque:   isLiteral,
	})
}

// pushArgFrame opens a frame for one placeholder/argument occurrence. Only the 1-based argument position is
// recorded; the argument value itself is never stored in the origin map.
func pushArgFrame(b sb.SQLBuilder, argPosition int) {
	sb.PushFrame(b, sb.FrameSpec{Kind: sb.OriginArg, ArgIndex: argPosition})
}
