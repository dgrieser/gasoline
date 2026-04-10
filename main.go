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
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const (
	defaultDBPath     = "gasoline.db"
	tankerKoenigBase  = "https://creativecommons.tankerkoenig.de/json"
	nominatimBaseURL  = "https://nominatim.openstreetmap.org/search"
	defaultUserAgent  = "gasoline-cli/1.0 (local utility)"
	envAPIKeyName     = "TANKER_KOENIG_API_KEY"
	envDBPathName     = "GASOLINE_DB_PATH"
	sqliteBusyTimeout = 5000
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
	CityName   string   `json:"city_name"`
	RecordedAt string   `json:"recorded_at"`
	IsOpen     bool     `json:"is_open"`
	E5         *float64 `json:"e5,omitempty"`
	E10        *float64 `json:"e10,omitempty"`
	Diesel     *float64 `json:"diesel,omitempty"`
}

var stdout io.Writer = os.Stdout
var countryCodePattern = regexp.MustCompile(`^[A-Za-z]{2}$`)
var geoNamesFeatureCodePattern = regexp.MustCompile(`^(PPL|PPLC|PPLA[1-9]*)$`)

type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
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
	case "help", "-h", "--help":
		printUsage()
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func printUsage() {
	fmt.Println(`gasoline: persist Tankerkönig station prices into SQLite

Commands:
  update   geocode a city if needed, query Tankerkönig, store station snapshots
  compact  compact existing price snapshots in-place
  migrate  apply schema migrations to an existing database
  list cities   list cached city geocodes
  list stations list known stations with latest stored snapshot
  list history  show historical prices for one station
  import cities import GeoNames populated places for a 2-letter country code
  clear cities  clear all cached cities

Examples:
  gasoline update --city "Berlin, Germany" --radius 5
  gasoline compact
  gasoline migrate
  gasoline list cities
  gasoline list stations --city "Berlin, Germany"
  gasoline list history --station-id 474e5046-deaf-4f9b-9a32-9797b778f047 --fuel diesel
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

func runUpdate(args []string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath, "SQLite database file")
	city := fs.String("city", "", "City or place to geocode and query")
	radius := fs.Float64("radius", 5, "Search radius in km (max 25)")
	fuelType := fs.String("fuel", "all", "Fuel type: all, diesel, e5, e10")
	sortBy := fs.String("sort", "dist", "Sort order: dist or price")
	userAgent := fs.String("user-agent", defaultUserAgent, "User-Agent for Nominatim and API calls")
	outputLong, outputShort := addOutputFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	resolvedDBPath := resolveDBPath(fs, *dbPath)
	output, err := resolveOutputMode(*outputLong, *outputShort)
	if err != nil {
		return err
	}
	if strings.TrimSpace(*city) == "" {
		return errors.New("update requires --city")
	}
	if *radius <= 0 || *radius > 25 {
		return errors.New("--radius must be > 0 and <= 25")
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

	db, err := openDB(resolvedDBPath)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx := context.Background()
	if err := initSchema(ctx, db); err != nil {
		return err
	}

	location, cached, err := getOrCreateCity(ctx, db, strings.TrimSpace(*city), cfg.UserAgent)
	if err != nil {
		return err
	}

	stations, err := fetchStations(ctx, cfg, location.Lat, location.Lng, *radius, *fuelType, *sortBy)
	if err != nil {
		return err
	}

	recordedAt := time.Now().UTC()
	if err := persistUpdate(ctx, db, location, stations, recordedAt, *radius); err != nil {
		return err
	}

	cacheStatus := "resolved via geocoder"
	if cached {
		cacheStatus = "loaded from cache"
	}

	if output == outputJSON {
		return writeJSON(updateResult{
			City:        location,
			CacheStatus: cacheStatus,
			StoredCount: len(stations),
			RecordedAt:  recordedAt.Format(time.RFC3339),
			DBPath:      resolvedDBPath,
		})
	}

	fmt.Fprintf(stdout, "city: %s\n", location.Name)
	fmt.Fprintf(stdout, "display: %s\n", location.DisplayName)
	fmt.Fprintf(stdout, "coordinates: %.6f, %.6f (%s)\n", location.Lat, location.Lng, cacheStatus)
	fmt.Fprintf(stdout, "stored %d station snapshots at %s in %s\n", len(stations), recordedAt.Format(time.RFC3339), resolvedDBPath)
	return nil
}

func runCompact(args []string) error {
	fs := flag.NewFlagSet("compact", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath, "SQLite database file")
	outputLong, outputShort := addOutputFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	resolvedDBPath := resolveDBPath(fs, *dbPath)
	output, err := resolveOutputMode(*outputLong, *outputShort)
	if err != nil {
		return err
	}

	db, err := openDB(resolvedDBPath)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx := context.Background()
	if err := initSchema(ctx, db); err != nil {
		return err
	}

	result, err := compactPriceSnapshots(ctx, db)
	if err != nil {
		return err
	}
	result.DBPath = resolvedDBPath

	if output == outputJSON {
		return writeJSON(result)
	}

	fmt.Fprintf(stdout, "compacted %d stations in %s\n", result.StationsProcessed, resolvedDBPath)
	fmt.Fprintf(stdout, "snapshots: %d -> %d (deleted=%d, updated=%d)\n", result.BeforeCount, result.AfterCount, result.DeletedCount, result.UpdatedCount)
	return nil
}

func runMigrate(args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath, "SQLite database file")
	outputLong, outputShort := addOutputFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	resolvedDBPath := resolveDBPath(fs, *dbPath)
	output, err := resolveOutputMode(*outputLong, *outputShort)
	if err != nil {
		return err
	}

	db, err := openDB(resolvedDBPath)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx := context.Background()
	if err := ensureSchema(ctx, db); err != nil {
		return err
	}

	result, err := migrateSchema(ctx, db)
	if err != nil {
		return err
	}
	result.DBPath = resolvedDBPath

	if output == outputJSON {
		return writeJSON(result)
	}
	if len(result.Applied) == 0 {
		fmt.Fprintf(stdout, "no migrations needed for %s\n", resolvedDBPath)
		return nil
	}
	fmt.Fprintf(stdout, "applied %d migrations to %s\n", len(result.Applied), resolvedDBPath)
	for _, migration := range result.Applied {
		fmt.Fprintf(stdout, "- %s\n", migration)
	}
	return nil
}

func runCities(args []string) error {
	fs := flag.NewFlagSet("cities", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath, "SQLite database file")
	outputLong, outputShort := addOutputFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	resolvedDBPath := resolveDBPath(fs, *dbPath)
	output, err := resolveOutputMode(*outputLong, *outputShort)
	if err != nil {
		return err
	}

	db, err := openDB(resolvedDBPath)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx := context.Background()
	if err := initSchema(ctx, db); err != nil {
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
	dbPath := fs.String("db", defaultDBPath, "SQLite database file")
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
	resolvedDBPath := resolveDBPath(fs, *dbPath)

	db, err := openDB(resolvedDBPath)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx := context.Background()
	if err := initSchema(ctx, db); err != nil {
		return err
	}

	sourceURL := fmt.Sprintf("https://download.geonames.org/export/dump/%s.zip", countryCode)
	cities, err := downloadGeoNamesCities(ctx, sourceURL, countryCode, defaultUserAgent)
	if err != nil {
		return err
	}

	importedCount, err := importCities(ctx, db, cities)
	if err != nil {
		return err
	}

	result := importCitiesResult{
		CountryCode:   countryCode,
		SourceURL:     sourceURL,
		ParsedCount:   len(cities),
		ImportedCount: importedCount,
		DBPath:        resolvedDBPath,
	}
	if output == outputJSON {
		return writeJSON(result)
	}

	fmt.Fprintf(stdout, "source: %s\n", sourceURL)
	fmt.Fprintf(stdout, "country: %s\n", countryCode)
	fmt.Fprintf(stdout, "parsed %d cities\n", result.ParsedCount)
	fmt.Fprintf(stdout, "imported %d cities into %s\n", result.ImportedCount, resolvedDBPath)
	return nil
}

func runClearCities(args []string) error {
	fs := flag.NewFlagSet("clear cities", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath, "SQLite database file")
	outputLong, outputShort := addOutputFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	output, err := resolveOutputMode(*outputLong, *outputShort)
	if err != nil {
		return err
	}
	resolvedDBPath := resolveDBPath(fs, *dbPath)

	db, err := openDB(resolvedDBPath)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx := context.Background()
	if err := initSchema(ctx, db); err != nil {
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
		DBPath:       resolvedDBPath,
	}
	if output == outputJSON {
		return writeJSON(result)
	}

	fmt.Fprintf(stdout, "cleared %d cities from %s\n", result.ClearedCount, resolvedDBPath)
	return nil
}

func runStations(args []string) error {
	fs := flag.NewFlagSet("stations", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath, "SQLite database file")
	city := fs.String("city", "", "Optional city filter from stored sync runs")
	limit := fs.Int("limit", 50, "Max rows to print")
	outputLong, outputShort := addOutputFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	resolvedDBPath := resolveDBPath(fs, *dbPath)
	output, err := resolveOutputMode(*outputLong, *outputShort)
	if err != nil {
		return err
	}
	if *limit <= 0 {
		return errors.New("--limit must be > 0")
	}

	db, err := openDB(resolvedDBPath)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx := context.Background()
	if err := initSchema(ctx, db); err != nil {
		return err
	}

	var (
		rows *sql.Rows
	)
	if strings.TrimSpace(*city) == "" {
		rows, err = db.QueryContext(ctx, `
			SELECT
				s.id, s.name, COALESCE(s.brand, ''), COALESCE(s.street, ''), COALESCE(s.place, ''),
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
			ORDER BY s.name ASC
			LIMIT ?
		`, *limit)
	} else {
		rows, err = db.QueryContext(ctx, `
			SELECT
				s.id, s.name, COALESCE(s.brand, ''), COALESCE(s.street, ''), COALESCE(s.place, ''),
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
			ORDER BY s.name ASC
			LIMIT ?
		`, strings.TrimSpace(*city), *limit)
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
	dbPath := fs.String("db", defaultDBPath, "SQLite database file")
	stationID := fs.String("station-id", "", "Station UUID")
	fuel := fs.String("fuel", "all", "Fuel type: all, diesel, e5, e10")
	limit := fs.Int("limit", 100, "Max history rows")
	outputLong, outputShort := addOutputFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	resolvedDBPath := resolveDBPath(fs, *dbPath)
	output, err := resolveOutputMode(*outputLong, *outputShort)
	if err != nil {
		return err
	}
	if strings.TrimSpace(*stationID) == "" {
		return errors.New("history requires --station-id")
	}
	if !isValidFuelType(*fuel) {
		return errors.New("--fuel must be one of: all, diesel, e5, e10")
	}
	if *limit <= 0 {
		return errors.New("--limit must be > 0")
	}

	db, err := openDB(resolvedDBPath)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx := context.Background()
	if err := initSchema(ctx, db); err != nil {
		return err
	}

	query := `
		SELECT city_name, recorded_at, is_open, e5, e10, diesel
		FROM price_snapshots
		WHERE station_id = ?
		ORDER BY recorded_at DESC
		LIMIT ?
	`
	rows, err := db.QueryContext(ctx, query, strings.TrimSpace(*stationID), *limit)
	if err != nil {
		return err
	}
	defer rows.Close()

	var results []historyRow
	for rows.Next() {
		var (
			cityName, recordedAt string
			isOpen               bool
			e5, e10, diesel      sql.NullFloat64
		)
		if err := rows.Scan(&cityName, &recordedAt, &isOpen, &e5, &e10, &diesel); err != nil {
			return err
		}
		row := historyRow{
			CityName:   cityName,
			RecordedAt: recordedAt,
			IsOpen:     isOpen,
		}
		switch *fuel {
		case "e5":
			row.E5 = nullFloatPtr(e5)
			if output == outputText {
				fmt.Fprintf(stdout, "%s | city=%s | open=%t | e5=%s\n", recordedAt, cityName, isOpen, formatNullFloat(e5))
			}
		case "e10":
			row.E10 = nullFloatPtr(e10)
			if output == outputText {
				fmt.Fprintf(stdout, "%s | city=%s | open=%t | e10=%s\n", recordedAt, cityName, isOpen, formatNullFloat(e10))
			}
		case "diesel":
			row.Diesel = nullFloatPtr(diesel)
			if output == outputText {
				fmt.Fprintf(stdout, "%s | city=%s | open=%t | diesel=%s\n", recordedAt, cityName, isOpen, formatNullFloat(diesel))
			}
		default:
			row.E5 = nullFloatPtr(e5)
			row.E10 = nullFloatPtr(e10)
			row.Diesel = nullFloatPtr(diesel)
			if output == outputText {
				fmt.Fprintf(stdout, "%s | city=%s | open=%t | e5=%s e10=%s diesel=%s\n",
					recordedAt, cityName, isOpen, formatNullFloat(e5), formatNullFloat(e10), formatNullFloat(diesel))
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

func openDB(path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil && filepath.Dir(path) != "." {
		return nil, err
	}
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(%d)&_pragma=foreign_keys(1)", path, sqliteBusyTimeout)
	return sql.Open("sqlite", dsn)
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

func initSchema(ctx context.Context, db *sql.DB) error {
	if err := ensureSchema(ctx, db); err != nil {
		return err
	}
	_, err := migrateSchema(ctx, db)
	return err
}

func ensureSchema(ctx context.Context, db *sql.DB) error {
	schema := `
	CREATE TABLE IF NOT EXISTS cities (
		name TEXT PRIMARY KEY,
		normalized_name TEXT NOT NULL,
		display_name TEXT NOT NULL,
		lat REAL NOT NULL,
		lng REAL NOT NULL,
		created_at TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS stations (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		brand TEXT,
		street TEXT,
		house_number TEXT,
		post_code INTEGER,
		place TEXT,
		lat REAL NOT NULL,
		lng REAL NOT NULL,
		first_seen_at TEXT NOT NULL,
		last_seen_at TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS price_snapshots (
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
	);

	CREATE INDEX IF NOT EXISTS idx_price_snapshots_station_recorded
		ON price_snapshots(station_id, recorded_at DESC);

	CREATE INDEX IF NOT EXISTS idx_price_snapshots_city_recorded
		ON price_snapshots(city_name, recorded_at DESC);

	CREATE INDEX IF NOT EXISTS idx_stations_lat_lng
		ON stations(lat, lng);
	`
	_, err := db.ExecContext(ctx, schema)
	return err
}

func migrateSchema(ctx context.Context, db *sql.DB) (migrateResult, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return migrateResult{}, err
	}
	defer tx.Rollback()

	var result migrateResult
	if err := migrateCitiesNormalizedName(ctx, tx, &result); err != nil {
		return migrateResult{}, err
	}
	if err := migrateCitiesDeduplicate(ctx, tx, &result); err != nil {
		return migrateResult{}, err
	}
	if err := migratePriceSnapshotsDropDistKM(ctx, tx, &result); err != nil {
		return migrateResult{}, err
	}

	if err := tx.Commit(); err != nil {
		return migrateResult{}, err
	}
	return result, nil
}

func migrateCitiesNormalizedName(ctx context.Context, tx *sql.Tx, result *migrateResult) error {
	hasColumn, err := tableHasColumn(ctx, tx, "cities", "normalized_name")
	if err != nil {
		return err
	}
	if !hasColumn {
		if _, err := tx.ExecContext(ctx, `
			ALTER TABLE cities
			ADD COLUMN normalized_name TEXT NOT NULL DEFAULT ''
		`); err != nil {
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

		if shouldPromoteCityDisplay(keeper.DisplayName, row.DisplayName) {
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

func migratePriceSnapshotsDropDistKM(ctx context.Context, tx *sql.Tx, result *migrateResult) error {
	hasColumn, err := tableHasColumn(ctx, tx, "price_snapshots", "dist_km")
	if err != nil {
		return err
	}
	if !hasColumn {
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

func tableHasColumn(ctx context.Context, q queryer, tableName, columnName string) (bool, error) {
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

func importCities(ctx context.Context, db *sql.DB, cities []cachedCity) (int, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO cities (name, normalized_name, display_name, lat, lng, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			normalized_name = excluded.normalized_name,
			display_name = excluded.display_name,
			lat = excluded.lat,
			lng = excluded.lng
	`)
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

func shouldPromoteCityDisplay(current, candidate string) bool {
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
	return len(candidate) > len(current)
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
		DisplayName: results[0].DisplayName,
		Lat:         lat,
		Lng:         lng,
	}, nil
}

func needsNormalizedNameRefresh(city cachedCity) bool {
	return strings.TrimSpace(city.Name) == "" || city.Name == city.DisplayName
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

func persistUpdate(ctx context.Context, db *sql.DB, city cachedCity, stations []tankerStation, recordedAt time.Time, searchRadiusKm float64) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, station := range stations {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO stations (
				id, name, brand, street, house_number, post_code, place, lat, lng, first_seen_at, last_seen_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				name = excluded.name,
				brand = excluded.brand,
				street = excluded.street,
				house_number = excluded.house_number,
				post_code = excluded.post_code,
				place = excluded.place,
				lat = excluded.lat,
				lng = excluded.lng,
				last_seen_at = excluded.last_seen_at
		`, station.ID, station.Name, station.Brand, station.Street, station.HouseNumber, station.PostCode, station.Place, station.Lat, station.Lng, recordedAt.Format(time.RFC3339), recordedAt.Format(time.RFC3339)); err != nil {
			return err
		}

		if err := persistPriceSnapshot(ctx, tx, city, station, recordedAt, searchRadiusKm); err != nil {
			return err
		}
	}

	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO cities (name, normalized_name, display_name, lat, lng, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		city.QueryName, city.Name, city.DisplayName, city.Lat, city.Lng, recordedAt.Format(time.RFC3339),
	); err != nil {
		return err
	}

	return tx.Commit()
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

func persistPriceSnapshot(ctx context.Context, tx *sql.Tx, city cachedCity, station tankerStation, recordedAt time.Time, searchRadiusKm float64) error {
	latest, previous, err := latestPriceSnapshots(ctx, tx, station.ID)
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
		return insertPriceSnapshot(ctx, tx, city, station, recordedAt, searchRadiusKm)
	case !priceSnapshotValuesEqual(*latest, current):
		return insertPriceSnapshot(ctx, tx, city, station, recordedAt, searchRadiusKm)
	case previous != nil && !priceSnapshotValuesEqual(*latest, *previous):
		return insertPriceSnapshot(ctx, tx, city, station, recordedAt, searchRadiusKm)
	default:
		_, err := tx.ExecContext(ctx, `
			UPDATE price_snapshots
			SET city_name = ?, recorded_at = ?, search_radius_km = ?, is_open = ?, e5 = ?, e10 = ?, diesel = ?
			WHERE id = ?
		`, city.Name, recordedAt.Format(time.RFC3339), searchRadiusKm, boolToInt(station.IsOpen), nullableFloat(station.E5), nullableFloat(station.E10), nullableFloat(station.Diesel), latest.ID)
		return err
	}
}

func latestPriceSnapshots(ctx context.Context, tx *sql.Tx, stationID string) (*priceSnapshotValues, *priceSnapshotValues, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, is_open, e5, e10, diesel
		FROM price_snapshots
		WHERE station_id = ?
		ORDER BY recorded_at DESC, id DESC
		LIMIT 2
	`, stationID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	var snapshots []*priceSnapshotValues
	for rows.Next() {
		snapshot := priceSnapshotValues{}
		if err := rows.Scan(&snapshot.ID, &snapshot.IsOpen, &snapshot.E5, &snapshot.E10, &snapshot.Diesel); err != nil {
			return nil, nil, err
		}
		snapshots = append(snapshots, &snapshot)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	if len(snapshots) == 0 {
		return nil, nil, nil
	}
	if len(snapshots) == 1 {
		return snapshots[0], nil, nil
	}
	return snapshots[0], snapshots[1], nil
}

func insertPriceSnapshot(ctx context.Context, tx *sql.Tx, city cachedCity, station tankerStation, recordedAt time.Time, searchRadiusKm float64) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO price_snapshots (
			station_id, city_name, recorded_at, search_radius_km, is_open, e5, e10, diesel
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, station.ID, city.Name, recordedAt.Format(time.RFC3339), searchRadiusKm, boolToInt(station.IsOpen), nullableFloat(station.E5), nullableFloat(station.E10), nullableFloat(station.Diesel))
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
