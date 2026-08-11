package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitSchemaCreatesAuthAndSettingsTables(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	for _, table := range []string{"users", "settings", "update_targets", "notification_state"} {
		var name string
		err := db.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&name)
		if err != nil {
			t.Fatalf("table %s missing: %v", table, err)
		}
	}

	// The notification texts are the whole of the stored configuration.
	want := map[string]string{
		settingCheckTemplate:   defaultCheckTemplate,
		settingSuggestTemplate: defaultSuggestTemplate,
		// Title templates default to empty: notifications fall back to the
		// user's pushover_app_name until an admin configures a template.
		settingCheckTitleTemplate:   "",
		settingSuggestTitleTemplate: "",
	}
	rows, err := db.QueryContext(ctx, `SELECT name, value FROM settings`)
	if err != nil {
		t.Fatalf("query settings: %v", err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var name, value string
		if err := rows.Scan(&name, &value); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[name] = value
	}
	if len(got) != len(want) {
		t.Fatalf("seeded settings count = %d, want %d: %v", len(got), len(want), got)
	}
	for name, value := range want {
		if got[name] != value {
			t.Fatalf("setting %s = %q, want %q", name, got[name], value)
		}
	}
}

func TestMigrateSeedDefaultSettingsIsIdempotentAndKeepsEdits(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `UPDATE settings SET value = 'edited' WHERE name = ?`, settingCheckTemplate); err != nil {
		t.Fatalf("update setting: %v", err)
	}

	result, err := migrateSchema(ctx, db, dialectSQLite)
	if err != nil {
		t.Fatalf("migrateSchema: %v", err)
	}
	if containsString(result.Applied, "settings.seed_defaults") {
		t.Fatalf("second migrate reported seeding again: %v", result.Applied)
	}

	var template string
	if err := db.QueryRowContext(ctx, `SELECT value FROM settings WHERE name = ?`, settingCheckTemplate).Scan(&template); err != nil {
		t.Fatalf("read setting: %v", err)
	}
	if template != "edited" {
		t.Fatalf("check template = %q, want the admin edit to survive", template)
	}
}

func TestMigrateDropsObsoleteSettings(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// An install upgrading from a version that still stored the scope, fuel,
	// model and margin configuration.
	for _, name := range obsoleteSettings {
		if _, err := db.ExecContext(ctx, kvUpsertSQL(dialectSQLite, "settings"),
			name, "stale", "2026-01-01T00:00:00Z"); err != nil {
			t.Fatalf("seed obsolete setting %s: %v", name, err)
		}
	}
	result, err := migrateSchema(ctx, db, dialectSQLite)
	if err != nil {
		t.Fatalf("migrateSchema: %v", err)
	}
	if !containsString(result.Applied, "settings.drop_obsolete") {
		t.Fatalf("migrate did not report dropping obsolete settings: %v", result.Applied)
	}
	var remaining int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM settings`).Scan(&remaining); err != nil {
		t.Fatalf("count settings: %v", err)
	}
	if remaining != len(seededSettings()) {
		t.Fatalf("settings rows = %d, want only the %d seeded templates", remaining, len(seededSettings()))
	}
	// Re-running must not report the drop again.
	second, err := migrateSchema(ctx, db, dialectSQLite)
	if err != nil {
		t.Fatalf("second migrateSchema: %v", err)
	}
	if containsString(second.Applied, "settings.drop_obsolete") {
		t.Fatalf("second migrate reported dropping again: %v", second.Applied)
	}
}

func TestLoadSettingsOverlaysDefaultsAndIgnoresUnknownKeys(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `DELETE FROM settings`); err != nil {
		t.Fatalf("clear settings: %v", err)
	}
	s, err := loadSettings(ctx, db)
	if err != nil {
		t.Fatalf("loadSettings on empty table: %v", err)
	}
	if s != defaultAppSettings() {
		t.Fatalf("empty table settings = %+v, want defaults", s)
	}

	if _, err := db.ExecContext(ctx, kvUpsertSQL(dialectSQLite, "settings"), settingCheckTemplate, "custom", "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("insert setting: %v", err)
	}
	// Keys an older version wrote must not become errors: an install that has
	// not migrated yet still has to run.
	for _, name := range append([]string{"unknown_key"}, obsoleteSettings...) {
		if _, err := db.ExecContext(ctx, kvUpsertSQL(dialectSQLite, "settings"), name, "ignored", "2026-01-01T00:00:00Z"); err != nil {
			t.Fatalf("insert setting %s: %v", name, err)
		}
	}
	s, err = loadSettings(ctx, db)
	if err != nil {
		t.Fatalf("loadSettings: %v", err)
	}
	if s.CheckTemplate != "custom" || s.SuggestTemplate != defaultSuggestTemplate {
		t.Fatalf("settings = %+v, want the stored check template and the default suggest template", s)
	}
}

func TestLoadSettingsUnescapesTemplates(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, kvUpsertSQL(dialectSQLite, "settings"),
		settingSuggestTemplate, `{{weekday_formatted}}\n{{start_time}}\n{{price_formatted}} EUR`, "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("insert setting: %v", err)
	}
	if _, err := db.ExecContext(ctx, kvUpsertSQL(dialectSQLite, "settings"),
		settingCheckTitleTemplate, `Tanken\nfür {{cheapest_price_formatted}} EUR`, "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("insert setting: %v", err)
	}
	s, err := loadSettings(ctx, db)
	if err != nil {
		t.Fatalf("loadSettings: %v", err)
	}
	if want := "{{weekday_formatted}}\n{{start_time}}\n{{price_formatted}} EUR"; s.SuggestTemplate != want {
		t.Fatalf("SuggestTemplate = %q, want %q", s.SuggestTemplate, want)
	}
	if want := "Tanken\nfür {{cheapest_price_formatted}} EUR"; s.CheckTitleTemplate != want {
		t.Fatalf("CheckTitleTemplate = %q, want %q", s.CheckTitleTemplate, want)
	}
	if s.CheckTemplate != defaultCheckTemplate {
		t.Fatalf("CheckTemplate = %q, want default", s.CheckTemplate)
	}
}

func TestSuggestFuelsCoversEveryTrackedFuel(t *testing.T) {
	// Every fuel is always computed, so this list and the validator that
	// guards the SQL column name must not drift apart.
	if want := []string{"diesel", "e5", "e10"}; strings.Join(suggestFuels, ",") != strings.Join(want, ",") {
		t.Fatalf("suggestFuels = %v, want %v", suggestFuels, want)
	}
	for _, fuel := range suggestFuels {
		if !isSuggestFuelType(fuel) {
			t.Errorf("isSuggestFuelType(%q) = false, want true", fuel)
		}
		if _, err := suggestFuelColumn(fuel); err != nil {
			t.Errorf("suggestFuelColumn(%q): %v", fuel, err)
		}
	}
	if isSuggestFuelType("premium") {
		t.Error("isSuggestFuelType accepted an unknown fuel")
	}
}

func TestLoadUpdateTargetsOrdered(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	for _, target := range []struct {
		city   string
		radius float64
	}{{"Berlin", 10}, {"Hamburg", 25}} {
		if _, err := db.ExecContext(ctx, `INSERT INTO update_targets (city, radius_km, created_at) VALUES (?, ?, ?)`,
			target.city, target.radius, "2026-01-01T00:00:00Z"); err != nil {
			t.Fatalf("insert target: %v", err)
		}
	}
	targets, err := loadUpdateTargets(ctx, db)
	if err != nil {
		t.Fatalf("loadUpdateTargets: %v", err)
	}
	if len(targets) != 2 || targets[0].City != "Berlin" || targets[0].RadiusKM != 10 || targets[1].City != "Hamburg" || targets[1].RadiusKM != 25 {
		t.Fatalf("targets = %+v", targets)
	}
}

func TestNotificationStateRoundTripAndBaselineClear(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if _, found, err := getNotificationState(ctx, db, "check_baseline:diesel:Berlin"); err != nil || found {
		t.Fatalf("unexpected state: found=%v err=%v", found, err)
	}
	if err := setNotificationState(ctx, db, dialectSQLite, "check_baseline:diesel:Berlin", "1.599"); err != nil {
		t.Fatalf("setNotificationState: %v", err)
	}
	if err := setNotificationState(ctx, db, dialectSQLite, "check_baseline:diesel:Berlin", "1.549"); err != nil {
		t.Fatalf("setNotificationState upsert: %v", err)
	}
	if err := setNotificationState(ctx, db, dialectSQLite, "check_baseline_reset_date", "2026-04-26"); err != nil {
		t.Fatalf("setNotificationState: %v", err)
	}
	value, found, err := getNotificationState(ctx, db, "check_baseline:diesel:Berlin")
	if err != nil || !found || value != "1.549" {
		t.Fatalf("state = %q found=%v err=%v, want 1.549", value, found, err)
	}
	if err := clearCheckBaselines(ctx, db); err != nil {
		t.Fatalf("clearCheckBaselines: %v", err)
	}
	if _, found, _ := getNotificationState(ctx, db, "check_baseline:diesel:Berlin"); found {
		t.Fatal("baseline survived clearCheckBaselines")
	}
	if _, found, _ := getNotificationState(ctx, db, "check_baseline_reset_date"); !found {
		t.Fatal("reset marker must survive clearCheckBaselines")
	}
}

func TestRunUpdateUsesUpdateTargetsWhenNoFlags(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "targets.db")
	t.Setenv(envAPIKeyName, "test-key")

	// Seed targets in a pre-initialized database.
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	ctx := context.Background()
	if err := initSchema(ctx, db, dialectSQLite); err != nil {
		t.Fatalf("initSchema: %v", err)
	}
	for _, target := range []struct {
		city   string
		radius float64
	}{{"Berlin", 10}, {"Pforzheim", 25}} {
		if _, err := db.ExecContext(ctx, `INSERT INTO update_targets (city, radius_km, created_at) VALUES (?, ?, ?)`,
			target.city, target.radius, "2026-01-01T00:00:00Z"); err != nil {
			t.Fatalf("insert target: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

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
			radByLat[u.Query().Get("lat")] = u.Query().Get("rad")
			body := `{"ok":true,"stations":[{"id":"s-1","name":"S","brand":"B","street":"St","place":"P","lat":1,"lng":2,"dist":1,"diesel":1.5,"e5":1.7,"e10":1.6,"isOpen":true,"houseNumber":"1","postCode":1}]}`
			return jsonResponse(http.StatusOK, body), nil
		default:
			return nil, fmt.Errorf("unexpected URL: %s", u.String())
		}
	})
	defer restore()

	output := captureStdout(t, func() error {
		return run([]string{"update", "--db", dbPath, "--output", "json"})
	})

	var result multiUpdateResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("unmarshal: %v\noutput=%s", err, output)
	}
	if len(result.Results) != 2 {
		t.Fatalf("results = %d, want 2 target cities", len(result.Results))
	}
	if radByLat["52.500000"] != "10.00" || radByLat["48.900000"] != "25.00" {
		t.Fatalf("per-target radii = %v, want Berlin=10.00 Pforzheim=25.00", radByLat)
	}
}

func TestRunUpdateExplicitFlagsIgnoreTargets(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "flags.db")
	t.Setenv(envAPIKeyName, "test-key")

	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	ctx := context.Background()
	if err := initSchema(ctx, db, dialectSQLite); err != nil {
		t.Fatalf("initSchema: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO update_targets (city, radius_km, created_at) VALUES (?, ?, ?)`,
		"Pforzheim", 25.0, "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("insert target: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	var cities []string
	restore := stubDefaultTransport(t, func(req *http.Request) (*http.Response, error) {
		u := req.URL
		switch {
		case strings.HasPrefix(u.String(), nominatimBaseURL):
			cities = append(cities, u.Query().Get("q"))
			return jsonResponse(http.StatusOK, `[{"name":"Berlin","display_name":"Berlin, DE","lat":"52.5","lon":"13.4"}]`), nil
		case strings.HasPrefix(u.String(), tankerKoenigBase+"/list.php"):
			body := `{"ok":true,"stations":[{"id":"s-1","name":"S","brand":"B","street":"St","place":"P","lat":1,"lng":2,"dist":1,"diesel":1.5,"e5":1.7,"e10":1.6,"isOpen":true,"houseNumber":"1","postCode":1}]}`
			return jsonResponse(http.StatusOK, body), nil
		default:
			return nil, fmt.Errorf("unexpected URL: %s", u.String())
		}
	})
	defer restore()

	captureStdout(t, func() error {
		return run([]string{"update", "--db", dbPath, "--city", "Berlin", "--output", "json"})
	})
	if len(cities) != 1 || !strings.Contains(cities[0], "Berlin") {
		t.Fatalf("geocoded cities = %v, want only the explicit Berlin", cities)
	}
}

func TestRunUpdateNoFlagsNoTargetsErrors(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "empty.db")
	t.Setenv(envAPIKeyName, "test-key")

	err := run([]string{"update", "--db", dbPath})
	if err == nil || !strings.Contains(err.Error(), "--city") {
		t.Fatalf("err = %v, want update requires --city", err)
	}
}

func TestPriceCheckVerdictHonoursMargin(t *testing.T) {
	// Percentile held at 50 so only the margin can decide the verdict.
	const percentile = 50.0
	// 3 ct below the reference: "low" under a 2 ct margin, merely "typical"
	// under a 5 ct one.
	if got := priceCheckVerdict(1.67, 1.70, percentile, 0.020); got != "low" {
		t.Fatalf("verdict with a 2 ct margin = %q, want low", got)
	}
	if got := priceCheckVerdict(1.67, 1.70, percentile, 0.050); got != "typical" {
		t.Fatalf("verdict with a 5 ct margin = %q, want typical", got)
	}
	if got := priceCheckVerdict(1.73, 1.70, percentile, 0.020); got != "high" {
		t.Fatalf("verdict above the reference with a 2 ct margin = %q, want high", got)
	}
	if got := priceCheckVerdict(1.73, 1.70, percentile, 0.050); got != "typical" {
		t.Fatalf("verdict above the reference with a 5 ct margin = %q, want typical", got)
	}
}

func TestMigrateBackfillsNotifyLocationOnASingleTargetInstall(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `
		CREATE TABLE user_notify_cities (
			user_id INTEGER NOT NULL,
			city TEXT NOT NULL,
			created_at TEXT NOT NULL,
			PRIMARY KEY (user_id, city)
		)`); err != nil {
		t.Fatalf("recreate legacy table: %v", err)
	}
	insertSuggestCity(t, db, cachedCity{QueryName: "Berlin", Name: "Berlin", DisplayName: "Berlin", Lat: 52.517389, Lng: 13.395131})
	insertUpdateTargetRow(t, db, "Berlin", 25)
	if _, err := db.ExecContext(ctx, kvUpsertSQL(dialectSQLite, "settings"),
		"range_km", "8", "2026-04-01T00:00:00Z"); err != nil {
		t.Fatalf("seed legacy range: %v", err)
	}
	// The ordinary case: one target, and a user on the old default of selecting
	// nothing. "All cities" and "the one city" are the same area here, so their
	// notifications must keep working rather than being switched off.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO users (email, password_hash, status, created_at, pushover_user_key, pushover_token, notify_check_enabled)
		VALUES ('default@example.com', 'x', 'approved', '2026-04-01T00:00:00Z', 'k', 't', 1)`); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	result, err := migrateSchema(ctx, db, dialectSQLite)
	if err != nil {
		t.Fatalf("migrateSchema: %v", err)
	}
	if !containsString(result.Applied, "users.notify_location.backfilled=1") {
		t.Fatalf("migrate did not backfill the empty selection: %v", result.Applied)
	}
	for _, step := range result.Applied {
		if strings.HasPrefix(step, "users.notify_location.needs_area") {
			t.Fatalf("a single-target install must need no review: %v", result.Applied)
		}
	}
	var city string
	var lat, lng, radius float64
	if err := db.QueryRowContext(ctx,
		`SELECT notify_city, notify_lat, notify_lng, notify_radius_km FROM users WHERE email = 'default@example.com'`,
	).Scan(&city, &lat, &lng, &radius); err != nil {
		t.Fatalf("read location: %v", err)
	}
	// At the legacy range, not the 25 km collection radius.
	if city != "Berlin" || lat != 52.517389 || lng != 13.395131 || radius != 8 {
		t.Fatalf("location = %s %v/%v r%v, want Berlin at the legacy range 8", city, lat, lng, radius)
	}
}

func TestMigrateBackfillsNotifyLocationFromCitySelection(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// An install from before notifications had their own location: users were
	// subscribed to a set of admin update targets.
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE user_notify_cities (
			user_id INTEGER NOT NULL,
			city TEXT NOT NULL,
			created_at TEXT NOT NULL,
			PRIMARY KEY (user_id, city)
		)`); err != nil {
		t.Fatalf("recreate legacy table: %v", err)
	}
	insertSuggestCity(t, db, cachedCity{QueryName: "Berlin", Name: "Berlin", DisplayName: "Berlin", Lat: 52.517389, Lng: 13.395131})
	insertSuggestCity(t, db, cachedCity{QueryName: "Hamburg", Name: "Hamburg", DisplayName: "Hamburg", Lat: 53.550556, Lng: 9.993333})
	// The collection radii are deliberately unlike the notification range: the
	// old notify path measured with settings.range_km around the selected city,
	// while a target's radius only decided what got collected.
	insertUpdateTargetRow(t, db, "Berlin", 7)
	insertUpdateTargetRow(t, db, "Hamburg", 12)
	if _, err := db.ExecContext(ctx, kvUpsertSQL(dialectSQLite, "settings"),
		"range_km", "5", "2026-04-01T00:00:00Z"); err != nil {
		t.Fatalf("seed legacy range: %v", err)
	}

	// The first three would receive notifications; the fourth never configured
	// Pushover, so nothing changes for them either way.
	for _, u := range []struct {
		email    string
		pushover bool
	}{
		{"picked@example.com", true},
		{"multi@example.com", true},
		{"none@example.com", true},
		{"dormant@example.com", false},
	} {
		key, token := "", ""
		if u.pushover {
			key, token = "k", "t"
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO users (email, password_hash, status, created_at, pushover_user_key, pushover_token, notify_check_enabled)
			VALUES (?, 'x', 'approved', '2026-04-01T00:00:00Z', ?, ?, 1)`,
			u.email, key, token); err != nil {
			t.Fatalf("insert user %s: %v", u.email, err)
		}
	}
	// User 1 picked Hamburg; user 2 picked both; users 3 and 4 picked nothing,
	// which used to mean every city.
	for _, sel := range []struct {
		userID int
		city   string
	}{{1, "Hamburg"}, {2, "Hamburg"}, {2, "Berlin"}} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO user_notify_cities (user_id, city, created_at) VALUES (?, ?, '2026-04-01T00:00:00Z')`,
			sel.userID, sel.city); err != nil {
			t.Fatalf("insert selection: %v", err)
		}
	}

	var result migrateResult
	stderr := captureStderr(t, func() {
		var err error
		result, err = migrateSchema(ctx, db, dialectSQLite)
		if err != nil {
			t.Fatalf("migrateSchema: %v", err)
		}
	})
	if !containsString(result.Applied, "users.drop_notify_cities") {
		t.Fatalf("migrate did not drop the legacy table: %v", result.Applied)
	}
	// Exactly one selection is expressible as an area.
	if !containsString(result.Applied, "users.notify_location.backfilled=1") {
		t.Fatalf("migrate did not backfill the single-city user: %v", result.Applied)
	}
	// The other two are reported, by address, because their old selection cannot
	// become one area and the selection is about to be gone.
	if !containsString(result.Applied, "users.notify_location.needs_area=2") {
		t.Fatalf("migrate did not report the users needing an area: %v", result.Applied)
	}
	for _, email := range []string{"multi@example.com", "none@example.com"} {
		if !strings.Contains(stderr, email) {
			t.Errorf("migrate did not name %s as needing an area: %q", email, stderr)
		}
	}
	if strings.Contains(stderr, "dormant@example.com") {
		t.Errorf("a user who receives nothing anyway was reported: %q", stderr)
	}

	type loc struct {
		city     string
		lat, lng float64
		radius   float64
	}
	got := map[int64]loc{}
	rows, err := db.QueryContext(ctx, `SELECT id, notify_city, notify_lat, notify_lng, notify_radius_km FROM users ORDER BY id`)
	if err != nil {
		t.Fatalf("read locations: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var l loc
		if err := rows.Scan(&id, &l.city, &l.lat, &l.lng, &l.radius); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[id] = l
	}
	// The single selection is carried over at the legacy notification range, not
	// at Hamburg's 12 km collection radius.
	if got[1] != (loc{"Hamburg", 53.550556, 9.993333, 5}) {
		t.Errorf("user 1 = %+v, want Hamburg at the legacy range 5", got[1])
	}
	// Everyone else is left without an area rather than being narrowed to one
	// city they never asked for.
	for _, id := range []int64{2, 3, 4} {
		if got[id] != (loc{}) {
			t.Errorf("user %d = %+v, want no invented area", id, got[id])
		}
	}

	// Re-running is a no-op.
	second, err := migrateSchema(ctx, db, dialectSQLite)
	if err != nil {
		t.Fatalf("second migrateSchema: %v", err)
	}
	for _, step := range []string{"users.notify_location", "users.drop_notify_cities", "users.notify_location.backfilled=1"} {
		if containsString(second.Applied, step) {
			t.Errorf("second migrate reported %s again: %v", step, second.Applied)
		}
	}
}
