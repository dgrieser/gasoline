package main

import (
	"context"
	"encoding/json"
	"flag"
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

	want := map[string]string{
		settingFuel:            "diesel",
		settingRangeKM:         "5",
		settingHistoryDays:     "21",
		settingPredictDays:     "3",
		settingLimitPerDay:     "3",
		settingCheckLimit:      "5",
		settingSuggestTimes:    "08:00,13:00",
		settingCheckResetTime:  "00:00",
		settingNotifyDays:      "mon,tue,wed,thu,fri,sat,sun",
		settingNotifyWindows:   "07:00-21:00",
		settingCheckTemplate:   defaultCheckTemplate,
		settingSuggestTemplate: defaultSuggestTemplate,
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

	if _, err := db.ExecContext(ctx, `UPDATE settings SET value = 'e5' WHERE name = ?`, settingFuel); err != nil {
		t.Fatalf("update setting: %v", err)
	}

	result, err := migrateSchema(ctx, db, dialectSQLite)
	if err != nil {
		t.Fatalf("migrateSchema: %v", err)
	}
	if containsString(result.Applied, "settings.seed_defaults") {
		t.Fatalf("second migrate reported seeding again: %v", result.Applied)
	}

	var fuel string
	if err := db.QueryRowContext(ctx, `SELECT value FROM settings WHERE name = ?`, settingFuel).Scan(&fuel); err != nil {
		t.Fatalf("read setting: %v", err)
	}
	if fuel != "e5" {
		t.Fatalf("fuel = %q, want the admin edit e5 to survive", fuel)
	}
}

func TestLoadSettingsOverlaysDefaultsAndRejectsBadNumbers(t *testing.T) {
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

	if _, err := db.ExecContext(ctx, kvUpsertSQL(dialectSQLite, "settings"), settingFuel, "e10", "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("insert setting: %v", err)
	}
	if _, err := db.ExecContext(ctx, kvUpsertSQL(dialectSQLite, "settings"), settingHistoryDays, "30", "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("insert setting: %v", err)
	}
	if _, err := db.ExecContext(ctx, kvUpsertSQL(dialectSQLite, "settings"), "unknown_key", "ignored", "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("insert setting: %v", err)
	}
	s, err = loadSettings(ctx, db)
	if err != nil {
		t.Fatalf("loadSettings: %v", err)
	}
	if s.Fuel != "e10" || s.HistoryDays != 30 || s.PredictDays != 3 {
		t.Fatalf("settings = %+v, want fuel=e10 history=30 predict=3", s)
	}

	if _, err := db.ExecContext(ctx, kvUpsertSQL(dialectSQLite, "settings"), settingRangeKM, "abc", "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("insert setting: %v", err)
	}
	if _, err := loadSettings(ctx, db); err == nil || !strings.Contains(err.Error(), settingRangeKM) {
		t.Fatalf("loadSettings error = %v, want error naming %s", err, settingRangeKM)
	}
}

func TestApplySuggestSettingsPrecedence(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `UPDATE settings SET value = 'e5' WHERE name = ?`, settingFuel); err != nil {
		t.Fatalf("update setting: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE settings SET value = '12' WHERE name = ?`, settingRangeKM); err != nil {
		t.Fatalf("update setting: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO update_targets (city, radius_km, created_at) VALUES (?, ?, ?)`,
		"Berlin", 10.0, "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("insert target: %v", err)
	}

	// No explicit flags: DB settings win over hardcoded defaults.
	fs := flag.NewFlagSet("suggest", flag.ContinueOnError)
	fs.String("city", "", "")
	fs.Float64("range-km", 5, "")
	fs.String("fuel", "diesel", "")
	fs.Int("history-days", 21, "")
	fs.Int("predict-days", 3, "")
	fs.Int("limit-per-day", 3, "")
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("parse: %v", err)
	}
	opts := suggestOptions{City: "", RangeKM: 5, Fuel: "diesel", HistoryDays: 21, PredictDays: 3, LimitPerDay: 3}
	if err := applySuggestSettings(ctx, db, fs, &opts); err != nil {
		t.Fatalf("applySuggestSettings: %v", err)
	}
	if opts.Fuel != "e5" || opts.RangeKM != 12 || opts.City != "Berlin" {
		t.Fatalf("opts = %+v, want fuel=e5 range=12 city=Berlin", opts)
	}

	// Explicit flags beat the DB.
	fs = flag.NewFlagSet("suggest", flag.ContinueOnError)
	fs.String("city", "", "")
	fs.Float64("range-km", 5, "")
	fs.String("fuel", "diesel", "")
	fs.Int("history-days", 21, "")
	fs.Int("predict-days", 3, "")
	fs.Int("limit-per-day", 3, "")
	if err := fs.Parse([]string{"--fuel", "diesel", "--city", "Hamburg"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	opts = suggestOptions{City: "Hamburg", RangeKM: 5, Fuel: "diesel", HistoryDays: 21, PredictDays: 3, LimitPerDay: 3}
	if err := applySuggestSettings(ctx, db, fs, &opts); err != nil {
		t.Fatalf("applySuggestSettings: %v", err)
	}
	if opts.Fuel != "diesel" || opts.City != "Hamburg" {
		t.Fatalf("opts = %+v, want explicit fuel=diesel city=Hamburg preserved", opts)
	}
	if opts.RangeKM != 12 {
		t.Fatalf("range = %v, want DB value 12 for unset flag", opts.RangeKM)
	}
}

func TestApplyCheckSettingsPrecedence(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `UPDATE settings SET value = '9' WHERE name = ?`, settingCheckLimit); err != nil {
		t.Fatalf("update setting: %v", err)
	}
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.String("city", "", "")
	fs.Float64("range-km", 5, "")
	fs.String("fuel", "diesel", "")
	fs.Int("history-days", 21, "")
	fs.Int("predict-days", 3, "")
	fs.Int("limit", 5, "")
	if err := fs.Parse([]string{"--limit", "2"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	opts := checkOptions{RangeKM: 5, Fuel: "diesel", HistoryDays: 21, PredictDays: 3, Limit: 2}
	if err := applyCheckSettings(ctx, db, fs, &opts); err != nil {
		t.Fatalf("applyCheckSettings: %v", err)
	}
	if opts.Limit != 2 {
		t.Fatalf("limit = %d, want explicit flag 2", opts.Limit)
	}

	fs = flag.NewFlagSet("check", flag.ContinueOnError)
	fs.String("city", "", "")
	fs.Float64("range-km", 5, "")
	fs.String("fuel", "diesel", "")
	fs.Int("history-days", 21, "")
	fs.Int("predict-days", 3, "")
	fs.Int("limit", 5, "")
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("parse: %v", err)
	}
	opts = checkOptions{RangeKM: 5, Fuel: "diesel", HistoryDays: 21, PredictDays: 3, Limit: 5}
	if err := applyCheckSettings(ctx, db, fs, &opts); err != nil {
		t.Fatalf("applyCheckSettings: %v", err)
	}
	if opts.Limit != 9 {
		t.Fatalf("limit = %d, want DB value 9", opts.Limit)
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
