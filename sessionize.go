package goqu

import (
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/doug-martin/goqu/v9/exp"
	"github.com/doug-martin/goqu/v9/internal/errors"
)

// SessionizeRejectNullFunction is the SQLite scalar function invoked by the RejectNullTimes guard.
// SQLite cannot raise from a SELECT on its own, so applications register this function on their
// connections (e.g. via SQLiteConn.RegisterFunc in mattn/go-sqlite3) and return an error when its
// argument is SQL NULL. If the function is missing the statement fails at prepare time, so NULL
// event times are never silently accepted.
const SessionizeRejectNullFunction = "goqu_sz_reject_null"

// NullTimeStrategy declares how Sessionize deals with rows whose integer event time is NULL.
type NullTimeStrategy int

const (
	// RejectNullTimes is the default. The generated SQL fails at runtime with an explicit error
	// as soon as a NULL event time is encountered. NULLs are never silently coalesced into the
	// normal time line (no COALESCE to zero anywhere in the generated SQL).
	//
	// The guard is dialect native: PostgreSQL and SQL Server raise on integer division by zero,
	// MySQL uses the DIV operator (plain "/" returns NULL there), and SQLite, which has no way to
	// raise from a SELECT, calls a scalar function named goqu_sz_reject_null(event_time). The
	// application must register that function on its SQLite connection (e.g. via
	// SQLiteConn.RegisterFunc in mattn/go-sqlite3) and return an error when the argument is SQL
	// NULL; if the function is missing the statement already fails at prepare time, so the failure
	// is always loud and NULL rows can never slip through.
	RejectNullTimes NullTimeStrategy = iota
	// IndependentNullSessions gives every NULL-time event its own session instead of rejecting the
	// query. The first non-NULL event following a run of NULLs also starts a new session. No COALESCE
	// is used: NULL arithmetic is never evaluated, the boundary is driven by IS NULL checks.
	IndependentNullSessions
)

// SessionizeConfig configures SelectDataset.Sessionize.
//
// Every column is declared explicitly (strings are parsed as unqualified identifiers); the planner
// never reaches for SELECT * or invents column names beyond the internal, collision-checked aliases.
type SessionizeConfig struct {
	// PartitionBy lists the columns that identify independent event streams. Each partition is
	// sessionized on its own and session numbers restart at 1 inside every partition. It may be empty,
	// in which case the whole relation is one partition.
	PartitionBy []interface{}
	// EventTime is the integer column holding the event time. Only a plain identifier is accepted:
	// functions, literals, CASE expressions and arithmetic (which could be volatile) are rejected.
	EventTime interface{}
	// TieBreaker is the column that makes the order total for events sharing the same event time.
	// It is required, must be a plain identifier different from EventTime, and must be unique inside
	// a partition. goqu cannot inspect database constraints, so the uniqueness guarantee is part of
	// the caller contract: without it a stable total order cannot be proven and planning is rejected.
	TieBreaker interface{}
	// Gap is the non-negative idle threshold. Two adjacent events stay in the same session while
	// their time difference is at most Gap; a difference strictly greater than Gap starts a new
	// session (a difference equal to Gap keeps the current session).
	Gap int64
	// NullStrategy selects the NULL event time policy, see NullTimeStrategy.
	NullStrategy NullTimeStrategy
	// SessionNumberCol, SessionStartCol and SessionEndCol are the names of the three columns the
	// resulting dataset returns. Empty values default to session_number, session_start and
	// session_end. They must be simple, distinct identifiers and must not collide with an input
	// column.
	SessionNumberCol string
	SessionStartCol  string
	SessionEndCol    string
}

// Internal alias role names; they only serve as collision-check seeds and are suffixed on demand.
const (
	sessionizeAliasPrefix = "goqu_sz_"

	sessionizeDefaultNumberCol = "session_number"
	sessionizeDefaultStartCol  = "session_start"
	sessionizeDefaultEndCol    = "session_end"

	// Extra columns appended to the partition columns when sizing slices: the base projection
	// adds the event time and the tie breaker; the final aggregation adds the session number plus
	// the start and end boundaries.
	projectionExtraCols = 2
	finalAggExtraCols   = 3
)

var simpleIdentifierPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type sessionizePlanner struct {
	cfg   *SessionizeConfig
	parts []exp.IdentifierExpression
	ts    exp.IdentifierExpression
	tie   exp.IdentifierExpression

	numberCol string
	startCol  string
	endCol    string

	// Internal, collision-checked aliases.
	srcAlias  string
	lagAlias  string
	flagAlias string
	cumAlias  string
	aggAlias  string
	prevCol   string
	flagCol   string
	noCol     string

	taken map[string]struct{}
}

// Sessionize compiles the structured event dataset into a new immutable dataset that returns one row
// per session: the partition columns, session_number, session_start and session_end.
//
// The SQL is built as layered derived tables, each layer adding exactly one column:
//
//  1. an explicit projection of the declared columns over the input relation, keeping the input
//     WHERE (and only the WHERE) in place, i.e. before sessionization, plus the NULL rejection guard
//     when RejectNullTimes is used;
//  2. LAG(event_time) OVER (PARTITION BY ... ORDER BY event_time, tie_breaker) to fetch the previous
//     valid time inside the partition;
//  3. a boundary flag rendered as CASE (never a nested window function in the same SELECT), applying
//     the strictly-greater-than-Gap rule with an overflow-safe comparison;
//  4. a cumulative SUM of the flag using a ROWS frame (ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT
//     ROW), so equal-time peers are counted individually instead of being merged by a RANGE frame;
//  5. the final aggregation grouped by partition and session number, so session_start/session_end
//     are the real MIN/MAX of each session rather than the partition-wide MIN/MAX.
//
// The input dataset is not modified. Filters and pagination applied to the returned dataset wrap the
// already-numbered sessions, so paging can never renumber sessions.
func (sd *SelectDataset) Sessionize(cfg *SessionizeConfig) (*SelectDataset, error) {
	if sd == nil {
		return nil, errors.New("sessionize: nil input dataset")
	}
	if cfg == nil {
		return nil, errors.New("sessionize: SessionizeConfig is required")
	}
	if err := validateSessionizeDialect(sd); err != nil {
		return nil, err
	}
	if err := validateSessionizeInput(sd); err != nil {
		return nil, err
	}

	p, err := newSessionizePlanner(cfg)
	if err != nil {
		return nil, err
	}
	if err := p.reserveInputNames(sd); err != nil {
		return nil, err
	}
	p.allocateAliases()

	return p.build(sd), nil
}

func validateSessionizeDialect(sd *SelectDataset) error {
	// Only the built in, option-backed dialects can be proven to support the required window
	// functions. A custom SQLDialect implementation is unknown to the planner and is rejected
	// instead of emitting SQL that may not run.
	d, ok := sd.dialect.(*sqlDialect)
	if !ok {
		return errors.New(
			"sessionize: unsupported dialect %T: only registered built in dialects with window "+
				"function support are accepted",
			sd.dialect,
		)
	}
	if !d.dialectOptions.SupportsWindowFunction {
		return errors.New(
			"sessionize: dialect %q does not support window functions, register a dialect with "+
				"SQLDialectOptions.SupportsWindowFunction enabled",
			d.Dialect(),
		)
	}
	return nil
}

func validateSessionizeInput(sd *SelectDataset) error {
	if sd.Error() != nil {
		return sd.Error()
	}
	c := sd.GetClauses()
	if !c.HasSources() {
		return errors.New("sessionize: input dataset must have a FROM clause providing the event relation")
	}
	if from := c.From().Columns(); len(from) != 1 {
		return errors.New("sessionize: input dataset must read from exactly one relation, comma joins are rejected")
	}
	// Row shaping clauses change or constrain the event granularity and would make sessionization
	// ambiguous. WHERE is intentionally allowed: it belongs in front of the sessionization layers.
	if c.Distinct() != nil {
		return errors.New("sessionize: DISTINCT on the input dataset is not supported")
	}
	if c.GroupBy() != nil {
		return errors.New("sessionize: GROUP BY on the input dataset is not supported, events must be raw rows")
	}
	if c.Having() != nil {
		return errors.New("sessionize: HAVING on the input dataset is not supported, events must be raw rows")
	}
	if c.Qualify() != nil {
		return errors.New("sessionize: QUALIFY on the input dataset is not supported, events must be raw rows")
	}
	if c.HasOrder() {
		return errors.New("sessionize: ORDER BY on the input dataset is not supported, order is defined by event time and tie breaker")
	}
	if c.HasLimit() {
		return errors.New("sessionize: LIMIT on the input dataset is not supported, it would silently truncate the events before sessionization")
	}
	if c.Offset() != 0 {
		return errors.New("sessionize: OFFSET on the input dataset is not supported, it would silently truncate the events before sessionization")
	}
	if len(c.Compounds()) != 0 {
		return errors.New("sessionize: compound queries (UNION/INTERSECT/...) on the input dataset are not supported")
	}
	if len(c.Windows()) != 0 {
		return errors.New("sessionize: WINDOW clauses on the input dataset are not supported")
	}
	if len(c.Joins()) != 0 {
		return errors.New("sessionize: joins on the input dataset are not supported, pass a single structured event relation")
	}
	if c.Lock() != nil {
		return errors.New("sessionize: row locks (FOR UPDATE/...) on the input dataset are not supported")
	}
	return validateSessionizeSource(c.From().Columns()[0])
}

func validateSessionizeSource(source exp.Expression) error {
	switch s := source.(type) {
	case exp.IdentifierExpression:
		return nil
	case exp.AliasedExpression:
		return validateSessionizeSource(s.Aliased())
	case *SelectDataset:
		return validateSessionizeInput(s)
	case exp.LiteralExpression:
		return errors.New("sessionize: raw SQL in the FROM clause is rejected, pass a table identifier or a structured dataset")
	default:
		return errors.New("sessionize: unsupported FROM source %T, only table identifiers and structured datasets are accepted", source)
	}
}

func newSessionizePlanner(cfg *SessionizeConfig) (*sessionizePlanner, error) {
	if cfg.Gap < 0 {
		return nil, errors.New("sessionize: Gap must be non-negative, got %d", cfg.Gap)
	}
	if cfg.NullStrategy != RejectNullTimes && cfg.NullStrategy != IndependentNullSessions {
		return nil, errors.New("sessionize: unknown NullTimeStrategy %d", cfg.NullStrategy)
	}

	ts, err := requireSimpleColumn("EventTime", cfg.EventTime)
	if err != nil {
		return nil, err
	}
	if cfg.TieBreaker == nil {
		return nil, errors.New(
			"sessionize: TieBreaker is required: without a unique tie breaker a total order " +
				"over equal event times cannot be proven",
		)
	}
	tie, err := requireSimpleColumn("TieBreaker", cfg.TieBreaker)
	if err != nil {
		return nil, err
	}
	if columnName(tie) == columnName(ts) {
		return nil, errors.New("sessionize: TieBreaker must be a different column than EventTime")
	}

	parts := make([]exp.IdentifierExpression, 0, len(cfg.PartitionBy))
	seen := map[string]struct{}{columnName(ts): {}, columnName(tie): {}}
	for i, raw := range cfg.PartitionBy {
		part, err := requireSimpleColumn("PartitionBy", raw)
		if err != nil {
			return nil, err
		}
		name := columnName(part)
		if _, dup := seen[name]; dup {
			return nil, errors.New("sessionize: duplicate partition column %q at position %d", name, i)
		}
		seen[name] = struct{}{}
		parts = append(parts, part)
	}

	numberCol, err := requireOutputColumn("SessionNumberCol", cfg.SessionNumberCol, sessionizeDefaultNumberCol, seen)
	if err != nil {
		return nil, err
	}
	startCol, err := requireOutputColumn("SessionStartCol", cfg.SessionStartCol, sessionizeDefaultStartCol, seen)
	if err != nil {
		return nil, err
	}
	endCol, err := requireOutputColumn("SessionEndCol", cfg.SessionEndCol, sessionizeDefaultEndCol, seen)
	if err != nil {
		return nil, err
	}
	if startCol == endCol || numberCol == startCol || numberCol == endCol {
		return nil, errors.New("sessionize: SessionNumberCol, SessionStartCol and SessionEndCol must be distinct")
	}

	return &sessionizePlanner{
		cfg:       cfg,
		parts:     parts,
		ts:        ts,
		tie:       tie,
		numberCol: numberCol,
		startCol:  startCol,
		endCol:    endCol,
		taken:     seen,
	}, nil
}

func requireSimpleColumn(field string, raw interface{}) (exp.IdentifierExpression, error) {
	if raw == nil {
		return nil, errors.New("sessionize: %s is required", field)
	}
	var id exp.IdentifierExpression
	switch v := raw.(type) {
	case string:
		id = I(v)
	case exp.IdentifierExpression:
		id = v
	default:
		return nil, errors.New(
			"sessionize: %s must be a plain column identifier, got %T: functions, literals and arithmetic are rejected",
			field, raw,
		)
	}
	if id.GetSchema() != "" || id.GetTable() != "" {
		return nil, errors.New("sessionize: %s must be an unqualified column name, got a schema/table qualified identifier", field)
	}
	name, ok := id.GetCol().(string)
	if !ok || !simpleIdentifierPattern.MatchString(name) {
		return nil, errors.New("sessionize: %s must be a simple identifier matching %s", field, simpleIdentifierPattern.String())
	}
	return id, nil
}

func requireOutputColumn(field, value, fallback string, taken map[string]struct{}) (string, error) {
	name := value
	if name == "" {
		name = fallback
	}
	if !simpleIdentifierPattern.MatchString(name) {
		return "", errors.New("sessionize: %s must be a simple identifier matching %s, got %q", field, simpleIdentifierPattern.String(), name)
	}
	if _, collides := taken[name]; collides {
		return "", errors.New("sessionize: %s %q collides with an input column", field, name)
	}
	taken[name] = struct{}{}
	return name, nil
}

func columnName(id exp.IdentifierExpression) string {
	name, _ := id.GetCol().(string)
	return name
}

// reserveInputNames collects every name visible in the input scopes (CTE names, relation aliases)
// so internal aliases cannot shadow them.
func (p *sessionizePlanner) reserveInputNames(sd *SelectDataset) error {
	return reserveDatasetNames(sd, p.taken)
}

func reserveDatasetNames(sd *SelectDataset, taken map[string]struct{}) error {
	c := sd.GetClauses()
	for _, cte := range c.CommonTables() {
		taken[cte.Name().Literal()] = struct{}{}
	}
	if c.HasAlias() {
		if name := c.Alias().GetTable(); name != "" {
			taken[name] = struct{}{}
		}
	}
	if !c.HasSources() {
		return nil
	}
	for _, source := range c.From().Columns() {
		if err := reserveSourceNames(source, taken); err != nil {
			return err
		}
	}
	return nil
}

func reserveSourceNames(source exp.Expression, taken map[string]struct{}) error {
	switch s := source.(type) {
	case exp.AliasedExpression:
		if id := s.GetAs(); id != nil {
			if name := id.GetTable(); name != "" {
				taken[name] = struct{}{}
			}
		}
		return reserveSourceNames(s.Aliased(), taken)
	case *SelectDataset:
		return reserveDatasetNames(s, taken)
	default:
		return nil
	}
}

func (p *sessionizePlanner) allocateAliases() {
	p.srcAlias = p.unique(sessionizeAliasPrefix + "src")
	p.lagAlias = p.unique(sessionizeAliasPrefix + "lag")
	p.flagAlias = p.unique(sessionizeAliasPrefix + "flag")
	p.cumAlias = p.unique(sessionizeAliasPrefix + "cum")
	p.aggAlias = p.unique(sessionizeAliasPrefix + "agg")
	p.prevCol = p.unique(sessionizeAliasPrefix + "prev_ts")
	p.flagCol = p.unique(sessionizeAliasPrefix + "is_new")
	p.noCol = p.unique(sessionizeAliasPrefix + "sess_no")
}

func (p *sessionizePlanner) unique(base string) string {
	if _, exists := p.taken[base]; !exists {
		p.taken[base] = struct{}{}
		return base
	}
	for i := 0; ; i++ {
		candidate := base + "_" + strconv.Itoa(i)
		if _, exists := p.taken[candidate]; !exists {
			p.taken[candidate] = struct{}{}
			return candidate
		}
	}
}

func (p *sessionizePlanner) build(input *SelectDataset) *SelectDataset {
	prepared := input.IsPrepared()
	factory := input.queryFactory
	dialectName := input.dialect.Dialect()

	// Layer 1: explicit projection over the input relation, original WHERE stays inside the input
	// subquery (pre-sessionization). Reject mode adds the explicit NULL guard here.
	src := input.Clone().(*SelectDataset)
	base := newDataset(dialectName, factory).Prepared(prepared).
		Select(p.projection()...).
		From(src.As(p.srcAlias))
	if p.cfg.NullStrategy == RejectNullTimes {
		base = base.Where(p.nullGuard(dialectName))
	}

	// Layer 2: previous event time inside the partition via LAG. Same-time peers stay ordered by the
	// unique tie breaker, so the order is total.
	lagWindow := W().OrderBy(p.ts.Asc(), p.tie.Asc())
	if len(p.parts) > 0 {
		lagWindow = lagWindow.PartitionBy(toInterfaces(p.parts)...)
	}
	lagExpr := Func("LAG", p.ts).Over(lagWindow).As(p.prevCol)
	lagDS := newDataset(dialectName, factory).Prepared(prepared).
		Select(append(p.projection(), lagExpr)...).
		From(base.As(p.lagAlias))

	// Layer 3: boundary flag. The window function is consumed from the previous layer instead of
	// being nested into this SELECT.
	flagDS := newDataset(dialectName, factory).Prepared(prepared).
		Select(append(p.projection(), I(p.prevCol), p.boundaryFlag().As(p.flagCol))...).
		From(lagDS.As(p.flagAlias))

	// Layer 4: cumulative session number. The explicit ROWS frame counts every peer row instead of
	// merging ORDER BY peers the way the default RANGE frame would.
	cumDS := newDataset(dialectName, factory).Prepared(prepared).
		Select(append(p.projection(), I(p.prevCol), I(p.flagCol), p.cumulativeSum().As(p.noCol))...).
		From(flagDS.As(p.cumAlias))

	// Layer 5: one row per real session. Start/end are MIN/MAX within the (partition, session) group.
	finalSelect := make([]interface{}, 0, len(p.parts)+finalAggExtraCols)
	groupCols := make([]interface{}, 0, len(p.parts)+1)
	orderCols := make([]exp.OrderedExpression, 0, len(p.parts)+1)
	for _, part := range p.parts {
		finalSelect = append(finalSelect, part)
		groupCols = append(groupCols, part)
		orderCols = append(orderCols, part.Asc())
	}
	finalSelect = append(finalSelect,
		I(p.noCol).As(p.numberCol),
		MIN(p.ts).As(p.startCol),
		MAX(p.ts).As(p.endCol),
	)
	groupCols = append(groupCols, I(p.noCol))
	orderCols = append(orderCols, I(p.noCol).Asc())

	return newDataset(dialectName, factory).Prepared(prepared).
		Select(finalSelect...).
		From(cumDS.As(p.aggAlias)).
		GroupBy(groupCols...).
		Order(orderCols...)
}

func (p *sessionizePlanner) projection() []interface{} {
	cols := make([]interface{}, 0, len(p.parts)+projectionExtraCols)
	for _, part := range p.parts {
		cols = append(cols, part)
	}
	cols = append(cols, p.ts, p.tie)
	return cols
}

// nullGuard emits an expression that hard-errors on any NULL event time instead of hiding it:
//   - SQLite has no built-in way to raise from a SELECT, so a scalar UDF named
//     goqu_sz_reject_null is called; a missing function fails the statement at prepare time.
//   - MySQL needs the integer DIV operator because plain "/" returns NULL on division by zero.
//   - PostgreSQL, SQL Server and the default dialect raise on integer division by zero.
func (p *sessionizePlanner) nullGuard(dialectName string) exp.Expression {
	name := strings.ToLower(dialectName)
	switch {
	case strings.HasPrefix(name, "sqlite"):
		return L(SessionizeRejectNullFunction+"(?) = 1", p.ts)
	case strings.HasPrefix(name, "mysql"):
		return L("1 DIV (CASE WHEN ? IS NULL THEN 0 ELSE 1 END) = 1", p.ts)
	default:
		return L("1 / (CASE WHEN ? IS NULL THEN 0 ELSE 1 END) = 1", p.ts)
	}
}

func (p *sessionizePlanner) boundaryFlag() exp.CaseExpression {
	prev := I(p.prevCol)
	// safeMax is the largest value for which prev + Gap cannot overflow int64. It is computed in Go
	// (Gap is non-negative, so MaxInt64-Gap is itself safe) and bound as an ordinary parameter.
	safeMax := math.MaxInt64 - p.cfg.Gap
	gapTest := L("? > ? + ?", p.ts, prev, p.cfg.Gap)

	flag := Case()
	if p.cfg.NullStrategy == IndependentNullSessions {
		// Every NULL-time event becomes its own session.
		flag = flag.When(p.ts.IsNull(), int64(1))
	}
	return flag.
		// First row of the partition, or the first non-NULL row after a run of NULLs.
		When(prev.IsNull(), int64(1)).
		// prev + Gap would overflow: ts can never overtake it, so no boundary can be crossed.
		When(prev.Gt(safeMax), int64(0)).
		// Strictly greater than Gap starts a new session; equal to Gap stays in the current one.
		When(gapTest, int64(1)).
		Else(int64(0))
}

func (p *sessionizePlanner) cumulativeSum() exp.LiteralExpression {
	orderCols := exp.NewOrderedColumnList(p.ts.Asc(), p.tie.Asc())
	if len(p.parts) == 0 {
		return L(
			"SUM(?) OVER (ORDER BY ? ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW)",
			I(p.flagCol), orderCols,
		)
	}
	partCols := exp.NewColumnListExpression(toInterfaces(p.parts)...)
	return L(
		"SUM(?) OVER (PARTITION BY ? ORDER BY ? ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW)",
		I(p.flagCol), partCols, orderCols,
	)
}

func toInterfaces(ids []exp.IdentifierExpression) []interface{} {
	out := make([]interface{}, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}
