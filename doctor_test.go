package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// seedDoctorDB builds a small but structurally complete database: two stations,
// several runs, and evaluated predictions across confidences, leads and target
// hours, so every accuracy-page query has something to aggregate.
func seedDoctorDB(t *testing.T) (string, *sql.DB) {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "doctor.db")
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := initSchema(ctx, db, dialectSQLite); err != nil {
		t.Fatalf("initSchema: %v", err)
	}
	for i, id := range []string{"st-1", "st-2"} {
		if _, err := db.ExecContext(ctx, `INSERT INTO stations
			(id, name, brand, street, house_number, post_code, place, lat, lng, first_seen_at, last_seen_at)
			VALUES (?, ?, 'Brand', 'Street', '1', 12345, 'Berlin', ?, 13.0, '', '')`,
			id, "Station "+id, 52.0+float64(i)/100); err != nil {
			t.Fatalf("insert station: %v", err)
		}
	}
	base := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	confs := []string{"low", "medium", "high"}
	for run := 0; run < 4; run++ {
		runAt := base.Add(time.Duration(run) * time.Hour)
		res, err := db.ExecContext(ctx, `INSERT INTO prediction_runs
			(run_at, city_name, fuel, range_km, history_days, predict_days)
			VALUES (?, 'Berlin', 'diesel', 5, 30, 3)`, runAt.Format(time.RFC3339))
		if err != nil {
			t.Fatalf("insert run: %v", err)
		}
		runID, _ := res.LastInsertId()
		for _, station := range []string{"st-1", "st-2"} {
			for lead := 1; lead <= 3; lead++ {
				target := runAt.Add(time.Duration(lead) * time.Hour)
				predicted := 1.70 + float64(lead)/100
				actual := predicted + 0.005
				if _, err := db.ExecContext(ctx, `INSERT INTO price_predictions
					(run_id, station_id, fuel, target_start, target_end, predicted_price, confidence,
					 sample_count, is_suggestion, lead_minutes, applied_correction, actual_price, error, evaluated_at)
					VALUES (?, ?, 'diesel', ?, ?, ?, ?, 20, 0, ?, 0.0, ?, ?, ?)`,
					runID, station,
					target.Format(time.RFC3339), target.Add(time.Hour).Format(time.RFC3339),
					predicted, confs[lead%len(confs)], lead*60, actual, actual-predicted,
					target.Add(2*time.Hour).Format(time.RFC3339)); err != nil {
					t.Fatalf("insert prediction: %v", err)
				}
			}
			if _, err := db.ExecContext(ctx, `INSERT INTO price_check_decisions
				(run_id, station_id, fuel, decided_at, target_start, target_end, observed_price, observed_at,
				 predicted_price, error, history_percentile, confidence, sample_count, verdict, recommendation,
				 expected_lower, expected_drop, day_floor_price, day_floor_at, regret, outcome_evaluated_at)
				VALUES (?, ?, 'diesel', ?, ?, ?, 1.72, ?, 1.71, 0.01, 0.4, 'medium', 20, 'low', 'buy',
				 0, NULL, 1.71, ?, 0.01, ?)`,
				runID, station, runAt.Format(time.RFC3339),
				runAt.Format(time.RFC3339), runAt.Add(time.Hour).Format(time.RFC3339),
				runAt.Format(time.RFC3339), runAt.Format(time.RFC3339),
				runAt.Add(3*time.Hour).Format(time.RFC3339)); err != nil {
				t.Fatalf("insert decision: %v", err)
			}
		}
	}
	return dbPath, db
}

// pageSpecs builds the queries as the page runs them: hints included, since the
// covering index exists on any migrated database.
func pageSpecs(f doctorFilters, hasDecisions bool) []accuracyQuerySpec {
	return accuracyQuerySpecsFor(accuracyQueryContext{
		Filters: f, Dialect: dialectSQLite, HasDecisions: hasDecisions, AccuracyIndexPresent: true,
	})
}

// unhintedSpecs builds them as a database without the covering index gets them.
func unhintedSpecs(f doctorFilters, hasDecisions bool) []accuracyQuerySpec {
	return accuracyQuerySpecsFor(accuracyQueryContext{
		Filters: f, Dialect: dialectSQLite, HasDecisions: hasDecisions, AccuracyIndexPresent: false,
	})
}

// forcedSpecs builds them the way --try-index does.
func forcedSpecs(f doctorFilters, hasDecisions bool, index string) []accuracyQuerySpec {
	return accuracyQuerySpecsFor(accuracyQueryContext{
		Filters: f, Dialect: dialectSQLite, HasDecisions: hasDecisions,
		AccuracyIndexPresent: true, ForceIndex: index,
	})
}

func TestRunDoctorReportsTablesIndexesAndQueries(t *testing.T) {
	dbPath, db := seedDoctorDB(t)
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	output := captureStdout(t, func() error {
		return run([]string{"doctor", "--db", dbPath, "--from", "2026-07-01", "--to", "2026-07-31", "--output", "json"})
	})

	var result doctorResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("unmarshal doctor output: %v\noutput=%s", err, output)
	}

	if result.Database.Driver != string(dialectSQLite) {
		t.Fatalf("driver = %q, want %q", result.Database.Driver, dialectSQLite)
	}
	if result.Database.Version == "" || result.Database.Version == "unknown" {
		t.Fatalf("server version = %q, want a real version", result.Database.Version)
	}

	tables := map[string]doctorTable{}
	for _, tbl := range result.Tables {
		tables[tbl.Name] = tbl
	}
	// 4 runs x 2 stations x 3 leads.
	if got := tables["price_predictions"].Rows; got != 24 {
		t.Fatalf("price_predictions rows = %d, want 24", got)
	}
	if tables["price_predictions"].RowsApproximate {
		t.Fatal("SQLite counts are exact, not approximate")
	}
	if tables["price_predictions"].Missing {
		t.Fatal("price_predictions reported missing")
	}

	var accuracy *doctorIndex
	for i := range result.Indexes {
		if result.Indexes[i].Name == "idx_price_predictions_accuracy" {
			accuracy = &result.Indexes[i]
		}
	}
	if accuracy == nil {
		t.Fatal("accuracy index not reported at all")
	}
	if !accuracy.Present {
		t.Fatal("accuracy index reported missing on a freshly created schema")
	}
	wantColumns := []string{"fuel", "target_start", "station_id", "run_id",
		"error", "actual_price", "predicted_price", "confidence", "lead_minutes"}
	if strings.Join(accuracy.Columns, ",") != strings.Join(wantColumns, ",") {
		t.Fatalf("accuracy index columns = %v, want %v", accuracy.Columns, wantColumns)
	}

	// Every page query must run, return rows, and be attributed to a table.
	wantQueries := []string{"summary", "summary_latest", "by_confidence", "by_lead",
		"by_hour", "series", "rows", "decisions"}
	got := map[string]doctorQuery{}
	for _, q := range result.Queries {
		got[q.Name] = q
	}
	for _, name := range wantQueries {
		q, ok := got[name]
		if !ok {
			t.Fatalf("query %s missing from report; got %v", name, result.Queries)
		}
		if q.Error != "" {
			t.Fatalf("query %s failed: %s", name, q.Error)
		}
		if q.Rows == 0 {
			t.Fatalf("query %s returned no rows against seeded data", name)
		}
		if q.Table == "" {
			t.Fatalf("query %s has no table attribution", name)
		}
		if len(q.Plan) == 0 {
			t.Fatalf("query %s has no plan", name)
		}
	}
	if got["decisions"].Table != "price_check_decisions" {
		t.Fatalf("decisions query table = %q, want price_check_decisions", got["decisions"].Table)
	}
}

// TestRunDoctorFlagsMissingIndex is the case that matters operationally: an
// install that never ran the migration should be told which index is absent
// rather than left to read plans.
func TestRunDoctorFlagsMissingIndex(t *testing.T) {
	ctx := context.Background()
	dbPath, db := seedDoctorDB(t)
	if _, err := db.ExecContext(ctx, `DROP INDEX idx_price_predictions_accuracy`); err != nil {
		t.Fatalf("drop index: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	output := captureStdout(t, func() error {
		return run([]string{"doctor", "--db", dbPath, "--skip-queries", "--output", "json"})
	})
	var result doctorResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("unmarshal: %v\noutput=%s", err, output)
	}

	for _, idx := range result.Indexes {
		if idx.Name == "idx_price_predictions_accuracy" && idx.Present {
			t.Fatal("dropped index still reported present")
		}
	}
	var warned bool
	for _, f := range result.Findings {
		if f.Severity == "warn" &&
			strings.Contains(f.Message, "idx_price_predictions_accuracy") &&
			strings.Contains(f.Message, "is missing") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("no warning about the missing index; findings=%v", result.Findings)
	}
	// --skip-queries must not silently run them anyway.
	for _, q := range result.Queries {
		if !q.Skipped {
			t.Fatalf("--skip-queries still ran %s", q.Name)
		}
	}
}

// TestRunDoctorRefusesToCreateADatabase covers the no-argument case. Opening a
// SQLite path creates the file, which every other command wants; here it would
// leave a stray empty database behind and answer "every table is absent" when
// the real answer is that there is no database at that path.
func TestRunDoctorRefusesToCreateADatabase(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "nope.db")
	err := run([]string{"doctor", "--db", missing})
	if err == nil {
		t.Fatal("expected an error for a database that does not exist")
	}
	if !strings.Contains(err.Error(), "no database at") {
		t.Fatalf("error = %q, want it to name the missing database", err.Error())
	}
	if _, statErr := os.Stat(missing); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("doctor created the database file it was asked to inspect")
	}
	if entries, readErr := os.ReadDir(dir); readErr != nil {
		t.Fatalf("readdir: %v", readErr)
	} else if len(entries) != 0 {
		t.Fatalf("doctor left %d files behind", len(entries))
	}
}

// TestRunDoctorSkipsQueriesWithoutTheTable keeps an un-migrated database from
// producing the same missing-table error once per query, which buries the one
// finding that matters.
func TestRunDoctorSkipsQueriesWithoutTheTable(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "bare.db")
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE unrelated (x INTEGER)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	output := captureStdout(t, func() error {
		return run([]string{"doctor", "--db", dbPath, "--output", "json"})
	})
	var result doctorResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("unmarshal: %v\noutput=%s", err, output)
	}
	if len(result.Queries) != 1 {
		t.Fatalf("got %d query entries, want a single skipped one", len(result.Queries))
	}
	if !result.Queries[0].Skipped {
		t.Fatalf("query %s ran without price_predictions", result.Queries[0].Name)
	}
	if !strings.Contains(result.Queries[0].Purpose, "price_predictions does not exist") {
		t.Fatalf("skip reason = %q, want it to name the missing table", result.Queries[0].Purpose)
	}
	for _, q := range result.Queries {
		if q.Error != "" {
			t.Fatalf("skipped query still reported an error: %s", q.Error)
		}
	}
}

// TestRunDoctorLeavesDatabaseUntouched pins doctor as read-only. It must not
// create or migrate the schema: an operator diagnosing a database should not
// have it changed under them, and a missing table is itself a finding.
func TestRunDoctorLeavesDatabaseUntouched(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "empty.db")
	// A zero-byte file is a valid, schemaless SQLite database — the case where
	// the database is really there and really empty, as opposed to absent.
	if err := os.WriteFile(dbPath, nil, 0o644); err != nil {
		t.Fatalf("create empty db: %v", err)
	}

	output := captureStdout(t, func() error {
		return run([]string{"doctor", "--db", dbPath, "--output", "json"})
	})
	var result doctorResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("unmarshal: %v\noutput=%s", err, output)
	}
	for _, tbl := range result.Tables {
		if !tbl.Missing {
			t.Fatalf("table %s exists after doctor ran on an empty database", tbl.Name)
		}
	}

	reopened, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	exists, err := tableExists(ctx, reopened, dialectSQLite, "price_predictions")
	if err != nil {
		t.Fatalf("tableExists: %v", err)
	}
	if exists {
		t.Fatal("doctor created the schema; it must stay read-only")
	}
}

func TestRunDoctorRejectsInvalidFlags(t *testing.T) {
	dbPath, db := seedDoctorDB(t)
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"bad fuel", []string{"doctor", "--db", dbPath, "--fuel", "kerosene"}, "--fuel must be one of"},
		{"bad confidence", []string{"doctor", "--db", dbPath, "--confidence", "high"}, "--confidence must be one of"},
		{"bad range", []string{"doctor", "--db", dbPath, "--range", "90d"}, "--range must be one of"},
		{"range with from", []string{"doctor", "--db", dbPath, "--range", "7d", "--from", "2026-07-01"}, "cannot be combined"},
		{"half range", []string{"doctor", "--db", dbPath, "--from", "2026-07-01"}, "must be given together"},
		{"bad date", []string{"doctor", "--db", dbPath, "--from", "01.07.2026", "--to", "2026-07-31"}, "YYYY-MM-DD"},
		{"positional", []string{"doctor", "--db", dbPath, "extra"}, "no positional arguments"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := run(tc.args)
			if err == nil {
				t.Fatalf("expected an error for %v", tc.args)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to contain %q", err.Error(), tc.want)
			}
		})
	}
}

func TestResolveDoctorRange(t *testing.T) {
	now := time.Date(2026, 8, 5, 14, 30, 0, 0, time.UTC)
	cases := []struct {
		name           string
		rangeName      string
		from, to       string
		wantFrom       string
		wantTo         string
		wantErrPortion string
	}{
		{name: "default is 14 days", wantFrom: "2026-07-22T00:00:00Z", wantTo: "2026-08-05T23:59:59Z"},
		{name: "7d", rangeName: "7d", wantFrom: "2026-07-29T00:00:00Z", wantTo: "2026-08-05T23:59:59Z"},
		{name: "30d", rangeName: "30d", wantFrom: "2026-07-06T00:00:00Z", wantTo: "2026-08-05T23:59:59Z"},
		{name: "explicit", from: "2026-06-01", to: "2026-06-30",
			wantFrom: "2026-06-01T00:00:00Z", wantTo: "2026-06-30T23:59:59Z"},
		{name: "unknown range", rangeName: "90d", wantErrPortion: "--range must be one of"},
		{name: "range plus from", rangeName: "7d", from: "2026-06-01", wantErrPortion: "cannot be combined"},
		{name: "from without to", from: "2026-06-01", wantErrPortion: "must be given together"},
		{name: "malformed date", from: "2026-6-1", to: "2026-06-30", wantErrPortion: "YYYY-MM-DD"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			from, to, err := resolveDoctorRange(tc.rangeName, tc.from, tc.to, now)
			if tc.wantErrPortion != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrPortion) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantErrPortion)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if from != tc.wantFrom || to != tc.wantTo {
				t.Fatalf("range = %s .. %s, want %s .. %s", from, to, tc.wantFrom, tc.wantTo)
			}
		})
	}
}

// TestClassifyPlanScopesVerdictsToTheDrivingTable covers the plan shapes that
// look alarming but are not: the accuracy queries also walk a materialised
// subquery and the tiny stations table, and counting those as table scans
// would report a healthy page as broken.
func TestClassifyPlanScopesVerdictsToTheDrivingTable(t *testing.T) {
	indexes := []string{"idx_price_predictions_accuracy", "idx_price_predictions_station_fuel_target"}

	t.Run("sqlite covering read through a subquery", func(t *testing.T) {
		plan := []string{
			"CO-ROUTINE latest",
			"SEARCH pp USING COVERING INDEX idx_price_predictions_accuracy (fuel=? AND target_start>?)",
			"SCAN latest",
			"SEARCH pp USING COVERING INDEX idx_price_predictions_accuracy (target_start=? AND station_id=?)",
		}
		uses, covering, fullScan := classifyPlan(plan, nil, dialectSQLite, indexes, "pp")
		if uses != "idx_price_predictions_accuracy" || !covering {
			t.Fatalf("uses=%q covering=%v, want the accuracy index, covering", uses, covering)
		}
		if fullScan {
			t.Fatal("SCAN latest is a materialised subquery, not a table scan")
		}
	})

	t.Run("sqlite scan of a joined small table", func(t *testing.T) {
		plan := []string{
			"SCAN s",
			"SEARCH pp USING INDEX idx_price_predictions_station_fuel_target (station_id=? AND fuel=?)",
			"SEARCH pr USING INTEGER PRIMARY KEY (rowid=?)",
			"USE TEMP B-TREE FOR ORDER BY",
		}
		uses, covering, fullScan := classifyPlan(plan, nil, dialectSQLite, indexes, "pp")
		if uses != "idx_price_predictions_station_fuel_target" {
			t.Fatalf("uses = %q, want the station index", uses)
		}
		if covering {
			t.Fatal("plan is not a covering read")
		}
		if fullScan {
			t.Fatal("SCAN s is the stations table, not price_predictions")
		}
	})

	t.Run("sqlite genuine table scan", func(t *testing.T) {
		plan := []string{"SCAN pp", "USE TEMP B-TREE FOR GROUP BY"}
		uses, _, fullScan := classifyPlan(plan, nil, dialectSQLite, indexes, "pp")
		if uses != "" {
			t.Fatalf("uses = %q, want none", uses)
		}
		if !fullScan {
			t.Fatal("SCAN pp with no index is a table scan and must be reported")
		}
	})

	// The MySQL fixtures below are real EXPLAIN rows from the production
	// database, which is where the two bugs these cases pin were found: the
	// verdict was read out of the rendered text, so `possible_keys` was
	// mistaken for the chosen index and `select_type=PRIMARY` was mistaken for
	// the PRIMARY key.
	t.Run("mysql reports the chosen key, not a candidate", func(t *testing.T) {
		cells := []map[string]string{{
			"id": "1", "select_type": "SIMPLE", "table": "pp", "type": "ref",
			"possible_keys": "idx_price_predictions_due,idx_price_predictions_accuracy",
			"key":           "idx_price_predictions_due",
			"key_len":       "66", "ref": "const", "rows": "251558", "Extra": "Using where",
		}}
		uses, covering, fullScan := classifyPlan(nil, cells, dialectMySQL, indexes, "pp")
		if uses != "idx_price_predictions_due" {
			t.Fatalf("uses = %q, want the key MySQL committed to, not a candidate", uses)
		}
		if covering || fullScan {
			t.Fatalf("covering=%v fullScan=%v, want neither", covering, fullScan)
		}
	})

	t.Run("mysql select_type=PRIMARY is not an index", func(t *testing.T) {
		cells := []map[string]string{
			{"id": "1", "select_type": "PRIMARY", "table": "<derived2>", "type": "ALL", "rows": "25150"},
			{"id": "1", "select_type": "PRIMARY", "table": "pp", "type": "ref",
				"possible_keys": "idx_price_predictions_station_fuel_target,run_id",
				"key":           "idx_price_predictions_station_fuel_target",
				"key_len":       "258", "ref": "latest.station_id", "rows": "184",
				"Extra": "Using index condition; Using where"},
			{"id": "2", "select_type": "DERIVED", "table": "pp", "type": "ref",
				"key": "idx_price_predictions_due", "rows": "251558",
				"Extra": "Using where; Using temporary"},
		}
		uses, covering, fullScan := classifyPlan(nil, cells, dialectMySQL, indexes, "pp")
		if uses != "idx_price_predictions_station_fuel_target" {
			t.Fatalf("uses = %q, want the station index; \"PRIMARY\" here is a select_type", uses)
		}
		if covering {
			t.Fatal("\"Using index condition\" still reads rows and is not a covering read")
		}
		if fullScan {
			t.Fatal("type=ALL on <derived2> is the materialised subquery, not price_predictions")
		}
	})

	t.Run("mysql covering read", func(t *testing.T) {
		cells := []map[string]string{{
			"id": "1", "select_type": "SIMPLE", "table": "pp", "type": "ref",
			"key": "idx_price_predictions_accuracy", "rows": "251558",
			"Extra": "Using where; Using index",
		}}
		uses, covering, fullScan := classifyPlan(nil, cells, dialectMySQL, indexes, "pp")
		if uses != "idx_price_predictions_accuracy" || !covering || fullScan {
			t.Fatalf("uses=%q covering=%v fullScan=%v, want accuracy index, covering, no scan", uses, covering, fullScan)
		}
	})

	t.Run("mysql full scan", func(t *testing.T) {
		cells := []map[string]string{{
			"id": "1", "select_type": "SIMPLE", "table": "d", "type": "ALL",
			"possible_keys": "idx_price_check_decisions_due", "rows": "5677",
			"Extra": "Using where; Using temporary",
		}}
		uses, _, fullScan := classifyPlan(nil, cells, dialectMySQL, indexes, "d")
		if !fullScan {
			t.Fatal("type=ALL on the driving table is a full scan")
		}
		if uses != "" {
			t.Fatalf("uses = %q, want none: a full scan uses no index", uses)
		}
	})

	t.Run("mysql scan of another table is ignored", func(t *testing.T) {
		cells := []map[string]string{
			{"id": "1", "select_type": "SIMPLE", "table": "s", "type": "ALL", "rows": "360",
				"Extra": "Using temporary; Using filesort"},
			{"id": "1", "select_type": "SIMPLE", "table": "pp", "type": "ref",
				"key": "idx_price_predictions_station_fuel_target", "rows": "184",
				"Extra": "Using index condition; Using where"},
		}
		_, _, fullScan := classifyPlan(nil, cells, dialectMySQL, indexes, "pp")
		if fullScan {
			t.Fatal("a scan of the stations table must not be blamed on price_predictions")
		}
	})

	t.Run("mysql explain analyze falls back to text", func(t *testing.T) {
		plan := []string{
			"-> Aggregate: count(0)",
			"    -> Index range scan on pp using idx_price_predictions_accuracy, with index condition",
		}
		uses, _, _ := classifyPlan(plan, nil, dialectMySQL, indexes, "pp")
		if uses != "idx_price_predictions_accuracy" {
			t.Fatalf("uses = %q, want the accuracy index from the text form", uses)
		}
	})
}

// TestDoctorAccuracyQueriesMatchViewer guards the duplication doctor accepts:
// its SQL mirrors web/index.php's accuracy handler, which is PHP and cannot be
// shared. Each signature must appear on both sides, so changing either the page
// or doctor without the other fails here.
func TestDoctorAccuracyQueriesMatchViewer(t *testing.T) {
	viewer, err := os.ReadFile(filepath.Join("web", "index.php"))
	if err != nil {
		t.Fatalf("read viewer: %v", err)
	}
	php := string(viewer)

	signatures := map[string][]string{
		"summary": {
			"COUNT(DISTINCT pp.station_id)",
			"AVG(pp.error * pp.error)",
			"SUM(CASE WHEN ABS(pp.error) <= 0.01 THEN 1 ELSE 0 END)",
		},
		"summary_latest": {
			"MAX(pp.run_id)",
			"GROUP BY pp.station_id, pp.target_start",
			"latest.run_id = pp.run_id",
			// The outer fuel predicate is what makes the covering index usable
			// for the join; losing it silently costs ~18 s on the live
			// database, so both sides must keep it.
			"WHERE pp.fuel = ",
		},
		"by_confidence": {"GROUP BY pp.confidence"},
		"by_lead": {
			"WHEN pp.lead_minutes < 360 THEN '3-6h'",
			"MIN(pp.lead_minutes)",
		},
		"by_hour": {"SUBSTR(pp.target_start, 12, 2)"},
		"series":  {"AVG(pp.predicted_price)", "GROUP BY pp.target_start"},
		"rows": {
			"COALESCE(s.name_override, s.name)",
			"ORDER BY pp.target_start DESC, pp.station_id ASC",
			// Filter, order and cap must stay inside the derived table, ahead
			// of the metadata joins.
			") page ",
			"JOIN prediction_runs pr ON pr.id = page.run_id",
			"JOIN stations s ON s.id = page.station_id",
		},
		"decisions": {
			"SUM(CASE WHEN d.regret <= 0.01 THEN 1 ELSE 0 END)",
			"GROUP BY d.recommendation",
		},
	}

	specs := map[string]accuracyQuerySpec{}
	for _, spec := range pageSpecs(doctorFilters{Fuel: "diesel", Confidence: "all",
		From: "2026-07-01T00:00:00Z", To: "2026-07-31T23:59:59Z"}, true) {
		specs[spec.name] = spec
	}

	for name, wanted := range signatures {
		spec, ok := specs[name]
		if !ok {
			t.Fatalf("doctor no longer builds a %q query", name)
		}
		for _, sig := range wanted {
			if !strings.Contains(spec.sql, sig) {
				t.Errorf("doctor's %s query lost %q:\n%s", name, sig, spec.sql)
			}
			if !strings.Contains(php, sig) {
				t.Errorf("web/index.php no longer contains %q, so doctor's %s query has drifted from the page", sig, name)
			}
		}
	}

	// The shared filter is the reason the index exists; both sides must apply
	// the same one.
	for _, sig := range []string{
		"pp.actual_price IS NOT NULL AND pp.fuel = ",
		"pp.confidence IN ('medium', 'high')",
	} {
		if !strings.Contains(php, sig) {
			t.Errorf("web/index.php no longer contains the shared filter fragment %q", sig)
		}
	}
	mediumHigh := pageSpecs(doctorFilters{Fuel: "diesel", Confidence: "medium_high"}, false)
	if !strings.Contains(mediumHigh[0].sql, "pp.confidence IN ('medium', 'high')") {
		t.Error("doctor ignores --confidence medium_high")
	}
	// A run covers every station currently being fed, so there is no city
	// filter left to apply on either side.
	if strings.Contains(php, "pr.city_name") {
		t.Error("web/index.php still filters the accuracy page by city")
	}
	if strings.Contains(mediumHigh[0].sql, "pr.city_name") {
		t.Errorf("doctor still filters by city:\n%s", mediumHigh[0].sql)
	}
}

func TestFormatCountAndBytes(t *testing.T) {
	countCases := map[int64]string{0: "0", 1: "1", 999: "999", 1000: "1,000",
		9720000: "9,720,000", 1234567890: "1,234,567,890"}
	for in, want := range countCases {
		if got := formatCount(in); got != want {
			t.Errorf("formatCount(%d) = %q, want %q", in, got, want)
		}
	}
	byteCases := map[int64]string{512: "512 B", 2048: "2.0 KB",
		5 * 1024 * 1024: "5.0 MB", 3 * 1024 * 1024 * 1024: "3.0 GB"}
	for in, want := range byteCases {
		if got := formatBytes(in); got != want {
			t.Errorf("formatBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

// legacyAccuracySQL is the pre-optimization form of the two queries this
// branch rewrote, kept verbatim so the rewrites can be proven equivalent
// rather than argued to be. The summary_latest outer query had no WHERE, and
// the rows query joined the metadata tables before filtering, ordering and
// capping.
func legacyAccuracySQL(f doctorFilters) map[string]accuracyQuerySpec {
	joinRuns := ""
	where := "pp.actual_price IS NOT NULL AND pp.fuel = ? AND pp.target_start >= ? AND pp.target_start <= ?"
	args := []any{f.Fuel, f.From, f.To}
	if f.Confidence == "medium_high" {
		where += " AND pp.confidence IN ('medium', 'high')"
	}
	return map[string]accuracyQuerySpec{
		"summary_latest": {
			sql: "SELECT COUNT(*) AS n, AVG(ABS(pp.error)) AS mae, AVG(pp.error) AS bias, " +
				"SUM(CASE WHEN ABS(pp.error) <= 0.02 THEN 1 ELSE 0 END) AS within2 " +
				"FROM price_predictions pp JOIN (" +
				"SELECT pp.station_id AS station_id, pp.target_start AS target_start, MAX(pp.run_id) AS run_id " +
				"FROM price_predictions pp " + joinRuns + "WHERE " + where +
				" GROUP BY pp.station_id, pp.target_start" +
				") latest ON latest.station_id = pp.station_id" +
				" AND latest.target_start = pp.target_start" +
				" AND latest.run_id = pp.run_id",
			args: args,
		},
		"rows": {
			sql: "SELECT pp.station_id, pp.fuel, pr.run_at, pp.target_start, pp.target_end, " +
				"pp.predicted_price, pp.actual_price, pp.error, pp.confidence, pp.lead_minutes, pp.is_suggestion, " +
				"COALESCE(s.name_override, s.name) AS name, s.brand, s.street, s.house_number, s.post_code, s.place " +
				"FROM price_predictions pp " +
				"JOIN prediction_runs pr ON pr.id = pp.run_id " +
				"JOIN stations s ON s.id = pp.station_id " +
				"WHERE " + where +
				" ORDER BY pp.target_start DESC, pp.station_id ASC LIMIT 1001",
			args: args,
		},
	}
}

// queryRowStrings returns every row as one comparable string, sorted. Sorting
// makes the comparison a multiset test: rows tied on (target_start,
// station_id) — and every window has several, one per hourly run — have no
// defined order in either form of the query, so their sequence is not part of
// what the rewrite must preserve. Row content and multiplicity are.
func queryRowStrings(t *testing.T, db *sql.DB, spec accuracyQuerySpec) []string {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), spec.sql, spec.args...)
	if err != nil {
		t.Fatalf("query: %v\nsql=%s", err, spec.sql)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatalf("columns: %v", err)
	}
	var out []string
	for rows.Next() {
		cells := make([]sql.NullString, len(columns))
		targets := make([]any, len(columns))
		for i := range cells {
			targets[i] = &cells[i]
		}
		if err := rows.Scan(targets...); err != nil {
			t.Fatalf("scan: %v", err)
		}
		var parts []string
		for i, c := range cells {
			parts = append(parts, columns[i]+"="+c.String)
		}
		out = append(out, strings.Join(parts, "|"))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	sort.Strings(out)
	return out
}

// seedEquivalenceDB builds the cases that could break the rewrites: two fuels
// (so the outer fuel predicate has something to wrongly exclude), two cities
// (so the city filter is exercised), several runs per target window (so
// "latest run" is a real choice), all three confidences, and unevaluated rows
// that the filter must keep excluding.
func seedEquivalenceDB(t *testing.T) *sql.DB {
	t.Helper()
	ctx := context.Background()
	db, err := openDB(filepath.Join(t.TempDir(), "equiv.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := initSchema(ctx, db, dialectSQLite); err != nil {
		t.Fatalf("initSchema: %v", err)
	}
	stations := []string{"st-a", "st-b", "st-c"}
	for i, id := range stations {
		if _, err := db.ExecContext(ctx, `INSERT INTO stations
			(id, name, name_override, brand, street, house_number, post_code, place, lat, lng, first_seen_at, last_seen_at)
			VALUES (?, ?, ?, 'Brand', 'Street', '1', 12345, 'Town', ?, 13.0, '', '')`,
			id, "Name "+id, map[bool]any{true: "Override " + id, false: nil}[i == 1],
			52.0+float64(i)/100); err != nil {
			t.Fatalf("insert station: %v", err)
		}
	}
	base := time.Date(2026, 7, 10, 6, 0, 0, 0, time.UTC)
	confs := []string{"low", "medium", "high"}
	n := 0
	for _, city := range []string{"Berlin", "Hamburg"} {
		for _, fuel := range []string{"diesel", "e5"} {
			for run := 0; run < 3; run++ {
				runAt := base.Add(time.Duration(run) * time.Hour)
				res, err := db.ExecContext(ctx, `INSERT INTO prediction_runs
					(run_at, city_name, fuel, range_km, history_days, predict_days)
					VALUES (?, ?, ?, 5, 30, 3)`, runAt.Format(time.RFC3339), city, fuel)
				if err != nil {
					t.Fatalf("insert run: %v", err)
				}
				runID, _ := res.LastInsertId()
				for si, station := range stations {
					for lead := 1; lead <= 4; lead++ {
						// Overlapping grids: successive runs re-predict the
						// same absolute target hours, which is what makes the
						// latest-run dedup meaningful.
						target := base.Add(time.Duration(lead) * time.Hour)
						predicted := 1.70 + float64(lead)/100 + float64(run)/1000
						n++
						// Leave every seventh row unevaluated.
						if n%7 == 0 {
							if _, err := db.ExecContext(ctx, `INSERT INTO price_predictions
								(run_id, station_id, fuel, target_start, target_end, predicted_price, confidence,
								 sample_count, is_suggestion, lead_minutes, applied_correction, actual_price, error, evaluated_at)
								VALUES (?, ?, ?, ?, ?, ?, ?, 20, 0, ?, 0.0, NULL, NULL, NULL)`,
								runID, station, fuel, target.Format(time.RFC3339),
								target.Add(time.Hour).Format(time.RFC3339), predicted,
								confs[(si+lead)%len(confs)], lead*60); err != nil {
								t.Fatalf("insert unevaluated: %v", err)
							}
							continue
						}
						actual := predicted + float64((n%5)-2)/100
						if _, err := db.ExecContext(ctx, `INSERT INTO price_predictions
							(run_id, station_id, fuel, target_start, target_end, predicted_price, confidence,
							 sample_count, is_suggestion, lead_minutes, applied_correction, actual_price, error, evaluated_at)
							VALUES (?, ?, ?, ?, ?, ?, ?, 20, ?, ?, 0.0, ?, ?, ?)`,
							runID, station, fuel, target.Format(time.RFC3339),
							target.Add(time.Hour).Format(time.RFC3339), predicted,
							confs[(si+lead)%len(confs)], n%2, lead*60, actual, actual-predicted,
							target.Add(2*time.Hour).Format(time.RFC3339)); err != nil {
							t.Fatalf("insert prediction: %v", err)
						}
					}
				}
			}
		}
	}
	return db
}

// TestAccuracyQueryRewritesPreserveResults is the safety net for the two
// queries this branch rewrote for speed. Both rewrites are supposed to be pure
// optimizations — the outer fuel predicate is implied by the join keys, and
// filtering before the metadata joins cannot change which rows survive — so
// every filter combination must return exactly what the old SQL returned.
func TestAccuracyQueryRewritesPreserveResults(t *testing.T) {
	db := seedEquivalenceDB(t)
	defer db.Close()

	filterCases := []doctorFilters{
		{Fuel: "diesel", Confidence: "all", From: "2026-07-01T00:00:00Z", To: "2026-07-31T23:59:59Z"},
		{Fuel: "e5", Confidence: "all", From: "2026-07-01T00:00:00Z", To: "2026-07-31T23:59:59Z"},
		{Fuel: "diesel", Confidence: "medium_high", From: "2026-07-01T00:00:00Z", To: "2026-07-31T23:59:59Z"},
		{Fuel: "diesel", Confidence: "all", From: "2026-07-01T00:00:00Z", To: "2026-07-31T23:59:59Z"},
		{Fuel: "diesel", Confidence: "medium_high", From: "2026-07-01T00:00:00Z", To: "2026-07-31T23:59:59Z"},
		// A window that selects a subset, so the range bound is exercised too.
		{Fuel: "diesel", Confidence: "all", From: "2026-07-10T00:00:00Z", To: "2026-07-10T08:59:59Z"},
		// A window that selects nothing at all.
		{Fuel: "diesel", Confidence: "all", From: "2026-01-01T00:00:00Z", To: "2026-01-02T23:59:59Z"},
	}

	for _, filters := range filterCases {
		label := filters.Fuel + "/" + filters.Confidence + "/" + filters.From
		t.Run(label, func(t *testing.T) {
			legacy := legacyAccuracySQL(filters)
			current := map[string]accuracyQuerySpec{}
			for _, spec := range pageSpecs(filters, true) {
				current[spec.name] = spec
			}

			for _, name := range []string{"summary_latest", "rows"} {
				before := queryRowStrings(t, db, legacy[name])
				after := queryRowStrings(t, db, current[name])
				if len(before) != len(after) {
					t.Fatalf("%s returns %d rows, was %d", name, len(after), len(before))
				}
				for i := range before {
					if before[i] != after[i] {
						t.Fatalf("%s row %d changed:\n old: %s\n new: %s", name, i, before[i], after[i])
					}
				}
			}
		})
	}
}

// TestAccuracyRowsRewriteRespectsTheCap checks the part of the rows rewrite
// that moved: the LIMIT now applies inside the derived table, so it must still
// cap the result and still keep the newest targets rather than an arbitrary
// slice.
func TestAccuracyRowsRewriteRespectsTheCap(t *testing.T) {
	ctx := context.Background()
	db, err := openDB(filepath.Join(t.TempDir(), "cap.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	if err := initSchema(ctx, db, dialectSQLite); err != nil {
		t.Fatalf("initSchema: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO stations
		(id, name, lat, lng, first_seen_at, last_seen_at) VALUES ('st', 'Station', 52.0, 13.0, '', '')`); err != nil {
		t.Fatalf("insert station: %v", err)
	}
	res, err := db.ExecContext(ctx, `INSERT INTO prediction_runs
		(run_at, city_name, fuel, range_km, history_days, predict_days)
		VALUES ('2026-07-01T00:00:00Z', 'Berlin', 'diesel', 5, 30, 3)`)
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	runID, _ := res.LastInsertId()
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	const total = 1200
	for i := 0; i < total; i++ {
		target := base.Add(time.Duration(i) * time.Hour)
		if _, err := db.ExecContext(ctx, `INSERT INTO price_predictions
			(run_id, station_id, fuel, target_start, target_end, predicted_price, confidence,
			 sample_count, is_suggestion, lead_minutes, applied_correction, actual_price, error, evaluated_at)
			VALUES (?, 'st', 'diesel', ?, ?, 1.7, 'medium', 20, 0, 60, 0.0, 1.71, 0.01, ?)`,
			runID, target.Format(time.RFC3339), target.Add(time.Hour).Format(time.RFC3339),
			target.Format(time.RFC3339)); err != nil {
			t.Fatalf("insert prediction: %v", err)
		}
	}

	filters := doctorFilters{Fuel: "diesel", Confidence: "all",
		From: "2026-01-01T00:00:00Z", To: "2027-01-01T23:59:59Z"}
	var rowsSpec accuracyQuerySpec
	for _, spec := range pageSpecs(filters, false) {
		if spec.name == "rows" {
			rowsSpec = spec
		}
	}
	got := queryRowStrings(t, db, rowsSpec)
	if len(got) != 1001 {
		t.Fatalf("rows returned %d, want the 1001 cap", len(got))
	}
	// The cap must keep the newest targets: the oldest included target is
	// 1001 hours back from the newest, not from the start of the data.
	newest := base.Add(time.Duration(total-1) * time.Hour)
	oldestKept := newest.Add(-1000 * time.Hour).Format(time.RFC3339)
	var found bool
	for _, row := range got {
		if strings.Contains(row, "target_start="+oldestKept) {
			found = true
		}
		if strings.Contains(row, "target_start="+base.Format(time.RFC3339)) {
			t.Fatal("cap kept the oldest target; it must keep the newest")
		}
	}
	if !found {
		t.Fatalf("expected the 1001st-newest target %s to be included", oldestKept)
	}
}

func TestIndexHintSyntax(t *testing.T) {
	if got := indexHintSyntax(dialectMySQL, "idx_x"); got != "FORCE INDEX (idx_x)" {
		t.Errorf("mysql hint = %q", got)
	}
	if got := indexHintSyntax(dialectSQLite, "idx_x"); got != "INDEXED BY idx_x" {
		t.Errorf("sqlite hint = %q", got)
	}
	if got := indexHintSyntax(dialectMySQL, ""); got != "" {
		t.Errorf("empty index must produce no hint, got %q", got)
	}
}

// TestPageHintsExactlyTheMeasuredQueries pins the hint policy. Hinting is a
// last resort justified by measurement, so the set of queries carrying one is
// part of the decision, not an implementation detail: series and rows measured
// +1% and -2% on the live database and must stay unhinted.
func TestPageHintsExactlyTheMeasuredQueries(t *testing.T) {
	filters := doctorFilters{Fuel: "diesel", Confidence: "all",
		From: "2026-07-01T00:00:00Z", To: "2026-07-31T23:59:59Z"}
	hint := indexHintSyntax(dialectSQLite, "idx_price_predictions_accuracy")

	wantHinted := map[string]bool{
		"summary": true, "summary_latest": true,
		"by_confidence": true, "by_lead": true, "by_hour": true,
	}
	seen := map[string]bool{}
	for _, spec := range pageSpecs(filters, true) {
		hinted := strings.Contains(spec.sql, hint)
		seen[spec.name] = true
		if wantHinted[spec.name] != hinted {
			t.Errorf("%s hinted=%v, want %v:\n%s", spec.name, hinted, wantHinted[spec.name], spec.sql)
		}
		// A hint on only some references would leave the query running a plan
		// nobody measured; summary_latest reads price_predictions twice.
		if hinted {
			refs := strings.Count(spec.sql, "price_predictions pp")
			if got := strings.Count(spec.sql, hint); got != refs {
				t.Errorf("%s hints %d of %d references:\n%s", spec.name, got, refs, spec.sql)
			}
		}
	}
	for name := range wantHinted {
		if !seen[name] {
			t.Errorf("expected a %q query", name)
		}
	}
}

// TestUnhintedWithoutTheIndex covers the un-migrated database: the page checks
// the index exists before hinting, because forcing an unknown key is an error
// rather than a slow query.
func TestUnhintedWithoutTheIndex(t *testing.T) {
	filters := doctorFilters{Fuel: "diesel", Confidence: "all",
		From: "2026-07-01T00:00:00Z", To: "2026-07-31T23:59:59Z"}
	for _, spec := range unhintedSpecs(filters, true) {
		if strings.Contains(spec.sql, "INDEXED BY") || strings.Contains(spec.sql, "FORCE INDEX") {
			t.Errorf("%s hints an index that does not exist:\n%s", spec.name, spec.sql)
		}
	}
}

// TestForceIndexReplacesThePageHint keeps --try-index honest: it must measure
// the forced index alone, not stack a second hint onto the page's.
func TestForceIndexReplacesThePageHint(t *testing.T) {
	filters := doctorFilters{Fuel: "diesel", Confidence: "all",
		From: "2026-07-01T00:00:00Z", To: "2026-07-31T23:59:59Z"}
	pageHint := indexHintSyntax(dialectSQLite, "idx_price_predictions_accuracy")
	otherHint := indexHintSyntax(dialectSQLite, "idx_price_predictions_station_fuel_target")

	for _, spec := range forcedSpecs(filters, true, "idx_price_predictions_station_fuel_target") {
		if spec.table != "price_predictions" {
			if strings.Contains(spec.sql, otherHint) {
				t.Errorf("%s reads %s and must not be hinted", spec.name, spec.table)
			}
			continue
		}
		if strings.Contains(spec.sql, pageHint) {
			t.Errorf("%s stacked the page hint with the forced one:\n%s", spec.name, spec.sql)
		}
		refs := strings.Count(spec.sql, "price_predictions pp")
		if got := strings.Count(spec.sql, otherHint); got != refs {
			t.Errorf("%s forces the index on %d of %d references:\n%s", spec.name, got, refs, spec.sql)
		}
	}
}

// TestHintedQueriesReturnIdenticalResults is the property the whole hint
// decision rests on: forcing an index changes the plan, never the answer.
func TestHintedQueriesReturnIdenticalResults(t *testing.T) {
	db := seedEquivalenceDB(t)
	defer db.Close()
	if _, err := db.ExecContext(context.Background(), `ANALYZE`); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	filterCases := []doctorFilters{
		{Fuel: "diesel", Confidence: "all", From: "2026-07-01T00:00:00Z", To: "2026-07-31T23:59:59Z"},
		{Fuel: "diesel", Confidence: "medium_high", From: "2026-07-01T00:00:00Z", To: "2026-07-31T23:59:59Z"},
		{Fuel: "e5", Confidence: "all", From: "2026-07-10T00:00:00Z", To: "2026-07-10T08:59:59Z"},
	}
	for _, filters := range filterCases {
		t.Run(filters.Fuel+"/"+filters.Confidence, func(t *testing.T) {
			plain := map[string]accuracyQuerySpec{}
			for _, spec := range unhintedSpecs(filters, true) {
				plain[spec.name] = spec
			}
			// Both what the page now runs, and what --try-index forces.
			variants := append(pageSpecs(filters, true),
				forcedSpecs(filters, true, "idx_price_predictions_accuracy")...)
			for _, spec := range variants {
				if spec.table != "price_predictions" {
					continue
				}
				before := queryRowStrings(t, db, plain[spec.name])
				after := queryRowStrings(t, db, spec)
				if len(before) != len(after) {
					t.Fatalf("hinted %s returns %d rows, unhinted %d", spec.name, len(after), len(before))
				}
				for i := range before {
					if before[i] != after[i] {
						t.Fatalf("hinted %s row %d differs:\n plain: %s\n hint:  %s",
							spec.name, i, before[i], after[i])
					}
				}
			}
		})
	}
}

// TestViewerHintMatchesDoctor cross-checks the hint across the two
// implementations: the page builds it in PHP, doctor in Go, and a mismatch
// would make doctor measure a plan the page never runs.
func TestViewerHintMatchesDoctor(t *testing.T) {
	viewer, err := os.ReadFile(filepath.Join("web", "index.php"))
	if err != nil {
		t.Fatalf("read viewer: %v", err)
	}
	php := string(viewer)

	for _, want := range []string{
		indexHintSyntax(dialectMySQL, "idx_price_predictions_accuracy"),
		indexHintSyntax(dialectSQLite, "idx_price_predictions_accuracy"),
	} {
		if !strings.Contains(php, want) {
			t.Errorf("web/index.php does not emit the hint %q that doctor mirrors", want)
		}
	}
	// The page must guard the hint on the index existing.
	if !strings.Contains(php, "gasolineIndexExists") {
		t.Error("web/index.php hints without checking the index exists")
	}
	// And it must apply it to exactly the queries doctor thinks it does. Six
	// SQL references, because summary_latest reads price_predictions twice; the
	// two unhinted queries keep naming the table directly.
	if got := strings.Count(php, "$ppHinted ."); got != 6 {
		t.Errorf("web/index.php splices $ppHinted into %d queries, want 6", got)
	}
	if got := strings.Count(php, "'FROM price_predictions pp ' . $joinRuns"); got != 2 {
		t.Errorf("web/index.php has %d unhinted accuracy queries, want 2 (series and rows)", got)
	}
}

func TestRunDoctorTryIndexReportsBothTimings(t *testing.T) {
	dbPath, db := seedDoctorDB(t)
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	output := captureStdout(t, func() error {
		return run([]string{"doctor", "--db", dbPath, "--from", "2026-07-01", "--to", "2026-07-31",
			"--try-index", "idx_price_predictions_accuracy", "--output", "json"})
	})
	var result doctorResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("unmarshal: %v\noutput=%s", err, output)
	}

	var hintedCount int
	for _, q := range result.Queries {
		if q.Table != "price_predictions" {
			if q.Hinted != nil {
				t.Errorf("query %s reads %s and must not be hinted", q.Name, q.Table)
			}
			continue
		}
		if q.Hinted == nil {
			t.Fatalf("query %s has no hinted comparison", q.Name)
		}
		hintedCount++
		if q.Hinted.Error != "" {
			t.Fatalf("hinted %s failed: %s", q.Name, q.Hinted.Error)
		}
		if q.Hinted.Index != "idx_price_predictions_accuracy" {
			t.Errorf("hinted %s records index %q", q.Name, q.Hinted.Index)
		}
		// The property that makes the comparison trustworthy.
		if q.Hinted.Rows != q.Rows {
			t.Errorf("hinted %s returned %d rows, unhinted %d", q.Name, q.Hinted.Rows, q.Rows)
		}
	}
	if hintedCount == 0 {
		t.Fatal("no query was compared")
	}
	var verdict bool
	for _, f := range result.Findings {
		if strings.Contains(f.Message, "idx_price_predictions_accuracy") &&
			(strings.Contains(f.Message, "would cut") || strings.Contains(f.Message, "little difference") ||
				strings.Contains(f.Message, "is slower")) {
			verdict = true
		}
	}
	if !verdict {
		t.Fatalf("--try-index produced no verdict; findings=%v", result.Findings)
	}

	// Without the flag there must be no comparison at all.
	plainOutput := captureStdout(t, func() error {
		return run([]string{"doctor", "--db", dbPath, "--from", "2026-07-01", "--to", "2026-07-31", "--output", "json"})
	})
	var plainResult doctorResult
	if err := json.Unmarshal([]byte(plainOutput), &plainResult); err != nil {
		t.Fatalf("unmarshal plain: %v", err)
	}
	for _, q := range plainResult.Queries {
		if q.Hinted != nil {
			t.Fatalf("query %s was hinted without --try-index", q.Name)
		}
	}
}

// TestRunDoctorTryIndexSurvivesUnusableIndex keeps a bad --try-index value from
// taking the whole report down: the per-query error is the answer.
func TestRunDoctorTryIndexSurvivesUnusableIndex(t *testing.T) {
	dbPath, db := seedDoctorDB(t)
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	output := captureStdout(t, func() error {
		return run([]string{"doctor", "--db", dbPath, "--from", "2026-07-01", "--to", "2026-07-31",
			"--try-index", "idx_does_not_exist", "--output", "json"})
	})
	var result doctorResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("unmarshal: %v\noutput=%s", err, output)
	}
	var reported bool
	for _, q := range result.Queries {
		if q.Table != "price_predictions" {
			continue
		}
		// The unhinted measurement must still have succeeded.
		if q.Error != "" {
			t.Fatalf("query %s broke because of a bad --try-index: %s", q.Name, q.Error)
		}
		if q.Hinted != nil && q.Hinted.Error != "" {
			reported = true
		}
	}
	if !reported {
		t.Fatal("a nonexistent --try-index must be reported as a per-query error")
	}
}

// TestRunDoctorQueriesSurviveMissingAccuracyIndex is the safety case for
// hinting the page's queries: on a database that has not run `gasoline
// migrate`, the index named in the hint does not exist, and forcing an unknown
// key is a hard error rather than a slow query. The page and doctor both have
// to notice and fall back to unhinted SQL.
func TestRunDoctorQueriesSurviveMissingAccuracyIndex(t *testing.T) {
	ctx := context.Background()
	dbPath, db := seedDoctorDB(t)
	if _, err := db.ExecContext(ctx, `DROP INDEX idx_price_predictions_accuracy`); err != nil {
		t.Fatalf("drop index: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	output := captureStdout(t, func() error {
		return run([]string{"doctor", "--db", dbPath, "--from", "2026-07-01", "--to", "2026-07-31", "--output", "json"})
	})
	var result doctorResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("unmarshal: %v\noutput=%s", err, output)
	}
	if len(result.Queries) == 0 {
		t.Fatal("no queries ran")
	}
	for _, q := range result.Queries {
		if q.Error != "" {
			t.Fatalf("query %s failed without the index: %s", q.Name, q.Error)
		}
		if strings.Contains(q.SQL, "INDEXED BY") || strings.Contains(q.SQL, "FORCE INDEX") {
			t.Fatalf("query %s hints an index that does not exist:\n%s", q.Name, q.SQL)
		}
		if q.Rows == 0 {
			t.Fatalf("query %s returned nothing against seeded data", q.Name)
		}
	}
}

// seedDoctorScopeDB builds the situation an operator actually reports: two
// configured targets, one of which is being fed; a city whose target was
// removed days ago and which now only has stored predictions left; and a city
// that is not a target but is still receiving price updates, which is the one
// case that means suggest and check still cover it.
func seedDoctorScopeDB(t *testing.T, now time.Time) (string, *sql.DB) {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "scope.db")
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := initSchema(ctx, db, dialectSQLite); err != nil {
		t.Fatalf("initSchema: %v", err)
	}

	cities := []struct {
		name string
		lat  float64
		lng  float64
	}{
		{"lübbecke", 52.302721, 8.618305},
		{"uchte", 52.499750, 8.909280},
		{"mönsheim", 48.849000, 8.882000},
		{"hameln", 52.103500, 9.356600},
	}
	for _, city := range cities {
		if _, err := db.ExecContext(ctx, `INSERT INTO cities
			(name, normalized_name, display_name, lat, lng, created_at)
			VALUES (?, ?, ?, ?, ?, ?)`,
			city.name, city.name, city.name, city.lat, city.lng, now.Format(time.RFC3339)); err != nil {
			t.Fatalf("insert city: %v", err)
		}
	}
	for _, target := range []string{"lübbecke", "uchte"} {
		if _, err := db.ExecContext(ctx, `INSERT INTO update_targets (city, radius_km, created_at) VALUES (?, 25, ?)`,
			target, now.Format(time.RFC3339)); err != nil {
			t.Fatalf("insert target: %v", err)
		}
	}

	// stations: owner city, and how long ago it was last fed.
	stations := []struct {
		id    string
		city  string
		aged  time.Duration
		runID bool
	}{
		{id: "fed-1", city: "lübbecke", aged: time.Hour, runID: true},
		{id: "fed-2", city: "lübbecke", aged: time.Hour, runID: true},
		{id: "stale-1", city: "mönsheim", aged: 72 * time.Hour},
		{id: "stale-2", city: "mönsheim", aged: 96 * time.Hour},
		{id: "leak-1", city: "hameln", aged: 2 * time.Hour, runID: true},
	}
	var runID int64
	res, err := db.ExecContext(ctx, `INSERT INTO prediction_runs
		(run_at, city_name, fuel, range_km, history_days, predict_days, station_count)
		VALUES (?, '', 'diesel', 0, 30, 3, 3)`, now.Add(-30*time.Minute).Format(time.RFC3339))
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	if runID, err = res.LastInsertId(); err != nil {
		t.Fatalf("run id: %v", err)
	}
	// A stale city's last run before it dropped out, so its predictions have a
	// run to belong to without being part of the newest one.
	staleRes, err := db.ExecContext(ctx, `INSERT INTO prediction_runs
		(run_at, city_name, fuel, range_km, history_days, predict_days, station_count)
		VALUES (?, '', 'diesel', 0, 30, 3, 2)`, now.Add(-72*time.Hour).Format(time.RFC3339))
	if err != nil {
		t.Fatalf("insert stale run: %v", err)
	}
	staleRunID, err := staleRes.LastInsertId()
	if err != nil {
		t.Fatalf("stale run id: %v", err)
	}

	for _, station := range stations {
		recordedAt := now.Add(-station.aged)
		if _, err := db.ExecContext(ctx, `INSERT INTO stations
			(id, name, brand, street, house_number, post_code, place, lat, lng, first_seen_at, last_seen_at)
			VALUES (?, ?, 'Brand', 'Street', '1', 12345, ?, 52.0, 8.0, ?, ?)`,
			station.id, "Station "+station.id, station.city,
			recordedAt.Format(time.RFC3339), recordedAt.Format(time.RFC3339)); err != nil {
			t.Fatalf("insert station: %v", err)
		}
		// Two snapshots each, so the newest one is what decides ownership.
		for _, offset := range []time.Duration{2 * time.Hour, 0} {
			if _, err := db.ExecContext(ctx, `INSERT INTO price_snapshots
				(station_id, city_name, recorded_at, search_radius_km, is_open, e5, e10, diesel)
				VALUES (?, ?, ?, 25, 1, 1.80, 1.75, 1.70)`,
				station.id, station.city, recordedAt.Add(-offset).Format(time.RFC3339)); err != nil {
				t.Fatalf("insert snapshot: %v", err)
			}
		}
		run := runID
		if !station.runID {
			run = staleRunID
		}
		target := now.Add(time.Hour)
		if !station.runID {
			target = now.Add(-70 * time.Hour)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO price_predictions
			(run_id, station_id, fuel, target_start, target_end, predicted_price, confidence,
			 sample_count, is_suggestion, lead_minutes)
			VALUES (?, ?, 'diesel', ?, ?, 1.70, 'medium', 20, 1, 60)`,
			run, station.id, target.Format(time.RFC3339), target.Add(time.Hour).Format(time.RFC3339)); err != nil {
			t.Fatalf("insert prediction: %v", err)
		}
	}
	return dbPath, db
}

func doctorScopeFor(t *testing.T, dbPath string) doctorScope {
	t.Helper()
	output := captureStdout(t, func() error {
		return run([]string{"doctor", "--db", dbPath, "--skip-queries", "--output", "json"})
	})
	var result doctorResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("unmarshal doctor output: %v\noutput=%s", err, output)
	}
	return result.Scope
}

func scopeCity(t *testing.T, s doctorScope, name string) doctorScopeCity {
	t.Helper()
	for _, city := range s.Cities {
		if city.City == name {
			return city
		}
	}
	t.Fatalf("city %q missing from scope report; got %+v", name, s.Cities)
	return doctorScopeCity{}
}

// TestDoctorScopeTellsStaleHistoryFromLiveScope is the whole point of the scope
// check: "a city I removed still shows up" has two causes, and they need
// opposite responses.
func TestDoctorScopeTellsStaleHistoryFromLiveScope(t *testing.T) {
	now := time.Now().UTC()
	dbPath, db := seedDoctorScopeDB(t, now)
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	scope := doctorScopeFor(t, dbPath)
	if scope.Skipped {
		t.Fatalf("scope check skipped: %s", scope.Reason)
	}
	if !scope.Configured {
		t.Fatal("targets are configured, but the report says otherwise")
	}
	if scope.FreshnessHours != stationFreshness.Hours() {
		t.Fatalf("freshness = %v, want %v", scope.FreshnessHours, stationFreshness.Hours())
	}

	fed := scopeCity(t, scope, "lübbecke")
	if fed.Stations != 2 || fed.InScope != 2 || fed.InLatestRun != 2 || !fed.Target {
		t.Fatalf("fed target city = %+v, want 2 stations, all in scope and in the latest run", fed)
	}
	if fed.NewestPrediction != "" {
		t.Fatalf("a fed city needs no stored-prediction lookup, got %q", fed.NewestPrediction)
	}

	// The target that owns nothing still gets a row, so a target the sweep
	// never reaches is visible rather than absent.
	empty := scopeCity(t, scope, "uchte")
	if !empty.Target || empty.Stations != 0 {
		t.Fatalf("target owning no station = %+v, want a target row with no stations", empty)
	}

	stale := scopeCity(t, scope, "mönsheim")
	if stale.Target {
		t.Fatal("mönsheim is not an update target")
	}
	if stale.Stations != 2 || stale.InScope != 0 || stale.InLatestRun != 0 {
		t.Fatalf("stale city = %+v, want 2 stations, none in scope, none in the latest run", stale)
	}
	if stale.NewestPrediction == "" {
		t.Fatal("a stale city's stored predictions are what keeps it on the accuracy page; none reported")
	}

	leak := scopeCity(t, scope, "hameln")
	if leak.Target || leak.InScope != 1 {
		t.Fatalf("leaking city = %+v, want a non-target city with a station in scope", leak)
	}

	if scope.LatestRun == nil {
		t.Fatal("no latest run reported")
	}
	if scope.LatestRun.Stations != 3 {
		t.Fatalf("latest run stations = %d, want 3", scope.LatestRun.Stations)
	}
	if scope.LatestRun.OutOfScope != 0 {
		t.Fatalf("latest run out-of-scope stations = %d, want 0", scope.LatestRun.OutOfScope)
	}

	findings := doctorScopeFindings(scope)
	var warnedLeak, explainedStale bool
	for _, finding := range findings {
		switch {
		case finding.Severity == "warn" && strings.Contains(finding.Message, "hameln") &&
			strings.Contains(finding.Message, "not an update target"):
			warnedLeak = true
		case finding.Severity == "info" && strings.Contains(finding.Message, "mönsheim") &&
			strings.Contains(finding.Message, "suggest --persist` run drops them"):
			explainedStale = true
		}
	}
	if !warnedLeak {
		t.Fatalf("a non-target city still in scope must be a warning; findings=%+v", findings)
	}
	if !explainedStale {
		t.Fatalf("a dropped city with retained predictions must be explained; findings=%+v", findings)
	}
}

// TestDoctorScopeFlagsATargetThatStoppedBeingFed separates a broken sweep from a
// removed city: the target is still configured, so its stations leaving scope is
// a collection failure, not retention.
func TestDoctorScopeFlagsATargetThatStoppedBeingFed(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	dbPath, db := seedDoctorScopeDB(t, now)
	if _, err := db.ExecContext(ctx,
		`UPDATE price_snapshots SET recorded_at = ? WHERE city_name = 'lübbecke'`,
		now.Add(-96*time.Hour).Format(time.RFC3339)); err != nil {
		t.Fatalf("age snapshots: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	scope := doctorScopeFor(t, dbPath)
	fed := scopeCity(t, scope, "lübbecke")
	if fed.InScope != 0 {
		t.Fatalf("aged target city in scope = %d, want 0", fed.InScope)
	}
	// The run still names those stations, which is the inconsistency to report.
	if scope.LatestRun == nil || scope.LatestRun.OutOfScope != 2 {
		t.Fatalf("latest run = %+v, want 2 out-of-scope stations", scope.LatestRun)
	}

	findings := doctorScopeFindings(scope)
	var warnedTarget, warnedRun bool
	for _, finding := range findings {
		if finding.Severity != "warn" {
			continue
		}
		if strings.Contains(finding.Message, "lübbecke") && strings.Contains(finding.Message, "no price update since") {
			warnedTarget = true
		}
		if strings.Contains(finding.Message, "newest prediction run") {
			warnedRun = true
		}
	}
	if !warnedTarget {
		t.Fatalf("a configured target that stopped being fed must warn; findings=%+v", findings)
	}
	if !warnedRun {
		t.Fatalf("a run covering out-of-scope stations must warn; findings=%+v", findings)
	}
}

// TestDoctorScopeSkipsWithoutTheTables keeps the scope section from failing the
// whole report on a database that predates it.
func TestDoctorScopeSkipsWithoutTheTables(t *testing.T) {
	ctx := context.Background()
	dbPath, db := seedDoctorDB(t)
	if _, err := db.ExecContext(ctx, `DROP TABLE price_snapshots`); err != nil {
		t.Fatalf("drop table: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	scope := doctorScopeFor(t, dbPath)
	if !scope.Skipped || scope.Reason == "" {
		t.Fatalf("scope = %+v, want a skip with a reason", scope)
	}
	findings := doctorScopeFindings(scope)
	if len(findings) != 1 || findings[0].Severity != "info" {
		t.Fatalf("findings = %+v, want a single info line", findings)
	}
}

// TestDoctorScopeTextListsEveryCity covers the text rendering, which is what an
// operator actually reads.
func TestDoctorScopeTextListsEveryCity(t *testing.T) {
	now := time.Now().UTC()
	dbPath, db := seedDoctorScopeDB(t, now)
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	output := captureStdout(t, func() error {
		return run([]string{"doctor", "--db", dbPath, "--skip-queries"})
	})
	for _, want := range []string{"scope:", "lübbecke", "uchte", "mönsheim", "hameln",
		"newest price update", "newest run:", "25.0 km"} {
		if !strings.Contains(output, want) {
			t.Fatalf("doctor text output missing %q\noutput=%s", want, output)
		}
	}
}
