package sqlgen

import (
	"github.com/doug-martin/goqu/v9/exp"
	"github.com/doug-martin/goqu/v9/internal/sb"
)

// DialectAppender is implemented by expressions (such as datasets) that can render themselves with the dialect of the
// statement they are embedded in instead of their own. A single statement must use one dialect, so a subquery, CTE
// or compound query built with a different (e.g. the default) dialect would otherwise emit incompatible SQL, such as
// a "?" placeholder inside a Postgres prepared statement.
type DialectAppender interface {
	// AppendSQLWithDialect renders the expression with the given dialect. self is the appendable the generator was
	// given: when it is not the receiver itself (e.g. a wrapper that embeds a dataset and overrides AppendSQL),
	// implementations must delegate to self.AppendSQL so the wrapper's rendering is preserved.
	AppendSQLWithDialect(b sb.SQLBuilder, dialect string, do *SQLDialectOptions, self exp.AppendableExpression)
}

// appendNested appends ae to b using the dialect of the enclosing statement when ae supports it.
func appendNested(b sb.SQLBuilder, ae exp.AppendableExpression, dialect string, do *SQLDialectOptions) {
	if da, ok := ae.(DialectAppender); ok {
		// A goqu dataset (or any expression implementing the origin protocol) renders through the
		// generators, so its clauses can be recorded: open a real child scope with parent linkage.
		pushScopeFrame(b, "subquery")
		defer sb.PopFrame(b)
		da.AppendSQLWithDialect(b, dialect, do, ae)
		return
	}
	// An external AppendableExpression writes SQL straight into the builder without exposing its inner
	// nodes. Record the whole range as opaque rather than fabricating origins for its contents.
	pushOpaqueFrame(b, "custom_appendable")
	defer sb.PopFrame(b)
	ae.AppendSQL(b)
}
