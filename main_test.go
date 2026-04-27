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

	if err := persistUpdate(ctx, db, city, []tankerStation{station}, times[0], 5); err != nil {
		t.Fatalf("persist initial update: %v", err)
	}
	assertSnapshotCount(t, db, 1)
	assertLatestSnapshot(t, db, times[0].Format(time.RFC3339), 2.349)

	if err := persistUpdate(ctx, db, city, []tankerStation{station}, times[1], 5); err != nil {
		t.Fatalf("persist unchanged update: %v", err)
	}
	assertSnapshotCount(t, db, 1)
	assertLatestSnapshot(t, db, times[1].Format(time.RFC3339), 2.349)

	diesel = 2.389
	station.Diesel = &diesel
	if err := persistUpdate(ctx, db, city, []tankerStation{station}, times[2], 5); err != nil {
		t.Fatalf("persist changed update: %v", err)
	}
	assertSnapshotCount(t, db, 2)
	assertLatestSnapshot(t, db, times[2].Format(time.RFC3339), 2.389)

	if err := persistUpdate(ctx, db, city, []tankerStation{station}, times[3], 5); err != nil {
		t.Fatalf("persist first unchanged update after change: %v", err)
	}
	assertSnapshotCount(t, db, 3)
	assertLatestSnapshot(t, db, times[3].Format(time.RFC3339), 2.389)

	if err := persistUpdate(ctx, db, city, []tankerStation{station}, times[4], 5); err != nil {
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

	if err := persistUpdate(ctx, db, city, []tankerStation{station}, first, 5); err != nil {
		t.Fatalf("persist first update: %v", err)
	}

	station.Dist = 9.99
	if err := persistUpdate(ctx, db, city, []tankerStation{station}, second, 5); err != nil {
		t.Fatalf("persist distance-only update: %v", err)
	}
	assertSnapshotCount(t, db, 1)

	station.IsOpen = false
	if err := persistUpdate(ctx, db, city, []tankerStation{station}, third, 5); err != nil {
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
	if err := initSchema(ctx, db); err != nil {
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
		return run([]string{"check", "--db", dbPath, "--city", "Berlin", "--fuel", "diesel", "--history-days", "10", "--predict-days", "1", "--output", "json"})
	})

	var checks []priceCheckRow
	if err := json.Unmarshal([]byte(output), &checks); err != nil {
		t.Fatalf("unmarshal check output: %v\noutput=%s", err, output)
	}
	if len(checks) != 1 {
		t.Fatalf("len(checks) = %d, want 1", len(checks))
	}
	if checks[0].StationID != "station-1" || checks[0].Station.ID != "station-1" {
		t.Fatalf("station fields = %+v, want station-1", checks[0])
	}
	if checks[0].Recommendation != "buy" {
		t.Fatalf("recommendation = %q, want buy", checks[0].Recommendation)
	}
}

func TestSuggestGasReturnsDayAndTimeSuggestionsWithinRange(t *testing.T) {
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
	insertSuggestStation(t, db, "far-station", "Far Station", 53.500000, 13.395131)

	for day := 20; day <= 25; day++ {
		insertSuggestSnapshot(t, db, "near-station", "Berlin", time.Date(2026, 4, day, 17, 0, 0, 0, time.UTC), 2.200, true)
		insertSuggestSnapshot(t, db, "near-station", "Berlin", time.Date(2026, 4, day, 18, 0, 0, 0, time.UTC), 2.000, true)
		insertSuggestSnapshot(t, db, "near-station", "Berlin", time.Date(2026, 4, day, 19, 0, 0, 0, time.UTC), 2.200, true)
		insertSuggestSnapshot(t, db, "far-station", "Berlin", time.Date(2026, 4, day, 18, 0, 0, 0, time.UTC), 1.500, true)
	}

	suggestions, err := suggestGas(ctx, db, suggestOptions{
		City:        "Berlin",
		RangeKM:     5,
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
		if suggestion.StationID != "near-station" {
			t.Fatalf("station id = %q, want near-station", suggestion.StationID)
		}
		if suggestion.StartTime != "18:00" || suggestion.EndTime != "19:00" {
			t.Fatalf("time window = %s-%s, want 18:00-19:00", suggestion.StartTime, suggestion.EndTime)
		}
		if suggestion.DistanceKM > 0.1 {
			t.Fatalf("distance = %.1f, want near station distance", suggestion.DistanceKM)
		}
		if suggestion.Station.Address != "Test Street 1, 10115 Berlin" {
			t.Fatalf("station address = %q, want formatted address", suggestion.Station.Address)
		}
		if suggestion.Station.Brand != "TEST" || suggestion.Station.Street != "Test Street" || suggestion.Station.HouseNumber != "1" || suggestion.Station.PostCode != 10115 || suggestion.Station.Place != "Berlin" {
			t.Fatalf("station metadata = %+v, want persisted station details", suggestion.Station)
		}
		if suggestion.PredictedPrice >= 2.200 {
			t.Fatalf("predicted price = %.3f, want lower than 2.200", suggestion.PredictedPrice)
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
		City:        "Berlin",
		RangeKM:     5,
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
		City:        "Berlin",
		RangeKM:     5,
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
		City:        "Berlin",
		RangeKM:     5,
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
		{name: "city", opts: checkOptions{RangeKM: 5, Fuel: "diesel", HistoryDays: 21, PredictDays: 3, Limit: 5}, want: "requires --city"},
		{name: "fuel", opts: checkOptions{City: "Berlin", RangeKM: 5, Fuel: "premium", HistoryDays: 21, PredictDays: 3, Limit: 5}, want: "--fuel"},
		{name: "limit", opts: checkOptions{City: "Berlin", RangeKM: 5, Fuel: "diesel", HistoryDays: 21, PredictDays: 3, Limit: -1}, want: "--limit"},
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

	imported, err := importCities(ctx, db, []cachedCity{{
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

	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("open migrated db: %v", err)
	}
	defer db.Close()

	ctx := context.Background()

	hasNormalizedName, err := tableHasColumn(ctx, db, "cities", "normalized_name")
	if err != nil {
		t.Fatalf("tableHasColumn cities.normalized_name: %v", err)
	}
	if !hasNormalizedName {
		t.Fatal("expected cities.normalized_name after migration")
	}

	hasDistKM, err := tableHasColumn(ctx, db, "price_snapshots", "dist_km")
	if err != nil {
		t.Fatalf("tableHasColumn price_snapshots.dist_km: %v", err)
	}
	if hasDistKM {
		t.Fatal("expected price_snapshots.dist_km to be removed")
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
	if err := initSchema(context.Background(), db); err != nil {
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
	if err := initSchema(ctx, db); err != nil {
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

func seedUncompactedFixtureDB(t *testing.T) string {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "uncompacted.db")
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	if err := initSchema(ctx, db); err != nil {
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
	if err := initSchema(ctx, db); err != nil {
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
