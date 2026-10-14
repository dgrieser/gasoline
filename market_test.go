package main

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"
)

// The fixtures keep the shape of real OilPriceAPI and ECB responses, trimmed.
const latestFixture = `{"status":"success","data":{"prices":[
	{"price":1451.75,"formatted":"$1451.75","currency":"USD","code":"GASOIL_FUTURES_2026_11","created_at":"2026-10-14T12:32:23.377Z","type":"futures","unit":"tonne","data_status":"current","as_of":"2026-10-14T12:22:00.000Z"},
	{"price":105.81,"formatted":"$105.81","currency":"USD","code":"BRENT_CRUDE_USD","created_at":"2026-10-14T12:31:18.802Z","type":"spot_price","unit":"barrel","data_status":"current","as_of":"2026-10-14T12:21:00.000Z"},
	{"price":99.0,"code":"WTI_USD","currency":"USD","unit":"barrel","data_status":"current"}
],"metadata":{"request_id":"x","timestamp":"2026-10-14T12:37:10Z","version":"v1"}}}`

const gasoilIntradayFixture = `{"commodity":"GASOIL_FUTURES","date":"2026-10-08","contracts":[
	{"contract_month":"2026-11","contract_code":"GASOIL_FUTURES_2026_11","price_data":[
		{"timestamp":"2026-10-08T08:17:18Z","price":1300.0,"currency":"USD"}]},
	{"contract_month":"2026-10","contract_code":"GASOIL_FUTURES_2026_10","price_data":[
		{"timestamp":"2026-10-08T08:17:18Z","price":1400.0,"currency":"USD"},
		{"timestamp":"2026-10-08T08:47:18Z","price":1410.0,"currency":"USD"},
		{"timestamp":"2026-10-08T09:02:18Z","price":1420.0,"currency":"USD"}]},
	{"contract_month":"2026-09","contract_code":"GASOIL_FUTURES_2026_09","price_data":[]}
]}`

const brentIntradayFixture = `{"commodity":"BRENT_FUTURES","date":"2026-10-08","contracts":[
	{"contract_month":"2026-12","contract_code":"BRENT_FUTURES_2026_12","price_data":[
		{"timestamp":"2026-10-08T08:36:21Z","price":101.5,"currency":"USD"}]}
]}`

const ecbFixture = `<?xml version="1.0" encoding="UTF-8"?>
<gesmes:Envelope xmlns:gesmes="http://www.gesmes.org/xml/2002-08-01" xmlns="http://www.ecb.int/vocabulary/2002-08-01/eurofxref">
	<gesmes:subject>Reference rates</gesmes:subject>
	<Cube>
		<Cube time='2026-10-13'>
			<Cube currency='USD' rate='1.1188'/>
			<Cube currency='JPY' rate='176.91'/>
		</Cube>
		<Cube time='2026-10-12'>
			<Cube currency='USD' rate='1.1150'/>
		</Cube>
	</Cube>
</gesmes:Envelope>`

func fakeMarketServer(t *testing.T, requests *[]string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*requests = append(*requests, r.URL.Path)
		if strings.HasPrefix(r.URL.Path, "/v1/") && r.Header.Get("Authorization") != "Token test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/v1/prices/latest":
			if got := r.URL.Query().Get("by_code"); got != "GASOIL_FUTURES,BRENT_CRUDE_USD" {
				t.Errorf("by_code = %q", got)
			}
			_, _ = w.Write([]byte(latestFixture))
		case r.URL.Path == "/v1/futures/ice-gasoil/intraday":
			_, _ = w.Write([]byte(gasoilIntradayFixture))
		case r.URL.Path == "/v1/futures/ice-brent/intraday":
			_, _ = w.Write([]byte(brentIntradayFixture))
		case r.URL.Path == "/ecb/daily.xml", r.URL.Path == "/ecb/hist.xml":
			_, _ = w.Write([]byte(ecbFixture))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func testMarketEndpoints(server *httptest.Server) marketEndpoints {
	return marketEndpoints{
		OilPriceAPI: server.URL + "/v1",
		ECBDaily:    server.URL + "/ecb/daily.xml",
		ECBHistory:  server.URL + "/ecb/hist.xml",
	}
}

func TestCollectMarketStoresLatestAndRates(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	var requests []string
	server := fakeMarketServer(t, &requests)
	now := time.Date(2026, 10, 14, 12, 40, 0, 0, time.UTC)

	result, err := collectMarket(ctx, db, dialectSQLite, testMarketEndpoints(server), "test-key", now, 0)
	if err != nil {
		t.Fatalf("collectMarket: %v", err)
	}
	if len(result.Warnings) != 0 {
		t.Fatalf("warnings = %v", result.Warnings)
	}
	// Gasoil + Brent (WTI is not ours) + two ECB days.
	if result.Fetched != 4 || result.Stored != 4 {
		t.Fatalf("fetched %d stored %d, want 4 and 4", result.Fetched, result.Stored)
	}
	gasoil, err := loadMarketPrices(ctx, db, marketSeriesGasoil, now.Add(-time.Hour), now)
	if err != nil {
		t.Fatalf("loadMarketPrices: %v", err)
	}
	if len(gasoil) != 1 || gasoil[0].Price != 1451.75 || gasoil[0].Contract != "GASOIL_FUTURES_2026_11" ||
		!gasoil[0].ObservedAt.Equal(time.Date(2026, 10, 14, 12, 22, 0, 0, time.UTC)) {
		t.Fatalf("gasoil = %+v, want the as_of quote", gasoil)
	}
	rates, err := loadMarketPrices(ctx, db, marketSeriesEURUSD, now.AddDate(0, 0, -5), now)
	if err != nil {
		t.Fatalf("loadMarketPrices: %v", err)
	}
	if len(rates) != 2 || rates[1].Price != 1.1188 || !rates[1].ObservedAt.Equal(time.Date(2026, 10, 13, 15, 0, 0, 0, time.UTC)) {
		t.Fatalf("rates = %+v, want both USD rates at 15:00 UTC", rates)
	}

	// The same quotes again are not stored twice.
	again, err := collectMarket(ctx, db, dialectSQLite, testMarketEndpoints(server), "test-key", now.Add(time.Minute), 0)
	if err != nil {
		t.Fatalf("second collectMarket: %v", err)
	}
	if again.Stored != 0 {
		t.Fatalf("second run stored %d, want 0", again.Stored)
	}
}

func TestCollectMarketBackfillsTheFrontMonthHourly(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	var requests []string
	server := fakeMarketServer(t, &requests)
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

	result, err := collectMarket(ctx, db, dialectSQLite, testMarketEndpoints(server), "test-key", now, 1)
	if err != nil {
		t.Fatalf("collectMarket: %v", err)
	}
	if result.Backfilled != 1 {
		t.Fatalf("backfilled %d days, want 1", result.Backfilled)
	}
	gasoil, err := loadMarketPrices(ctx, db, marketSeriesGasoil, time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 8, 23, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("loadMarketPrices: %v", err)
	}
	// Front month is 2026-10 (the empty 2026-09 does not count); the 08:xx
	// hour keeps its last quote.
	if len(gasoil) != 2 || gasoil[0].Price != 1410.0 || gasoil[1].Price != 1420.0 || gasoil[0].Contract != "GASOIL_FUTURES_2026_10" {
		t.Fatalf("backfilled gasoil = %+v, want hourly 1410 then 1420 from the October contract", gasoil)
	}
	var sawHistory bool
	for _, path := range requests {
		if path == "/ecb/hist.xml" {
			sawHistory = true
		}
	}
	if !sawHistory {
		t.Fatalf("backfill did not read the ECB history: %v", requests)
	}
}

func TestCollectMarketWithoutKeyStillStoresRates(t *testing.T) {
	db := openTestDB(t)
	var requests []string
	server := fakeMarketServer(t, &requests)
	result, err := collectMarket(context.Background(), db, dialectSQLite, testMarketEndpoints(server), "", time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC), 0)
	if err != nil {
		t.Fatalf("collectMarket: %v", err)
	}
	if len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0], envOilPriceAPIKeyName) {
		t.Fatalf("warnings = %v, want the missing key", result.Warnings)
	}
	for _, path := range requests {
		if strings.HasPrefix(path, "/v1/") {
			t.Fatalf("called the futures API without a key: %v", requests)
		}
	}
	if result.Stored != 2 {
		t.Fatalf("stored %d, want the two ECB rates", result.Stored)
	}
}

func TestCollectMarketFailsWhenNothingArrives(t *testing.T) {
	db := openTestDB(t)
	server := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(server.Close)
	if _, err := collectMarket(context.Background(), db, dialectSQLite, testMarketEndpoints(server), "test-key", time.Now(), 0); err == nil {
		t.Fatal("collectMarket succeeded with every source failing")
	}
}

func TestPriceAtRespectsMaxAge(t *testing.T) {
	base := time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC)
	prices := []marketPrice{
		{ObservedAt: base, Price: 1},
		{ObservedAt: base.Add(2 * time.Hour), Price: 2},
	}
	if _, ok := priceAt(prices, base.Add(-time.Minute), time.Hour); ok {
		t.Fatal("a moment before the first observation has a price")
	}
	if got, ok := priceAt(prices, base.Add(90*time.Minute), time.Hour*2); !ok || got.Price != 1 {
		t.Fatalf("priceAt = %v %v, want the earlier observation", got, ok)
	}
	if got, ok := priceAt(prices, base.Add(2*time.Hour), time.Hour); !ok || got.Price != 2 {
		t.Fatalf("priceAt at an observation = %v %v, want it", got, ok)
	}
	if _, ok := priceAt(prices, base.Add(30*time.Hour), time.Hour); ok {
		t.Fatal("a stale observation stood in for a much later moment")
	}
}

func TestWholesaleMoveSkipsContractRolls(t *testing.T) {
	day := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	rates := []marketPrice{{ObservedAt: day.AddDate(0, 0, -2), Price: 1.0}}
	quotes := func(beforeContract, afterContract string) []marketPrice {
		return []marketPrice{
			{ObservedAt: day.Add(-12 * time.Hour), Price: 1400, Contract: beforeContract},
			{ObservedAt: day.Add(11 * time.Hour), Price: 1450, Contract: afterContract},
		}
	}
	if _, ok := wholesaleMoveEuroPerLiter(marketSeriesGasoil, quotes("GASOIL_FUTURES_2026_10", "GASOIL_FUTURES_2026_11"), rates, "2026-10-13", 12, time.UTC); ok {
		t.Fatal("a move across a contract roll was measured")
	}
	move, ok := wholesaleMoveEuroPerLiter(marketSeriesGasoil, quotes("GASOIL_FUTURES_2026_11", "GASOIL_FUTURES_2026_11"), rates, "2026-10-13", 12, time.UTC)
	if !ok || math.Abs(move-50/litersPerGasoilTonne) > 1e-9 {
		t.Fatalf("move = %v %v, want 50 USD/t in euro per liter", move, ok)
	}
	if _, ok := wholesaleMoveEuroPerLiter(marketSeriesGasoil, quotes("GASOIL_FUTURES_2026_11", ""), rates, "2026-10-13", 12, time.UTC); !ok {
		t.Fatal("the undated latest-price benchmark was treated as a roll")
	}
}

// TestWholesaleMoveUsesWallClockAnchorOnDSTDays pins the window to the local
// jump hour on the day clocks go back (Oct 25 2026 in Germany): midnight plus
// twelve hours would be 11:00 there, an hour early.
func TestWholesaleMoveUsesWallClockAnchorOnDSTDays(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skipf("no tzdata: %v", err)
	}
	rates := []marketPrice{{ObservedAt: time.Date(2026, 10, 23, 15, 0, 0, 0, time.UTC), Price: 1.0}}
	// The window ends at 11:00 local on Oct 25 (10:00 UTC, CET). A quote at
	// 10:30 local (09:30 UTC) is inside it; one at 11:30 local is not.
	futures := []marketPrice{
		{ObservedAt: time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC), Price: 1400}, // Oct 24 12:00 CEST
		{ObservedAt: time.Date(2026, 10, 25, 9, 30, 0, 0, time.UTC), Price: 1450},
		{ObservedAt: time.Date(2026, 10, 25, 10, 30, 0, 0, time.UTC), Price: 1500},
	}
	move, ok := wholesaleMoveEuroPerLiter(marketSeriesGasoil, futures, rates, "2026-10-25", 12, berlin)
	if !ok || math.Abs(move-50/litersPerGasoilTonne) > 1e-9 {
		t.Fatalf("move = %v %v, want the 1400 -> 1450 move (50 USD/t)", move, ok)
	}
}

// TestMarketBacktestFindsTheWholesaleRelation runs the backtest pieces on a
// synthetic market whose noon raise is exactly 1.19 times the gasoil move per
// liter (VAT on top), and checks the fit recovers it and beats a flat level.
func TestMarketBacktestFindsTheWholesaleRelation(t *testing.T) {
	firstDay := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	const days = 20
	rate := 1.10
	// Gasoil in USD/t: a seeded walk, one value per day at 09:00 UTC.
	gasoil := make([]float64, days)
	gasoil[0] = 1300
	for i := 1; i < days; i++ {
		gasoil[i] = gasoil[i-1] + float64((i*37)%11-5)*8
	}
	// The pricing day starting at noon of day i raises by the move from the
	// previous noon to 11:00 — the 09:00 quotes here.
	levels := make([]float64, days)
	levels[0] = 2.00
	for i := 1; i < days; i++ {
		levels[i] = levels[i-1] + 1.19*(gasoil[i]-gasoil[i-1])/rate/litersPerGasoilTonne
	}
	var intervals []priceInterval
	for _, station := range []string{"s1", "s2", "s3", "s4", "s5", "s6"} {
		for _, interval := range sawtoothIntervals(station, firstDay, days, func(int) float64 { return 0 }) {
			// The level steps at noon: hours before noon belong to the
			// previous pricing day.
			day := int(interval.Start.Sub(firstDay).Hours() / 24)
			if interval.Start.Hour() < 12 {
				day--
			}
			if day < 0 {
				day = 0
			}
			interval.Price += levels[day]
			intervals = append(intervals, interval)
		}
	}
	var futures, rates []marketPrice
	for i := 0; i < days; i++ {
		day := firstDay.AddDate(0, 0, i)
		futures = append(futures, marketPrice{ObservedAt: day.Add(9 * time.Hour), Price: gasoil[i]})
		futures = append(futures, marketPrice{ObservedAt: day.Add(12 * time.Hour), Price: gasoil[i]})
		rates = append(rates, marketPrice{ObservedAt: day, Price: rate})
	}
	now := firstDay.AddDate(0, 0, days)

	anchor, jumps := marketJumps(intervals, now, time.UTC)
	if anchor != 12 {
		t.Fatalf("anchor = %d, want 12", anchor)
	}
	var dayList []string
	moves := make(map[string]float64)
	for day := range jumps {
		dayList = append(dayList, day)
		if move, ok := wholesaleMoveEuroPerLiter(marketSeriesGasoil, futures, rates, day, anchor, time.UTC); ok {
			moves[day] = move
		}
	}
	sort.Strings(dayList)
	result := fitJumpBacktest(dayList, jumps, moves)
	if result.Days < 10 {
		t.Fatalf("only %d days paired: jumps %v moves %v", result.Days, jumps, moves)
	}
	if math.Abs(result.Slope-1.19) > 0.02 || result.R2 < 0.99 {
		t.Fatalf("slope %.3f R² %.3f, want 1.19 and a near-perfect fit", result.Slope, result.R2)
	}
	if result.MAEFitCt >= result.MAEFlatCt {
		t.Fatalf("fit MAE %.3f ct does not beat the flat level's %.3f ct", result.MAEFitCt, result.MAEFlatCt)
	}
}

func TestFitJumpBacktestNeedsThreeDays(t *testing.T) {
	result := fitJumpBacktest([]string{"a", "b"}, map[string]float64{"a": 0.01, "b": 0.02}, map[string]float64{"a": 0.01, "b": 0.02})
	if result.Days != 2 || result.Rows != nil {
		t.Fatalf("result = %+v, want no fit below three days", result)
	}
}
