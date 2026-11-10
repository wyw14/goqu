package sqlite3_test

import (
	"context"
	"database/sql"
	"sort"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/doug-martin/goqu/v9"
	"github.com/stretchr/testify/suite"
)

// saleRow is one input row. NULLable keys and amounts are represented with nil so the reference implementation
// has to distinguish a data NULL from a key aggregated away.
type saleRow struct {
	region  interface{}
	product interface{}
	amount  interface{}
}

// groupingResult mirrors the output columns of the lowered GROUPING SETS query.
type groupingResult struct {
	Region      *string         `db:"region"`
	Product     *string         `db:"product"`
	RowCount    int64           `db:"row_count"`
	AmountCount int64           `db:"amount_count"`
	AmountSum   sql.NullInt64   `db:"amount_sum"`
	AmountAvg   sql.NullFloat64 `db:"amount_avg"`
	Gid         int             `db:"gid"`
}

// expectedRow is one row produced by the Go reference implementation.
type expectedRow struct {
	region      interface{}
	product     interface{}
	rowCount    int64
	amountCount int64
	amountSum   sql.NullInt64
	amountAvg   sql.NullFloat64
	gid         int
}

type groupingSetsSuite struct {
	suite.Suite
	db  *goqu.Database
	raw *sql.DB
}

func (s *groupingSetsSuite) SetupSuite() {
	raw, err := sql.Open("sqlite3", ":memory:")
	s.Require().NoError(err)
	s.raw = raw
	s.db = goqu.New("sqlite3", raw)
}

func (s *groupingSetsSuite) SetupTest() {
	_, err := s.db.Exec("DROP TABLE IF EXISTS `sales`")
	s.Require().NoError(err)
	_, err = s.db.Exec("CREATE TABLE `sales` (`region` TEXT, `product` TEXT, `amount` INTEGER)")
	s.Require().NoError(err)

	_, err = s.db.Exec("DROP TABLE IF EXISTS `bits`")
	s.Require().NoError(err)
	_, err = s.db.Exec("CREATE TABLE `bits` (`a` TEXT, `b` TEXT, `c` TEXT)")
	s.Require().NoError(err)
}

// sampleRows includes NULL grouping keys, a NULL amount, repeated groups and more than one region/product value.
func sampleRows() []saleRow {
	return []saleRow{
		{region: "east", product: "apple", amount: int64(10)},
		{region: "east", product: "apple", amount: int64(20)},
		{region: "east", product: "pear", amount: int64(30)},
		{region: "west", product: "apple", amount: nil},
		{region: nil, product: "apple", amount: int64(50)},
		{region: "west", product: nil, amount: int64(40)},
	}
}

func (s *groupingSetsSuite) seedSales(rows []saleRow) {
	for _, row := range rows {
		_, err := s.db.Exec(
			"INSERT INTO `sales` (`region`, `product`, `amount`) VALUES (?, ?, ?)",
			row.region, row.product, row.amount,
		)
		s.Require().NoError(err)
	}
}

// groupingSetsDataset builds the query under test: keys region/product, sets (region,product), (region), ().
func groupingSetsDataset(db *goqu.Database) *goqu.SelectDataset {
	return db.From("sales").
		Select(
			goqu.I("region"),
			goqu.I("product"),
			goqu.COUNT("*").As("row_count"),
			goqu.COUNT("amount").As("amount_count"),
			goqu.SUM("amount").As("amount_sum"),
			goqu.AVG("amount").As("amount_avg"),
			goqu.GroupingID("region", "product").As("gid"),
		).
		GroupingSets(goqu.GroupingSets(0,
			goqu.GroupingSet("region", "product"),
			goqu.GroupingSet("region"),
			goqu.GroupingSet(),
		))
}

// referenceGroupingSets is the independent, in-memory reference implementation of GROUPING SETS semantics. Each
// entry of sets lists the grouped keys; an empty slice is the grand total set. Duplicate sets produce duplicate
// output rows.
func referenceGroupingSets(rows []saleRow, sets [][]string) []expectedRow {
	out := make([]expectedRow, 0, len(sets))
	for _, set := range sets {
		type groupKey struct {
			region  interface{}
			product interface{}
		}
		groups := map[groupKey][]saleRow{}
		var order []groupKey
		for _, row := range rows {
			key := groupKey{}
			for _, keyName := range set {
				switch keyName {
				case "region":
					key.region = row.region
				case "product":
					key.product = row.product
				}
			}
			if _, seen := groups[key]; !seen {
				order = append(order, key)
			}
			groups[key] = append(groups[key], row)
		}

		// The grand total set always yields one row, even on empty input.
		if len(set) == 0 {
			order = []groupKey{{}}
			groups[groupKey{}] = rows
		}

		for _, key := range order {
			members := groups[key]
			expected := expectedRow{
				rowCount: int64(len(members)),
				gid:      referenceBitmap(set),
			}
			var sum int64
			for _, member := range members {
				if member.amount != nil {
					expected.amountCount++
					sum += member.amount.(int64)
				}
			}
			if expected.amountCount > 0 {
				expected.amountSum = sql.NullInt64{Int64: sum, Valid: true}
				expected.amountAvg = sql.NullFloat64{Float64: float64(sum) / float64(expected.amountCount), Valid: true}
			}
			if containsKey(set, "region") {
				expected.region = key.region
			}
			if containsKey(set, "product") {
				expected.product = key.product
			}
			out = append(out, expected)
		}
	}
	return out
}

// referenceBitmap mirrors SQL GROUPING(region, product): the first key is the most significant bit and a set bit
// means the key is aggregated away.
func referenceBitmap(set []string) int {
	bitmap := 0
	for i, keyName := range []string{"region", "product"} {
		if !containsKey(set, keyName) {
			bitmap |= 1 << (1 - i)
		}
	}
	return bitmap
}

func containsKey(set []string, key string) bool {
	for _, k := range set {
		if k == key {
			return true
		}
	}
	return false
}

func sortExpected(rows []expectedRow) {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].gid != rows[j].gid {
			return rows[i].gid < rows[j].gid
		}
		if keyString(rows[i].region) != keyString(rows[j].region) {
			return keyString(rows[i].region) < keyString(rows[j].region)
		}
		return keyString(rows[i].product) < keyString(rows[j].product)
	})
}

func keyString(val interface{}) string {
	if val == nil {
		return "\x00"
	}
	return val.(string)
}

func (s *groupingSetsSuite) assertMatchesReference(actual []groupingResult, expected []expectedRow) {
	s.Require().Len(actual, len(expected))
	sort.Slice(actual, func(i, j int) bool {
		if actual[i].Gid != actual[j].Gid {
			return actual[i].Gid < actual[j].Gid
		}
		if ptrKey(actual[i].Region) != ptrKey(actual[j].Region) {
			return ptrKey(actual[i].Region) < ptrKey(actual[j].Region)
		}
		return ptrKey(actual[i].Product) < ptrKey(actual[j].Product)
	})
	sortExpected(expected)

	for i := range expected {
		got, want := actual[i], expected[i]
		s.Equal(want.gid, got.Gid, "row %d bitmap", i)
		s.Equal(ptrString(want.region), got.Region, "row %d region", i)
		s.Equal(ptrString(want.product), got.Product, "row %d product", i)
		s.Equal(want.rowCount, got.RowCount, "row %d COUNT(*)", i)
		s.Equal(want.amountCount, got.AmountCount, "row %d COUNT(amount)", i)
		s.Equal(want.amountSum, got.AmountSum, "row %d SUM(amount)", i)
		if want.amountAvg.Valid {
			s.Require().True(got.AmountAvg.Valid, "row %d AVG(amount) should be non-null", i)
			s.InDelta(want.amountAvg.Float64, got.AmountAvg.Float64, 1e-9, "row %d AVG(amount)", i)
		} else {
			s.False(got.AmountAvg.Valid, "row %d AVG(amount) should be null", i)
		}
	}
}

func ptrKey(val *string) string {
	if val == nil {
		return "\x00"
	}
	return *val
}

func ptrString(val interface{}) *string {
	if val == nil {
		return nil
	}
	str := val.(string)
	return &str
}

func (s *groupingSetsSuite) TestLoweredGroupingSetsMatchesReference() {
	rows := sampleRows()
	s.seedSales(rows)

	var actual []groupingResult
	s.Require().NoError(groupingSetsDataset(s.db).ScanStructs(&actual))

	expected := referenceGroupingSets(rows, [][]string{
		{"region", "product"},
		{"region"},
		{},
	})
	s.assertMatchesReference(actual, expected)

	// The bitmap must distinguish a data NULL (bitmap 0, key still grouped) from a key aggregated away.
	var dataNullRows, grandTotalRows int
	for _, row := range actual {
		if row.Gid == 0 && row.Region == nil {
			dataNullRows++
		}
		if row.Gid == 3 {
			grandTotalRows++
			s.Nil(row.Region)
			s.Nil(row.Product)
		}
	}
	s.Equal(1, dataNullRows, "the NULL region input row stays a normal group with bitmap 0")
	s.Equal(1, grandTotalRows, "the grand total set contributes exactly one bitmap-3 row")
}

func (s *groupingSetsSuite) TestLoweredGroupingSetsColumnIdentity() {
	s.seedSales(sampleRows())

	rawSQL, _, err := groupingSetsDataset(s.db).ToSQL()
	s.Require().NoError(err)
	s.Contains(rawSQL, "0 AS `gid`")

	rows, err := s.raw.QueryContext(context.Background(), rawSQL)
	s.Require().NoError(err)
	defer rows.Close()
	columns, err := rows.Columns()
	s.Require().NoError(err)
	s.Equal([]string{"region", "product", "row_count", "amount_count", "amount_sum", "amount_avg", "gid"}, columns)
	s.Require().NoError(rows.Err())
}

func (s *groupingSetsSuite) TestLoweredGroupingSetsDuplicateSets() {
	rows := sampleRows()
	s.seedSales(rows)

	ds := s.db.From("sales").
		Select(
			goqu.I("region"),
			goqu.COUNT("*").As("row_count"),
			goqu.GroupingID("region").As("gid"),
		).
		GroupingSets(goqu.GroupingSets(0,
			goqu.GroupingSet("region"),
			goqu.GroupingSet("region"),
			goqu.GroupingSet(),
		))

	var actual []struct {
		Region   *string `db:"region"`
		RowCount int64   `db:"row_count"`
		Gid      int     `db:"gid"`
	}
	s.Require().NoError(ds.ScanStructs(&actual))

	expected := referenceGroupingSets(rows, [][]string{
		{"region"},
		{"region"},
		{},
	})
	s.Require().Len(actual, len(expected))

	regionBranches, grandTotalBranches := 0, 0
	for _, row := range actual {
		switch row.Gid {
		case 0:
			regionBranches++
		case 1:
			grandTotalBranches++
		}
	}
	// Three distinct region values (east, west and the NULL region), and the (region) set is listed twice.
	s.Equal(6, regionBranches, "the (region) set is listed twice, so its groups appear twice")
	s.Equal(1, grandTotalBranches, "the grand total set is listed once")
}

func (s *groupingSetsSuite) TestLoweredGroupingSetsEmptyTableGrandTotal() {
	// No rows are inserted: the empty grouping set must still emit the grand total row, and each aggregate must
	// keep its own empty-set semantics instead of sharing one intermediate value.
	var actual []groupingResult
	s.Require().NoError(groupingSetsDataset(s.db).ScanStructs(&actual))

	s.Require().Len(actual, 1)
	row := actual[0]
	s.Equal(3, row.Gid)
	s.Equal(int64(0), row.RowCount, "COUNT(*) on empty input is 0")
	s.Equal(int64(0), row.AmountCount, "COUNT(amount) on empty input is 0")
	s.False(row.AmountSum.Valid, "SUM(amount) on empty input is NULL, not 0")
	s.False(row.AmountAvg.Valid, "AVG(amount) on empty input is NULL, not 0")
	s.Nil(row.Region)
	s.Nil(row.Product)

	expected := referenceGroupingSets(nil, [][]string{{"region", "product"}, {"region"}, {}})
	s.assertMatchesReference(actual, expected)
}

func (s *groupingSetsSuite) TestLoweredGroupingSetsBitmapBitOrder() {
	_, err := s.db.Exec("INSERT INTO `bits` (`a`, `b`, `c`) VALUES ('a1', 'b1', 'c1'), ('a1', 'b2', 'c2')")
	s.Require().NoError(err)

	ds := s.db.From("bits").
		Select(
			goqu.I("a"), goqu.I("b"), goqu.I("c"),
			goqu.COUNT("*").As("cnt"),
			goqu.GroupingID("a", "b", "c").As("gid"),
		).
		GroupingSets(goqu.GroupingSets(0,
			goqu.GroupingSet("a", "b", "c"),
			goqu.GroupingSet("a", "c"),
			goqu.GroupingSet("c"),
			goqu.GroupingSet(),
		))

	type bitRow struct {
		A   *string `db:"a"`
		B   *string `db:"b"`
		C   *string `db:"c"`
		Cnt int64   `db:"cnt"`
		Gid int     `db:"gid"`
	}
	var actual []bitRow
	s.Require().NoError(ds.ScanStructs(&actual))

	seen := map[int]bool{}
	for _, row := range actual {
		seen[row.Gid] = true
		switch row.Gid {
		case 0: // (a,b,c): nothing aggregated away
			s.NotNil(row.A)
			s.NotNil(row.B)
			s.NotNil(row.C)
		case 2: // (a,c): b is aggregated away, bit position 1 => 2
			s.NotNil(row.A)
			s.Nil(row.B)
			s.NotNil(row.C)
		case 6: // (c): a and b aggregated away => 4|2
			s.Nil(row.A)
			s.Nil(row.B)
			s.NotNil(row.C)
		case 7: // (): all three aggregated away
			s.Nil(row.A)
			s.Nil(row.B)
			s.Nil(row.C)
		default:
			s.Failf("unexpected bitmap", "gid=%d", row.Gid)
		}
	}
	s.ElementsMatch([]int{0, 2, 6, 7}, keysOf(seen))
}

func keysOf(m map[int]bool) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func (s *groupingSetsSuite) TestLoweredGroupingSetsGlobalOrderLimit() {
	s.seedSales(sampleRows())

	ds := groupingSetsDataset(s.db).Order(goqu.I("gid").Asc(), goqu.I("row_count").Desc()).Limit(3)
	var actual []groupingResult
	s.Require().NoError(ds.ScanStructs(&actual))
	s.Require().Len(actual, 3, "LIMIT applies to the merged UNION ALL result, not each branch")

	previousGid := -1
	for _, row := range actual {
		s.GreaterOrEqual(row.Gid, previousGid)
		previousGid = row.Gid
	}
}

func (s *groupingSetsSuite) TestLoweredGroupingSetsHavingPerSet() {
	s.seedSales(sampleRows())

	ds := groupingSetsDataset(s.db).Having(
		goqu.GroupingID("region", "product").Gt(0),
		goqu.COUNT("*").Gt(1),
	)
	var actual []groupingResult
	s.Require().NoError(ds.ScanStructs(&actual))

	for _, row := range actual {
		s.Greater(row.Gid, 0, "HAVING on the bitmap is evaluated per branch")
		s.Greater(row.RowCount, int64(1), "HAVING on the aggregate is evaluated per branch")
	}

	// On an empty table the grand total aggregate is COUNT(*)=0, so HAVING removes the only row.
	_, err := s.db.Exec("DELETE FROM `sales`")
	s.Require().NoError(err)
	var empty []groupingResult
	s.Require().NoError(ds.ScanStructs(&empty))
	s.Empty(empty)
}

func (s *groupingSetsSuite) TestLoweredGroupingSetsPreparedArgs() {
	s.seedSales(sampleRows())

	ds := groupingSetsDataset(s.db).
		Where(goqu.I("amount").Gt(15)).
		Prepared(true)

	sqlText, args, err := ds.ToSQL()
	s.Require().NoError(err)
	s.Contains(sqlText, "?")
	// Three branches share the same filter: each argument appears once per branch in branch order.
	s.Len(args, 3)
	s.Equal([]interface{}{int64(15), int64(15), int64(15)}, args)

	var actual []groupingResult
	s.Require().NoError(ds.ScanStructs(&actual))

	filtered := make([]saleRow, 0)
	for _, row := range sampleRows() {
		if amount, ok := row.amount.(int64); ok && amount > 15 {
			filtered = append(filtered, row)
		}
	}
	expected := referenceGroupingSets(filtered, [][]string{{"region", "product"}, {"region"}, {}})
	s.assertMatchesReference(actual, expected)
}

func (s *groupingSetsSuite) TestLoweredGroupingSetsTypedNullRuns() {
	s.seedSales(sampleRows())

	ds := s.db.From("sales").
		Select(
			goqu.GroupingKey("region", "TEXT"),
			goqu.COUNT("*").As("row_count"),
			goqu.GroupingID("region").As("gid"),
		).
		GroupingSets(goqu.GroupingSets(0,
			goqu.GroupingSet(goqu.GroupingKey("region", "TEXT")),
			goqu.GroupingSet(),
		))

	sqlText, _, err := ds.ToSQL()
	s.Require().NoError(err)
	s.Contains(sqlText, "CAST(NULL AS TEXT)")

	var rows []struct {
		Region   *string `db:"region"`
		RowCount int64   `db:"row_count"`
		Gid      int     `db:"gid"`
	}
	s.Require().NoError(ds.ScanStructs(&rows))
	s.NotEmpty(rows)
}

// Sanity check that a query without grouping sets still behaves normally on the same table.
func (s *groupingSetsSuite) TestPlainGroupByUnaffected() {
	s.seedSales(sampleRows())

	type countRow struct {
		Region *string `db:"region"`
		Count  int64   `db:"count"`
	}
	var rows []countRow
	err := s.db.From("sales").
		Select(goqu.I("region"), goqu.COUNT("*").As("count")).
		GroupBy("region").
		Order(goqu.I("region").Asc()).
		ScanStructs(&rows)
	s.Require().NoError(err)
	s.Len(rows, 3)
}

func TestGroupingSetsSuite(t *testing.T) {
	suite.Run(t, new(groupingSetsSuite))
}
