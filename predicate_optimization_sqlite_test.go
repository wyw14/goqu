package goqu_test

import (
	"database/sql"
	"fmt"
	"math/rand"
	"sync/atomic"
	"testing"

	sqlite3driver "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/suite"

	"github.com/doug-martin/goqu/v9"
	"github.com/doug-martin/goqu/v9/dialect/sqlite3"
	"github.com/doug-martin/goqu/v9/exp"
)

const (
	predOptDriverName  = "sqlite3_predicate_optimizer"
	predOptDialectName = "sqlite3_predicate_optimizer_dialect"
	triStateRows       = 27
	randomTreeCount    = 150
	randomDataRows     = 30
	minOptimizedTrees  = 25
	treeDepth          = 3
)

type predicateOptimizerSQLiteSuite struct {
	suite.Suite
	db      *sql.DB
	counter atomic.Int64
}

func TestPredicateOptimizerSQLiteSuite(t *testing.T) {
	suite.Run(t, new(predicateOptimizerSQLiteSuite))
}

func (s *predicateOptimizerSQLiteSuite) SetupSuite() {
	sql.Register(predOptDriverName, &sqlite3driver.SQLiteDriver{
		ConnectHook: func(conn *sqlite3driver.SQLiteConn) error {
			return conn.RegisterFunc("oc", func(v any) any {
				s.counter.Add(1)
				return v
			}, true)
		},
	})
	options := sqlite3.DialectOptions()
	options.OptimizePredicates = true
	goqu.RegisterDialect(predOptDialectName, options)

	db, err := sql.Open(predOptDriverName, ":memory:")
	s.Require().NoError(err)
	db.SetMaxOpenConns(1)
	s.db = db

	s.createTriStateTable()
	s.createRandomTable()
}

func (s *predicateOptimizerSQLiteSuite) TearDownSuite() {
	s.Require().NoError(s.db.Close())
}

func (s *predicateOptimizerSQLiteSuite) createTriStateTable() {
	_, err := s.db.Exec(`CREATE TEMP TABLE tt(a INT, b INT, c INT)`)
	s.Require().NoError(err)
	values := []any{nil, 0, 1}
	for _, a := range values {
		for _, b := range values {
			for _, c := range values {
				_, err = s.db.Exec(`INSERT INTO tt(a, b, c) VALUES (?, ?, ?)`, a, b, c)
				s.Require().NoError(err)
			}
		}
	}
	var count int
	s.Require().NoError(s.db.QueryRow(`SELECT COUNT(*) FROM tt`).Scan(&count))
	s.Equal(triStateRows, count)
}

func (s *predicateOptimizerSQLiteSuite) createRandomTable() {
	_, err := s.db.Exec(`CREATE TEMP TABLE tr(a INT, b INT, c INT)`)
	s.Require().NoError(err)
	rng := rand.New(rand.NewSource(99)) //nolint:gosec // deterministic RNG for repeatable test fixtures
	pool := []any{nil, 0, 1, 2, 3}
	for i := 0; i < randomDataRows; i++ {
		_, err = s.db.Exec(`INSERT INTO tr(a, b, c) VALUES (?, ?, ?)`,
			pool[rng.Intn(len(pool))], pool[rng.Intn(len(pool))], pool[rng.Intn(len(pool))])
		s.Require().NoError(err)
	}
}

// renderPredicate builds the SELECT/WHERE statement for a predicate with optimization switched on or off, so the
// only difference between the two statements is the optimizer.
func (s *predicateOptimizerSQLiteSuite) renderPredicate(
	table string, predicate exp.Expression, optimized, asColumn bool,
) (query string, args []any) {
	dialectName := "sqlite3"
	if optimized {
		dialectName = predOptDialectName
	}
	ds := goqu.Dialect(dialectName).From(table).Prepared(true)
	if asColumn {
		ds = ds.Select(predicate)
	} else {
		ds = ds.Where(predicate)
	}
	query, args, err := ds.ToSQL()
	s.Require().NoError(err)
	return query, args
}

// evalTriState evaluates a predicate as a SELECT column over every tri-state row and returns one NullInt64 per row.
func (s *predicateOptimizerSQLiteSuite) evalTriState(predicate exp.Expression, optimized bool) []sql.NullInt64 {
	query, args := s.renderPredicate("tt", predicate, optimized, true)
	rows, err := s.db.Query(query, args...)
	s.Require().NoError(err)
	defer rows.Close()
	results := make([]sql.NullInt64, 0, triStateRows)
	for rows.Next() {
		var value sql.NullInt64
		s.Require().NoError(rows.Scan(&value))
		results = append(results, value)
	}
	s.Require().NoError(rows.Err())
	return results
}

func (s *predicateOptimizerSQLiteSuite) countRows(
	table string, predicate exp.Expression, optimized bool,
) (count int, calls int64) {
	dialectName := "sqlite3"
	if optimized {
		dialectName = predOptDialectName
	}
	ds := goqu.Dialect(dialectName).From(table).
		Select(goqu.COUNT("*")).Where(predicate).Prepared(true)
	query, args, err := ds.ToSQL()
	s.Require().NoError(err)
	before := s.counter.Load()
	s.Require().NoError(s.db.QueryRow(query, args...).Scan(&count))
	calls = s.counter.Load() - before
	return count, calls
}

func (s *predicateOptimizerSQLiteSuite) assertEquivalent(predicate exp.Expression, hasOpaque bool) {
	original := s.evalTriState(predicate, false)
	optimized := s.evalTriState(predicate, true)
	s.Equal(original, optimized)
	if hasOpaque {
		_, originalCalls := s.countRows("tt", predicate, false)
		_, optimizedCalls := s.countRows("tt", predicate, true)
		s.Equal(originalCalls, optimizedCalls, "opaque evaluation count changed")
	}
}

func (s *predicateOptimizerSQLiteSuite) TestExhaustiveThreeValuedLogic() {
	a, b, c := goqu.I("a"), goqu.I("b"), goqu.I("c")
	oc := func(col interface{}) exp.SQLFunctionExpression { return goqu.Func("oc", col) }
	predicates := []struct {
		name      string
		predicate exp.Expression
		opaque    bool
	}{
		{"shared factor under OR",
			goqu.Or(goqu.And(a.Eq(1), b.Eq(1)), goqu.And(a.Eq(1), c.Eq(0))), false},
		{"shared factor under AND",
			goqu.And(goqu.Or(a.Eq(1), b.Eq(1)), goqu.Or(a.Eq(1), c.Eq(0))), false},
		{"shared IS NULL factor",
			goqu.Or(goqu.And(a.Is(nil), b.Eq(1)), goqu.And(a.Is(nil), c.Eq(0))), false},
		{"shared NOT factor keeps NeqOp",
			goqu.Or(goqu.And(a.Neq(1), b.Eq(1)), goqu.And(a.Neq(1), c.IsNot(nil))), false},
		{"shared IN factor",
			goqu.Or(goqu.And(a.In(1, 0), b.Eq(1)), goqu.And(a.In(1, 0), c.Eq(0))), false},
		{"shared NOT IN factor",
			goqu.Or(goqu.And(a.NotIn(1), b.Eq(1)), goqu.And(a.NotIn(1), c.Eq(0))), false},
		{"shared LIKE and NOT LIKE",
			goqu.Or(goqu.And(b.Like("1"), a.Eq(1)), goqu.And(b.Like("1"), c.Eq(0))), false},
		{"duplicate atom idempotence",
			goqu.And(a.Eq(1), a.Eq(1), b.Eq(0)), false},
		{"nested two level factors",
			goqu.Or(
				goqu.And(a.Eq(1), b.Eq(1), c.Eq(0)),
				goqu.And(a.Eq(1), b.Eq(1), a.Is(nil)),
			), false},
		{"opaque function cannot be factored",
			goqu.Or(goqu.And(oc(a).Eq(1), b.Eq(1)), goqu.And(oc(a).Eq(1), c.Eq(0))), true},
		{"opaque remainder blocks factoring but keeps semantics",
			goqu.Or(goqu.And(a.Eq(1), oc(b).Eq(1)), goqu.And(a.Eq(1), oc(c).Eq(0))), true},
		{"ex map barrier with pure factor",
			goqu.Or(
				goqu.And(a.Eq(1), goqu.Ex{"b": 1}),
				goqu.And(a.Eq(1), c.Eq(0)),
			), false},
		{"literal SQL barrier",
			goqu.And(goqu.L("oc(1) > 0"), goqu.L("oc(1) > 0")), true},
	}
	for _, tc := range predicates {
		s.Run(tc.name, func() {
			s.assertEquivalent(tc.predicate, tc.opaque)
		})
	}
}

// randomPredicateGenerator builds small structured AND/OR trees from a fixed atom pool so repeated factors occur
// naturally. Opaque oc atoms are tracked so side effect counts can be compared.
type randomPredicateGenerator struct {
	rng     *rand.Rand
	columns []exp.IdentifierExpression
}

func (g *randomPredicateGenerator) atom() (exp.Expression, bool) {
	col := g.columns[g.rng.Intn(len(g.columns))]
	if g.rng.Intn(5) == 0 {
		return goqu.Func("oc", col).Eq(g.rng.Intn(3)), true
	}
	switch g.rng.Intn(7) {
	case 0:
		return col.Eq(g.rng.Intn(3)), false
	case 1:
		return col.Neq(g.rng.Intn(3)), false
	case 2:
		return col.Gt(g.rng.Intn(2)), false
	case 3:
		return col.Is(nil), false
	case 4:
		return col.IsNot(nil), false
	case 5:
		return col.In(g.rng.Intn(3), g.rng.Intn(3)), false
	default:
		return col.NotIn(g.rng.Intn(3), g.rng.Intn(3)), false
	}
}

func (g *randomPredicateGenerator) tree(depth int) (exp.Expression, bool) {
	if depth == 0 || g.rng.Intn(3) == 0 {
		return g.atom()
	}
	childCount := 2 + g.rng.Intn(2)
	children := make([]exp.Expression, 0, childCount)
	opaque := false
	for i := 0; i < childCount; i++ {
		child, childOpaque := g.tree(depth - 1)
		children = append(children, child)
		opaque = opaque || childOpaque
	}
	if g.rng.Intn(2) == 0 {
		return goqu.And(children...), opaque
	}
	return goqu.Or(children...), opaque
}

func (s *predicateOptimizerSQLiteSuite) TestRandomSmallTrees() {
	generator := &randomPredicateGenerator{
		rng:     rand.New(rand.NewSource(20261110)), //nolint:gosec // deterministic RNG for repeatable test fixtures
		columns: []exp.IdentifierExpression{goqu.I("a"), goqu.I("b"), goqu.I("c")},
	}
	applied := 0
	for i := 0; i < randomTreeCount; i++ {
		predicate, opaque := generator.tree(treeDepth)
		_, report := exp.OptimizePredicateExpression(predicate, exp.DefaultPredicateOptimizationBudget())
		if report.Applied {
			applied++
		}
		originalCount, originalCalls := s.countRows("tr", predicate, false)
		optimizedCount, optimizedCalls := s.countRows("tr", predicate, true)
		s.Equal(originalCount, optimizedCount, "tree %d returned different rows: %+v", i, report)
		if opaque {
			s.Equal(originalCalls, optimizedCalls, "tree %d changed opaque evaluation count", i)
		}
	}
	s.GreaterOrEqual(applied, minOptimizedTrees, "the optimizer should fire on a substantial share of random trees")
}

func (s *predicateOptimizerSQLiteSuite) TestRepeatedFactorReducesSQLAndArgs() {
	large := "the same large parameter is referenced by every conjunction branch"
	a, b, c := goqu.I("a"), goqu.I("b"), goqu.I("c")
	predicate := goqu.Or(
		goqu.And(a.Eq(large), b.Gt(1)),
		goqu.And(a.Eq(large), c.Lt(9)),
	)
	offQuery, offArgs := s.renderPredicate("tr", predicate, false, false)
	onQuery, onArgs := s.renderPredicate("tr", predicate, true, false)
	s.Less(len(onQuery), len(offQuery))
	s.Less(len(onArgs), len(offArgs))
	s.Equal(3, len(onArgs))
	s.Equal(4, len(offArgs))

	offCount, _ := s.countRows("tr", predicate, false)
	onCount, _ := s.countRows("tr", predicate, true)
	s.Equal(offCount, onCount)

	// the optimized statement has the textbook factored shape
	s.Equal(
		"SELECT * FROM `tr` WHERE ((`a` = ?) AND ((`b` > ?) OR (`c` < ?)))",
		onQuery,
	)
}

func (s *predicateOptimizerSQLiteSuite) TestSubqueryIsAnOpaqueBarrier() {
	a, c := goqu.I("a"), goqu.I("c")
	subquery := goqu.From("tt").Select(goqu.I("a")).Where(goqu.I("b").Eq(1))
	predicate := goqu.Or(
		goqu.And(a.Eq(1), goqu.I("a").In(subquery)),
		goqu.And(a.Eq(1), c.Eq(0)),
	)
	// the IN subquery makes the subtree opaque, so the shared "a" factor must not be extracted
	_, report := exp.OptimizePredicateExpression(predicate, exp.DefaultPredicateOptimizationBudget())
	s.False(report.Applied)
	offQuery, offArgs := s.renderPredicate("tt", predicate, false, false)
	onQuery, onArgs := s.renderPredicate("tt", predicate, true, false)
	s.Equal(offQuery, onQuery)
	s.Equal(offArgs, onArgs)
}

func (s *predicateOptimizerSQLiteSuite) TestPreparedAndInterpolatedStayEquivalent() {
	a, b := goqu.I("a"), goqu.I("b")
	predicate := goqu.Or(
		goqu.And(a.Eq(1), b.Eq(0)),
		goqu.And(a.Eq(1), b.Is(nil)),
	)
	for _, prepared := range []bool{true, false} {
		original := s.evalTriStatePrepared(predicate, false, prepared)
		optimized := s.evalTriStatePrepared(predicate, true, prepared)
		s.Equal(original, optimized, fmt.Sprintf("prepared=%v", prepared))
	}
}

func (s *predicateOptimizerSQLiteSuite) evalTriStatePrepared(
	predicate exp.Expression, optimized, prepared bool,
) []sql.NullInt64 {
	dialectName := "sqlite3"
	if optimized {
		dialectName = predOptDialectName
	}
	ds := goqu.Dialect(dialectName).From("tt").Select(predicate)
	if prepared {
		ds = ds.Prepared(true)
	}
	query, args, err := ds.ToSQL()
	s.Require().NoError(err)
	rows, err := s.db.Query(query, args...)
	s.Require().NoError(err)
	defer rows.Close()
	results := make([]sql.NullInt64, 0, triStateRows)
	for rows.Next() {
		var value sql.NullInt64
		s.Require().NoError(rows.Scan(&value))
		results = append(results, value)
	}
	return results
}
