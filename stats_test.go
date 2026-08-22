package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// statsHistorySnapshots is how many rows seedStatsHistory writes: 28 days of
// hourly prices plus one fresh low snapshot.
const statsHistorySnapshots = 28*24 + 1

// commandRunRow is the recorded run as the Statistics page reads it.
type commandRunRow struct {
	ID         int64
	Command    string
	StartedAt  string
	FinishedAt sql.NullString
	DurationMS sql.NullInt64
	Status     string
	Error      sql.NullString
	Host       string
	Version    string
}

func readCommandRuns(t *testing.T, dbPath string) []commandRunRow {
	t.Helper()
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	rows, err := db.QueryContext(context.Background(), `
		SELECT id, command, started_at, finished_at, duration_ms, status, error, host, version
		FROM command_runs ORDER BY id
	`)
	if err != nil {
		t.Fatalf("query command_runs: %v", err)
	}
	defer rows.Close()

	var out []commandRunRow
	for rows.Next() {
		var r commandRunRow
		if err := rows.Scan(&r.ID, &r.Command, &r.StartedAt, &r.FinishedAt, &r.DurationMS, &r.Status, &r.Error, &r.Host, &r.Version); err != nil {
			t.Fatalf("scan command_runs: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("command_runs rows: %v", err)
	}
	return out
}

func readCommandRunMetrics(t *testing.T, dbPath string, runID int64) map[string]float64 {
	t.Helper()
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	rows, err := db.QueryContext(context.Background(),
		`SELECT name, value FROM command_run_metrics WHERE run_id = ?`, runID)
	if err != nil {
		t.Fatalf("query command_run_metrics: %v", err)
	}
	defer rows.Close()

	out := map[string]float64{}
	for rows.Next() {
		var name string
		var value float64
		if err := rows.Scan(&name, &value); err != nil {
			t.Fatalf("scan command_run_metrics: %v", err)
		}
		out[name] = value
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("command_run_metrics rows: %v", err)
	}
	return out
}

// wantMetrics asserts the exact metric set, so a renamed or dropped metric —
// the contract the Statistics page renders — fails here rather than silently
// blanking a column in the UI.
func wantMetrics(t *testing.T, got map[string]float64, want map[string]float64) {
	t.Helper()
	for name, value := range want {
		actual, ok := got[name]
		if !ok {
			t.Fatalf("metric %q missing, got %v", name, got)
		}
		if actual != value {
			t.Fatalf("metric %q = %v, want %v", name, actual, value)
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Fatalf("unexpected metric %q = %v", name, got[name])
		}
	}
}

func singleCommandRun(t *testing.T, dbPath, command string) (commandRunRow, map[string]float64) {
	t.Helper()
	runs := readCommandRuns(t, dbPath)
	if len(runs) != 1 {
		t.Fatalf("command_runs = %d rows, want 1: %+v", len(runs), runs)
	}
	if runs[0].Command != command {
		t.Fatalf("command = %q, want %q", runs[0].Command, command)
	}
	if !runs[0].FinishedAt.Valid || runs[0].FinishedAt.String == "" {
		t.Fatalf("finished_at not set: %+v", runs[0])
	}
	if !runs[0].DurationMS.Valid || runs[0].DurationMS.Int64 < 0 {
		t.Fatalf("duration_ms = %+v, want a non-negative value", runs[0].DurationMS)
	}
	if runs[0].Version == "" {
		t.Fatalf("version not recorded: %+v", runs[0])
	}
	return runs[0], readCommandRunMetrics(t, dbPath, runs[0].ID)
}

// runQuiet runs a command that is expected to fail, keeping its output off the
// test log. captureStdout cannot be used here: it fails the test on a non-nil
// error, and the error is what this asserts on.
func runQuiet(t *testing.T, args ...string) error {
	t.Helper()
	old := stdout
	stdout = io.Discard
	t.Cleanup(func() { stdout = old })
	return run(args)
}

func TestCommandRunRecordsUpdate(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "stats-update.db")
	t.Setenv(envAPIKeyName, "test-key")

	restore := stubDefaultTransport(t, func(req *http.Request) (*http.Response, error) {
		switch {
		case strings.HasPrefix(req.URL.String(), nominatimBaseURL):
			return jsonResponse(http.StatusOK, `[{"name":"Berlin","display_name":"Berlin, DE","lat":"52.5","lon":"13.4"}]`), nil
		case strings.HasPrefix(req.URL.String(), tankerKoenigBase+"/list.php"):
			return jsonResponse(http.StatusOK, `{"ok":true,"stations":[{"id":"station-1","name":"S","brand":"B","street":"St","place":"P","lat":52.5,"lng":13.4,"dist":1,"diesel":1.5,"e5":1.7,"e10":1.6,"isOpen":true,"houseNumber":"1","postCode":10115}]}`), nil
		default:
			return nil, fmt.Errorf("unexpected request URL: %s", req.URL.String())
		}
	})
	defer restore()

	captureStdout(t, func() error {
		return run([]string{"update", "--db", dbPath, "--city", "Berlin", "--output", "json"})
	})

	row, metrics := singleCommandRun(t, dbPath, "update")
	if row.Status != commandRunStatusOK {
		t.Fatalf("status = %q, want %q", row.Status, commandRunStatusOK)
	}
	if row.Error.Valid && row.Error.String != "" {
		t.Fatalf("error = %q, want empty", row.Error.String)
	}
	wantMetrics(t, metrics, map[string]float64{
		"cities":           1,
		"cities_failed":    0,
		"stations_fetched": 1,
		"snapshots_stored": 1,
	})
}

func TestCommandRunRecordsPartialFailure(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "stats-partial.db")
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
			if strings.HasPrefix(u.Query().Get("lat"), "48.9") {
				return nil, fmt.Errorf("simulated upstream failure")
			}
			return jsonResponse(http.StatusOK, `{"ok":true,"stations":[{"id":"s-1","name":"S","brand":"B","street":"St","place":"P","lat":52.5,"lng":13.4,"dist":1,"diesel":1.5,"e5":1.7,"e10":1.6,"isOpen":true,"houseNumber":"1","postCode":1}]}`), nil
		default:
			return nil, fmt.Errorf("unexpected URL: %s", u.String())
		}
	})
	defer restore()

	err := runQuiet(t, "update", "--db", dbPath, "--city", "Berlin", "--city", "Pforzheim", "--output", "json")
	if err == nil {
		t.Fatal("run returned nil, want the best-effort failure error")
	}

	row, metrics := singleCommandRun(t, dbPath, "update")
	if row.Status != commandRunStatusPartial {
		t.Fatalf("status = %q, want %q", row.Status, commandRunStatusPartial)
	}
	// A degraded run still carries the error, so the page can say what broke.
	if !row.Error.Valid || !strings.Contains(row.Error.String, "1 of 2 cities failed") {
		t.Fatalf("error = %+v, want it to mention the failed city", row.Error)
	}
	wantMetrics(t, metrics, map[string]float64{
		"cities":           2,
		"cities_failed":    1,
		"stations_fetched": 1,
		"snapshots_stored": 1,
	})
}

// seedStatsHistory gives one station enough open diesel history for the
// forecast model to produce a result.
func seedStatsHistory(t *testing.T, dbPath string) {
	t.Helper()
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := initSchema(context.Background(), db, dialectSQLite); err != nil {
		t.Fatalf("initSchema: %v", err)
	}
	insertSuggestCity(t, db, cachedCity{
		QueryName:   "Berlin",
		Name:        "Berlin",
		DisplayName: "Berlin",
		Lat:         52.517389,
		Lng:         13.395131,
	})
	insertSuggestStation(t, db, "station-1", "Station 1", 52.517389, 13.395131)

	nowLocal := time.Now().In(time.Local)
	// Four weeks of flat hourly history, so the model reaches medium/high
	// confidence and notify's delivery filter lets the buy-now row through.
	for daysAgo := 28; daysAgo >= 1; daysAgo-- {
		dayStart := localDayStart(nowLocal).AddDate(0, 0, -daysAgo)
		for hour := 0; hour < 24; hour++ {
			insertSuggestSnapshot(t, db, "station-1", "Berlin", dayStart.Add(time.Duration(hour)*time.Hour).In(time.UTC), 2.100, true)
		}
	}
	insertSuggestSnapshot(t, db, "station-1", "Berlin", time.Now().UTC(), 2.000, true)
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
}

// seedStatsNotifyRecipient adds the update target and the approved, always-in-
// window Pushover user notify needs before it does any work. It is anchored on
// the current clock, unlike notify_test.go's fixtures, because these tests go
// through run() and so cannot inject a fixed now.
func seedStatsNotifyRecipient(t *testing.T, dbPath string) {
	t.Helper()
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO update_targets (city, radius_km, created_at) VALUES (?, ?, ?)`,
		"Berlin", 5.0, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("insert target: %v", err)
	}
	seedNotifyUser(t, db, notifyUserFixture{
		Email:           "stats@example.com",
		UserKey:         "user-key-1",
		Token:           "token-1",
		Days:            defaultNotifyDays,
		Windows:         "00:00-23:59",
		CheckEnabled:    true,
		SuggestDisabled: true,
	})
}

func seedStatsNotifyRecipient2(t *testing.T, dbPath string) {
	t.Helper()
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	seedNotifyUser(t, db, notifyUserFixture{
		Email:           "stats2@example.com",
		UserKey:         "user-key-2",
		Token:           "token-2",
		Days:            defaultNotifyDays,
		Windows:         "00:00-23:59",
		CheckEnabled:    true,
		SuggestDisabled: true,
	})
}

func TestCommandRunRecordsCheck(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "stats-check.db")
	seedStatsHistory(t, dbPath)

	captureStdout(t, func() error {
		return run([]string{"check", "--db", dbPath, "--output", "json"})
	})

	row, metrics := singleCommandRun(t, dbPath, "check")
	if row.Status != commandRunStatusOK {
		t.Fatalf("status = %q, want %q", row.Status, commandRunStatusOK)
	}
	wantMetrics(t, metrics, map[string]float64{
		"fuels":             float64(len(suggestFuels)),
		"fuels_failed":      0,
		"stations":          1,
		"snapshots_scanned": statsHistorySnapshots,
		// The one station is checked once per fuel.
		"check_rows": float64(len(suggestFuels)),
	})
}

func TestCommandRunRecordsSuggestPersistCounters(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "stats-suggest.db")
	seedStatsHistory(t, dbPath)

	if err := run([]string{"suggest", "--db", dbPath, "--persist", "--quiet"}); err != nil {
		t.Fatalf("suggest --persist: %v", err)
	}

	row, metrics := singleCommandRun(t, dbPath, "suggest")
	if row.Status != commandRunStatusOK {
		t.Fatalf("status = %q, want %q", row.Status, commandRunStatusOK)
	}
	if metrics["persist"] != 1 {
		t.Fatalf("persist = %v, want 1", metrics["persist"])
	}
	if metrics["stations"] != 1 {
		t.Fatalf("stations = %v, want 1", metrics["stations"])
	}
	if metrics["predictions_stored"] <= 0 {
		t.Fatalf("predictions_stored = %v, want a positive count", metrics["predictions_stored"])
	}
	// Every persist-only counter has to be present, including the zero ones:
	// the page renders the set, and a missing name reads as "no data".
	for _, name := range []string{
		"decisions_stored", "predictions_evaluated", "outcomes_scored",
		"stations_bias_corrected", "pruned_predictions", "pruned_decisions",
		"unfed_stations", "unfed_predictions", "unfed_decisions",
	} {
		if _, ok := metrics[name]; !ok {
			t.Fatalf("metric %q missing, got %v", name, metrics)
		}
	}
}

func TestCommandRunSuggestWithoutPersistOmitsPersistCounters(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "stats-suggest-plain.db")
	seedStatsHistory(t, dbPath)

	captureStdout(t, func() error {
		return run([]string{"suggest", "--db", dbPath, "--output", "json"})
	})

	_, metrics := singleCommandRun(t, dbPath, "suggest")
	wantMetrics(t, metrics, map[string]float64{
		"persist":           0,
		"fuels":             float64(len(suggestFuels)),
		"fuels_failed":      0,
		"stations":          1,
		"snapshots_scanned": statsHistorySnapshots,
	})
}

func TestCommandRunNotifyDryRunRecordsNothing(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "stats-notify.db")
	seedStatsHistory(t, dbPath)
	seedStatsNotifyRecipient(t, dbPath)
	pushes := stubPushover(t, nil)

	captureStdout(t, func() error {
		return run([]string{"notify", "--db", dbPath, "--dry-run"})
	})

	if runs := readCommandRuns(t, dbPath); len(runs) != 0 {
		t.Fatalf("command_runs = %+v, want no rows for a dry run", runs)
	}
	if len(*pushes) != 0 {
		t.Fatalf("dry run sent %d notifications, want none", len(*pushes))
	}

	// The same command without --dry-run does record, so the emptiness above
	// is the dry-run rule and not a broken recorder.
	captureStdout(t, func() error {
		return run([]string{"notify", "--db", dbPath})
	})
	row, metrics := singleCommandRun(t, dbPath, "notify")
	if row.Status != commandRunStatusOK {
		t.Fatalf("status = %q, want %q", row.Status, commandRunStatusOK)
	}
	if metrics["users"] != 1 {
		t.Fatalf("users = %v, want 1", metrics["users"])
	}
	if metrics["stations"] != 1 {
		t.Fatalf("stations = %v, want 1", metrics["stations"])
	}
	if metrics["sent"] != float64(len(*pushes)) {
		t.Fatalf("sent = %v, want %d to match the deliveries", metrics["sent"], len(*pushes))
	}
	if metrics["failed"] != 0 {
		t.Fatalf("failed = %v, want 0", metrics["failed"])
	}
	for _, name := range []string{"check_rows", "suggest_rows", "baseline_reset"} {
		if _, ok := metrics[name]; !ok {
			t.Fatalf("metric %q missing, got %v", name, metrics)
		}
	}
}

func TestCommandRunNotifyRecordsPartialWhenSomeSendsFail(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "stats-notify-partial.db")
	seedStatsHistory(t, dbPath)
	seedStatsNotifyRecipient(t, dbPath)
	seedStatsNotifyRecipient2(t, dbPath)
	// One recipient's Pushover call fails; the other's succeeds.
	pushes := stubPushover(t, func(user string) bool { return user == "user-key-2" })

	captureStdout(t, func() error {
		return run([]string{"notify", "--db", dbPath})
	})

	row, metrics := singleCommandRun(t, dbPath, "notify")
	if metrics["sent"] == 0 || metrics["failed"] == 0 {
		t.Fatalf("sent=%v failed=%v, want both non-zero for a partial run (pushes=%d)",
			metrics["sent"], metrics["failed"], len(*pushes))
	}
	if row.Status != commandRunStatusPartial {
		t.Fatalf("status = %q, want %q", row.Status, commandRunStatusPartial)
	}
}

// TestCommandRunNotifyRecordsErrorWhenEverySendFails covers both output modes.
// The JSON path used to return straight out of writeJSON, so the deferred
// recorder saw no error and stored 'ok' on a run that delivered nothing — the
// exact opposite of what happened, and a zero exit code with it.
func TestCommandRunNotifyRecordsErrorWhenEverySendFails(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"json", []string{"--output", "json"}},
		{"text", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			dbPath := filepath.Join(dir, "stats-notify-allfail.db")
			seedStatsHistory(t, dbPath)
			seedStatsNotifyRecipient(t, dbPath)
			// Every Pushover call fails, so there is nothing partial about it.
			pushes := stubPushover(t, func(string) bool { return true })

			args := append([]string{"notify", "--db", dbPath}, tc.args...)
			err := runQuiet(t, args...)
			if err == nil {
				t.Fatal("run returned nil, want the all-sends-failed error")
			}
			if !strings.Contains(err.Error(), "notification sends failed") {
				t.Fatalf("error = %v, want the all-sends-failed error", err)
			}
			if len(*pushes) != 0 {
				t.Fatalf("delivered %d notifications, want none", len(*pushes))
			}

			row, metrics := singleCommandRun(t, dbPath, "notify")
			if row.Status != commandRunStatusError {
				t.Fatalf("status = %q, want %q", row.Status, commandRunStatusError)
			}
			if !row.Error.Valid || !strings.Contains(row.Error.String, "notification sends failed") {
				t.Fatalf("recorded error = %+v, want the all-sends-failed error", row.Error)
			}
			if metrics["sent"] != 0 {
				t.Fatalf("sent = %v, want 0", metrics["sent"])
			}
			if metrics["failed"] != 1 {
				t.Fatalf("failed = %v, want 1", metrics["failed"])
			}
		})
	}
}

// TestCommandRunRecordsSingleCityFailure covers the one-target bail-out, which
// returns from inside the fetch loop. It used to skip the metric block at the
// end of runUpdate entirely, so the commonest failure on a single-target
// install recorded an error run carrying no counters at all.
func TestCommandRunRecordsSingleCityFailure(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "stats-onecity.db")
	t.Setenv(envAPIKeyName, "test-key")

	restore := stubDefaultTransport(t, func(req *http.Request) (*http.Response, error) {
		u := req.URL
		switch {
		case strings.HasPrefix(u.String(), nominatimBaseURL):
			return jsonResponse(http.StatusOK, `[{"name":"Berlin","display_name":"Berlin, DE","lat":"52.5","lon":"13.4"}]`), nil
		case strings.HasPrefix(u.String(), tankerKoenigBase+"/list.php"):
			return nil, fmt.Errorf("simulated upstream failure")
		default:
			return nil, fmt.Errorf("unexpected URL: %s", u.String())
		}
	})
	defer restore()

	err := runQuiet(t, "update", "--db", dbPath, "--city", "Berlin", "--output", "json")
	if err == nil {
		t.Fatal("run returned nil, want the fetch failure")
	}
	// A single target keeps reporting the fetch error itself, not the
	// best-effort tally the multi-city path produces.
	if !strings.Contains(err.Error(), "simulated upstream failure") {
		t.Fatalf("error = %v, want the raw fetch failure", err)
	}
	if strings.Contains(err.Error(), "cities failed") {
		t.Fatalf("error = %v, want the single-city shape preserved", err)
	}

	row, metrics := singleCommandRun(t, dbPath, "update")
	if row.Status != commandRunStatusError {
		t.Fatalf("status = %q, want %q", row.Status, commandRunStatusError)
	}
	wantMetrics(t, metrics, map[string]float64{
		"cities":           1,
		"cities_failed":    1,
		"stations_fetched": 0,
		"snapshots_stored": 0,
	})
}

func TestCommandRunSurvivesStatsWriteFailure(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "stats-broken.db")
	seedStatsHistory(t, dbPath)

	// Drop the tables behind the recorder's back: statistics are a diagnostic,
	// so losing them must not change what the command does or prints.
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	for _, stmt := range []string{`DROP TABLE command_run_metrics`, `DROP TABLE command_runs`} {
		if _, err := db.ExecContext(context.Background(), stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	// initSchema would recreate them, so go straight at the command internals
	// the way the command does, with the tables already gone.
	db, err = openDB(dbPath)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer db.Close()

	stats := beginCommandRun(context.Background(), db, "check")
	stats.set("check_rows", 3)
	stats.markPartial()
	stats.finish(context.Background(), nil)

	if _, err := db.ExecContext(context.Background(), `SELECT 1`); err != nil {
		t.Fatalf("database unusable after a failed stats write: %v", err)
	}
}

func TestCompactPrunesOldCommandRuns(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "stats-compact.db")

	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	ctx := context.Background()
	if err := initSchema(ctx, db, dialectSQLite); err != nil {
		t.Fatalf("initSchema: %v", err)
	}

	now := time.Now().UTC()
	insert := func(command string, age time.Duration) int64 {
		t.Helper()
		started := now.Add(-age)
		res, err := db.ExecContext(ctx, `
			INSERT INTO command_runs (command, started_at, finished_at, duration_ms, status, host, version)
			VALUES (?, ?, ?, ?, 'ok', 'host', 'test')
		`, command, started.Format(time.RFC3339), started.Add(time.Second).Format(time.RFC3339), 1000)
		if err != nil {
			t.Fatalf("insert run: %v", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			t.Fatalf("last insert id: %v", err)
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO command_run_metrics (run_id, name, value) VALUES (?, 'cities', 1)`, id); err != nil {
			t.Fatalf("insert metric: %v", err)
		}
		return id
	}
	oldID := insert("update", 40*24*time.Hour)
	freshID := insert("update", 5*24*time.Hour)
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	output := captureStdout(t, func() error {
		return run([]string{"compact", "--db", dbPath, "--output", "json"})
	})
	var result compactResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("unmarshal compact output: %v\noutput=%s", err, output)
	}
	if result.PrunedCommandRuns != 1 {
		t.Fatalf("pruned_command_runs = %d, want 1", result.PrunedCommandRuns)
	}

	runs := readCommandRuns(t, dbPath)
	if len(runs) != 1 || runs[0].ID != freshID {
		t.Fatalf("runs = %+v, want only the fresh run %d", runs, freshID)
	}
	// The metrics go with their run, or the child rows outlive the foreign key.
	if metrics := readCommandRunMetrics(t, dbPath, oldID); len(metrics) != 0 {
		t.Fatalf("metrics for the pruned run = %v, want none", metrics)
	}
	if metrics := readCommandRunMetrics(t, dbPath, freshID); len(metrics) != 1 {
		t.Fatalf("metrics for the fresh run = %v, want the one seeded", metrics)
	}
}

func TestCompactReportsPrunedRunsInTextOutput(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "stats-compact-text.db")

	output := captureStdout(t, func() error {
		return run([]string{"compact", "--db", dbPath})
	})
	want := fmt.Sprintf("pruned 0 command run records older than %d days", commandRunRetentionDays)
	if !strings.Contains(output, want) {
		t.Fatalf("output = %q, want it to contain %q", output, want)
	}
}

// TestDoctorKnowsTheCommandRunTables guards the two hardcoded lists in
// doctor.go: a new table missing from doctorTables goes unreported, and one
// missing from doctorExpectedIndexes has all its indexes flagged Unexpected.
func TestDoctorKnowsTheCommandRunTables(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "doctor-stats.db")

	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := initSchema(context.Background(), db, dialectSQLite); err != nil {
		t.Fatalf("initSchema: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	output := captureStdout(t, func() error {
		return run([]string{"doctor", "--db", dbPath, "--skip-queries", "--output", "json"})
	})
	var report doctorResult
	if err := json.Unmarshal([]byte(output), &report); err != nil {
		t.Fatalf("unmarshal doctor output: %v\noutput=%s", err, output)
	}

	reported := map[string]bool{}
	for _, table := range report.Tables {
		reported[table.Name] = true
	}
	for _, want := range []string{"command_runs", "command_run_metrics"} {
		if !reported[want] {
			t.Fatalf("doctor did not report table %q; tables=%+v", want, report.Tables)
		}
	}

	seen := map[string]bool{}
	for _, idx := range report.Indexes {
		if !strings.HasPrefix(idx.Table, "command_run") {
			continue
		}
		seen[idx.Name] = true
		if !idx.Present {
			t.Fatalf("index %s.%s missing from the schema", idx.Table, idx.Name)
		}
		if idx.Unexpected {
			t.Fatalf("index %s.%s flagged unexpected; add it to doctorExpectedIndexes", idx.Table, idx.Name)
		}
	}
	for _, want := range []string{
		"idx_command_runs_command_started",
		"idx_command_runs_started",
		"idx_command_run_metrics_run",
	} {
		if !seen[want] {
			t.Fatalf("doctor did not report index %q", want)
		}
	}
}

// TestSchemaStatementsCreateCommandRunTables checks both dialect branches carry
// the same logical schema, since only the SQLite one is exercised by the rest
// of the suite.
func TestSchemaStatementsCreateCommandRunTables(t *testing.T) {
	for _, d := range []dialect{dialectSQLite, dialectMySQL} {
		joined := strings.Join(schemaStatements(d), "\n")
		for _, want := range []string{
			"CREATE TABLE IF NOT EXISTS command_runs",
			"CREATE TABLE IF NOT EXISTS command_run_metrics",
			"idx_command_runs_command_started",
			"idx_command_runs_started",
			"idx_command_run_metrics_run",
			"REFERENCES command_runs(id)",
		} {
			if !strings.Contains(joined, want) {
				t.Fatalf("dialect %v schema missing %q", d, want)
			}
		}
	}

	// migrate-to-mysql copies only what its manifest lists, and the parent has
	// to precede the child so the foreign key holds mid-copy.
	var runsAt, metricsAt = -1, -1
	for i, table := range migrationTables {
		switch table.name {
		case "command_runs":
			runsAt = i
		case "command_run_metrics":
			metricsAt = i
		}
	}
	if runsAt < 0 || metricsAt < 0 {
		t.Fatalf("migrationTables missing the command run tables: %+v", migrationTables)
	}
	if runsAt > metricsAt {
		t.Fatalf("command_runs is copied after command_run_metrics, breaking the foreign key")
	}
}

// A recorded error can carry a city or station name, and the column is
// utf8mb4: truncation must not leave half a rune behind.
func TestTruncateRunErrorKeepsValidUTF8(t *testing.T) {
	for _, pad := range []int{0, 1, 2, 3} {
		msg := strings.Repeat("a", commandRunErrorLimit-pad) + strings.Repeat("ü", 20)
		got := truncateRunError(msg)
		if !utf8.ValidString(got) {
			t.Fatalf("pad=%d produced invalid UTF-8", pad)
		}
		if len([]rune(got)) > commandRunErrorLimit+1 {
			t.Fatalf("pad=%d result too long: %d runes", pad, len([]rune(got)))
		}
	}
	short := "boom"
	if truncateRunError(short) != short {
		t.Fatalf("short message was altered")
	}
}
