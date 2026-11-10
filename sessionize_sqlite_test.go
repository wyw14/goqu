package goqu_test

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"sort"
	"testing"

	"github.com/doug-martin/goqu/v9"
	"github.com/doug-martin/goqu/v9/dialect/sqlite3"
	mattnsqlite3 "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	sessionizeSQLiteDialect = "sqlite3_sessionize_it"
	// Driver with the goqu_sz_reject_null UDF registered on every connection.
	rejectingDriver = "sqlite3_sessionize_reject"
	// Plain driver without the UDF, used to prove the guard fails closed.
	plainDriver = "sqlite3"

	errNullEventTime = "goqu: null event time rejected by sessionize"
)

func init() {
	opts := sqlite3.DialectOptions()
	// Modern SQLite (3.25+) supports window functions; the registered sqlite3 dialect stays
	// conservative, so tests opt in explicitly.
	opts.SupportsWindowFunction = true
	goqu.RegisterDialect(sessionizeSQLiteDialect, opts)

	// Register a driver whose connections expose the reject-NULL UDF. mattn hands SQL NULL to a
	// scalar function as a nil []byte (not a nil interface), so both shapes are checked.
	sql.Register(rejectingDriver, &mattnsqlite3.SQLiteDriver{
		ConnectHook: func(conn *mattnsqlite3.SQLiteConn) error {
			return conn.RegisterFunc(goqu.SessionizeRejectNullFunction, func(v interface{}) (int, error) {
				if v == nil {
					return 0, errors.New(errNullEventTime)
				}
				if b, ok := v.([]byte); ok && b == nil {
					return 0, errors.New(errNullEventTime)
				}
				return 1, nil
			}, true)
		},
	})
}

// refEvent is one structured input event. sql.NullInt64 models a NULL event time explicitly.
type refEvent[K comparable] struct {
	Key K
	TS  sql.NullInt64
	ID  int64
}

// refSession is the expected session row, computed entirely in Go over a finite event list.
type refSession[K comparable] struct {
	Key    K
	Number int64
	Start  sql.NullInt64
	End    sql.NullInt64
}

// exceedsGap is the overflow-safe Go mirror of the SQL boundary predicate: it answers
// ts-prev > gap without ever subtracting two int64 values.
func exceedsGap(prev, ts, gap int64) bool {
	if prev > math.MaxInt64-gap {
		// prev + gap would overflow; ts can never overtake it, so no boundary can be crossed.
		return false
	}
	return ts > prev+gap
}

// referenceSessions is the finite-list reference implementation: sort per partition by
// (NULL time first, event time, unique tie breaker) and walk the list, applying exactly the
// same rules the generated SQL implements.
func referenceSessions[K comparable](
	events []refEvent[K], gap int64, strategy goqu.NullTimeStrategy, compareKey func(a, b K) int,
) ([]refSession[K], error) {
	grouped := map[K][]refEvent[K]{}
	keys := make([]K, 0)
	for _, e := range events {
		if _, ok := grouped[e.Key]; !ok {
			keys = append(keys, e.Key)
		}
		grouped[e.Key] = append(grouped[e.Key], e)
	}
	sort.Slice(keys, func(i, j int) bool { return compareKey(keys[i], keys[j]) < 0 })

	var out []refSession[K]
	for _, key := range keys {
		rows := grouped[key]
		sort.SliceStable(rows, func(i, j int) bool {
			a, b := rows[i].TS, rows[j].TS
			// Mirror SQLite ASC ordering: NULLs first, then time, then the unique tie breaker.
			if !a.Valid || !b.Valid {
				if a.Valid != b.Valid {
					return !a.Valid
				}
			} else if a.Int64 != b.Int64 {
				return a.Int64 < b.Int64
			}
			return rows[i].ID < rows[j].ID
		})

		var number int64
		var current *refSession[K]
		var prevTS sql.NullInt64
		havePrev := false
		flush := func() {
			if current != nil {
				out = append(out, *current)
				current = nil
			}
		}
		for _, row := range rows {
			newSession := false
			if !row.TS.Valid {
				if strategy == goqu.RejectNullTimes {
					return nil, assert.AnError
				}
				newSession = true
			} else if !havePrev || !prevTS.Valid {
				// First row of the partition, or first non-NULL row after a run of NULLs.
				newSession = true
			} else if exceedsGap(prevTS.Int64, row.TS.Int64, gap) {
				// Strictly greater than gap; equal to gap stays in the current session.
				newSession = true
			}
			if newSession {
				flush()
				number++
				current = &refSession[K]{Key: key, Number: number, Start: row.TS, End: row.TS}
			} else if current != nil && row.TS.Valid {
				if row.TS.Int64 < current.Start.Int64 {
					current.Start = row.TS
				}
				if row.TS.Int64 > current.End.Int64 {
					current.End = row.TS
				}
			}
			prevTS = row.TS
			havePrev = true
		}
		flush()
	}
	return out, nil
}

type sessionRow struct {
	UserID        string        `db:"user_id"`
	SessionNumber int64         `db:"session_number"`
	SessionStart  sql.NullInt64 `db:"session_start"`
	SessionEnd    sql.NullInt64 `db:"session_end"`
}

type compositeKey struct {
	Tenant string
	UserID string
}

type compositeSessionRow struct {
	Tenant        string        `db:"tenant"`
	UserID        string        `db:"user_id"`
	SessionNumber int64         `db:"session_number"`
	SessionStart  sql.NullInt64 `db:"session_start"`
	SessionEnd    sql.NullInt64 `db:"session_end"`
}

func openSessionizeDB(t *testing.T) (*goqu.Database, *sql.DB) {
	t.Helper()
	db, err := sql.Open(rejectingDriver, ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	_, err = db.ExecContext(context.Background(), `CREATE TABLE events (
		user_id  TEXT NOT NULL,
		ts       INTEGER,
		event_id INTEGER NOT NULL,
		kind     TEXT NOT NULL
	)`)
	require.NoError(t, err)
	return goqu.New(sessionizeSQLiteDialect, db), db
}

func insertEvents(t *testing.T, db *sql.DB, rows []refEvent[string], kinds []string) {
	t.Helper()
	stmt, err := db.PrepareContext(context.Background(), `INSERT INTO events (user_id, ts, event_id, kind) VALUES (?, ?, ?, ?)`)
	require.NoError(t, err)
	defer stmt.Close()
	for i, r := range rows {
		var ts interface{}
		if r.TS.Valid {
			ts = r.TS.Int64
		}
		kind := "click"
		if len(kinds) > i {
			kind = kinds[i]
		}
		_, err := stmt.ExecContext(context.Background(), r.Key, ts, r.ID, kind)
		require.NoError(t, err)
	}
}

func nullInt(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: true} }

// sessionizeFixture covers every required edge: shuffled insertion, duplicate times, multiple
// partitions, NULL times and int64 extreme values.
func sessionizeFixture() []refEvent[string] {
	return []refEvent[string]{
		{Key: "u1", TS: nullInt(100), ID: 2},
		{Key: "u2", TS: nullInt(50), ID: 20},
		{Key: "u1", TS: nullInt(math.MaxInt64 - 1), ID: 9},
		{Key: "u1", TS: sql.NullInt64{}, ID: 7}, // NULL event time
		{Key: "u1", TS: nullInt(121), ID: 5},
		{Key: "u2", TS: sql.NullInt64{}, ID: 22}, // NULL event time
		{Key: "u1", TS: nullInt(100), ID: 1},     // duplicate timestamp, lower tie breaker
		{Key: "u1", TS: nullInt(105), ID: 3},
		{Key: "u2", TS: nullInt(51), ID: 21},
		{Key: "u1", TS: nullInt(math.MinInt64), ID: 10},
		{Key: "u1", TS: nullInt(110), ID: 4},
		{Key: "u1", TS: nullInt(1000), ID: 8},
		{Key: "u2", TS: nullInt(70), ID: 23},
		{Key: "u1", TS: nullInt(120), ID: 6},            // diff 100->...->120 includes an exactly-gap step
		{Key: "u1", TS: nullInt(math.MinInt64), ID: 11}, // duplicate extreme timestamp
		{Key: "u1", TS: nullInt(math.MaxInt64), ID: 12},
	}
}

func compareStringKey(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func scanSessions(t *testing.T, ds *goqu.SelectDataset) []sessionRow {
	t.Helper()
	var got []sessionRow
	require.NoError(t, ds.ScanStructs(&got))
	return got
}

func assertSessionsMatchReference[K comparable](t *testing.T, got []sessionRow, want []refSession[K], keyOf func(sessionRow) K) {
	t.Helper()
	require.Len(t, got, len(want))
	for i, w := range want {
		assert.Equal(t, keyOf(got[i]), w.Key, "row %d key", i)
		assert.Equal(t, w.Number, got[i].SessionNumber, "row %d session number", i)
		assert.Equal(t, w.Start.Valid, got[i].SessionStart.Valid, "row %d start nullity", i)
		assert.Equal(t, w.End.Valid, got[i].SessionEnd.Valid, "row %d end nullity", i)
		if w.Start.Valid {
			assert.Equal(t, w.Start.Int64, got[i].SessionStart.Int64, "row %d start", i)
		}
		if w.End.Valid {
			assert.Equal(t, w.End.Int64, got[i].SessionEnd.Int64, "row %d end", i)
		}
	}
}

func TestSessionizeSQLiteIndependentNullsMatchesGoReference(t *testing.T) {
	gdb, db := openSessionizeDB(t)
	events := sessionizeFixture()
	insertEvents(t, db, events, nil)

	const gap = int64(10)
	cfg := &goqu.SessionizeConfig{
		PartitionBy:  []interface{}{"user_id"},
		EventTime:    "ts",
		TieBreaker:   "event_id",
		Gap:          gap,
		NullStrategy: goqu.IndependentNullSessions,
	}
	ds, err := gdb.From("events").Sessionize(cfg)
	require.NoError(t, err)

	got := scanSessions(t, ds)
	want, err := referenceSessions(events, gap, goqu.IndependentNullSessions, compareStringKey)
	require.NoError(t, err)
	assertSessionsMatchReference(t, got, want, func(r sessionRow) string { return r.UserID })

	// Every output row carries real per-session boundaries, never partition-wide min/max.
	for _, row := range got {
		if row.SessionStart.Valid {
			assert.LessOrEqual(t, row.SessionStart.Int64, row.SessionEnd.Int64)
		} else {
			assert.False(t, row.SessionEnd.Valid, "NULL-time session must have NULL start and end")
		}
	}
}

func TestSessionizeSQLiteRejectNullsFailsExplicitly(t *testing.T) {
	gdb, db := openSessionizeDB(t)
	insertEvents(t, db, sessionizeFixture(), nil)

	cfg := &goqu.SessionizeConfig{
		PartitionBy: []interface{}{"user_id"},
		EventTime:   "ts",
		TieBreaker:  "event_id",
		Gap:         10,
		// RejectNullTimes is the zero value but set it explicitly for readability.
		NullStrategy: goqu.RejectNullTimes,
	}
	ds, err := gdb.From("events").Sessionize(cfg)
	require.NoError(t, err)

	var got []sessionRow
	err = ds.ScanStructs(&got)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "null event time", "expected an explicit NULL rejection, got %v", err)
}

func TestSessionizeSQLiteRejectGuardFailsClosedWithoutUDF(t *testing.T) {
	// On a connection where the UDF was not registered, the reject query must fail at prepare
	// time instead of silently accepting NULL event times.
	db, err := sql.Open(plainDriver, ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	_, err = db.ExecContext(context.Background(),
		`CREATE TABLE events (user_id TEXT NOT NULL, ts INTEGER, event_id INTEGER NOT NULL, kind TEXT NOT NULL)`)
	require.NoError(t, err)
	_, err = db.ExecContext(context.Background(), `INSERT INTO events (user_id, ts, event_id, kind) VALUES ('u1', NULL, 1, 'click')`)
	require.NoError(t, err)

	gdb := goqu.New(sessionizeSQLiteDialect, db)
	ds, err := gdb.From("events").Sessionize(&goqu.SessionizeConfig{
		PartitionBy: []interface{}{"user_id"},
		EventTime:   "ts",
		TieBreaker:  "event_id",
		Gap:         10,
	})
	require.NoError(t, err)

	var got []sessionRow
	err = ds.ScanStructs(&got)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "goqu_sz_reject_null")
}

func TestSessionizeSQLiteRejectNullsWithoutNullsMatchesReference(t *testing.T) {
	gdb, db := openSessionizeDB(t)
	events := sessionizeFixture()
	nonNull := make([]refEvent[string], 0, len(events))
	for _, e := range events {
		if e.TS.Valid {
			nonNull = append(nonNull, e)
		}
	}
	insertEvents(t, db, nonNull, nil)

	const gap = int64(10)
	cfg := &goqu.SessionizeConfig{
		PartitionBy: []interface{}{"user_id"},
		EventTime:   "ts",
		TieBreaker:  "event_id",
		Gap:         gap,
	}
	ds, err := gdb.From("events").Sessionize(cfg)
	require.NoError(t, err)

	got := scanSessions(t, ds)
	want, err := referenceSessions(nonNull, gap, goqu.RejectNullTimes, compareStringKey)
	require.NoError(t, err)
	assertSessionsMatchReference(t, got, want, func(r sessionRow) string { return r.UserID })
}

type noPartitionSessionRow struct {
	SessionNumber int64         `db:"session_number"`
	SessionStart  sql.NullInt64 `db:"session_start"`
	SessionEnd    sql.NullInt64 `db:"session_end"`
}

func TestSessionizeSQLiteNoPartitionIndependentNulls(t *testing.T) {
	gdb, db := openSessionizeDB(t)
	events := []refEvent[string]{
		{Key: "u1", TS: nullInt(100), ID: 1},
		{Key: "u1", TS: sql.NullInt64{}, ID: 2}, // NULL time, independent session
		{Key: "u1", TS: nullInt(100), ID: 3},    // duplicate time, stable tie breaker order
		{Key: "u1", TS: nullInt(105), ID: 4},
		{Key: "u2", TS: nullInt(200), ID: 5},
		{Key: "u2", TS: sql.NullInt64{}, ID: 6}, // NULL time, independent session
	}
	insertEvents(t, db, events, nil)

	const gap = int64(10)
	ds, err := gdb.From("events").Sessionize(&goqu.SessionizeConfig{
		EventTime:    "ts",
		TieBreaker:   "event_id",
		Gap:          gap,
		NullStrategy: goqu.IndependentNullSessions,
	})
	require.NoError(t, err)

	var got []noPartitionSessionRow
	require.NoError(t, ds.ScanStructs(&got))

	// With no partition columns every row shares one reference partition.
	single := make([]refEvent[string], len(events))
	for i, e := range events {
		e.Key = ""
		single[i] = e
	}
	want, err := referenceSessions(single, gap, goqu.IndependentNullSessions, compareStringKey)
	require.NoError(t, err)
	require.Len(t, got, len(want))
	for i, w := range want {
		assert.Equal(t, w.Number, got[i].SessionNumber, "row %d number", i)
		assert.Equal(t, w.Start, got[i].SessionStart, "row %d start", i)
		assert.Equal(t, w.End, got[i].SessionEnd, "row %d end", i)
	}
}

func TestSessionizeSQLiteGapZeroAndGapEquality(t *testing.T) {
	gdb, db := openSessionizeDB(t)
	events := []refEvent[string]{
		{Key: "u1", TS: nullInt(100), ID: 1},
		{Key: "u1", TS: nullInt(100), ID: 2}, // duplicate time stays in the same session
		{Key: "u1", TS: nullInt(101), ID: 3}, // gap 0: any positive difference starts a new session
		{Key: "u1", TS: nullInt(200), ID: 4},
		{Key: "u1", TS: nullInt(205), ID: 5}, // difference exactly 5 == gap: stays in the session
		{Key: "u1", TS: nullInt(206), ID: 6}, // difference 6 > gap 5: new session
	}
	insertEvents(t, db, events, nil)

	// gap = 0: distinct timestamps split, equal timestamps stay together.
	zeroDS, err := gdb.From("events").Where(goqu.I("event_id").Lte(uint(3))).Sessionize(&goqu.SessionizeConfig{
		PartitionBy: []interface{}{"user_id"},
		EventTime:   "ts",
		TieBreaker:  "event_id",
		Gap:         0,
	})
	require.NoError(t, err)
	zeroWant, err := referenceSessions(events[:3], 0, goqu.RejectNullTimes, compareStringKey)
	require.NoError(t, err)
	assertSessionsMatchReference(t, scanSessions(t, zeroDS), zeroWant, func(r sessionRow) string { return r.UserID })

	// gap = 5: equality keeps the session, strictly greater splits.
	fiveDS, err := gdb.From("events").Where(goqu.I("event_id").Gte(uint(4))).Sessionize(&goqu.SessionizeConfig{
		PartitionBy: []interface{}{"user_id"},
		EventTime:   "ts",
		TieBreaker:  "event_id",
		Gap:         5,
	})
	require.NoError(t, err)
	fiveWant, err := referenceSessions(events[3:], 5, goqu.RejectNullTimes, compareStringKey)
	require.NoError(t, err)
	assertSessionsMatchReference(t, scanSessions(t, fiveDS), fiveWant, func(r sessionRow) string { return r.UserID })
}

func TestSessionizeSQLiteInsertOrderDoesNotChangeSessions(t *testing.T) {
	gdb, db := openSessionizeDB(t)
	events := sessionizeFixture()

	// First insertion: the shuffled fixture order.
	insertEvents(t, db, events, nil)
	first := scanSessions(t, mustSessionize(t, gdb.From("events"), 10, goqu.IndependentNullSessions))

	// Second insertion: the same rows in reversed physical order, in a fresh table/database.
	gdb2, db2 := openSessionizeDB(t)
	reversed := append([]refEvent[string](nil), events...)
	sort.SliceStable(reversed, func(i, j int) bool { return reversed[i].ID > reversed[j].ID })
	insertEvents(t, db2, reversed, nil)
	second := scanSessions(t, mustSessionize(t, gdb2.From("events"), 10, goqu.IndependentNullSessions))

	assert.Equal(t, first, second)
}

func TestSessionizeSQLitePagingKeepsFullSessionIdentity(t *testing.T) {
	gdb, db := openSessionizeDB(t)
	events := sessionizeFixture()
	insertEvents(t, db, events, nil)

	const gap = int64(10)
	fullDS, err := gdb.From("events").Sessionize(&goqu.SessionizeConfig{
		PartitionBy:  []interface{}{"user_id"},
		EventTime:    "ts",
		TieBreaker:   "event_id",
		Gap:          gap,
		NullStrategy: goqu.IndependentNullSessions,
	})
	require.NoError(t, err)
	full := scanSessions(t, fullDS)

	// Page through the already-numbered result. The inner sessionization query is untouched, so
	// concatenating the pages must reproduce the full result exactly.
	var pages []sessionRow
	pageSize := uint(2)
	for offset := uint(0); ; offset += pageSize {
		pageDS := fullDS.Limit(pageSize).Offset(offset)
		page := scanSessions(t, pageDS)
		pages = append(pages, page...)
		if len(page) < int(pageSize) {
			break
		}
	}
	assert.Equal(t, full, pages)

	// An outer filter must not renumber: session 2 keeps number 2 instead of becoming number 1.
	numberTwo := scanSessions(t, fullDS.Where(goqu.I("session_number").Eq(uint(2))))
	for _, row := range numberTwo {
		assert.Equal(t, int64(2), row.SessionNumber)
	}
	assert.Len(t, numberTwo, countSessions(full, 2))
}

func TestSessionizeSQLitePreparedStatementsAndInputFilter(t *testing.T) {
	gdb, db := openSessionizeDB(t)
	events := sessionizeFixture()
	kinds := make([]string, len(events))
	filtered := make([]refEvent[string], 0, len(events))
	for i, e := range events {
		if e.ID%2 == 0 {
			kinds[i] = "keep"
			filtered = append(filtered, e)
		} else {
			kinds[i] = "drop"
		}
	}
	insertEvents(t, db, events, kinds)

	const gap = int64(10)
	// The original filter sits in front of the sessionization layers and binds through prepared
	// statement placeholders together with the planner's own gap parameters.
	input := gdb.From("events").Where(goqu.I("kind").Eq("keep")).Prepared(true)
	ds, err := input.Sessionize(&goqu.SessionizeConfig{
		PartitionBy:  []interface{}{"user_id"},
		EventTime:    "ts",
		TieBreaker:   "event_id",
		Gap:          gap,
		NullStrategy: goqu.IndependentNullSessions,
	})
	require.NoError(t, err)
	query, args, err := ds.ToSQL()
	require.NoError(t, err)
	assert.NotEmpty(t, args, "gap and filter parameters must be bound, not interpolated")
	assert.Contains(t, query, "?")

	got := scanSessions(t, ds)
	want, err := referenceSessions(filtered, gap, goqu.IndependentNullSessions, compareStringKey)
	require.NoError(t, err)
	assertSessionsMatchReference(t, got, want, func(r sessionRow) string { return r.UserID })
}

func TestSessionizeSQLiteCompositePartition(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	_, err = db.ExecContext(context.Background(), `CREATE TABLE events2 (
		tenant   TEXT NOT NULL,
		user_id  TEXT NOT NULL,
		ts       INTEGER,
		event_id INTEGER NOT NULL
	)`)
	require.NoError(t, err)

	events := []refEvent[compositeKey]{
		{Key: compositeKey{"t1", "u1"}, TS: nullInt(100), ID: 1},
		{Key: compositeKey{"t1", "u1"}, TS: nullInt(200), ID: 2},
		{Key: compositeKey{"t2", "u1"}, TS: nullInt(100), ID: 3}, // same user, different tenant: restart at 1
		{Key: compositeKey{"t1", "u1"}, TS: sql.NullInt64{}, ID: 4},
		{Key: compositeKey{"t2", "u1"}, TS: nullInt(105), ID: 5},
	}
	stmt, err := db.PrepareContext(context.Background(), `INSERT INTO events2 (tenant, user_id, ts, event_id) VALUES (?, ?, ?, ?)`)
	require.NoError(t, err)
	for _, e := range events {
		var ts interface{}
		if e.TS.Valid {
			ts = e.TS.Int64
		}
		_, err := stmt.ExecContext(context.Background(), e.Key.Tenant, e.Key.UserID, ts, e.ID)
		require.NoError(t, err)
	}
	require.NoError(t, stmt.Close())

	gdb := goqu.New(sessionizeSQLiteDialect, db)
	ds, err := gdb.From("events2").Sessionize(&goqu.SessionizeConfig{
		PartitionBy:  []interface{}{"tenant", "user_id"},
		EventTime:    "ts",
		TieBreaker:   "event_id",
		Gap:          10,
		NullStrategy: goqu.IndependentNullSessions,
	})
	require.NoError(t, err)

	var got []compositeSessionRow
	require.NoError(t, ds.ScanStructs(&got))
	want, err := referenceSessions(events, int64(10), goqu.IndependentNullSessions, func(a, b compositeKey) int {
		if a.Tenant != b.Tenant {
			return compareStringKey(a.Tenant, b.Tenant)
		}
		return compareStringKey(a.UserID, b.UserID)
	})
	require.NoError(t, err)
	require.Len(t, got, len(want))
	for i, w := range want {
		assert.Equal(t, w.Key.Tenant, got[i].Tenant, "row %d tenant", i)
		assert.Equal(t, w.Key.UserID, got[i].UserID, "row %d user", i)
		assert.Equal(t, w.Number, got[i].SessionNumber, "row %d number", i)
		assert.Equal(t, w.Start, got[i].SessionStart, "row %d start", i)
		assert.Equal(t, w.End, got[i].SessionEnd, "row %d end", i)
	}
}

func TestSessionizeSQLiteExtremeInt64DoesNotOverflow(t *testing.T) {
	gdb, db := openSessionizeDB(t)
	events := []refEvent[string]{
		{Key: "u1", TS: nullInt(math.MinInt64), ID: 1},
		{Key: "u1", TS: nullInt(math.MinInt64), ID: 2},
		{Key: "u1", TS: nullInt(math.MaxInt64 - 1), ID: 3},
		{Key: "u1", TS: nullInt(math.MaxInt64), ID: 4},
	}
	insertEvents(t, db, events, nil)

	const gap = int64(5)
	ds, err := gdb.From("events").Sessionize(&goqu.SessionizeConfig{
		PartitionBy: []interface{}{"user_id"},
		EventTime:   "ts",
		TieBreaker:  "event_id",
		Gap:         gap,
	})
	require.NoError(t, err)

	got := scanSessions(t, ds)
	want, err := referenceSessions(events, gap, goqu.RejectNullTimes, compareStringKey)
	require.NoError(t, err)
	assertSessionsMatchReference(t, got, want, func(r sessionRow) string { return r.UserID })

	// The two extreme sessions must carry the real extreme values as their boundaries.
	require.Len(t, got, 2)
	assert.Equal(t, int64(math.MinInt64), got[0].SessionStart.Int64)
	assert.Equal(t, int64(math.MinInt64), got[0].SessionEnd.Int64)
	assert.Equal(t, int64(math.MaxInt64-1), got[1].SessionStart.Int64)
	assert.Equal(t, int64(math.MaxInt64), got[1].SessionEnd.Int64)
}

func mustSessionize(t *testing.T, ds *goqu.SelectDataset, gap int64, strategy goqu.NullTimeStrategy) *goqu.SelectDataset {
	t.Helper()
	out, err := ds.Sessionize(&goqu.SessionizeConfig{
		PartitionBy:  []interface{}{"user_id"},
		EventTime:    "ts",
		TieBreaker:   "event_id",
		Gap:          gap,
		NullStrategy: strategy,
	})
	require.NoError(t, err)
	return out
}

func countSessions(rows []sessionRow, number int64) int {
	n := 0
	for _, r := range rows {
		if r.SessionNumber == number {
			n++
		}
	}
	return n
}
