package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestLoadDotEnv(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	content := strings.Join([]string{
		`# comment`,
		`TANKER_KOENIG_API_KEY="test-key"`,
		`USER_AGENT='gasoline-test/1.0'`,
		`EMPTY=`,
		``,
	}, "\n")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write env: %v", err)
	}

	values, err := loadDotEnv(path)
	if err != nil {
		t.Fatalf("loadDotEnv: %v", err)
	}

	if got := values[envAPIKeyName]; got != "test-key" {
		t.Fatalf("api key = %q, want %q", got, "test-key")
	}
	if got := values["USER_AGENT"]; got != "gasoline-test/1.0" {
		t.Fatalf("user agent = %q, want %q", got, "gasoline-test/1.0")
	}
	if got := values["EMPTY"]; got != "" {
		t.Fatalf("empty = %q, want empty string", got)
	}
}

func TestLoadConfigAllowsMissingDotEnv(t *testing.T) {
	t.Parallel()

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	tempDir := t.TempDir()
	if err := os.Chdir(tempDir); err != nil {
		t.Fatalf("chdir temp dir: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(wd); err != nil {
			t.Fatalf("restore cwd: %v", err)
		}
	})

	_, err = loadConfig("gasoline-test/1.0")
	if err == nil {
		t.Fatal("expected missing API key error")
	}
	if !strings.Contains(err.Error(), "not set in environment or .env") {
		t.Fatalf("err = %v, want missing api key error", err)
	}
}

func TestValidationHelpers(t *testing.T) {
	t.Parallel()

	validFuels := []string{"all", "diesel", "e5", "e10"}
	for _, fuel := range validFuels {
		if !isValidFuelType(fuel) {
			t.Fatalf("expected valid fuel type %q", fuel)
		}
	}
	if isValidFuelType("premium") {
		t.Fatal("unexpected valid fuel type")
	}

	validSorts := []string{"dist", "price"}
	for _, sort := range validSorts {
		if !isValidSort(sort) {
			t.Fatalf("expected valid sort %q", sort)
		}
	}
	if isValidSort("name") {
		t.Fatal("unexpected valid sort")
	}
}

func TestPersistUpdateAndQueryHistory(t *testing.T) {
	t.Parallel()

	db := openTestDB(t)
	ctx := context.Background()
	recordedAt := time.Date(2026, 4, 2, 9, 15, 0, 0, time.UTC)

	priceE5 := 1.789
	priceE10 := 1.729
	priceDiesel := 1.659

	city := cachedCity{
		QueryName:   "Berlin, Germany",
		Name:        "Berlin",
		DisplayName: "Berlin, Deutschland",
		Lat:         52.517389,
		Lng:         13.395131,
	}
	stations := []tankerStation{
		{
			ID:          "station-1",
			Name:        "Test Station",
			Brand:       "ARAL",
			Street:      "Test Street",
			Place:       "Berlin",
			Lat:         52.5,
			Lng:         13.4,
			Dist:        1.25,
			Diesel:      &priceDiesel,
			E5:          &priceE5,
			E10:         &priceE10,
			IsOpen:      true,
			HouseNumber: "1",
			PostCode:    10115,
		},
	}

	if err := persistUpdate(ctx, db, city, stations, recordedAt, 5); err != nil {
		t.Fatalf("persistUpdate: %v", err)
	}

	var stationCount, snapshotCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM stations`).Scan(&stationCount); err != nil {
		t.Fatalf("count stations: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM price_snapshots`).Scan(&snapshotCount); err != nil {
		t.Fatalf("count snapshots: %v", err)
	}
	if stationCount != 1 {
		t.Fatalf("station count = %d, want 1", stationCount)
	}
	if snapshotCount != 1 {
		t.Fatalf("snapshot count = %d, want 1", snapshotCount)
	}

	var (
		name       string
		lastSeenAt string
		cityName   string
		dist       float64
		isOpen     bool
		diesel     sql.NullFloat64
	)
	if err := db.QueryRowContext(ctx, `
		SELECT s.name, s.last_seen_at, ps.city_name, ps.dist_km, ps.is_open, ps.diesel
		FROM stations s
		JOIN price_snapshots ps ON ps.station_id = s.id
		WHERE s.id = ?
	`, "station-1").Scan(&name, &lastSeenAt, &cityName, &dist, &isOpen, &diesel); err != nil {
		t.Fatalf("query stored rows: %v", err)
	}

	if name != "Test Station" {
		t.Fatalf("name = %q, want %q", name, "Test Station")
	}
	if lastSeenAt != recordedAt.Format(time.RFC3339) {
		t.Fatalf("lastSeenAt = %q, want %q", lastSeenAt, recordedAt.Format(time.RFC3339))
	}
	if cityName != city.Name {
		t.Fatalf("cityName = %q, want %q", cityName, city.Name)
	}
	if dist != 1.25 {
		t.Fatalf("dist = %v, want 1.25", dist)
	}
	if !isOpen {
		t.Fatal("expected station to be open")
	}
	if !diesel.Valid || diesel.Float64 != priceDiesel {
		t.Fatalf("diesel = %+v, want %v", diesel, priceDiesel)
	}
}

func TestGetOrCreateCityUsesCache(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	var requests atomic.Int32
	restore := stubDefaultTransport(t, func(req *http.Request) (*http.Response, error) {
		requests.Add(1)
		body := `[{"name":"Berlin","display_name":"Berlin, Deutschland","lat":"52.517389","lon":"13.395131"}]`
		return jsonResponse(http.StatusOK, body), nil
	})
	defer restore()

	city, cached, err := getOrCreateCity(ctx, db, "Berlin, Germany", "gasoline-test/1.0")
	if err != nil {
		t.Fatalf("first getOrCreateCity: %v", err)
	}
	if cached {
		t.Fatal("first lookup should not come from cache")
	}
	if city.DisplayName != "Berlin, Deutschland" {
		t.Fatalf("display name = %q", city.DisplayName)
	}
	if city.Name != "Berlin" {
		t.Fatalf("normalized name = %q", city.Name)
	}

	city, cached, err = getOrCreateCity(ctx, db, "Berlin, Germany", "gasoline-test/1.0")
	if err != nil {
		t.Fatalf("second getOrCreateCity: %v", err)
	}
	if !cached {
		t.Fatal("second lookup should come from cache")
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("geocoder requests = %d, want 1", got)
	}
	if city.Name != "Berlin" {
		t.Fatalf("cached normalized name = %q", city.Name)
	}
}

func TestRunCitiesSupportsJSONOutput(t *testing.T) {
	dbPath := seedFixtureDB(t)
	output := captureStdout(t, func() error {
		return run([]string{"cities", "--db", dbPath, "--output", "json"})
	})

	var cities []cityRow
	if err := json.Unmarshal([]byte(output), &cities); err != nil {
		t.Fatalf("unmarshal cities output: %v\noutput=%s", err, output)
	}
	if len(cities) != 1 {
		t.Fatalf("len(cities) = %d, want 1", len(cities))
	}
	if cities[0].Name != "Berlin" {
		t.Fatalf("city name = %q", cities[0].Name)
	}
}

func TestRunStationsSupportsShortJSONFlag(t *testing.T) {
	dbPath := seedFixtureDB(t)
	output := captureStdout(t, func() error {
		return run([]string{"stations", "--db", dbPath, "-o", "json"})
	})

	var stations []stationRow
	if err := json.Unmarshal([]byte(output), &stations); err != nil {
		t.Fatalf("unmarshal stations output: %v\noutput=%s", err, output)
	}
	if len(stations) != 1 {
		t.Fatalf("len(stations) = %d, want 1", len(stations))
	}
	if stations[0].ID != "station-1" {
		t.Fatalf("station id = %q", stations[0].ID)
	}
	if stations[0].Diesel == nil || *stations[0].Diesel != 1.659 {
		t.Fatalf("diesel = %v, want 1.659", stations[0].Diesel)
	}
}

func TestRunHistorySupportsJSONOutput(t *testing.T) {
	dbPath := seedFixtureDB(t)
	output := captureStdout(t, func() error {
		return run([]string{"history", "--db", dbPath, "--station-id", "station-1", "--fuel", "diesel", "--output", "json"})
	})

	var history []historyRow
	if err := json.Unmarshal([]byte(output), &history); err != nil {
		t.Fatalf("unmarshal history output: %v\noutput=%s", err, output)
	}
	if len(history) != 1 {
		t.Fatalf("len(history) = %d, want 1", len(history))
	}
	if history[0].Diesel == nil || *history[0].Diesel != 1.659 {
		t.Fatalf("diesel = %v, want 1.659", history[0].Diesel)
	}
	if history[0].E5 != nil || history[0].E10 != nil {
		t.Fatalf("expected only diesel field in filtered history row: %+v", history[0])
	}
}

func TestRunUpdateSupportsJSONOutput(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "update.db")
	t.Setenv(envAPIKeyName, "test-key")

	restore := stubDefaultTransport(t, func(req *http.Request) (*http.Response, error) {
		switch {
		case strings.HasPrefix(req.URL.String(), nominatimBaseURL):
			body := `[{"name":"Berlin","display_name":"Berlin, Deutschland","lat":"52.517389","lon":"13.395131"}]`
			return jsonResponse(http.StatusOK, body), nil
		case strings.HasPrefix(req.URL.String(), tankerKoenigBase+"/list.php"):
			body := `{"ok":true,"stations":[{"id":"station-1","name":"Test Station","brand":"ARAL","street":"Test Street","place":"Berlin","lat":52.5,"lng":13.4,"dist":1.25,"diesel":1.659,"e5":1.789,"e10":1.729,"isOpen":true,"houseNumber":"1","postCode":10115}]}`
			return jsonResponse(http.StatusOK, body), nil
		default:
			return nil, fmt.Errorf("unexpected request URL: %s", req.URL.String())
		}
	})
	defer restore()

	output := captureStdout(t, func() error {
		return run([]string{"update", "--db", dbPath, "--city", "Berlin, Germany", "--output", "json"})
	})

	var result updateResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("unmarshal update output: %v\noutput=%s", err, output)
	}
	if result.City.Name != "Berlin" {
		t.Fatalf("city name = %q", result.City.Name)
	}
	if result.StoredCount != 1 {
		t.Fatalf("stored_count = %d, want 1", result.StoredCount)
	}
}

func TestResolveOutputModeRejectsConflictingFlags(t *testing.T) {
	err := run([]string{"cities", "--db", filepath.Join(t.TempDir(), "test.db"), "--output", "txt", "-o", "json"})
	if err == nil || !strings.Contains(err.Error(), "--output and -o must match") {
		t.Fatalf("err = %v, want conflicting output flag error", err)
	}
}

func TestResolveDBPathUsesEnvVarWhenFlagUnset(t *testing.T) {
	t.Setenv(envDBPathName, "/tmp/from-env.db")

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath, "SQLite database file")
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("parse flags: %v", err)
	}

	if got := resolveDBPath(fs, *dbPath); got != "/tmp/from-env.db" {
		t.Fatalf("resolveDBPath = %q, want %q", got, "/tmp/from-env.db")
	}
}

func TestResolveDBPathPrefersFlagOverEnvVar(t *testing.T) {
	t.Setenv(envDBPathName, "/tmp/from-env.db")

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	dbPath := fs.String("db", defaultDBPath, "SQLite database file")
	if err := fs.Parse([]string{"--db", "/tmp/from-flag.db"}); err != nil {
		t.Fatalf("parse flags: %v", err)
	}

	if got := resolveDBPath(fs, *dbPath); got != "/tmp/from-flag.db" {
		t.Fatalf("resolveDBPath = %q, want %q", got, "/tmp/from-flag.db")
	}
}

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Close()
	})
	if err := initSchema(context.Background(), db); err != nil {
		t.Fatalf("initSchema: %v", err)
	}
	return db
}

func seedFixtureDB(t *testing.T) string {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "fixture.db")
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	if err := initSchema(ctx, db); err != nil {
		t.Fatalf("initSchema: %v", err)
	}

	city := cachedCity{
		QueryName:   "Berlin, Germany",
		Name:        "Berlin",
		DisplayName: "Berlin, Deutschland",
		Lat:         52.517389,
		Lng:         13.395131,
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO cities (name, normalized_name, display_name, lat, lng, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, city.QueryName, city.Name, city.DisplayName, city.Lat, city.Lng, "2026-04-02T09:00:00Z")
	if err != nil {
		t.Fatalf("insert city: %v", err)
	}

	diesel := 1.659
	e5 := 1.789
	e10 := 1.729
	stations := []tankerStation{{
		ID:          "station-1",
		Name:        "Test Station",
		Brand:       "ARAL",
		Street:      "Test Street",
		Place:       "Berlin",
		Lat:         52.5,
		Lng:         13.4,
		Dist:        1.25,
		Diesel:      &diesel,
		E5:          &e5,
		E10:         &e10,
		IsOpen:      true,
		HouseNumber: "1",
		PostCode:    10115,
	}}
	if err := persistUpdate(ctx, db, city, stations, time.Date(2026, 4, 2, 9, 15, 0, 0, time.UTC), 5); err != nil {
		t.Fatalf("persistUpdate: %v", err)
	}

	return dbPath
}

func captureStdout(t *testing.T, fn func() error) string {
	t.Helper()

	old := stdout
	var buf bytes.Buffer
	stdout = &buf
	t.Cleanup(func() {
		stdout = old
	})

	if err := fn(); err != nil {
		t.Fatalf("run: %v", err)
	}
	return buf.String()
}

func stubDefaultTransport(t *testing.T, fn func(*http.Request) (*http.Response, error)) func() {
	t.Helper()

	original := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(fn)
	http.DefaultClient.Transport = http.DefaultTransport

	return func() {
		http.DefaultTransport = original
		http.DefaultClient.Transport = original
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func jsonResponse(statusCode int, body string) *http.Response {
	return &http.Response{
		StatusCode: statusCode,
		Status:     fmt.Sprintf("%d %s", statusCode, http.StatusText(statusCode)),
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
