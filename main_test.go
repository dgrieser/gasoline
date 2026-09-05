package main

import (
	"archive/zip"
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
	"sort"
	"strconv"
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
		DisplayName: "Berlin",
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

	if err := persistUpdate(ctx, db, dialectSQLite, city, stations, recordedAt, 5); err != nil {
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
		isOpen     bool
		diesel     sql.NullFloat64
	)
	if err := db.QueryRowContext(ctx, `
		SELECT s.name, s.last_seen_at, ps.city_name, ps.is_open, ps.diesel
		FROM stations s
		JOIN price_snapshots ps ON ps.station_id = s.id
		WHERE s.id = ?
	`, "station-1").Scan(&name, &lastSeenAt, &cityName, &isOpen, &diesel); err != nil {
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
	if !isOpen {
		t.Fatal("expected station to be open")
	}
	if !diesel.Valid || diesel.Float64 != priceDiesel {
		t.Fatalf("diesel = %+v, want %v", diesel, priceDiesel)
	}
}

func TestPersistUpdateCompactsUnchangedSnapshots(t *testing.T) {
	t.Parallel()

	db := openTestDB(t)
	ctx := context.Background()
	city := cachedCity{
		QueryName:   "Luebbecke, Germany",
		Name:        "Lübbecke",
		DisplayName: "Lübbecke, Deutschland",
		Lat:         52.3027209,
		Lng:         8.6183054,
	}

	e5 := 2.189
	e10 := 2.149
	diesel := 2.349
	station := tankerStation{
		ID:          "station-1",
		Name:        "Test Station",
		Brand:       "ARAL",
		Street:      "Test Street",
		Place:       "Lübbecke",
		Lat:         52.3,
		Lng:         8.6,
		Dist:        4.60,
		Diesel:      &diesel,
		E5:          &e5,
		E10:         &e10,
		IsOpen:      true,
		HouseNumber: "1",
		PostCode:    32312,
	}

	times := []time.Time{
		time.Date(2026, 4, 7, 10, 20, 2, 0, time.UTC),
		time.Date(2026, 4, 7, 10, 25, 2, 0, time.UTC),
		time.Date(2026, 4, 7, 10, 30, 8, 0, time.UTC),
		time.Date(2026, 4, 7, 10, 35, 8, 0, time.UTC),
		time.Date(2026, 4, 7, 10, 40, 8, 0, time.UTC),
	}

	if err := persistUpdate(ctx, db, dialectSQLite, city, []tankerStation{station}, times[0], 5); err != nil {
		t.Fatalf("persist initial update: %v", err)
	}
	assertSnapshotCount(t, db, 1)
	assertLatestSnapshot(t, db, times[0].Format(time.RFC3339), 2.349)

	if err := persistUpdate(ctx, db, dialectSQLite, city, []tankerStation{station}, times[1], 5); err != nil {
		t.Fatalf("persist unchanged update: %v", err)
	}
	assertSnapshotCount(t, db, 1)
	assertLatestSnapshot(t, db, times[1].Format(time.RFC3339), 2.349)

	diesel = 2.389
	station.Diesel = &diesel
	if err := persistUpdate(ctx, db, dialectSQLite, city, []tankerStation{station}, times[2], 5); err != nil {
		t.Fatalf("persist changed update: %v", err)
	}
	assertSnapshotCount(t, db, 2)
	assertLatestSnapshot(t, db, times[2].Format(time.RFC3339), 2.389)

	if err := persistUpdate(ctx, db, dialectSQLite, city, []tankerStation{station}, times[3], 5); err != nil {
		t.Fatalf("persist first unchanged update after change: %v", err)
	}
	assertSnapshotCount(t, db, 3)
	assertLatestSnapshot(t, db, times[3].Format(time.RFC3339), 2.389)

	if err := persistUpdate(ctx, db, dialectSQLite, city, []tankerStation{station}, times[4], 5); err != nil {
		t.Fatalf("persist later unchanged update after change: %v", err)
	}
	assertSnapshotCount(t, db, 3)
	assertLatestSnapshot(t, db, times[4].Format(time.RFC3339), 2.389)

	rows, err := db.QueryContext(ctx, `
		SELECT recorded_at, diesel
		FROM price_snapshots
		WHERE station_id = ?
		ORDER BY recorded_at
	`, station.ID)
	if err != nil {
		t.Fatalf("query snapshots: %v", err)
	}
	defer rows.Close()

	var got []string
	for rows.Next() {
		var (
			recordedAt string
			diesel     float64
		)
		if err := rows.Scan(&recordedAt, &diesel); err != nil {
			t.Fatalf("scan snapshot: %v", err)
		}
		got = append(got, fmt.Sprintf("%s %.3f", recordedAt, diesel))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate snapshots: %v", err)
	}

	want := []string{
		"2026-04-07T10:25:02Z 2.349",
		"2026-04-07T10:30:08Z 2.389",
		"2026-04-07T10:40:08Z 2.389",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("snapshots =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestPersistUpdateIgnoresDistanceChangeButTracksOpenChange(t *testing.T) {
	t.Parallel()

	db := openTestDB(t)
	ctx := context.Background()
	city := cachedCity{
		QueryName:   "Berlin, Germany",
		Name:        "Berlin",
		DisplayName: "Berlin",
		Lat:         52.517389,
		Lng:         13.395131,
	}

	e5 := 1.789
	e10 := 1.729
	diesel := 1.659
	station := tankerStation{
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
	}

	first := time.Date(2026, 4, 7, 11, 0, 0, 0, time.UTC)
	second := time.Date(2026, 4, 7, 11, 5, 0, 0, time.UTC)
	third := time.Date(2026, 4, 7, 11, 10, 0, 0, time.UTC)

	if err := persistUpdate(ctx, db, dialectSQLite, city, []tankerStation{station}, first, 5); err != nil {
		t.Fatalf("persist first update: %v", err)
	}

	station.Dist = 9.99
	if err := persistUpdate(ctx, db, dialectSQLite, city, []tankerStation{station}, second, 5); err != nil {
		t.Fatalf("persist distance-only update: %v", err)
	}
	assertSnapshotCount(t, db, 1)

	station.IsOpen = false
	if err := persistUpdate(ctx, db, dialectSQLite, city, []tankerStation{station}, third, 5); err != nil {
		t.Fatalf("persist open change update: %v", err)
	}
	assertSnapshotCount(t, db, 2)
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
	if city.DisplayName != "Berlin" {
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

func TestGetOrCreateCityRefreshesLegacyNormalizedName(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	_, err := db.ExecContext(ctx, `
		INSERT INTO cities (name, normalized_name, display_name, lat, lng, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, "Luebbecke, Germany", "Luebbecke, Kreis Minden-Luebbecke, Nordrhein-Westfalen, 32312, Deutschland", "Luebbecke, Kreis Minden-Luebbecke, Nordrhein-Westfalen, 32312, Deutschland", 52.3027209, 8.6183054, "2026-04-03T20:00:00Z")
	if err != nil {
		t.Fatalf("insert legacy city: %v", err)
	}

	var requests atomic.Int32
	restore := stubDefaultTransport(t, func(req *http.Request) (*http.Response, error) {
		requests.Add(1)
		body := `[{"name":"Lübbecke","display_name":"Lübbecke, Kreis Minden-Lübbecke, Nordrhein-Westfalen, 32312, Deutschland","lat":"52.3027209","lon":"8.6183054"}]`
		return jsonResponse(http.StatusOK, body), nil
	})
	defer restore()

	city, cached, err := getOrCreateCity(ctx, db, "Luebbecke, Germany", "gasoline-test/1.0")
	if err != nil {
		t.Fatalf("getOrCreateCity: %v", err)
	}
	if cached {
		t.Fatal("legacy normalized_name row should be refreshed via geocoder")
	}
	if city.Name != "Lübbecke" {
		t.Fatalf("normalized name = %q", city.Name)
	}
	if city.DisplayName != "Lübbecke" {
		t.Fatalf("display name = %q", city.DisplayName)
	}

	var normalizedName string
	if err := db.QueryRowContext(ctx, `SELECT normalized_name FROM cities WHERE name = ?`, "Luebbecke, Germany").Scan(&normalizedName); err != nil {
		t.Fatalf("query normalized_name: %v", err)
	}
	if normalizedName != "Lübbecke" {
		t.Fatalf("stored normalized_name = %q", normalizedName)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("geocoder requests = %d, want 1", got)
	}
}

func TestGetOrCreateCityReusesCanonicalCityForAliasQuery(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	_, err := db.ExecContext(ctx, `
		INSERT INTO cities (name, normalized_name, display_name, lat, lng, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, "Lübbecke", "Lübbecke", "Lübbecke", 52.306990, 8.614230, "2026-04-10T13:48:51Z")
	if err != nil {
		t.Fatalf("insert canonical city: %v", err)
	}

	var requests atomic.Int32
	restore := stubDefaultTransport(t, func(req *http.Request) (*http.Response, error) {
		requests.Add(1)
		body := `[{"name":"Lübbecke","display_name":"Lübbecke, Kreis Minden-Lübbecke, Nordrhein-Westfalen, 32312, Deutschland","lat":"52.3027209","lon":"8.6183054"}]`
		return jsonResponse(http.StatusOK, body), nil
	})
	defer restore()

	city, cached, err := getOrCreateCity(ctx, db, "Luebbecke", "gasoline-test/1.0")
	if err != nil {
		t.Fatalf("getOrCreateCity: %v", err)
	}
	if cached {
		t.Fatal("alias lookup should geocode once and refresh canonical cache row")
	}
	if city.QueryName != "Lübbecke" {
		t.Fatalf("query name = %q, want canonical row key", city.QueryName)
	}
	if city.Name != "Lübbecke" {
		t.Fatalf("normalized name = %q", city.Name)
	}
	if city.DisplayName != "Lübbecke" {
		t.Fatalf("display name = %q", city.DisplayName)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("geocoder requests = %d, want 1", got)
	}

	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM cities`).Scan(&count); err != nil {
		t.Fatalf("count cities: %v", err)
	}
	if count != 1 {
		t.Fatalf("city count = %d, want 1", count)
	}

	var displayName string
	if err := db.QueryRowContext(ctx, `SELECT display_name FROM cities WHERE name = ?`, "Lübbecke").Scan(&displayName); err != nil {
		t.Fatalf("query canonical display_name: %v", err)
	}
	if displayName != city.DisplayName {
		t.Fatalf("stored display_name = %q, want %q", displayName, city.DisplayName)
	}
}

func TestRunListCitiesSupportsJSONOutput(t *testing.T) {
	dbPath := seedFixtureDB(t)
	output := captureStdout(t, func() error {
		return run([]string{"list", "cities", "--db", dbPath, "--output", "json"})
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

func TestRunListStationsSupportsShortJSONFlag(t *testing.T) {
	dbPath := seedFixtureDB(t)
	output := captureStdout(t, func() error {
		return run([]string{"list", "stations", "--db", dbPath, "-o", "json"})
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

func TestRunListStationsLimitZeroIsUnlimited(t *testing.T) {
	dbPath := seedFixtureDB(t)
	insertSecondFixtureStation(t, dbPath)

	output := captureStdout(t, func() error {
		return run([]string{"list", "stations", "--db", dbPath, "--limit", "0", "--output", "json"})
	})

	var stations []stationRow
	if err := json.Unmarshal([]byte(output), &stations); err != nil {
		t.Fatalf("unmarshal stations output: %v\noutput=%s", err, output)
	}
	if len(stations) != 2 {
		t.Fatalf("len(stations) = %d, want 2", len(stations))
	}
}

func TestRunListHistorySupportsJSONOutput(t *testing.T) {
	dbPath := seedFixtureDB(t)
	output := captureStdout(t, func() error {
		return run([]string{"list", "history", "--db", dbPath, "--station-id", "station-1", "--fuel", "diesel", "--output", "json"})
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

func TestRunListHistoryAllowsMissingStationID(t *testing.T) {
	dbPath := seedFixtureDB(t)
	insertSecondFixtureStation(t, dbPath)

	output := captureStdout(t, func() error {
		return run([]string{"list", "history", "--db", dbPath, "--limit", "0", "--output", "json"})
	})

	var history []historyRow
	if err := json.Unmarshal([]byte(output), &history); err != nil {
		t.Fatalf("unmarshal history output: %v\noutput=%s", err, output)
	}
	if len(history) != 2 {
		t.Fatalf("len(history) = %d, want 2", len(history))
	}
	if history[0].StationID != "station-2" || history[0].StationName != "Other Station" {
		t.Fatalf("latest station = %q/%q, want station-2/Other Station", history[0].StationID, history[0].StationName)
	}
	if history[1].StationID != "station-1" || history[1].StationName != "Test Station" {
		t.Fatalf("older station = %q/%q, want station-1/Test Station", history[1].StationID, history[1].StationName)
	}
}

func TestRunCheckSupportsJSONOutput(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "check.db")
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	ctx := context.Background()
	if err := initSchema(ctx, db, dialectSQLite); err != nil {
		t.Fatalf("initSchema: %v", err)
	}
	city := cachedCity{
		QueryName:   "Berlin",
		Name:        "Berlin",
		DisplayName: "Berlin",
		Lat:         52.517389,
		Lng:         13.395131,
	}
	insertSuggestCity(t, db, city)
	insertSuggestStation(t, db, "station-1", "Station 1", 52.517389, 13.395131)

	nowLocal := time.Now().In(time.Local)
	for daysAgo := 6; daysAgo >= 1; daysAgo-- {
		dayStart := localDayStart(nowLocal).AddDate(0, 0, -daysAgo)
		for hour := 0; hour < 24; hour++ {
			insertSuggestSnapshot(t, db, "station-1", "Berlin", dayStart.Add(time.Duration(hour)*time.Hour).In(time.UTC), 2.100, true)
		}
	}
	insertSuggestSnapshot(t, db, "station-1", "Berlin", time.Now().UTC(), 2.000, true)
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	output := captureStdout(t, func() error {
		return run([]string{"check", "--db", dbPath, "--output", "json"})
	})

	var results []fuelCheckResult
	if err := json.Unmarshal([]byte(output), &results); err != nil {
		t.Fatalf("unmarshal check output: %v\noutput=%s", err, output)
	}
	if len(results) != len(suggestFuels) || results[0].Fuel != "diesel" {
		t.Fatalf("results = %+v, want one per fuel starting with diesel", results)
	}
	checks := results[0].Checks
	if len(checks) != 1 {
		t.Fatalf("len(checks) = %d, want 1", len(checks))
	}
	if checks[0].StationID != "station-1" || checks[0].Station.ID != "station-1" {
		t.Fatalf("station fields = %+v, want station-1", checks[0])
	}
	if checks[0].Station.City != "Berlin" {
		t.Fatalf("station city = %q, want the owning update target Berlin", checks[0].Station.City)
	}
	if checks[0].Recommendation != "buy" {
		t.Fatalf("recommendation = %q, want buy", checks[0].Recommendation)
	}
}

func TestRunCheckCoversEveryFuelAndEveryFedStation(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "check-multi.db")
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	ctx := context.Background()
	if err := initSchema(ctx, db, dialectSQLite); err != nil {
		t.Fatalf("initSchema: %v", err)
	}
	insertSuggestCity(t, db, cachedCity{QueryName: "Berlin", Name: "Berlin", DisplayName: "Berlin", Lat: 52.517389, Lng: 13.395131})
	insertSuggestCity(t, db, cachedCity{QueryName: "Hamburg", Name: "Hamburg", DisplayName: "Hamburg", Lat: 53.550556, Lng: 9.993333})
	insertSuggestStation(t, db, "station-b", "Station B", 52.517389, 13.395131)
	insertSuggestStation(t, db, "station-h", "Station H", 53.550556, 9.993333)

	nowLocal := time.Now().In(time.Local)
	for daysAgo := 6; daysAgo >= 1; daysAgo-- {
		dayStart := localDayStart(nowLocal).AddDate(0, 0, -daysAgo)
		for hour := 0; hour < 24; hour++ {
			at := dayStart.Add(time.Duration(hour) * time.Hour).In(time.UTC)
			insertSuggestSnapshot(t, db, "station-b", "Berlin", at, 2.100, true)
			insertSuggestSnapshot(t, db, "station-h", "Hamburg", at, 1.900, true)
		}
	}
	insertSuggestSnapshot(t, db, "station-b", "Berlin", time.Now().UTC(), 2.000, true)
	insertSuggestSnapshot(t, db, "station-h", "Hamburg", time.Now().UTC(), 1.800, true)
	insertUpdateTargetRow(t, db, "Berlin", 5)
	insertUpdateTargetRow(t, db, "Hamburg", 5)
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	output := captureStdout(t, func() error {
		return run([]string{"check", "--db", dbPath, "--output", "json"})
	})
	var results []fuelCheckResult
	if err := json.Unmarshal([]byte(output), &results); err != nil {
		t.Fatalf("unmarshal check output: %v\noutput=%s", err, output)
	}
	if len(results) != len(suggestFuels) {
		t.Fatalf("got %d fuel results, want %d", len(results), len(suggestFuels))
	}
	// One run per fuel, and each covers both cities' stations: there is no
	// radius and no per-city fan-out any more, so the two stations appear
	// together with the city each one is attributed to.
	for i, wantFuel := range suggestFuels {
		if results[i].Fuel != wantFuel {
			t.Fatalf("results[%d].Fuel = %q, want %q", i, results[i].Fuel, wantFuel)
		}
		if results[i].Error != "" {
			t.Fatalf("results[%d] unexpected error: %s", i, results[i].Error)
		}
		cities := map[string]string{}
		for _, row := range results[i].Checks {
			cities[row.StationID] = row.Station.City
		}
		if cities["station-b"] != "Berlin" || cities["station-h"] != "Hamburg" {
			t.Fatalf("results[%d] station cities = %+v, want station-b in Berlin and station-h in Hamburg", i, cities)
		}
	}
}

func TestRunCheckIsBestEffortAcrossFuels(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "check-besteffort.db")
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	ctx := context.Background()
	if err := initSchema(ctx, db, dialectSQLite); err != nil {
		t.Fatalf("initSchema: %v", err)
	}
	insertSuggestCity(t, db, cachedCity{QueryName: "Berlin", Name: "Berlin", DisplayName: "Berlin", Lat: 52.517389, Lng: 13.395131})
	insertSuggestStation(t, db, "station-b", "Station B", 52.517389, 13.395131)
	nowLocal := time.Now().In(time.Local)
	for daysAgo := 6; daysAgo >= 1; daysAgo-- {
		dayStart := localDayStart(nowLocal).AddDate(0, 0, -daysAgo)
		for hour := 0; hour < 24; hour++ {
			at := dayStart.Add(time.Duration(hour) * time.Hour).In(time.UTC)
			insertSuggestSnapshot(t, db, "station-b", "Berlin", at, 2.100, true)
		}
	}
	insertSuggestSnapshotDieselOnly(t, db, "station-b", "Berlin", time.Now().UTC(), 2.000, true)
	insertUpdateTargetRow(t, db, "Berlin", 5)
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	old := stdout
	var buf bytes.Buffer
	stdout = &buf
	runErr := run([]string{"check", "--db", dbPath, "--output", "json"})
	stdout = old
	if runErr == nil || !strings.Contains(runErr.Error(), "2 of 3 fuels failed") {
		t.Fatalf("run error = %v, want '2 of 3 fuels failed'", runErr)
	}
	var results []fuelCheckResult
	if err := json.Unmarshal(buf.Bytes(), &results); err != nil {
		t.Fatalf("unmarshal best-effort check output: %v\noutput=%s", err, buf.String())
	}
	if len(results) != 3 {
		t.Fatalf("got %d fuel results, want 3", len(results))
	}
	// Only diesel has stored prices: the other two fuels fail without taking
	// the run down with them.
	if results[0].Fuel != "diesel" || results[0].Error != "" || len(results[0].Checks) != 1 {
		t.Fatalf("results[0] = %+v, want one diesel row without error", results[0])
	}
	for _, res := range results[1:] {
		if res.Error == "" {
			t.Fatalf("%s unexpectedly succeeded without stored prices", res.Fuel)
		}
		if res.Checks != nil {
			t.Fatalf("%s checks = %+v, want null for a failed fuel", res.Fuel, res.Checks)
		}
	}
}

func TestSuggestGasCoversEveryFedStationRegardlessOfDistance(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	city := cachedCity{
		QueryName:   "Berlin",
		Name:        "Berlin",
		DisplayName: "Berlin",
		Lat:         52.517389,
		Lng:         13.395131,
	}
	insertSuggestCity(t, db, city)
	insertSuggestStation(t, db, "near-station", "Near Station", 52.517389, 13.395131)
	// Roughly 110 km from the city centre. There is no radius any more: being
	// fed is what puts a station in scope, so the cheaper far station wins.
	insertSuggestStation(t, db, "far-station", "Far Station", 53.500000, 13.395131)

	for day := 20; day <= 25; day++ {
		insertSuggestSnapshot(t, db, "near-station", "Berlin", time.Date(2026, 4, day, 17, 0, 0, 0, time.UTC), 2.200, true)
		insertSuggestSnapshot(t, db, "near-station", "Berlin", time.Date(2026, 4, day, 18, 0, 0, 0, time.UTC), 2.000, true)
		insertSuggestSnapshot(t, db, "near-station", "Berlin", time.Date(2026, 4, day, 19, 0, 0, 0, time.UTC), 2.200, true)
		insertSuggestSnapshot(t, db, "far-station", "Berlin", time.Date(2026, 4, day, 18, 0, 0, 0, time.UTC), 1.500, true)
	}

	suggestions, err := suggestGas(ctx, db, suggestOptions{
		Fuel:        "diesel",
		HistoryDays: 10,
		PredictDays: 2,
		LimitPerDay: 1,
		Now:         time.Date(2026, 4, 26, 15, 30, 0, 0, time.UTC),
		Location:    time.UTC,
	})
	if err != nil {
		t.Fatalf("suggestGas: %v", err)
	}
	if len(suggestions) != 2 {
		t.Fatalf("len(suggestions) = %d, want 2: %+v", len(suggestions), suggestions)
	}
	for _, suggestion := range suggestions {
		// The far station is the cheapest, and distance no longer excludes it.
		if suggestion.StationID != "far-station" {
			t.Fatalf("station id = %q, want the cheapest fed station far-station", suggestion.StationID)
		}
		if suggestion.DistanceKM < 100 {
			t.Fatalf("distance = %.1f, want the far station's real distance to the owning city", suggestion.DistanceKM)
		}
		if suggestion.Station.City != "Berlin" {
			t.Fatalf("station city = %q, want the owning update target Berlin", suggestion.Station.City)
		}
		if suggestion.Station.Address != "Test Street 1, 10115 Berlin" {
			t.Fatalf("station address = %q, want formatted address", suggestion.Station.Address)
		}
		if suggestion.Station.Brand != "TEST" || suggestion.Station.Street != "Test Street" || suggestion.Station.HouseNumber != "1" || suggestion.Station.PostCode != 10115 || suggestion.Station.Place != "Berlin" {
			t.Fatalf("station metadata = %+v, want persisted station details", suggestion.Station)
		}
		if suggestion.PredictedPrice >= 2.000 {
			t.Fatalf("predicted price = %.3f, want the far station's cheaper level", suggestion.PredictedPrice)
		}
	}
	if suggestions[0].Date != "2026-04-26" || suggestions[0].Weekday != "Sunday" {
		t.Fatalf("first suggestion date = %s/%s, want 2026-04-26/Sunday", suggestions[0].Date, suggestions[0].Weekday)
	}
	if suggestions[1].Date != "2026-04-27" || suggestions[1].Weekday != "Monday" {
		t.Fatalf("second suggestion date = %s/%s, want 2026-04-27/Monday", suggestions[1].Date, suggestions[1].Weekday)
	}
}

func TestReconstructPriceIntervalsClipsAndSkipsUnavailablePrices(t *testing.T) {
	historyStart := time.Date(2026, 4, 11, 0, 0, 0, 0, time.UTC)
	now := time.Date(2026, 4, 13, 0, 0, 0, 0, time.UTC)
	snapshots := []suggestSnapshot{
		{
			StationID:   "station-1",
			StationName: "Station 1",
			DistanceKM:  1,
			RecordedAt:  time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
			IsOpen:      true,
			Price:       sql.NullFloat64{Float64: 2.000, Valid: true},
		},
		{
			StationID:   "station-1",
			StationName: "Station 1",
			DistanceKM:  1,
			RecordedAt:  time.Date(2026, 4, 11, 12, 0, 0, 0, time.UTC),
			IsOpen:      false,
			Price:       sql.NullFloat64{Float64: 2.100, Valid: true},
		},
		{
			StationID:   "station-1",
			StationName: "Station 1",
			DistanceKM:  1,
			RecordedAt:  time.Date(2026, 4, 11, 18, 0, 0, 0, time.UTC),
			IsOpen:      true,
			Price:       sql.NullFloat64{},
		},
		{
			StationID:   "station-1",
			StationName: "Station 1",
			DistanceKM:  1,
			RecordedAt:  time.Date(2026, 4, 12, 0, 0, 0, 0, time.UTC),
			IsOpen:      true,
			Price:       sql.NullFloat64{Float64: 2.200, Valid: true},
		},
	}

	intervals := reconstructPriceIntervals(snapshots, historyStart, now)
	if len(intervals) != 2 {
		t.Fatalf("len(intervals) = %d, want 2: %+v", len(intervals), intervals)
	}
	if !intervals[0].Start.Equal(historyStart) || !intervals[0].End.Equal(time.Date(2026, 4, 11, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("first interval = %s-%s, want clipped history start to closed snapshot", intervals[0].Start, intervals[0].End)
	}
	if intervals[0].Price != 2.000 {
		t.Fatalf("first interval price = %.3f, want 2.000", intervals[0].Price)
	}
	if !intervals[1].Start.Equal(time.Date(2026, 4, 12, 0, 0, 0, 0, time.UTC)) || !intervals[1].End.Equal(now) {
		t.Fatalf("second interval = %s-%s, want 2026-04-12 to now", intervals[1].Start, intervals[1].End)
	}
}

func TestWeightedMedianPriceUsesSampleWeights(t *testing.T) {
	got, ok := weightedMedianPrice([]priceSample{
		{Price: 1.900, Weight: 1},
		{Price: 2.000, Weight: 1},
		{Price: 2.100, Weight: 10},
	})
	if !ok {
		t.Fatal("weightedMedianPrice returned !ok")
	}
	if got != 2.100 {
		t.Fatalf("weighted median = %.3f, want 2.100", got)
	}
}

func TestGenerateSuggestionsStartsTomorrowWhenTodayHasNoFutureHours(t *testing.T) {
	model := forecastModel{
		Stations: map[string]forecastStation{
			"station-1": {Station: suggestionStationRow{ID: "station-1", Name: "Station 1", DistanceKM: 1.2}},
		},
		WeekdayHour: make(map[stationWeekdayHourKey][]priceSample),
		Hour: map[stationHourKey][]priceSample{
			{StationID: "station-1", Hour: 0}: {{Price: 2.000, Weight: 1, Date: "2026-04-20"}},
		},
		Recent: map[string][]priceSample{
			"station-1": {{Price: 2.000, Weight: 1, Date: "2026-04-20"}},
		},
	}

	suggestions := generateSuggestions(model, "diesel", time.Date(2026, 4, 26, 23, 30, 0, 0, time.UTC), time.UTC, 1, 1)
	if len(suggestions) != 1 {
		t.Fatalf("len(suggestions) = %d, want 1", len(suggestions))
	}
	if suggestions[0].Date != "2026-04-27" || suggestions[0].StartTime != "00:00" {
		t.Fatalf("suggestion = %s %s, want 2026-04-27 00:00", suggestions[0].Date, suggestions[0].StartTime)
	}
}

func TestMergeSuggestions(t *testing.T) {
	stationA := suggestionStationRow{ID: "station-a", Name: "Station A"}
	stationB := suggestionStationRow{ID: "station-b", Name: "Station B"}
	input := []suggestionRow{
		{Date: "2026-04-27", StationID: "station-a", Station: stationA, StartTime: "18:00", EndTime: "19:00", PredictedPrice: 2.115, Confidence: "medium", SampleCount: 5},
		{Date: "2026-04-27", StationID: "station-b", Station: stationB, StartTime: "19:00", EndTime: "20:00", PredictedPrice: 2.100, Confidence: "high", SampleCount: 8},
		{Date: "2026-04-27", StationID: "station-a", Station: stationA, StartTime: "20:00", EndTime: "21:00", PredictedPrice: 2.115, Confidence: "medium", SampleCount: 3},
		{Date: "2026-04-27", StationID: "station-a", Station: stationA, StartTime: "22:00", EndTime: "23:00", PredictedPrice: 2.115, Confidence: "medium", SampleCount: 3},
	}

	got := mergeSuggestions(input)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2: %+v", len(got), got)
	}

	a := got[0]
	if a.StationID != "station-a" {
		t.Fatalf("first entry station = %q, want station-a", a.StationID)
	}
	if a.StartTime != "18:00" || a.EndTime != "23:00" {
		t.Fatalf("station-a window = %s-%s, want 18:00-23:00", a.StartTime, a.EndTime)
	}
	if a.SampleCount != 5 {
		t.Fatalf("station-a SampleCount = %d, want 5 (max)", a.SampleCount)
	}

	b := got[1]
	if b.StationID != "station-b" {
		t.Fatalf("second entry station = %q, want station-b", b.StationID)
	}
	if b.StartTime != "19:00" || b.EndTime != "20:00" {
		t.Fatalf("station-b window = %s-%s, want 19:00-20:00", b.StartTime, b.EndTime)
	}
}

func TestCheckGasRecommendsBuyForLowCurrentPrice(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	city := cachedCity{
		QueryName:   "Berlin",
		Name:        "Berlin",
		DisplayName: "Berlin",
		Lat:         52.517389,
		Lng:         13.395131,
	}
	insertSuggestCity(t, db, city)
	insertSuggestStation(t, db, "station-1", "Station 1", 52.517389, 13.395131)

	for day := 20; day <= 25; day++ {
		insertSuggestSnapshot(t, db, "station-1", "Berlin", time.Date(2026, 4, day, 15, 0, 0, 0, time.UTC), 2.200, true)
		insertSuggestSnapshot(t, db, "station-1", "Berlin", time.Date(2026, 4, day, 16, 0, 0, 0, time.UTC), 2.300, true)
	}
	insertSuggestSnapshot(t, db, "station-1", "Berlin", time.Date(2026, 4, 26, 14, 0, 0, 0, time.UTC), 2.000, true)

	checks, err := checkGas(ctx, db, checkOptions{
		Fuel:        "diesel",
		HistoryDays: 10,
		PredictDays: 1,
		Limit:       5,
		Now:         time.Date(2026, 4, 26, 15, 30, 0, 0, time.UTC),
		Location:    time.UTC,
	})
	if err != nil {
		t.Fatalf("checkGas: %v", err)
	}
	if len(checks) != 1 {
		t.Fatalf("len(checks) = %d, want 1: %+v", len(checks), checks)
	}
	check := checks[0]
	if check.Recommendation != "buy" || check.Verdict != "low" {
		t.Fatalf("recommendation/verdict = %s/%s, want buy/low", check.Recommendation, check.Verdict)
	}
	if check.ExpectedLower {
		t.Fatal("expected no lower future forecast")
	}
	if check.Station.Address != "Test Street 1, 10115 Berlin" {
		t.Fatalf("station address = %q, want formatted address", check.Station.Address)
	}
}

func TestCheckGasRecommendsWaitForLowerFuturePrice(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	city := cachedCity{
		QueryName:   "Berlin",
		Name:        "Berlin",
		DisplayName: "Berlin",
		Lat:         52.517389,
		Lng:         13.395131,
	}
	insertSuggestCity(t, db, city)
	insertSuggestStation(t, db, "station-1", "Station 1", 52.517389, 13.395131)

	for day := 20; day <= 25; day++ {
		insertSuggestSnapshot(t, db, "station-1", "Berlin", time.Date(2026, 4, day, 15, 0, 0, 0, time.UTC), 2.200, true)
		insertSuggestSnapshot(t, db, "station-1", "Berlin", time.Date(2026, 4, day, 18, 0, 0, 0, time.UTC), 2.000, true)
		insertSuggestSnapshot(t, db, "station-1", "Berlin", time.Date(2026, 4, day, 19, 0, 0, 0, time.UTC), 2.200, true)
	}
	insertSuggestSnapshot(t, db, "station-1", "Berlin", time.Date(2026, 4, 26, 15, 0, 0, 0, time.UTC), 2.200, true)

	checks, err := checkGas(ctx, db, checkOptions{
		Fuel:        "diesel",
		HistoryDays: 10,
		PredictDays: 1,
		Limit:       5,
		Now:         time.Date(2026, 4, 26, 15, 30, 0, 0, time.UTC),
		Location:    time.UTC,
	})
	if err != nil {
		t.Fatalf("checkGas: %v", err)
	}
	if len(checks) != 1 {
		t.Fatalf("len(checks) = %d, want 1: %+v", len(checks), checks)
	}
	check := checks[0]
	if check.Recommendation != "wait" || !check.ExpectedLower {
		t.Fatalf("recommendation/expected_lower = %s/%t, want wait/true", check.Recommendation, check.ExpectedLower)
	}
	if check.BestFutureStartTime != "18:00" || check.BestFutureEndTime != "19:00" {
		t.Fatalf("future window = %s-%s, want 18:00-19:00", check.BestFutureStartTime, check.BestFutureEndTime)
	}
	if check.ExpectedDrop < 0.140 {
		t.Fatalf("expected_drop = %.3f, want modeled drop below current price", check.ExpectedDrop)
	}
}

func TestValidateCheckOptions(t *testing.T) {
	valid := checkOptions{
		Fuel:        "diesel",
		HistoryDays: 21,
		PredictDays: 3,
		Limit:       5,
	}
	if err := validateCheckOptions(valid); err != nil {
		t.Fatalf("validateCheckOptions valid: %v", err)
	}

	cases := []struct {
		name string
		opts checkOptions
		want string
	}{
		{name: "fuel", opts: checkOptions{Fuel: "premium", HistoryDays: 21, PredictDays: 3, Limit: 5}, want: "fuel must be one of"},
		{name: "history", opts: checkOptions{Fuel: "diesel", HistoryDays: 0, PredictDays: 3, Limit: 5}, want: "history days"},
		{name: "limit", opts: checkOptions{Fuel: "diesel", HistoryDays: 21, PredictDays: 3, Limit: -1}, want: "check row limit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateCheckOptions(tc.opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func insertSecondFixtureStation(t *testing.T, dbPath string) {
	t.Helper()

	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO stations (
			id, name, brand, street, house_number, post_code, place, lat, lng, first_seen_at, last_seen_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, "station-2", "Other Station", "ESSO", "Other Street", "2", 10115, "Berlin", 52.6, 13.5, "2026-04-02T10:15:00Z", "2026-04-02T10:15:00Z"); err != nil {
		t.Fatalf("insert station: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO price_snapshots (
			station_id, city_name, recorded_at, search_radius_km, is_open, e5, e10, diesel
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, "station-2", "Berlin", "2026-04-02T10:15:00Z", 5, 1, 1.809, 1.749, 1.679); err != nil {
		t.Fatalf("insert snapshot: %v", err)
	}
}

func insertSuggestCity(t *testing.T, db *sql.DB, city cachedCity) {
	t.Helper()

	_, err := db.ExecContext(context.Background(), `
		INSERT INTO cities (name, normalized_name, display_name, lat, lng, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, city.QueryName, city.Name, city.DisplayName, city.Lat, city.Lng, "2026-04-20T00:00:00Z")
	if err != nil {
		t.Fatalf("insert city: %v", err)
	}
}

func insertSuggestStation(t *testing.T, db *sql.DB, id, name string, lat, lng float64) {
	t.Helper()

	_, err := db.ExecContext(context.Background(), `
		INSERT INTO stations (
			id, name, brand, street, house_number, post_code, place, lat, lng, first_seen_at, last_seen_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, id, name, "TEST", "Test Street", "1", 10115, "Berlin", lat, lng, "2026-04-20T00:00:00Z", "2026-04-25T19:00:00Z")
	if err != nil {
		t.Fatalf("insert station %q: %v", id, err)
	}
}

func insertSuggestSnapshot(t *testing.T, db *sql.DB, stationID, cityName string, recordedAt time.Time, diesel float64, isOpen bool) {
	t.Helper()

	e5 := diesel + 0.080
	e10 := diesel + 0.020
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO price_snapshots (
			station_id, city_name, recorded_at, search_radius_km, is_open, e5, e10, diesel
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, stationID, cityName, recordedAt.Format(time.RFC3339), 5, boolToInt(isOpen), e5, e10, diesel)
	if err != nil {
		t.Fatalf("insert snapshot %q at %s: %v", stationID, recordedAt.Format(time.RFC3339), err)
	}
}

// insertSuggestSnapshotDieselOnly stores a snapshot with no e5/e10 price, so a
// fixture can give one fuel history and leave the others without any.
func insertSuggestSnapshotDieselOnly(t *testing.T, db *sql.DB, stationID, cityName string, recordedAt time.Time, diesel float64, isOpen bool) {
	t.Helper()

	_, err := db.ExecContext(context.Background(), `
		INSERT INTO price_snapshots (
			station_id, city_name, recorded_at, search_radius_km, is_open, e5, e10, diesel
		) VALUES (?, ?, ?, ?, ?, NULL, NULL, ?)
	`, stationID, cityName, recordedAt.Format(time.RFC3339), 5, boolToInt(isOpen), diesel)
	if err != nil {
		t.Fatalf("insert diesel-only snapshot %q at %s: %v", stationID, recordedAt.Format(time.RFC3339), err)
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

func sameCityQueries(a, b []cityQuery) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestBuildCityQueries(t *testing.T) {
	c := func(name string) updateArg { return updateArg{kind: argCity, city: name} }
	r := func(v float64) updateArg { return updateArg{kind: argRadius, radius: v} }

	cases := []struct {
		name   string
		events []updateArg
		want   []cityQuery
	}{
		// The six precedence examples from the feature request, verbatim.
		{"example1_per_city", []updateArg{c("Luebbecke"), r(25), c("Berlin"), r(5)},
			[]cityQuery{{"Luebbecke", 25}, {"Berlin", 5}}},
		{"example2_global_before", []updateArg{r(5), c("Luebbecke"), c("Berlin")},
			[]cityQuery{{"Luebbecke", 5}, {"Berlin", 5}}},
		{"example3_trailing_per_city", []updateArg{r(5), c("Luebbecke"), c("Berlin"), c("Pforzheim"), r(25)},
			[]cityQuery{{"Luebbecke", 5}, {"Berlin", 5}, {"Pforzheim", 25}}},
		{"example4_all_overridden", []updateArg{r(5), c("Luebbecke"), r(24), c("Berlin"), r(23), c("Pforzheim"), r(22)},
			[]cityQuery{{"Luebbecke", 24}, {"Berlin", 23}, {"Pforzheim", 22}}},
		{"example5_no_propagate", []updateArg{r(5), c("Luebbecke"), c("Berlin"), c("Pforzheim"), r(25), c("Enzberg")},
			[]cityQuery{{"Luebbecke", 5}, {"Berlin", 5}, {"Pforzheim", 25}, {"Enzberg", 5}}},
		{"example6_no_global_default", []updateArg{c("Luebbecke"), r(25), c("Berlin"), c("Pforzheim"), r(26), c("Enzberg")},
			[]cityQuery{{"Luebbecke", 25}, {"Berlin", 5}, {"Pforzheim", 26}, {"Enzberg", 5}}},
		// Edge cases.
		{"trailing_radius_single", []updateArg{c("A"), r(7)}, []cityQuery{{"A", 7}}},
		{"two_radii_one_slot_last_wins", []updateArg{c("A"), r(1), r(2)}, []cityQuery{{"A", 2}}},
		{"two_leading_radii_last_wins", []updateArg{r(1), r(2), c("A")}, []cityQuery{{"A", 2}}},
		{"radius_only_no_city", []updateArg{r(5)}, nil},
		{"no_radius_uses_default", []updateArg{c("A"), c("B")}, []cityQuery{{"A", defaultRadiusKm}, {"B", defaultRadiusKm}}},
		{"empty", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildCityQueries(tc.events)
			if !sameCityQueries(got, tc.want) {
				t.Fatalf("buildCityQueries = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestUpdateFlagParseOrder(t *testing.T) {
	parse := func(args ...string) ([]cityQuery, error) {
		var events []updateArg
		fs := flag.NewFlagSet("update", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		fs.Var(cityFlag{&events}, "city", "")
		fs.Var(radiusFlag{&events}, "radius", "")
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		return buildCityQueries(events), nil
	}

	// Space-separated form preserves left-to-right interleave order.
	got, err := parse("--city", "Luebbecke", "--radius", "25", "--city", "Berlin", "--radius", "5")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if want := []cityQuery{{"Luebbecke", 25}, {"Berlin", 5}}; !sameCityQueries(got, want) {
		t.Fatalf("space form = %+v, want %+v", got, want)
	}

	// --flag=value form, with a leading global radius.
	got, err = parse("--radius=10", "--city=Berlin", "--city=Pforzheim", "--radius=25")
	if err != nil {
		t.Fatalf("parse =form: %v", err)
	}
	if want := []cityQuery{{"Berlin", 10}, {"Pforzheim", 25}}; !sameCityQueries(got, want) {
		t.Fatalf("=form = %+v, want %+v", got, want)
	}

	// A non-numeric radius is rejected at parse time.
	if _, err := parse("--city", "Berlin", "--radius", "abc"); err == nil {
		t.Fatalf("expected error for non-numeric --radius")
	}
}

func TestValidateCityQueries(t *testing.T) {
	if err := validateCityQueries([]cityQuery{{"Berlin", 5}, {"Pforzheim", 25}}); err != nil {
		t.Fatalf("valid: %v", err)
	}

	// The widest radius the tiled fetch covers.
	if err := validateCityQueries([]cityQuery{{"Berlin", maxRequestRadiusKM}}); err != nil {
		t.Fatalf("max radius: %v", err)
	}

	// Names are trimmed in place.
	qs := []cityQuery{{"  Berlin  ", 5}}
	if err := validateCityQueries(qs); err != nil {
		t.Fatalf("trim valid: %v", err)
	}
	if qs[0].name != "Berlin" {
		t.Fatalf("name not trimmed: %q", qs[0].name)
	}

	wantRange := fmt.Sprintf("> 0 and <= %.0f", maxRequestRadiusKM)
	cases := []struct {
		name string
		qs   []cityQuery
		want string
	}{
		{"empty", nil, "requires --city"},
		{"empty_name", []cityQuery{{"   ", 5}}, "must not be empty"},
		{"radius_zero", []cityQuery{{"Berlin", 0}}, wantRange},
		{"radius_too_big", []cityQuery{{"Berlin", maxRequestRadiusKM + 1}}, wantRange},
		{"duplicate", []cityQuery{{"Berlin", 5}, {"Berlin", 10}}, "given more than once"},
		{"duplicate_case_insensitive", []cityQuery{{"Berlin", 5}, {"berlin", 10}}, "given more than once"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateCityQueries(tc.qs)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestRunUpdateMultiCity(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "multi.db")
	t.Setenv(envAPIKeyName, "test-key")

	radByLat := map[string]string{}
	restore := stubDefaultTransport(t, func(req *http.Request) (*http.Response, error) {
		u := req.URL
		switch {
		case strings.HasPrefix(u.String(), nominatimBaseURL):
			switch q := u.Query().Get("q"); {
			case strings.Contains(q, "Berlin"):
				return jsonResponse(http.StatusOK, `[{"name":"Berlin","display_name":"Berlin, DE","lat":"52.500000","lon":"13.400000"}]`), nil
			case strings.Contains(q, "Pforzheim"):
				return jsonResponse(http.StatusOK, `[{"name":"Pforzheim","display_name":"Pforzheim, DE","lat":"48.900000","lon":"8.700000"}]`), nil
			default:
				return nil, fmt.Errorf("unexpected geocode q: %s", q)
			}
		case strings.HasPrefix(u.String(), tankerKoenigBase+"/list.php"):
			lat := u.Query().Get("lat")
			radByLat[lat] = u.Query().Get("rad")
			// Distinct, non-overlapping stations: this test is about radius
			// precedence, not de-duplication (see TestRunUpdateMultiCityOverlap).
			body := fmt.Sprintf(`{"ok":true,"stations":[{"id":"s-%s","name":"S","brand":"B","street":"St","place":"P","lat":%s,"lng":2,"dist":1,"diesel":1.5,"e5":1.7,"e10":1.6,"isOpen":true,"houseNumber":"1","postCode":1}]}`, lat, lat)
			return jsonResponse(http.StatusOK, body), nil
		default:
			return nil, fmt.Errorf("unexpected URL: %s", u.String())
		}
	})
	defer restore()

	// --radius 10 (global) --city Berlin --city Pforzheim --radius 25 => Berlin=10, Pforzheim=25.
	output := captureStdout(t, func() error {
		return run([]string{"update", "--db", dbPath, "--radius", "10", "--city", "Berlin", "--city", "Pforzheim", "--radius", "25", "--output", "json"})
	})

	var result multiUpdateResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("unmarshal: %v\noutput=%s", err, output)
	}
	if len(result.Results) != 2 {
		t.Fatalf("results = %d, want 2", len(result.Results))
	}
	if result.StoredCount != 2 {
		t.Fatalf("stored_count = %d, want 2", result.StoredCount)
	}
	byCity := map[string]cityUpdateResult{}
	for _, r := range result.Results {
		byCity[r.City.Name] = r
	}
	if byCity["Berlin"].RadiusKm != 10 {
		t.Fatalf("Berlin radius_km = %v, want 10", byCity["Berlin"].RadiusKm)
	}
	if byCity["Pforzheim"].RadiusKm != 25 {
		t.Fatalf("Pforzheim radius_km = %v, want 25", byCity["Pforzheim"].RadiusKm)
	}
	if radByLat["52.500000"] != "10.00" {
		t.Fatalf("Berlin rad param = %q, want 10.00", radByLat["52.500000"])
	}
	if radByLat["48.900000"] != "25.00" {
		t.Fatalf("Pforzheim rad param = %q, want 25.00", radByLat["48.900000"])
	}
}

func TestRunUpdateMultiCityBestEffort(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "besteffort.db")
	t.Setenv(envAPIKeyName, "test-key")

	restore := stubDefaultTransport(t, func(req *http.Request) (*http.Response, error) {
		u := req.URL
		switch {
		case strings.HasPrefix(u.String(), nominatimBaseURL):
			switch q := u.Query().Get("q"); {
			case strings.Contains(q, "Berlin"):
				return jsonResponse(http.StatusOK, `[{"name":"Berlin","display_name":"Berlin, DE","lat":"52.5","lon":"13.4"}]`), nil
			case strings.Contains(q, "Pforzheim"):
				return jsonResponse(http.StatusOK, `[{"name":"Pforzheim","display_name":"Pforzheim, DE","lat":"48.9","lon":"8.7"}]`), nil
			default:
				return nil, fmt.Errorf("unexpected geocode q: %s", q)
			}
		case strings.HasPrefix(u.String(), tankerKoenigBase+"/list.php"):
			// Pforzheim (lat ~48.9) fails upstream; Berlin (lat ~52.5) succeeds.
			if strings.HasPrefix(u.Query().Get("lat"), "48.9") {
				return nil, fmt.Errorf("simulated upstream failure")
			}
			body := `{"ok":true,"stations":[{"id":"s-1","name":"S","brand":"B","street":"St","place":"P","lat":1,"lng":2,"dist":1,"diesel":1.5,"e5":1.7,"e10":1.6,"isOpen":true,"houseNumber":"1","postCode":1}]}`
			return jsonResponse(http.StatusOK, body), nil
		default:
			return nil, fmt.Errorf("unexpected URL: %s", u.String())
		}
	})
	defer restore()

	// run() returns a non-nil error here, so capture stdout manually.
	old := stdout
	var buf bytes.Buffer
	stdout = &buf
	t.Cleanup(func() { stdout = old })

	err := run([]string{"update", "--db", dbPath, "--city", "Berlin", "--city", "Pforzheim", "--output", "json"})
	if err == nil {
		t.Fatalf("expected non-nil error when a city fails")
	}
	if !strings.Contains(err.Error(), "1 of 2 cities failed") {
		t.Fatalf("err = %v, want containing '1 of 2 cities failed'", err)
	}

	var result multiUpdateResult
	if e := json.Unmarshal(buf.Bytes(), &result); e != nil {
		t.Fatalf("unmarshal: %v\noutput=%s", e, buf.String())
	}
	if result.StoredCount != 1 {
		t.Fatalf("stored_count = %d, want 1 (Berlin persisted)", result.StoredCount)
	}
	var berlin, pforzheim *cityUpdateResult
	for i := range result.Results {
		switch result.Results[i].Query {
		case "Berlin":
			berlin = &result.Results[i]
		case "Pforzheim":
			pforzheim = &result.Results[i]
		}
	}
	if berlin == nil || berlin.Error != "" || berlin.StoredCount != 1 {
		t.Fatalf("Berlin result = %+v, want success with 1 snapshot", berlin)
	}
	if pforzheim == nil || pforzheim.Error == "" {
		t.Fatalf("Pforzheim result = %+v, want a recorded error", pforzheim)
	}
}

func TestDedupeFetches(t *testing.T) {
	station := func(id string, lat, lng float64) tankerStation {
		return tankerStation{ID: id, Name: id, Lat: lat, Lng: lng}
	}
	fetch := func(name string, lat, lng, radius float64, stations ...tankerStation) cityFetch {
		return cityFetch{
			Query:      cityQuery{name: name, radius: radius},
			City:       cachedCity{QueryName: name, Name: name, Lat: lat, Lng: lng},
			Stations:   stations,
			RecordedAt: time.Date(2026, 4, 2, 9, 0, 0, 0, time.UTC),
		}
	}

	shared := station("shared", 52.42, 13.10) // ~3.5 km to Potsdam, ~22 km to Berlin
	berlinOnly := station("berlin-only", 52.52, 13.41)
	potsdamOnly := station("potsdam-only", 52.39, 13.05)

	berlin := fetch("Berlin", 52.5, 13.4, 25, berlinOnly, shared)
	potsdam := fetch("Potsdam", 52.4, 13.06, 25, shared, potsdamOnly)

	observations := dedupeFetches([]cityFetch{berlin, potsdam})
	if len(observations) != 3 {
		t.Fatalf("observations = %d, want 3 (the shared station counted once)", len(observations))
	}

	owner := map[string]string{}
	var ids []string
	for _, obs := range observations {
		owner[obs.Station.ID] = obs.City.Name
		ids = append(ids, obs.Station.ID)
	}
	if owner["shared"] != "Potsdam" {
		t.Fatalf("shared station owner = %q, want Potsdam (nearest centre)", owner["shared"])
	}
	if owner["berlin-only"] != "Berlin" || owner["potsdam-only"] != "Potsdam" {
		t.Fatalf("exclusive stations mis-attributed: %v", owner)
	}
	if !sort.StringsAreSorted(ids) {
		t.Fatalf("observation order = %v, want station-id order for deterministic writes", ids)
	}

	// Reversing the targets must not change who owns the shared station:
	// stable attribution is what keeps unchanged rows compactable across runs.
	reversed := dedupeFetches([]cityFetch{potsdam, berlin})
	for _, obs := range reversed {
		if obs.Station.ID == "shared" && obs.City.Name != "Potsdam" {
			t.Fatalf("shared station owner = %q after reordering, want Potsdam", obs.City.Name)
		}
	}
}

// Targets are fetched one after another, so a farther target can see a price
// change the nearer one missed. Ownership must follow distance, but the stored
// reading must follow freshness, or the change is lost until the next sweep.
func TestDedupeFetchesKeepsFreshestReading(t *testing.T) {
	stale := 1.500
	fresh := 1.409
	at := time.Date(2026, 4, 2, 9, 0, 0, 0, time.UTC)

	nearer := cityFetch{
		Query:      cityQuery{name: "Potsdam", radius: 25},
		City:       cachedCity{Name: "Potsdam", Lat: 52.4, Lng: 13.06},
		Stations:   []tankerStation{{ID: "shared", Name: "S", Lat: 52.42, Lng: 13.10, Diesel: &stale, IsOpen: true}},
		RecordedAt: at,
	}
	farther := cityFetch{
		Query:      cityQuery{name: "Berlin", radius: 25},
		City:       cachedCity{Name: "Berlin", Lat: 52.5, Lng: 13.4},
		Stations:   []tankerStation{{ID: "shared", Name: "S", Lat: 52.42, Lng: 13.10, Diesel: &fresh, IsOpen: true}},
		RecordedAt: at.Add(2 * time.Second),
	}

	observations := dedupeFetches([]cityFetch{nearer, farther})
	if len(observations) != 1 {
		t.Fatalf("observations = %d, want 1", len(observations))
	}
	obs := observations[0]
	if obs.City.Name != "Potsdam" || obs.FetchIndex != 0 || obs.RadiusKM != 25 {
		t.Fatalf("owner = %q (fetch %d, radius %v), want Potsdam at fetch 0", obs.City.Name, obs.FetchIndex, obs.RadiusKM)
	}
	if obs.Station.Diesel == nil || *obs.Station.Diesel != fresh {
		t.Fatalf("stored diesel = %v, want the fresher %v from the farther target", obs.Station.Diesel, fresh)
	}
	if !obs.RecordedAt.Equal(at.Add(2 * time.Second)) {
		t.Fatalf("recorded_at = %v, want the fresher fetch's stamp", obs.RecordedAt)
	}
}

// Equidistant targets must resolve the same way every run, otherwise a station
// would flip owners and defeat snapshot compaction.
func TestDedupeFetchesTieGoesToEarlierTarget(t *testing.T) {
	st := tankerStation{ID: "tie", Name: "tie", Lat: 52.0, Lng: 13.0}
	first := cityFetch{
		Query:    cityQuery{name: "First", radius: 25},
		City:     cachedCity{Name: "First", Lat: 52.1, Lng: 13.0},
		Stations: []tankerStation{st},
	}
	second := cityFetch{
		Query:    cityQuery{name: "Second", radius: 25},
		City:     cachedCity{Name: "Second", Lat: 51.9, Lng: 13.0},
		Stations: []tankerStation{st},
	}

	observations := dedupeFetches([]cityFetch{first, second})
	if len(observations) != 1 {
		t.Fatalf("observations = %d, want 1", len(observations))
	}
	if observations[0].City.Name != "First" {
		t.Fatalf("owner = %q, want First on an exact tie", observations[0].City.Name)
	}
}

// A sweep only ranks the targets it fetched, so ownership must be settled
// against the city that already owns the station too — otherwise a single-city
// run, or a sweep whose nearer target failed, silently reassigns it.
func TestResolveSnapshotOwner(t *testing.T) {
	t.Parallel()

	db := openTestDB(t)
	ctx := context.Background()
	for _, c := range []struct {
		name     string
		lat, lng float64
	}{
		{"Potsdam", 52.4, 13.06},
		{"Berlin", 52.5, 13.4},
	} {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO cities (name, normalized_name, display_name, lat, lng, created_at)
			VALUES (?, ?, ?, ?, ?, ?)
		`, c.name, c.name, c.name, c.lat, c.lng, "2026-04-02T09:00:00Z"); err != nil {
			t.Fatalf("insert city %s: %v", c.name, err)
		}
	}

	// ~3.5 km from Potsdam's centre, ~22 km from Berlin's.
	station := tankerStation{ID: "shared", Lat: 52.42, Lng: 13.10}
	berlin := cachedCity{Name: "Berlin", Lat: 52.5, Lng: 13.4}
	potsdam := cachedCity{Name: "Potsdam", Lat: 52.4, Lng: 13.06}

	// Both cities cover the station: Potsdam at 3.5 km, Berlin at 22 km. These
	// cases model a configured install, where an absent city means a removed
	// target rather than a city this run did not name.
	configured := func(radii map[string]float64) cityCoverage {
		return cityCoverage{radiusByCity: radii, configured: true}
	}
	covered := configured(map[string]float64{"Potsdam": 5, "Berlin": 25})

	tests := []struct {
		name         string
		currentOwner string
		candidate    cachedCity
		coverage     cityCoverage
		want         string
	}{
		{"unowned station goes to the fetching city", "", berlin, covered, "Berlin"},
		{"owner re-fetching itself keeps it", "Berlin", berlin, covered, "Berlin"},
		{"nearer owner survives a farther city's run", "Potsdam", berlin, covered, "Potsdam"},
		{"farther owner loses to a nearer city", "Berlin", potsdam, covered, "Potsdam"},
		{"unknown owner cannot hold the station", "Atlantis", berlin, covered, "Berlin"},
		// The owner's target was removed. Its cities row survives, and the
		// remaining target keeps the station fresh forever, so without the
		// coverage check the station would never move.
		{
			name: "removed owner hands the station over", currentOwner: "Potsdam", candidate: berlin,
			coverage: configured(map[string]float64{"Berlin": 25}), want: "Berlin",
		},
		// The owner's radius was shrunk past the station.
		{
			name: "shrunk owner hands the station over", currentOwner: "Potsdam", candidate: berlin,
			coverage: configured(map[string]float64{"Potsdam": 2, "Berlin": 25}), want: "Berlin",
		},
		// Shrunk, but still reaching: ownership is stable.
		{
			name: "owner still in reach after a shrink keeps it", currentOwner: "Potsdam", candidate: berlin,
			coverage: configured(map[string]float64{"Potsdam": 4, "Berlin": 25}), want: "Potsdam",
		},
		// A flag-driven install has no configuration to compare against, so an
		// owner this run did not name — a solo run, or a failed fetch — keeps the
		// station on distance alone.
		{
			name: "unnamed owner keeps the station without configured targets", currentOwner: "Potsdam", candidate: berlin,
			coverage: cityCoverage{radiusByCity: map[string]float64{"Berlin": 25}}, want: "Potsdam",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer tx.Rollback()
			got, err := resolveSnapshotOwner(ctx, tx, newCityCentres(tc.coverage), tc.currentOwner, tc.candidate, station)
			if err != nil {
				t.Fatalf("resolveSnapshotOwner: %v", err)
			}
			if got != tc.want {
				t.Fatalf("owner = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLoadCityCoverageMergesTargetsAndSweep(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	insertSuggestCity(t, db, cachedCity{QueryName: "Berlin, Germany", Name: "Berlin", DisplayName: "Berlin, Deutschland", Lat: 52.5, Lng: 13.4})
	insertSuggestCity(t, db, cachedCity{QueryName: "Potsdam", Name: "Potsdam", DisplayName: "Potsdam", Lat: 52.4, Lng: 13.06})
	insertSuggestCity(t, db, cachedCity{QueryName: "Hamburg", Name: "Hamburg", DisplayName: "Hamburg", Lat: 53.55, Lng: 9.99})
	// A target written the long way still has to be keyed by the normalized name
	// that ownership uses.
	insertUpdateTargetRow(t, db, "Berlin, Germany", 5)
	insertUpdateTargetRow(t, db, "Potsdam", 10)
	insertUpdateTargetRow(t, db, "Ghosttown", 20) // never geocoded

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	loaded, err := loadCityCoverage(ctx, tx, []cityFetch{
		// An ad-hoc sweep reaching further than the configured target, plus a
		// city that is not a target at all.
		{Query: cityQuery{name: "Berlin", radius: 25}, City: cachedCity{Name: "Berlin"}},
		{Query: cityQuery{name: "Hamburg", radius: 15}, City: cachedCity{Name: "Hamburg"}},
	})
	if err != nil {
		t.Fatalf("loadCityCoverage: %v", err)
	}
	if !loaded.configured {
		t.Error("coverage must be authoritative when update targets exist")
	}
	coverage := loaded.radiusByCity
	// The wider of target and sweep wins, so an ad-hoc wide run cannot hand away
	// ownership the scheduled target would keep.
	if coverage["Berlin"] != 25 {
		t.Errorf("Berlin coverage = %v, want the wider sweep radius 25", coverage["Berlin"])
	}
	// A configured target that this sweep did not fetch still covers its own.
	if coverage["Potsdam"] != 10 {
		t.Errorf("Potsdam coverage = %v, want the configured 10", coverage["Potsdam"])
	}
	// A swept city that is not a target counts too — it is what fetched them.
	if coverage["Hamburg"] != 15 {
		t.Errorf("Hamburg coverage = %v, want the swept 15", coverage["Hamburg"])
	}
	// A target whose city was never geocoded cannot own anything.
	if _, ok := coverage["Ghosttown"]; ok {
		t.Error("an ungeocoded target must not appear in the coverage")
	}

	// With no update targets at all a sweep is flag-driven, and absence from the
	// coverage cannot be read as a removal.
	if _, err := tx.ExecContext(ctx, `DELETE FROM update_targets`); err != nil {
		t.Fatalf("clear targets: %v", err)
	}
	flagDriven, err := loadCityCoverage(ctx, tx, []cityFetch{
		{Query: cityQuery{name: "Berlin", radius: 25}, City: cachedCity{Name: "Berlin"}},
	})
	if err != nil {
		t.Fatalf("loadCityCoverage without targets: %v", err)
	}
	if flagDriven.configured {
		t.Error("coverage must not be authoritative without configured targets")
	}
	if reaches, authoritative := flagDriven.stillReaches("Potsdam", 3.5); reaches || authoritative {
		t.Errorf("stillReaches for an unnamed city = (%v, %v), want (false, false)", reaches, authoritative)
	}
}

// A station within range of two requested cities is stored once, owned by the
// city whose centre is nearest — and repeated runs must not accumulate rows
// for it, since the shared station no longer defeats snapshot compaction.
func TestRunUpdateMultiCityOverlap(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "overlap.db")
	t.Setenv(envAPIKeyName, "test-key")

	// Identical station (same id, same prices) reported for both cities. It
	// sits ~3.5 km from Potsdam's centre and ~22 km from Berlin's.
	sharedStation := `{"ok":true,"stations":[{"id":"shared-1","name":"S","brand":"B","street":"St","place":"P","lat":52.42,"lng":13.10,"dist":1,"diesel":1.5,"e5":1.7,"e10":1.6,"isOpen":true,"houseNumber":"1","postCode":1}]}`
	restore := stubDefaultTransport(t, func(req *http.Request) (*http.Response, error) {
		u := req.URL
		switch {
		case strings.HasPrefix(u.String(), nominatimBaseURL):
			switch q := u.Query().Get("q"); {
			case strings.Contains(q, "Berlin"):
				return jsonResponse(http.StatusOK, `[{"name":"Berlin","display_name":"Berlin, DE","lat":"52.5","lon":"13.4"}]`), nil
			case strings.Contains(q, "Potsdam"):
				return jsonResponse(http.StatusOK, `[{"name":"Potsdam","display_name":"Potsdam, DE","lat":"52.4","lon":"13.06"}]`), nil
			default:
				return nil, fmt.Errorf("unexpected geocode q: %s", q)
			}
		case strings.HasPrefix(u.String(), tankerKoenigBase+"/list.php"):
			return jsonResponse(http.StatusOK, sharedStation), nil
		default:
			return nil, fmt.Errorf("unexpected URL: %s", u.String())
		}
	})
	defer restore()

	updateBoth := func() multiUpdateResult {
		t.Helper()
		out := captureStdout(t, func() error {
			return run([]string{"update", "--db", dbPath, "--radius", "25", "--city", "Berlin", "--city", "Potsdam", "--output", "json"})
		})
		var result multiUpdateResult
		if err := json.Unmarshal([]byte(out), &result); err != nil {
			t.Fatalf("unmarshal: %v\noutput=%s", err, out)
		}
		return result
	}

	result := updateBoth()
	if result.StoredCount != 1 || result.FetchedCount != 2 {
		t.Fatalf("stored_count = %d, fetched_count = %d, want 1 and 2", result.StoredCount, result.FetchedCount)
	}
	byCity := map[string]cityUpdateResult{}
	for _, r := range result.Results {
		byCity[r.City.Name] = r
	}
	if got := byCity["Potsdam"]; got.StoredCount != 1 || got.FetchedCount != 1 {
		t.Fatalf("Potsdam stored/fetched = %d/%d, want 1/1: the nearer centre owns the station", got.StoredCount, got.FetchedCount)
	}
	if got := byCity["Berlin"]; got.StoredCount != 0 || got.FetchedCount != 1 {
		t.Fatalf("Berlin stored/fetched = %d/%d, want 0/1: the station belongs to Potsdam", got.StoredCount, got.FetchedCount)
	}

	// A second sweep with unchanged prices must roll the row forward, not
	// insert. This is what the old per-city duplication made impossible.
	updateBoth()

	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	rows, err := db.QueryContext(context.Background(),
		`SELECT city_name FROM price_snapshots WHERE station_id = 'shared-1' ORDER BY city_name`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var cities []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		cities = append(cities, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(cities) != 1 || cities[0] != "Potsdam" {
		t.Fatalf("shared station city associations = %v, want [Potsdam]", cities)
	}
}

// overlapStub serves two cities whose radii share one station. diesel is read
// per request so a test can change the price between fetches, and failCity
// makes one target's station fetch fail.
func overlapStub(t *testing.T, diesel func(city string) string, failCity string) func() {
	t.Helper()
	centres := map[string][2]string{
		"Berlin":  {"52.5", "13.4"},
		"Potsdam": {"52.4", "13.06"},
	}
	// The station sits ~3.5 km from Potsdam's centre and ~22 km from Berlin's.
	byLat := map[string]string{"52.500000": "Berlin", "52.400000": "Potsdam"}

	return stubDefaultTransport(t, func(req *http.Request) (*http.Response, error) {
		u := req.URL
		switch {
		case strings.HasPrefix(u.String(), nominatimBaseURL):
			q := u.Query().Get("q")
			for name, centre := range centres {
				if strings.Contains(q, name) {
					return jsonResponse(http.StatusOK, fmt.Sprintf(
						`[{"name":%q,"display_name":"%s, DE","lat":%q,"lon":%q}]`, name, name, centre[0], centre[1])), nil
				}
			}
			return nil, fmt.Errorf("unexpected geocode q: %s", q)
		case strings.HasPrefix(u.String(), tankerKoenigBase+"/list.php"):
			city, ok := byLat[u.Query().Get("lat")]
			if !ok {
				return nil, fmt.Errorf("unexpected lat: %s", u.Query().Get("lat"))
			}
			if city == failCity {
				return jsonResponse(http.StatusInternalServerError, `{"ok":false,"message":"upstream down"}`), nil
			}
			// Honour the requested radius the way the real API does, so a
			// shrunk target genuinely stops seeing the station.
			radius, err := strconv.ParseFloat(u.Query().Get("rad"), 64)
			if err != nil {
				return nil, fmt.Errorf("unexpected rad: %s", u.Query().Get("rad"))
			}
			centre := centres[city]
			lat, _ := strconv.ParseFloat(centre[0], 64)
			lng, _ := strconv.ParseFloat(centre[1], 64)
			if haversineKM(lat, lng, 52.42, 13.10) > radius {
				return jsonResponse(http.StatusOK, `{"ok":true,"stations":[]}`), nil
			}
			return jsonResponse(http.StatusOK, fmt.Sprintf(
				`{"ok":true,"stations":[{"id":"shared-1","name":"S","brand":"B","street":"St","place":"Teltow","lat":52.42,"lng":13.10,"dist":1,"diesel":%s,"e5":1.7,"e10":1.6,"isOpen":true,"houseNumber":"1","postCode":1}]}`,
				diesel(city))), nil
		}
		return nil, fmt.Errorf("unexpected URL: %s", u.String())
	})
}

func sharedStationOwner(t *testing.T, dbPath string) (string, int) {
	t.Helper()
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	var owner string
	var rows int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM price_snapshots WHERE station_id = 'shared-1'`).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT city_name FROM price_snapshots WHERE station_id = 'shared-1' ORDER BY recorded_at DESC, id DESC LIMIT 1`).Scan(&owner); err != nil {
		t.Fatalf("owner: %v", err)
	}
	return owner, rows
}

// A later run covering only the farther city must not take the station away
// from the nearer one: ownership cannot depend on which targets an invocation
// happens to include.
func TestRunUpdateSoloCityKeepsNearerOwner(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "solo.db")
	t.Setenv(envAPIKeyName, "test-key")
	restore := overlapStub(t, func(string) string { return "1.500" }, "")
	defer restore()

	_ = captureStdout(t, func() error {
		return run([]string{"update", "--db", dbPath, "--radius", "25", "--city", "Berlin", "--city", "Potsdam"})
	})
	if owner, rows := sharedStationOwner(t, dbPath); owner != "Potsdam" || rows != 1 {
		t.Fatalf("after sweep: owner = %q in %d rows, want Potsdam in 1", owner, rows)
	}

	_ = captureStdout(t, func() error {
		return run([]string{"update", "--db", dbPath, "--radius", "25", "--city", "Berlin"})
	})
	if owner, rows := sharedStationOwner(t, dbPath); owner != "Potsdam" || rows != 1 {
		t.Fatalf("after solo Berlin run: owner = %q in %d rows, want Potsdam in 1", owner, rows)
	}
}

// A transient failure of the nearer target must not hand its stations to the
// farther one either.
func TestRunUpdateFailedNearerTargetKeepsOwner(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "failover.db")
	t.Setenv(envAPIKeyName, "test-key")

	restore := overlapStub(t, func(string) string { return "1.500" }, "")
	_ = captureStdout(t, func() error {
		return run([]string{"update", "--db", dbPath, "--radius", "25", "--city", "Berlin", "--city", "Potsdam"})
	})
	restore()
	if owner, _ := sharedStationOwner(t, dbPath); owner != "Potsdam" {
		t.Fatalf("after sweep: owner = %q, want Potsdam", owner)
	}

	// Same sweep, but Potsdam's fetch fails.
	restore = overlapStub(t, func(string) string { return "1.500" }, "Potsdam")
	defer restore()
	_ = captureStdout(t, func() error {
		// Best-effort: the failing city makes the run report an error.
		_ = run([]string{"update", "--db", dbPath, "--radius", "25", "--city", "Berlin", "--city", "Potsdam"})
		return nil
	})
	if owner, rows := sharedStationOwner(t, dbPath); owner != "Potsdam" || rows != 1 {
		t.Fatalf("after Potsdam failed: owner = %q in %d rows, want Potsdam in 1", owner, rows)
	}
}

// The nearer target is fetched first, so a price change the farther target sees
// moments later must still be stored — de-duplication picks the owner, not the
// reading.
func TestRunUpdateOverlapStoresFreshestReading(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "fresh.db")
	t.Setenv(envAPIKeyName, "test-key")

	// Potsdam (nearer, fetched first) still reports the old price; Berlin sees
	// the drop.
	restore := overlapStub(t, func(city string) string {
		if city == "Berlin" {
			return "1.409"
		}
		return "1.500"
	}, "")
	defer restore()

	_ = captureStdout(t, func() error {
		return run([]string{"update", "--db", dbPath, "--radius", "25", "--city", "Potsdam", "--city", "Berlin"})
	})

	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()
	var diesel float64
	var owner string
	if err := db.QueryRowContext(context.Background(),
		`SELECT diesel, city_name FROM price_snapshots WHERE station_id = 'shared-1'`).Scan(&diesel, &owner); err != nil {
		t.Fatalf("query: %v", err)
	}
	if diesel != 1.409 {
		t.Fatalf("diesel = %v, want 1.409: the freshest reading wins", diesel)
	}
	if owner != "Potsdam" {
		t.Fatalf("owner = %q, want Potsdam: the nearest centre still owns it", owner)
	}
}

// recorded_at must be stamped after the data is fetched, not before the run
// begins, so a slow fetch is not backdated.
func TestRunUpdateStampsAfterFetch(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "stamp.db")
	t.Setenv(envAPIKeyName, "test-key")

	restore := stubDefaultTransport(t, func(req *http.Request) (*http.Response, error) {
		u := req.URL
		switch {
		case strings.HasPrefix(u.String(), nominatimBaseURL):
			return jsonResponse(http.StatusOK, `[{"name":"Berlin","display_name":"Berlin, DE","lat":"52.5","lon":"13.4"}]`), nil
		case strings.HasPrefix(u.String(), tankerKoenigBase+"/list.php"):
			time.Sleep(2 * time.Second) // simulate a slow upstream fetch
			body := `{"ok":true,"stations":[{"id":"s-1","name":"S","brand":"B","street":"St","place":"P","lat":1,"lng":2,"dist":1,"diesel":1.5,"e5":1.7,"e10":1.6,"isOpen":true,"houseNumber":"1","postCode":1}]}`
			return jsonResponse(http.StatusOK, body), nil
		default:
			return nil, fmt.Errorf("unexpected URL: %s", u.String())
		}
	})
	defer restore()

	before := time.Now().UTC()
	output := captureStdout(t, func() error {
		return run([]string{"update", "--db", dbPath, "--city", "Berlin", "--output", "json"})
	})

	var result updateResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("unmarshal: %v\noutput=%s", err, output)
	}
	recordedAt, err := time.Parse(time.RFC3339, result.RecordedAt)
	if err != nil {
		t.Fatalf("parse recorded_at %q: %v", result.RecordedAt, err)
	}
	if delta := recordedAt.Sub(before); delta < time.Second {
		t.Fatalf("recorded_at only %v after run start; want >= 1s (stamped after the slow fetch)", delta)
	}
}

func TestRunImportCitiesSupportsJSONOutput(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "cities.db")

	restore := stubDefaultTransport(t, func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "https://download.geonames.org/export/dump/DE.zip" {
			return nil, fmt.Errorf("unexpected request URL: %s", req.URL.String())
		}
		return zipResponse(t, map[string]string{
			"DE.txt": strings.Join([]string{
				"1\tBerlin\tBerlin\tBerlin\t52.5200\t13.4050\tP\tPPL\tDE",
				"2\tHamburg\tHamburg\tHamburg\t53.5511\t9.9937\tP\tPPLA2\tDE",
				"3\tVillage\tVillage\tVillage\t50.0000\t8.0000\tP\tPPLL\tDE",
				"4\tAdmin\tAdmin\tAdmin\t51.0000\t9.0000\tA\tPPL\tDE",
				"5\tParis\tParis\tParis\t48.8566\t2.3522\tP\tPPL\tFR",
			}, "\n"),
		}), nil
	})
	defer restore()

	output := captureStdout(t, func() error {
		return run([]string{"import", "cities", "--db", dbPath, "--output", "json", "de"})
	})

	var result importCitiesResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("unmarshal import-cities output: %v\noutput=%s", err, output)
	}
	if result.CountryCode != "DE" {
		t.Fatalf("country_code = %q, want %q", result.CountryCode, "DE")
	}
	if result.ParsedCount != 2 || result.ImportedCount != 2 {
		t.Fatalf("counts = parsed:%d imported:%d, want 2/2", result.ParsedCount, result.ImportedCount)
	}

	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	var count int
	if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM cities`).Scan(&count); err != nil {
		t.Fatalf("count cities: %v", err)
	}
	if count != 2 {
		t.Fatalf("city count = %d, want 2", count)
	}
}

func TestRunImportCitiesRequiresCountryCode(t *testing.T) {
	err := run([]string{"import", "cities"})
	if err == nil || !strings.Contains(err.Error(), "2-letter country code") {
		t.Fatalf("err = %v, want country code validation error", err)
	}
}

func TestRunImportCitiesRejectsInvalidCountryCode(t *testing.T) {
	err := run([]string{"import", "cities", "DEU"})
	if err == nil || !strings.Contains(err.Error(), "2-letter country code") {
		t.Fatalf("err = %v, want country code validation error", err)
	}
}

func TestRunClearCitiesSupportsJSONOutput(t *testing.T) {
	dbPath := seedFixtureDB(t)

	output := captureStdout(t, func() error {
		return run([]string{"clear", "cities", "--db", dbPath, "--output", "json"})
	})

	var result clearCitiesResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("unmarshal clear cities output: %v\noutput=%s", err, output)
	}
	if result.ClearedCount != 1 {
		t.Fatalf("cleared_count = %d, want 1", result.ClearedCount)
	}

	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	var count int
	if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM cities`).Scan(&count); err != nil {
		t.Fatalf("count cities: %v", err)
	}
	if count != 0 {
		t.Fatalf("city count = %d, want 0", count)
	}
}

func TestRunImportCitiesUpsertsExistingRows(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	_, err := db.ExecContext(ctx, `
		INSERT INTO cities (name, normalized_name, display_name, lat, lng, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, "Berlin", "Berlin", "Berlin", 1.0, 2.0, "2026-04-01T00:00:00Z")
	if err != nil {
		t.Fatalf("insert seed city: %v", err)
	}

	imported, err := importCities(ctx, db, dialectSQLite, []cachedCity{{
		Name:        "Berlin",
		DisplayName: "Berlin",
		Lat:         52.5200,
		Lng:         13.4050,
	}})
	if err != nil {
		t.Fatalf("importCities: %v", err)
	}
	if imported != 1 {
		t.Fatalf("imported = %d, want 1", imported)
	}

	var (
		lat       float64
		lng       float64
		createdAt string
		count     int
	)
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*), lat, lng, created_at FROM cities WHERE name = ?`, "Berlin").Scan(&count, &lat, &lng, &createdAt); err != nil {
		t.Fatalf("query city: %v", err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
	if lat != 52.5200 || lng != 13.4050 {
		t.Fatalf("coordinates = %.4f, %.4f, want 52.5200, 13.4050", lat, lng)
	}
	if createdAt != "2026-04-01T00:00:00Z" {
		t.Fatalf("created_at = %q, want seed timestamp", createdAt)
	}
}

func TestParseGeoNamesZipRequiresCountryFile(t *testing.T) {
	body := buildZipBytes(t, map[string]string{
		"FR.txt": "1\tParis\tParis\tParis\t48.8566\t2.3522\tP\tPPL\tFR\n",
	})

	_, err := parseGeoNamesZip(body, "DE")
	if err == nil || !strings.Contains(err.Error(), "DE.txt") {
		t.Fatalf("err = %v, want missing file error", err)
	}
}

func TestRunCompactCompactsExistingSnapshots(t *testing.T) {
	dbPath := seedUncompactedFixtureDB(t)

	output := captureStdout(t, func() error {
		return run([]string{"compact", "--db", dbPath, "--output", "json"})
	})

	var result compactResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("unmarshal compact output: %v\noutput=%s", err, output)
	}
	if result.StationsProcessed != 1 {
		t.Fatalf("stations_processed = %d, want 1", result.StationsProcessed)
	}
	if result.BeforeCount != 8 {
		t.Fatalf("before_count = %d, want 8", result.BeforeCount)
	}
	if result.AfterCount != 5 {
		t.Fatalf("after_count = %d, want 5", result.AfterCount)
	}
	if result.DeletedCount != 3 {
		t.Fatalf("deleted_count = %d, want 3", result.DeletedCount)
	}
	if result.UpdatedCount != 3 {
		t.Fatalf("updated_count = %d, want 3", result.UpdatedCount)
	}

	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("open compacted db: %v", err)
	}
	defer db.Close()

	rows, err := db.QueryContext(context.Background(), `
		SELECT recorded_at, is_open, diesel
		FROM price_snapshots
		WHERE station_id = ?
		ORDER BY recorded_at ASC, id ASC
	`, "station-1")
	if err != nil {
		t.Fatalf("query compacted snapshots: %v", err)
	}
	defer rows.Close()

	var got []string
	for rows.Next() {
		var (
			recordedAt string
			isOpen     bool
			diesel     float64
		)
		if err := rows.Scan(&recordedAt, &isOpen, &diesel); err != nil {
			t.Fatalf("scan compacted snapshot: %v", err)
		}
		got = append(got, fmt.Sprintf("%s open=%t diesel=%.3f", recordedAt, isOpen, diesel))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate compacted snapshots: %v", err)
	}

	want := []string{
		"2026-04-07T10:25:02Z open=true diesel=2.349",
		"2026-04-07T10:30:08Z open=true diesel=2.389",
		"2026-04-07T10:40:08Z open=true diesel=2.389",
		"2026-04-07T16:00:02Z open=true diesel=2.349",
		"2026-04-07T16:10:02Z open=true diesel=2.349",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("compacted snapshots =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestMigratePredictionsAccuracyIndexBackfills covers the upgrade path: a
// database created before the index existed must gain it on migrate. MySQL is
// the case that needs it (its inline index declaration no-ops on an existing
// table), and dropping the index from a SQLite schema reproduces that state.
func TestMigratePredictionsAccuracyIndexBackfills(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "accuracy-index.db")
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	if err := initSchema(ctx, db, dialectSQLite); err != nil {
		t.Fatalf("initSchema: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DROP INDEX idx_price_predictions_accuracy`); err != nil {
		t.Fatalf("drop index: %v", err)
	}

	// migrateSchema alone, not initSchema: ensureSchema would recreate the
	// index through CREATE INDEX IF NOT EXISTS and hide a broken migration.
	result, err := migrateSchema(ctx, db, dialectSQLite)
	if err != nil {
		t.Fatalf("migrateSchema: %v", err)
	}
	if !containsString(result.Applied, "price_predictions.idx_price_predictions_accuracy") {
		t.Fatalf("applied migrations = %v, want price_predictions.idx_price_predictions_accuracy", result.Applied)
	}
	hasIndex, err := tableHasIndex(ctx, db, dialectSQLite, "price_predictions", "idx_price_predictions_accuracy")
	if err != nil {
		t.Fatalf("tableHasIndex: %v", err)
	}
	if !hasIndex {
		t.Fatal("expected idx_price_predictions_accuracy after migration")
	}

	// Idempotent: a second pass must not report or attempt the change again.
	again, err := migrateSchema(ctx, db, dialectSQLite)
	if err != nil {
		t.Fatalf("migrateSchema second pass: %v", err)
	}
	if containsString(again.Applied, "price_predictions.idx_price_predictions_accuracy") {
		t.Fatalf("second migrate re-applied the index: %v", again.Applied)
	}
}

// TestMigrateRunsSuggestionBiasBackfills covers the upgrade path: a database
// created before prediction_runs.suggestion_bias existed must gain the column
// on migrate, defaulting old runs to 0 (their bias was never recorded, and a
// raw price is what the dashboard displayed for them anyway).
func TestMigrateRunsSuggestionBiasBackfills(t *testing.T) {
	ctx := context.Background()
	db, err := openDB(filepath.Join(t.TempDir(), "suggestion-bias.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	if err := initSchema(ctx, db, dialectSQLite); err != nil {
		t.Fatalf("initSchema: %v", err)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE prediction_runs DROP COLUMN suggestion_bias`); err != nil {
		t.Fatalf("drop column: %v", err)
	}
	// A run persisted by the old schema, present before the upgrade.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO prediction_runs (run_at, city_name, fuel, range_km, history_days, predict_days, jump_anchor_hour, station_count)
		VALUES ('2026-04-25T09:00:00Z', '', 'diesel', 0, 30, 3, 12, 1)
	`); err != nil {
		t.Fatalf("insert legacy run: %v", err)
	}

	result, err := migrateSchema(ctx, db, dialectSQLite)
	if err != nil {
		t.Fatalf("migrateSchema: %v", err)
	}
	if !containsString(result.Applied, "prediction_runs.suggestion_bias") {
		t.Fatalf("applied migrations = %v, want prediction_runs.suggestion_bias", result.Applied)
	}
	var legacyBias float64
	if err := db.QueryRowContext(ctx, `SELECT suggestion_bias FROM prediction_runs`).Scan(&legacyBias); err != nil {
		t.Fatalf("read legacy run: %v", err)
	}
	if legacyBias != 0 {
		t.Fatalf("legacy suggestion_bias = %v, want 0", legacyBias)
	}

	// Idempotent: a second pass must not report or attempt the change again.
	again, err := migrateSchema(ctx, db, dialectSQLite)
	if err != nil {
		t.Fatalf("migrateSchema second pass: %v", err)
	}
	if containsString(again.Applied, "prediction_runs.suggestion_bias") {
		t.Fatalf("second migrate re-applied the column: %v", again.Applied)
	}
}

func TestTableHasIndexReportsMissingIndex(t *testing.T) {
	ctx := context.Background()
	db, err := openDB(filepath.Join(t.TempDir(), "index-probe.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	if err := initSchema(ctx, db, dialectSQLite); err != nil {
		t.Fatalf("initSchema: %v", err)
	}
	has, err := tableHasIndex(ctx, db, dialectSQLite, "price_predictions", "idx_price_predictions_nonexistent")
	if err != nil {
		t.Fatalf("tableHasIndex: %v", err)
	}
	if has {
		t.Fatal("expected a made-up index name to be reported missing")
	}
}

// TestAccuracyPageAggregatesStayIndexOnly pins the reason
// idx_price_predictions_accuracy is shaped the way it is. The admin accuracy
// page runs several aggregate passes over the same (fuel, target_start range)
// slice; each must be answerable from the index alone. Widening what the page
// selects without widening the index turns these back into table scans over
// millions of rows, which is exactly the regression this guards.
func TestAccuracyPageAggregatesStayIndexOnly(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "accuracy-plan.db")
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	if err := initSchema(ctx, db, dialectSQLite); err != nil {
		t.Fatalf("initSchema: %v", err)
	}
	// The planner needs to believe the table is large enough that a covering
	// index beats a scan; sqlite_stat1 is how it learns that without the test
	// having to insert millions of rows.
	if _, err := db.ExecContext(ctx, `ANALYZE`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO sqlite_stat1 (tbl, idx, stat) VALUES
			('price_predictions', 'idx_price_predictions_accuracy', '2000000 400000 6000 2 1 1 1 1 1'),
			('price_predictions', 'idx_price_predictions_station_fuel_target', '2000000 20000 7000 2'),
			('price_predictions', 'idx_price_predictions_due', '2000000 700000 700000 3'),
			('price_predictions', 'idx_price_predictions_run', '2000000 500')
	`); err != nil {
		t.Fatalf("seed stats: %v", err)
	}
	// Reopen so the planner loads the seeded statistics.
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	db, err = openDB(dbPath)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}

	where := `pp.actual_price IS NOT NULL AND pp.fuel = ? AND pp.target_start >= ? AND pp.target_start <= ?`
	cases := []struct {
		name  string
		query string
	}{
		{"summary", `SELECT COUNT(*), COUNT(DISTINCT pp.station_id), AVG(ABS(pp.error)), AVG(pp.error),
			AVG(pp.error * pp.error), MIN(pp.error), MAX(pp.error)
			FROM price_predictions pp WHERE ` + where},
		{"series", `SELECT pp.target_start, AVG(pp.predicted_price), AVG(pp.actual_price), COUNT(*)
			FROM price_predictions pp WHERE ` + where + ` GROUP BY pp.target_start ORDER BY pp.target_start ASC`},
		{"latest_per_window", `SELECT COUNT(*), AVG(ABS(pp.error)) FROM price_predictions pp JOIN (
				SELECT pp.station_id AS station_id, pp.target_start AS target_start, MAX(pp.run_id) AS run_id
				FROM price_predictions pp WHERE ` + where + ` GROUP BY pp.station_id, pp.target_start
			) latest ON latest.station_id = pp.station_id AND latest.target_start = pp.target_start
				AND latest.run_id = pp.run_id`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := db.QueryContext(ctx, `EXPLAIN QUERY PLAN `+tc.query,
				"diesel", "2026-07-01T00:00:00Z", "2026-07-15T23:59:59Z")
			if err != nil {
				t.Fatalf("explain: %v", err)
			}
			defer rows.Close()
			var plan []string
			for rows.Next() {
				var id, parent, notUsed int
				var detail string
				if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
					t.Fatalf("scan plan: %v", err)
				}
				plan = append(plan, detail)
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("plan rows: %v", err)
			}
			joined := strings.Join(plan, "\n")
			if !strings.Contains(joined, "COVERING INDEX idx_price_predictions_accuracy") {
				t.Fatalf("query is not answered from the covering index; plan:\n%s", joined)
			}
			if strings.Contains(joined, "SCAN price_predictions") {
				t.Fatalf("query still scans the table; plan:\n%s", joined)
			}
		})
	}
}

func TestRunMigrateAppliesLegacySchemaChanges(t *testing.T) {
	dbPath := seedLegacyFixtureDB(t)

	output := captureStdout(t, func() error {
		return run([]string{"migrate", "--db", dbPath, "--output", "json"})
	})

	var result migrateResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("unmarshal migrate output: %v\noutput=%s", err, output)
	}
	if !containsString(result.Applied, "cities.normalized_name") {
		t.Fatalf("applied migrations = %v, want cities.normalized_name", result.Applied)
	}
	if !containsString(result.Applied, "price_snapshots.dist_km") {
		t.Fatalf("applied migrations = %v, want price_snapshots.dist_km", result.Applied)
	}
	if !containsString(result.Applied, "stations.name_override") {
		t.Fatalf("applied migrations = %v, want stations.name_override", result.Applied)
	}
	if !containsString(result.Applied, "users.notify_fuel") {
		t.Fatalf("applied migrations = %v, want users.notify_fuel", result.Applied)
	}
	if !containsString(result.Applied, "users.notify_suggest_enabled") {
		t.Fatalf("applied migrations = %v, want users.notify_suggest_enabled", result.Applied)
	}

	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("open migrated db: %v", err)
	}
	defer db.Close()

	ctx := context.Background()

	hasNormalizedName, err := tableHasColumn(ctx, db, dialectSQLite, "cities", "normalized_name")
	if err != nil {
		t.Fatalf("tableHasColumn cities.normalized_name: %v", err)
	}
	if !hasNormalizedName {
		t.Fatal("expected cities.normalized_name after migration")
	}

	hasDistKM, err := tableHasColumn(ctx, db, dialectSQLite, "price_snapshots", "dist_km")
	if err != nil {
		t.Fatalf("tableHasColumn price_snapshots.dist_km: %v", err)
	}
	if hasDistKM {
		t.Fatal("expected price_snapshots.dist_km to be removed")
	}

	hasNameOverride, err := tableHasColumn(ctx, db, dialectSQLite, "stations", "name_override")
	if err != nil {
		t.Fatalf("tableHasColumn stations.name_override: %v", err)
	}
	if !hasNameOverride {
		t.Fatal("expected stations.name_override after migration")
	}

	hasNotifyFuel, err := tableHasColumn(ctx, db, dialectSQLite, "users", "notify_fuel")
	if err != nil {
		t.Fatalf("tableHasColumn users.notify_fuel: %v", err)
	}
	if !hasNotifyFuel {
		t.Fatal("expected users.notify_fuel after migration")
	}

	hasNotifySuggestEnabled, err := tableHasColumn(ctx, db, dialectSQLite, "users", "notify_suggest_enabled")
	if err != nil {
		t.Fatalf("tableHasColumn users.notify_suggest_enabled: %v", err)
	}
	if !hasNotifySuggestEnabled {
		t.Fatal("expected users.notify_suggest_enabled after migration")
	}

	var normalizedName string
	if err := db.QueryRowContext(ctx, `SELECT normalized_name FROM cities WHERE name = ?`, "Berlin, Germany").Scan(&normalizedName); err != nil {
		t.Fatalf("query normalized_name: %v", err)
	}
	if normalizedName != "Berlin, Deutschland" {
		t.Fatalf("normalized_name = %q, want %q", normalizedName, "Berlin, Deutschland")
	}

	var (
		cityName       string
		recordedAt     string
		searchRadiusKM float64
		isOpen         bool
		diesel         float64
	)
	if err := db.QueryRowContext(ctx, `
		SELECT city_name, recorded_at, search_radius_km, is_open, diesel
		FROM price_snapshots
		WHERE station_id = ?
	`, "station-1").Scan(&cityName, &recordedAt, &searchRadiusKM, &isOpen, &diesel); err != nil {
		t.Fatalf("query migrated snapshot: %v", err)
	}
	if cityName != "Berlin" {
		t.Fatalf("city_name = %q, want %q", cityName, "Berlin")
	}
	if recordedAt != "2026-04-02T09:15:00Z" {
		t.Fatalf("recorded_at = %q, want %q", recordedAt, "2026-04-02T09:15:00Z")
	}
	if searchRadiusKM != 5 {
		t.Fatalf("search_radius_km = %v, want 5", searchRadiusKM)
	}
	if !isOpen {
		t.Fatal("expected migrated snapshot to stay open")
	}
	if diesel != 1.659 {
		t.Fatalf("diesel = %v, want 1.659", diesel)
	}
}

func TestRunMigrateReportsNoChangesForCurrentSchema(t *testing.T) {
	dbPath := seedFixtureDB(t)

	output := captureStdout(t, func() error {
		return run([]string{"migrate", "--db", dbPath, "--output", "json"})
	})

	var result migrateResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("unmarshal migrate output: %v\noutput=%s", err, output)
	}
	if len(result.Applied) != 0 {
		t.Fatalf("applied migrations = %v, want none", result.Applied)
	}
}

func TestRunMigrateDeduplicatesCitiesByNormalizedName(t *testing.T) {
	dbPath := seedDuplicateCitiesFixtureDB(t)

	output := captureStdout(t, func() error {
		return run([]string{"migrate", "--db", dbPath, "--output", "json"})
	})

	var result migrateResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("unmarshal migrate output: %v\noutput=%s", err, output)
	}
	if !containsString(result.Applied, "cities.deduplicate_normalized_name") {
		t.Fatalf("applied migrations = %v, want cities.deduplicate_normalized_name", result.Applied)
	}
	if !containsString(result.Applied, "cities.display_name") {
		t.Fatalf("applied migrations = %v, want cities.display_name", result.Applied)
	}

	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("open migrated db: %v", err)
	}
	defer db.Close()

	var count int
	if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM cities WHERE normalized_name = ?`, "Lübbecke").Scan(&count); err != nil {
		t.Fatalf("count deduplicated cities: %v", err)
	}
	if count != 1 {
		t.Fatalf("deduplicated city count = %d, want 1", count)
	}

	var (
		name        string
		displayName string
	)
	if err := db.QueryRowContext(context.Background(), `
		SELECT name, display_name
		FROM cities
		WHERE normalized_name = ?
	`, "Lübbecke").Scan(&name, &displayName); err != nil {
		t.Fatalf("query deduplicated city: %v", err)
	}
	if name != "Lübbecke" {
		t.Fatalf("kept city name = %q, want %q", name, "Lübbecke")
	}
	if displayName != "Lübbecke" {
		t.Fatalf("display_name = %q", displayName)
	}
}

func TestPersistUpdatePreservesNameOverride(t *testing.T) {
	t.Parallel()

	db := openTestDB(t)
	ctx := context.Background()

	city := cachedCity{
		QueryName:   "Berlin, Germany",
		Name:        "Berlin",
		DisplayName: "Berlin",
		Lat:         52.517389,
		Lng:         13.395131,
	}
	diesel := 1.659
	stations := []tankerStation{{
		ID:     "station-1",
		Name:   "Original Name",
		Brand:  "ARAL",
		Lat:    52.5,
		Lng:    13.4,
		Diesel: &diesel,
		IsOpen: true,
	}}
	if err := persistUpdate(ctx, db, dialectSQLite, city, stations, time.Date(2026, 4, 2, 9, 15, 0, 0, time.UTC), 5); err != nil {
		t.Fatalf("persistUpdate first: %v", err)
	}

	if _, err := db.ExecContext(ctx, `UPDATE stations SET name_override = ? WHERE id = ?`, "My Favourite", "station-1"); err != nil {
		t.Fatalf("set override: %v", err)
	}

	stations[0].Name = "Upstream Renamed"
	if err := persistUpdate(ctx, db, dialectSQLite, city, stations, time.Date(2026, 4, 2, 10, 15, 0, 0, time.UTC), 5); err != nil {
		t.Fatalf("persistUpdate second: %v", err)
	}

	var canonical string
	var override sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT name, name_override FROM stations WHERE id = ?`, "station-1").Scan(&canonical, &override); err != nil {
		t.Fatalf("query station: %v", err)
	}
	if canonical != "Upstream Renamed" {
		t.Fatalf("canonical name = %q, want %q", canonical, "Upstream Renamed")
	}
	if !override.Valid || override.String != "My Favourite" {
		t.Fatalf("name_override = %+v, want %q", override, "My Favourite")
	}
}

func TestRunListStationsPrefersNameOverride(t *testing.T) {
	dbPath := seedFixtureDB(t)

	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if _, err := db.ExecContext(context.Background(), `UPDATE stations SET name_override = ? WHERE id = ?`, "Custom Display", "station-1"); err != nil {
		t.Fatalf("set override: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	output := captureStdout(t, func() error {
		return run([]string{"list", "stations", "--db", dbPath, "--output", "json"})
	})

	var stations []stationRow
	if err := json.Unmarshal([]byte(output), &stations); err != nil {
		t.Fatalf("unmarshal stations output: %v\noutput=%s", err, output)
	}
	if len(stations) != 1 {
		t.Fatalf("len(stations) = %d, want 1", len(stations))
	}
	if stations[0].Name != "Custom Display" {
		t.Fatalf("station name = %q, want %q", stations[0].Name, "Custom Display")
	}
}

func TestRunRenameSetsAndClearsOverride(t *testing.T) {
	dbPath := seedFixtureDB(t)

	output := captureStdout(t, func() error {
		return run([]string{"rename", "--db", dbPath, "--output", "json", "station-1", "My Pump"})
	})

	var result renameResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("unmarshal rename output: %v\noutput=%s", err, output)
	}
	if result.StationID != "station-1" || result.Previous != "Test Station" || result.New != "My Pump" || result.Cleared {
		t.Fatalf("rename result = %+v", result)
	}

	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var override sql.NullString
	if err := db.QueryRowContext(context.Background(), `SELECT name_override FROM stations WHERE id = ?`, "station-1").Scan(&override); err != nil {
		t.Fatalf("query override: %v", err)
	}
	if !override.Valid || override.String != "My Pump" {
		t.Fatalf("name_override = %+v, want %q", override, "My Pump")
	}

	clearOutput := captureStdout(t, func() error {
		return run([]string{"rename", "--db", dbPath, "--output", "json", "--clear", "station-1"})
	})
	var clearResult renameResult
	if err := json.Unmarshal([]byte(clearOutput), &clearResult); err != nil {
		t.Fatalf("unmarshal clear output: %v\noutput=%s", err, clearOutput)
	}
	if clearResult.Previous != "My Pump" || clearResult.New != "Test Station" || !clearResult.Cleared {
		t.Fatalf("clear result = %+v", clearResult)
	}

	if err := db.QueryRowContext(context.Background(), `SELECT name_override FROM stations WHERE id = ?`, "station-1").Scan(&override); err != nil {
		t.Fatalf("query override after clear: %v", err)
	}
	if override.Valid {
		t.Fatalf("expected NULL name_override after clear, got %q", override.String)
	}
}

func TestRunRenameRejectsUnknownStation(t *testing.T) {
	dbPath := seedFixtureDB(t)

	err := run([]string{"rename", "--db", dbPath, "missing-id", "Whatever"})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v, want not-found error", err)
	}
}

func TestRunRenameRejectsEmptyName(t *testing.T) {
	dbPath := seedFixtureDB(t)

	err := run([]string{"rename", "--db", dbPath, "station-1", ""})
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("err = %v, want empty-name error", err)
	}
}

func TestRunRenameClearRejectsExtraName(t *testing.T) {
	dbPath := seedFixtureDB(t)

	err := run([]string{"rename", "--db", dbPath, "--clear", "station-1", "Extra"})
	if err == nil || !strings.Contains(err.Error(), "one positional") {
		t.Fatalf("err = %v, want positional-arg error", err)
	}
}

func TestResolveOutputModeRejectsConflictingFlags(t *testing.T) {
	err := run([]string{"list", "cities", "--db", filepath.Join(t.TempDir(), "test.db"), "--output", "txt", "-o", "json"})
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
	if err := initSchema(context.Background(), db, dialectSQLite); err != nil {
		t.Fatalf("initSchema: %v", err)
	}
	return db
}

func assertSnapshotCount(t *testing.T, db *sql.DB, want int) {
	t.Helper()

	var got int
	if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM price_snapshots`).Scan(&got); err != nil {
		t.Fatalf("count snapshots: %v", err)
	}
	if got != want {
		t.Fatalf("snapshot count = %d, want %d", got, want)
	}
}

func assertLatestSnapshot(t *testing.T, db *sql.DB, wantRecordedAt string, wantDiesel float64) {
	t.Helper()

	var (
		recordedAt string
		diesel     float64
	)
	if err := db.QueryRowContext(context.Background(), `
		SELECT recorded_at, diesel
		FROM price_snapshots
		ORDER BY recorded_at DESC, id DESC
		LIMIT 1
	`).Scan(&recordedAt, &diesel); err != nil {
		t.Fatalf("query latest snapshot: %v", err)
	}
	if recordedAt != wantRecordedAt {
		t.Fatalf("latest recorded_at = %q, want %q", recordedAt, wantRecordedAt)
	}
	if diesel != wantDiesel {
		t.Fatalf("latest diesel = %v, want %v", diesel, wantDiesel)
	}
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
	if err := initSchema(ctx, db, dialectSQLite); err != nil {
		t.Fatalf("initSchema: %v", err)
	}

	city := cachedCity{
		QueryName:   "Berlin, Germany",
		Name:        "Berlin",
		DisplayName: "Berlin",
		Lat:         52.517389,
		Lng:         13.395131,
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO cities (name, normalized_name, normalized_lower, display_name, lat, lng, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, city.QueryName, city.Name, citySearchKey(city.Name), city.DisplayName, city.Lat, city.Lng,
		"2026-04-02T09:00:00Z")
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
	if err := persistUpdate(ctx, db, dialectSQLite, city, stations, time.Date(2026, 4, 2, 9, 15, 0, 0, time.UTC), 5); err != nil {
		t.Fatalf("persistUpdate: %v", err)
	}

	return dbPath
}

func seedUncompactedFixtureDB(t *testing.T) string {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "uncompacted.db")
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	if err := initSchema(ctx, db, dialectSQLite); err != nil {
		t.Fatalf("initSchema: %v", err)
	}

	_, err = db.ExecContext(ctx, `
		INSERT INTO stations (
			id, name, brand, street, house_number, post_code, place, lat, lng, first_seen_at, last_seen_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, "station-1", "Test Station", "ARAL", "Test Street", "1", 32312, "Lübbecke", 52.3, 8.6, "2026-04-07T10:20:02Z", "2026-04-07T16:10:02Z")
	if err != nil {
		t.Fatalf("insert station: %v", err)
	}

	e5 := 2.189
	e10 := 2.149
	for _, snapshot := range []struct {
		recordedAt string
		diesel     float64
	}{
		{"2026-04-07T10:20:02Z", 2.349},
		{"2026-04-07T10:25:02Z", 2.349},
		{"2026-04-07T10:30:08Z", 2.389},
		{"2026-04-07T10:35:08Z", 2.389},
		{"2026-04-07T10:40:08Z", 2.389},
		{"2026-04-07T16:00:02Z", 2.349},
		{"2026-04-07T16:05:02Z", 2.349},
		{"2026-04-07T16:10:02Z", 2.349},
	} {
		_, err := db.ExecContext(ctx, `
			INSERT INTO price_snapshots (
				station_id, city_name, recorded_at, search_radius_km, is_open, e5, e10, diesel
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		`, "station-1", "Lübbecke", snapshot.recordedAt, 5, 1, e5, e10, snapshot.diesel)
		if err != nil {
			t.Fatalf("insert snapshot %s: %v", snapshot.recordedAt, err)
		}
	}

	return dbPath
}

func seedLegacyFixtureDB(t *testing.T) string {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "legacy.db")
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	legacySchema := `
	CREATE TABLE cities (
		name TEXT PRIMARY KEY,
		display_name TEXT NOT NULL,
		lat REAL NOT NULL,
		lng REAL NOT NULL,
		created_at TEXT NOT NULL
	);

	CREATE TABLE stations (
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

	CREATE TABLE price_snapshots (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		station_id TEXT NOT NULL,
		city_name TEXT NOT NULL,
		recorded_at TEXT NOT NULL,
		dist_km REAL NOT NULL,
		search_radius_km REAL NOT NULL DEFAULT 5,
		is_open INTEGER NOT NULL,
		e5 REAL,
		e10 REAL,
		diesel REAL,
		FOREIGN KEY (station_id) REFERENCES stations(id)
	);

	CREATE TABLE users (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		email TEXT NOT NULL UNIQUE,
		password_hash TEXT NOT NULL,
		is_admin INTEGER NOT NULL DEFAULT 0,
		status TEXT NOT NULL DEFAULT 'pending',
		created_at TEXT NOT NULL,
		approved_at TEXT,
		notify_method TEXT NOT NULL DEFAULT 'pushover',
		pushover_app_name TEXT NOT NULL DEFAULT 'gasoline',
		pushover_user_key TEXT NOT NULL DEFAULT '',
		pushover_token TEXT NOT NULL DEFAULT '',
		notify_days TEXT NOT NULL DEFAULT 'mon,tue,wed,thu,fri,sat,sun',
		notify_windows TEXT NOT NULL DEFAULT '07:00-21:00',
		notify_suggest_times TEXT NOT NULL DEFAULT '08:00,13:00',
		notify_check_enabled INTEGER NOT NULL DEFAULT 0,
		notify_last_suggest TEXT NOT NULL DEFAULT ''
	);
	`
	if _, err := db.ExecContext(ctx, legacySchema); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}

	if _, err := db.ExecContext(ctx, `
		INSERT INTO cities (name, display_name, lat, lng, created_at)
		VALUES (?, ?, ?, ?, ?)
	`, "Berlin, Germany", "Berlin, Deutschland", 52.517389, 13.395131, "2026-04-02T09:00:00Z"); err != nil {
		t.Fatalf("insert legacy city: %v", err)
	}

	if _, err := db.ExecContext(ctx, `
		INSERT INTO stations (
			id, name, brand, street, house_number, post_code, place, lat, lng, first_seen_at, last_seen_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, "station-1", "Test Station", "ARAL", "Test Street", "1", 10115, "Berlin", 52.5, 13.4, "2026-04-02T09:15:00Z", "2026-04-02T09:15:00Z"); err != nil {
		t.Fatalf("insert legacy station: %v", err)
	}

	if _, err := db.ExecContext(ctx, `
		INSERT INTO price_snapshots (
			station_id, city_name, recorded_at, dist_km, search_radius_km, is_open, e5, e10, diesel
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, "station-1", "Berlin", "2026-04-02T09:15:00Z", 1.25, 5, 1, 1.789, 1.729, 1.659); err != nil {
		t.Fatalf("insert legacy snapshot: %v", err)
	}

	return dbPath
}

func seedDuplicateCitiesFixtureDB(t *testing.T) string {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "duplicate-cities.db")
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	if err := initSchema(ctx, db, dialectSQLite); err != nil {
		t.Fatalf("initSchema: %v", err)
	}

	rows := []struct {
		name        string
		displayName string
		lat         float64
		lng         float64
		createdAt   string
	}{
		{"Lübbecke", "Lübbecke", 52.306990, 8.614230, "2026-04-10T13:48:51Z"},
		{"Luebbecke", "Lübbecke, Kreis Minden-Lübbecke, Nordrhein-Westfalen, 32312, Deutschland", 52.3027209, 8.6183054, "2026-04-10T13:51:57Z"},
		{"", "Lübbecke, Kreis Minden-Lübbecke, Nordrhein-Westfalen, 32312, Deutschland", 52.3027209, 8.6183054, "2026-04-10T13:51:57Z"},
	}
	for _, row := range rows {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO cities (name, normalized_name, display_name, lat, lng, created_at)
			VALUES (?, ?, ?, ?, ?, ?)
		`, row.name, "Lübbecke", row.displayName, row.lat, row.lng, row.createdAt); err != nil {
			t.Fatalf("insert duplicate city %q: %v", row.name, err)
		}
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

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
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

func zipResponse(t *testing.T, files map[string]string) *http.Response {
	t.Helper()

	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     fmt.Sprintf("%d %s", http.StatusOK, http.StatusText(http.StatusOK)),
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewReader(buildZipBytes(t, files))),
	}
}

func buildZipBytes(t *testing.T, files map[string]string) []byte {
	t.Helper()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("create zip entry %s: %v", name, err)
		}
		if _, err := io.WriteString(w, content); err != nil {
			t.Fatalf("write zip entry %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip writer: %v", err)
	}
	return buf.Bytes()
}

// sawtoothIntervals builds the regulated daily price shape: cheap overnight
// and morning, a single big raise at 12:00, then stepwise decay. The
// duration-weighted median of one day is base-0.05, so intraday offsets range
// from -0.07 (late morning) to +0.30 (right after the jump).
func sawtoothIntervals(stationID string, firstDay time.Time, days int, baseFor func(day int) float64) []priceInterval {
	segments := []struct {
		startHour int
		offset    float64
	}{
		{0, -0.05}, {8, -0.10}, {11, -0.12}, {12, 0.25}, {14, 0.10}, {18, 0.02}, {22, -0.05},
	}
	var intervals []priceInterval
	for day := 0; day < days; day++ {
		dayStart := firstDay.AddDate(0, 0, day)
		base := baseFor(day)
		for i, segment := range segments {
			end := dayStart.AddDate(0, 0, 1)
			if i+1 < len(segments) {
				end = dayStart.Add(time.Duration(segments[i+1].startHour) * time.Hour)
			}
			intervals = append(intervals, priceInterval{
				StationID:   stationID,
				StationName: stationID,
				Start:       dayStart.Add(time.Duration(segment.startHour) * time.Hour),
				End:         end,
				Price:       base + segment.offset,
			})
		}
	}
	return intervals
}

func TestInferJumpAnchorHourDetectsNoonJump(t *testing.T) {
	firstDay := time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC)
	intervals := sawtoothIntervals("s1", firstDay, 10, func(int) float64 { return 2.00 })
	if anchor := inferJumpAnchorHour(intervals, time.UTC); anchor != 12 {
		t.Fatalf("anchor = %d, want 12", anchor)
	}

	var flat []priceInterval
	for day := 0; day < 10; day++ {
		flat = append(flat, priceInterval{
			StationID: "s1",
			Start:     firstDay.AddDate(0, 0, day),
			End:       firstDay.AddDate(0, 0, day+1),
			Price:     2.00,
		})
	}
	if anchor := inferJumpAnchorHour(flat, time.UTC); anchor != 0 {
		t.Fatalf("anchor for flat prices = %d, want 0", anchor)
	}
}

func TestBuildForecastModelFollowsBaselineShift(t *testing.T) {
	firstDay := time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC)
	// One week at 1.95, then one week at 2.10: the forecast must track the new
	// level instead of the smeared median of both regimes.
	intervals := sawtoothIntervals("s1", firstDay, 14, func(day int) float64 {
		if day < 7 {
			return 1.95
		}
		return 2.10
	})
	now := time.Date(2026, 4, 24, 0, 30, 0, 0, time.UTC)

	model := buildForecastModel(intervals, now, time.UTC)
	if model.JumpAnchorHour != 12 {
		t.Fatalf("jump anchor = %d, want 12", model.JumpAnchorHour)
	}
	station := model.Stations["s1"]
	if !station.OffsetMode {
		t.Fatal("station not in offset mode despite two weeks of data")
	}
	if station.BaselineForecast < 2.04 || station.BaselineForecast > 2.06 {
		t.Fatalf("baseline forecast = %.4f, want ~2.05 (current regime), not the old 1.90 level", station.BaselineForecast)
	}

	score, ok := scoreForecast(model, "s1", time.Date(2026, 4, 24, 11, 0, 0, 0, time.UTC))
	if !ok {
		t.Fatal("scoreForecast returned !ok")
	}
	if score.PredictedPrice < 1.97 || score.PredictedPrice > 2.02 {
		t.Fatalf("predicted 11:00 price = %.4f, want ~1.98 on the new regime (old regime would be ~1.83)", score.PredictedPrice)
	}
}

func TestBuildForecastModelSparseHistoryFallsBackToAbsolute(t *testing.T) {
	firstDay := time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC)
	intervals := sawtoothIntervals("s1", firstDay, 2, func(int) float64 { return 2.00 })
	now := time.Date(2026, 4, 12, 0, 30, 0, 0, time.UTC)

	model := buildForecastModel(intervals, now, time.UTC)
	station := model.Stations["s1"]
	if station.OffsetMode {
		t.Fatal("station in offset mode with only two days of data")
	}
	score, ok := scoreForecast(model, "s1", time.Date(now.Year(), now.Month(), now.Day(), 11, 0, 0, 0, time.UTC))
	if !ok {
		t.Fatal("scoreForecast returned !ok in absolute fallback mode")
	}
	if score.PredictedPrice < 1.80 || score.PredictedPrice > 2.40 {
		t.Fatalf("absolute-mode prediction = %.4f, want a plausible absolute price", score.PredictedPrice)
	}
}

func TestScoreForecastOffsetModeIgnoresRecentLevel(t *testing.T) {
	// The recent bucket carries a poisoned +0.50 level. In offset mode the
	// blend must not let it damp the intraday shape; in absolute mode it
	// stays part of the level estimate.
	weekdaySamples := []priceSample{
		{Price: -0.10, Weight: 60, Date: "2026-04-06"},
		{Price: -0.10, Weight: 60, Date: "2026-04-13"},
		{Price: -0.10, Weight: 60, Date: "2026-04-20"},
	}
	model := forecastModel{
		Stations: map[string]forecastStation{
			"s": {OffsetMode: true, BaselineForecast: 2.00},
		},
		WeekdayHour: map[stationWeekdayHourKey][]priceSample{
			{StationID: "s", Weekday: time.Monday, Hour: 11}: weekdaySamples,
		},
		Hour: map[stationHourKey][]priceSample{
			{StationID: "s", Hour: 11}: {{Price: -0.10, Weight: 60}},
		},
		Recent: map[string][]priceSample{
			"s": {{Price: 0.50, Weight: 600}},
		},
	}
	target := time.Date(2026, 4, 27, 11, 0, 0, 0, time.UTC) // Monday
	score, ok := scoreForecast(model, "s", target)
	if !ok {
		t.Fatal("scoreForecast returned !ok")
	}
	if score.PredictedPrice < 1.899 || score.PredictedPrice > 1.901 {
		t.Fatalf("offset-mode prediction = %.4f, want 1.90 (recent level ignored)", score.PredictedPrice)
	}

	absolute := model
	absolute.Stations = map[string]forecastStation{"s": {}}
	absolute.WeekdayHour = map[stationWeekdayHourKey][]priceSample{}
	absolute.Hour = map[stationHourKey][]priceSample{
		{StationID: "s", Hour: 11}: {{Price: 1.90, Weight: 60}},
	}
	absolute.Recent = map[string][]priceSample{
		"s": {{Price: 2.10, Weight: 60}},
	}
	score, ok = scoreForecast(absolute, "s", target)
	if !ok {
		t.Fatal("scoreForecast returned !ok in absolute mode")
	}
	if score.PredictedPrice < 1.949 || score.PredictedPrice > 1.951 {
		t.Fatalf("absolute-mode prediction = %.4f, want 1.95 (recent still blended)", score.PredictedPrice)
	}
}

func TestEstimateBaselineDriftDampsAndCaps(t *testing.T) {
	nowLocal := time.Date(2026, 4, 25, 9, 0, 0, 0, time.UTC)
	days := func(values ...float64) map[string]dayBaseline {
		result := make(map[string]dayBaseline)
		for i, value := range values {
			day := nowLocal.AddDate(0, 0, -len(values)+i).Format("2006-01-02")
			result[day] = dayBaseline{Value: value, CoverageMinutes: 1440}
		}
		return result
	}

	// Steady +1 ct/day across six adjacent deltas: damped to +0.5 ct.
	rising := map[string]map[string]dayBaseline{"s1": days(2.00, 2.01, 2.02, 2.03, 2.04, 2.05, 2.06)}
	drift := estimateBaselineDrift(rising, 0, nowLocal)
	if drift < 0.0049 || drift > 0.0051 {
		t.Fatalf("drift = %.4f, want 0.005 (damped +0.01/day)", drift)
	}

	// A violent trend is capped.
	steep := map[string]map[string]dayBaseline{"s1": days(2.00, 2.06, 2.12, 2.18, 2.24, 2.30, 2.36)}
	if drift := estimateBaselineDrift(steep, 0, nowLocal); drift != baselineDriftMaxAbsPerDay {
		t.Fatalf("steep drift = %.4f, want capped %.2f", drift, baselineDriftMaxAbsPerDay)
	}

	// Too few deltas: no drift.
	sparse := map[string]map[string]dayBaseline{"s1": days(2.00, 2.01, 2.02)}
	if drift := estimateBaselineDrift(sparse, 0, nowLocal); drift != 0 {
		t.Fatalf("sparse drift = %.4f, want 0", drift)
	}

	// Flat market: exactly zero.
	flat := map[string]map[string]dayBaseline{"s1": days(2.00, 2.00, 2.00, 2.00, 2.00, 2.00, 2.00)}
	if drift := estimateBaselineDrift(flat, 0, nowLocal); drift != 0 {
		t.Fatalf("flat drift = %.4f, want 0", drift)
	}
}

func TestScoreForecastExtrapolatesBaselineDrift(t *testing.T) {
	nowLocal := time.Date(2026, 4, 25, 9, 0, 0, 0, time.UTC)
	model := forecastModel{
		Stations: map[string]forecastStation{
			"s": {OffsetMode: true, BaselineForecast: 2.00},
		},
		Hour: map[stationHourKey][]priceSample{
			{StationID: "s", Hour: 9}: {{Price: 0, Weight: 60}},
		},
		Recent:         map[string][]priceSample{"s": {{Price: 0, Weight: 60}}},
		JumpAnchorHour: 12,
		NowLocal:       nowLocal,
		BaselineDrift:  0.005,
	}

	// Same pricing day (before the next noon crossing): no drift applied.
	score, ok := scoreForecast(model, "s", nowLocal.Add(30*time.Minute))
	if !ok || score.PredictedPrice != 2.00 {
		t.Fatalf("same-day prediction = %.4f, want 2.00", score.PredictedPrice)
	}
	// Two noon crossings ahead: two days of drift.
	score, ok = scoreForecast(model, "s", nowLocal.AddDate(0, 0, 2))
	if !ok {
		t.Fatal("scoreForecast returned !ok")
	}
	if score.PredictedPrice < 2.0099 || score.PredictedPrice > 2.0101 {
		t.Fatalf("two-days-out prediction = %.4f, want 2.01 (+2x drift)", score.PredictedPrice)
	}
}

func TestGenerateSuggestionsOrdersByRawScoreNotRoundedDisplay(t *testing.T) {
	// Two stations whose raw prices differ but round to the same cent. The
	// raw-cheaper one must win, with and without a non-cent-aligned display
	// bias — under rounded ordering the two would tie (falling through to
	// name order, which here prefers the raw-more-expensive station) and the
	// bias could then split the tie differently.
	hourSamples := func(price float64) []priceSample {
		return []priceSample{{Price: price, Weight: 60}}
	}
	model := forecastModel{
		Stations: map[string]forecastStation{
			"cheap-raw": {Station: suggestionStationRow{ID: "cheap-raw", Name: "zzz station"}},
			"dear-raw":  {Station: suggestionStationRow{ID: "dear-raw", Name: "aaa station"}},
		},
		Hour: map[stationHourKey][]priceSample{
			{StationID: "cheap-raw", Hour: 23}: hourSamples(1.796),
			{StationID: "dear-raw", Hour: 23}:  hourSamples(1.804),
		},
		Recent: map[string][]priceSample{
			"cheap-raw": hourSamples(1.796),
			"dear-raw":  hourSamples(1.804),
		},
	}
	now := time.Date(2026, 4, 24, 22, 30, 0, 0, time.UTC)

	for _, bias := range []float64{0, 0.0031} {
		model.SuggestionBias = bias
		suggestions := generateSuggestions(model, "diesel", now, time.UTC, 1, 1)
		if len(suggestions) == 0 {
			t.Fatalf("no suggestions with bias %.4f", bias)
		}
		if suggestions[0].StationID != "cheap-raw" {
			t.Fatalf("bias %.4f selected %s, want the raw-cheaper station", bias, suggestions[0].StationID)
		}
	}
}

func TestPricingDayAnchorsAtJumpHour(t *testing.T) {
	early := time.Date(2026, 4, 11, 5, 0, 0, 0, time.UTC)
	if day := pricingDay(early, 12); day != "2026-04-10" {
		t.Fatalf("pricingDay(05:00, anchor 12) = %s, want 2026-04-10", day)
	}
	if day := pricingDay(early, 0); day != "2026-04-11" {
		t.Fatalf("pricingDay(05:00, anchor 0) = %s, want 2026-04-11", day)
	}
}

func TestComputeDailyBaselinesSkipsSparseAndOpenDays(t *testing.T) {
	dayOne := time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC)
	var buckets []hourBucket
	for hour := 0; hour < 23; hour++ {
		buckets = append(buckets, hourBucket{StationID: "s1", Start: dayOne.Add(time.Duration(hour) * time.Hour), Minutes: 60, Price: 2.30})
	}
	buckets = append(buckets, hourBucket{StationID: "s1", Start: dayOne.Add(23 * time.Hour), Minutes: 60, Price: 2.20})
	// Day two has only three hours of coverage and must be skipped.
	dayTwo := dayOne.AddDate(0, 0, 1)
	for hour := 0; hour < 3; hour++ {
		buckets = append(buckets, hourBucket{StationID: "s1", Start: dayTwo.Add(time.Duration(hour) * time.Hour), Minutes: 60, Price: 1.00})
	}
	// The open current day never yields a baseline.
	nowLocal := time.Date(2026, 4, 12, 12, 30, 0, 0, time.UTC)
	buckets = append(buckets, hourBucket{StationID: "s1", Start: nowLocal.Add(-time.Hour), Minutes: 600, Price: 1.00})

	baselines := computeDailyBaselines(buckets, 0, nowLocal)
	stationDays := baselines["s1"]
	if len(stationDays) != 1 {
		t.Fatalf("baseline days = %d, want 1: %+v", len(stationDays), stationDays)
	}
	baseline, ok := stationDays["2026-04-10"]
	if !ok {
		t.Fatalf("missing baseline for 2026-04-10: %+v", stationDays)
	}
	if baseline.Value != 2.30 {
		t.Fatalf("baseline = %.3f, want duration-weighted median 2.300", baseline.Value)
	}
	if baseline.CoverageMinutes != 1440 {
		t.Fatalf("coverage = %.0f, want 1440", baseline.CoverageMinutes)
	}
}

func TestEstimateCurrentBaselineDeShapesPartialRegime(t *testing.T) {
	model := forecastModel{
		Hour: map[stationHourKey][]priceSample{
			{StationID: "s1", Hour: 12}: {{Price: 0.30, Weight: 60}},
			{StationID: "s1", Hour: 14}: {{Price: 0.15, Weight: 60}},
		},
	}
	day := time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC)
	buckets := []hourBucket{
		{StationID: "s1", Start: day.Add(12 * time.Hour), Minutes: 60, Price: 2.30},
		{StationID: "s1", Start: day.Add(14 * time.Hour), Minutes: 60, Price: 2.15},
	}
	estimate, ok := estimateCurrentBaseline(model, "s1", buckets)
	if !ok {
		t.Fatal("estimateCurrentBaseline returned !ok")
	}
	if estimate < 1.999 || estimate > 2.001 {
		t.Fatalf("estimate = %.4f, want 2.00 after de-shaping", estimate)
	}

	short := []hourBucket{{StationID: "s1", Start: day.Add(12 * time.Hour), Minutes: 30, Price: 2.30}}
	if _, ok := estimateCurrentBaseline(model, "s1", short); ok {
		t.Fatal("estimateCurrentBaseline accepted 30 minutes of coverage")
	}
}

func insertSawtoothDay(t *testing.T, db *sql.DB, stationID, cityName string, day time.Time, base float64) {
	t.Helper()
	for _, segment := range []struct {
		hour   int
		offset float64
	}{
		{0, -0.05}, {8, -0.10}, {11, -0.12}, {12, 0.25}, {14, 0.10}, {18, 0.02}, {22, -0.05},
	} {
		insertSuggestSnapshot(t, db, stationID, cityName, day.Add(time.Duration(segment.hour)*time.Hour), base+segment.offset, true)
	}
}

// insertSawtoothDayDieselOnly is insertSawtoothDay with no e5/e10 prices, for
// fixtures that need one fuel to have history and the others none.
func insertSawtoothDayDieselOnly(t *testing.T, db *sql.DB, stationID, cityName string, day time.Time, base float64) {
	t.Helper()
	for _, segment := range []struct {
		hour   int
		offset float64
	}{
		{0, -0.05}, {8, -0.10}, {11, -0.12}, {12, 0.25}, {14, 0.10}, {18, 0.02}, {22, -0.05},
	} {
		insertSuggestSnapshotDieselOnly(t, db, stationID, cityName, day.Add(time.Duration(segment.hour)*time.Hour), base+segment.offset, true)
	}
}

func TestSuggestGasPrefersPreJumpWindowWithNoonJump(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	city := cachedCity{QueryName: "Berlin", Name: "Berlin", DisplayName: "Berlin", Lat: 52.517389, Lng: 13.395131}
	insertSuggestCity(t, db, city)
	insertSuggestStation(t, db, "station-1", "Station 1", 52.517389, 13.395131)
	for day := 10; day <= 24; day++ {
		insertSawtoothDay(t, db, "station-1", "Berlin", time.Date(2026, 4, day, 0, 0, 0, 0, time.UTC), 2.00)
	}

	suggestions, err := suggestGas(ctx, db, suggestOptions{
		Fuel:        "diesel",
		HistoryDays: 30,
		PredictDays: 1,
		LimitPerDay: 1,
		Now:         time.Date(2026, 4, 25, 9, 30, 0, 0, time.UTC),
		Location:    time.UTC,
	})
	if err != nil {
		t.Fatalf("suggestGas: %v", err)
	}
	if len(suggestions) != 1 {
		t.Fatalf("len(suggestions) = %d, want 1: %+v", len(suggestions), suggestions)
	}
	suggestion := suggestions[0]
	if suggestion.StartTime != "11:00" || suggestion.EndTime != "12:00" {
		t.Fatalf("window = %s-%s, want the cheap 11:00-12:00 pre-jump hour", suggestion.StartTime, suggestion.EndTime)
	}
	if suggestion.PredictedPrice < 1.86 || suggestion.PredictedPrice > 1.90 {
		t.Fatalf("predicted = %.3f, want ~1.88", suggestion.PredictedPrice)
	}
}

func TestCheckGasStaysRegimeRelativeAfterMarketWideJump(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	city := cachedCity{QueryName: "Berlin", Name: "Berlin", DisplayName: "Berlin", Lat: 52.517389, Lng: 13.395131}
	insertSuggestCity(t, db, city)
	insertSuggestStation(t, db, "station-1", "Station 1", 52.517389, 13.395131)
	for day := 10; day <= 24; day++ {
		insertSawtoothDay(t, db, "station-1", "Berlin", time.Date(2026, 4, day, 0, 0, 0, 0, time.UTC), 1.90)
	}
	// Today the noon raise lands the market on a new, 15 cent higher level.
	today := time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC)
	insertSuggestSnapshot(t, db, "station-1", "Berlin", today, 1.85, true)
	insertSuggestSnapshot(t, db, "station-1", "Berlin", today.Add(8*time.Hour), 1.80, true)
	insertSuggestSnapshot(t, db, "station-1", "Berlin", today.Add(11*time.Hour), 1.78, true)
	insertSuggestSnapshot(t, db, "station-1", "Berlin", today.Add(12*time.Hour), 2.30, true)
	insertSuggestSnapshot(t, db, "station-1", "Berlin", today.Add(14*time.Hour), 2.15, true)
	insertSuggestSnapshot(t, db, "station-1", "Berlin", today.Add(18*time.Hour), 2.07, true)

	checks, err := checkGas(ctx, db, checkOptions{
		Fuel:        "diesel",
		HistoryDays: 30,
		PredictDays: 1,
		Limit:       5,
		Now:         time.Date(2026, 4, 25, 18, 30, 0, 0, time.UTC),
		Location:    time.UTC,
	})
	if err != nil {
		t.Fatalf("checkGas: %v", err)
	}
	if len(checks) != 1 {
		t.Fatalf("len(checks) = %d, want 1: %+v", len(checks), checks)
	}
	check := checks[0]
	// 2.07 sits far above every absolute price of the last two weeks, so the
	// old absolute-percentile model would read "high". Relative to the fresh
	// post-jump level it is a typical evening price.
	if check.Verdict != "typical" {
		t.Fatalf("verdict = %s (percentile %.1f), want typical despite the market-wide jump", check.Verdict, check.HistoryPercentile)
	}
	if check.Recommendation != "wait" || !check.ExpectedLower {
		t.Fatalf("recommendation/expected_lower = %s/%t, want wait/true (late-evening dip ahead)", check.Recommendation, check.ExpectedLower)
	}
	if check.BestFutureStartTime != "22:00" {
		t.Fatalf("best future start = %s, want 22:00", check.BestFutureStartTime)
	}
}

func TestInferJumpAnchorHourIgnoresRisesAcrossGaps(t *testing.T) {
	day := time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC)
	var intervals []priceInterval
	// Station closed overnight: each day is one open interval, and the price
	// rise materializes across the closed gap. It must not be attributed to
	// the 06:00 reopening hour.
	for d := 0; d < 10; d++ {
		intervals = append(intervals, priceInterval{
			StationID: "s1",
			Start:     day.AddDate(0, 0, d).Add(6 * time.Hour),
			End:       day.AddDate(0, 0, d).Add(20 * time.Hour),
			Price:     2.00 + float64(d)*0.01,
		})
	}
	if anchor := inferJumpAnchorHour(intervals, time.UTC); anchor != 0 {
		t.Fatalf("anchor = %d, want 0 (rises across closure gaps must not count)", anchor)
	}
}

func TestLoadSnapshotScanDropsStationsThatStoppedBeingFed(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 4, 26, 15, 30, 0, 0, time.UTC)

	insertSuggestCity(t, db, cachedCity{QueryName: "Berlin", Name: "Berlin", DisplayName: "Berlin", Lat: 52.517389, Lng: 13.395131})
	insertSuggestStation(t, db, "live", "Live Station", 52.517389, 13.395131)
	insertSuggestStation(t, db, "dropped", "Dropped Station", 52.517389, 13.395131)

	// Both stations have history; only one is still receiving updates. A target
	// that was removed or shrunk leaves its stations looking exactly like this.
	for day := 20; day <= 25; day++ {
		insertSuggestSnapshot(t, db, "live", "Berlin", time.Date(2026, 4, day, 18, 0, 0, 0, time.UTC), 2.000, true)
	}
	// The dropped station's newest snapshot is six days old, well past the
	// freshness window even though it is inside the history window.
	for day := 18; day <= 20; day++ {
		insertSuggestSnapshot(t, db, "dropped", "Berlin", time.Date(2026, 4, day, 18, 0, 0, 0, time.UTC), 1.500, true)
	}
	insertSuggestSnapshot(t, db, "live", "Berlin", now.Add(-30*time.Minute), 1.990, true)

	scan, err := loadSnapshotScan(ctx, db, now.AddDate(0, 0, -10), now)
	if err != nil {
		t.Fatalf("loadSnapshotScan: %v", err)
	}
	if _, ok := scan.Stations["live"]; !ok {
		t.Fatal("a station still being fed must stay in scope")
	}
	if _, ok := scan.Stations["dropped"]; ok {
		t.Fatalf("a station whose last snapshot is older than %v must leave scope", stationFreshness)
	}
	for _, row := range scan.Rows {
		if row.StationID != "live" {
			t.Fatalf("scan carries rows for out-of-scope station %q", row.StationID)
		}
	}
}

func TestLoadSnapshotScanAttributesStationsToTheirOwningCity(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 4, 26, 15, 30, 0, 0, time.UTC)

	insertSuggestCity(t, db, cachedCity{QueryName: "Berlin", Name: "Berlin", DisplayName: "Berlin", Lat: 52.517389, Lng: 13.395131})
	insertSuggestCity(t, db, cachedCity{QueryName: "Potsdam", Name: "Potsdam", DisplayName: "Potsdam", Lat: 52.4, Lng: 13.06})
	insertSuggestStation(t, db, "in-berlin", "Berlin Station", 52.517389, 13.395131)
	insertSuggestStation(t, db, "in-potsdam", "Potsdam Station", 52.4, 13.06)
	insertSuggestSnapshot(t, db, "in-berlin", "Berlin", now.Add(-time.Hour), 2.000, true)
	insertSuggestSnapshot(t, db, "in-potsdam", "Potsdam", now.Add(-time.Hour), 1.900, true)
	// Ownership moved to a nearer centre: the newest snapshot decides, so this
	// station counts as Potsdam's even though it started out as Berlin's.
	insertSuggestStation(t, db, "moved", "Moved Station", 52.41, 13.07)
	insertSuggestSnapshot(t, db, "moved", "Berlin", now.Add(-3*time.Hour), 1.950, true)
	insertSuggestSnapshot(t, db, "moved", "Potsdam", now.Add(-time.Hour), 1.950, true)

	scan, err := loadSnapshotScan(ctx, db, now.AddDate(0, 0, -10), now)
	if err != nil {
		t.Fatalf("loadSnapshotScan: %v", err)
	}
	// Owning-city distance is opt-in: notifications measure from the subscriber
	// instead, so the scan does not pay for the cities lookup unless asked.
	if err := scan.fillOwningCityDistances(ctx, db); err != nil {
		t.Fatalf("fillOwningCityDistances: %v", err)
	}
	for id, wantCity := range map[string]string{"in-berlin": "Berlin", "in-potsdam": "Potsdam", "moved": "Potsdam"} {
		station, ok := scan.Stations[id]
		if !ok {
			t.Fatalf("station %q missing from the scan", id)
		}
		if station.City != wantCity {
			t.Errorf("station %q city = %q, want %q", id, station.City, wantCity)
		}
	}
	// Distance is measured to the owning centre, not to some global origin.
	if d := scan.Stations["in-potsdam"].DistanceKM; d > 0.1 {
		t.Errorf("distance for a station at its own city centre = %.2f, want ~0", d)
	}
	if d := scan.Stations["moved"].DistanceKM; d < 0.5 || d > 5 {
		t.Errorf("distance for the moved station = %.2f, want a short hop to the Potsdam centre", d)
	}
}

func TestForecastModelWithinRadiusFiltersAndRestatesDistance(t *testing.T) {
	// Berlin centre, a station ~8 km west of it, and Hamburg.
	model := forecastModel{
		Stations: map[string]forecastStation{
			"here": {Station: suggestionStationRow{ID: "here", Lat: 52.517389, Lng: 13.395131}},
			"near": {Station: suggestionStationRow{ID: "near", Lat: 52.517389, Lng: 13.277}},
			"far":  {Station: suggestionStationRow{ID: "far", Lat: 53.550556, Lng: 9.993333}},
		},
		Hour:           map[stationHourKey][]priceSample{{StationID: "far", Hour: 8}: {{Price: 1.5}}},
		JumpAnchorHour: 12,
	}
	view := model.withinRadius(52.517389, 13.395131, 25)
	if len(view.Stations) != 2 {
		t.Fatalf("view has %d stations, want the two inside 25 km", len(view.Stations))
	}
	if _, ok := view.Stations["far"]; ok {
		t.Error("a station outside the radius must be filtered out")
	}
	// Distances are restated from the point that was asked about.
	if d := view.Stations["here"].Station.DistanceKM; d > 0.1 {
		t.Errorf("distance at the point itself = %.2f, want ~0", d)
	}
	if d := view.Stations["near"].Station.DistanceKM; d < 7 || d > 9 {
		t.Errorf("distance for the nearby station = %.2f, want ~8", d)
	}
	if len(model.Stations) != 3 {
		t.Error("withinRadius mutated the model it filtered")
	}
	// The sample maps are shared rather than rebuilt, and the rest carries over.
	if len(view.Hour) != len(model.Hour) || view.JumpAnchorHour != 12 {
		t.Errorf("view = %d hour keys anchor %d, want the model's own", len(view.Hour), view.JumpAnchorHour)
	}
	if len(model.withinRadius(0, 0, 1).Stations) != 0 {
		t.Error("an area with no stations must yield an empty view")
	}
}

func TestLoadSnapshotScanDoesNotDuplicateHistoryForCitiesSharingANormalizedName(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 4, 26, 15, 30, 0, 0, time.UTC)

	// cities.normalized_name has no unique constraint: the same place is cached
	// once per query string an admin ever used. Both of these normalize to
	// "Berlin", which is what the snapshots are filed under.
	insertSuggestCity(t, db, cachedCity{QueryName: "Berlin", Name: "Berlin", DisplayName: "Berlin", Lat: 52.517389, Lng: 13.395131})
	insertSuggestCity(t, db, cachedCity{QueryName: "Berlin, Germany", Name: "Berlin", DisplayName: "Berlin, Deutschland", Lat: 52.4, Lng: 13.0})
	insertSuggestStation(t, db, "station-1", "Station 1", 52.517389, 13.395131)

	const snapshots = 4
	for i := 0; i < snapshots; i++ {
		insertSuggestSnapshot(t, db, "station-1", "Berlin", now.Add(-time.Duration(i+1)*time.Hour), 2.000, true)
	}

	scan, err := loadSnapshotScan(ctx, db, now.AddDate(0, 0, -10), now)
	if err != nil {
		t.Fatalf("loadSnapshotScan: %v", err)
	}
	if err := scan.fillOwningCityDistances(ctx, db); err != nil {
		t.Fatalf("fillOwningCityDistances: %v", err)
	}
	if len(scan.Rows) != snapshots {
		t.Fatalf("scan rows = %d, want %d: a second cached spelling must not duplicate the history", len(scan.Rows), snapshots)
	}
	// The centre is picked deterministically — the row whose query name already
	// is the normalized name — so the station sits at its own city centre.
	if d := scan.Stations["station-1"].DistanceKM; d > 0.1 {
		t.Errorf("distance = %.2f, want ~0 from the preferred cached city row", d)
	}
}

func TestLoadCityCentresPrefersTheCanonicalRow(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Inserted worst-first so a naive "last row wins" would pick the wrong one.
	insertSuggestCity(t, db, cachedCity{QueryName: "Berlin, Germany", Name: "Berlin", DisplayName: "Berlin, Deutschland", Lat: 52.4, Lng: 13.0})
	insertSuggestCity(t, db, cachedCity{QueryName: "Berlin", Name: "Berlin", DisplayName: "Berlin", Lat: 52.517389, Lng: 13.395131})
	insertSuggestCity(t, db, cachedCity{QueryName: "Hamburg", Name: "Hamburg", DisplayName: "Hamburg", Lat: 53.550556, Lng: 9.993333})

	centres, err := loadCityCentres(ctx, db, []string{"Berlin", "Hamburg", "Nowhere"})
	if err != nil {
		t.Fatalf("loadCityCentres: %v", err)
	}
	if len(centres) != 2 {
		t.Fatalf("centres = %+v, want only the two cached cities", centres)
	}
	if centres["Berlin"].Lat != 52.517389 || centres["Berlin"].Lng != 13.395131 {
		t.Errorf("Berlin centre = %+v, want the canonical row's coordinates", centres["Berlin"])
	}
	if _, err := loadCityCentres(ctx, db, nil); err != nil {
		t.Errorf("loadCityCentres with no names: %v", err)
	}
}

func TestLookupCityNormalizedName(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	insertSuggestCity(t, db, cachedCity{
		QueryName: "Berlin, Germany", Name: "Berlin", DisplayName: "Berlin, Deutschland",
		Lat: 52.517389, Lng: 13.395131,
	})

	// The query string, the normalized name and the display name all resolve to
	// the name the snapshots are filed under.
	for _, in := range []string{"Berlin, Germany", "Berlin", "Berlin, Deutschland", "  Berlin  "} {
		got, err := lookupCityNormalizedName(ctx, db, in)
		if err != nil {
			t.Fatalf("lookupCityNormalizedName(%q): %v", in, err)
		}
		if got != "Berlin" {
			t.Errorf("lookupCityNormalizedName(%q) = %q, want Berlin", in, got)
		}
	}
	if _, err := lookupCityNormalizedName(ctx, db, "Nowhere"); err == nil {
		t.Error("an uncached city must report an error")
	}
}

// The regression this guards: deleting an update target leaves its cities row
// behind, and the remaining target keeps the station fresh forever, so ownership
// used to stay on the removed city indefinitely.
func TestRunUpdateTransfersOwnershipWhenATargetIsRemoved(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "removed-target.db")
	t.Setenv(envAPIKeyName, "test-key")
	restore := overlapStub(t, func(string) string { return "1.500" }, "")
	defer restore()

	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := initSchema(context.Background(), db, dialectSQLite); err != nil {
		t.Fatalf("initSchema: %v", err)
	}
	insertUpdateTargetRow(t, db, "Berlin", 25)
	insertUpdateTargetRow(t, db, "Potsdam", 25)
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	// Both targets reach the station; the nearer one owns it.
	_ = captureStdout(t, func() error {
		return run([]string{"update", "--db", dbPath})
	})
	if owner, _ := sharedStationOwner(t, dbPath); owner != "Potsdam" {
		t.Fatalf("after the configured sweep: owner = %q, want Potsdam", owner)
	}

	// The admin removes the owning target. Berlin keeps the station fed, so
	// freshness alone will never let it go.
	db, err = openDB(dbPath)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	if _, err := db.ExecContext(context.Background(), `DELETE FROM update_targets WHERE city = 'Potsdam'`); err != nil {
		t.Fatalf("delete target: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	_ = captureStdout(t, func() error {
		return run([]string{"update", "--db", dbPath})
	})
	if owner, rows := sharedStationOwner(t, dbPath); owner != "Berlin" || rows != 1 {
		t.Fatalf("after removing Potsdam: owner = %q in %d rows, want Berlin in 1", owner, rows)
	}
}

// The same for a radius shrink: the target stays, but no longer reaches.
func TestRunUpdateTransfersOwnershipWhenATargetShrinks(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "shrunk-target.db")
	t.Setenv(envAPIKeyName, "test-key")
	restore := overlapStub(t, func(string) string { return "1.500" }, "")
	defer restore()

	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := initSchema(context.Background(), db, dialectSQLite); err != nil {
		t.Fatalf("initSchema: %v", err)
	}
	insertUpdateTargetRow(t, db, "Berlin", 25)
	insertUpdateTargetRow(t, db, "Potsdam", 25)
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	_ = captureStdout(t, func() error {
		return run([]string{"update", "--db", dbPath})
	})
	if owner, _ := sharedStationOwner(t, dbPath); owner != "Potsdam" {
		t.Fatalf("after the configured sweep: owner = %q, want Potsdam", owner)
	}

	// The station sits ~3.5 km out, so a 2 km radius no longer contains it.
	db, err = openDB(dbPath)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	if _, err := db.ExecContext(context.Background(), `UPDATE update_targets SET radius_km = 2 WHERE city = 'Potsdam'`); err != nil {
		t.Fatalf("shrink target: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	_ = captureStdout(t, func() error {
		return run([]string{"update", "--db", dbPath})
	})
	if owner, rows := sharedStationOwner(t, dbPath); owner != "Berlin" || rows != 1 {
		t.Fatalf("after shrinking Potsdam: owner = %q in %d rows, want Berlin in 1", owner, rows)
	}
}

// TestMigrateBackfillsCitySearchKeys covers the upgrade path for the typeahead's
// folded column: an install that predates it has to come out of `migrate` with
// every city findable, including the ones the database engine could not have
// folded correctly itself.
func TestMigrateBackfillsCitySearchKeys(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "cities.db")
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	// A cities table as it stood before normalized_lower existed.
	if _, err := db.ExecContext(ctx, `CREATE TABLE cities (
		name TEXT PRIMARY KEY,
		normalized_name TEXT NOT NULL,
		display_name TEXT NOT NULL,
		lat REAL NOT NULL,
		lng REAL NOT NULL,
		created_at TEXT NOT NULL
	)`); err != nil {
		t.Fatalf("legacy cities: %v", err)
	}
	for _, name := range []string{"Lübbecke", "LÜBZ", "enzberg"} {
		if _, err := db.ExecContext(ctx, `INSERT INTO cities
			(name, normalized_name, display_name, lat, lng, created_at)
			VALUES (?, ?, ?, 52.0, 13.0, '2026-04-02T09:00:00Z')`, name, name, name); err != nil {
			t.Fatalf("insert legacy city: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	output := captureStdout(t, func() error {
		return run([]string{"migrate", "--db", dbPath, "--output", "json"})
	})
	var result migrateResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("unmarshal migrate output: %v\noutput=%s", err, output)
	}
	if !containsString(result.Applied, "cities.normalized_lower") {
		t.Fatalf("applied = %v, want the column to be added", result.Applied)
	}
	if !containsString(result.Applied, "cities.idx_cities_search") {
		t.Fatalf("applied = %v, want the index to be created", result.Applied)
	}
	backfilled := false
	for _, applied := range result.Applied {
		if strings.HasPrefix(applied, "cities.normalized_lower_backfill") {
			backfilled = true
		}
	}
	if !backfilled {
		t.Fatalf("applied = %v, want a backfill", result.Applied)
	}

	db, err = openDB(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db.Close()
	for name, want := range map[string]string{
		"Lübbecke": "lübbecke",
		"LÜBZ":     "lübz",
		"enzberg":  "enzberg",
	} {
		var got string
		if err := db.QueryRowContext(ctx,
			`SELECT normalized_lower FROM cities WHERE name = ?`, name).Scan(&got); err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if got != want {
			t.Errorf("normalized_lower for %q = %q, want %q", name, got, want)
		}
	}

	// The reason the backfill is Go rather than SQL: SQLite's lower() is
	// ASCII-only, so LÜBZ would have kept an unmatchable key.
	var sqliteFold string
	if err := db.QueryRowContext(ctx,
		`SELECT lower(normalized_name) FROM cities WHERE name = 'LÜBZ'`).Scan(&sqliteFold); err != nil {
		t.Fatalf("sqlite lower: %v", err)
	}
	if sqliteFold == "lübz" {
		t.Skip("this SQLite build folds beyond ASCII, so the Go backfill is belt-and-braces here")
	}
	if sqliteFold != "lÜbz" {
		t.Fatalf("SQLite lower('LÜBZ') = %q, which this test's premise did not expect", sqliteFold)
	}

	// Running it again must be a no-op, not a repeated backfill.
	output = captureStdout(t, func() error {
		return run([]string{"migrate", "--db", dbPath, "--output", "json"})
	})
	var second migrateResult
	if err := json.Unmarshal([]byte(output), &second); err != nil {
		t.Fatalf("unmarshal second migrate: %v", err)
	}
	if len(second.Applied) != 0 {
		t.Errorf("second migrate applied %v, want nothing", second.Applied)
	}
}

// TestCityWritePathsStoreTheSearchKey covers the two statements that write
// cities: a city cached by an update sweep, and a bulk `import cities` row.
// Either one forgetting the folded key would leave that city unfindable.
func TestCityWritePathsStoreTheSearchKey(t *testing.T) {
	ctx := context.Background()
	db, err := openDB(filepath.Join(t.TempDir(), "writes.db"))
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()
	if err := initSchema(ctx, db, dialectSQLite); err != nil {
		t.Fatalf("initSchema: %v", err)
	}

	if _, err := db.ExecContext(ctx, citiesInsertIgnoreSQL(dialectSQLite),
		"Lübbecke, Germany", "Lübbecke", citySearchKey("Lübbecke"), "Lübbecke, Germany",
		52.3, 8.6, "2026-04-02T09:00:00Z"); err != nil {
		t.Fatalf("insert-ignore: %v", err)
	}
	if _, err := importCities(ctx, db, dialectSQLite, []cachedCity{
		{Name: "Uchte", DisplayName: "Uchte, Germany", Lat: 52.5, Lng: 8.9},
	}); err != nil {
		t.Fatalf("importCities: %v", err)
	}

	for name, want := range map[string]string{"Lübbecke": "lübbecke", "Uchte": "uchte"} {
		var got string
		if err := db.QueryRowContext(ctx,
			`SELECT normalized_lower FROM cities WHERE normalized_name = ?`, name).Scan(&got); err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if got != want {
			t.Errorf("%s stored normalized_lower %q, want %q", name, got, want)
		}
	}
}

// A tiled target has to look like one city to everything downstream: one
// snapshot per station, one recorded_at for the whole city, and the radius that
// was asked for on every row.
func TestRunUpdateTiled(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "tiled.db")
	t.Setenv(envAPIKeyName, "test-key")
	// A real pacing window, on a clock that only moves when the code waits: the
	// fetch spans minutes of clock time without costing the test any, which is
	// what makes the single-timestamp assertion below mean something.
	clock := stubTileClock(t)

	const centreLat, centreLng = 52.5, 13.4
	var rads []string
	restore := stubDefaultTransport(t, func(req *http.Request) (*http.Response, error) {
		u := req.URL
		switch {
		case strings.HasPrefix(u.String(), nominatimBaseURL):
			return jsonResponse(http.StatusOK, `[{"name":"Berlin","display_name":"Berlin, DE","lat":"52.500000","lon":"13.400000"}]`), nil
		case strings.HasPrefix(u.String(), tankerKoenigBase+"/list.php"):
			lat, _ := strconv.ParseFloat(u.Query().Get("lat"), 64)
			lng, _ := strconv.ParseFloat(u.Query().Get("lng"), 64)
			rads = append(rads, u.Query().Get("rad"))
			index := len(rads) - 1
			// Every tile also reports the station in the middle, so the merge
			// has real duplicates to fold.
			return tileListResponse(
				tileStation("shared", centreLat, centreLng, 1, 0),
				tileStation(fmt.Sprintf("own-%d", index), lat, lng, 2, 0),
			), nil
		default:
			return nil, fmt.Errorf("unexpected URL: %s", u.String())
		}
	})
	defer restore()

	output := captureStdout(t, func() error {
		return run([]string{"update", "--db", dbPath, "--city", "Berlin", "--radius", "42", "--output", "json"})
	})

	var result updateResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("unmarshal: %v\noutput=%s", err, output)
	}

	// The fetch really was spread over the pacing windows, so stamping each
	// tile separately would have produced visibly different timestamps.
	if len(clock.slept) == 0 {
		t.Fatal("the tiled fetch never waited, so the timestamp check below proves nothing")
	}
	if spanned := clock.now.Sub(clock.start); spanned < time.Minute {
		t.Fatalf("the fetch spanned only %v of clock time, too little to tell one stamp from many", spanned)
	}

	wantTiles, err := planSearchTiles(centreLat, centreLng, 42)
	if err != nil {
		t.Fatalf("planSearchTiles: %v", err)
	}
	if len(rads) != len(wantTiles) {
		t.Fatalf("%d API requests, want %d", len(rads), len(wantTiles))
	}
	for i, rad := range rads {
		if rad != "25.00" {
			t.Fatalf("request %d asked for rad=%q, want 25.00", i, rad)
		}
	}
	if result.TilesQueried != len(wantTiles) {
		t.Fatalf("tiles_queried = %d, want %d", result.TilesQueried, len(wantTiles))
	}
	if result.TilesFailed != 0 {
		t.Fatalf("tiles_failed = %d, want 0", result.TilesFailed)
	}
	// One station per tile plus the one they all share.
	if want := len(wantTiles) + 1; result.StoredCount != want {
		t.Fatalf("stored_count = %d, want %d", result.StoredCount, want)
	}

	db, err := openDatabase(context.Background(), dbConfig{Driver: dialectSQLite, Path: dbPath})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	var snapshots, stations, instants int
	var radius float64
	if err := db.QueryRow(`
		SELECT COUNT(*), COUNT(DISTINCT station_id), COUNT(DISTINCT recorded_at), MAX(search_radius_km)
		FROM price_snapshots
	`).Scan(&snapshots, &stations, &instants, &radius); err != nil {
		t.Fatalf("query: %v", err)
	}
	if snapshots != stations {
		t.Fatalf("%d snapshots for %d stations: the tiles were not de-duplicated by id", snapshots, stations)
	}
	if instants != 1 {
		t.Fatalf("%d distinct recorded_at values, want 1 for a tiled city", instants)
	}
	// And that one instant is when the first request went out, not when the
	// last one came back.
	var recordedAt string
	if err := db.QueryRow(`SELECT recorded_at FROM price_snapshots LIMIT 1`).Scan(&recordedAt); err != nil {
		t.Fatalf("recorded_at: %v", err)
	}
	if want := clock.start.UTC().Format(time.RFC3339); recordedAt != want {
		t.Fatalf("recorded_at = %q, want %q (the first request's instant)", recordedAt, want)
	}
	if radius != 42 {
		t.Fatalf("search_radius_km = %v, want the 42 that was asked for", radius)
	}

	// One city, not one per tile centre.
	var cities int
	if err := db.QueryRow(`SELECT COUNT(*) FROM cities`).Scan(&cities); err != nil {
		t.Fatalf("cities: %v", err)
	}
	if cities != 1 {
		t.Fatalf("%d cities rows, want 1", cities)
	}
}

// A tile that never answers costs only its own stations: the city is still
// stored, and the run says it was degraded.
func TestRunUpdateTiledPartial(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "partial.db")
	t.Setenv(envAPIKeyName, "test-key")
	stubTileClock(t)

	calls := 0
	restore := stubDefaultTransport(t, func(req *http.Request) (*http.Response, error) {
		u := req.URL
		switch {
		case strings.HasPrefix(u.String(), nominatimBaseURL):
			return jsonResponse(http.StatusOK, `[{"name":"Berlin","display_name":"Berlin, DE","lat":"52.500000","lon":"13.400000"}]`), nil
		case strings.HasPrefix(u.String(), tankerKoenigBase+"/list.php"):
			lat, _ := strconv.ParseFloat(u.Query().Get("lat"), 64)
			lng, _ := strconv.ParseFloat(u.Query().Get("lng"), 64)
			calls++
			if calls == 3 || calls == 4 { // one tile, both of its attempts
				return jsonResponse(http.StatusBadGateway, `{"ok":false,"message":"upstream"}`), nil
			}
			return tileListResponse(tileStation(fmt.Sprintf("s-%d", calls), lat, lng, 1, 0)), nil
		default:
			return nil, fmt.Errorf("unexpected URL: %s", u.String())
		}
	})
	defer restore()

	output := captureStdout(t, func() error {
		return run([]string{"update", "--db", dbPath, "--city", "Berlin", "--radius", "42", "--request-delay", "0", "--output", "json"})
	})

	var result updateResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("unmarshal: %v\noutput=%s", err, output)
	}
	if result.TilesFailed != 1 {
		t.Fatalf("tiles_failed = %d, want 1", result.TilesFailed)
	}
	if result.StoredCount == 0 {
		t.Fatal("a single failed tile threw the whole city away")
	}

	db, err := openDatabase(context.Background(), dbConfig{Driver: dialectSQLite, Path: dbPath})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	var status string
	if err := db.QueryRow(`SELECT status FROM command_runs WHERE command = 'update' ORDER BY id DESC LIMIT 1`).Scan(&status); err != nil {
		t.Fatalf("command_runs: %v", err)
	}
	if status != "partial" {
		t.Fatalf("run status = %q, want partial", status)
	}

	metrics := map[string]float64{}
	rows, err := db.Query(`
		SELECT m.name, m.value FROM command_run_metrics m
		JOIN command_runs r ON r.id = m.run_id
		WHERE r.command = 'update' ORDER BY r.id DESC
	`)
	if err != nil {
		t.Fatalf("metrics: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var value float64
		if err := rows.Scan(&name, &value); err != nil {
			t.Fatalf("scan: %v", err)
		}
		metrics[name] = value
	}
	if metrics["tiles_failed"] != 1 {
		t.Fatalf("tiles_failed metric = %v, want 1", metrics["tiles_failed"])
	}
	if metrics["tiles"] < 2 {
		t.Fatalf("tiles metric = %v, want the tile count of a tiled sweep", metrics["tiles"])
	}
}

// The contract that keeps every existing install untouched: a radius the API
// serves is one request, with no pacing at all.
func TestRunUpdateUntiledIssuesOneRequestAndNeverWaits(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "untiled.db")
	t.Setenv(envAPIKeyName, "test-key")
	clock := stubTileClock(t)

	calls := 0
	restore := stubDefaultTransport(t, func(req *http.Request) (*http.Response, error) {
		u := req.URL
		switch {
		case strings.HasPrefix(u.String(), nominatimBaseURL):
			return jsonResponse(http.StatusOK, `[{"name":"Berlin","display_name":"Berlin, DE","lat":"52.500000","lon":"13.400000"}]`), nil
		case strings.HasPrefix(u.String(), tankerKoenigBase+"/list.php"):
			calls++
			if rad := u.Query().Get("rad"); rad != "25.00" {
				return nil, fmt.Errorf("rad = %q, want 25.00", rad)
			}
			return tileListResponse(tileStation("only", 52.5, 13.4, 1, 0)), nil
		default:
			return nil, fmt.Errorf("unexpected URL: %s", u.String())
		}
	})
	defer restore()

	output := captureStdout(t, func() error {
		return run([]string{"update", "--db", dbPath, "--city", "Berlin", "--radius", "25", "--output", "json"})
	})

	if calls != 1 {
		t.Fatalf("%d API requests for a 25 km radius, want 1", calls)
	}
	if len(clock.slept) != 0 {
		t.Fatalf("an untiled sweep waited %v", clock.slept)
	}

	var result updateResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("unmarshal: %v\noutput=%s", err, output)
	}
	// The tile counters stay out of an untiled run's output entirely.
	if result.TilesQueried != 0 || result.TilesFailed != 0 {
		t.Fatalf("tiles_queried = %d, tiles_failed = %d, want both absent", result.TilesQueried, result.TilesFailed)
	}
	if strings.Contains(output, "tiles_queried") || strings.Contains(output, "tiles_failed") {
		t.Fatalf("untiled JSON mentions the tile counters:\n%s", output)
	}
}

func TestRunUpdateRejectsBadPacingFlags(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "pacing.db")
	t.Setenv(envAPIKeyName, "test-key")

	for _, args := range [][]string{
		{"update", "--db", dbPath, "--city", "Berlin", "--request-delay", "-1s"},
		{"update", "--db", dbPath, "--city", "Berlin", "--request-burst", "0"},
	} {
		if err := run(args); err == nil {
			t.Fatalf("%v was accepted", args)
		}
	}
}

// The text output names the queries a tiled target took, and says nothing about
// them for a target the API served in one.
func TestRunUpdateTiledTextOutput(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(envAPIKeyName, "test-key")
	stubTileClock(t)

	calls := 0
	restore := stubDefaultTransport(t, func(req *http.Request) (*http.Response, error) {
		u := req.URL
		switch {
		case strings.HasPrefix(u.String(), nominatimBaseURL):
			if strings.Contains(u.Query().Get("q"), "Uchte") {
				return jsonResponse(http.StatusOK, `[{"name":"Uchte","display_name":"Uchte, DE","lat":"52.480000","lon":"8.930000"}]`), nil
			}
			return jsonResponse(http.StatusOK, `[{"name":"Berlin","display_name":"Berlin, DE","lat":"52.500000","lon":"13.400000"}]`), nil
		case strings.HasPrefix(u.String(), tankerKoenigBase+"/list.php"):
			lat, _ := strconv.ParseFloat(u.Query().Get("lat"), 64)
			lng, _ := strconv.ParseFloat(u.Query().Get("lng"), 64)
			calls++
			if calls == 5 || calls == 6 { // one tile, both attempts
				return jsonResponse(http.StatusBadGateway, `{"ok":false,"message":"upstream"}`), nil
			}
			return tileListResponse(tileStation(fmt.Sprintf("s-%d", calls), lat, lng, 2, 0)), nil
		}
		return nil, fmt.Errorf("unexpected URL: %s", u)
	})
	defer restore()

	single := captureStdout(t, func() error {
		return run([]string{"update", "--db", filepath.Join(dir, "a.db"), "--city", "Berlin", "--radius", "42"})
	})
	if !strings.Contains(single, "from 6 queries (1 failed)") {
		t.Fatalf("single-city output does not name the queries:\n%s", single)
	}
	// One "in" clause, naming the database — not two.
	if strings.Count(single, " in ") != 1 {
		t.Fatalf("single-city output reads awkwardly:\n%s", single)
	}

	calls = 0
	multi := captureStdout(t, func() error {
		return run([]string{"update", "--db", filepath.Join(dir, "b.db"), "--radius", "42", "--city", "Berlin", "--city", "Uchte", "--radius", "5"})
	})
	if !strings.Contains(multi, "radius 42.00 km in 6 queries (1 failed), stored") {
		t.Fatalf("tiled target is not reported:\n%s", multi)
	}
	// The 5 km target fitted in one request, so its line is unchanged.
	if !strings.Contains(multi, "radius 5.00 km, stored") {
		t.Fatalf("untiled target's line changed:\n%s", multi)
	}
}

// A city's first request can be held back by the pacing when an earlier city
// filled the current window, and the snapshot instant has to be the moment that
// request actually went out — not the moment the city's turn came up.
func TestRunUpdateTiledStampsFirstRequestAcrossCities(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "twocities.db")
	t.Setenv(envAPIKeyName, "test-key")
	clock := stubTileClock(t)

	// 40 km is 6 tiles, and the pace belongs to the API key rather than to the
	// city, so the second city's very first request is the seventh of the sweep
	// and is held back by whatever the defaults make of that. The timeline is
	// derived from the defaults rather than written out, so tuning them moves
	// this test's expectations with them instead of breaking it.
	const radius = "40"
	pacing := defaultLimiter()
	if tiles, err := planSearchTiles(52.5, 13.4, 40); err != nil || len(tiles) != 6 {
		t.Fatalf("40 km planned %d tiles (err %v), want 6 — the pacing timeline below assumes it", len(tiles), err)
	}

	calls := 0
	restore := stubDefaultTransport(t, func(req *http.Request) (*http.Response, error) {
		u := req.URL
		switch {
		case strings.HasPrefix(u.String(), nominatimBaseURL):
			if strings.Contains(u.Query().Get("q"), "Uchte") {
				return jsonResponse(http.StatusOK, `[{"name":"Uchte","display_name":"Uchte, DE","lat":"52.480000","lon":"8.930000"}]`), nil
			}
			return jsonResponse(http.StatusOK, `[{"name":"Berlin","display_name":"Berlin, DE","lat":"52.500000","lon":"13.400000"}]`), nil
		case strings.HasPrefix(u.String(), tankerKoenigBase+"/list.php"):
			lat, _ := strconv.ParseFloat(u.Query().Get("lat"), 64)
			lng, _ := strconv.ParseFloat(u.Query().Get("lng"), 64)
			calls++
			return tileListResponse(tileStation(fmt.Sprintf("s-%d", calls), lat, lng, 2, 0)), nil
		}
		return nil, fmt.Errorf("unexpected URL: %s", u)
	})
	defer restore()

	if err := run([]string{"update", "--db", dbPath, "--radius", radius, "--city", "Berlin", "--city", "Uchte", "--output", "json"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if calls != 12 {
		t.Fatalf("%d API requests, want 12 (6 per city)", calls)
	}

	db, err := openDatabase(context.Background(), dbConfig{Driver: dialectSQLite, Path: dbPath})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	stamps := map[string]string{}
	rows, err := db.Query(`SELECT city_name, recorded_at, COUNT(*) FROM price_snapshots GROUP BY city_name, recorded_at`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var city, recordedAt string
		var count int
		if err := rows.Scan(&city, &recordedAt, &count); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if previous, seen := stamps[city]; seen {
			t.Fatalf("%s has two snapshot instants: %s and %s", city, previous, recordedAt)
		}
		stamps[city] = recordedAt
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	// The first city starts on an empty window, so its first request goes out
	// immediately.
	if want := clock.start.UTC().Format(time.RFC3339); stamps["Berlin"] != want {
		t.Errorf("Berlin recorded_at = %q, want %q (its first request went out at once)", stamps["Berlin"], want)
	}
	// The second city's first request is held until its slot comes up, and that
	// is the instant its stations were observed.
	if want := clock.start.Add(pacing.pace(7)).UTC().Format(time.RFC3339); stamps["Uchte"] != want {
		t.Errorf("Uchte recorded_at = %q, want %q (the instant its first request actually went out)", stamps["Uchte"], want)
	}
}
