package sb

import "sort"

// OriginKind classifies the semantic construct a span was recorded for.
type OriginKind int

const (
	// OriginScope marks a statement boundary: the root statement or a nested subquery/CTE/compound scope.
	OriginScope OriginKind = iota
	// OriginClause marks a dataset clause, e.g. "select", "from", "where", "join[0]", "cte[1]".
	OriginClause
	// OriginExpression marks the render of an expression node, carrying its identity and occurrence.
	OriginExpression
	// OriginArg marks a single placeholder/argument occurrence (prepared statements only).
	OriginArg
	// OriginOpaque marks SQL text whose internal structure goqu cannot resolve, namely LiteralExpression
	// text and custom AppendableExpressions that do not implement the origin protocol.
	OriginOpaque
)

// String returns a stable, lowercase name for the kind (used in diagnostics and tests).
func (k OriginKind) String() string {
	switch k {
	case OriginScope:
		return "scope"
	case OriginClause:
		return "clause"
	case OriginExpression:
		return "expression"
	case OriginArg:
		return "arg"
	case OriginOpaque:
		return "opaque"
	default:
		return "unknown"
	}
}

// OriginSpan maps a byte range of the final UTF-8 SQL string back to the semantic construct that produced it.
//
// All offsets are byte offsets (not rune offsets), so they stay correct when identifiers are escaped or contain
// multi-byte characters, when parentheses are added, or when numbered placeholders change width (e.g. $9 -> $10).
//
// OriginSpan never carries argument values: prepared-statement arguments are referenced only by ArgIndex.
type OriginSpan struct {
	// Start is the inclusive start byte offset of the span in the final SQL string.
	Start int
	// End is the exclusive end byte offset of the span in the final SQL string.
	End int
	// Kind is the semantic construct that opened the span.
	Kind OriginKind
	// Path is the clause path from the statement root through nested scopes, e.g.
	// ["select", "where"] or ["select", "with", "cte[0]", "subquery", "where"].
	// Expression and argument spans do not add path segments of their own.
	Path []string
	// ExprID is a 1-based identity stable for the whole generation. The same expression node rendered at
	// several positions shares one ExprID; separately constructed but structurally identical nodes get
	// different IDs. It is 0 for spans that do not correspond to an expression node.
	ExprID uint64
	// Occurrence is the 0-based render number of ExprID within the statement (each reuse is a separate occurrence).
	Occurrence int
	// ArgIndex is the 1-based positional argument number for OriginArg spans, 0 for every other kind.
	ArgIndex int
	// Opaque is true when the span covers text whose inner nodes cannot be reported (literals, custom appendables).
	// Nested parameter sub-expressions are still recorded as their own spans inside the opaque range.
	Opaque bool
	// Scope is the id of the (sub)statement scope the span belongs to. The root statement is scope 0;
	// subqueries, CTE bodies and compound sides open child scopes.
	Scope int
	// ParentScope is the scope enclosing Scope (0 for the root scope).
	ParentScope int

	// depth is the frame nesting depth, used by At to select the innermost span; it is not part of the
	// public diagnostic contract.
	depth int
}

// OriginMap is the byte-span to semantic-source mapping collected while one statement was rendered.
// A map is owned by exactly one generation run: it is not affected by builder pool reuse or concurrent
// generations, and stays valid after the builder that produced it is released.
type OriginMap struct {
	spans      []OriginSpan
	incomplete bool
	err        error
	finalized  bool
}

// Spans returns the recorded spans. After a successful generation the map is finalized and spans are in
// document order (by Start, innermost first when starts are equal); an incomplete map keeps pop order, which
// is still valid for At lookups. The returned slice must be treated as read-only.
func (m *OriginMap) Spans() []OriginSpan {
	return m.spans
}

// Incomplete reports whether rendering failed before the statement was finished. When true the partial spans
// are diagnostic only and no executable SQL string is returned alongside the map.
func (m *OriginMap) Incomplete() bool {
	return m.incomplete
}

// Err returns the rendering error that made the map incomplete, or nil.
func (m *OriginMap) Err() error {
	return m.err
}

// MarkIncomplete freezes the map as a partial diagnostic. It is called when SQL generation returns an error,
// so a half-finished statement is never handed out as executable SQL.
func (m *OriginMap) MarkIncomplete(err error) {
	m.incomplete = true
	if m.err == nil {
		m.err = err
	}
}

// Finalize orders the collected spans for lookup. A map is finalized exactly once.
func (m *OriginMap) Finalize() {
	if m.finalized {
		return
	}
	m.finalized = true
	sort.SliceStable(m.spans, func(i, j int) bool {
		if m.spans[i].Start != m.spans[j].Start {
			return m.spans[i].Start < m.spans[j].Start
		}
		// at the same start the innermost span comes first
		return m.spans[i].depth > m.spans[j].depth
	})
}

// At returns the innermost span containing the given byte offset, or false when the offset is outside the
// statement. Use it to map a database-reported error position to the deepest known semantic source rather
// than to the first full-text substring that happens to match.
func (m *OriginMap) At(offset int) (OriginSpan, bool) {
	best := -1
	for i := range m.spans {
		s := &m.spans[i]
		if s.Start <= offset && offset < s.End {
			if best == -1 || s.depth > m.spans[best].depth ||
				(s.depth == m.spans[best].depth && s.End <= m.spans[best].End) {
				best = i
			}
		}
	}
	if best == -1 {
		return OriginSpan{}, false
	}
	return m.spans[best], true
}

// FrameSpec describes the semantic frame a generator opens around the bytes it is about to write.
type FrameSpec struct {
	// Segment is appended to Path while the frame is open. Empty segments (expression and arg frames)
	// do not appear in paths.
	Segment string
	// Kind classifies the frame.
	Kind OriginKind
	// Identity is the per-generation pointer identity of the boxed expression node, or 0.
	Identity uintptr
	// ArgIndex is the 1-based argument position for OriginArg frames.
	ArgIndex int
	// Opaque marks frames whose internals cannot be reported.
	Opaque bool
	// OpensScope makes the frame open a child (sub)statement scope.
	OpensScope bool
}

type (
	originFrame struct {
		segment     string
		kind        OriginKind
		start       int
		depth       int
		exprID      uint64
		occurrence  int
		argIndex    int
		opaque      bool
		scope       int
		parentScope int
		opensScope  bool
	}
	exprIdentity struct {
		id          uint64
		occurrences int
	}
	originRecorder struct {
		m           *OriginMap
		length      int
		frames      []originFrame
		scope       int
		scopeParent []int
		nextScope   int
		nextExprID  uint64
		exprs       map[uintptr]exprIdentity
	}
)

func newOriginRecorder() (*originRecorder, *OriginMap) {
	m := &OriginMap{}
	return &originRecorder{m: m, exprs: make(map[uintptr]exprIdentity)}, m
}

// afterWrite synchronizes the recorded SQL length with the builder buffer after a successful write.
func (r *originRecorder) afterWrite(bufLen int) {
	r.length = bufLen
}

func (r *originRecorder) parentOf(scope int) int {
	if scope >= 0 && scope < len(r.scopeParent) {
		return r.scopeParent[scope]
	}
	return 0
}

func (r *originRecorder) push(spec FrameSpec) {
	f := originFrame{
		segment:     spec.Segment,
		kind:        spec.Kind,
		start:       r.length,
		depth:       len(r.frames),
		argIndex:    spec.ArgIndex,
		opaque:      spec.Opaque,
		scope:       r.scope,
		parentScope: r.parentOf(r.scope),
		opensScope:  spec.OpensScope,
	}
	if spec.Identity != 0 {
		reg, ok := r.exprs[spec.Identity]
		if !ok {
			r.nextExprID++
			reg = exprIdentity{id: r.nextExprID}
		}
		f.exprID = reg.id
		f.occurrence = reg.occurrences
		reg.occurrences++
		r.exprs[spec.Identity] = reg
	}
	if spec.OpensScope {
		newScope := r.nextScope
		r.nextScope++
		// scopeParent is appended in scope-id order, so it stays indexable by scope id.
		r.scopeParent = append(r.scopeParent, r.scope)
		f.parentScope = r.scope
		f.scope = newScope
		r.scope = newScope
	}
	r.frames = append(r.frames, f)
}

func (r *originRecorder) pop() {
	last := len(r.frames) - 1
	if last < 0 {
		return
	}
	f := r.frames[last]
	// Snapshot the path while the frame is still on the stack so its own segment is included.
	path := r.currentPath()
	r.frames = r.frames[:last]
	if f.opensScope {
		r.scope = f.parentScope
	}
	end := r.length
	// Zero-length frames carry no bytes and can never be hit by an error position, skip them.
	if end <= f.start {
		return
	}
	r.m.spans = append(r.m.spans, OriginSpan{
		Start:       f.start,
		End:         end,
		Kind:        f.kind,
		Path:        path,
		ExprID:      f.exprID,
		Occurrence:  f.occurrence,
		ArgIndex:    f.argIndex,
		Opaque:      f.opaque,
		Scope:       f.scope,
		ParentScope: f.parentScope,
		depth:       f.depth,
	})
}

// currentPath snapshots the segments of every open frame (skipping empty ones).
func (r *originRecorder) currentPath() []string {
	path := make([]string, 0, len(r.frames))
	for _, f := range r.frames {
		if f.segment != "" {
			path = append(path, f.segment)
		}
	}
	return path
}

// OriginAware is implemented by SQL builders that have an active origin recorder. The optional interface keeps
// origin tracking out of the SQLBuilder contract, so external implementations and ordinary generations are
// unaffected.
type OriginAware interface {
	OriginPush(spec FrameSpec)
	OriginPop()
}

// PushFrame opens an origin frame on a recording builder; it is a no-op on ordinary builders. Every PushFrame
// must be paired with a PopFrame on the same builder.
func PushFrame(b SQLBuilder, spec FrameSpec) {
	if oa, ok := b.(OriginAware); ok {
		oa.OriginPush(spec)
	}
}

// PopFrame closes the frame opened by the matching PushFrame; it is a no-op on ordinary builders.
func PopFrame(b SQLBuilder) {
	if oa, ok := b.(OriginAware); ok {
		oa.OriginPop()
	}
}
