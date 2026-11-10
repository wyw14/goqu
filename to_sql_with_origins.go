package goqu

import "github.com/doug-martin/goqu/v9/internal/sb"

// OriginKind classifies the semantic construct a span was recorded for. It is an alias of the internal
// recorder kind, so values returned by ToSQLWithOrigins can be switched on without importing internal packages.
type (
	OriginKind = sb.OriginKind
	// OriginSpan maps a byte range of the final UTF-8 SQL string back to the semantic construct that
	// produced it (dataset clause path, expression identity and argument occurrence).
	OriginSpan = sb.OriginSpan
	// OriginMap is the byte-span to semantic-source mapping collected for one statement generation.
	OriginMap = sb.OriginMap
)

// Origin kind values, see the sb.OriginKind documentation for semantics.
const (
	OriginScope      = sb.OriginScope
	OriginClause     = sb.OriginClause
	OriginExpression = sb.OriginExpression
	OriginArg        = sb.OriginArg
	OriginOpaque     = sb.OriginOpaque
)

// finishWithOrigins finalizes a generation run. On success the SQL, args and the finalized map are returned.
// On failure the partial map is marked incomplete and the half-finished SQL is deliberately not returned, so
// an unusable statement can never be executed by mistake.
func finishWithOrigins(b sb.SQLBuilder, origins *sb.OriginMap) (sql string, args []any, m *OriginMap, err error) {
	sql, args, err = b.ToSQL()
	if err != nil {
		origins.MarkIncomplete(err)
		return "", []any{}, origins, err
	}
	origins.Finalize()
	return sql, args, origins, nil
}

// ToSQLWithOrigins behaves like ToSQL but additionally returns the origin map of the generated statement.
//
// The returned SQL string and argument order are identical to ToSQL. Every span addresses the final UTF-8
// byte string, so identifier escaping, multi-byte characters, parentheses, repeated expressions and numbered
// placeholder width changes do not shift the mapping. Reusing the same expression at several positions
// produces separate occurrences; subqueries, CTEs and compound queries keep parent-child scopes.
func (sd *SelectDataset) ToSQLWithOrigins() (sql string, args []any, origins *OriginMap, err error) {
	b, m := sb.NewSQLBuilderWithOrigins(sd.isPrepared.Bool())
	defer sb.ReleaseSQLBuilder(b)
	sb.PushFrame(b, sb.FrameSpec{Segment: "select", Kind: sb.OriginScope, OpensScope: true})
	if sd.err != nil {
		b.SetError(sd.err)
	} else {
		sd.dialect.ToSelectSQL(b, sd.GetClauses())
	}
	sb.PopFrame(b)
	return finishWithOrigins(b, m)
}

// ToSQLWithOrigins behaves like InsertDataset.ToSQL but additionally returns the origin map.
func (id *InsertDataset) ToSQLWithOrigins() (sql string, args []any, origins *OriginMap, err error) {
	b, m := sb.NewSQLBuilderWithOrigins(id.isPrepared.Bool())
	defer sb.ReleaseSQLBuilder(b)
	sb.PushFrame(b, sb.FrameSpec{Segment: "insert", Kind: sb.OriginScope, OpensScope: true})
	if id.err != nil {
		b.SetError(id.err)
	} else {
		if b.IsPrepared() {
			// mirror insertSQLBuilder: pre-size the args once for bulk inserts (allocation only, no ordering effect)
			totalArgs := 0
			for _, row := range id.clauses.Vals() {
				totalArgs += len(row)
			}
			b.GrowArgs(totalArgs)
		}
		id.dialect.ToInsertSQL(b, id.clauses)
	}
	sb.PopFrame(b)
	return finishWithOrigins(b, m)
}

// ToSQLWithOrigins behaves like UpdateDataset.ToSQL but additionally returns the origin map.
func (ud *UpdateDataset) ToSQLWithOrigins() (sql string, args []any, origins *OriginMap, err error) {
	b, m := sb.NewSQLBuilderWithOrigins(ud.isPrepared.Bool())
	defer sb.ReleaseSQLBuilder(b)
	sb.PushFrame(b, sb.FrameSpec{Segment: "update", Kind: sb.OriginScope, OpensScope: true})
	if ud.err != nil {
		b.SetError(ud.err)
	} else {
		ud.dialect.ToUpdateSQL(b, ud.clauses)
	}
	sb.PopFrame(b)
	return finishWithOrigins(b, m)
}

// ToSQLWithOrigins behaves like DeleteDataset.ToSQL but additionally returns the origin map.
func (dd *DeleteDataset) ToSQLWithOrigins() (sql string, args []any, origins *OriginMap, err error) {
	b, m := sb.NewSQLBuilderWithOrigins(dd.isPrepared.Bool())
	defer sb.ReleaseSQLBuilder(b)
	sb.PushFrame(b, sb.FrameSpec{Segment: "delete", Kind: sb.OriginScope, OpensScope: true})
	if dd.err != nil {
		b.SetError(dd.err)
	} else {
		dd.dialect.ToDeleteSQL(b, dd.clauses)
	}
	sb.PopFrame(b)
	return finishWithOrigins(b, m)
}

// ToSQLWithOrigins behaves like TruncateDataset.ToSQL but additionally returns the origin map.
func (td *TruncateDataset) ToSQLWithOrigins() (sql string, args []any, origins *OriginMap, err error) {
	b, m := sb.NewSQLBuilderWithOrigins(td.isPrepared.Bool())
	defer sb.ReleaseSQLBuilder(b)
	sb.PushFrame(b, sb.FrameSpec{Segment: "truncate", Kind: sb.OriginScope, OpensScope: true})
	if td.err != nil {
		b.SetError(td.err)
	} else {
		td.dialect.ToTruncateSQL(b, td.clauses)
	}
	sb.PopFrame(b)
	return finishWithOrigins(b, m)
}
