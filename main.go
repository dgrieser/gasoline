package main

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Build metadata, injected via -ldflags by goreleaser.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

const (
	defaultDBPath     = "gasoline.db"
	tankerKoenigBase  = "https://creativecommons.tankerkoenig.de/json"
	nominatimBaseURL  = "https://nominatim.openstreetmap.org/search"
	defaultUserAgent  = "gasoline-cli/1.0 (local utility)"
	envAPIKeyName     = "TANKER_KOENIG_API_KEY"
	envDBPathName     = "GASOLINE_DB_PATH"
	envBaseURLName    = "GASOLINE_BASE_URL"
	sqliteBusyTimeout = 5000
	defaultRadiusKm   = 5.0
)

type config struct {
	APIKey    string
	UserAgent string
}

type tankerListResponse struct {
	OK       bool            `json:"ok"`
	Message  string          `json:"message"`
	Status   string          `json:"status"`
	Stations []tankerStation `json:"stations"`
}

type tankerStation struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Brand       string   `json:"brand"`
	Street      string   `json:"street"`
	Place       string   `json:"place"`
	Lat         float64  `json:"lat"`
	Lng         float64  `json:"lng"`
	Dist        float64  `json:"dist"`
	Diesel      *float64 `json:"diesel"`
	E5          *float64 `json:"e5"`
	E10         *float64 `json:"e10"`
	IsOpen      bool     `json:"isOpen"`
	HouseNumber string   `json:"houseNumber"`
	PostCode    int      `json:"postCode"`
}

type nominatimResult struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Lat         string `json:"lat"`
	Lon         string `json:"lon"`
}

type cachedCity struct {
	QueryName   string `json:"-"`
	Name        string
	DisplayName string
	Lat         float64
	Lng         float64
}

type outputMode string

const (
	outputText outputMode = "txt"
	outputJSON outputMode = "json"
)

type updateResult struct {
	City        cachedCity `json:"city"`
	CacheStatus string     `json:"cache_status"`
	StoredCount int        `json:"stored_count"`
	RecordedAt  string     `json:"recorded_at"`
	DBPath      string     `json:"db_path"`
}

// cityUpdateResult is one target's outcome. FetchedCount is what the API
// returned for that target; StoredCount is how many of those snapshots this
// target wrote. They differ when an overlapping target's centre sits nearer to
// a shared station, which makes that target write it instead (see
// dedupeFetches).
type cityUpdateResult struct {
	Query        string     `json:"query"`
	City         cachedCity `json:"city"`
	CacheStatus  string     `json:"cache_status,omitempty"`
	RadiusKm     float64    `json:"radius_km"`
	FetchedCount int        `json:"fetched_count"`
	StoredCount  int        `json:"stored_count"`
	RecordedAt   string     `json:"recorded_at,omitempty"`
	Error        string     `json:"error,omitempty"`
}

type multiUpdateResult struct {
	Results []cityUpdateResult `json:"results"`
	// FetchedCount sums what the targets returned; StoredCount is the rows
	// written after overlapping targets are de-duplicated.
	FetchedCount int    `json:"fetched_count"`
	StoredCount  int    `json:"stored_count"`
	RecordedAt   string `json:"recorded_at"`
	DBPath       string `json:"db_path"`
}

type compactResult struct {
	StationsProcessed int    `json:"stations_processed"`
	BeforeCount       int    `json:"before_count"`
	AfterCount        int    `json:"after_count"`
	DeletedCount      int    `json:"deleted_count"`
	UpdatedCount      int    `json:"updated_count"`
	DBPath            string `json:"db_path"`
}

type importCitiesResult struct {
	CountryCode   string `json:"country_code"`
	SourceURL     string `json:"source_url"`
	ParsedCount   int    `json:"parsed_count"`
	ImportedCount int    `json:"imported_count"`
	DBPath        string `json:"db_path"`
}

type migrateResult struct {
	Applied []string `json:"applied"`
	DBPath  string   `json:"db_path"`
}

type clearCitiesResult struct {
	ClearedCount int    `json:"cleared_count"`
	DBPath       string `json:"db_path"`
}

type renameResult struct {
	StationID string `json:"station_id"`
	Previous  string `json:"previous"`
	New       string `json:"new"`
	Cleared   bool   `json:"cleared"`
}

type cityRow struct {
	Name        string  `json:"name"`
	DisplayName string  `json:"display_name"`
	Lat         float64 `json:"lat"`
	Lng         float64 `json:"lng"`
	CreatedAt   string  `json:"created_at"`
}

type stationRow struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Brand      string   `json:"brand"`
	Street     string   `json:"street"`
	Place      string   `json:"place"`
	RecordedAt string   `json:"recorded_at"`
	IsOpen     bool     `json:"is_open"`
	E5         *float64 `json:"e5"`
	E10        *float64 `json:"e10"`
	Diesel     *float64 `json:"diesel"`
}

type historyRow struct {
	StationID   string   `json:"station_id"`
	StationName string   `json:"station_name"`
	CityName    string   `json:"city_name"`
	RecordedAt  string   `json:"recorded_at"`
	IsOpen      bool     `json:"is_open"`
	E5          *float64 `json:"e5,omitempty"`
	E10         *float64 `json:"e10,omitempty"`
	Diesel      *float64 `json:"diesel,omitempty"`
}

type suggestionRow struct {
	Date           string               `json:"date"`
	Weekday        string               `json:"weekday"`
	StartTime      string               `json:"start_time"`
	EndTime        string               `json:"end_time"`
	StationID      string               `json:"station_id"`
	StationName    string               `json:"station_name"`
	DistanceKM     float64              `json:"distance_km"`
	Station        suggestionStationRow `json:"station"`
	Fuel           string               `json:"fuel"`
	PredictedPrice float64              `json:"predicted_price"`
	Confidence     string               `json:"confidence"`
	SampleCount    int                  `json:"sample_count"`
}

type suggestionStationRow struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Brand       string  `json:"brand"`
	Street      string  `json:"street"`
	HouseNumber string  `json:"house_number"`
	PostCode    int     `json:"post_code"`
	Place       string  `json:"place"`
	Lat         float64 `json:"lat"`
	Lng         float64 `json:"lng"`
	FirstSeenAt string  `json:"first_seen_at"`
	LastSeenAt  string  `json:"last_seen_at"`
	Address     string  `json:"address"`
	// City is the update target that owns this station: the nearest fed city
	// centre, resolved at collection time. DistanceKM is measured to that
	// centre.
	City       string  `json:"city"`
	DistanceKM float64 `json:"distance_km"`
}

type suggestOptions struct {
	Fuel        string
	HistoryDays int
	PredictDays int
	LimitPerDay int
	Now         time.Time
	Location    *time.Location
}

// normalized fills in the zero clock fields so every entry point into a
// computation shares one notion of "now".
func (o suggestOptions) normalized() suggestOptions {
	if o.Now.IsZero() {
		o.Now = time.Now().UTC()
	}
	if o.Location == nil {
		o.Location = time.Local
	}
	return o
}

type checkOptions struct {
	Fuel        string
	HistoryDays int
	PredictDays int
	Limit       int
	Now         time.Time
	Location    *time.Location
}

func (o checkOptions) normalized() checkOptions {
	if o.Now.IsZero() {
		o.Now = time.Now().UTC()
	}
	if o.Location == nil {
		o.Location = time.Local
	}
	return o
}

type suggestSnapshot struct {
	StationID   string
	StationName string
	DistanceKM  float64
	Station     suggestionStationRow
	RecordedAt  time.Time
	IsOpen      bool
	Price       sql.NullFloat64
}

type priceInterval struct {
	StationID   string
	StationName string
	DistanceKM  float64
	Station     suggestionStationRow
	Start       time.Time
	End         time.Time
	Price       float64
}

type forecastStation struct {
	Station suggestionStationRow
	// OffsetMode marks stations whose samples in WeekdayHour/Hour/Recent are
	// offsets from their pricing-day baseline instead of absolute prices. The
	// zero value keeps the legacy absolute-price behavior.
	OffsetMode bool
	// BaselineForecast is the estimated current price level, added back to the
	// blended offset medians when OffsetMode is set. It is held flat across the
	// prediction window: future baseline shifts are unknowable from history.
	BaselineForecast float64
	// BiasCorrection is a learned correction from persisted predicted-vs-actual
	// errors (see loadLearnedCorrections). Zero when no evaluation data exists.
	BiasCorrection float64
}

// priceSample carries either an absolute price or, for stations in offset
// mode, the difference to the pricing-day baseline.
type priceSample struct {
	Price  float64
	Weight float64
	Date   string
}

// hourBucket is one local-time hour slice of a priceInterval.
type hourBucket struct {
	StationID string
	Start     time.Time // local, hour-truncated
	Minutes   float64
	AgeDays   float64
	Price     float64
}

type dayBaseline struct {
	Value           float64
	CoverageMinutes float64
}

// Structural robustness thresholds of the forecast model. These are not user
// settings: they gate when the baseline/offset decomposition is trustworthy.
const (
	// minBaselineCoverageMinutes is the minimum recorded coverage a pricing
	// day needs before its weighted median counts as a baseline.
	minBaselineCoverageMinutes = 360
	// minBaselineDays is the minimum number of usable pricing-day baselines a
	// station needs before it switches from absolute prices to offsets.
	minBaselineDays = 3
	// minCurrentRegimeCoverageMinutes is the minimum de-shaped coverage since
	// the last jump-anchor crossing needed to trust the current-level estimate.
	minCurrentRegimeCoverageMinutes = 60
	// minJumpDetectionEuro is the smallest median upward move that lets an
	// hour qualify as the market-wide daily jump anchor.
	minJumpDetectionEuro = 0.01
	// jumpDominanceRatio is how clearly the anchor hour's total upward
	// movement must beat the runner-up hour before it is trusted.
	jumpDominanceRatio = 1.5
	// baselineDriftWindowDays is how far back adjacent-day baseline deltas
	// feed the drift estimate. Long enough to smooth daily noise, short
	// enough to track a turning market.
	baselineDriftWindowDays = 7
	// baselineDriftDamping shrinks the measured drift before extrapolating
	// it: day-over-day moves are noisy (they oscillate several cents around a
	// small mean), so extrapolating the full median would overshoot whenever
	// the market turns.
	baselineDriftDamping = 0.5
	// baselineDriftMaxAbsPerDay caps the extrapolated drift in euro per
	// pricing day.
	baselineDriftMaxAbsPerDay = 0.02
	// baselineDriftMinSamples gates the drift until enough adjacent-day
	// deltas exist across stations.
	baselineDriftMinSamples = 5
)

type stationWeekdayHourKey struct {
	StationID string
	Weekday   time.Weekday
	Hour      int
}

type stationHourKey struct {
	StationID string
	Hour      int
}

type forecastModel struct {
	Stations    map[string]forecastStation
	WeekdayHour map[stationWeekdayHourKey][]priceSample
	Hour        map[stationHourKey][]priceSample
	Recent      map[string][]priceSample
	// JumpAnchorHour is the inferred local hour at which the market-wide
	// once-per-day price raise happens (0 when no dominant jump was found, in
	// which case pricing days are plain calendar days).
	JumpAnchorHour int
	// NowLocal is the model's build time in the forecast location. Lead-time
	// dependent corrections need it to place a target relative to now; the
	// zero value disables them, which keeps hand-built models in tests inert.
	NowLocal time.Time
	// BaselineDrift is the damped market-wide day-over-day baseline move in
	// euro per pricing day (see estimateBaselineDrift). Zero in flat markets.
	BaselineDrift float64
	// HourLeadBias holds market-wide learned corrections for the parts of the
	// daily price curve the shape model systematically misses, keyed by local
	// target hour, lead bucket and day class (see loadLearnedCorrections).
	HourLeadBias map[hourLeadKey]float64
	// ConfidenceByLead holds empirically calibrated confidence labels per
	// station and lead bucket, measured from evaluated prediction errors.
	// When a cell exists it overrides the sample-count heuristic.
	ConfidenceByLead map[stationLeadKey]string
	// SuggestionBias is the measured selection bias of suggested windows:
	// picking the minimum predicted window across many candidates
	// preferentially picks windows whose prediction erred low, so the printed
	// price runs optimistic even when the model is unbiased overall. Added to
	// displayed suggestion prices only — never to the persisted grid, which
	// must keep measuring the raw model.
	SuggestionBias float64
}

// leadBucket coarsens a prediction's lead time for learned corrections. The
// boundaries mirror the accuracy page's buckets: within one bucket the error
// profile is close to uniform, across them it visibly shifts.
type leadBucket int

const (
	leadBucket0to1h leadBucket = iota
	leadBucket1to6h
	leadBucket6to24h
	// leadBucketBeyond24h exists so callers can recognize long leads; learned
	// corrections are not trained there (day-ahead surprises would leak into
	// them) but the 6-24h cell still applies, since the intraday shape error
	// it captures does not fade with distance.
	leadBucketBeyond24h
)

func leadBucketFor(minutes float64) leadBucket {
	switch {
	case minutes <= 60:
		return leadBucket0to1h
	case minutes <= 360:
		return leadBucket1to6h
	case minutes <= 1440:
		return leadBucket6to24h
	default:
		return leadBucketBeyond24h
	}
}

// hourLeadKey addresses one cell of the learned hour-of-day correction grid.
type hourLeadKey struct {
	Hour    int
	Lead    leadBucket
	Weekend bool
}

type stationLeadKey struct {
	StationID string
	Lead      leadBucket
}

// isWeekendLike groups days whose pricing behaves like a weekend: the noon
// raise that dominates weekday errors is much weaker or absent on Saturdays,
// Sundays and public holidays (the error tail shows the model over-predicting
// exactly those spikes), so corrections must not blend the two regimes.
func isWeekendLike(t time.Time) bool {
	weekday := t.Weekday()
	return weekday == time.Saturday || weekday == time.Sunday || isGermanHoliday(t)
}

type forecastScore struct {
	PredictedPrice float64
	Confidence     string
	SampleCount    int
	// LearnedCorrection is the part of PredictedPrice contributed by the
	// learned feedback loops (station bias + hour-lead grid). Persisted with
	// each prediction so learning can reconstruct the raw model error later —
	// stored errors always measure the corrected prediction, and training on
	// them as if they were raw would make the loops correct their own output.
	LearnedCorrection float64
}

type priceCheckRow struct {
	RecordedAt            string               `json:"recorded_at"`
	StationID             string               `json:"station_id"`
	StationName           string               `json:"station_name"`
	DistanceKM            float64              `json:"distance_km"`
	Station               suggestionStationRow `json:"station"`
	Fuel                  string               `json:"fuel"`
	CurrentPrice          float64              `json:"current_price"`
	PredictedCurrentPrice float64              `json:"predicted_current_price"`
	HistoryPercentile     float64              `json:"history_percentile"`
	Verdict               string               `json:"verdict"`
	Recommendation        string               `json:"recommendation"`
	ExpectedLower         bool                 `json:"expected_lower"`
	BestFutureDate        string               `json:"best_future_date,omitempty"`
	BestFutureWeekday     string               `json:"best_future_weekday,omitempty"`
	BestFutureStartTime   string               `json:"best_future_start_time,omitempty"`
	BestFutureEndTime     string               `json:"best_future_end_time,omitempty"`
	BestFuturePrice       float64              `json:"best_future_price,omitempty"`
	ExpectedDrop          float64              `json:"expected_drop,omitempty"`
	Confidence            string               `json:"confidence"`
	SampleCount           int                  `json:"sample_count"`

	// CurrentPrice and PredictedCurrentPrice above are rounded for display,
	// but the verdict is decided on the unrounded values. German pump prices
	// carry three decimals, so rounding to two moves a price by up to half a
	// cent — enough to matter when the decision log's whole purpose is
	// measuring a residual against a 1 ct bucket. These keep the values the
	// verdict actually saw. Unexported, so the JSON output is unchanged.
	rawCurrentPrice   float64
	rawPredictedPrice float64
}

type futureForecast struct {
	Start time.Time
	End   time.Time
	Score forecastScore
}

var stdout io.Writer = os.Stdout
var countryCodePattern = regexp.MustCompile(`^[A-Za-z]{2}$`)
var geoNamesFeatureCodePattern = regexp.MustCompile(`^(PPL|PPLC|PPLA[1-9]*)$`)

type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		printUsage()
		return nil
	}

	switch args[0] {
	case "update":
		return runUpdate(args[1:])
	case "compact":
		return runCompact(args[1:])
	case "migrate":
		return runMigrate(args[1:])
	case "migrate-to-mysql":
		return runMigrateToMySQL(args[1:])
	case "list":
		return runList(args[1:])
	case "import":
		return runImport(args[1:])
	case "clear":
		return runClear(args[1:])
	case "cities":
		return runCities(args[1:])
	case "import-cities":
		return runImportCities(args[1:])
	case "stations":
		return runStations(args[1:])
	case "history":
		return runHistory(args[1:])
	case "suggest":
		return runSuggest(args[1:])
	case "check":
		return runCheck(args[1:])
	case "notify":
		return runNotify(args[1:])
	case "rename":
		return runRename(args[1:])
	case "merge-stations":
		return runMergeStations(args[1:])
	case "doctor":
		return runDoctor(args[1:])
	case "version", "-v", "--version":
		fmt.Fprintf(stdout, "gasoline %s (commit %s, built %s)\n", version, commit, date)
		return nil
	case "help", "-h", "--help":
		printUsage()
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func printUsage() {
	fmt.Println(`gasoline: persist Tankerkönig station prices into SQLite or MySQL

Commands:
  update   geocode a city if needed, query Tankerkönig, store station snapshots
  compact  compact existing price snapshots in-place
  migrate  apply schema migrations to an existing database
  migrate-to-mysql copy a SQLite database into a MySQL server
  list cities   list cached city geocodes
  list stations list known stations with latest stored snapshot
  list history  show historical prices
  suggest       predict cheap fueling windows by day and time, for every fuel
  check         check if latest stored prices are currently low, for every fuel
  notify        send Pushover notifications to configured web users
  rename        set a persistent display-name override for a station
  merge-stations merge duplicate station identities into one canonical station
  doctor        inspect a live database read-only: table sizes, indexes, and
                timings plus query plans for the admin accuracy page's SQL
  import cities import GeoNames populated places for a 2-letter country code
  clear cities  clear all cached cities
  version       print build version information

Database:
  Commands use SQLite (--db, default gasoline.db) unless MySQL is selected via
  --db-driver mysql (or GASOLINE_DB_DRIVER=mysql). MySQL connection settings:
  --mysql-dsn "user:pass@tcp(host:3306)/gasoline", or individual
  --mysql-host/--mysql-port/--mysql-user/--mysql-password/--mysql-database flags.
  All settings can also come from the environment or a local .env file:
  GASOLINE_DB_DRIVER, GASOLINE_MYSQL_DSN, GASOLINE_MYSQL_HOST, GASOLINE_MYSQL_PORT,
  GASOLINE_MYSQL_USER, GASOLINE_MYSQL_PASSWORD, GASOLINE_MYSQL_DATABASE.

Examples:
  gasoline update --city "Berlin, Germany" --radius 5
  gasoline update --radius 10 --city Berlin --city "Lübbecke" --radius 25 --city Pforzheim
  gasoline update --city Berlin --db-driver mysql --mysql-dsn "gas:secret@tcp(db.example.com:3306)/gasoline"
  gasoline compact
  gasoline migrate
  gasoline migrate-to-mysql --db gasoline.db --mysql-host db.example.com --mysql-user gas --mysql-password secret --mysql-database gasoline
  gasoline list cities
  gasoline list stations --city "Berlin, Germany"
  gasoline list history --fuel diesel
  gasoline suggest
  gasoline suggest --persist --quiet
  gasoline check
  gasoline notify --dry-run
  gasoline rename <station-id> "Custom Name"
  gasoline rename --clear <station-id>
  gasoline merge-stations --detect
  gasoline merge-stations --into <canonical-id> <duplicate-id> [<duplicate-id>...]
  gasoline doctor
  gasoline doctor --explain --analyze --db-driver mysql
  gasoline doctor --skip-queries --output json
  gasoline import cities DE
  gasoline clear cities`)
}

func runList(args []string) error {
	if len(args) == 0 {
		return errors.New("list requires a subcommand: cities, stations, history")
	}

	switch args[0] {
	case "cities":
		return runCities(args[1:])
	case "stations":
		return runStations(args[1:])
	case "history":
		return runHistory(args[1:])
	default:
		return fmt.Errorf("unknown list subcommand %q", args[0])
	}
}

func runImport(args []string) error {
	if len(args) == 0 {
		return errors.New("import requires a subcommand: cities")
	}

	switch args[0] {
	case "cities":
		return runImportCities(args[1:])
	default:
		return fmt.Errorf("unknown import subcommand %q", args[0])
	}
}

func runClear(args []string) error {
	if len(args) == 0 {
		return errors.New("clear requires a subcommand: cities")
	}

	switch args[0] {
	case "cities":
		return runClearCities(args[1:])
	default:
		return fmt.Errorf("unknown clear subcommand %q", args[0])
	}
}

type updateArgKind int

const (
	argCity updateArgKind = iota
	argRadius
)

type updateArg struct {
	kind   updateArgKind
	city   string
	radius float64
}

// cityFlag and radiusFlag both append to one shared, ordered slice in Set().
// flag.Parse calls Set left-to-right in encounter order, so the slice captures
// the exact interleaved order of --city and --radius across the command line —
// which the precedence rules depend on (stdlib scalar flags lose that order).
type cityFlag struct{ events *[]updateArg }

func (f cityFlag) String() string { return "" }
func (f cityFlag) Set(v string) error {
	*f.events = append(*f.events, updateArg{kind: argCity, city: v})
	return nil
}

type radiusFlag struct{ events *[]updateArg }

func (f radiusFlag) String() string { return "" }
func (f radiusFlag) Set(v string) error {
	r, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil {
		return fmt.Errorf("invalid --radius %q: %w", v, err)
	}
	*f.events = append(*f.events, updateArg{kind: argRadius, radius: r})
	return nil
}

type cityQuery struct {
	name   string
	radius float64
}

// buildCityQueries folds ordered --city/--radius events into per-city queries.
// A --radius before any --city sets the global default; a --radius after a city
// attaches to that city only and does not propagate to later cities.
func buildCityQueries(events []updateArg) []cityQuery {
	global := defaultRadiusKm
	sawCity := false
	var queries []cityQuery
	for _, e := range events {
		switch e.kind {
		case argRadius:
			if !sawCity {
				global = e.radius
			} else {
				queries[len(queries)-1].radius = e.radius
			}
		case argCity:
			sawCity = true
			queries = append(queries, cityQuery{name: e.city, radius: global})
		}
	}
	return queries
}

// validateCityQueries trims names in place and rejects empty queries, empty
// names, out-of-range radii, and duplicate (case-insensitive) city names.
func validateCityQueries(queries []cityQuery) error {
	if len(queries) == 0 {
		return errors.New("update requires --city")
	}
	seen := map[string]bool{}
	for i := range queries {
		queries[i].name = strings.TrimSpace(queries[i].name)
		if queries[i].name == "" {
			return errors.New("--city must not be empty")
		}
		if queries[i].radius <= 0 || queries[i].radius > 25 {
			return fmt.Errorf("--radius for %q must be > 0 and <= 25", queries[i].name)
		}
		key := strings.ToLower(queries[i].name)
		if seen[key] {
			return fmt.Errorf("--city %q given more than once", queries[i].name)
		}
		seen[key] = true
	}
	return nil
}

func runUpdate(args []string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	dbf := addDBFlags(fs)
	var events []updateArg
	fs.Var(cityFlag{&events}, "city", "City or place to geocode (repeatable)")
	fs.Var(radiusFlag{&events}, "radius", "Search radius in km, repeatable; default 5, max 25")
	fuelType := fs.String("fuel", "all", "Fuel type: all, diesel, e5, e10")
	sortBy := fs.String("sort", "dist", "Sort order: dist or price")
	userAgent := fs.String("user-agent", defaultUserAgent, "User-Agent for Nominatim and API calls")
	outputLong, outputShort := addOutputFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	dbCfg, err := resolveDBConfig(fs, dbf)
	if err != nil {
		return err
	}
	output, err := resolveOutputMode(*outputLong, *outputShort)
	if err != nil {
		return err
	}
	if !isValidFuelType(*fuelType) {
		return errors.New("--fuel must be one of: all, diesel, e5, e10")
	}
	if !isValidSort(*sortBy) {
		return errors.New("--sort must be one of: dist, price")
	}
	if *fuelType == "all" {
		*sortBy = "dist"
	}

	cfg, err := loadConfig(*userAgent)
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

	queries := buildCityQueries(events)
	// Without any --city/--radius flags, fall back to the admin-configured
	// update targets. Explicit flags never mix with DB targets.
	if len(events) == 0 {
		targets, err := loadUpdateTargets(ctx, db)
		if err != nil {
			return err
		}
		for _, tgt := range targets {
			queries = append(queries, cityQuery{name: tgt.City, radius: tgt.RadiusKM})
		}
	}
	if err := validateCityQueries(queries); err != nil {
		return err
	}

	// Fetch every target before writing anything: targets with overlapping
	// radii report the same station, and a sweep has to see all of them at
	// once to keep exactly one snapshot per station. Each city is geocoded,
	// fetched, and stamped independently, so a slow or failed earlier city
	// does not backdate later ones.
	runAt := time.Now().UTC().Format(time.RFC3339)
	fetched := make([]*cityFetch, len(queries))
	fetchErrs := make([]string, len(queries))
	failures := 0
	for i, q := range queries {
		f, err := fetchCityStations(ctx, db, cfg, q, *fuelType, *sortBy)
		if err != nil {
			// Single city: preserve the original error shape.
			if len(queries) == 1 {
				return err
			}
			failures++
			fetchErrs[i] = err.Error()
			continue
		}
		fetched[i] = &f
	}

	// queryOf maps a fetch back to the target that produced it, so per-city
	// counts survive the failures that leave holes in `fetched`.
	fetches := make([]cityFetch, 0, len(queries))
	queryOf := make([]int, 0, len(queries))
	for i := range queries {
		if fetched[i] != nil {
			fetches = append(fetches, *fetched[i])
			queryOf = append(queryOf, i)
		}
	}

	observations := dedupeFetches(fetches)
	if len(fetches) > 0 {
		if err := persistSweep(ctx, db, dbCfg.Driver, fetches, observations); err != nil {
			return err
		}
	}

	stored := make([]int, len(queries))
	for _, obs := range observations {
		stored[queryOf[obs.FetchIndex]]++
	}

	results := make([]cityUpdateResult, 0, len(queries))
	totalFetched := 0
	for i, q := range queries {
		if fetched[i] == nil {
			results = append(results, cityUpdateResult{Query: q.name, RadiusKm: q.radius, Error: fetchErrs[i]})
			continue
		}
		f := fetched[i]
		cacheStatus := "resolved via geocoder"
		if f.Cached {
			cacheStatus = "loaded from cache"
		}
		totalFetched += len(f.Stations)
		results = append(results, cityUpdateResult{
			Query:        q.name,
			City:         f.City,
			CacheStatus:  cacheStatus,
			RadiusKm:     q.radius,
			FetchedCount: len(f.Stations),
			StoredCount:  stored[i],
			RecordedAt:   f.RecordedAt.Format(time.RFC3339),
		})
	}

	// Single city: preserve the original behavior and output shape.
	if len(queries) == 1 {
		res := results[0]
		if output == outputJSON {
			return writeJSON(updateResult{
				City:        res.City,
				CacheStatus: res.CacheStatus,
				StoredCount: res.StoredCount,
				RecordedAt:  res.RecordedAt,
				DBPath:      dbCfg.Description(),
			})
		}
		printCityUpdate(res)
		fmt.Fprintf(stdout, "stored %d station snapshots at %s in %s\n", res.StoredCount, res.RecordedAt, dbCfg.Description())
		return nil
	}

	if output == outputJSON {
		if err := writeJSON(multiUpdateResult{
			Results:      results,
			FetchedCount: totalFetched,
			StoredCount:  len(observations),
			RecordedAt:   runAt,
			DBPath:       dbCfg.Description(),
		}); err != nil {
			return err
		}
	} else {
		for _, res := range results {
			if res.Error != "" {
				fmt.Fprintf(stdout, "city: %s\n  error: %s\n\n", res.Query, res.Error)
				continue
			}
			printCityUpdate(res)
			if shared := res.FetchedCount - res.StoredCount; shared > 0 {
				fmt.Fprintf(stdout, "  radius %.2f km, stored %d of %d snapshots at %s (%d shared with a nearer city)\n\n",
					res.RadiusKm, res.StoredCount, res.FetchedCount, res.RecordedAt, shared)
			} else {
				fmt.Fprintf(stdout, "  radius %.2f km, stored %d snapshots at %s\n\n", res.RadiusKm, res.StoredCount, res.RecordedAt)
			}
		}
		fmt.Fprintf(stdout, "updated %d of %d cities, stored %d station snapshots in %s\n",
			len(queries)-failures, len(queries), len(observations), dbCfg.Description())
	}

	if failures > 0 {
		return fmt.Errorf("%d of %d cities failed", failures, len(queries))
	}
	return nil
}

// cityFetch is one target's stations, geocoded and fetched but not yet stored
// as snapshots.
type cityFetch struct {
	Query      cityQuery
	City       cachedCity
	Cached     bool
	Stations   []tankerStation
	RecordedAt time.Time
}

// fetchCityStations geocodes one target and fetches its stations. It writes no
// snapshots, so a whole sweep can be de-duplicated before it touches
// price_snapshots. (Geocoding still caches the city itself, as before.)
func fetchCityStations(ctx context.Context, db *sql.DB, cfg config, q cityQuery, fuelType, sortBy string) (cityFetch, error) {
	location, cached, err := getOrCreateCity(ctx, db, q.name, cfg.UserAgent)
	if err != nil {
		return cityFetch{}, err
	}
	stations, err := fetchStations(ctx, cfg, location.Lat, location.Lng, q.radius, fuelType, sortBy)
	if err != nil {
		return cityFetch{}, err
	}
	// Stamp after the data is fetched so the snapshot reflects when it was
	// observed, not when the (possibly multi-city) run began.
	return cityFetch{
		Query:      q,
		City:       location,
		Cached:     cached,
		Stations:   stations,
		RecordedAt: time.Now().UTC(),
	}, nil
}

// stationObservation is the winning reading for one station in a sweep, plus
// the target that owns it. Ownership and reading are chosen independently: the
// nearest target owns the station, but the freshest fetch supplies the prices.
type stationObservation struct {
	Station    tankerStation
	City       cachedCity
	FetchIndex int
	RadiusKM   float64
	RecordedAt time.Time
	DistanceKM float64
}

// dedupeFetches folds a sweep into one observation per station, so targets
// with overlapping radii no longer store the same reading once per city.
//
// Ownership goes to the nearest centre, ties to the earlier target, so it
// stays stable across runs — that stability is what lets persistPriceSnapshot
// roll an unchanged row forward instead of inserting a fresh one every time.
// The reading itself comes from the newest fetch that saw the station, whoever
// that was: targets are fetched one after another, so a farther target can
// observe a price change the nearer one missed, and dropping it would hide the
// change until the next sweep.
//
// Observations come back in station-id order so a sweep writes deterministically.
func dedupeFetches(fetches []cityFetch) []stationObservation {
	best := make(map[string]stationObservation, len(fetches))
	for i, f := range fetches {
		for _, station := range f.Stations {
			distance := haversineKM(f.City.Lat, f.City.Lng, station.Lat, station.Lng)
			current, seen := best[station.ID]
			if !seen {
				best[station.ID] = stationObservation{
					Station:    station,
					City:       f.City,
					FetchIndex: i,
					RadiusKM:   f.Query.radius,
					RecordedAt: f.RecordedAt,
					DistanceKM: distance,
				}
				continue
			}
			if distance < current.DistanceKM {
				current.City = f.City
				current.FetchIndex = i
				current.RadiusKM = f.Query.radius
				current.DistanceKM = distance
			}
			if f.RecordedAt.After(current.RecordedAt) {
				current.Station = station
				current.RecordedAt = f.RecordedAt
			}
			best[station.ID] = current
		}
	}

	ids := make([]string, 0, len(best))
	for id := range best {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	observations := make([]stationObservation, 0, len(ids))
	for _, id := range ids {
		observations = append(observations, best[id])
	}
	return observations
}

// persistSweep stores one de-duplicated sweep in a single transaction: every
// station once, attributed to the target that owns it, plus every fetched
// city. Best-effort across targets happens in the fetch phase, so a target
// that failed simply contributes no observations here.
func persistSweep(ctx context.Context, db *sql.DB, d dialect, fetches []cityFetch, observations []stationObservation) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	aliases, err := loadStationAliases(ctx, tx)
	if err != nil {
		return err
	}

	centres := newCityCentres()
	written := make(map[string]bool)
	for _, obs := range observations {
		recordedAt := obs.RecordedAt.Format(time.RFC3339)
		// The stations row is kept fresh under the fetched id even for an
		// alias, so last_seen_at keeps recording that the API still returns
		// it; only the price history is redirected to the canonical station.
		if _, err := tx.ExecContext(ctx, stationsUpsertSQL(d),
			obs.Station.ID, obs.Station.Name, obs.Station.Brand, obs.Station.Street, obs.Station.HouseNumber,
			obs.Station.PostCode, obs.Station.Place, obs.Station.Lat, obs.Station.Lng, recordedAt, recordedAt); err != nil {
			return err
		}

		snapshotStation := obs.Station
		if canonical, ok := aliases[obs.Station.ID]; ok {
			snapshotStation.ID = canonical
		}
		// When the API returns a station and one or more of its aliases in
		// the same sweep, only the first write per canonical id counts — they
		// carry the same prices, and a second write would immediately pass
		// the "value changed since previous" test against the first.
		if written[snapshotStation.ID] {
			continue
		}
		written[snapshotStation.ID] = true

		if err := persistPriceSnapshot(ctx, tx, centres, obs.City, snapshotStation, obs.RecordedAt, obs.RadiusKM); err != nil {
			return err
		}
	}

	for _, f := range fetches {
		if _, err := tx.ExecContext(ctx, citiesInsertIgnoreSQL(d),
			f.City.QueryName, f.City.Name, f.City.DisplayName, f.City.Lat, f.City.Lng, f.RecordedAt.Format(time.RFC3339),
		); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// loadStationAliases returns duplicate-station redirects as alias id →
// canonical id, following at most one hop (merge-stations never creates
// chains, but a manual edit must not loop the sweep).
func loadStationAliases(ctx context.Context, tx *sql.Tx) (map[string]string, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, alias_of
		FROM stations
		WHERE alias_of IS NOT NULL AND alias_of != ''
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	aliases := make(map[string]string)
	for rows.Next() {
		var id, canonical string
		if err := rows.Scan(&id, &canonical); err != nil {
			return nil, err
		}
		if canonical != id {
			aliases[id] = canonical
		}
	}
	return aliases, rows.Err()
}

func printCityUpdate(res cityUpdateResult) {
	fmt.Fprintf(stdout, "city: %s\n", res.City.Name)
	fmt.Fprintf(stdout, "display: %s\n", res.City.DisplayName)
	fmt.Fprintf(stdout, "coordinates: %.6f, %.6f (%s)\n", res.City.Lat, res.City.Lng, res.CacheStatus)
}

func runCompact(args []string) error {
	fs := flag.NewFlagSet("compact", flag.ContinueOnError)
	dbf := addDBFlags(fs)
	outputLong, outputShort := addOutputFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
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

	result, err := compactPriceSnapshots(ctx, db)
	if err != nil {
		return err
	}
	result.DBPath = dbCfg.Description()

	if output == outputJSON {
		return writeJSON(result)
	}

	fmt.Fprintf(stdout, "compacted %d stations in %s\n", result.StationsProcessed, dbCfg.Description())
	fmt.Fprintf(stdout, "snapshots: %d -> %d (deleted=%d, updated=%d)\n", result.BeforeCount, result.AfterCount, result.DeletedCount, result.UpdatedCount)
	return nil
}

func runMigrate(args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	dbf := addDBFlags(fs)
	outputLong, outputShort := addOutputFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
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

	if err := ensureSchema(ctx, db, dbCfg.Driver); err != nil {
		return err
	}

	result, err := migrateSchema(ctx, db, dbCfg.Driver)
	if err != nil {
		return err
	}
	result.DBPath = dbCfg.Description()

	if output == outputJSON {
		return writeJSON(result)
	}
	if len(result.Applied) == 0 {
		fmt.Fprintf(stdout, "no migrations needed for %s\n", dbCfg.Description())
		return nil
	}
	fmt.Fprintf(stdout, "applied %d migrations to %s\n", len(result.Applied), dbCfg.Description())
	for _, migration := range result.Applied {
		fmt.Fprintf(stdout, "- %s\n", migration)
	}
	return nil
}

func runCities(args []string) error {
	fs := flag.NewFlagSet("cities", flag.ContinueOnError)
	dbf := addDBFlags(fs)
	outputLong, outputShort := addOutputFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
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

	rows, err := db.QueryContext(ctx, `
		SELECT normalized_name, display_name, lat, lng, created_at
		FROM cities
		ORDER BY normalized_name ASC
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	var results []cityRow
	for rows.Next() {
		var name, displayName, createdAt string
		var lat, lng float64
		if err := rows.Scan(&name, &displayName, &lat, &lng, &createdAt); err != nil {
			return err
		}
		row := cityRow{
			Name:        name,
			DisplayName: displayName,
			Lat:         lat,
			Lng:         lng,
			CreatedAt:   createdAt,
		}
		results = append(results, row)
		if output == outputText {
			fmt.Fprintf(stdout, "%s | %.6f, %.6f | cached_at=%s | %s\n", name, lat, lng, createdAt, displayName)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if output == outputJSON {
		return writeJSON(results)
	}
	return nil
}

func runImportCities(args []string) error {
	fs := flag.NewFlagSet("import-cities", flag.ContinueOnError)
	dbf := addDBFlags(fs)
	outputLong, outputShort := addOutputFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	output, err := resolveOutputMode(*outputLong, *outputShort)
	if err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("import-cities requires a 2-letter country code argument")
	}

	countryCode := strings.ToUpper(strings.TrimSpace(fs.Arg(0)))
	if !countryCodePattern.MatchString(countryCode) {
		return errors.New("import-cities requires a 2-letter country code argument")
	}
	dbCfg, err := resolveDBConfig(fs, dbf)
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

	sourceURL := fmt.Sprintf("https://download.geonames.org/export/dump/%s.zip", countryCode)
	cities, err := downloadGeoNamesCities(ctx, sourceURL, countryCode, defaultUserAgent)
	if err != nil {
		return err
	}

	importedCount, err := importCities(ctx, db, dbCfg.Driver, cities)
	if err != nil {
		return err
	}

	result := importCitiesResult{
		CountryCode:   countryCode,
		SourceURL:     sourceURL,
		ParsedCount:   len(cities),
		ImportedCount: importedCount,
		DBPath:        dbCfg.Description(),
	}
	if output == outputJSON {
		return writeJSON(result)
	}

	fmt.Fprintf(stdout, "source: %s\n", sourceURL)
	fmt.Fprintf(stdout, "country: %s\n", countryCode)
	fmt.Fprintf(stdout, "parsed %d cities\n", result.ParsedCount)
	fmt.Fprintf(stdout, "imported %d cities into %s\n", result.ImportedCount, dbCfg.Description())
	return nil
}

func runClearCities(args []string) error {
	fs := flag.NewFlagSet("clear cities", flag.ContinueOnError)
	dbf := addDBFlags(fs)
	outputLong, outputShort := addOutputFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	output, err := resolveOutputMode(*outputLong, *outputShort)
	if err != nil {
		return err
	}
	dbCfg, err := resolveDBConfig(fs, dbf)
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

	resultExec, err := db.ExecContext(ctx, `DELETE FROM cities`)
	if err != nil {
		return err
	}
	clearedCount, err := resultExec.RowsAffected()
	if err != nil {
		return err
	}

	result := clearCitiesResult{
		ClearedCount: int(clearedCount),
		DBPath:       dbCfg.Description(),
	}
	if output == outputJSON {
		return writeJSON(result)
	}

	fmt.Fprintf(stdout, "cleared %d cities from %s\n", result.ClearedCount, dbCfg.Description())
	return nil
}

func runStations(args []string) error {
	fs := flag.NewFlagSet("stations", flag.ContinueOnError)
	dbf := addDBFlags(fs)
	city := fs.String("city", "", "Optional city filter from stored sync runs")
	limit := fs.Int("limit", 50, "Max rows to print; 0 for no limit")
	outputLong, outputShort := addOutputFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	dbCfg, err := resolveDBConfig(fs, dbf)
	if err != nil {
		return err
	}
	output, err := resolveOutputMode(*outputLong, *outputShort)
	if err != nil {
		return err
	}
	if *limit < 0 {
		return errors.New("--limit must be >= 0")
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

	var (
		rows *sql.Rows
	)
	if strings.TrimSpace(*city) == "" {
		rows, err = db.QueryContext(ctx, `
			SELECT
				s.id, COALESCE(s.name_override, s.name), COALESCE(s.brand, ''), COALESCE(s.street, ''), COALESCE(s.place, ''),
				ps.recorded_at, ps.is_open, ps.e5, ps.e10, ps.diesel
			FROM stations s
			JOIN (
				SELECT station_id, MAX(recorded_at) AS latest_recorded_at
				FROM price_snapshots
				GROUP BY station_id
			) latest ON latest.station_id = s.id
			JOIN price_snapshots ps
				ON ps.station_id = latest.station_id
				AND ps.recorded_at = latest.latest_recorded_at
			ORDER BY COALESCE(s.name_override, s.name) ASC
			LIMIT ?
		`, queryLimit(dbCfg.Driver, *limit))
	} else {
		rows, err = db.QueryContext(ctx, `
			SELECT
				s.id, COALESCE(s.name_override, s.name), COALESCE(s.brand, ''), COALESCE(s.street, ''), COALESCE(s.place, ''),
				ps.recorded_at, ps.is_open, ps.e5, ps.e10, ps.diesel
			FROM stations s
			JOIN (
				SELECT station_id, MAX(recorded_at) AS latest_recorded_at
				FROM price_snapshots
				WHERE city_name = ?
				GROUP BY station_id
			) latest ON latest.station_id = s.id
			JOIN price_snapshots ps
				ON ps.station_id = latest.station_id
				AND ps.recorded_at = latest.latest_recorded_at
			ORDER BY COALESCE(s.name_override, s.name) ASC
			LIMIT ?
		`, strings.TrimSpace(*city), queryLimit(dbCfg.Driver, *limit))
	}
	if err != nil {
		return err
	}
	defer rows.Close()

	var results []stationRow
	for rows.Next() {
		var (
			id, name, brand, street, place, recordedAt string
			isOpen                                     bool
			e5, e10, diesel                            sql.NullFloat64
		)
		if err := rows.Scan(&id, &name, &brand, &street, &place, &recordedAt, &isOpen, &e5, &e10, &diesel); err != nil {
			return err
		}
		row := stationRow{
			ID:         id,
			Name:       name,
			Brand:      brand,
			Street:     strings.TrimSpace(street),
			Place:      strings.TrimSpace(place),
			RecordedAt: recordedAt,
			IsOpen:     isOpen,
			E5:         nullFloatPtr(e5),
			E10:        nullFloatPtr(e10),
			Diesel:     nullFloatPtr(diesel),
		}
		results = append(results, row)
		if output == outputText {
			fmt.Fprintf(stdout, "%s | %s | %s | %s %s | open=%t | e5=%s e10=%s diesel=%s | at=%s\n",
				id,
				name,
				blankDash(brand),
				row.Street,
				row.Place,
				isOpen,
				formatNullFloat(e5),
				formatNullFloat(e10),
				formatNullFloat(diesel),
				recordedAt,
			)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if output == outputJSON {
		return writeJSON(results)
	}
	return nil
}

func runHistory(args []string) error {
	fs := flag.NewFlagSet("history", flag.ContinueOnError)
	dbf := addDBFlags(fs)
	stationID := fs.String("station-id", "", "Station UUID")
	fuel := fs.String("fuel", "all", "Fuel type: all, diesel, e5, e10")
	limit := fs.Int("limit", 100, "Max history rows; 0 for no limit")
	outputLong, outputShort := addOutputFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	dbCfg, err := resolveDBConfig(fs, dbf)
	if err != nil {
		return err
	}
	output, err := resolveOutputMode(*outputLong, *outputShort)
	if err != nil {
		return err
	}
	if !isValidFuelType(*fuel) {
		return errors.New("--fuel must be one of: all, diesel, e5, e10")
	}
	if *limit < 0 {
		return errors.New("--limit must be >= 0")
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

	stationFilter := strings.TrimSpace(*stationID)
	var rows *sql.Rows
	if stationFilter == "" {
		rows, err = db.QueryContext(ctx, `
			SELECT ps.station_id, COALESCE(s.name_override, s.name), ps.city_name, ps.recorded_at, ps.is_open, ps.e5, ps.e10, ps.diesel
			FROM price_snapshots ps
			JOIN stations s ON s.id = ps.station_id
			ORDER BY ps.recorded_at DESC
			LIMIT ?
		`, queryLimit(dbCfg.Driver, *limit))
	} else {
		rows, err = db.QueryContext(ctx, `
			SELECT ps.station_id, COALESCE(s.name_override, s.name), ps.city_name, ps.recorded_at, ps.is_open, ps.e5, ps.e10, ps.diesel
			FROM price_snapshots ps
			JOIN stations s ON s.id = ps.station_id
			WHERE ps.station_id = ?
			ORDER BY ps.recorded_at DESC
			LIMIT ?
		`, stationFilter, queryLimit(dbCfg.Driver, *limit))
	}
	if err != nil {
		return err
	}
	defer rows.Close()

	var results []historyRow
	for rows.Next() {
		var (
			stationID, stationName, cityName, recordedAt string
			isOpen                                       bool
			e5, e10, diesel                              sql.NullFloat64
		)
		if err := rows.Scan(&stationID, &stationName, &cityName, &recordedAt, &isOpen, &e5, &e10, &diesel); err != nil {
			return err
		}
		row := historyRow{
			StationID:   stationID,
			StationName: stationName,
			CityName:    cityName,
			RecordedAt:  recordedAt,
			IsOpen:      isOpen,
		}
		switch *fuel {
		case "e5":
			row.E5 = nullFloatPtr(e5)
			if output == outputText {
				printHistoryText(stationFilter, stationID, stationName, recordedAt, cityName, isOpen, "e5="+formatNullFloat(e5))
			}
		case "e10":
			row.E10 = nullFloatPtr(e10)
			if output == outputText {
				printHistoryText(stationFilter, stationID, stationName, recordedAt, cityName, isOpen, "e10="+formatNullFloat(e10))
			}
		case "diesel":
			row.Diesel = nullFloatPtr(diesel)
			if output == outputText {
				printHistoryText(stationFilter, stationID, stationName, recordedAt, cityName, isOpen, "diesel="+formatNullFloat(diesel))
			}
		default:
			row.E5 = nullFloatPtr(e5)
			row.E10 = nullFloatPtr(e10)
			row.Diesel = nullFloatPtr(diesel)
			if output == outputText {
				printHistoryText(stationFilter, stationID, stationName, recordedAt, cityName, isOpen,
					fmt.Sprintf("e5=%s e10=%s diesel=%s", formatNullFloat(e5), formatNullFloat(e10), formatNullFloat(diesel)))
			}
		}
		results = append(results, row)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if output == outputJSON {
		return writeJSON(results)
	}
	return nil
}

func runSuggest(args []string) error {
	fs := flag.NewFlagSet("suggest", flag.ContinueOnError)
	dbf := addDBFlags(fs)
	persist := fs.Bool("persist", false, "Store the full prediction grid in the database, evaluate past predictions against actual prices, and learn from the errors")
	var quiet bool
	fs.BoolVar(&quiet, "quiet", false, "Suppress the suggestion output; requires --persist (store only)")
	fs.BoolVar(&quiet, "q", false, "Suppress the suggestion output; requires --persist (store only)")
	outputLong, outputShort := addOutputFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	dbCfg, err := resolveDBConfig(fs, dbf)
	if err != nil {
		return err
	}
	output, err := resolveOutputMode(*outputLong, *outputShort)
	if err != nil {
		return err
	}
	if quiet && !*persist {
		return errors.New("--quiet requires --persist")
	}

	opts := suggestOptions{
		HistoryDays: modelHistoryDays,
		PredictDays: forecastPredictDays,
		LimitPerDay: suggestLimitPerDay,
		Now:         time.Now().UTC(),
		Location:    time.Local,
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

	// One pass over the history serves every fuel.
	scan, err := loadSnapshotScan(ctx, db, opts.Now.AddDate(0, 0, -opts.HistoryDays), opts.Now)
	if err != nil {
		return err
	}

	// Best-effort across fuels like `update` is across cities: compute and
	// persist each fuel independently, report failures at the end.
	results := make([]fuelSuggestResult, 0, len(suggestFuels))
	failures := 0
	var totals persistCounts
	var evaluated, outcomes int
	for _, fuel := range suggestFuels {
		fuelOpts := opts
		fuelOpts.Fuel = fuel
		// Evaluate before computing so freshly measured errors feed this
		// fuel's bias correction. Evaluation is fuel-scoped, so it belongs
		// inside the loop.
		if *persist {
			n, err := evaluateDuePredictions(ctx, db, fuel, opts.Now)
			if err != nil {
				return err
			}
			evaluated += n
			// Settle check decisions whose pricing day has finished, so a
			// decision logged today is scored against that day's floor on a
			// later run.
			m, err := evaluateCheckOutcomes(ctx, db, fuel, opts.Now, opts.Location)
			if err != nil {
				return err
			}
			outcomes += m
		}
		suggestions, counts, err := suggestOneFuel(ctx, db, scan, fuelOpts, *persist)
		if err != nil {
			failures++
			if quiet {
				// Quiet suppresses the per-fuel sections, so this is the only
				// place the failure detail can surface.
				fmt.Fprintf(os.Stderr, "suggest %s: %v\n", fuel, err)
			}
			results = append(results, fuelSuggestResult{Fuel: fuel, Error: err.Error()})
			continue
		}
		totals = totals.add(counts)
		if suggestions == nil {
			// A success always carries an array; only failed fuels are null.
			suggestions = []suggestionRow{}
		}
		results = append(results, fuelSuggestResult{Fuel: fuel, Suggestions: suggestions})
	}

	if *persist {
		// Decisions first: prunePredictions collects the runs both tables
		// leave behind, so it has to run last.
		prunedDecisions, err := pruneCheckDecisions(ctx, db, opts.Now)
		if err != nil {
			return err
		}
		pruned, err := prunePredictions(ctx, db, opts.Now)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr,
			"persist: stored %d predictions, %d decisions, evaluated %d, outcomes %d, bias-corrected %d stations, pruned %d/%d\n",
			totals.Predictions, totals.Decisions, evaluated, outcomes, totals.BiasedStations, pruned, prunedDecisions)
	}

	if !quiet {
		if output == outputJSON {
			if err := writeJSON(results); err != nil {
				return err
			}
		} else {
			printFuelResults(results, func(res fuelSuggestResult) {
				printSuggestionsText(res.Suggestions)
			})
		}
	}

	if failures == len(suggestFuels) {
		return fmt.Errorf("all %d fuels failed", failures)
	}
	if failures > 0 {
		return fmt.Errorf("%d of %d fuels failed", failures, len(suggestFuels))
	}
	return nil
}

func runCheck(args []string) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	dbf := addDBFlags(fs)
	outputLong, outputShort := addOutputFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	dbCfg, err := resolveDBConfig(fs, dbf)
	if err != nil {
		return err
	}
	output, err := resolveOutputMode(*outputLong, *outputShort)
	if err != nil {
		return err
	}

	opts := checkOptions{
		HistoryDays: modelHistoryDays,
		PredictDays: forecastPredictDays,
		Limit:       checkRowLimit,
		Now:         time.Now().UTC(),
		Location:    time.Local,
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

	// One pass over the history serves every fuel.
	scan, err := loadSnapshotScan(ctx, db, opts.Now.AddDate(0, 0, -opts.HistoryDays), opts.Now)
	if err != nil {
		return err
	}

	// Best-effort across fuels: check all, report failures at the end.
	results := make([]fuelCheckResult, 0, len(suggestFuels))
	failures := 0
	for _, fuel := range suggestFuels {
		fuelOpts := opts
		fuelOpts.Fuel = fuel
		checks, err := checkGasFromScan(ctx, db, scan, fuelOpts)
		if err != nil {
			failures++
			results = append(results, fuelCheckResult{Fuel: fuel, Error: err.Error()})
			continue
		}
		if checks == nil {
			// A success always carries an array; only failed fuels are null.
			checks = []priceCheckRow{}
		}
		results = append(results, fuelCheckResult{Fuel: fuel, Checks: checks})
	}

	if output == outputJSON {
		if err := writeJSON(results); err != nil {
			return err
		}
	} else {
		printFuelResults(results, func(res fuelCheckResult) {
			printPriceChecksText(res.Checks)
		})
	}
	if failures == len(suggestFuels) {
		return fmt.Errorf("all %d fuels failed", failures)
	}
	if failures > 0 {
		return fmt.Errorf("%d of %d fuels failed", failures, len(suggestFuels))
	}
	return nil
}

func runRename(args []string) error {
	fs := flag.NewFlagSet("rename", flag.ContinueOnError)
	dbf := addDBFlags(fs)
	clear := fs.Bool("clear", false, "Remove the name override (revert to the Tankerkönig name)")
	outputLong, outputShort := addOutputFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	dbCfg, err := resolveDBConfig(fs, dbf)
	if err != nil {
		return err
	}
	output, err := resolveOutputMode(*outputLong, *outputShort)
	if err != nil {
		return err
	}

	rest := fs.Args()
	var stationID, newName string
	if *clear {
		if len(rest) != 1 {
			return errors.New("rename --clear requires exactly one positional argument: <station-id>")
		}
		stationID = strings.TrimSpace(rest[0])
	} else {
		if len(rest) != 2 {
			return errors.New("rename requires two positional arguments: <station-id> <new-name>")
		}
		stationID = strings.TrimSpace(rest[0])
		newName = strings.TrimSpace(rest[1])
		if newName == "" {
			return errors.New("new name must not be empty; use --clear to remove an override")
		}
	}
	if stationID == "" {
		return errors.New("station id must not be empty")
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

	var (
		canonicalName   string
		currentOverride sql.NullString
	)
	if err := db.QueryRowContext(ctx, `SELECT name, name_override FROM stations WHERE id = ?`, stationID).Scan(&canonicalName, &currentOverride); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("station %q not found", stationID)
		}
		return err
	}

	previousEffective := canonicalName
	if currentOverride.Valid {
		previousEffective = currentOverride.String
	}

	var (
		exec         sql.Result
		newEffective string
	)
	if *clear {
		exec, err = db.ExecContext(ctx, `UPDATE stations SET name_override = NULL WHERE id = ?`, stationID)
		newEffective = canonicalName
	} else {
		exec, err = db.ExecContext(ctx, `UPDATE stations SET name_override = ? WHERE id = ?`, newName, stationID)
		newEffective = newName
	}
	if err != nil {
		return err
	}
	affected, err := exec.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return fmt.Errorf("station %q not found", stationID)
	}

	result := renameResult{
		StationID: stationID,
		Previous:  previousEffective,
		New:       newEffective,
		Cleared:   *clear,
	}
	if output == outputJSON {
		return writeJSON(result)
	}
	if *clear {
		fmt.Fprintf(stdout, "cleared override for %s: %q → %q\n", stationID, previousEffective, newEffective)
	} else {
		fmt.Fprintf(stdout, "renamed %s: %q → %q\n", stationID, previousEffective, newEffective)
	}
	return nil
}

func validateSuggestOptions(opts suggestOptions) error {
	if !isSuggestFuelType(opts.Fuel) {
		return errors.New("fuel must be one of: diesel, e5, e10")
	}
	if opts.HistoryDays <= 0 {
		return errors.New("history days must be > 0")
	}
	if opts.PredictDays <= 0 {
		return errors.New("prediction days must be > 0")
	}
	if opts.LimitPerDay <= 0 {
		return errors.New("suggestions per day must be > 0")
	}
	return nil
}

func validateCheckOptions(opts checkOptions) error {
	if !isSuggestFuelType(opts.Fuel) {
		return errors.New("fuel must be one of: diesel, e5, e10")
	}
	if opts.HistoryDays <= 0 {
		return errors.New("history days must be > 0")
	}
	if opts.PredictDays <= 0 {
		return errors.New("prediction days must be > 0")
	}
	if opts.Limit < 0 {
		return errors.New("check row limit must be >= 0")
	}
	return nil
}

// persistCounts tallies what one persisted run wrote, for the summary line.
// Zero across the board when --persist is not set.
type persistCounts struct {
	Predictions    int
	Decisions      int
	BiasedStations int
}

func (c persistCounts) add(other persistCounts) persistCounts {
	c.Predictions += other.Predictions
	c.Decisions += other.Decisions
	c.BiasedStations += other.BiasedStations
	return c
}

// suggestOneFuel computes one fuel's suggestions from the shared history scan
// and, when persist is set, stores the prediction run and the check decisions
// taken against the same model. Returns the suggestions plus the counts for the
// persist summary.
func suggestOneFuel(ctx context.Context, db *sql.DB, scan snapshotScan, opts suggestOptions, persist bool) ([]suggestionRow, persistCounts, error) {
	computation, err := computeSuggestionsFromScan(ctx, db, scan, opts)
	if err != nil {
		return nil, persistCounts{}, err
	}
	if !persist {
		return computation.Suggestions, persistCounts{}, nil
	}
	runID, stored, err := persistPredictionRun(ctx, db, computation, opts)
	if err != nil {
		return nil, persistCounts{}, err
	}
	decisions, err := persistCheckDecisions(ctx, db, computation, opts, runID)
	if err != nil {
		return nil, persistCounts{}, err
	}
	counts := persistCounts{Predictions: stored, Decisions: decisions}
	for _, station := range computation.Model.Stations {
		if station.BiasCorrection != 0 {
			counts.BiasedStations++
		}
	}
	return computation.Suggestions, counts, nil
}

// fuelSuggestResult is one fuel's outcome in a suggest run. Suggestions is
// null only for fuels whose computation failed.
type fuelSuggestResult struct {
	Fuel        string          `json:"fuel"`
	Suggestions []suggestionRow `json:"suggestions"`
	Error       string          `json:"error,omitempty"`
}

// fuelCheckResult is one fuel's outcome in a check run. Checks is null only for
// fuels whose computation failed.
type fuelCheckResult struct {
	Fuel   string          `json:"fuel"`
	Checks []priceCheckRow `json:"checks"`
	Error  string          `json:"error,omitempty"`
}

func (r fuelSuggestResult) header() (string, string) { return r.Fuel, r.Error }
func (r fuelCheckResult) header() (string, string)   { return r.Fuel, r.Error }

// printFuelResults renders the text output of a run: one `fuel: <name>`
// section per result, blank line between sections, an indented error line for
// fuels that failed.
func printFuelResults[T interface{ header() (string, string) }](results []T, printBody func(T)) {
	for i, res := range results {
		if i > 0 {
			fmt.Fprintln(stdout)
		}
		fuel, errMsg := res.header()
		fmt.Fprintf(stdout, "fuel: %s\n", fuel)
		if errMsg != "" {
			fmt.Fprintf(stdout, "  error: %s\n", errMsg)
			continue
		}
		printBody(res)
	}
}

// suggestComputation carries the full state of one suggest run so the
// persistent mode can store the complete forecast grid, not just the printed
// suggestions.
type suggestComputation struct {
	Now         time.Time
	Location    *time.Location
	Model       forecastModel
	Suggestions []suggestionRow
	// Snapshots are the raw rows the model was built from. They are kept so
	// the persist path can score the current hour against the observed price
	// without reloading them.
	Snapshots []suggestSnapshot
}

func suggestGas(ctx context.Context, db *sql.DB, opts suggestOptions) ([]suggestionRow, error) {
	computation, err := computeSuggestions(ctx, db, opts)
	if err != nil {
		return nil, err
	}
	return computation.Suggestions, nil
}

func computeSuggestions(ctx context.Context, db *sql.DB, opts suggestOptions) (*suggestComputation, error) {
	opts = opts.normalized()
	if err := validateSuggestOptions(opts); err != nil {
		return nil, err
	}
	scan, err := loadSnapshotScan(ctx, db, opts.Now.AddDate(0, 0, -opts.HistoryDays), opts.Now)
	if err != nil {
		return nil, err
	}
	return computeSuggestionsFromScan(ctx, db, scan, opts)
}

// computeSuggestionsFromScan builds one fuel's forecast from an already loaded
// history, so a run covering every fuel reads the snapshots once instead of
// once per fuel.
func computeSuggestionsFromScan(ctx context.Context, db *sql.DB, scan snapshotScan, opts suggestOptions) (*suggestComputation, error) {
	opts = opts.normalized()
	if err := validateSuggestOptions(opts); err != nil {
		return nil, err
	}
	historyStart := opts.Now.AddDate(0, 0, -opts.HistoryDays)
	snapshots := scan.forFuel(opts.Fuel)
	intervals := reconstructPriceIntervals(snapshots, historyStart, opts.Now)
	if len(intervals) == 0 {
		return nil, errors.New("not enough historical open-price data for suggestions")
	}

	model := buildForecastModel(intervals, opts.Now, opts.Location)
	if err := applyLearnedCorrections(ctx, db, &model, opts.Fuel, opts.Now, opts.Location); err != nil {
		return nil, err
	}
	suggestions := mergeSuggestions(generateSuggestions(model, opts.Fuel, opts.Now, opts.Location, opts.PredictDays, opts.LimitPerDay))
	if len(suggestions) == 0 {
		return nil, errors.New("not enough historical price patterns for suggestions")
	}
	return &suggestComputation{
		Now:         opts.Now,
		Location:    opts.Location,
		Model:       model,
		Suggestions: suggestions,
		Snapshots:   snapshots,
	}, nil
}

func checkGas(ctx context.Context, db *sql.DB, opts checkOptions) ([]priceCheckRow, error) {
	opts = opts.normalized()
	if err := validateCheckOptions(opts); err != nil {
		return nil, err
	}
	scan, err := loadSnapshotScan(ctx, db, opts.Now.AddDate(0, 0, -opts.HistoryDays), opts.Now)
	if err != nil {
		return nil, err
	}
	return checkGasFromScan(ctx, db, scan, opts)
}

// checkGasFromScan is the check counterpart of computeSuggestionsFromScan.
func checkGasFromScan(ctx context.Context, db *sql.DB, scan snapshotScan, opts checkOptions) ([]priceCheckRow, error) {
	opts = opts.normalized()
	if err := validateCheckOptions(opts); err != nil {
		return nil, err
	}
	historyStart := opts.Now.AddDate(0, 0, -opts.HistoryDays)
	snapshots := scan.forFuel(opts.Fuel)
	intervals := reconstructPriceIntervals(snapshots, historyStart, opts.Now)
	if len(intervals) == 0 {
		return nil, errors.New("not enough historical open-price data for checks")
	}

	model := buildForecastModel(intervals, opts.Now, opts.Location)
	if err := applyLearnedCorrections(ctx, db, &model, opts.Fuel, opts.Now, opts.Location); err != nil {
		return nil, err
	}
	checks := generatePriceChecks(model, snapshots, opts.Fuel, opts.Now, opts.Location, opts.PredictDays, opts.Limit)
	if len(checks) == 0 {
		return nil, errors.New("not enough current open-price data for checks")
	}
	return checks, nil
}

// snapshotScan is one pass over the price history: every station still being
// fed, plus one row per stored observation carrying all three fuels. The scan
// is shared by every fuel a run covers.
type snapshotScan struct {
	// Stations is keyed by station id and carries the owning update target and
	// the distance to its centre.
	Stations map[string]suggestionStationRow
	// Rows are ordered by station, then by time — the order
	// reconstructPriceIntervals depends on to pair each observation with the
	// next one from the same station.
	Rows []snapshotScanRow
}

// snapshotScanRow is one stored observation, kept compact because a full
// history holds a lot of them: the station details live once in
// snapshotScan.Stations rather than on every row.
type snapshotScanRow struct {
	StationID  string
	RecordedAt time.Time
	IsOpen     bool
	Diesel     sql.NullFloat64
	E5         sql.NullFloat64
	E10        sql.NullFloat64
}

func (r snapshotScanRow) price(fuel string) sql.NullFloat64 {
	switch fuel {
	case "e5":
		return r.E5
	case "e10":
		return r.E10
	default:
		return r.Diesel
	}
}

// forFuel projects the shared scan onto one fuel, preserving scan order.
func (s snapshotScan) forFuel(fuel string) []suggestSnapshot {
	snapshots := make([]suggestSnapshot, 0, len(s.Rows))
	for _, row := range s.Rows {
		station := s.Stations[row.StationID]
		snapshots = append(snapshots, suggestSnapshot{
			StationID:   row.StationID,
			StationName: station.Name,
			DistanceKM:  station.DistanceKM,
			Station:     station,
			RecordedAt:  row.RecordedAt,
			IsOpen:      row.IsOpen,
			Price:       row.price(fuel),
		})
	}
	return snapshots
}

// loadSnapshotScan reads the price history of every station currently being fed.
//
// There is no radius and no city centre in the selection: the station universe
// is whatever `gasoline update` collects, bounded by each update target's own
// radius, and a station leaves scope once it stops receiving price updates
// (stationFreshness) — which is exactly what happens when a target is removed
// or its radius shrinks. Each station is attributed to the target that owns it,
// the nearest fed centre as resolved at collection time and recorded in
// price_snapshots.city_name, and the reported distance is measured to that
// centre.
func loadSnapshotScan(ctx context.Context, db *sql.DB, historyStart, now time.Time) (snapshotScan, error) {
	scan := snapshotScan{Stations: map[string]suggestionStationRow{}}
	historyStartText := historyStart.Format(time.RFC3339)
	rows, err := db.QueryContext(ctx, `
		SELECT
			s.id,
			COALESCE(s.name_override, s.name),
			COALESCE(s.brand, ''),
			COALESCE(s.street, ''),
			COALESCE(s.house_number, ''),
			COALESCE(s.post_code, 0),
			COALESCE(s.place, ''),
			s.lat,
			s.lng,
			s.first_seen_at,
			s.last_seen_at,
			ps.recorded_at,
			ps.is_open,
			ps.city_name,
			c.lat,
			c.lng,
			ps.diesel,
			ps.e5,
			ps.e10
		FROM stations s
		JOIN price_snapshots ps ON ps.station_id = s.id
		LEFT JOIN cities c ON c.normalized_name = ps.city_name
		WHERE (
				ps.recorded_at >= ?
				OR ps.recorded_at = (
					SELECT MAX(prior.recorded_at)
					FROM price_snapshots prior
					WHERE prior.station_id = s.id
						AND prior.recorded_at < ?
				)
			)
			AND EXISTS (
				SELECT 1
				FROM price_snapshots fresh
				WHERE fresh.station_id = s.id
					AND fresh.recorded_at >= ?
			)
		ORDER BY s.id ASC, ps.recorded_at ASC, ps.id ASC
	`, historyStartText, historyStartText, now.Add(-stationFreshness).Format(time.RFC3339))
	if err != nil {
		return snapshotScan{}, err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			stationID, stationName, brand, street, houseNumber, place string
			firstSeenAt, lastSeenAt, recordedAtText, cityName         string
			postCode                                                  int
			lat, lng                                                  float64
			isOpen                                                    bool
			cityLat, cityLng                                          sql.NullFloat64
			diesel, e5, e10                                           sql.NullFloat64
		)
		if err := rows.Scan(
			&stationID,
			&stationName,
			&brand,
			&street,
			&houseNumber,
			&postCode,
			&place,
			&lat,
			&lng,
			&firstSeenAt,
			&lastSeenAt,
			&recordedAtText,
			&isOpen,
			&cityName,
			&cityLat,
			&cityLng,
			&diesel,
			&e5,
			&e10,
		); err != nil {
			return snapshotScan{}, err
		}
		recordedAt, err := time.Parse(time.RFC3339, recordedAtText)
		if err != nil {
			return snapshotScan{}, fmt.Errorf("parse recorded_at %q: %w", recordedAtText, err)
		}
		if recordedAt.After(now) {
			continue
		}
		// A station's owner can move to a nearer centre; the scan is ordered
		// oldest first, so the last row seen carries the current owner. A city
		// missing from `cities` leaves the distance unknown rather than
		// discarding the station's prices.
		distanceKM := 0.0
		if cityLat.Valid && cityLng.Valid {
			distanceKM = haversineKM(cityLat.Float64, cityLng.Float64, lat, lng)
		}
		scan.Stations[stationID] = suggestionStationRow{
			ID:          stationID,
			Name:        stationName,
			Brand:       brand,
			Street:      street,
			HouseNumber: houseNumber,
			PostCode:    postCode,
			Place:       place,
			Lat:         lat,
			Lng:         lng,
			FirstSeenAt: firstSeenAt,
			LastSeenAt:  lastSeenAt,
			Address:     formatStationAddress(street, houseNumber, postCode, place),
			City:        cityName,
			DistanceKM:  distanceKM,
		}
		scan.Rows = append(scan.Rows, snapshotScanRow{
			StationID:  stationID,
			RecordedAt: recordedAt,
			IsOpen:     isOpen,
			Diesel:     diesel,
			E5:         e5,
			E10:        e10,
		})
	}
	return scan, rows.Err()
}

func reconstructPriceIntervals(snapshots []suggestSnapshot, historyStart, now time.Time) []priceInterval {
	var intervals []priceInterval
	for i, snapshot := range snapshots {
		end := now
		if i+1 < len(snapshots) && snapshots[i+1].StationID == snapshot.StationID {
			end = snapshots[i+1].RecordedAt
		}
		start := snapshot.RecordedAt
		if start.Before(historyStart) {
			start = historyStart
		}
		if end.After(now) {
			end = now
		}
		if !end.After(start) || !snapshot.IsOpen || !snapshot.Price.Valid {
			continue
		}
		intervals = append(intervals, priceInterval{
			StationID:   snapshot.StationID,
			StationName: snapshot.StationName,
			DistanceKM:  snapshot.DistanceKM,
			Station:     snapshot.Station,
			Start:       start,
			End:         end,
			Price:       snapshot.Price.Float64,
		})
	}
	return intervals
}

// buildForecastModel decomposes each station's history into a per-pricing-day
// baseline plus intraday offsets. The pricing day is anchored at the inferred
// daily jump hour, so every pricing day sits inside a single price regime even
// when the market level shifts day to day. Stations without enough complete
// pricing days keep the legacy absolute-price behavior.
func buildForecastModel(intervals []priceInterval, now time.Time, location *time.Location) forecastModel {
	model := forecastModel{
		Stations:    make(map[string]forecastStation),
		WeekdayHour: make(map[stationWeekdayHourKey][]priceSample),
		Hour:        make(map[stationHourKey][]priceSample),
		Recent:      make(map[string][]priceSample),
	}
	nowLocal := now.In(location)
	model.NowLocal = nowLocal
	model.JumpAnchorHour = inferJumpAnchorHour(intervals, location)
	buckets := collectHourBuckets(intervals, now, location)
	baselines := computeDailyBaselines(buckets, model.JumpAnchorHour, nowLocal)
	model.BaselineDrift = estimateBaselineDrift(baselines, model.JumpAnchorHour, nowLocal)
	currentDay := pricingDay(nowLocal, model.JumpAnchorHour)

	for _, interval := range intervals {
		model.Stations[interval.StationID] = forecastStation{
			Station: interval.Station,
		}
	}

	currentBuckets := make(map[string][]hourBucket)
	for _, bucket := range buckets {
		if len(baselines[bucket.StationID]) < minBaselineDays {
			model.addSample(bucket, bucket.Price)
			continue
		}
		day := pricingDay(bucket.Start, model.JumpAnchorHour)
		if day == currentDay {
			// The open pricing day has no settled baseline yet; convert these
			// buckets once the current level estimate exists (below).
			currentBuckets[bucket.StationID] = append(currentBuckets[bucket.StationID], bucket)
			continue
		}
		baseline, ok := baselines[bucket.StationID][day]
		if !ok {
			// Sparse historical day without a trustworthy baseline: mixing its
			// absolute level into the offsets would smear regimes, so drop it.
			continue
		}
		model.addSample(bucket, bucket.Price-baseline.Value)
	}

	for stationID, station := range model.Stations {
		stationBaselines := baselines[stationID]
		if len(stationBaselines) < minBaselineDays {
			continue
		}
		station.OffsetMode = true
		// Data since the last jump crossing lies entirely in the current price
		// regime and is the freshest level signal; fall back to the most
		// recent complete pricing day when there is not enough of it yet.
		estimate, ok := estimateCurrentBaseline(model, stationID, currentBuckets[stationID])
		if !ok {
			estimate = latestBaselineValue(stationBaselines)
		}
		station.BaselineForecast = estimate
		for _, bucket := range currentBuckets[stationID] {
			model.addSample(bucket, bucket.Price-estimate)
		}
		model.Stations[stationID] = station
	}
	return model
}

// collectHourBuckets splits price intervals into local-time hour slices,
// keeping the overlap duration and the recency age of each slice.
func collectHourBuckets(intervals []priceInterval, now time.Time, location *time.Location) []hourBucket {
	nowLocal := now.In(location)
	var buckets []hourBucket
	for _, interval := range intervals {
		localStart := interval.Start.In(location)
		localEnd := interval.End.In(location)
		for bucketStart := localHourStart(localStart); bucketStart.Before(localEnd); bucketStart = bucketStart.Add(time.Hour) {
			bucketEnd := bucketStart.Add(time.Hour)
			overlapStart := maxTime(localStart, bucketStart)
			overlapEnd := minTime(localEnd, bucketEnd)
			if !overlapEnd.After(overlapStart) {
				continue
			}

			minutes := overlapEnd.Sub(overlapStart).Minutes()
			midpoint := overlapStart.Add(overlapEnd.Sub(overlapStart) / 2)
			ageDays := nowLocal.Sub(midpoint).Hours() / 24
			if ageDays < 0 {
				ageDays = 0
			}
			buckets = append(buckets, hourBucket{
				StationID: interval.StationID,
				Start:     bucketStart,
				Minutes:   minutes,
				AgeDays:   ageDays,
				Price:     interval.Price,
			})
		}
	}
	return buckets
}

func (m *forecastModel) addSample(bucket hourBucket, price float64) {
	sample := priceSample{
		Price:  price,
		Weight: bucket.Minutes * math.Exp(-bucket.AgeDays/10),
		Date:   bucket.Start.Format("2006-01-02"),
	}
	// A public holiday does not price like its calendar weekday, so it is kept
	// out of the weekday bucket. The sample still feeds the hour and recent
	// sets, which carry no weekday assumption, so no price data is lost.
	if !isGermanHoliday(bucket.Start) {
		weekdayKey := stationWeekdayHourKey{
			StationID: bucket.StationID,
			Weekday:   bucket.Start.Weekday(),
			Hour:      bucket.Start.Hour(),
		}
		m.WeekdayHour[weekdayKey] = append(m.WeekdayHour[weekdayKey], sample)
	}
	hourKey := stationHourKey{
		StationID: bucket.StationID,
		Hour:      bucket.Start.Hour(),
	}
	m.Hour[hourKey] = append(m.Hour[hourKey], sample)
	m.Recent[bucket.StationID] = append(m.Recent[bucket.StationID], sample)
}

// inferJumpAnchorHour finds the local hour where upward price moves
// concentrate across all stations — the market-wide once-per-day raise (the
// regulation currently puts it at noon, but nothing here assumes that; if the
// rule changes the inferred anchor follows the data). Returns 0 (calendar-day
// pricing) when no hour dominates clearly.
func inferJumpAnchorHour(intervals []priceInterval, location *time.Location) int {
	risesByHour := make(map[int][]float64)
	for i := 1; i < len(intervals); i++ {
		previous := intervals[i-1]
		current := intervals[i]
		// Only contiguous intervals count: across a gap (closed or invalid
		// snapshots were skipped) the price change accumulated over the whole
		// gap and would be misattributed to the reopening hour.
		if previous.StationID != current.StationID || !current.Start.Equal(previous.End) {
			continue
		}
		delta := current.Price - previous.Price
		if delta <= 0 {
			continue
		}
		hour := current.Start.In(location).Hour()
		risesByHour[hour] = append(risesByHour[hour], delta)
	}

	bestHour, bestTotal, runnerUpTotal := 0, 0.0, 0.0
	for hour, deltas := range risesByHour {
		var total float64
		for _, delta := range deltas {
			total += delta
		}
		switch {
		case total > bestTotal:
			runnerUpTotal = bestTotal
			bestTotal = total
			bestHour = hour
		case total > runnerUpTotal:
			runnerUpTotal = total
		}
	}
	if bestTotal <= 0 || medianFloat(risesByHour[bestHour]) < minJumpDetectionEuro {
		return 0
	}
	if runnerUpTotal > 0 && bestTotal < jumpDominanceRatio*runnerUpTotal {
		return 0
	}
	return bestHour
}

func medianFloat(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	ordered := append([]float64(nil), values...)
	sort.Float64s(ordered)
	middle := len(ordered) / 2
	if len(ordered)%2 == 0 {
		return (ordered[middle-1] + ordered[middle]) / 2
	}
	return ordered[middle]
}

// pricingDay maps a local time onto the 24h window anchored at the daily jump
// hour, so a day-over-day baseline shift never lands inside one pricing day.
func pricingDay(local time.Time, anchorHour int) string {
	return local.Add(-time.Duration(anchorHour) * time.Hour).Format("2006-01-02")
}

// computeDailyBaselines returns the duration-weighted median price per station
// and complete pricing day. The still-open current pricing day is excluded,
// as are days with too little recorded coverage: a partial day's median is
// biased by whichever side of the daily sawtooth it happens to cover.
func computeDailyBaselines(buckets []hourBucket, anchorHour int, nowLocal time.Time) map[string]map[string]dayBaseline {
	currentDay := pricingDay(nowLocal, anchorHour)
	grouped := make(map[string]map[string][]priceSample)
	for _, bucket := range buckets {
		day := pricingDay(bucket.Start, anchorHour)
		if day >= currentDay {
			continue
		}
		days := grouped[bucket.StationID]
		if days == nil {
			days = make(map[string][]priceSample)
			grouped[bucket.StationID] = days
		}
		// Weight by duration only: within one pricing day recency is
		// irrelevant, the day is a single price regime.
		days[day] = append(days[day], priceSample{Price: bucket.Price, Weight: bucket.Minutes, Date: day})
	}

	baselines := make(map[string]map[string]dayBaseline)
	for stationID, days := range grouped {
		for day, samples := range days {
			var coverage float64
			for _, sample := range samples {
				coverage += sample.Weight
			}
			if coverage < minBaselineCoverageMinutes {
				continue
			}
			value, ok := weightedMedianPrice(samples)
			if !ok {
				continue
			}
			stationBaselines := baselines[stationID]
			if stationBaselines == nil {
				stationBaselines = make(map[string]dayBaseline)
				baselines[stationID] = stationBaselines
			}
			stationBaselines[day] = dayBaseline{Value: value, CoverageMinutes: coverage}
		}
	}
	return baselines
}

// estimateBaselineDrift measures how the market level has been moving: the
// median of adjacent-pricing-day baseline deltas across all stations inside
// the drift window, damped and capped before it is extrapolated per day of
// lead. Non-adjacent days are skipped — a delta across a coverage gap spans
// several days' moves and would be misread as one day's.
func estimateBaselineDrift(baselines map[string]map[string]dayBaseline, anchorHour int, nowLocal time.Time) float64 {
	cutoff := pricingDay(nowLocal.AddDate(0, 0, -baselineDriftWindowDays), anchorHour)
	var deltas []float64
	for _, days := range baselines {
		ordered := make([]string, 0, len(days))
		for day := range days {
			ordered = append(ordered, day)
		}
		sort.Strings(ordered)
		for i := 1; i < len(ordered); i++ {
			if ordered[i] < cutoff {
				continue
			}
			previous, err := time.Parse("2006-01-02", ordered[i-1])
			if err != nil {
				continue
			}
			current, err := time.Parse("2006-01-02", ordered[i])
			if err != nil {
				continue
			}
			if current.Sub(previous) != 24*time.Hour {
				continue
			}
			deltas = append(deltas, days[ordered[i]].Value-days[ordered[i-1]].Value)
		}
	}
	if len(deltas) < baselineDriftMinSamples {
		return 0
	}
	drift := medianFloat(deltas) * baselineDriftDamping
	if drift > baselineDriftMaxAbsPerDay {
		drift = baselineDriftMaxAbsPerDay
	}
	if drift < -baselineDriftMaxAbsPerDay {
		drift = -baselineDriftMaxAbsPerDay
	}
	return drift
}

// estimateCurrentBaseline de-shapes the open pricing day's buckets by
// subtracting the learned per-hour offsets (built from complete days only) and
// takes their duration-weighted median. This removes the time-of-day bias, so
// even one or two hours of post-jump data yield a usable level estimate.
func estimateCurrentBaseline(model forecastModel, stationID string, buckets []hourBucket) (float64, bool) {
	var (
		samples  []priceSample
		coverage float64
	)
	for _, bucket := range buckets {
		hourOffset, ok := weightedMedianPrice(model.Hour[stationHourKey{StationID: stationID, Hour: bucket.Start.Hour()}])
		if !ok {
			continue
		}
		samples = append(samples, priceSample{Price: bucket.Price - hourOffset, Weight: bucket.Minutes})
		coverage += bucket.Minutes
	}
	if coverage < minCurrentRegimeCoverageMinutes {
		return 0, false
	}
	return weightedMedianPrice(samples)
}

func latestBaselineValue(baselines map[string]dayBaseline) float64 {
	var (
		latestDay string
		value     float64
	)
	for day, baseline := range baselines {
		if day > latestDay {
			latestDay = day
			value = baseline.Value
		}
	}
	return value
}

func generateSuggestions(model forecastModel, fuel string, now time.Time, location *time.Location, predictDays, limitPerDay int) []suggestionRow {
	nowLocal := now.In(location)
	start := nextLocalHour(nowLocal)
	firstDay := localDayStart(start)
	end := firstDay.AddDate(0, 0, predictDays)

	stationIDs := make([]string, 0, len(model.Stations))
	for stationID := range model.Stations {
		stationIDs = append(stationIDs, stationID)
	}
	sort.Strings(stationIDs)

	byDate := make(map[string][]suggestionCandidate)
	for candidateStart := start; candidateStart.Before(end); candidateStart = candidateStart.Add(time.Hour) {
		for _, stationID := range stationIDs {
			station := model.Stations[stationID].Station
			score, ok := scoreForecast(model, stationID, candidateStart)
			if !ok {
				continue
			}
			candidateEnd := candidateStart.Add(time.Hour)
			date := candidateStart.Format("2006-01-02")
			stationOutput := station
			stationOutput.DistanceKM = roundTo(station.DistanceKM, 1)
			byDate[date] = append(byDate[date], suggestionCandidate{
				suggestionRow: suggestionRow{
					Date:        date,
					Weekday:     candidateStart.Weekday().String(),
					StartTime:   candidateStart.Format("15:04"),
					EndTime:     candidateEnd.Format("15:04"),
					StationID:   station.ID,
					StationName: station.Name,
					DistanceKM:  stationOutput.DistanceKM,
					Station:     stationOutput,
					Fuel:        fuel,
					// The selection bias is a display honesty correction: the
					// printed price stops being optimistic by the measured
					// amount. Ordering uses rawPrice below, so the correction
					// (and the cent rounding) cannot change which window gets
					// suggested; the persisted grid stores the raw score.
					PredictedPrice: roundTo(score.PredictedPrice+model.SuggestionBias, 2),
					Confidence:     score.Confidence,
					SampleCount:    score.SampleCount,
				},
				start:    candidateStart,
				rawPrice: score.PredictedPrice,
			})
		}
	}

	var suggestions []suggestionRow
	for dayStart := firstDay; dayStart.Before(end); dayStart = dayStart.AddDate(0, 0, 1) {
		date := dayStart.Format("2006-01-02")
		candidates := byDate[date]
		sort.SliceStable(candidates, func(i, j int) bool {
			return suggestionCandidateLess(candidates[i], candidates[j])
		})

		var selected []suggestionCandidate
		for _, candidate := range candidates {
			if duplicatesNearbyStationWindow(candidate, selected) {
				continue
			}
			selected = append(selected, candidate)
			suggestions = append(suggestions, candidate.suggestionRow)
			if len(selected) == limitPerDay {
				break
			}
		}
	}
	return suggestions
}

// forCity returns a view of the model restricted to the stations the given
// update target owns. The per-station sample maps are shared rather than
// copied: only the station set that the suggestion pass iterates over changes,
// so this is a filter over an already built model, not a rebuild.
func (m forecastModel) forCity(city string) forecastModel {
	filtered := m
	filtered.Stations = make(map[string]forecastStation, len(m.Stations))
	for stationID, station := range m.Stations {
		if station.Station.City == city {
			filtered.Stations[stationID] = station
		}
	}
	return filtered
}

func mergeSuggestions(suggestions []suggestionRow) []suggestionRow {
	type groupKey struct {
		Date           string
		StationID      string
		PredictedPrice float64
		Confidence     string
	}
	var result []suggestionRow
	seen := make(map[groupKey]int)
	for _, s := range suggestions {
		key := groupKey{s.Date, s.StationID, s.PredictedPrice, s.Confidence}
		if idx, ok := seen[key]; ok {
			if s.EndTime > result[idx].EndTime {
				result[idx].EndTime = s.EndTime
			}
			if s.SampleCount > result[idx].SampleCount {
				result[idx].SampleCount = s.SampleCount
			}
		} else {
			seen[key] = len(result)
			result = append(result, s)
		}
	}
	return result
}

func generatePriceChecks(model forecastModel, snapshots []suggestSnapshot, fuel string, now time.Time, location *time.Location, predictDays, limit int) []priceCheckRow {
	nowLocal := now.In(location)
	latestByStation := latestSnapshotsByStation(snapshots)
	stationIDs := make([]string, 0, len(latestByStation))
	for stationID := range latestByStation {
		stationIDs = append(stationIDs, stationID)
	}
	sort.Strings(stationIDs)

	var checks []priceCheckRow
	for _, stationID := range stationIDs {
		snapshot := latestByStation[stationID]
		if !snapshot.IsOpen || !snapshot.Price.Valid {
			continue
		}

		currentScore, ok := scoreForecast(model, stationID, nowLocal)
		if !ok {
			continue
		}
		// In offset mode the percentile compares the price's position within
		// the daily sawtooth, not its absolute level — a market-wide baseline
		// jump must not make every station read "high".
		comparePrice := snapshot.Price.Float64
		if station := model.Stations[stationID]; station.OffsetMode {
			comparePrice -= station.BaselineForecast
		}
		percentile, ok := weightedPricePercentile(model.Recent[stationID], comparePrice)
		if !ok {
			continue
		}

		station := snapshot.Station
		if modelStation, ok := model.Stations[stationID]; ok {
			station = modelStation.Station
		}
		station.DistanceKM = roundTo(station.DistanceKM, 1)

		row := priceCheckRow{
			RecordedAt:            snapshot.RecordedAt.Format(time.RFC3339),
			StationID:             station.ID,
			StationName:           station.Name,
			DistanceKM:            station.DistanceKM,
			Station:               station,
			Fuel:                  fuel,
			CurrentPrice:          roundTo(snapshot.Price.Float64, 2),
			PredictedCurrentPrice: roundTo(currentScore.PredictedPrice, 2),
			rawCurrentPrice:       snapshot.Price.Float64,
			rawPredictedPrice:     currentScore.PredictedPrice,
			HistoryPercentile:     roundTo(percentile, 1),
			Confidence:            currentScore.Confidence,
			SampleCount:           currentScore.SampleCount,
		}
		row.Verdict = priceCheckVerdict(snapshot.Price.Float64, currentScore.PredictedPrice, percentile, defaultCheckDelta)

		if future, ok := bestFutureForecast(model, stationID, nowLocal, predictDays); ok {
			row.BestFutureDate = future.Start.Format("2006-01-02")
			row.BestFutureWeekday = future.Start.Weekday().String()
			row.BestFutureStartTime = future.Start.Format("15:04")
			row.BestFutureEndTime = future.End.Format("15:04")
			row.BestFuturePrice = roundTo(future.Score.PredictedPrice, 2)
			drop := snapshot.Price.Float64 - future.Score.PredictedPrice
			if drop > 0 {
				row.ExpectedDrop = roundTo(drop, 2)
			}
			if drop >= defaultCheckDelta {
				row.ExpectedLower = true
				row.Confidence = lowerConfidence(row.Confidence, future.Score.Confidence)
			}
		}
		row.Recommendation = priceCheckRecommendation(row.Verdict, row.ExpectedLower)
		checks = append(checks, row)
	}

	sort.SliceStable(checks, func(i, j int) bool {
		return priceCheckLess(checks[i], checks[j])
	})
	if limit > 0 && len(checks) > limit {
		checks = checks[:limit]
	}
	return checks
}

func latestSnapshotsByStation(snapshots []suggestSnapshot) map[string]suggestSnapshot {
	latest := make(map[string]suggestSnapshot)
	for _, snapshot := range snapshots {
		existing, ok := latest[snapshot.StationID]
		if !ok || snapshot.RecordedAt.After(existing.RecordedAt) {
			latest[snapshot.StationID] = snapshot
		}
	}
	return latest
}

func bestFutureForecast(model forecastModel, stationID string, nowLocal time.Time, predictDays int) (futureForecast, bool) {
	start := localHourStart(nowLocal).Add(time.Hour)
	firstDay := localDayStart(start)
	end := firstDay.AddDate(0, 0, predictDays)

	var (
		best futureForecast
		ok   bool
	)
	for candidateStart := start; candidateStart.Before(end); candidateStart = candidateStart.Add(time.Hour) {
		score, scoreOK := scoreForecast(model, stationID, candidateStart)
		if !scoreOK {
			continue
		}
		candidate := futureForecast{
			Start: candidateStart,
			End:   candidateStart.Add(time.Hour),
			Score: score,
		}
		if !ok || futureForecastLess(candidate, best) {
			best = candidate
			ok = true
		}
	}
	return best, ok
}

func futureForecastLess(a, b futureForecast) bool {
	if a.Score.PredictedPrice != b.Score.PredictedPrice {
		return a.Score.PredictedPrice < b.Score.PredictedPrice
	}
	if confidenceRank(a.Score.Confidence) != confidenceRank(b.Score.Confidence) {
		return confidenceRank(a.Score.Confidence) > confidenceRank(b.Score.Confidence)
	}
	return a.Start.Before(b.Start)
}

type suggestionCandidate struct {
	suggestionRow
	start time.Time
	// rawPrice is the model score before display rounding and the suggestion
	// selection-bias correction. Candidate ordering uses it so that neither
	// cent rounding nor the display correction can flip which window wins:
	// two raw prices that round to the same cent would otherwise fall through
	// to the tie-breakers, and adding a non-cent-aligned constant before
	// rounding can split or create exactly such ties.
	rawPrice float64
}

// scoreForecast predicts the price for one station at one local target time.
// The full time is taken rather than weekday and hour because the weekday
// blend is only valid on ordinary days: on a public holiday the weekday bucket
// describes a different kind of day, so the target's date decides whether that
// blend applies at all.
func scoreForecast(model forecastModel, stationID string, target time.Time) (forecastScore, bool) {
	weekday, hour := target.Weekday(), target.Hour()
	sameWeekday := model.WeekdayHour[stationWeekdayHourKey{StationID: stationID, Weekday: weekday, Hour: hour}]
	if isGermanHoliday(target) {
		// Holidays are excluded from the weekday buckets at ingest, so this
		// bucket holds ordinary weekdays only — the wrong reference for a
		// holiday. Fall through to the hour/recent blend below, the same path
		// a station with too little weekday history already takes.
		sameWeekday = nil
	}
	sameHour := model.Hour[stationHourKey{StationID: stationID, Hour: hour}]
	recent := model.Recent[stationID]
	sameHourScore, ok := weightedMedianPrice(sameHour)
	if !ok {
		return forecastScore{}, false
	}
	recentScore, ok := weightedMedianPrice(recent)
	if !ok {
		return forecastScore{}, false
	}

	var (
		predicted   float64
		confidence  string
		sampleCount int
	)
	station := model.Stations[stationID]
	if len(sameWeekday) >= 3 {
		sameWeekdayScore, ok := weightedMedianPrice(sameWeekday)
		if !ok {
			return forecastScore{}, false
		}
		if station.OffsetMode {
			// In offset mode the recent bucket's median is ~0 by construction
			// (offsets center on the pricing-day baseline), so blending it in
			// only damps the intraday shape — measured as bias growing with
			// the hour offset: peaks under-predicted, valleys over-predicted.
			// The level job it did in absolute mode belongs to the baseline.
			predicted = (0.60*sameWeekdayScore + 0.30*sameHourScore) / 0.90
		} else {
			predicted = 0.60*sameWeekdayScore + 0.30*sameHourScore + 0.10*recentScore
		}
		sampleCount = len(sameWeekday)
		switch {
		case len(sameWeekday) >= 8 && distinctSampleDays(sameWeekday) >= 5:
			confidence = "high"
		case len(sameHour) >= 5:
			confidence = "medium"
		default:
			confidence = "low"
		}
	} else {
		if station.OffsetMode {
			predicted = sameHourScore
		} else {
			predicted = 0.75*sameHourScore + 0.25*recentScore
		}
		sampleCount = len(sameHour)
		confidence = "low"
	}

	if station.OffsetMode {
		predicted += station.BaselineForecast
		// The baseline itself is held flat — the daily surprise is unknowable —
		// but its recent damped drift is not: without it every prediction that
		// crosses a pricing-day boundary inherits a stale level, giving the
		// measured lead-growing bias in trending markets.
		if model.BaselineDrift != 0 && !model.NowLocal.IsZero() {
			predicted += model.BaselineDrift * float64(pricingDaysAhead(model.NowLocal, target, model.JumpAnchorHour))
		}
	}
	learnedCorrection := station.BiasCorrection
	predicted += station.BiasCorrection

	if !model.NowLocal.IsZero() {
		lead := leadBucketFor(target.Sub(model.NowLocal).Minutes())
		// Learned cells stop at 24h — beyond that, day-ahead level surprises
		// would contaminate them — but the intraday shape error they capture
		// does not fade with distance, so long leads reuse the 6-24h cell.
		cell := lead
		if cell == leadBucketBeyond24h {
			cell = leadBucket6to24h
		}
		if correction, ok := model.HourLeadBias[hourLeadKey{Hour: target.Hour(), Lead: cell, Weekend: isWeekendLike(target)}]; ok {
			predicted += correction
			learnedCorrection += correction
		}
		if label, ok := model.ConfidenceByLead[stationLeadKey{StationID: stationID, Lead: cell}]; ok {
			// The calibration was measured at <=24h leads; a >24h target adds
			// level risk the cell never saw, so it cannot claim "high" there.
			if lead == leadBucketBeyond24h && label == "high" {
				label = "medium"
			}
			confidence = label
		}
	}

	return forecastScore{
		PredictedPrice:    predicted,
		Confidence:        confidence,
		SampleCount:       sampleCount,
		LearnedCorrection: learnedCorrection,
	}, true
}

// pricingDaysAhead counts how many pricing-day boundaries (anchor-hour
// crossings) lie between now and the target. Same pricing day means zero.
func pricingDaysAhead(nowLocal, target time.Time, anchorHour int) int {
	nowDay, err := time.Parse("2006-01-02", pricingDay(nowLocal, anchorHour))
	if err != nil {
		return 0
	}
	targetDay, err := time.Parse("2006-01-02", pricingDay(target, anchorHour))
	if err != nil {
		return 0
	}
	return int(targetDay.Sub(nowDay).Hours() / 24)
}

func weightedMedianPrice(samples []priceSample) (float64, bool) {
	if len(samples) == 0 {
		return 0, false
	}
	ordered := append([]priceSample(nil), samples...)
	sort.Slice(ordered, func(i, j int) bool {
		return ordered[i].Price < ordered[j].Price
	})

	var totalWeight float64
	for _, sample := range ordered {
		totalWeight += sample.Weight
	}
	if totalWeight <= 0 {
		return 0, false
	}

	var cumulative float64
	for _, sample := range ordered {
		cumulative += sample.Weight
		if cumulative >= totalWeight/2 {
			return sample.Price, true
		}
	}
	return ordered[len(ordered)-1].Price, true
}

func weightedPricePercentile(samples []priceSample, price float64) (float64, bool) {
	var (
		totalWeight float64
		lowerWeight float64
		equalWeight float64
	)
	for _, sample := range samples {
		if sample.Weight <= 0 {
			continue
		}
		totalWeight += sample.Weight
		switch {
		case sample.Price < price:
			lowerWeight += sample.Weight
		case sample.Price == price:
			equalWeight += sample.Weight
		}
	}
	if totalWeight <= 0 {
		return 0, false
	}
	return 100 * (lowerWeight + equalWeight/2) / totalWeight, true
}

func priceCheckVerdict(currentPrice, predictedPrice, historyPercentile, delta float64) string {
	switch {
	case historyPercentile <= 30 || currentPrice <= predictedPrice-delta:
		return "low"
	case historyPercentile >= 70 || currentPrice >= predictedPrice+delta:
		return "high"
	default:
		return "typical"
	}
}

func priceCheckRecommendation(verdict string, expectedLower bool) string {
	if expectedLower {
		return "wait"
	}
	if verdict == "low" {
		return "buy"
	}
	return "hold"
}

func lowerConfidence(a, b string) string {
	if confidenceRank(a) <= confidenceRank(b) {
		return a
	}
	return b
}

func printSuggestionsText(suggestions []suggestionRow) {
	currentDate := ""
	for _, suggestion := range suggestions {
		if suggestion.Date != currentDate {
			if currentDate != "" {
				fmt.Fprintln(stdout)
			}
			fmt.Fprintf(stdout, "%s %s\n", suggestion.Weekday, suggestion.Date)
			currentDate = suggestion.Date
		}
		stationLabel := suggestion.StationName
		if suggestion.Station.Brand != "" {
			stationLabel = fmt.Sprintf("%s [%s]", stationLabel, suggestion.Station.Brand)
		}
		if suggestion.Station.Address != "" {
			stationLabel = fmt.Sprintf("%s | %s", stationLabel, suggestion.Station.Address)
		}
		fmt.Fprintf(stdout, "  %s-%s  %s  %.1f km  %s  predicted %.2f  confidence %s  samples=%d\n",
			suggestion.StartTime,
			suggestion.EndTime,
			stationLabel,
			suggestion.DistanceKM,
			suggestion.Fuel,
			suggestion.PredictedPrice,
			suggestion.Confidence,
			suggestion.SampleCount,
		)
	}
}

func printPriceChecksText(checks []priceCheckRow) {
	for _, check := range checks {
		stationLabel := formatStationLabel(check.StationName, check.Station)
		futureLabel := "no lower forecast"
		if check.ExpectedLower {
			futureLabel = fmt.Sprintf("wait for %s %s-%s predicted %.2f drop %.2f",
				check.BestFutureDate,
				check.BestFutureStartTime,
				check.BestFutureEndTime,
				check.BestFuturePrice,
				check.ExpectedDrop,
			)
		} else if check.BestFutureDate != "" {
			futureLabel = fmt.Sprintf("best future %s %s-%s predicted %.2f",
				check.BestFutureDate,
				check.BestFutureStartTime,
				check.BestFutureEndTime,
				check.BestFuturePrice,
			)
		}
		fmt.Fprintf(stdout, "%s  %.1f km  %s current %.2f  verdict %s  recommendation %s  confidence %s  percentile %.1f%%  forecast-now %.2f  at=%s  %s\n",
			stationLabel,
			check.DistanceKM,
			check.Fuel,
			check.CurrentPrice,
			check.Verdict,
			check.Recommendation,
			check.Confidence,
			check.HistoryPercentile,
			check.PredictedCurrentPrice,
			check.RecordedAt,
			futureLabel,
		)
	}
}

func formatStationLabel(stationName string, station suggestionStationRow) string {
	stationLabel := stationName
	if station.Brand != "" {
		stationLabel = fmt.Sprintf("%s [%s]", stationLabel, station.Brand)
	}
	if station.Address != "" {
		stationLabel = fmt.Sprintf("%s | %s", stationLabel, station.Address)
	}
	return stationLabel
}

func printHistoryText(stationFilter, stationID, stationName, recordedAt, cityName string, isOpen bool, prices string) {
	if stationFilter == "" {
		fmt.Fprintf(stdout, "%s | %s | %s | city=%s | open=%t | %s\n",
			recordedAt, stationID, stationName, cityName, isOpen, prices)
		return
	}
	fmt.Fprintf(stdout, "%s | city=%s | open=%t | %s\n", recordedAt, cityName, isOpen, prices)
}

func addOutputFlags(fs *flag.FlagSet) (*string, *string) {
	return fs.String("output", "", "Output format: txt or json"), fs.String("o", "", "Output format: txt or json")
}

func resolveOutputMode(longValue, shortValue string) (outputMode, error) {
	longValue = strings.TrimSpace(longValue)
	shortValue = strings.TrimSpace(shortValue)
	if longValue != "" && shortValue != "" && longValue != shortValue {
		return "", errors.New("--output and -o must match when both are provided")
	}
	value := blankOr(longValue, shortValue)
	if value == "" {
		return outputText, nil
	}
	switch outputMode(value) {
	case outputText, outputJSON:
		return outputMode(value), nil
	default:
		return "", errors.New("--output must be one of: txt, json")
	}
}

func writeJSON(v any) error {
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func nullFloatPtr(v sql.NullFloat64) *float64 {
	if !v.Valid {
		return nil
	}
	value := v.Float64
	return &value
}

func loadConfig(userAgent string) (config, error) {
	apiKey := strings.TrimSpace(os.Getenv(envAPIKeyName))
	if apiKey == "" {
		values, err := loadDotEnv(".env")
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return config{}, err
		}
		apiKey = strings.TrimSpace(values[envAPIKeyName])
	}
	if apiKey == "" {
		return config{}, fmt.Errorf("%s is not set in environment or .env", envAPIKeyName)
	}
	return config{
		APIKey:    apiKey,
		UserAgent: strings.TrimSpace(userAgent),
	}, nil
}

func loadDotEnv(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	values := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		values[strings.TrimSpace(key)] = strings.Trim(strings.TrimSpace(value), `"'`)
	}
	return values, nil
}

func resolveDBPath(fs *flag.FlagSet, flagValue string) string {
	if flagWasSet(fs, "db") {
		return flagValue
	}
	if envValue := strings.TrimSpace(os.Getenv(envDBPathName)); envValue != "" {
		return envValue
	}
	return flagValue
}

func flagWasSet(fs *flag.FlagSet, name string) bool {
	var found bool
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

func initSchema(ctx context.Context, db *sql.DB, d dialect) error {
	if err := ensureSchema(ctx, db, d); err != nil {
		return err
	}
	_, err := migrateSchema(ctx, db, d)
	return err
}

func ensureSchema(ctx context.Context, db *sql.DB, d dialect) error {
	for _, stmt := range schemaStatements(d) {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

func migrateSchema(ctx context.Context, db *sql.DB, d dialect) (migrateResult, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return migrateResult{}, err
	}
	defer tx.Rollback()

	var result migrateResult
	if err := migrateCitiesNormalizedName(ctx, tx, d, &result); err != nil {
		return migrateResult{}, err
	}
	if err := migrateCitiesDisplayName(ctx, tx, &result); err != nil {
		return migrateResult{}, err
	}
	if err := migrateCitiesDeduplicate(ctx, tx, &result); err != nil {
		return migrateResult{}, err
	}
	if err := migratePriceSnapshotsDropDistKM(ctx, tx, d, &result); err != nil {
		return migrateResult{}, err
	}
	if err := migrateStationsNameOverride(ctx, tx, d, &result); err != nil {
		return migrateResult{}, err
	}
	if err := migrateStationsAliasOf(ctx, tx, d, &result); err != nil {
		return migrateResult{}, err
	}
	if err := migratePredictionsAppliedCorrection(ctx, tx, d, &result); err != nil {
		return migrateResult{}, err
	}
	if err := migrateUsersNotifyFuel(ctx, tx, d, &result); err != nil {
		return migrateResult{}, err
	}
	if err := migrateUsersNotifySuggestEnabled(ctx, tx, d, &result); err != nil {
		return migrateResult{}, err
	}
	if err := migratePredictionsAccuracyIndex(ctx, tx, d, &result); err != nil {
		return migrateResult{}, err
	}
	if err := migrateSeedDefaultSettings(ctx, tx, d, &result); err != nil {
		return migrateResult{}, err
	}
	if err := migrateDropObsoleteSettings(ctx, tx, d, &result); err != nil {
		return migrateResult{}, err
	}

	if err := tx.Commit(); err != nil {
		return migrateResult{}, err
	}
	return result, nil
}

func migrateStationsNameOverride(ctx context.Context, tx *sql.Tx, d dialect, result *migrateResult) error {
	hasColumn, err := tableHasColumn(ctx, tx, d, "stations", "name_override")
	if err != nil {
		return err
	}
	if hasColumn {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE stations ADD COLUMN name_override TEXT`); err != nil {
		return err
	}
	result.Applied = append(result.Applied, "stations.name_override")
	return nil
}

// migrateStationsAliasOf adds the alias_of column that marks a station as a
// duplicate identity of another (see merge-stations). New databases already
// get it from schemaStatements.
func migrateStationsAliasOf(ctx context.Context, tx *sql.Tx, d dialect, result *migrateResult) error {
	hasColumn, err := tableHasColumn(ctx, tx, d, "stations", "alias_of")
	if err != nil {
		return err
	}
	if hasColumn {
		return nil
	}
	colType := "TEXT"
	if d == dialectMySQL {
		colType = "VARCHAR(64)"
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE stations ADD COLUMN alias_of %s`, colType)); err != nil {
		return err
	}
	result.Applied = append(result.Applied, "stations.alias_of")
	return nil
}

// migratePredictionsAppliedCorrection adds the column recording the learned
// correction a prediction carried when it was stored. It stays NULL on rows
// persisted before the column existed, which doubles as a version gate: the
// learning loops only train on rows whose raw model error they can
// reconstruct, so predictions of older model versions never contaminate the
// current corrections.
func migratePredictionsAppliedCorrection(ctx context.Context, tx *sql.Tx, d dialect, result *migrateResult) error {
	hasColumn, err := tableHasColumn(ctx, tx, d, "price_predictions", "applied_correction")
	if err != nil {
		return err
	}
	if hasColumn {
		return nil
	}
	colType := "REAL"
	if d == dialectMySQL {
		colType = "DOUBLE"
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE price_predictions ADD COLUMN applied_correction %s`, colType)); err != nil {
		return err
	}
	result.Applied = append(result.Applied, "price_predictions.applied_correction")
	return nil
}

// migratePredictionsAccuracyIndex backfills the covering index the admin
// accuracy page's aggregates rely on (see schemaStatements). New databases get
// it from the schema; existing ones need the ALTER because MySQL declares the
// index inline in a CREATE TABLE IF NOT EXISTS that no-ops on an existing
// table. SQLite normally arrives here already satisfied — its CREATE INDEX IF
// NOT EXISTS runs in ensureSchema — so the check keeps this a no-op there and
// the CREATE form below only matters when migrate runs on its own.
//
// Every column the index names has existed since price_predictions was
// created, so there is no ordering constraint against the column migrations
// above.
//
// On a large price_predictions this builds for a while. InnoDB adds a
// secondary index in place without blocking reads or writes, so a persist run
// racing the migration is safe; it just contends for IO.
func migratePredictionsAccuracyIndex(ctx context.Context, tx *sql.Tx, d dialect, result *migrateResult) error {
	hasIndex, err := tableHasIndex(ctx, tx, d, "price_predictions", "idx_price_predictions_accuracy")
	if err != nil {
		return err
	}
	if hasIndex {
		return nil
	}
	stmt := `CREATE INDEX idx_price_predictions_accuracy
		ON price_predictions(fuel, target_start, station_id, run_id,
			error, actual_price, predicted_price, confidence, lead_minutes)`
	if d == dialectMySQL {
		stmt = `ALTER TABLE price_predictions
			ADD INDEX idx_price_predictions_accuracy (
				fuel, target_start, station_id, run_id,
				error, actual_price, predicted_price, confidence, lead_minutes
			)`
	}
	if _, err := tx.ExecContext(ctx, stmt); err != nil {
		return err
	}
	result.Applied = append(result.Applied, "price_predictions.idx_price_predictions_accuracy")
	return nil
}

// migrateUsersNotifyFuel adds the per-user notify_fuel column to pre-existing
// installs. New databases already get it from schemaStatements; the ALTER only
// fires where the users table predates the column.
func migrateUsersNotifyFuel(ctx context.Context, tx *sql.Tx, d dialect, result *migrateResult) error {
	hasColumn, err := tableHasColumn(ctx, tx, d, "users", "notify_fuel")
	if err != nil {
		return err
	}
	if hasColumn {
		return nil
	}
	colType := "TEXT"
	if d == dialectMySQL {
		colType = "VARCHAR(16)"
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(
		`ALTER TABLE users ADD COLUMN notify_fuel %s NOT NULL DEFAULT 'diesel'`, colType,
	)); err != nil {
		return err
	}
	result.Applied = append(result.Applied, "users.notify_fuel")
	return nil
}

// migrateUsersNotifySuggestEnabled adds the per-user notify_suggest_enabled
// column to pre-existing installs. It defaults to 1 so users who received
// suggestions before the per-user toggle existed keep receiving them; new
// databases already get the column from schemaStatements.
func migrateUsersNotifySuggestEnabled(ctx context.Context, tx *sql.Tx, d dialect, result *migrateResult) error {
	hasColumn, err := tableHasColumn(ctx, tx, d, "users", "notify_suggest_enabled")
	if err != nil {
		return err
	}
	if hasColumn {
		return nil
	}
	colType := "INTEGER"
	if d == dialectMySQL {
		colType = "TINYINT"
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(
		`ALTER TABLE users ADD COLUMN notify_suggest_enabled %s NOT NULL DEFAULT 1`, colType,
	)); err != nil {
		return err
	}
	result.Applied = append(result.Applied, "users.notify_suggest_enabled")
	return nil
}

func migrateCitiesNormalizedName(ctx context.Context, tx *sql.Tx, d dialect, result *migrateResult) error {
	hasColumn, err := tableHasColumn(ctx, tx, d, "cities", "normalized_name")
	if err != nil {
		return err
	}
	if !hasColumn {
		// MySQL forbids DEFAULT on TEXT columns, so use a bounded VARCHAR there.
		columnDef := `normalized_name TEXT NOT NULL DEFAULT ''`
		if d == dialectMySQL {
			columnDef = `normalized_name VARCHAR(255) NOT NULL DEFAULT ''`
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`
			ALTER TABLE cities
			ADD COLUMN %s
		`, columnDef)); err != nil {
			return err
		}
		result.Applied = append(result.Applied, "cities.normalized_name")
	}

	_, err = tx.ExecContext(ctx, `
		UPDATE cities
		SET normalized_name = CASE
			WHEN TRIM(normalized_name) <> '' THEN normalized_name
			WHEN TRIM(display_name) <> '' THEN display_name
			ELSE name
		END
	`)
	return err
}

func migrateCitiesDeduplicate(ctx context.Context, tx *sql.Tx, result *migrateResult) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT name, normalized_name, display_name, lat, lng
		FROM cities
		WHERE TRIM(normalized_name) <> ''
		ORDER BY normalized_name ASC,
			CASE WHEN name = normalized_name THEN 0 ELSE 1 END ASC,
			created_at ASC,
			name ASC
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	type cityCacheRow struct {
		QueryName   string
		Name        string
		DisplayName string
		Lat         float64
		Lng         float64
	}

	var (
		lastNormalized string
		keeper         cityCacheRow
		haveKeeper     bool
		deduped        bool
	)

	for rows.Next() {
		var row cityCacheRow
		if err := rows.Scan(&row.QueryName, &row.Name, &row.DisplayName, &row.Lat, &row.Lng); err != nil {
			return err
		}

		if !haveKeeper || row.Name != lastNormalized {
			keeper = row
			lastNormalized = row.Name
			haveKeeper = true
			continue
		}

		if shouldPromoteCityDisplay(keeper.Name, keeper.DisplayName, row.DisplayName) {
			if _, err := tx.ExecContext(ctx, `
				UPDATE cities
				SET display_name = ?, lat = ?, lng = ?
				WHERE name = ?
			`, row.DisplayName, row.Lat, row.Lng, keeper.QueryName); err != nil {
				return err
			}
			keeper.DisplayName = row.DisplayName
			keeper.Lat = row.Lat
			keeper.Lng = row.Lng
		}

		if _, err := tx.ExecContext(ctx, `DELETE FROM cities WHERE name = ?`, row.QueryName); err != nil {
			return err
		}
		deduped = true
	}
	if err := rows.Err(); err != nil {
		return err
	}

	if deduped {
		result.Applied = append(result.Applied, "cities.deduplicate_normalized_name")
	}
	return nil
}

func migrateCitiesDisplayName(ctx context.Context, tx *sql.Tx, result *migrateResult) error {
	res, err := tx.ExecContext(ctx, `
		UPDATE cities
		SET display_name = normalized_name
		WHERE TRIM(normalized_name) <> ''
			AND display_name <> normalized_name
	`)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected > 0 {
		result.Applied = append(result.Applied, "cities.display_name")
	}
	return nil
}

func migratePriceSnapshotsDropDistKM(ctx context.Context, tx *sql.Tx, d dialect, result *migrateResult) error {
	hasColumn, err := tableHasColumn(ctx, tx, d, "price_snapshots", "dist_km")
	if err != nil {
		return err
	}
	if !hasColumn {
		return nil
	}

	if d == dialectMySQL {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE price_snapshots DROP COLUMN dist_km`); err != nil {
			return err
		}
		result.Applied = append(result.Applied, "price_snapshots.dist_km")
		return nil
	}

	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE price_snapshots_new (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			station_id TEXT NOT NULL,
			city_name TEXT NOT NULL,
			recorded_at TEXT NOT NULL,
			search_radius_km REAL NOT NULL DEFAULT 5,
			is_open INTEGER NOT NULL,
			e5 REAL,
			e10 REAL,
			diesel REAL,
			FOREIGN KEY (station_id) REFERENCES stations(id)
		)
	`); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO price_snapshots_new (
			id, station_id, city_name, recorded_at, search_radius_km, is_open, e5, e10, diesel
		)
		SELECT id, station_id, city_name, recorded_at, search_radius_km, is_open, e5, e10, diesel
		FROM price_snapshots
	`); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `DROP TABLE price_snapshots`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE price_snapshots_new RENAME TO price_snapshots`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		CREATE INDEX IF NOT EXISTS idx_price_snapshots_station_recorded
			ON price_snapshots(station_id, recorded_at DESC)
	`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		CREATE INDEX IF NOT EXISTS idx_price_snapshots_city_recorded
			ON price_snapshots(city_name, recorded_at DESC)
	`); err != nil {
		return err
	}

	result.Applied = append(result.Applied, "price_snapshots.dist_km")
	return nil
}

func tableHasColumn(ctx context.Context, q queryer, d dialect, tableName, columnName string) (bool, error) {
	if d == dialectMySQL {
		var count int
		row := q.QueryRowContext(ctx, `
			SELECT COUNT(*)
			FROM information_schema.columns
			WHERE table_schema = DATABASE() AND table_name = ? AND column_name = ?
		`, tableName, columnName)
		if err := row.Scan(&count); err != nil {
			return false, err
		}
		return count > 0, nil
	}

	rows, err := q.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s)", tableName))
	if err != nil {
		return false, err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			cid        int
			name       string
			columnType string
			notNull    int
			defaultVal sql.NullString
			pk         int
		)
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultVal, &pk); err != nil {
			return false, err
		}
		if name == columnName {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return false, nil
}

// tableHasIndex reports whether tableName carries an index of the given name.
// Index names are per-table in MySQL and global in SQLite; matching on both
// table and name is correct under either rule.
func tableHasIndex(ctx context.Context, q queryer, d dialect, tableName, indexName string) (bool, error) {
	if d == dialectMySQL {
		var count int
		row := q.QueryRowContext(ctx, `
			SELECT COUNT(*)
			FROM information_schema.statistics
			WHERE table_schema = DATABASE() AND table_name = ? AND index_name = ?
		`, tableName, indexName)
		if err := row.Scan(&count); err != nil {
			return false, err
		}
		return count > 0, nil
	}

	rows, err := q.QueryContext(ctx, fmt.Sprintf("PRAGMA index_list(%s)", tableName))
	if err != nil {
		return false, err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			seq     int
			name    string
			unique  int
			origin  string
			partial int
		)
		if err := rows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			return false, err
		}
		if name == indexName {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return false, nil
}

func downloadGeoNamesCities(ctx context.Context, sourceURL, countryCode, userAgent string) ([]cachedCity, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", blankOr(userAgent, defaultUserAgent))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("geonames download failed: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return parseGeoNamesZip(body, countryCode)
}

func parseGeoNamesZip(body []byte, countryCode string) ([]cachedCity, error) {
	readerAt := bytes.NewReader(body)
	archive, err := zip.NewReader(readerAt, int64(len(body)))
	if err != nil {
		return nil, err
	}

	targetName := countryCode + ".txt"
	for _, file := range archive.File {
		if filepath.Base(file.Name) != targetName {
			continue
		}

		rc, err := file.Open()
		if err != nil {
			return nil, err
		}

		cities, parseErr := parseGeoNamesCities(rc, countryCode)
		closeErr := rc.Close()
		if parseErr != nil {
			return nil, parseErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		return cities, nil
	}

	return nil, fmt.Errorf("zip archive does not contain %s", targetName)
}

func parseGeoNamesCities(r io.Reader, countryCode string) ([]cachedCity, error) {
	reader := csv.NewReader(r)
	reader.Comma = '\t'
	reader.FieldsPerRecord = -1
	reader.LazyQuotes = true
	reader.ReuseRecord = true

	var cities []cachedCity
	for {
		record, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return cities, nil
		}
		if err != nil {
			return nil, err
		}
		if len(record) < 9 || record[6] != "P" || !geoNamesFeatureCodePattern.MatchString(record[7]) || record[8] != countryCode {
			continue
		}

		lat, err := strconv.ParseFloat(record[4], 64)
		if err != nil {
			return nil, fmt.Errorf("invalid latitude for %q: %w", record[1], err)
		}
		lng, err := strconv.ParseFloat(record[5], 64)
		if err != nil {
			return nil, fmt.Errorf("invalid longitude for %q: %w", record[1], err)
		}

		name := strings.TrimSpace(record[1])
		if name == "" {
			continue
		}

		cities = append(cities, cachedCity{
			Name:        name,
			DisplayName: name,
			Lat:         lat,
			Lng:         lng,
		})
	}
}

func importCities(ctx context.Context, db *sql.DB, d dialect, cities []cachedCity) (int, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, citiesUpsertSQL(d))
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	createdAt := time.Now().UTC().Format(time.RFC3339)
	for _, city := range cities {
		if _, err := stmt.ExecContext(ctx, city.Name, city.Name, city.DisplayName, city.Lat, city.Lng, createdAt); err != nil {
			return 0, err
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(cities), nil
}

func getOrCreateCity(ctx context.Context, db *sql.DB, cityName, userAgent string) (cachedCity, bool, error) {
	var city cachedCity
	row := db.QueryRowContext(ctx, `
		SELECT name, normalized_name, display_name, lat, lng
		FROM cities
		WHERE name = ?
	`, cityName)
	if err := row.Scan(&city.QueryName, &city.Name, &city.DisplayName, &city.Lat, &city.Lng); err == nil {
		if needsNormalizedNameRefresh(city) {
			geo, err := geocodeCity(ctx, cityName, userAgent)
			if err != nil {
				return cachedCity{}, false, err
			}
			geocoded := cachedCity{
				QueryName:   city.QueryName,
				Name:        geo.Name,
				DisplayName: geo.DisplayName,
				Lat:         geo.Lat,
				Lng:         geo.Lng,
			}
			canonical, found, err := findCanonicalCity(ctx, db, geocoded.Name, geocoded.DisplayName)
			if err != nil {
				return cachedCity{}, false, err
			}
			if found && canonical.QueryName != city.QueryName {
				if err := updateCachedCity(ctx, db, canonical.QueryName, geocoded); err != nil {
					return cachedCity{}, false, err
				}
				if _, err := db.ExecContext(ctx, `DELETE FROM cities WHERE name = ?`, city.QueryName); err != nil {
					return cachedCity{}, false, err
				}
				geocoded.QueryName = canonical.QueryName
				return geocoded, false, nil
			}
			_, err = db.ExecContext(ctx, `
				UPDATE cities
				SET normalized_name = ?, display_name = ?, lat = ?, lng = ?
				WHERE name = ?
			`, geocoded.Name, geocoded.DisplayName, geocoded.Lat, geocoded.Lng, city.QueryName)
			if err != nil {
				return cachedCity{}, false, err
			}
			return geocoded, false, nil
		}
		return city, true, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return cachedCity{}, false, err
	}

	geo, err := geocodeCity(ctx, cityName, userAgent)
	if err != nil {
		return cachedCity{}, false, err
	}
	city = cachedCity{
		QueryName:   cityName,
		Name:        geo.Name,
		DisplayName: geo.DisplayName,
		Lat:         geo.Lat,
		Lng:         geo.Lng,
	}
	canonical, found, err := findCanonicalCity(ctx, db, city.Name, city.DisplayName)
	if err != nil {
		return cachedCity{}, false, err
	}
	if found {
		if err := updateCachedCity(ctx, db, canonical.QueryName, city); err != nil {
			return cachedCity{}, false, err
		}
		city.QueryName = canonical.QueryName
		return city, false, nil
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO cities (name, normalized_name, display_name, lat, lng, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, city.QueryName, city.Name, city.DisplayName, city.Lat, city.Lng, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return cachedCity{}, false, err
	}
	return city, false, nil
}

func findCanonicalCity(ctx context.Context, db *sql.DB, normalizedName, displayName string) (cachedCity, bool, error) {
	var city cachedCity
	row := db.QueryRowContext(ctx, `
		SELECT name, normalized_name, display_name, lat, lng
		FROM cities
		WHERE normalized_name = ?
			OR name = ?
			OR display_name = ?
		ORDER BY CASE WHEN name = normalized_name THEN 0 ELSE 1 END ASC, created_at ASC, name ASC
		LIMIT 1
	`, normalizedName, normalizedName, displayName)
	if err := row.Scan(&city.QueryName, &city.Name, &city.DisplayName, &city.Lat, &city.Lng); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return cachedCity{}, false, nil
		}
		return cachedCity{}, false, err
	}
	return city, true, nil
}

func updateCachedCity(ctx context.Context, db *sql.DB, queryName string, city cachedCity) error {
	_, err := db.ExecContext(ctx, `
		UPDATE cities
		SET normalized_name = ?, display_name = ?, lat = ?, lng = ?
		WHERE name = ?
	`, city.Name, city.DisplayName, city.Lat, city.Lng, queryName)
	return err
}

func shouldPromoteCityDisplay(normalizedName, current, candidate string) bool {
	normalizedName = strings.TrimSpace(normalizedName)
	current = strings.TrimSpace(current)
	candidate = strings.TrimSpace(candidate)
	if candidate == "" {
		return false
	}
	if current == "" {
		return true
	}
	if current == candidate {
		return false
	}
	if current == normalizedName {
		return false
	}
	if candidate == normalizedName {
		return true
	}
	return len(candidate) < len(current)
}

func geocodeCity(ctx context.Context, city string, userAgent string) (cachedCity, error) {
	values := url.Values{}
	values.Set("q", city)
	values.Set("format", "json")
	values.Set("limit", "1")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, nominatimBaseURL+"?"+values.Encode(), nil)
	if err != nil {
		return cachedCity{}, err
	}
	req.Header.Set("User-Agent", blankOr(userAgent, defaultUserAgent))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return cachedCity{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return cachedCity{}, fmt.Errorf("nominatim request failed: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var results []nominatimResult
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		return cachedCity{}, err
	}
	if len(results) == 0 {
		return cachedCity{}, fmt.Errorf("no geocoding result for %q", city)
	}

	lat, err := strconv.ParseFloat(results[0].Lat, 64)
	if err != nil {
		return cachedCity{}, err
	}
	lng, err := strconv.ParseFloat(results[0].Lon, 64)
	if err != nil {
		return cachedCity{}, err
	}

	return cachedCity{
		QueryName:   city,
		Name:        blankOr(results[0].Name, results[0].DisplayName),
		DisplayName: blankOr(results[0].Name, results[0].DisplayName),
		Lat:         lat,
		Lng:         lng,
	}, nil
}

func needsNormalizedNameRefresh(city cachedCity) bool {
	return strings.TrimSpace(city.Name) == "" ||
		(strings.TrimSpace(city.Name) == strings.TrimSpace(city.DisplayName) && strings.Contains(city.Name, ","))
}

func fetchStations(ctx context.Context, cfg config, lat, lng, radius float64, fuelType, sortBy string) ([]tankerStation, error) {
	values := url.Values{}
	values.Set("lat", strconv.FormatFloat(lat, 'f', 6, 64))
	values.Set("lng", strconv.FormatFloat(lng, 'f', 6, 64))
	values.Set("rad", strconv.FormatFloat(radius, 'f', 2, 64))
	values.Set("type", fuelType)
	values.Set("apikey", cfg.APIKey)
	if fuelType != "all" {
		values.Set("sort", sortBy)
	} else {
		values.Set("sort", "dist")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tankerKoenigBase+"/list.php?"+values.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", blankOr(cfg.UserAgent, defaultUserAgent))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("tankerkönig request failed: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var payload tankerListResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	if !payload.OK {
		return nil, fmt.Errorf("tankerkönig API error: %s", blankOr(payload.Message, payload.Status))
	}
	return payload.Stations, nil
}

// persistUpdate stores one city's fetch: the single-target shape of
// persistSweep, which is what a multi-city sweep uses.
func persistUpdate(ctx context.Context, db *sql.DB, d dialect, city cachedCity, stations []tankerStation, recordedAt time.Time, searchRadiusKm float64) error {
	fetches := []cityFetch{{
		Query:      cityQuery{name: city.QueryName, radius: searchRadiusKm},
		City:       city,
		Stations:   stations,
		RecordedAt: recordedAt,
	}}
	return persistSweep(ctx, db, d, fetches, dedupeFetches(fetches))
}

type priceSnapshotValues struct {
	ID     int64
	IsOpen bool
	E5     sql.NullFloat64
	E10    sql.NullFloat64
	Diesel sql.NullFloat64
}

type compactSnapshotRow struct {
	priceSnapshotValues
	CityName       string
	RecordedAt     string
	SearchRadiusKM float64
	Updated        bool
}

func compactPriceSnapshots(ctx context.Context, db *sql.DB) (compactResult, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return compactResult{}, err
	}
	defer tx.Rollback()

	var result compactResult
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM price_snapshots`).Scan(&result.BeforeCount); err != nil {
		return compactResult{}, err
	}

	stationRows, err := tx.QueryContext(ctx, `
		SELECT DISTINCT station_id
		FROM price_snapshots
		ORDER BY station_id
	`)
	if err != nil {
		return compactResult{}, err
	}
	var stationIDs []string
	for stationRows.Next() {
		var stationID string
		if err := stationRows.Scan(&stationID); err != nil {
			stationRows.Close()
			return compactResult{}, err
		}
		stationIDs = append(stationIDs, stationID)
	}
	if err := stationRows.Err(); err != nil {
		stationRows.Close()
		return compactResult{}, err
	}
	if err := stationRows.Close(); err != nil {
		return compactResult{}, err
	}

	for _, stationID := range stationIDs {
		snapshots, err := loadCompactSnapshots(ctx, tx, stationID)
		if err != nil {
			return compactResult{}, err
		}
		kept, deleteIDs := compactSnapshotRows(snapshots)
		result.StationsProcessed++

		for _, snapshot := range kept {
			if !snapshot.Updated {
				continue
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE price_snapshots
				SET city_name = ?, recorded_at = ?, search_radius_km = ?, is_open = ?, e5 = ?, e10 = ?, diesel = ?
				WHERE id = ?
			`, snapshot.CityName, snapshot.RecordedAt, snapshot.SearchRadiusKM, boolToInt(snapshot.IsOpen), nullFloatValue(snapshot.E5), nullFloatValue(snapshot.E10), nullFloatValue(snapshot.Diesel), snapshot.ID); err != nil {
				return compactResult{}, err
			}
			result.UpdatedCount++
		}
		for _, id := range deleteIDs {
			if _, err := tx.ExecContext(ctx, `DELETE FROM price_snapshots WHERE id = ?`, id); err != nil {
				return compactResult{}, err
			}
			result.DeletedCount++
		}
	}

	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM price_snapshots`).Scan(&result.AfterCount); err != nil {
		return compactResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return compactResult{}, err
	}
	return result, nil
}

func loadCompactSnapshots(ctx context.Context, tx *sql.Tx, stationID string) ([]compactSnapshotRow, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, city_name, recorded_at, search_radius_km, is_open, e5, e10, diesel
		FROM price_snapshots
		WHERE station_id = ?
		ORDER BY recorded_at ASC, id ASC
	`, stationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var snapshots []compactSnapshotRow
	for rows.Next() {
		var snapshot compactSnapshotRow
		if err := rows.Scan(
			&snapshot.ID,
			&snapshot.CityName,
			&snapshot.RecordedAt,
			&snapshot.SearchRadiusKM,
			&snapshot.IsOpen,
			&snapshot.E5,
			&snapshot.E10,
			&snapshot.Diesel,
		); err != nil {
			return nil, err
		}
		snapshots = append(snapshots, snapshot)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return snapshots, nil
}

func compactSnapshotRows(snapshots []compactSnapshotRow) ([]compactSnapshotRow, []int64) {
	var kept []compactSnapshotRow
	var deleteIDs []int64

	for _, snapshot := range snapshots {
		if len(kept) == 0 {
			kept = append(kept, snapshot)
			continue
		}

		latest := kept[len(kept)-1]
		switch {
		case !priceSnapshotValuesEqual(latest.priceSnapshotValues, snapshot.priceSnapshotValues):
			kept = append(kept, snapshot)
		case len(kept) >= 2 && !priceSnapshotValuesEqual(latest.priceSnapshotValues, kept[len(kept)-2].priceSnapshotValues):
			kept = append(kept, snapshot)
		default:
			deleteID := snapshot.ID
			snapshot.ID = latest.ID
			snapshot.Updated = true
			kept[len(kept)-1] = snapshot
			deleteIDs = append(deleteIDs, deleteID)
		}
	}

	return kept, deleteIDs
}

func persistPriceSnapshot(ctx context.Context, tx *sql.Tx, centres *cityCentres, city cachedCity, station tankerStation, recordedAt time.Time, searchRadiusKm float64) error {
	latest, previous, currentOwner, err := latestPriceSnapshots(ctx, tx, station.ID)
	if err != nil {
		return err
	}

	// A station inside two targets' radii reaches this once per sweep, not once
	// per city (see dedupeFetches), so no row has to be duplicated to keep it.
	// Which city the row names is decided separately, against every city known
	// to own it — not just the ones this sweep happened to fetch.
	owner, err := resolveSnapshotOwner(ctx, tx, centres, currentOwner, city, station)
	if err != nil {
		return err
	}

	current := priceSnapshotValues{
		IsOpen: station.IsOpen,
		E5:     floatPtrToNull(station.E5),
		E10:    floatPtrToNull(station.E10),
		Diesel: floatPtrToNull(station.Diesel),
	}

	switch {
	case latest == nil:
		return insertPriceSnapshot(ctx, tx, owner, station, recordedAt, searchRadiusKm)
	case !priceSnapshotValuesEqual(*latest, current):
		return insertPriceSnapshot(ctx, tx, owner, station, recordedAt, searchRadiusKm)
	case previous != nil && !priceSnapshotValuesEqual(*latest, *previous):
		return insertPriceSnapshot(ctx, tx, owner, station, recordedAt, searchRadiusKm)
	default:
		_, err := tx.ExecContext(ctx, `
			UPDATE price_snapshots
			SET city_name = ?, recorded_at = ?, search_radius_km = ?, is_open = ?, e5 = ?, e10 = ?, diesel = ?
			WHERE id = ?
		`, owner, recordedAt.Format(time.RFC3339), searchRadiusKm, boolToInt(station.IsOpen), nullableFloat(station.E5), nullableFloat(station.E10), nullableFloat(station.Diesel), latest.ID)
		return err
	}
}

// latestPriceSnapshots returns the two most recent snapshots for a station plus
// the city_name of the latest one, which is the station's current owner.
func latestPriceSnapshots(ctx context.Context, tx *sql.Tx, stationID string) (*priceSnapshotValues, *priceSnapshotValues, string, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, city_name, is_open, e5, e10, diesel
		FROM price_snapshots
		WHERE station_id = ?
		ORDER BY recorded_at DESC, id DESC
		LIMIT 2
	`, stationID)
	if err != nil {
		return nil, nil, "", err
	}
	defer rows.Close()

	var snapshots []*priceSnapshotValues
	var cities []string
	for rows.Next() {
		snapshot := priceSnapshotValues{}
		var cityName string
		if err := rows.Scan(&snapshot.ID, &cityName, &snapshot.IsOpen, &snapshot.E5, &snapshot.E10, &snapshot.Diesel); err != nil {
			return nil, nil, "", err
		}
		snapshots = append(snapshots, &snapshot)
		cities = append(cities, cityName)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, "", err
	}

	if len(snapshots) == 0 {
		return nil, nil, "", nil
	}
	if len(snapshots) == 1 {
		return snapshots[0], nil, cities[0], nil
	}
	return snapshots[0], snapshots[1], cities[0], nil
}

// cityCentres caches city-centre lookups for one sweep. A snapshot's owner is
// stored as a normalized city name, and resolving it means scanning a cities
// table that `import cities` can fill with thousands of rows, so each owner is
// looked up at most once per sweep.
type cityCentres struct {
	byName map[string]*cachedCity
}

func newCityCentres() *cityCentres {
	return &cityCentres{byName: map[string]*cachedCity{}}
}

// lookup returns the centre cached for a normalized city name, or nil when no
// such city is known.
func (c *cityCentres) lookup(ctx context.Context, tx *sql.Tx, name string) (*cachedCity, error) {
	if centre, ok := c.byName[name]; ok {
		return centre, nil
	}
	var centre cachedCity
	// Same preference order as loadCachedCity: a row whose query name already
	// is the normalized name wins, then the oldest.
	err := tx.QueryRowContext(ctx, `
		SELECT normalized_name, lat, lng
		FROM cities
		WHERE normalized_name = ?
		ORDER BY CASE WHEN name = normalized_name THEN 0 ELSE 1 END ASC, created_at ASC, name ASC
		LIMIT 1
	`, name).Scan(&centre.Name, &centre.Lat, &centre.Lng)
	if errors.Is(err, sql.ErrNoRows) {
		c.byName[name] = nil
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c.byName[name] = &centre
	return &centre, nil
}

// resolveSnapshotOwner picks the city_name a snapshot carries. dedupeFetches
// only ranks the targets a sweep actually fetched, so a single-city run — or a
// sweep whose nearer target failed — must not take a station away from the
// nearer city that already owns it. Ownership therefore moves only to a
// strictly nearer centre, which keeps ownership independent of invocation
// order and transient fetch failures.
func resolveSnapshotOwner(ctx context.Context, tx *sql.Tx, centres *cityCentres, currentOwner string, candidate cachedCity, station tankerStation) (string, error) {
	if currentOwner == "" || currentOwner == candidate.Name {
		return candidate.Name, nil
	}
	owner, err := centres.lookup(ctx, tx, currentOwner)
	if err != nil {
		return "", err
	}
	if owner == nil {
		// The owning city is no longer cached (target deleted, cache cleared),
		// so it can no longer claim the station.
		return candidate.Name, nil
	}
	if haversineKM(owner.Lat, owner.Lng, station.Lat, station.Lng) <=
		haversineKM(candidate.Lat, candidate.Lng, station.Lat, station.Lng) {
		return currentOwner, nil
	}
	return candidate.Name, nil
}

func insertPriceSnapshot(ctx context.Context, tx *sql.Tx, cityName string, station tankerStation, recordedAt time.Time, searchRadiusKm float64) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO price_snapshots (
			station_id, city_name, recorded_at, search_radius_km, is_open, e5, e10, diesel
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, station.ID, cityName, recordedAt.Format(time.RFC3339), searchRadiusKm, boolToInt(station.IsOpen), nullableFloat(station.E5), nullableFloat(station.E10), nullableFloat(station.Diesel))
	return err
}

func priceSnapshotValuesEqual(a, b priceSnapshotValues) bool {
	return a.IsOpen == b.IsOpen &&
		nullFloatEqual(a.E5, b.E5) &&
		nullFloatEqual(a.E10, b.E10) &&
		nullFloatEqual(a.Diesel, b.Diesel)
}

func nullFloatEqual(a, b sql.NullFloat64) bool {
	if a.Valid != b.Valid {
		return false
	}
	return !a.Valid || a.Float64 == b.Float64
}

func floatPtrToNull(v *float64) sql.NullFloat64 {
	if v == nil {
		return sql.NullFloat64{}
	}
	return sql.NullFloat64{Float64: *v, Valid: true}
}

func nullFloatValue(v sql.NullFloat64) any {
	if !v.Valid {
		return nil
	}
	return v.Float64
}

func nullableFloat(v *float64) any {
	if v == nil {
		return nil
	}
	return *v
}

func suggestFuelColumn(fuel string) (string, error) {
	if !isSuggestFuelType(fuel) {
		return "", errors.New("--fuel must be one of: diesel, e5, e10")
	}
	return fuel, nil
}

func haversineKM(lat1, lng1, lat2, lng2 float64) float64 {
	const earthRadiusKM = 6371.0088
	lat1Rad := degreesToRadians(lat1)
	lat2Rad := degreesToRadians(lat2)
	deltaLat := degreesToRadians(lat2 - lat1)
	deltaLng := degreesToRadians(lng2 - lng1)

	a := math.Sin(deltaLat/2)*math.Sin(deltaLat/2) +
		math.Cos(lat1Rad)*math.Cos(lat2Rad)*math.Sin(deltaLng/2)*math.Sin(deltaLng/2)
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
	return earthRadiusKM * c
}

func degreesToRadians(degrees float64) float64 {
	return degrees * math.Pi / 180
}

func localHourStart(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, t.Location())
}

func nextLocalHour(t time.Time) time.Time {
	start := localHourStart(t)
	if t.Equal(start) {
		return start
	}
	return start.Add(time.Hour)
}

func localDayStart(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func roundTo(value float64, decimals int) float64 {
	factor := math.Pow10(decimals)
	return math.Round(value*factor) / factor
}

func distinctSampleDays(samples []priceSample) int {
	days := make(map[string]struct{})
	for _, sample := range samples {
		days[sample.Date] = struct{}{}
	}
	return len(days)
}

func formatStationAddress(street, houseNumber string, postCode int, place string) string {
	street = strings.TrimSpace(street)
	houseNumber = strings.TrimSpace(houseNumber)
	place = strings.TrimSpace(place)

	streetLine := strings.TrimSpace(strings.Join(nonEmptyStrings(street, houseNumber), " "))
	var cityLine string
	if postCode > 0 && place != "" {
		cityLine = fmt.Sprintf("%d %s", postCode, place)
	} else if postCode > 0 {
		cityLine = strconv.Itoa(postCode)
	} else {
		cityLine = place
	}

	return strings.Join(nonEmptyStrings(streetLine, cityLine), ", ")
}

func nonEmptyStrings(values ...string) []string {
	var result []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			result = append(result, value)
		}
	}
	return result
}

func priceCheckLess(a, b priceCheckRow) bool {
	if recommendationRank(a.Recommendation) != recommendationRank(b.Recommendation) {
		return recommendationRank(a.Recommendation) > recommendationRank(b.Recommendation)
	}
	if a.CurrentPrice != b.CurrentPrice {
		return a.CurrentPrice < b.CurrentPrice
	}
	if confidenceRank(a.Confidence) != confidenceRank(b.Confidence) {
		return confidenceRank(a.Confidence) > confidenceRank(b.Confidence)
	}
	if a.DistanceKM != b.DistanceKM {
		return a.DistanceKM < b.DistanceKM
	}
	return a.StationName < b.StationName
}

func recommendationRank(recommendation string) int {
	switch recommendation {
	case "buy":
		return 3
	case "hold":
		return 2
	case "wait":
		return 1
	default:
		return 0
	}
}

func suggestionCandidateLess(a, b suggestionCandidate) bool {
	if a.rawPrice != b.rawPrice {
		return a.rawPrice < b.rawPrice
	}
	if confidenceRank(a.Confidence) != confidenceRank(b.Confidence) {
		return confidenceRank(a.Confidence) > confidenceRank(b.Confidence)
	}
	if a.DistanceKM != b.DistanceKM {
		return a.DistanceKM < b.DistanceKM
	}
	if !a.start.Equal(b.start) {
		return a.start.Before(b.start)
	}
	return a.StationName < b.StationName
}

func confidenceRank(confidence string) int {
	switch confidence {
	case "high":
		return 3
	case "medium":
		return 2
	default:
		return 1
	}
}

func duplicatesNearbyStationWindow(candidate suggestionCandidate, selected []suggestionCandidate) bool {
	for _, existing := range selected {
		if existing.StationID != candidate.StationID {
			continue
		}
		if math.Abs(candidate.start.Sub(existing.start).Hours()) < 2 {
			return true
		}
	}
	return false
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func isValidFuelType(value string) bool {
	switch value {
	case "all", "diesel", "e5", "e10":
		return true
	default:
		return false
	}
}

// suggestFuels lists the fuels every suggest, check and notify run covers, in
// display order. All of them, always: users pick which one they are notified
// about, and measuring only a subset of what gets delivered would leave the
// rest unmeasured.
var suggestFuels = []string{"diesel", "e5", "e10"}

func isSuggestFuelType(value string) bool {
	for _, fuel := range suggestFuels {
		if value == fuel {
			return true
		}
	}
	return false
}

func isValidSort(value string) bool {
	switch value {
	case "dist", "price":
		return true
	default:
		return false
	}
}

func blankOr(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func blankDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}

func formatNullFloat(v sql.NullFloat64) string {
	if !v.Valid {
		return "-"
	}
	return fmt.Sprintf("%.3f", v.Float64)
}
