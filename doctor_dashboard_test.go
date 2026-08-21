package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// seedDashboardDB builds a database shaped like the dashboard reads it: a
// geocoded city, stations inside and outside its radius, one station that
// stopped being fed, snapshot history for the fed ones, and prediction runs
// whose grids reach into the future.
func seedDashboardDB(t *testing.T, now time.Time) (string, *sql.DB) {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "dashboard.db")
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := initSchema(ctx, db, dialectSQLite); err != nil {
		t.Fatalf("initSchema: %v", err)
	}
	centreLat, centreLng := 52.5200, 13.4050
	if _, err := db.ExecContext(ctx, `INSERT INTO cities
		(name, normalized_name, display_name, lat, lng, created_at)
		VALUES ('berlin', 'berlin', 'Berlin, Germany', ?, ?, ?)`,
		centreLat, centreLng, now.Format(time.RFC3339)); err != nil {
		t.Fatalf("insert city: %v", err)
	}

	// near-1/near-2 sit inside 5 km, far-1 well outside it, and stale-1 is
	// inside but has not been fed since before the freshness cutoff.
	stations := []struct {
		id    string
		lat   float64
		lng   float64
		fedAt time.Time
	}{
		{"near-1", centreLat + 0.005, centreLng, now.Add(-30 * time.Minute)},
		{"near-2", centreLat - 0.010, centreLng + 0.010, now.Add(-time.Hour)},
		{"far-1", centreLat + 0.900, centreLng, now.Add(-30 * time.Minute)},
		{"stale-1", centreLat + 0.002, centreLng, now.Add(-5 * 24 * time.Hour)},
	}
	for _, station := range stations {
		if _, err := db.ExecContext(ctx, `INSERT INTO stations
			(id, name, brand, street, house_number, post_code, place, lat, lng, first_seen_at, last_seen_at)
			VALUES (?, ?, 'Brand', 'Street', '1', 12345, 'Berlin', ?, ?, ?, ?)`,
			station.id, "Station "+station.id, station.lat, station.lng,
			now.AddDate(0, 0, -60).Format(time.RFC3339), station.fedAt.Format(time.RFC3339)); err != nil {
			t.Fatalf("insert station: %v", err)
		}
		// A short history per station, ending at its last-fed time.
		for step := 0; step < 6; step++ {
			recorded := station.fedAt.Add(-time.Duration(step) * time.Hour)
			if _, err := db.ExecContext(ctx, `INSERT INTO price_snapshots
				(station_id, city_name, recorded_at, search_radius_km, is_open, e5, e10, diesel)
				VALUES (?, 'berlin', ?, 5, 1, 1.899, 1.849, 1.759)`,
				station.id, recorded.Format(time.RFC3339)); err != nil {
				t.Fatalf("insert snapshot: %v", err)
			}
		}
	}

	// Two runs per fuel, the newer superseding the older, each with a grid that
	// runs from an hour ago to two days out.
	for run := 0; run < 2; run++ {
		runAt := now.Add(-time.Duration(2-run) * time.Hour)
		for _, fuel := range []string{"e5", "e10", "diesel"} {
			res, err := db.ExecContext(ctx, `INSERT INTO prediction_runs
				(run_at, city_name, fuel, range_km, history_days, predict_days, station_count, suggestion_bias)
				VALUES (?, 'berlin', ?, 5, 30, 3, 2, -0.004)`, runAt.Format(time.RFC3339), fuel)
			if err != nil {
				t.Fatalf("insert run: %v", err)
			}
			runID, _ := res.LastInsertId()
			for _, station := range []string{"near-1", "near-2"} {
				for hour := -1; hour <= 4; hour++ {
					target := runAt.Add(time.Duration(hour) * time.Hour)
					if _, err := db.ExecContext(ctx, `INSERT INTO price_predictions
						(run_id, station_id, fuel, target_start, target_end, predicted_price, baseline,
						 confidence, sample_count, is_suggestion, lead_minutes, applied_correction)
						VALUES (?, ?, ?, ?, ?, 1.712, 1.712, 'high', 30, 0, ?, 0.0)`,
						runID, station, fuel,
						target.Format(time.RFC3339), target.Add(time.Hour).Format(time.RFC3339),
						(hour+1)*60); err != nil {
						t.Fatalf("insert prediction: %v", err)
					}
				}
			}
		}
	}
	return dbPath, db
}

// dashboardOptions is the option set a `doctor dashboard` run builds.
func dashboardOptions(filters doctorDashboardFilters) doctorOptions {
	return doctorOptions{
		Pages:     doctorPages{Dashboard: true},
		Dashboard: filters,
		Probe:     true,
		Filters:   doctorFilters{Fuel: "diesel", Confidence: "all"},
		SlowMS:    1000,
	}
}

func TestRunDashboardChecksReproducesAPageLoad(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Hour)
	_, db := seedDashboardDB(t, now)
	defer db.Close()
	ctx := context.Background()

	opts := dashboardOptions(doctorDashboardFilters{
		City: "berlin", RadiusKM: 5, Fuel: "all",
		From: now.AddDate(0, 0, -7).Format("2006-01-02") + "T00:00:00Z",
	})
	present := map[string]bool{"stations": true, "price_snapshots": true, "cities": true,
		"price_predictions": true, "prediction_runs": true}
	dash := runDashboardChecks(ctx, db, dialectSQLite, opts, present, nil, doctorScope{}, now)

	if !dash.Scope.CityFound {
		t.Fatal("berlin is geocoded, so the city must resolve")
	}
	// stale-1 is inside the radius but out of scope, far-1 is fed but outside
	// the bounding box, so only the two near stations survive.
	if dash.Scope.Stations != 2 || dash.Scope.Selected != 2 {
		t.Errorf("scope = %d in radius / %d selected, want 2/2 (%+v)", dash.Scope.Stations, dash.Scope.Selected, dash.Scope)
	}
	if dash.Scope.Candidates < 2 {
		t.Errorf("bounding box returned %d candidates, want at least the 2 in scope", dash.Scope.Candidates)
	}

	got := map[string]doctorQuery{}
	for _, q := range dash.Queries {
		got[q.Name] = q
	}
	for _, name := range []string{"city", "city_search", "scope_stations", "snapshots",
		"predictions_grid"} {
		q, ok := got[name]
		if !ok {
			t.Fatalf("dashboard check did not run %q (ran %v)", name, dash.Queries)
		}
		if q.Error != "" {
			t.Errorf("query %s failed: %s", name, q.Error)
		}
	}
	if got["snapshots"].Rows != 12 {
		t.Errorf("snapshots returned %d rows, want the 12 the two in-scope stations have", got["snapshots"].Rows)
	}
	// The grid carries run_id, which is what lets the page reduce these rows to
	// the newest run per station without a second query.
	if !strings.Contains(got["predictions_grid"].SQL, "pp.run_id") {
		t.Errorf("predictions_grid must select run_id:\n%s", got["predictions_grid"].SQL)
	}
	if probe := got["snapshots"].Probe; probe == nil {
		t.Fatal("snapshots has no probe, so nothing prices its row lookups")
	} else if probe.Rows != got["snapshots"].Rows {
		t.Errorf("snapshot probe read %d rows against the query's %d; the probe must read the same rows",
			probe.Rows, got["snapshots"].Rows)
	}
}

func TestDashboardChecksSkipTheHeavyQueriesWithoutAScope(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Hour)
	_, db := seedDashboardDB(t, now)
	defer db.Close()

	opts := dashboardOptions(doctorDashboardFilters{RadiusKM: 5, Fuel: "all"})
	opts.DashboardNoCity = true
	present := map[string]bool{"stations": true, "price_snapshots": true, "cities": true}
	dash := runDashboardChecks(context.Background(), db, dialectSQLite, opts, present, nil, doctorScope{}, now)

	names := []string{}
	for _, q := range dash.Queries {
		names = append(names, q.Name)
	}
	// The page itself skips both when there is no scope to display, so doctor
	// must not invent a load the page never issues.
	for _, absent := range []string{"snapshots", "predictions_latest", "predictions_grid", "city"} {
		if strings.Contains(strings.Join(names, ","), absent) {
			t.Errorf("unscoped dashboard must not run %q, ran %v", absent, names)
		}
	}
	if len(names) != 1 || names[0] != "scope_stations" {
		t.Fatalf("unscoped dashboard queries = %v, want just the station list", names)
	}
	if dash.Scope.Stations != 3 {
		t.Errorf("unscoped scope = %d stations, want the 3 still being fed", dash.Scope.Stations)
	}
}

func TestDashboardChecksPickTheBusiestCity(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Hour)
	_, db := seedDashboardDB(t, now)
	defer db.Close()

	scope := doctorScope{Cities: []doctorScopeCity{
		{City: "aachen", Stations: 4, InScope: 1},
		{City: "berlin", Stations: 9, InScope: 7},
	}}
	opts := dashboardOptions(doctorDashboardFilters{RadiusKM: 5, Fuel: "diesel"})
	present := map[string]bool{"stations": true, "price_snapshots": true, "cities": true}
	dash := runDashboardChecks(context.Background(), db, dialectSQLite, opts, present, nil, scope, now)

	if dash.Filters.City != "berlin" || !dash.Filters.CityAuto {
		t.Fatalf("filters = %+v, want the auto-picked berlin", dash.Filters)
	}
	findings := doctorDashboardFindings(dash, nil, opts)
	if !containsFinding(findings, "with the most stations in scope") {
		t.Errorf("an auto-picked city must be stated in the findings, got %v", findings)
	}
}

func TestDashboardChecksReportAnUnknownCity(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Hour)
	_, db := seedDashboardDB(t, now)
	defer db.Close()

	opts := dashboardOptions(doctorDashboardFilters{City: "nowhere", RadiusKM: 5, Fuel: "diesel"})
	present := map[string]bool{"stations": true, "price_snapshots": true, "cities": true}
	dash := runDashboardChecks(context.Background(), db, dialectSQLite, opts, present, nil, doctorScope{}, now)

	if dash.Scope.CityFound {
		t.Fatal("nowhere is not geocoded")
	}
	// The lookup that failed is still timed: that is the query an operator
	// wants to see, and it is the only one the page reaches.
	names := []string{}
	for _, q := range dash.Queries {
		names = append(names, q.Name)
	}
	// The page renders its error and stops, so nothing past the lookup runs.
	if strings.Join(names, ",") != "city,city_search" {
		t.Fatalf("queries = %v, want just the two cities lookups", names)
	}
	if !containsFinding(doctorDashboardFindings(dash, nil, opts), "is not in the cities table") {
		t.Error("an unresolvable city must be a finding")
	}
}

func TestDashboardFindingsNameTheStructuralCosts(t *testing.T) {
	opts := dashboardOptions(doctorDashboardFilters{City: "berlin", RadiusKM: 5, Fuel: "all"})
	opts.SlowMS = 100
	dash := &doctorDashboard{
		Filters: opts.Dashboard,
		Fuels:   []string{"e5", "e10", "diesel"},
		Scope:   doctorDashboardScope{Candidates: 120, Stations: 20, Selected: 20, CityFound: true},
		Queries: []doctorQuery{
			{Name: "city", Table: "cities", DurationMS: 40, Rows: 1,
				UsesIndex: "idx_cities_normalized"},
			// The plan says it used the index; it still reads all of it.
			{Name: "city_search", Table: "cities", DurationMS: 95, Rows: 20,
				UsesIndex: "idx_cities_normalized"},
			// 400 ms of lookups over 58,204 rows is ~7 µs each: a lot of cheap
			// buffer-pool reads, so the row count is the thing to reduce.
			{Name: "snapshots", Table: "price_snapshots", DurationMS: 3400, Rows: 58204,
				UsesIndex: "idx_price_snapshots_station_recorded",
				Probe: &doctorQueryProbe{Name: "keys only", DurationMS: 3000, Rows: 58204,
					UsesIndex: "idx_price_snapshots_station_recorded", CoveringHit: true}},
			{Name: "predictions_grid", Table: "price_predictions", DurationMS: 1200, Rows: 30248,
				UsesIndex: "idx_price_predictions_station_fuel_target",
				Probe:     &doctorQueryProbe{Name: "keys only", DurationMS: 180, Rows: 30248, CoveringHit: true}},
		},
	}
	tables := []doctorTable{
		{Name: "cities", Rows: 120_000},
		{Name: "price_snapshots", Rows: 9_000_000},
		{Name: "price_predictions", Rows: 41_000_000},
	}
	findings := doctorDashboardFindings(dash, tables, opts)
	joined := renderFindings(findings)

	for _, want := range []string{
		// The snapshot read's row lookups, priced against a covering index.
		"fetching is_open/e5/e10/diesel from table rows",
		"about 7 µs each",
		"idx_price_predictions_accuracy did for the accuracy page",

		// The grid reduced in PHP rather than in SQL.
		"PHP then reduces them to the newest run per station and fuel",
		// The bounding box overshooting the radius.
		"bounding box admits 120 stations for the 20",
		"dashboard page SQL total 4735 ms; slowest snapshots",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("findings are missing %q:\n%s", want, joined)
		}
	}
	for _, finding := range findings {
		if strings.Contains(finding.Message, "fetching is_open") && finding.Severity != "warn" {
			t.Errorf("nearly three seconds of row lookups is a warning, got %q", finding.Severity)
		}
	}
	// Seek latency earns the extra sentence; a cache hit must not get it.
	if strings.Contains(joined, "not staying in the buffer pool") {
		t.Errorf("1 µs per lookup is a cache hit, not a seek:\n%s", joined)
	}
}

func TestDashboardFindingsCallOutSeekLatency(t *testing.T) {
	opts := dashboardOptions(doctorDashboardFilters{City: "berlin", RadiusKM: 5, Fuel: "all"})
	// A lookup-bound read where each lookup costs a seek rather than a cache
	// hit. Reducing the row count is the wrong fix for this and the finding has
	// to say so, whichever query it lands on.
	dash := &doctorDashboard{
		Filters: opts.Dashboard,
		Scope:   doctorDashboardScope{Candidates: 10, Stations: 8, Selected: 8, CityFound: true},
		Queries: []doctorQuery{
			{Name: "snapshots", Table: "price_snapshots", DurationMS: 30256, Rows: 100000,
				UsesIndex: "idx_price_snapshots_station_recorded",
				Probe: &doctorQueryProbe{Name: "keys only", DurationMS: 256, Rows: 100000,
					UsesIndex: "idx_price_snapshots_station_recorded", CoveringHit: true}},
		},
	}
	joined := renderFindings(doctorDashboardFindings(dash, []doctorTable{{Name: "price_snapshots", Rows: 8_670_000}}, opts))
	for _, want := range []string{
		"spends 30000 ms of its 30256 ms",
		"about 300 µs each",
		"That is seek latency rather than a cache hit, so the table is not staying in the buffer pool",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("findings are missing %q:\n%s", want, joined)
		}
	}
}

func TestDashboardFindingsStayQuietOnASmallDatabase(t *testing.T) {
	opts := dashboardOptions(doctorDashboardFilters{City: "berlin", RadiusKM: 5, Fuel: "diesel"})
	dash := &doctorDashboard{
		Filters: opts.Dashboard,
		Scope:   doctorDashboardScope{Candidates: 3, Stations: 2, Selected: 2, CityFound: true},
		Queries: []doctorQuery{
			{Name: "city", Table: "cities", DurationMS: 0.3, Rows: 1, FullScan: true},
			{Name: "snapshots", Table: "price_snapshots", DurationMS: 2, Rows: 40,
				UsesIndex: "idx_price_snapshots_station_recorded",
				Probe:     &doctorQueryProbe{Name: "keys only", DurationMS: 1.6, Rows: 40, CoveringHit: true}},
		},
	}
	tables := []doctorTable{{Name: "cities", Rows: 3}, {Name: "price_snapshots", Rows: 240}}
	joined := renderFindings(doctorDashboardFindings(dash, tables, opts))
	if strings.Contains(joined, "warn") {
		t.Errorf("nothing here is worth a warning:\n%s", joined)
	}
	// 0.4 ms of the 2 ms is a fifth of the query and still nothing: a share
	// without an absolute floor reports every fast query as a problem.
	if strings.Contains(joined, "fetching is_open") {
		t.Errorf("sub-millisecond lookups must not be reported at all:\n%s", joined)
	}
}

func TestLookupSeverityNeedsBothAFloorAndAShare(t *testing.T) {
	cases := []struct {
		name                       string
		lookupsMS, totalMS, slowMS float64
		want                       string
		report                     bool
	}{
		// The production shape both bugs were found in: 4 ms of a 7 ms query is
		// more than half of it and must not warn.
		{"tiny query", 4, 7, 1000, "", false},
		{"noticeable but small", 40, 60, 1000, "info", true},
		{"most of a slow query", 600, 900, 1000, "warn", true},
		{"over the slow threshold on its own", 1200, 4000, 1000, "warn", true},
		// Big in absolute terms but a small share, and the query is under
		// --slow-ms: worth a note, not an alarm.
		{"small share of a fast query", 150, 900, 1000, "info", true},
		{"probe slower than the query", -3, 10, 1000, "", false},
	}
	for _, tc := range cases {
		got, report := lookupSeverity(tc.lookupsMS, tc.totalMS, tc.slowMS)
		if got != tc.want || report != tc.report {
			t.Errorf("%s: lookupSeverity(%v, %v, %v) = %q, %v; want %q, %v",
				tc.name, tc.lookupsMS, tc.totalMS, tc.slowMS, got, report, tc.want, tc.report)
		}
	}
}

func TestPerLookupMicros(t *testing.T) {
	if got := perLookupMicros(181085-256, 610978); math.Round(got) != 296 {
		t.Errorf("perLookupMicros = %v, want about 296 µs", got)
	}
	// No rows means no rate, rather than a division by zero.
	if got := perLookupMicros(100, 0); got != 0 {
		t.Errorf("perLookupMicros with no rows = %v, want 0", got)
	}
}

func TestResolveDoctorPages(t *testing.T) {
	cases := []struct {
		args  []string
		pages doctorPages
		rest  []string
		fails bool
	}{
		{args: nil, pages: doctorPages{Accuracy: true}},
		{args: []string{"--explain"}, pages: doctorPages{Accuracy: true}, rest: []string{"--explain"}},
		{args: []string{"accuracy", "--fuel", "e5"}, pages: doctorPages{Accuracy: true}, rest: []string{"--fuel", "e5"}},
		{args: []string{"dashboard"}, pages: doctorPages{Dashboard: true}, rest: []string{}},
		{args: []string{"all"}, pages: doctorPages{Accuracy: true, Dashboard: true}, rest: []string{}},
		{args: []string{"nonsense"}, fails: true},
	}
	for _, tc := range cases {
		pages, rest, err := resolveDoctorPages(tc.args)
		if tc.fails {
			if err == nil {
				t.Errorf("doctor %v must be rejected", tc.args)
			}
			continue
		}
		if err != nil {
			t.Errorf("doctor %v: %v", tc.args, err)
			continue
		}
		if pages != tc.pages {
			t.Errorf("doctor %v pages = %+v, want %+v", tc.args, pages, tc.pages)
		}
		if strings.Join(rest, " ") != strings.Join(tc.rest, " ") {
			t.Errorf("doctor %v rest = %v, want %v", tc.args, rest, tc.rest)
		}
	}
}

func TestResolveDashboardRange(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name, rangeName, from, to string
		wantFrom, wantTo          string
		fails                     bool
	}{
		// The page's own default: seven days back, upper bound left open.
		{name: "default", wantFrom: "2026-08-14T00:00:00Z"},
		{name: "7d", rangeName: "7d", wantFrom: "2026-08-14T00:00:00Z"},
		{name: "30d", rangeName: "30d", wantFrom: "2026-07-22T00:00:00Z"},
		// Unlike the accuracy page, either bound may stand alone.
		{name: "from only", from: "2026-08-01", wantFrom: "2026-08-01T00:00:00Z"},
		{name: "to only", to: "2026-08-05", wantTo: "2026-08-05T23:59:59Z"},
		{name: "both", from: "2026-08-01", to: "2026-08-05",
			wantFrom: "2026-08-01T00:00:00Z", wantTo: "2026-08-05T23:59:59Z"},
		{name: "range with from", rangeName: "7d", from: "2026-08-01", fails: true},
		{name: "bad range", rangeName: "3d", fails: true},
		{name: "bad from", from: "01.08.2026", fails: true},
	}
	for _, tc := range cases {
		from, to, err := resolveDashboardRange(tc.rangeName, tc.from, tc.to, now)
		if tc.fails {
			if err == nil {
				t.Errorf("%s: expected an error", tc.name)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if from != tc.wantFrom || to != tc.wantTo {
			t.Errorf("%s = %q .. %q, want %q .. %q", tc.name, from, to, tc.wantFrom, tc.wantTo)
		}
	}
}

func TestResolveDashboardRadiusMirrorsTheDropdown(t *testing.T) {
	for _, ok := range dashboardRadiusOptions {
		if got, err := resolveDashboardRadius(ok); err != nil || got != ok {
			t.Errorf("radius %d = %d, %v; want it accepted", ok, got, err)
		}
	}
	// A radius the page cannot produce would not be reproducing a page load.
	for _, bad := range []int{0, 3, 25, -5} {
		if _, err := resolveDashboardRadius(bad); err == nil {
			t.Errorf("radius %d must be rejected", bad)
		}
	}
}

func TestParseStationList(t *testing.T) {
	got := parseStationList(" a , b ,, a , c ")
	if strings.Join(got, ",") != "a,b,c" {
		t.Errorf("parseStationList = %v, want a,b,c with the duplicate dropped", got)
	}
	if len(parseStationList("  ")) != 0 {
		t.Error("an empty --station must resolve to no selection")
	}
}

func TestIntersectStationSelection(t *testing.T) {
	inScope := []string{"a", "b", "c"}
	if got := intersectStationSelection(inScope, nil); strings.Join(got, ",") != "a,b,c" {
		t.Errorf("no selection must keep the whole scope, got %v", got)
	}
	// The picker can only narrow: "z" is not in scope, so it cannot come back.
	if got := intersectStationSelection(inScope, []string{"c", "z"}); strings.Join(got, ",") != "c" {
		t.Errorf("selection = %v, want just c", got)
	}
}

func TestDashboardBoundingBoxMirrorsTheViewer(t *testing.T) {
	box := dashboardBoundingBox(52.52, 13.405, 5)
	// The latitude delta is the plain radius-over-degrees the page uses.
	if diff := math.Abs((box.MaxLat-box.MinLat)/2 - 5.0/111.32); diff > 1e-12 {
		t.Errorf("latitude delta is off by %g", diff)
	}
	// Longitude degrees shrink with latitude, so its delta must be the wider one.
	if box.MaxLng-box.MinLng <= box.MaxLat-box.MinLat {
		t.Error("at 52 degrees north the longitude span must exceed the latitude span")
	}
	// A near-polar latitude must not divide by a vanishing cosine.
	polar := dashboardBoundingBox(89.999, 0, 20)
	if math.IsInf(polar.MaxLng, 0) || math.IsNaN(polar.MaxLng) {
		t.Errorf("polar bounding box = %+v, want the clamped divisor to keep it finite", polar)
	}
}

// TestDoctorDashboardQueriesMatchViewer is the guard against the two sides
// drifting: doctor builds these queries in Go because the page builds them in
// PHP, so each one's distinguishing fragment has to still be in web/index.php.
func TestDoctorDashboardQueriesMatchViewer(t *testing.T) {
	viewer, err := os.ReadFile(filepath.Join("web", "index.php"))
	if err != nil {
		t.Fatalf("read viewer: %v", err)
	}
	php := string(viewer)

	signatures := map[string][]string{
		"city": {
			"SELECT normalized_name AS city_key, normalized_name AS city_name, display_name, lat, lng",
			"WHERE normalized_name = ",
		},
		"city_search": {
			// The prefix range is what idx_cities_search can answer; a
			// function around the column, or a LIKE, would not be seekable.
			"WHERE normalized_lower >= ",
			"AND normalized_lower < ",
			"ORDER BY normalized_lower ASC",
		},
		"scope_stations": {
			"COALESCE(NULLIF(TRIM(s.brand), ''), '') AS brand",
			// The freshness rule is what keeps the dashboard's station universe
			// equal to the one suggest and notify work from.
			"FROM price_snapshots fresh",
			"AND fresh.recorded_at >= ",
			"s.lat BETWEEN ",
		},
		"snapshots": {
			"ps.recorded_at >= ",
			"ps.station_id IN (",
			"ORDER BY ps.recorded_at ASC, ps.station_id ASC",
		},
		"predictions_grid": {
			"pp.predicted_price, pp.confidence, pr.run_at, pr.suggestion_bias",
			"AND pp.target_start > :pred_now",
			"ORDER BY pp.target_start ASC, pp.station_id ASC",
			// run_id is what lets the page reduce these rows to the newest run
			// without the second query that used to aggregate over the whole
			// retention window.
			"pp.run_id",
		},
	}

	specs := map[string]dashboardQuerySpec{}
	box := dashboardBoundingBox(52.52, 13.405, 5)
	for _, spec := range dashboardQuerySpecsFor(dashboardQueryContext{
		Filters:     doctorDashboardFilters{City: "berlin", RadiusKM: 5, Fuel: "all", From: "2026-08-14T00:00:00Z"},
		StationIDs:  []string{"near-1", "near-2"},
		FreshCutoff: "2026-08-19T12:00:00Z",
		BBox:        &box,
		Now:         time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC),
	}) {
		specs[spec.name] = spec
	}

	for name, wanted := range signatures {
		spec, ok := specs[name]
		if !ok {
			t.Fatalf("doctor no longer builds a %q query", name)
		}
		for _, sig := range wanted {
			// The PHP binds named parameters where doctor binds positional
			// ones, so a fragment ending at the placeholder is compared up to
			// that point on the doctor side.
			doctorSig := strings.TrimSuffix(sig, ":pred_now")
			if !strings.Contains(spec.sql, doctorSig) {
				t.Errorf("doctor's %s query lost %q:\n%s", name, doctorSig, spec.sql)
			}
			if !strings.Contains(php, sig) {
				t.Errorf("web/index.php no longer contains %q, so doctor's %s query has drifted from the page", sig, name)
			}
		}
	}

	// The raised-9 projection is applied in SQL for snapshots, so its cost is
	// part of what doctor measures; losing it on either side is drift.
	if !strings.Contains(php, "- {$milli} % 10 + 9) / 1000.0") {
		t.Error("web/index.php no longer normalizes snapshot prices in the projection")
	}
	if !strings.Contains(specs["snapshots"].sql, "% 10 + 9) / 1000.0") {
		t.Errorf("doctor's snapshot query lost the raised-9 projection:\n%s", specs["snapshots"].sql)
	}
	// The page's fuel filter expands to three fuels, which is three times the
	// prediction rows; both prediction queries must carry the expansion.
	for _, name := range []string{"predictions_grid"} {
		if !strings.Contains(specs[name].sql, "pp.fuel IN (?, ?, ?)") {
			t.Errorf("doctor's %s query does not expand --fuel all to three fuels:\n%s", name, specs[name].sql)
		}
	}
	// The query this replaced cost 158 s on production because it bounded
	// station and fuel but nothing in time. Neither side may grow it back.
	for _, gone := range []string{"MAX(pr.run_at)", "max_run_at"} {
		if strings.Contains(php, gone) {
			t.Errorf("web/index.php has regrown the unbounded newest-run aggregate (%q); "+
				"the newest run is derived from the grid rows instead", gone)
		}
	}
	if _, ok := specs["predictions_latest"]; ok {
		t.Error("doctor still measures a predictions_latest query the page no longer issues")
	}

	// The freshness constant is mirrored on both sides; predictions_test.go
	// guards the PHP literal, this guards that doctor applies it as a bound.
	if got := specs["scope_stations"].args[len(specs["scope_stations"].args)-1]; got != "2026-08-19T12:00:00Z" {
		t.Errorf("scope_stations binds %v as its freshness cutoff, want the resolved one", got)
	}
}

func TestRunDoctorDashboardEndToEnd(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Hour)
	dbPath, db := seedDashboardDB(t, now)
	db.Close()

	out := captureStdout(t, func() error {
		return run([]string{"doctor", "dashboard", "--db", dbPath, "--city", "berlin", "--radius", "5", "--sql"})
	})
	for _, want := range []string{
		"dashboard queries: city=berlin",
		"scope: ",
		"scope_stations",
		"snapshots",
		"probe/keys only",
		"predictions_grid",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor dashboard output is missing %q:\n%s", want, out)
		}
	}
	// The accuracy page was not asked for, so its section must be absent.
	if strings.Contains(out, "accuracy page queries") {
		t.Errorf("doctor dashboard must not measure the accuracy page:\n%s", out)
	}

	// JSON carries the same picture for a script to read.
	out = captureStdout(t, func() error {
		return run([]string{"doctor", "dashboard", "--db", dbPath, "--city", "berlin", "-o", "json"})
	})
	var report struct {
		Dashboard *struct {
			Filters doctorDashboardFilters `json:"filters"`
			Fuels   []string               `json:"fuels"`
			Queries []struct {
				Name  string            `json:"name"`
				Probe *doctorQueryProbe `json:"probe"`
			} `json:"queries"`
		} `json:"dashboard"`
		Queries []doctorQuery `json:"queries"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("decode json: %v\n%s", err, out)
	}
	if report.Dashboard == nil {
		t.Fatalf("json has no dashboard section:\n%s", out)
	}
	if len(report.Queries) != 0 {
		t.Errorf("json carries %d accuracy queries on a dashboard run", len(report.Queries))
	}
	if strings.Join(report.Dashboard.Fuels, ",") != "e5,e10,diesel" {
		t.Errorf("fuels = %v, want the page's default expansion", report.Dashboard.Fuels)
	}
	probed := 0
	for _, q := range report.Dashboard.Queries {
		if q.Probe != nil {
			probed++
		}
	}
	if probed != 2 {
		t.Errorf("%d queries carry a probe, want the 2 that have one", probed)
	}
}

func TestRunDoctorAllMeasuresBothPages(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Hour)
	dbPath, db := seedDashboardDB(t, now)
	db.Close()

	out := captureStdout(t, func() error {
		return run([]string{"doctor", "all", "--db", dbPath, "--city", "berlin"})
	})
	for _, want := range []string{"accuracy page queries", "dashboard queries: city=berlin"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor all is missing %q:\n%s", want, out)
		}
	}
}

func TestRunDoctorDashboardRejectsInvalidFlags(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Hour)
	dbPath, db := seedDashboardDB(t, now)
	db.Close()

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"unknown subcommand", []string{"predictions", "--db", dbPath}, "unknown doctor subcommand"},
		{"radius off the dropdown", []string{"dashboard", "--db", dbPath, "--radius", "25"}, "--radius must be one of"},
		{"city with no-city", []string{"dashboard", "--db", dbPath, "--city", "berlin", "--no-city"}, "cannot be combined"},
		{"range with from", []string{"dashboard", "--db", dbPath, "--range", "7d", "--from", "2026-08-01"}, "cannot be combined"},
		// --fuel all has no meaning for the accuracy page's queries.
		{"all fuels on both pages", []string{"all", "--db", dbPath, "--fuel", "all"}, "only works for"},
		{"all fuels on accuracy", []string{"accuracy", "--db", dbPath, "--fuel", "all"}, "--fuel must be one of"},
		// Dashboard-only flags must not be silently accepted elsewhere.
		{"city on accuracy", []string{"accuracy", "--db", dbPath, "--city", "berlin"}, "not defined"},
		{"probe on the default page", []string{"--db", dbPath, "--probe"}, "not defined"},
		// --try-index steers accuracy-page queries, of which a dashboard run
		// has none.
		{"try-index on dashboard", []string{"dashboard", "--db", dbPath, "--try-index", "idx_price_predictions_accuracy"}, "--try-index measures the accuracy page"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runDoctor(tc.args)
			if err == nil {
				t.Fatalf("doctor %v must fail", tc.args)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to contain %q", err.Error(), tc.want)
			}
		})
	}
}

func TestDashboardProbesCanBeTurnedOff(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Hour)
	dbPath, db := seedDashboardDB(t, now)
	db.Close()

	out := captureStdout(t, func() error {
		return run([]string{"doctor", "dashboard", "--db", dbPath, "--city", "berlin", "--probe=false"})
	})
	if strings.Contains(out, "probe/") {
		t.Errorf("--probe=false must run no probes:\n%s", out)
	}
	if !strings.Contains(out, "probes were skipped") {
		t.Errorf("--probe=false must say the report is missing those numbers:\n%s", out)
	}
}

// containsFinding reports whether any finding mentions the fragment.
func containsFinding(findings []doctorFinding, fragment string) bool {
	for _, finding := range findings {
		if strings.Contains(finding.Message, fragment) {
			return true
		}
	}
	return false
}

// renderFindings joins findings for a single Contains check.
func renderFindings(findings []doctorFinding) string {
	var b strings.Builder
	for _, finding := range findings {
		b.WriteString(finding.Severity)
		b.WriteString(" ")
		b.WriteString(finding.Message)
		b.WriteString("\n")
	}
	return b.String()
}

// TestCitySearchKeyIsMirroredByTheViewer pins the pairing citySearchKey's doc
// comment describes. The stored column and the search term are folded on
// opposite sides of the wire, so if the two implementations disagree a city
// stops being findable by its own name — and nothing else would notice.
func TestCitySearchKeyIsMirroredByTheViewer(t *testing.T) {
	viewer, err := os.ReadFile(filepath.Join("web", "index.php"))
	if err != nil {
		t.Fatalf("read viewer: %v", err)
	}
	php := string(viewer)

	// mb_strtolower folds beyond ASCII the way strings.ToLower does; plain
	// strtolower leaves Ü alone and would never match the stored key.
	if !strings.Contains(php, "mb_strtolower($q, 'UTF-8')") {
		t.Error("web/index.php no longer folds the search term with mb_strtolower, so it cannot match cities.normalized_lower")
	}
	if strings.Contains(php, "strtolower($q) . '%'") {
		t.Error("web/index.php still builds the old byte-folded LIKE prefix")
	}
	// The half-open range and its upper bound are what make the index usable.
	for _, want := range []string{
		"normalized_lower >= :prefix",
		"normalized_lower < :prefix_end",
		`$prefix . "\u{10FFFF}"`,
	} {
		if !strings.Contains(php, want) {
			t.Errorf("web/index.php no longer contains %q, so the typeahead is not an indexed range any more", want)
		}
	}
	// The fold itself: beyond ASCII, and a no-op on an already-folded name.
	for in, want := range map[string]string{
		"Lübbecke":       "lübbecke",
		"LÜBBECKE":       "lübbecke",
		"lübbecke":       "lübbecke",
		"Bad Oeynhausen": "bad oeynhausen",
	} {
		if got := citySearchKey(in); got != want {
			t.Errorf("citySearchKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestCitySearchRangeFindsNamesRegardlessOfCase runs the typeahead's own query
// — doctor's mirror of it, kept in step by TestDoctorDashboardQueriesMatchViewer
// — against real rows, so the half-open range and its U+10FFFF upper bound are
// exercised rather than reasoned about.
func TestCitySearchRangeFindsNamesRegardlessOfCase(t *testing.T) {
	ctx := context.Background()
	db, err := openDB(filepath.Join(t.TempDir(), "cities.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	if err := initSchema(ctx, db, dialectSQLite); err != nil {
		t.Fatalf("initSchema: %v", err)
	}
	for _, name := range []string{"Lübbecke", "Lübbrechtsen", "Lüchow", "Marl", "Enzberg", "LÜBZ"} {
		if _, err := db.ExecContext(ctx, `INSERT INTO cities
			(name, normalized_name, normalized_lower, display_name, lat, lng, created_at)
			VALUES (?, ?, ?, ?, 52.0, 13.0, '')`,
			name, name, citySearchKey(name), name); err != nil {
			t.Fatalf("insert city: %v", err)
		}
	}

	search := func(term string) []string {
		t.Helper()
		var spec dashboardQuerySpec
		for _, s := range dashboardQuerySpecsFor(dashboardQueryContext{
			Filters: doctorDashboardFilters{City: term, RadiusKM: 5, Fuel: "all"},
			Now:     time.Now().UTC(),
		}) {
			if s.name == "city_search" {
				spec = s
			}
		}
		if spec.name == "" {
			t.Fatalf("no city_search spec for %q", term)
		}
		rows, err := db.QueryContext(ctx, spec.sql, spec.args...)
		if err != nil {
			t.Fatalf("city_search: %v", err)
		}
		defer rows.Close()
		var found []string
		for rows.Next() {
			var key, display string
			if err := rows.Scan(&key, &display); err != nil {
				t.Fatalf("scan: %v", err)
			}
			found = append(found, key)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		return found
	}

	// citySearchTerm folds and takes the first three runes, like the page.
	// "lüb" must reach every spelling of it, whatever case it is stored in.
	if got := search("Lübbecke"); strings.Join(got, ",") != "Lübbecke,Lübbrechtsen,LÜBZ" {
		t.Errorf(`prefix "lüb" found %v, want the three Lüb names including the upper-cased one`, got)
	}
	// The upper bound must not leak into the next prefix.
	if got := search("Marl"); strings.Join(got, ",") != "Marl" {
		t.Errorf(`prefix "mar" found %v, want only Marl`, got)
	}
	if got := search("Xanten"); len(got) != 0 {
		t.Errorf("a prefix nothing starts with found %v, want nothing", got)
	}

	// And the index is what answers it, rather than a scan of the table.
	plan, cells, err := explainPlan(ctx, db, dialectSQLite, "SELECT normalized_name FROM cities "+
		"WHERE normalized_lower >= ? AND normalized_lower < ? ORDER BY normalized_lower ASC LIMIT 20",
		[]any{"lüb", "lüb" + string(rune(0x10FFFF))}, false)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	uses, _, fullScan := classifyPlan(plan, cells, dialectSQLite, []string{"idx_cities_search"}, "cities")
	if uses != "idx_cities_search" || fullScan {
		t.Errorf("typeahead plan uses=%q fullScan=%v, want idx_cities_search and no scan:\n%s",
			uses, fullScan, strings.Join(plan, "\n"))
	}
}
