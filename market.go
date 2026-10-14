package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Wholesale market data.
//
// Retail prices follow the wholesale market: diesel tracks ICE Low Sulphur
// Gasoil, petrol tracks crude, both converted at the euro/dollar rate. Under the
// once-per-day-raise rule the day's raise lands at noon, hours after the
// morning's futures trading, so the wholesale move since the previous noon may
// say in advance how large the next raise will be. Whether it does, and by how
// much, is an open question: `gasoline market` only collects the series and
// `gasoline market backtest` measures the relation against the recorded noon
// jumps. Nothing here feeds the forecast.
//
// Futures come from OilPriceAPI (OILPRICEAPI_KEY); its free plan allows 50
// requests a day, and one collection run costs one request, so an hourly cron
// fits. The euro reference rate comes from the ECB, which needs no key.

const (
	envOilPriceAPIKeyName = "OILPRICEAPI_KEY"

	// Series stored in market_prices.
	marketSeriesGasoil = "gasoil" // ICE Low Sulphur Gasoil front month, USD per tonne
	marketSeriesBrent  = "brent"  // ICE Brent front month, USD per barrel
	marketSeriesEURUSD = "eurusd" // ECB euro reference rate, USD per EUR

	// Volume conversions to euro per liter.
	litersPerGasoilTonne = 1183.0 // at a diesel density of 0.845 kg/l
	litersPerBarrel      = 158.987

	// marketBackfillMaxDays bounds --backfill-days. Every day costs one
	// request per futures series; the free plan's daily quota does not cover a
	// long backfill, OilPriceAPI's trial does.
	marketBackfillMaxDays = 90

	// marketPriceMaxAge is how old the last futures observation before a
	// moment may be and still stand for the price at that moment. Futures do
	// not trade overnight or at weekends; a Monday noon must not be compared
	// with a Friday price as if nothing had happened in between.
	marketPriceMaxAge = 18 * time.Hour
	// marketRateMaxAge is the same for the daily ECB rate, which is not set at
	// weekends or on TARGET holidays.
	marketRateMaxAge = 4 * 24 * time.Hour
	// marketJumpMinStations is the fewest stations whose day-over-day level
	// change makes a market jump.
	marketJumpMinStations = 5
)

// marketEndpoints are the upstream URLs; tests point them at a fake server.
type marketEndpoints struct {
	OilPriceAPI string // base URL including /v1
	ECBDaily    string
	ECBHistory  string
}

var defaultMarketEndpoints = marketEndpoints{
	OilPriceAPI: "https://api.oilpriceapi.com/v1",
	ECBDaily:    "https://www.ecb.europa.eu/stats/eurofxref/eurofxref-daily.xml",
	ECBHistory:  "https://www.ecb.europa.eu/stats/eurofxref/eurofxref-hist-90d.xml",
}

// marketPrice is one observation of one series.
type marketPrice struct {
	Series     string    `json:"series"`
	Contract   string    `json:"contract,omitempty"`
	ObservedAt time.Time `json:"observed_at"`
	Price      float64   `json:"price"`
	Unit       string    `json:"unit"`
	Currency   string    `json:"currency"`
	Source     string    `json:"source"`
}

type marketCollectResult struct {
	Stored     int           `json:"stored"`
	Fetched    int           `json:"fetched"`
	Latest     []marketPrice `json:"latest"`
	Backfilled int           `json:"backfilled_days"`
	Warnings   []string      `json:"warnings,omitempty"`
}

func oilPriceAPIKey() string {
	key := strings.TrimSpace(os.Getenv(envOilPriceAPIKeyName))
	if key != "" {
		return key
	}
	values, err := loadDotEnv(".env")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(values[envOilPriceAPIKeyName])
}

func runMarket(args []string) (err error) {
	if len(args) > 0 && args[0] == "backtest" {
		return runMarketBacktest(args[1:])
	}
	fs := flag.NewFlagSet("market", flag.ContinueOnError)
	dbf := addDBFlags(fs)
	backfillDays := fs.Int("backfill-days", 0, fmt.Sprintf("Also fetch hourly futures for the past N days (1-%d, one request per day and series) and the ECB's 90-day rate history", marketBackfillMaxDays))
	outputLong, outputShort := addOutputFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *backfillDays < 0 || *backfillDays > marketBackfillMaxDays {
		return fmt.Errorf("--backfill-days must be between 0 and %d", marketBackfillMaxDays)
	}
	dbCfg, err := resolveDBConfig(fs, dbf)
	if err != nil {
		return err
	}
	output, err := resolveOutputMode(*outputLong, *outputShort)
	if err != nil {
		return err
	}

	ctx := context.Background()
	db, err := openDatabase(ctx, dbCfg)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := initSchema(ctx, db, dbCfg.Driver); err != nil {
		return err
	}

	stats := beginCommandRun(ctx, db, "market")
	defer func() { stats.finish(ctx, err) }()

	result, err := collectMarket(ctx, db, dbCfg.Driver, defaultMarketEndpoints, oilPriceAPIKey(), time.Now().UTC(), *backfillDays)
	stats.set("fetched", float64(result.Fetched))
	stats.set("stored", float64(result.Stored))
	stats.set("warnings", float64(len(result.Warnings)))
	if err != nil {
		return err
	}
	if len(result.Warnings) > 0 && result.Fetched > 0 {
		stats.markPartial()
	}

	if output == outputJSON {
		return writeJSON(result)
	}
	for _, warning := range result.Warnings {
		fmt.Fprintf(os.Stderr, "warning: %s\n", warning)
	}
	for _, price := range result.Latest {
		fmt.Fprintf(stdout, "%s %s: %.4f %s/%s at %s\n", price.Series, price.Contract, price.Price, price.Currency, price.Unit, price.ObservedAt.Format(time.RFC3339))
	}
	fmt.Fprintf(stdout, "fetched %d, stored %d new observations", result.Fetched, result.Stored)
	if result.Backfilled > 0 {
		fmt.Fprintf(stdout, " (backfilled %d days)", result.Backfilled)
	}
	fmt.Fprintln(stdout)
	return nil
}

// collectMarket fetches the latest futures and the euro rate, optionally
// backfills, and stores whatever is new. A source that fails becomes a warning
// rather than an error, so one outage does not cost the other series; the run
// fails only when nothing at all could be fetched.
func collectMarket(ctx context.Context, db *sql.DB, d dialect, endpoints marketEndpoints, apiKey string, now time.Time, backfillDays int) (marketCollectResult, error) {
	var (
		result marketCollectResult
		prices []marketPrice
	)
	if apiKey == "" {
		result.Warnings = append(result.Warnings, envOilPriceAPIKeyName+" is not set in environment or .env; skipping futures")
	} else {
		latest, err := fetchOilPriceLatest(ctx, endpoints.OilPriceAPI, apiKey, now)
		if err != nil {
			result.Warnings = append(result.Warnings, "futures: "+err.Error())
		}
		result.Latest = append(result.Latest, latest...)
		prices = append(prices, latest...)
		for day := backfillDays; day >= 1; day-- {
			date := now.AddDate(0, 0, -day).Format("2006-01-02")
			for _, family := range []struct{ path, series string }{
				{"ice-gasoil", marketSeriesGasoil},
				{"ice-brent", marketSeriesBrent},
			} {
				hourly, err := fetchOilPriceIntraday(ctx, endpoints.OilPriceAPI, apiKey, family.path, family.series, date)
				if err != nil {
					result.Warnings = append(result.Warnings, fmt.Sprintf("backfill %s %s: %v", family.series, date, err))
					continue
				}
				prices = append(prices, hourly...)
			}
			result.Backfilled++
		}
	}

	rateURL := endpoints.ECBDaily
	if backfillDays > 0 {
		rateURL = endpoints.ECBHistory
	}
	rates, err := fetchECBRates(ctx, rateURL)
	if err != nil {
		result.Warnings = append(result.Warnings, "ecb: "+err.Error())
	} else if len(rates) > 0 {
		newest := rates[0]
		for _, rate := range rates {
			if rate.ObservedAt.After(newest.ObservedAt) {
				newest = rate
			}
		}
		result.Latest = append(result.Latest, newest)
		prices = append(prices, rates...)
	}

	result.Fetched = len(prices)
	if result.Fetched == 0 {
		return result, errors.New("no market data fetched: " + strings.Join(result.Warnings, "; "))
	}
	stored, err := storeMarketPrices(ctx, db, d, prices, now)
	result.Stored = stored
	return result, err
}

func marketGet(ctx context.Context, rawURL, apiKey string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Token "+apiKey)
	}
	req.Header.Set("Accept", "application/json, application/xml")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		snippet := strings.TrimSpace(string(body))
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, snippet)
	}
	return body, nil
}

// fetchOilPriceLatest reads the latest gasoil and Brent front-month prices in
// one request. The observation time is the market's as_of when given.
func fetchOilPriceLatest(ctx context.Context, baseURL, apiKey string, now time.Time) ([]marketPrice, error) {
	query := url.Values{"by_code": {"GASOIL_FUTURES,BRENT_CRUDE_USD"}}
	body, err := marketGet(ctx, strings.TrimRight(baseURL, "/")+"/prices/latest?"+query.Encode(), apiKey)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Status string `json:"status"`
		Data   struct {
			Prices []struct {
				Price      float64 `json:"price"`
				Code       string  `json:"code"`
				Currency   string  `json:"currency"`
				Unit       string  `json:"unit"`
				DataStatus string  `json:"data_status"`
				AsOf       string  `json:"as_of"`
				CreatedAt  string  `json:"created_at"`
			} `json:"prices"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode latest prices: %w", err)
	}
	if payload.Status != "success" {
		return nil, fmt.Errorf("latest prices status %q", payload.Status)
	}
	var prices []marketPrice
	for _, item := range payload.Data.Prices {
		series := ""
		switch {
		case strings.HasPrefix(item.Code, "GASOIL_FUTURES"):
			series = marketSeriesGasoil
		case strings.HasPrefix(item.Code, "BRENT_CRUDE_USD"):
			series = marketSeriesBrent
		default:
			continue
		}
		if item.DataStatus == "stale" || item.Price <= 0 {
			continue
		}
		observedAt := now
		for _, text := range []string{item.AsOf, item.CreatedAt} {
			if parsed, err := time.Parse(time.RFC3339, text); err == nil {
				observedAt = parsed
				break
			}
		}
		prices = append(prices, marketPrice{
			Series:     series,
			Contract:   item.Code,
			ObservedAt: observedAt.UTC().Truncate(time.Second),
			Price:      item.Price,
			Unit:       item.Unit,
			Currency:   item.Currency,
			Source:     "oilpriceapi",
		})
	}
	return prices, nil
}

// fetchOilPriceIntraday reads one day of 5-minute futures prices and keeps the
// front month — the earliest contract month that traded that day — at hourly
// resolution: the last price in each UTC hour.
func fetchOilPriceIntraday(ctx context.Context, baseURL, apiKey, familyPath, series, date string) ([]marketPrice, error) {
	query := url.Values{"date": {date}}
	body, err := marketGet(ctx, strings.TrimRight(baseURL, "/")+"/futures/"+familyPath+"/intraday?"+query.Encode(), apiKey)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Contracts []struct {
			ContractMonth string `json:"contract_month"`
			ContractCode  string `json:"contract_code"`
			PriceData     []struct {
				Timestamp string  `json:"timestamp"`
				Price     float64 `json:"price"`
				Currency  string  `json:"currency"`
			} `json:"price_data"`
		} `json:"contracts"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode intraday: %w", err)
	}
	front := -1
	for i, contract := range payload.Contracts {
		if len(contract.PriceData) == 0 {
			continue
		}
		if front < 0 || contract.ContractMonth < payload.Contracts[front].ContractMonth {
			front = i
		}
	}
	if front < 0 {
		return nil, nil
	}
	contract := payload.Contracts[front]
	unit := "tonne"
	if series == marketSeriesBrent {
		unit = "barrel"
	}
	lastPerHour := make(map[time.Time]marketPrice)
	for _, point := range contract.PriceData {
		at, err := time.Parse(time.RFC3339, point.Timestamp)
		if err != nil || point.Price <= 0 {
			continue
		}
		at = at.UTC()
		hour := at.Truncate(time.Hour)
		if existing, ok := lastPerHour[hour]; ok && !at.After(existing.ObservedAt) {
			continue
		}
		currency := point.Currency
		if currency == "" {
			currency = "USD"
		}
		lastPerHour[hour] = marketPrice{
			Series:     series,
			Contract:   contract.ContractCode,
			ObservedAt: at.Truncate(time.Second),
			Price:      point.Price,
			Unit:       unit,
			Currency:   currency,
			Source:     "oilpriceapi",
		}
	}
	prices := make([]marketPrice, 0, len(lastPerHour))
	for _, price := range lastPerHour {
		prices = append(prices, price)
	}
	sort.Slice(prices, func(i, j int) bool { return prices[i].ObservedAt.Before(prices[j].ObservedAt) })
	return prices, nil
}

// fetchECBRates reads the ECB's USD reference rate from its daily or history
// XML. The rate is set around 14:10 CET and published around 16:00 CET, so it
// is stored as observed at 15:00 UTC of its date — never earlier than it was
// actually known.
func fetchECBRates(ctx context.Context, rawURL string) ([]marketPrice, error) {
	body, err := marketGet(ctx, rawURL, "")
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Days []struct {
			Time  string `xml:"time,attr"`
			Rates []struct {
				Currency string `xml:"currency,attr"`
				Rate     string `xml:"rate,attr"`
			} `xml:"Cube"`
		} `xml:"Cube>Cube"`
	}
	if err := xml.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("decode ECB rates: %w", err)
	}
	var prices []marketPrice
	for _, day := range envelope.Days {
		date, err := time.Parse("2006-01-02", day.Time)
		if err != nil {
			continue
		}
		for _, rate := range day.Rates {
			if rate.Currency != "USD" {
				continue
			}
			value, err := strconv.ParseFloat(rate.Rate, 64)
			if err != nil || value <= 0 {
				continue
			}
			prices = append(prices, marketPrice{
				Series:     marketSeriesEURUSD,
				ObservedAt: date.Add(15 * time.Hour),
				Price:      value,
				Unit:       "EUR",
				Currency:   "USD",
				Source:     "ecb",
			})
		}
	}
	if len(prices) == 0 {
		return nil, errors.New("no USD rate in ECB response")
	}
	return prices, nil
}

// storeMarketPrices inserts observations not stored yet; (series, observed_at)
// is unique, so re-fetching the same quote or re-running a backfill is free.
func storeMarketPrices(ctx context.Context, db *sql.DB, d dialect, prices []marketPrice, now time.Time) (int, error) {
	insert := `INSERT OR IGNORE INTO market_prices (series, contract, observed_at, price, unit, currency, source, fetched_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	if d == dialectMySQL {
		insert = `INSERT IGNORE INTO market_prices (series, contract, observed_at, price, unit, currency, source, fetched_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	}
	fetchedAt := now.UTC().Format(time.RFC3339)
	stored := 0
	for _, price := range prices {
		res, err := db.ExecContext(ctx, insert, price.Series, price.Contract, price.ObservedAt.UTC().Format(time.RFC3339),
			price.Price, price.Unit, price.Currency, price.Source, fetchedAt)
		if err != nil {
			return stored, err
		}
		if n, err := res.RowsAffected(); err == nil {
			stored += int(n)
		}
	}
	return stored, nil
}

func loadMarketPrices(ctx context.Context, db *sql.DB, series string, from, to time.Time) ([]marketPrice, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT series, contract, observed_at, price, unit, currency, source
		FROM market_prices
		WHERE series = ? AND observed_at >= ? AND observed_at <= ?
		ORDER BY observed_at ASC
	`, series, from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var prices []marketPrice
	for rows.Next() {
		var (
			price    marketPrice
			observed string
		)
		if err := rows.Scan(&price.Series, &price.Contract, &observed, &price.Price, &price.Unit, &price.Currency, &price.Source); err != nil {
			return nil, err
		}
		at, err := time.Parse(time.RFC3339, observed)
		if err != nil {
			continue
		}
		price.ObservedAt = at
		prices = append(prices, price)
	}
	return prices, rows.Err()
}

// priceAt is the last observation at or before at, if it is no older than
// maxAge. prices must be ordered by time.
func priceAt(prices []marketPrice, at time.Time, maxAge time.Duration) (marketPrice, bool) {
	i := sort.Search(len(prices), func(i int) bool { return prices[i].ObservedAt.After(at) })
	if i == 0 {
		return marketPrice{}, false
	}
	last := prices[i-1]
	if at.Sub(last.ObservedAt) > maxAge {
		return marketPrice{}, false
	}
	return last, true
}

// contractRolled reports whether two futures quotes come from different dated
// contracts. Front months roll every month, and the gap between neighbouring
// contracts would read as a market move. A quote without a dated contract
// (the latest-price benchmark) is taken to be the front month.
func contractRolled(a, b marketPrice) bool {
	dated := func(p marketPrice) bool { return strings.Contains(p.Contract, "_20") }
	return dated(a) && dated(b) && a.Contract != b.Contract
}

// marketJumps measures the market's day-over-day level change per pricing day:
// for each station the change of its pricing-day baseline against the previous
// day, and per day the median across stations. With the once-per-day-raise
// rule the change happens at the day's start, the noon raise.
func marketJumps(intervals []priceInterval, now time.Time, location *time.Location) (int, map[string]float64) {
	anchor := inferJumpAnchorHour(intervals, location)
	buckets := collectHourBuckets(intervals, now, location)
	baselines := computeDailyBaselines(buckets, anchor, now.In(location))
	changes := make(map[string][]priceSample)
	for _, days := range baselines {
		for day, baseline := range days {
			date, err := time.Parse("2006-01-02", day)
			if err != nil {
				continue
			}
			previous, ok := days[date.AddDate(0, 0, -1).Format("2006-01-02")]
			if !ok {
				continue
			}
			changes[day] = append(changes[day], priceSample{Price: baseline.Value - previous.Value, Weight: 1})
		}
	}
	jumps := make(map[string]float64)
	for day, samples := range changes {
		if len(samples) < marketJumpMinStations {
			continue
		}
		if median, ok := weightedMedianPrice(samples); ok {
			jumps[day] = median
		}
	}
	return anchor, jumps
}

// wholesaleMoveEuroPerLiter is the futures move from the start of the previous
// pricing day to an hour before this one starts — what was known before the
// raise — converted to euro per liter.
func wholesaleMoveEuroPerLiter(series string, futures, rates []marketPrice, day string, anchor int, location *time.Location) (float64, bool) {
	date, err := time.ParseInLocation("2006-01-02", day, location)
	if err != nil {
		return 0, false
	}
	// Wall-clock anchor hour, not midnight plus a duration: on a DST change
	// day the two differ by an hour.
	dayStart := time.Date(date.Year(), date.Month(), date.Day(), anchor, 0, 0, 0, location)
	from, to := dayStart.AddDate(0, 0, -1), dayStart.Add(-time.Hour)
	before, ok := priceAt(futures, from, marketPriceMaxAge)
	if !ok {
		return 0, false
	}
	after, ok := priceAt(futures, to, marketPriceMaxAge)
	if !ok || contractRolled(before, after) {
		return 0, false
	}
	rate, ok := priceAt(rates, to, marketRateMaxAge)
	if !ok || rate.Price <= 0 {
		return 0, false
	}
	liters := litersPerGasoilTonne
	if series == marketSeriesBrent {
		liters = litersPerBarrel
	}
	return (after.Price - before.Price) / rate.Price / liters, true
}

type backtestDay struct {
	Day       string  `json:"day"`
	JumpCent  float64 `json:"jump_cent"`
	MoveCent  float64 `json:"wholesale_move_cent"`
	FitCent   float64 `json:"fitted_cent"`
	ErrorCent float64 `json:"loo_error_cent"`
}

type backtestResult struct {
	Fuel        string        `json:"fuel"`
	Series      string        `json:"series"`
	AnchorHour  int           `json:"anchor_hour"`
	Days        int           `json:"days"`
	Slope       float64       `json:"slope"`
	InterceptCt float64       `json:"intercept_cent"`
	R2          float64       `json:"r2"`
	MAEFlatCt   float64       `json:"mae_flat_cent"`
	MAEMeanCt   float64       `json:"mae_mean_cent"`
	MAEFitCt    float64       `json:"mae_fit_loo_cent"`
	Rows        []backtestDay `json:"rows"`
}

// fitJumpBacktest regresses the noon jump on the wholesale move. It reports
// the in-sample slope and R², and compares three predictors by mean absolute
// error: a flat level (what the forecast assumes today), the mean jump, and the
// fit — the fit leave-one-out, so it cannot score itself on the day it learned
// from.
func fitJumpBacktest(days []string, jumps, moves map[string]float64) backtestResult {
	var result backtestResult
	var xs, ys []float64
	var used []string
	for _, day := range days {
		jump, okJump := jumps[day]
		move, okMove := moves[day]
		if !okJump || !okMove {
			continue
		}
		used = append(used, day)
		xs = append(xs, move)
		ys = append(ys, jump)
	}
	result.Days = len(used)
	if len(used) < 3 {
		return result
	}
	fit := func(skip int) (float64, float64) {
		var n, sx, sy, sxx, sxy float64
		for i := range xs {
			if i == skip {
				continue
			}
			n++
			sx += xs[i]
			sy += ys[i]
			sxx += xs[i] * xs[i]
			sxy += xs[i] * ys[i]
		}
		den := n*sxx - sx*sx
		if den == 0 {
			return 0, sy / n
		}
		slope := (n*sxy - sx*sy) / den
		return slope, (sy - slope*sx) / n
	}
	slope, intercept := fit(-1)
	var meanY float64
	for _, y := range ys {
		meanY += y
	}
	meanY /= float64(len(ys))
	var ssRes, ssTot float64
	for i := range xs {
		residual := ys[i] - (intercept + slope*xs[i])
		ssRes += residual * residual
		ssTot += (ys[i] - meanY) * (ys[i] - meanY)
		looSlope, looIntercept := fit(i)
		looError := ys[i] - (looIntercept + looSlope*xs[i])
		result.MAEFitCt += math.Abs(looError) * 100
		result.MAEFlatCt += math.Abs(ys[i]) * 100
		result.MAEMeanCt += math.Abs(ys[i]-meanY) * 100
		result.Rows = append(result.Rows, backtestDay{
			Day:       used[i],
			JumpCent:  ys[i] * 100,
			MoveCent:  xs[i] * 100,
			FitCent:   (intercept + slope*xs[i]) * 100,
			ErrorCent: looError * 100,
		})
	}
	n := float64(len(xs))
	result.MAEFitCt /= n
	result.MAEFlatCt /= n
	result.MAEMeanCt /= n
	result.Slope = slope
	result.InterceptCt = intercept * 100
	if ssTot > 0 {
		result.R2 = 1 - ssRes/ssTot
	}
	return result
}

func runMarketBacktest(args []string) error {
	fs := flag.NewFlagSet("market backtest", flag.ContinueOnError)
	dbf := addDBFlags(fs)
	fuel := fs.String("fuel", "diesel", "Fuel whose noon jumps are measured: diesel, e5 or e10")
	series := fs.String("series", marketSeriesGasoil, "Wholesale series: gasoil or brent")
	days := fs.Int("days", 30, "Pricing days to evaluate (the history must cover them)")
	outputLong, outputShort := addOutputFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *fuel != "diesel" && *fuel != "e5" && *fuel != "e10" {
		return errors.New("--fuel must be one of: diesel, e5, e10")
	}
	if *series != marketSeriesGasoil && *series != marketSeriesBrent {
		return errors.New("--series must be one of: gasoil, brent")
	}
	if *days < 3 || *days > 90 {
		return errors.New("--days must be between 3 and 90")
	}
	dbCfg, err := resolveDBConfig(fs, dbf)
	if err != nil {
		return err
	}
	output, err := resolveOutputMode(*outputLong, *outputShort)
	if err != nil {
		return err
	}
	ctx := context.Background()
	db, err := openDatabase(ctx, dbCfg)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := initSchema(ctx, db, dbCfg.Driver); err != nil {
		return err
	}

	now := time.Now().UTC()
	historyStart := now.AddDate(0, 0, -*days-2)
	scan, err := loadSnapshotScan(ctx, db, historyStart, now)
	if err != nil {
		return err
	}
	intervals := reconstructPriceIntervals(scan.forFuel(*fuel), historyStart, now)
	anchor, jumps := marketJumps(intervals, now, time.Local)
	futures, err := loadMarketPrices(ctx, db, *series, historyStart.AddDate(0, 0, -2), now)
	if err != nil {
		return err
	}
	rates, err := loadMarketPrices(ctx, db, marketSeriesEURUSD, historyStart.AddDate(0, 0, -7), now)
	if err != nil {
		return err
	}
	var dayList []string
	moves := make(map[string]float64)
	for day := range jumps {
		dayList = append(dayList, day)
		if move, ok := wholesaleMoveEuroPerLiter(*series, futures, rates, day, anchor, time.Local); ok {
			moves[day] = move
		}
	}
	sort.Strings(dayList)
	result := fitJumpBacktest(dayList, jumps, moves)
	result.Fuel, result.Series, result.AnchorHour = *fuel, *series, anchor

	if output == outputJSON {
		return writeJSON(result)
	}
	fmt.Fprintf(stdout, "%s noon jumps vs %s move (anchor %02d:00), %d days with both\n", result.Fuel, result.Series, anchor, result.Days)
	if result.Days < 3 {
		fmt.Fprintln(stdout, "not enough days with both a measured jump and market data; collect longer or run `gasoline market --backfill-days N`")
		return nil
	}
	fmt.Fprintf(stdout, "%-10s %9s %9s %9s %9s\n", "day", "jump ct", "move ct", "fit ct", "loo err")
	for _, row := range result.Rows {
		fmt.Fprintf(stdout, "%-10s %9.2f %9.2f %9.2f %9.2f\n", row.Day, row.JumpCent, row.MoveCent, row.FitCent, row.ErrorCent)
	}
	fmt.Fprintf(stdout, "jump = %.2f ct + %.3f x move   R² %.3f\n", result.InterceptCt, result.Slope, result.R2)
	fmt.Fprintf(stdout, "mean abs error: flat level %.2f ct, mean jump %.2f ct, fit (leave-one-out) %.2f ct\n", result.MAEFlatCt, result.MAEMeanCt, result.MAEFitCt)
	return nil
}
