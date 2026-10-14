package main

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The fixtures keep the shape of real OilPriceAPI and ECB responses, trimmed.
const latestFixture = `{"status":"success","data":{"prices":[
	{"price":1451.75,"formatted":"$1451.75","currency":"USD","code":"GASOIL_FUTURES_2026_11","created_at":"2026-10-14T12:32:23.377Z","type":"futures","unit":"tonne","data_status":"current","as_of":"2026-10-14T12:22:00.000Z"},
	{"price":105.81,"formatted":"$105.81","currency":"USD","code":"BRENT_CRUDE_USD","created_at":"2026-10-14T12:31:18.802Z","type":"spot_price","unit":"barrel","data_status":"current","as_of":"2026-10-14T12:21:00.000Z"},
	{"price":3.26,"formatted":"$3.26","currency":"USD","code":"GASOLINE_RBOB_USD","created_at":"2026-10-14T12:14:43.608Z","type":"spot_price","unit":"gallon","data_status":"current","as_of":"2026-10-14T12:26:56.000Z"},
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

// rbobHistoryFixture is a page of hourly averages; the 12:00 bucket of Oct 9
// is the hour still running at the backfill tests' now.
const rbobHistoryFixture = `{"status":"success","data":{"prices":[
	{"price":3.31,"formatted":"$3.31","currency":"USD","code":"GASOLINE_RBOB_USD","created_at":"2026-10-09T12:00:00.000Z","type":"hourly_average","price_type":"spot_price","unit":"gallon","source":"aggregated","as_of":"2026-10-09T12:00:00.000Z","stale":false},
	{"price":3.27,"formatted":"$3.27","currency":"USD","code":"GASOLINE_RBOB_USD","created_at":"2026-10-08T08:00:00.000Z","type":"hourly_average","price_type":"spot_price","unit":"gallon","source":"aggregated","as_of":"2026-10-08T08:00:00.000Z","stale":true,"data_status":"stale"}
],"metadata":{"window":"historical","interval":"hourly","pagination":{"page":1,"per_page":500}}}}`

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
			if got := r.URL.Query().Get("by_code"); got != "GASOIL_FUTURES,BRENT_CRUDE_USD,GASOLINE_RBOB_USD" {
				t.Errorf("by_code = %q", got)
			}
			_, _ = w.Write([]byte(latestFixture))
		case r.URL.Path == "/v1/futures/ice-gasoil/intraday":
			_, _ = w.Write([]byte(gasoilIntradayFixture))
		case r.URL.Path == "/v1/futures/ice-brent/intraday":
			_, _ = w.Write([]byte(brentIntradayFixture))
		case r.URL.Path == "/v1/prices/historical":
			if got := r.URL.Query(); got.Get("by_code") != "GASOLINE_RBOB_USD" || got.Get("interval") != "hourly" {
				t.Errorf("history query = %v", got)
			}
			_, _ = w.Write([]byte(rbobHistoryFixture))
		case r.URL.Path == "/ecb/daily.xml", r.URL.Path == "/ecb/hist.xml":
			_, _ = w.Write([]byte(ecbFixture))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// stubMarketSleep makes the collector's waits instant and records them.
func stubMarketSleep(t *testing.T) *[]time.Duration {
	t.Helper()
	var waits []time.Duration
	original := marketSleep
	marketSleep = func(_ context.Context, d time.Duration) error {
		waits = append(waits, d)
		return nil
	}
	t.Cleanup(func() { marketSleep = original })
	return &waits
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
	// Gasoil + Brent + RBOB (WTI is not ours) + two ECB days.
	if result.Fetched != 5 || result.Stored != 5 {
		t.Fatalf("fetched %d stored %d, want 5 and 5", result.Fetched, result.Stored)
	}
	gasoil, err := loadMarketPrices(ctx, db, marketSeriesGasoil, now.Add(-time.Hour), now)
	if err != nil {
		t.Fatalf("loadMarketPrices: %v", err)
	}
	if len(gasoil) != 1 || gasoil[0].Price != 1451.75 || gasoil[0].Contract != "GASOIL_FUTURES_2026_11" ||
		!gasoil[0].ObservedAt.Equal(time.Date(2026, 10, 14, 12, 22, 0, 0, time.UTC)) {
		t.Fatalf("gasoil = %+v, want the as_of quote", gasoil)
	}
	rbob, err := loadMarketPrices(ctx, db, marketSeriesRBOB, now.Add(-time.Hour), now)
	if err != nil {
		t.Fatalf("loadMarketPrices: %v", err)
	}
	if len(rbob) != 1 || rbob[0].Price != 3.26 || rbob[0].Unit != "gallon" {
		t.Fatalf("rbob = %+v, want the 3.26 USD per gallon quote", rbob)
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
	waits := stubMarketSleep(t)
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
	rbob, err := loadMarketPrices(ctx, db, marketSeriesRBOB, time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 8, 23, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("loadMarketPrices: %v", err)
	}
	// RBOB comes from the hourly averages: the 08:00 bucket is known at
	// 09:00, and the hour still running at now is left out.
	if len(rbob) != 1 || rbob[0].Price != 3.27 || rbob[0].Unit != "gallon" ||
		!rbob[0].ObservedAt.Equal(time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)) {
		t.Fatalf("backfilled rbob = %+v, want the 08:00 average at 09:00 in USD per gallon", rbob)
	}
	if got := result.BackfilledQuotes; got[marketSeriesGasoil] != 2 || got[marketSeriesBrent] != 1 || got[marketSeriesRBOB] != 1 {
		t.Fatalf("backfilled quotes = %v, want gasoil 2, brent 1, rbob 1", got)
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
	// Every backfill request is paced, so a long backfill stays under the
	// per-minute limit.
	if len(*waits) != 3 || (*waits)[0] != marketBackfillRequestGap {
		t.Fatalf("waits = %v, want one gap before each of the 3 backfill requests (2 intraday, 1 history page)", *waits)
	}
}

// TestCollectMarketWaitsOutTheRateLimit pins the 429 handling: the request is
// retried after the wait the API asked for, and a day whose requests still fail
// is reported and not counted as backfilled.
func TestCollectMarketWaitsOutTheRateLimit(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name       string
		limited    int // intraday requests answered 429 before the API recovers
		backfilled int
		warnings   int
	}{
		{"one 429 is waited out", 1, 1, 0},
		{"a lasting 429 gives up and reports the day", 100, 0, 1},
	} {
		db := openTestDB(t)
		waits := stubMarketSleep(t)
		limited := tc.limited
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/v1/prices/latest":
				_, _ = w.Write([]byte(latestFixture))
			case "/v1/futures/ice-gasoil/intraday":
				if limited > 0 {
					limited--
					w.WriteHeader(http.StatusTooManyRequests)
					_, _ = w.Write([]byte(`{"error":"Rate limit exceeded","error_code":"RATE_LIMIT_EXCEEDED","retry_after":4}`))
					return
				}
				_, _ = w.Write([]byte(gasoilIntradayFixture))
			case "/v1/futures/ice-brent/intraday":
				_, _ = w.Write([]byte(brentIntradayFixture))
			case "/v1/prices/historical":
				_, _ = w.Write([]byte(rbobHistoryFixture))
			default:
				_, _ = w.Write([]byte(ecbFixture))
			}
		}))
		result, err := collectMarket(ctx, db, dialectSQLite, testMarketEndpoints(server), "test-key", now, 1)
		server.Close()
		if err != nil {
			t.Fatalf("%s: collectMarket: %v", tc.name, err)
		}
		if result.Backfilled != tc.backfilled || len(result.Warnings) != tc.warnings {
			t.Fatalf("%s: backfilled %d with warnings %v, want %d and %d warnings", tc.name, result.Backfilled, result.Warnings, tc.backfilled, tc.warnings)
		}
		var retryWaits int
		for _, wait := range *waits {
			if wait == 4*time.Second {
				retryWaits++
			}
		}
		wantRetryWaits := tc.limited
		if wantRetryWaits > marketRateLimitAttempts-1 {
			wantRetryWaits = marketRateLimitAttempts - 1
		}
		if retryWaits != wantRetryWaits {
			t.Fatalf("%s: waits = %v, want %d waits of the 4 s the API asked for", tc.name, *waits, wantRetryWaits)
		}
	}
}

// TestFetchOilPriceHourlyHistoryPages follows the history's pages until one
// comes back short.
func TestFetchOilPriceHourlyHistoryPages(t *testing.T) {
	stubMarketSleep(t)
	first := time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC)
	bucket := func(i int) string {
		return `{"price":3.2,"currency":"USD","code":"GASOLINE_RBOB_USD","created_at":"` + first.Add(time.Duration(i)*time.Hour).Format(time.RFC3339) + `"}`
	}
	var pages []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		pages = append(pages, page)
		var items []string
		switch page {
		case "1":
			for i := 0; i < marketHistoryPageSize; i++ {
				items = append(items, bucket(i))
			}
		case "2":
			items = append(items, bucket(marketHistoryPageSize), bucket(marketHistoryPageSize+1))
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"prices":[` + strings.Join(items, ",") + `]}}`))
	}))
	t.Cleanup(server.Close)
	future, _ := marketFutureFor(marketSeriesRBOB)
	prices, err := fetchOilPriceHourlyHistory(context.Background(), server.URL+"/v1", "test-key", future, first, first.AddDate(0, 0, 30))
	if err != nil {
		t.Fatalf("fetchOilPriceHourlyHistory: %v", err)
	}
	if len(pages) != 2 || len(prices) != marketHistoryPageSize+2 {
		t.Fatalf("pages %v gave %d prices, want 2 pages and %d prices", pages, len(prices), marketHistoryPageSize+2)
	}
	if !prices[0].ObservedAt.Equal(first.Add(time.Hour)) {
		t.Fatalf("first price observed at %v, want the end of its hour", prices[0].ObservedAt)
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

// syntheticWholesaleMarket is a market whose noon raise is exactly 1.19 times
// the gasoil move per liter (VAT on top), over six stations and 20 days from
// Sep 1 2026 (UTC). step adds a market-wide level change by time, for a tax
// change on top of the wholesale-driven level.
func syntheticWholesaleMarket(step func(at time.Time) float64) (intervals []priceInterval, futures, rates []marketPrice, now time.Time) {
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
			interval.Price += levels[day] + step(interval.Start)
			intervals = append(intervals, interval)
		}
	}
	for i := 0; i < days; i++ {
		day := firstDay.AddDate(0, 0, i)
		futures = append(futures, marketPrice{ObservedAt: day.Add(9 * time.Hour), Price: gasoil[i]})
		futures = append(futures, marketPrice{ObservedAt: day.Add(12 * time.Hour), Price: gasoil[i]})
		rates = append(rates, marketPrice{ObservedAt: day, Price: rate})
	}
	return intervals, futures, rates, firstDay.AddDate(0, 0, days)
}

// TestMarketBacktestFindsTheWholesaleRelation checks the backtest recovers the
// synthetic market's 1.19 and beats a flat level.
func TestMarketBacktestFindsTheWholesaleRelation(t *testing.T) {
	intervals, futures, rates, now := syntheticWholesaleMarket(func(time.Time) float64 { return 0 })
	result := marketBacktest(intervals, nil, marketSeriesGasoil, futures, rates, now, time.UTC)
	if result.AnchorHour != 12 {
		t.Fatalf("anchor = %d, want 12", result.AnchorHour)
	}
	if result.Days < 10 {
		t.Fatalf("only %d days paired: %+v", result.Days, result.Rows)
	}
	if math.Abs(result.Slope-1.19) > 0.02 || result.R2 < 0.99 {
		t.Fatalf("slope %.3f R² %.3f, want 1.19 and a near-perfect fit", result.Slope, result.R2)
	}
	if result.MAEFitCt >= result.MAEFlatCt {
		t.Fatalf("fit MAE %.3f ct does not beat the flat level's %.3f ct", result.MAEFitCt, result.MAEFlatCt)
	}
	if result.EventDays != 0 {
		t.Fatalf("left out %d days without any price event", result.EventDays)
	}
}

// TestMarketBacktestLeavesOutPriceEventDays puts a tax cut into the synthetic
// market at midnight, passed on only in part (13.7 of 16.7 ct), as the
// Tankrabatt was on Oct 1 2026. Without the event the one tax day ruins the
// fit; with it the two jumps that span the cut are left out and the wholesale
// relation is found again.
func TestMarketBacktestLeavesOutPriceEventDays(t *testing.T) {
	cut := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC)
	intervals, futures, rates, now := syntheticWholesaleMarket(func(at time.Time) float64 {
		if at.Before(cut) {
			return 0
		}
		return -0.137
	})
	events := []priceEvent{{EffectiveAt: cut, Fuel: priceEventAllFuels, Delta: -0.167}}

	unaware := marketBacktest(intervals, nil, marketSeriesGasoil, futures, rates, now, time.UTC)
	if unaware.R2 > 0.5 {
		t.Fatalf("without the event R² is %.3f; the tax day should spoil the fit", unaware.R2)
	}
	result := marketBacktest(intervals, events, marketSeriesGasoil, futures, rates, now, time.UTC)
	// The cut lands inside the Sep 11 pricing day, so the jumps of Sep 11
	// and Sep 12 both span it.
	if result.EventDays != 2 {
		t.Fatalf("left out %d days, want the 2 jumps around the cut", result.EventDays)
	}
	for _, row := range result.Rows {
		if row.Day == "2026-09-11" || row.Day == "2026-09-12" {
			t.Fatalf("day %s spans the cut but was fitted", row.Day)
		}
	}
	if math.Abs(result.Slope-1.19) > 0.02 || result.R2 < 0.99 {
		t.Fatalf("slope %.3f R² %.3f, want 1.19 and a near-perfect fit with the event known", result.Slope, result.R2)
	}
}

func TestJumpSpansPriceEventBounds(t *testing.T) {
	event := func(at time.Time) []priceEvent {
		return []priceEvent{{EffectiveAt: at, Fuel: priceEventAllFuels, Delta: 0.167}}
	}
	noonJan1 := time.Date(2027, 1, 1, 12, 0, 0, 0, time.UTC)
	// A raise at the Jan 1 noon is the Jan 1 jump itself; the Jan 2 jump
	// compares two levels that both include it.
	if !jumpSpansPriceEvent(event(noonJan1), "2027-01-01", 12, time.UTC) {
		t.Fatal("the jump at the event's own noon does not span it")
	}
	if jumpSpansPriceEvent(event(noonJan1), "2027-01-02", 12, time.UTC) {
		t.Fatal("the day after a noon event was left out")
	}
	if jumpSpansPriceEvent(event(noonJan1), "2026-12-31", 12, time.UTC) {
		t.Fatal("the day before a noon event was left out")
	}
}

func TestWholesaleMoveConvertsRBOBGallons(t *testing.T) {
	day := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	rates := []marketPrice{{ObservedAt: day.AddDate(0, 0, -1), Price: 1.0}}
	futures := []marketPrice{
		{ObservedAt: day.Add(-12 * time.Hour), Price: 3.20},
		{ObservedAt: day.Add(11 * time.Hour), Price: 3.30},
	}
	move, ok := wholesaleMoveEuroPerLiter(marketSeriesRBOB, futures, rates, "2026-10-13", 12, time.UTC)
	if !ok || math.Abs(move-0.10/litersPerGallon) > 1e-9 {
		t.Fatalf("move = %v %v, want 10 US cents per gallon in euro per liter", move, ok)
	}
}

func TestFitJumpBacktestNeedsThreeDays(t *testing.T) {
	result := fitJumpBacktest([]string{"a", "b"}, map[string]float64{"a": 0.01, "b": 0.02}, map[string]float64{"a": 0.01, "b": 0.02})
	if result.Days != 2 || result.Rows != nil {
		t.Fatalf("result = %+v, want no fit below three days", result)
	}
}
