package goqu_test

import (
	"strings"
	"testing"

	"github.com/doug-martin/goqu/v9"
	_ "github.com/doug-martin/goqu/v9/dialect/mysql"
	_ "github.com/doug-martin/goqu/v9/dialect/postgres"
	"github.com/doug-martin/goqu/v9/dialect/sqlite3"
	_ "github.com/doug-martin/goqu/v9/dialect/sqlserver"
	"github.com/doug-martin/goqu/v9/internal/errors"
	"github.com/doug-martin/goqu/v9/mocks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sqlite3WindowDialect registers a sqlite3 based dialect that opts into window function support,
// mirroring how an application on a modern SQLite build enables the capability.
func sqlite3WindowDialect(t *testing.T) string {
	t.Helper()
	const name = "sqlite3_sessionize_test"
	opts := sqlite3.DialectOptions()
	opts.SupportsWindowFunction = true
	goqu.RegisterDialect(name, opts)
	return name
}

func validCfg() *goqu.SessionizeConfig {
	return &goqu.SessionizeConfig{
		PartitionBy: []interface{}{"user_id"},
		EventTime:   "ts",
		TieBreaker:  "event_id",
		Gap:         10,
	}
}

func TestSessionizePostgresInterpolated(t *testing.T) {
	input := goqu.Dialect("postgres").From("events").Where(goqu.I("kind").Eq("click"))
	ds, err := input.Sessionize(validCfg())
	require.NoError(t, err)

	sql, args, err := ds.ToSQL()
	require.NoError(t, err)
	expected := `SELECT "user_id", "goqu_sz_sess_no" AS "session_number", MIN("ts") AS "session_start", MAX("ts") AS "session_end" ` +
		`FROM (SELECT "user_id", "ts", "event_id", "goqu_sz_prev_ts", "goqu_sz_is_new", ` +
		`SUM("goqu_sz_is_new") OVER (PARTITION BY "user_id" ORDER BY "ts" ASC, "event_id" ASC ` +
		`ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) AS "goqu_sz_sess_no" ` +
		`FROM (SELECT "user_id", "ts", "event_id", "goqu_sz_prev_ts", ` +
		`CASE  WHEN ("goqu_sz_prev_ts" IS NULL) THEN 1 WHEN ("goqu_sz_prev_ts" > 9223372036854775797) THEN 0 ` +
		`WHEN "ts" > "goqu_sz_prev_ts" + 10 THEN 1 ELSE 0 END AS "goqu_sz_is_new" ` +
		`FROM (SELECT "user_id", "ts", "event_id", ` +
		`LAG("ts") OVER (PARTITION BY "user_id" ORDER BY "ts" ASC, "event_id" ASC) AS "goqu_sz_prev_ts" ` +
		`FROM (SELECT "user_id", "ts", "event_id" FROM (SELECT * FROM "events" WHERE ("kind" = 'click')) AS "goqu_sz_src" ` +
		`WHERE 1 / (CASE WHEN "ts" IS NULL THEN 0 ELSE 1 END) = 1) AS "goqu_sz_lag") AS "goqu_sz_flag") ` +
		`AS "goqu_sz_cum") AS "goqu_sz_agg" ` +
		`GROUP BY "user_id", "goqu_sz_sess_no" ORDER BY "user_id" ASC, "goqu_sz_sess_no" ASC`
	assert.Equal(t, expected, sql)
	assert.Empty(t, args)
}

func TestSessionizePostgresPreparedParamsFlowThroughLayers(t *testing.T) {
	// The input dataset carries its own bound parameter; it must keep its position behind the
	// parameters introduced by the outer sessionization layers.
	input := goqu.Dialect("postgres").From("events").
		Where(goqu.I("kind").Eq("click")).
		Prepared(true)
	ds, err := input.Sessionize(validCfg())
	require.NoError(t, err)

	sql, args, err := ds.ToSQL()
	require.NoError(t, err)
	assert.Contains(t, sql, "$1")
	assert.Contains(t, sql, "$7")
	expectedArgs := []interface{}{int64(1), int64(9223372036854775797), int64(0), int64(10), int64(1), int64(0), "click"}
	assert.Equal(t, expectedArgs, args)
	// Every parameter must be numbered.
	assert.NotContains(t, sql, "?")
}

func TestSessionizeSQLiteIndependentNulls(t *testing.T) {
	dialectName := sqlite3WindowDialect(t)
	cfg := validCfg()
	cfg.NullStrategy = goqu.IndependentNullSessions

	input := goqu.Dialect(dialectName).From("events").Where(goqu.I("kind").Eq("click"))
	ds, err := input.Sessionize(cfg)
	require.NoError(t, err)

	sql, args, err := ds.ToSQL()
	require.NoError(t, err)
	expected := "SELECT `user_id`, `goqu_sz_sess_no` AS `session_number`, MIN(`ts`) AS `session_start`, MAX(`ts`) AS `session_end` " +
		"FROM (SELECT `user_id`, `ts`, `event_id`, `goqu_sz_prev_ts`, `goqu_sz_is_new`, " +
		"SUM(`goqu_sz_is_new`) OVER (PARTITION BY `user_id` ORDER BY `ts` ASC, `event_id` ASC " +
		"ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) AS `goqu_sz_sess_no` " +
		"FROM (SELECT `user_id`, `ts`, `event_id`, `goqu_sz_prev_ts`, " +
		"CASE  WHEN (`ts` IS NULL) THEN 1 WHEN (`goqu_sz_prev_ts` IS NULL) THEN 1 " +
		"WHEN (`goqu_sz_prev_ts` > 9223372036854775797) THEN 0 " +
		"WHEN `ts` > `goqu_sz_prev_ts` + 10 THEN 1 ELSE 0 END AS `goqu_sz_is_new` " +
		"FROM (SELECT `user_id`, `ts`, `event_id`, " +
		"LAG(`ts`) OVER (PARTITION BY `user_id` ORDER BY `ts` ASC, `event_id` ASC) AS `goqu_sz_prev_ts` " +
		"FROM (SELECT `user_id`, `ts`, `event_id` FROM (SELECT * FROM `events` WHERE (`kind` = 'click')) AS `goqu_sz_src`) " +
		"AS `goqu_sz_lag`) AS `goqu_sz_flag`) AS `goqu_sz_cum`) AS `goqu_sz_agg` " +
		"GROUP BY `user_id`, `goqu_sz_sess_no` ORDER BY `user_id` ASC, `goqu_sz_sess_no` ASC"
	assert.Equal(t, expected, sql)
	assert.Empty(t, args)
	// No COALESCE anywhere: NULLs are handled by explicit IS NULL checks.
	assert.NotContains(t, sql, "COALESCE")
}

func TestSessionizeNoPartitionGapZeroPrepared(t *testing.T) {
	dialectName := sqlite3WindowDialect(t)
	cfg := &goqu.SessionizeConfig{EventTime: "ts", TieBreaker: "event_id", Gap: 0}
	ds, err := goqu.Dialect(dialectName).From("events").Sessionize(cfg)
	require.NoError(t, err)

	sql, args, err := ds.Prepared(true).ToSQL()
	require.NoError(t, err)
	assert.Contains(t, sql, "OVER (ORDER BY `ts` ASC, `event_id` ASC ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW)")
	assert.NotContains(t, sql, "PARTITION BY")
	// gap 0 -> safeMax is MaxInt64.
	assert.Equal(t, []interface{}{nil, int64(1), int64(9223372036854775807), int64(0), int64(0), int64(1), int64(0)}, args)
}

func TestSessionizeMySQLUsesDivGuard(t *testing.T) {
	// mysql8 enables window functions; the NULL guard must use DIV so MySQL raises instead of
	// returning NULL.
	input := goqu.Dialect("mysql8").From("events")
	ds, err := input.Sessionize(validCfg())
	require.NoError(t, err)

	sql, _, err := ds.ToSQL()
	require.NoError(t, err)
	assert.Contains(t, sql, "1 DIV (CASE WHEN `ts` IS NULL THEN 0 ELSE 1 END) = 1")
}

func TestSessionizeSQLiteUsesRejectNullUDFGuard(t *testing.T) {
	dialectName := sqlite3WindowDialect(t)
	ds, err := goqu.Dialect(dialectName).From("events").Sessionize(validCfg())
	require.NoError(t, err)

	sql, _, err := ds.ToSQL()
	require.NoError(t, err)
	// SQLite cannot raise from a SELECT, so the guard calls an explicit UDF; a missing UDF
	// fails at prepare time (fail closed).
	assert.Contains(t, sql, "goqu_sz_reject_null(`ts`) = 1")
	assert.NotContains(t, sql, "COALESCE")

	independent := validCfg()
	independent.NullStrategy = goqu.IndependentNullSessions
	ds2, err := goqu.Dialect(dialectName).From("events").Sessionize(independent)
	require.NoError(t, err)
	sql2, _, err := ds2.ToSQL()
	require.NoError(t, err)
	assert.NotContains(t, sql2, "goqu_sz_reject_null")
}

func TestSessionizeCustomColumnNames(t *testing.T) {
	dialectName := sqlite3WindowDialect(t)
	cfg := &goqu.SessionizeConfig{
		PartitionBy:      []interface{}{"pid"},
		EventTime:        "ts",
		TieBreaker:       "seq",
		Gap:              5,
		SessionNumberCol: "sess_no",
		SessionStartCol:  "sess_start",
		SessionEndCol:    "sess_end",
	}
	ds, err := goqu.Dialect(dialectName).From("events").Sessionize(cfg)
	require.NoError(t, err)

	sql, _, err := ds.ToSQL()
	require.NoError(t, err)
	assert.Contains(t, sql, "`goqu_sz_sess_no` AS `sess_no`")
	assert.Contains(t, sql, "MIN(`ts`) AS `sess_start`")
	assert.Contains(t, sql, "MAX(`ts`) AS `sess_end`")
}

func TestSessionizeInternalAliasesAvoidCollisions(t *testing.T) {
	dialectName := sqlite3WindowDialect(t)

	t.Run("output column named like internal alias", func(t *testing.T) {
		cfg := &goqu.SessionizeConfig{
			EventTime:        "ts",
			TieBreaker:       "event_id",
			Gap:              1,
			SessionNumberCol: "goqu_sz_sess_no",
		}
		ds, err := goqu.Dialect(dialectName).From("events").Sessionize(cfg)
		require.NoError(t, err)

		sql, _, err := ds.ToSQL()
		require.NoError(t, err)
		// The internal cumulative column is renamed, the user keeps the requested output name.
		assert.Contains(t, sql, "`goqu_sz_sess_no_0` AS `goqu_sz_sess_no`")
		assert.NotContains(t, sql, "`goqu_sz_sess_no` AS `goqu_sz_sess_no`")
	})

	t.Run("input column named like internal alias", func(t *testing.T) {
		cfg := &goqu.SessionizeConfig{
			EventTime:  "ts",
			TieBreaker: "goqu_sz_src",
			Gap:        1,
		}
		ds, err := goqu.Dialect(dialectName).From("events").Sessionize(cfg)
		require.NoError(t, err)

		sql, _, err := ds.ToSQL()
		require.NoError(t, err)
		// The source derived table must not reuse the tie breaker column name.
		assert.Contains(t, sql, "AS `goqu_sz_src_0`")
		assert.Contains(t, sql, "`goqu_sz_src`")
	})

	t.Run("input CTE named like internal alias", func(t *testing.T) {
		cfg := &goqu.SessionizeConfig{EventTime: "ts", TieBreaker: "event_id", Gap: 1}
		input := goqu.Dialect(dialectName).From("goqu_sz_lag").
			With("goqu_sz_lag", goqu.Dialect(dialectName).From("raw_events"))
		ds, err := input.Sessionize(cfg)
		require.NoError(t, err)

		sql, _, err := ds.ToSQL()
		require.NoError(t, err)
		// The user CTE keeps its name; the internal LAG layer alias is renamed to avoid the clash.
		assert.Contains(t, sql, "WITH goqu_sz_lag AS")
		assert.Contains(t, sql, "AS `goqu_sz_lag_0`")
	})
}

func TestSessionizeInputIsNotModified(t *testing.T) {
	dialectName := sqlite3WindowDialect(t)
	input := goqu.Dialect(dialectName).From("events").Where(goqu.I("kind").Eq("click"))
	beforeSQL, beforeArgs, err := input.ToSQL()
	require.NoError(t, err)
	beforeClauses := input.GetClauses()

	_, err = input.Sessionize(validCfg())
	require.NoError(t, err)

	afterSQL, afterArgs, err := input.ToSQL()
	require.NoError(t, err)
	assert.Equal(t, beforeSQL, afterSQL)
	assert.Equal(t, beforeArgs, afterArgs)
	assert.Equal(t, beforeClauses, input.GetClauses())
}

func TestSessionizeOuterFilterAndPagingWrapNumberedSessions(t *testing.T) {
	dialectName := sqlite3WindowDialect(t)
	ds, err := goqu.Dialect(dialectName).From("events").Sessionize(validCfg())
	require.NoError(t, err)

	paged := ds.Where(goqu.I("session_number").Gt(uint(1))).Order(goqu.I("session_number").Desc()).Limit(2).Offset(3)
	sql, _, err := paged.ToSQL()
	require.NoError(t, err)

	// The cumulative numbering still sits inside the derived table and is computed over every
	// event; paging only wraps the already numbered, already aggregated result.
	assert.Contains(t, sql, "ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW")
	assert.Contains(t, sql, "LIMIT 2")
	assert.Contains(t, sql, "OFFSET 3")
	assert.Contains(t, sql, "WHERE (`session_number` > 1)")
	// Numbering is not recomputed at the outer level (no second SUM/LAG outside the derived table).
	outerPart := strings.SplitN(sql, "AS `goqu_sz_agg`", 2)[1]
	assert.NotContains(t, outerPart, "LAG(")
	assert.NotContains(t, outerPart, "OVER (")
}

type sessionizeRejection struct {
	name   string
	input  *goqu.SelectDataset
	cfg    *goqu.SessionizeConfig
	errSub string
}

func assertSessionizeRejection(t *testing.T, tt sessionizeRejection) {
	t.Helper()
	var ds *goqu.SelectDataset
	if tt.input != nil {
		ds = tt.input
	}
	_, err := ds.Sessionize(tt.cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), tt.errSub)
}

func TestSessionizeRejectsUnsupportedInputShape(t *testing.T) {
	dialectName := sqlite3WindowDialect(t)
	base := func() *goqu.SelectDataset { return goqu.Dialect(dialectName).From("events") }

	cases := []sessionizeRejection{
		{name: "nil input", input: nil, cfg: validCfg(), errSub: "nil input dataset"},
		{name: "nil config", input: base(), cfg: nil, errSub: "SessionizeConfig is required"},
		{
			name:   "missing from",
			input:  goqu.Dialect(dialectName).Select(goqu.V(1)),
			cfg:    validCfg(),
			errSub: "FROM clause",
		},
		{
			name:   "raw sql from",
			input:  goqu.Dialect(dialectName).From(goqu.L("(SELECT 1)")),
			cfg:    validCfg(),
			errSub: "raw SQL in the FROM clause is rejected",
		},
		{
			name:   "multiple sources",
			input:  goqu.Dialect(dialectName).From("a", "b"),
			cfg:    validCfg(),
			errSub: "exactly one relation",
		},
		{
			name:   "join",
			input:  base().InnerJoin(goqu.T("other"), goqu.On(goqu.I("other.id").Eq(goqu.I("events.id")))),
			cfg:    validCfg(),
			errSub: "joins on the input dataset are not supported",
		},
		{name: "distinct", input: base().Distinct("user_id"), cfg: validCfg(), errSub: "DISTINCT"},
		{name: "group by", input: base().GroupBy("user_id"), cfg: validCfg(), errSub: "GROUP BY"},
		{
			name:   "having",
			input:  base().Having(goqu.COUNT("*").Gt(1)),
			cfg:    validCfg(),
			errSub: "HAVING",
		},
		{
			name:   "order",
			input:  base().Order(goqu.I("ts").Asc()),
			cfg:    validCfg(),
			errSub: "ORDER BY on the input dataset",
		},
		{name: "limit", input: base().Limit(5), cfg: validCfg(), errSub: "LIMIT on the input dataset"},
		{
			name:   "offset",
			input:  base().Offset(5),
			cfg:    validCfg(),
			errSub: "OFFSET on the input dataset",
		},
		{
			name:   "compound",
			input:  base().UnionAll(goqu.Dialect(dialectName).From("other")),
			cfg:    validCfg(),
			errSub: "compound queries",
		},
		{
			name:   "window clause",
			input:  base().Window(goqu.W("w")),
			cfg:    validCfg(),
			errSub: "WINDOW clauses",
		},
		{
			name:   "lock",
			input:  base().ForUpdate(goqu.Wait),
			cfg:    validCfg(),
			errSub: "row locks",
		},
		{
			name:   "dataset already in error",
			input:  base().SetError(errors.New("goqu: boom")),
			cfg:    validCfg(),
			errSub: "boom",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) { assertSessionizeRejection(t, tt) })
	}
}

func TestSessionizeRejectsInvalidConfig(t *testing.T) {
	dialectName := sqlite3WindowDialect(t)
	base := func() *goqu.SelectDataset { return goqu.Dialect(dialectName).From("events") }

	cases := []sessionizeRejection{
		{
			name:  "volatile function event time",
			input: base(),
			cfg: &goqu.SessionizeConfig{
				PartitionBy: []interface{}{"user_id"},
				EventTime:   goqu.Func("UNIX_TIMESTAMP", goqu.I("created_at")),
				TieBreaker:  "event_id",
				Gap:         1,
			},
			errSub: "plain column identifier",
		},
		{
			name:   "literal event time",
			input:  base(),
			cfg:    &goqu.SessionizeConfig{EventTime: goqu.L("1"), TieBreaker: "event_id", Gap: 1},
			errSub: "plain column identifier",
		},
		{
			name:   "missing tie breaker",
			input:  base(),
			cfg:    &goqu.SessionizeConfig{EventTime: "ts", Gap: 1},
			errSub: "TieBreaker is required",
		},
		{
			name:   "tie breaker equals event time",
			input:  base(),
			cfg:    &goqu.SessionizeConfig{EventTime: "ts", TieBreaker: "ts", Gap: 1},
			errSub: "different column than EventTime",
		},
		{
			name:   "negative gap",
			input:  base(),
			cfg:    &goqu.SessionizeConfig{EventTime: "ts", TieBreaker: "event_id", Gap: -1},
			errSub: "Gap must be non-negative",
		},
		{
			name:  "duplicate partition column",
			input: base(),
			cfg: &goqu.SessionizeConfig{
				PartitionBy: []interface{}{"user_id", "user_id"},
				EventTime:   "ts",
				TieBreaker:  "event_id",
				Gap:         1,
			},
			errSub: "duplicate partition column",
		},
		{
			name:  "partition collides with event time",
			input: base(),
			cfg: &goqu.SessionizeConfig{
				PartitionBy: []interface{}{"ts"},
				EventTime:   "ts",
				TieBreaker:  "event_id",
				Gap:         1,
			},
			errSub: "duplicate partition column",
		},
		{
			name:  "invalid output column name",
			input: base(),
			cfg: &goqu.SessionizeConfig{
				EventTime:        "ts",
				TieBreaker:       "event_id",
				Gap:              1,
				SessionNumberCol: "1bad",
			},
			errSub: "simple identifier",
		},
		{
			name:  "output column collides with input",
			input: base(),
			cfg: &goqu.SessionizeConfig{
				EventTime:       "ts",
				TieBreaker:      "event_id",
				Gap:             1,
				SessionStartCol: "ts",
			},
			errSub: "collides with an input column",
		},
		{
			name:  "unknown null strategy",
			input: base(),
			cfg: &goqu.SessionizeConfig{
				EventTime:    "ts",
				TieBreaker:   "event_id",
				Gap:          1,
				NullStrategy: 42,
			},
			errSub: "unknown NullTimeStrategy",
		},
		{
			name:  "qualified event time",
			input: base(),
			cfg: &goqu.SessionizeConfig{
				EventTime:  goqu.I("events.ts"),
				TieBreaker: "event_id",
				Gap:        1,
			},
			errSub: "unqualified column name",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) { assertSessionizeRejection(t, tt) })
	}
}

func TestSessionizeRejectsDialectsWithoutWindowSupport(t *testing.T) {
	cfg := validCfg()

	noWindow := []struct {
		name    string
		dialect string
	}{
		{"sqlite3 registered options", "sqlite3"},
		{"mysql 5.x options", "mysql"},
		{"sqlserver registered options", "sqlserver"},
	}
	for _, tt := range noWindow {
		t.Run(tt.name, func(t *testing.T) {
			_, err := goqu.Dialect(tt.dialect).From("events").Sessionize(cfg)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "does not support window functions")
		})
	}

	t.Run("unknown custom dialect implementation", func(t *testing.T) {
		ds := goqu.From("events").SetDialect(&mocks.SQLDialect{})
		_, err := ds.Sessionize(cfg)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unsupported dialect")
	})
}

func TestSessionizeStructuredSubquerySourceAccepted(t *testing.T) {
	dialectName := sqlite3WindowDialect(t)
	inner := goqu.Dialect(dialectName).From("raw_events").Where(goqu.I("kind").Eq("click"))
	outer := goqu.Dialect(dialectName).From(inner.As("filtered"))

	ds, err := outer.Sessionize(validCfg())
	require.NoError(t, err)
	sql, _, err := ds.ToSQL()
	require.NoError(t, err)
	assert.Contains(t, sql, "FROM (SELECT * FROM `raw_events`")
	assert.Contains(t, sql, "LAG(")
}

func TestSessionizeRejectsRawSubquerySource(t *testing.T) {
	dialectName := sqlite3WindowDialect(t)
	inner := goqu.Dialect(dialectName).From(goqu.L("(SELECT * FROM raw_events)"))
	outer := goqu.Dialect(dialectName).From(inner.As("filtered"))
	_, err := outer.Sessionize(validCfg())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "raw SQL in the FROM clause is rejected")
}

func TestSessionizeDefaultDialectSupported(t *testing.T) {
	// The default dialect inherits window function support.
	ds, err := goqu.From("events").Sessionize(validCfg())
	require.NoError(t, err)
	sql, _, err := ds.ToSQL()
	require.NoError(t, err)
	assert.Contains(t, sql, "LAG(")
	assert.Contains(t, sql, "ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW")
}
