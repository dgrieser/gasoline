package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
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
	wantQueries := []string{"summary", "summary_latest", "breakdowns", "rows", "decisions"}
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

	// A tree plan has none of the traditional format's vocabulary, and it is
	// not a rare path: EXPLAIN ANALYZE always answers with one, and so does a
	// plain EXPLAIN on a server whose explain_format says TREE. Reading a tree
	// with only the tabular keywords is what reported every one of these as
	// "no index" against the production MySQL.
	t.Run("mysql tree plan covering read", func(t *testing.T) {
		plan := []string{
			"-> Group aggregate: max(pr.run_at)",
			"    -> Covering index range scan on pp using idx_price_predictions_accuracy  (cost=1.2 rows=610978)",
		}
		uses, covering, fullScan := classifyPlan(plan, nil, dialectMySQL, indexes, "pp")
		if uses != "idx_price_predictions_accuracy" || !covering {
			t.Fatalf("uses=%q covering=%v, want the accuracy index, covering", uses, covering)
		}
		if fullScan {
			t.Fatal("a covering index scan is not a table scan")
		}
	})

	t.Run("mysql tree plan index read that still fetches rows", func(t *testing.T) {
		plan := []string{
			"-> Nested loop inner join",
			"    -> Index lookup on pp using idx_price_predictions_station_fuel_target (station_id='a', fuel='e5')",
			"    -> Single-row index lookup on pr using PRIMARY (id=pp.run_id)",
		}
		uses, covering, fullScan := classifyPlan(plan, nil, dialectMySQL, indexes, "pp")
		if uses != "idx_price_predictions_station_fuel_target" {
			t.Fatalf("uses = %q, want the station index", uses)
		}
		if covering {
			t.Fatal("an index lookup that is not covering must not be reported as one")
		}
		if fullScan {
			t.Fatal("an index lookup is not a table scan")
		}
	})

	t.Run("mysql tree plan table scan", func(t *testing.T) {
		plan := []string{
			"-> Limit: 1 row(s)",
			"    -> Filter: (cities.normalized_name = 'berlin')",
			"        -> Table scan on cities  (cost=5500 rows=54280)",
		}
		uses, covering, fullScan := classifyPlan(plan, nil, dialectMySQL, []string{"PRIMARY"}, "cities")
		if uses != "" || covering {
			t.Fatalf("uses=%q covering=%v, want neither", uses, covering)
		}
		if !fullScan {
			t.Fatal("a tree plan's table scan must be reported as a table scan")
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
			// target_start first: the index's own order within one fuel, with
			// run_id the column immediately after both keys.
			"GROUP BY pp.target_start, pp.station_id",
			"latest.run_id = pp.run_id",
			// The outer fuel predicate is what makes the covering index usable
			// for the join; losing it silently costs ~18 s on the live
			// database, so both sides must keep it.
			"WHERE pp.fuel = ",
		},
		// One pass for the three breakdown tables and the chart. SUM rather
		// than AVG, because an average cannot be re-aggregated per dimension
		// afterwards; the hour is a substring of target_start and the chart is
		// the sum per target_start, so neither needs a pass of its own.
		"breakdowns": {
			"GROUP BY pp.target_start, pp.confidence, ",
			"WHEN pp.lead_minutes < 360 THEN 2",
			"MIN(pp.lead_minutes)",
			"SUM(ABS(pp.error))",
			"SUM(pp.error)",
			"SUM(pp.predicted_price)",
			"SUM(pp.actual_price)",
		},
		"rows": {
			"COALESCE(s.name_override, s.name)",
			// Both keys descending, so the LIMIT is a backward index read
			// rather than a sort of the whole slice.
			"ORDER BY pp.target_start DESC, pp.station_id DESC",
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
// zero across four runs on the live database and must stay unhinted.
func TestPageHintsExactlyTheMeasuredQueries(t *testing.T) {
	filters := doctorFilters{Fuel: "diesel", Confidence: "all",
		From: "2026-07-01T00:00:00Z", To: "2026-07-31T23:59:59Z"}
	hint := indexHintSyntax(dialectSQLite, "idx_price_predictions_accuracy")

	// rows joined this set once the table passed 8M rows: the optimizer began
	// driving it from idx_price_predictions_due, and forcing the covering index
	// measured 8.3-8.8 s against 6.0-6.5 s over four runs with the ranges never
	// overlapping. series stays out — there the same four runs straddled zero.
	wantHinted := map[string]bool{
		"summary": true, "summary_latest": true, "breakdowns": true, "rows": true,
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
	// And it must apply it to exactly the queries doctor thinks it does. Seven
	// SQL references for six hinted queries, because summary_latest reads
	// price_predictions twice; series is the one query still naming the table
	// directly.
	wantRefs := len(pageHintedQueries) + 1
	if got := strings.Count(php, "$ppHinted ."); got != wantRefs {
		t.Errorf("web/index.php splices $ppHinted into %d references, want %d", got, wantRefs)
	}
	// Every accuracy query is hinted now that the chart comes out of the
	// breakdown pass, so nothing should still name the table directly.
	if got := strings.Count(php, "'FROM price_predictions pp ' . $joinRuns"); got != 0 {
		t.Errorf("web/index.php has %d unhinted accuracy queries, want none", got)
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
	var verdict, band bool
	for _, f := range result.Findings {
		if !strings.Contains(f.Message, "idx_price_predictions_accuracy") {
			continue
		}
		if strings.Contains(f.Message, "cuts") || strings.Contains(f.Message, "not established") ||
			strings.Contains(f.Message, "slower on") || strings.Contains(f.Message, "nothing was actually compared") {
			verdict = true
		}
		// The queries the page already hints re-ran identical SQL, so the report
		// has to say what their repeats measured before any verdict lands.
		if strings.Contains(f.Message, "re-ran identical SQL") {
			band = true
		}
	}
	if !verdict {
		t.Fatalf("--try-index produced no verdict; findings=%v", result.Findings)
	}
	if !band {
		t.Fatalf("--try-index did not report the repeats' noise band; findings=%v", result.Findings)
	}

	// Five of these queries already force the index, so their comparison is a
	// repeat of the same statement and must be marked as one.
	repeats := 0
	for _, q := range result.Queries {
		if q.Hinted != nil && q.Hinted.Repeat {
			repeats++
			if !pageHintedQueries[q.Name] {
				t.Errorf("query %s is not one the page hints, so its comparison is not a repeat", q.Name)
			}
		}
	}
	if repeats != len(pageHintedQueries) {
		t.Errorf("%d comparisons were marked as repeats, want the %d the page already hints",
			repeats, len(pageHintedQueries))
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

// TestRunDoctorOptimizeReclaimsSpace is the case that prompted --optimize: rows
// were deleted in bulk (a removed update target taking its predictions with it)
// and the database kept the freed pages, so every size doctor reports stayed
// where it was.
func TestRunDoctorOptimizeReclaimsSpace(t *testing.T) {
	ctx := context.Background()
	dbPath, db := seedDoctorDB(t)
	// Enough rows for the free list to be visible against SQLite's page size.
	for i := 0; i < 20000; i++ {
		if _, err := db.ExecContext(ctx, `INSERT INTO price_predictions
			(run_id, station_id, fuel, target_start, target_end, predicted_price, confidence,
			 sample_count, is_suggestion, lead_minutes, applied_correction)
			VALUES ((SELECT MIN(id) FROM prediction_runs), 'st-1', 'e10', ?, ?, 1.70, 'low', 1, 0, 60, 0)`,
			time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i)*time.Minute).Format(time.RFC3339),
			time.Date(2026, 6, 1, 1, 0, 0, 0, time.UTC).Add(time.Duration(i)*time.Minute).Format(time.RFC3339),
		); err != nil {
			t.Fatalf("insert bulk prediction: %v", err)
		}
	}
	var seeded int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM price_predictions WHERE fuel = 'diesel'`).Scan(&seeded); err != nil {
		t.Fatalf("count seeded: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM price_predictions WHERE fuel = 'e10'`); err != nil {
		t.Fatalf("delete bulk predictions: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	before, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("stat before: %v", err)
	}

	output := captureStdout(t, func() error {
		return run([]string{"doctor", "--db", dbPath, "--skip-queries", "--optimize", "--output", "json"})
	})
	var result doctorResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("unmarshal: %v\noutput=%s", err, output)
	}
	if result.Optimize == nil {
		t.Fatal("--optimize produced no optimize section")
	}
	if !strings.Contains(result.Optimize.Statement, "VACUUM") {
		t.Fatalf("statement = %q, want the SQLite rebuild", result.Optimize.Statement)
	}
	if len(result.Optimize.Tables) != 1 || result.Optimize.Tables[0].Error != "" {
		t.Fatalf("optimize tables = %+v, want one successful entry", result.Optimize.Tables)
	}
	if result.Optimize.FileBytesBefore == nil || result.Optimize.FileBytesAfter == nil {
		t.Fatalf("optimize = %+v, want the file measured on both sides", result.Optimize)
	}
	if *result.Optimize.FileBytesAfter >= *result.Optimize.FileBytesBefore {
		t.Fatalf("file went from %d to %d bytes, want it smaller after the rebuild",
			*result.Optimize.FileBytesBefore, *result.Optimize.FileBytesAfter)
	}

	after, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("stat after: %v", err)
	}
	if after.Size() >= before.Size() {
		t.Fatalf("database file is %d bytes, was %d — nothing was returned", after.Size(), before.Size())
	}

	var reported bool
	for _, finding := range result.Findings {
		if strings.Contains(finding.Message, "optimize returned") {
			reported = true
		}
	}
	if !reported {
		t.Fatalf("findings do not report the reclaimed space: %+v", result.Findings)
	}

	// The rows that were not deleted have to survive a rebuild.
	reopened, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	var kept int
	if err := reopened.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM price_predictions WHERE fuel = 'diesel'`).Scan(&kept); err != nil {
		t.Fatalf("count kept: %v", err)
	}
	if kept != seeded {
		t.Fatalf("diesel predictions = %d after the rebuild, want the seeded %d", kept, seeded)
	}
}

// TestRunDoctorOptimizeIsOptIn keeps the default read-only: the report must not
// even carry an optimize section unless it was asked for.
func TestRunDoctorOptimizeIsOptIn(t *testing.T) {
	dbPath, db := seedDoctorDB(t)
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
	if result.Optimize != nil {
		t.Fatalf("optimize section present without --optimize: %+v", result.Optimize)
	}
	if strings.Contains(output, "VACUUM") {
		t.Error("doctor mentions a rebuild it did not run")
	}
}

// TestRunDoctorOptimizeTableIsMySQLOnly documents the engine difference instead
// of silently pretending one SQLite table was rebuilt.
func TestRunDoctorOptimizeTableIsMySQLOnly(t *testing.T) {
	dbPath, db := seedDoctorDB(t)
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	output := captureStdout(t, func() error {
		return run([]string{"doctor", "--db", dbPath, "--skip-queries",
			"--optimize", "--optimize-table", "price_predictions", "--output", "json"})
	})
	var result doctorResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("unmarshal: %v\noutput=%s", err, output)
	}
	if result.Optimize == nil || !strings.Contains(result.Optimize.Reason, "whole database") {
		t.Fatalf("optimize = %+v, want a note that VACUUM cannot be narrowed", result.Optimize)
	}
}

func TestResolveOptimizeTables(t *testing.T) {
	cases := []struct {
		name     string
		list     string
		optimize bool
		want     []string
		wantErr  string
	}{
		{name: "empty is off", list: "", optimize: false},
		{name: "needs the flag", list: "price_predictions", optimize: false, wantErr: "requires --optimize"},
		{name: "one table", list: "price_predictions", optimize: true, want: []string{"price_predictions"}},
		{
			name: "trimmed and de-duplicated", list: " price_snapshots , price_predictions ,price_snapshots",
			optimize: true, want: []string{"price_snapshots", "price_predictions"},
		},
		{name: "unknown table", list: "users", optimize: true, wantErr: "is not one of"},
		{name: "only separators", list: " , ", optimize: true, wantErr: "at least one table"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveOptimizeTables(tc.list, tc.optimize)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveOptimizeTables: %v", err)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("tables = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFormatDurationMS(t *testing.T) {
	cases := []struct {
		ms   float64
		want string
	}{
		{ms: 0, want: "0 ms"},
		{ms: 12.4, want: "12 ms"},
		{ms: 1500, want: "1.5 s"},
		{ms: 59_999, want: "60.0 s"},
		{ms: 90_000, want: "1.5 min"},
	}
	for _, tc := range cases {
		if got := formatDurationMS(tc.ms); got != tc.want {
			t.Errorf("formatDurationMS(%v) = %q, want %q", tc.ms, got, tc.want)
		}
	}
}

// TestAccuracyQueriesCarryProbes pins that every accuracy-page query has a probe
// and that each probe reads the same rows through the same access path as its
// query — a probe on a different plan would price the wrong thing.
func TestAccuracyQueriesCarryProbes(t *testing.T) {
	filters := doctorFilters{Fuel: "diesel", Confidence: "all",
		From: "2026-07-01T00:00:00Z", To: "2026-07-31T23:59:59Z"}
	specs := map[string]accuracyQuerySpec{}
	for _, spec := range pageSpecs(filters, true) {
		specs[spec.name] = spec
	}
	for name, spec := range specs {
		if spec.probe == nil {
			t.Errorf("query %s has no probe, so nothing can say where its time goes", name)
			continue
		}
		if spec.probeGap == "" {
			t.Errorf("query %s has a probe but does not say what the gap to it is", name)
		}
		if spec.probe.alias != spec.alias {
			t.Errorf("query %s probes alias %q but drives from %q", name, spec.probe.alias, spec.alias)
		}
		// Carrying the same hint is not enough: two of these queries are
		// unhinted, and there only pinning the plan the query actually took
		// keeps the probe comparable.
		if spec.table == "price_predictions" && spec.probe.sqlFor == nil {
			t.Errorf("query %s has a probe that cannot be pinned to its plan", name)
		}
		if spec.probe.sqlFor != nil {
			pinned := spec.probe.sqlFor("idx_price_predictions_due")
			if !strings.Contains(pinned, "idx_price_predictions_due") {
				t.Errorf("query %s probe ignores the index it is pinned to:\n%s", name, pinned)
			}
		}
		// A probe that carried a different hint would take a different plan and
		// its timing would not be comparable.
		if strings.Contains(spec.sql, "idx_price_predictions_accuracy") !=
			strings.Contains(spec.probe.sql, "idx_price_predictions_accuracy") {
			t.Errorf("query %s and its probe disagree about the index hint:\n%s\n%s",
				name, spec.sql, spec.probe.sql)
		}
	}

	// The two structural probes isolate a join rather than a projection, which
	// is the whole reason they are spelled out separately.
	if got := specs["summary_latest"].probe.sql; strings.Contains(got, "JOIN (") {
		t.Errorf("the summary_latest probe must drop the self-join, got:\n%s", got)
	}
	if got := specs["rows"].probe.sql; strings.Contains(got, "JOIN prediction_runs") ||
		strings.Contains(got, "JOIN stations") {
		t.Errorf("the rows probe must drop the metadata joins, got:\n%s", got)
	}
	// ...but must keep the cap, or it would read the whole range instead.
	if !strings.Contains(specs["rows"].probe.sql, "LIMIT 1001") {
		t.Errorf("the rows probe lost the page cap:\n%s", specs["rows"].probe.sql)
	}
}

// TestAccuracyProbesReadTheSameRows runs each query and its probe for real and
// checks the probe is not measuring a smaller slice than the query aggregates
// over, which is what makes the difference between them meaningful.
func TestAccuracyProbesReadTheSameRows(t *testing.T) {
	_, db := seedDoctorDB(t)
	defer db.Close()
	ctx := context.Background()

	filters := doctorFilters{Fuel: "diesel", Confidence: "all",
		From: "2026-07-01T00:00:00Z", To: "2026-07-31T23:59:59Z"}
	for _, spec := range pageSpecs(filters, true) {
		if spec.probe == nil {
			continue
		}
		queryRows, err := countQueryRows(ctx, db, spec.sql, spec.args)
		if err != nil {
			t.Fatalf("%s: %v", spec.name, err)
		}
		probeRows, err := countQueryRows(ctx, db, spec.probe.sql, spec.probe.args)
		if err != nil {
			t.Fatalf("%s probe: %v", spec.name, err)
		}
		if probeRows == 0 {
			t.Errorf("%s probe read nothing while the query returned %d rows", spec.name, queryRows)
		}
		// The aggregates reduce, so the probe reads at least as much as the
		// query returns; rows and its probe both stop at the cap.
		if probeRows < queryRows {
			t.Errorf("%s probe read %d rows, fewer than the query's %d — it is not the same slice",
				spec.name, probeRows, queryRows)
		}
	}
}

// TestAccuracyProbeFindingsDistinguishAggregationFromLookups pins the
// distinction the wording turns on: a covering read's remaining time is the
// aggregation, and calling it row lookups would send an operator after an index
// the query is already using.
func TestAccuracyProbeFindingsDistinguishAggregationFromLookups(t *testing.T) {
	opts := doctorOptions{Pages: doctorPages{Accuracy: true}, Probe: true, SlowMS: 1000}

	covering := doctorQuery{
		Name: "summary", Table: "price_predictions", DurationMS: 3600, Rows: 1,
		UsesIndex: "idx_price_predictions_accuracy", CoveringHit: true,
		Probe: &doctorQueryProbe{Name: "rows walked", DurationMS: 900, Rows: 2_400_000,
			UsesIndex: "idx_price_predictions_accuracy", CoveringHit: true, Comparable: true},
	}
	// A covering read's remaining time is the aggregation, so there is nothing
	// here to attribute to lookups. Its slice is reported once for the page
	// instead; see TestAccuracySliceFindingIsStatedOnce.
	if got := accuracyProbeFindings(covering, "aggregating those rows", opts); len(got) != 0 {
		t.Errorf("a covering read must not be reported as paying for lookups:\n%s", renderFindings(got))
	}

	lookupBound := doctorQuery{
		Name: "rows", Table: "price_predictions", DurationMS: 4100, Rows: 1001,
		UsesIndex: "idx_price_predictions_station_fuel_target",
		probeGap:  "joining prediction_runs and stations onto the capped page",
		Probe: &doctorQueryProbe{Name: "page only", DurationMS: 100, Rows: 1001,
			UsesIndex: "idx_price_predictions_station_fuel_target", Comparable: true},
	}
	got := renderFindings(accuracyProbeFindings(lookupBound, lookupBound.probeGap, opts))
	for _, want := range []string{
		"query rows spends 4000 ms of its 4100 ms joining prediction_runs and stations onto the capped page",
		"about 3996 µs each",
		"That is seek latency rather than a cache hit",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("findings are missing %q:\n%s", want, got)
		}
	}
}

// TestProbeOnADifferentPlanIsRefused pins the guard the production run needed:
// `series` is one of the two queries the page leaves unhinted, its probe's
// narrower projection led the optimizer to a different index, and the probe came
// out four times slower than the query it was meant to be a floor for. A gap
// between two unrelated plans is not a measurement of anything.
func TestProbeOnADifferentPlanIsRefused(t *testing.T) {
	opts := doctorOptions{Pages: doctorPages{Accuracy: true}, Probe: true, SlowMS: 1000}
	series := doctorQuery{
		Name: "series", Table: "price_predictions", DurationMS: 1627, Rows: 575,
		UsesIndex: "idx_price_predictions_accuracy", CoveringHit: true,
		probeGap: "grouping and ordering those rows",
		Probe: &doctorQueryProbe{Name: "rows walked", DurationMS: 6508, Rows: 1_363_186,
			UsesIndex: "idx_price_predictions_due", Comparable: false},
	}
	got := renderFindings(accuracyProbeFindings(series, series.probeGap, opts))
	for _, want := range []string{
		"query series took idx_price_predictions_accuracy while its probe took idx_price_predictions_due",
		"not measuring the same work",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("findings are missing %q:\n%s", want, got)
		}
	}
	// Above all it must not be reported as a cost of the query.
	if strings.Contains(got, "spends") {
		t.Errorf("an incomparable probe must not be attributed to the query:\n%s", got)
	}

	// The line itself has to carry the caveat, or its timing reads as a floor.
	line := captureStdout(t, func() error {
		writeDoctorProbeText(series.Probe, doctorOptions{}, false)
		return nil
	})
	if !strings.Contains(line, "different plan, not comparable") {
		t.Errorf("the probe line must say it is not comparable:\n%s", line)
	}

	// Nor may its rows or its time reach the page-level slice total.
	comparable := doctorQuery{Name: "summary", Table: "price_predictions", CoveringHit: true,
		Probe: &doctorQueryProbe{Name: "rows walked", Rows: 1_363_186, DurationMS: 1686, Comparable: true}}
	finding, ok := accuracySliceFinding([]doctorQuery{comparable, series})
	if !ok {
		t.Fatal("the comparable probe still reports its slice")
	}
	if strings.Contains(finding.Message, "series") || strings.Contains(finding.Message, "2 of") {
		t.Errorf("an incomparable probe must not be counted into the slice total:\n%s", finding.Message)
	}
}

// TestRunDoctorProbesAreOptIntoOut covers the flag on the accuracy page, which
// did not have one until the probes reached it.
func TestRunDoctorProbesAreOptIntoOut(t *testing.T) {
	dbPath, db := seedDoctorDB(t)
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	out := captureStdout(t, func() error {
		return run([]string{"doctor", "--db", dbPath})
	})
	if !strings.Contains(out, "probe/rows walked") {
		t.Errorf("the accuracy page must probe by default:\n%s", out)
	}
	if !strings.Contains(out, "probe/page only") || !strings.Contains(out, "probe/inner group only") {
		t.Errorf("the two structural probes must appear:\n%s", out)
	}

	out = captureStdout(t, func() error {
		return run([]string{"doctor", "--db", dbPath, "--probe=false"})
	})
	if strings.Contains(out, "probe/") {
		t.Errorf("--probe=false must run no probes:\n%s", out)
	}
	if !strings.Contains(out, "probes were skipped") {
		t.Errorf("--probe=false must say the report is missing those numbers:\n%s", out)
	}
}

// TestAccuracySliceFindingIsStatedOnce pins the shape of the page-level slice
// finding. The page runs several independent aggregate passes over the same
// range, so the row count belongs to the page rather than to any one query —
// repeating it under each of them said the same number five times.
func TestAccuracySliceFindingIsStatedOnce(t *testing.T) {
	walked := func(name string, rows int, ms float64) doctorQuery {
		return doctorQuery{Name: name, Table: "price_predictions", CoveringHit: true,
			Probe: &doctorQueryProbe{Name: "rows walked", Rows: rows, DurationMS: ms, Comparable: true}}
	}
	queries := []doctorQuery{
		walked("summary", 1_476_360, 371),
		walked("by_confidence", 1_476_360, 457),
		walked("by_lead", 1_476_360, 383),
		// A different slice and a differently-named probe must not be folded in.
		{Name: "summary_latest", Table: "price_predictions", CoveringHit: true,
			Probe: &doctorQueryProbe{Name: "inner group only", Rows: 21_570, DurationMS: 414, Comparable: true}},
		{Name: "skipped", Skipped: true},
	}
	finding, ok := accuracySliceFinding(queries)
	if !ok {
		t.Fatal("three queries over one slice must produce a finding")
	}
	for _, want := range []string{
		"3 of the page's queries each walk the same 1,476,360 rows",
		"(by_confidence, by_lead, summary)",
		"1211 ms of the total spent walking them over again",
		"a narrower --range is what shrinks that slice",
	} {
		if !strings.Contains(finding.Message, want) {
			t.Errorf("finding is missing %q:\n%s", want, finding.Message)
		}
	}

	// One query over a slice is not "over again", so it reads differently.
	single, ok := accuracySliceFinding([]doctorQuery{walked("summary", 900, 12)})
	if !ok {
		t.Fatal("a single probed query still reports its slice")
	}
	if !strings.Contains(single.Message, "query summary reads 900 rows for this filter, walked in 12 ms") {
		t.Errorf("single-query wording is wrong:\n%s", single.Message)
	}

	// Nothing probed, nothing to say.
	if _, ok := accuracySliceFinding([]doctorQuery{{Name: "summary"}}); ok {
		t.Error("no probes must produce no slice finding")
	}
}

// TestTryIndexVerdictIsJudgedAgainstItsOwnRepeats uses the production numbers
// that exposed the flaw. Five accuracy-page queries already force
// idx_price_predictions_accuracy, so forcing it again re-ran identical SQL and
// still moved by up to 27%. Against that, the one query whose SQL genuinely
// changed moved 28% — indistinguishable from the database having a different
// afternoon, and the old verdict called the whole thing "little difference"
// while five of its seven pairs were not comparisons at all.
func TestTryIndexVerdictIsJudgedAgainstItsOwnRepeats(t *testing.T) {
	pair := func(name string, base, hinted float64, repeat bool) doctorQuery {
		return doctorQuery{Name: name, Table: "price_predictions", DurationMS: base, Rows: 1,
			Hinted: &doctorQueryHint{Index: "idx_price_predictions_accuracy",
				DurationMS: hinted, Rows: 1, Repeat: repeat}}
	}
	queries := []doctorQuery{
		pair("summary", 2414.7, 2180.9, true),        // -10%
		pair("summary_latest", 2941.3, 2861.9, true), //  -3%
		pair("by_confidence", 2034.3, 2028.5, true),  //  -0%
		pair("by_lead", 2121.2, 2150.0, true),        //  +1%
		pair("by_hour", 2563.5, 1876.2, true),        // -27%, identical SQL
		pair("series", 1667.9, 1629.6, false),        //  -2%, genuinely unhinted
		pair("rows", 8024.2, 5770.6, false),          // -28%, genuinely unhinted
	}

	band, repeats := hintNoiseBand(queries)
	if repeats != 5 {
		t.Fatalf("repeats = %d, want the 5 pairs the page already hints", repeats)
	}
	if band < 26 || band > 28 {
		t.Fatalf("noise band = %.1f%%, want by_hour's ~27%%", band)
	}

	got := renderFindings(tryIndexFindings(queries, "idx_price_predictions_accuracy"))
	for _, want := range []string{
		"5 queries already force idx_price_predictions_accuracy, so their second timing re-ran identical SQL",
		"those repeats moved up to 27%",
		// series + rows together are -24%, inside the 27% band.
		"not established either way; repeat the run before acting on it",
		"(rows, series)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("findings are missing %q:\n%s", want, got)
		}
	}
	// The five no-op pairs must not be counted into the compared totals.
	if strings.Contains(got, "summary") || strings.Contains(got, "by_hour") {
		t.Errorf("a repeat must not appear among the queries actually compared:\n%s", got)
	}

	// A move clear of the band is a result, and says so.
	clear := []doctorQuery{
		pair("by_hour", 2000, 1990, true), // -0.5% band
		pair("rows", 8000, 2000, false),   // -75%, unmistakable
	}
	got = renderFindings(tryIndexFindings(clear, "idx_price_predictions_accuracy"))
	if !strings.Contains(got, "cuts the one query (rows) from 8000 ms to 2000 ms (75% less)") {
		t.Errorf("a move clear of the band must be reported as a result:\n%s", got)
	}
	if !strings.Contains(got, "warn") {
		t.Errorf("the optimizer picking a slower index is a warning:\n%s", got)
	}

	// Naming an index every query already forces compares nothing at all.
	allRepeats := []doctorQuery{pair("summary", 2400, 2100, true), pair("by_hour", 2000, 2200, true)}
	got = renderFindings(tryIndexFindings(allRepeats, "idx_price_predictions_accuracy"))
	if !strings.Contains(got, "nothing was actually compared") {
		t.Errorf("all-repeats must say nothing was compared:\n%s", got)
	}
}

// TestProbeAccountingForTheWholeQueryIsStated covers the case production hit on
// `rows`: the probe cost as much as the query, so the joins it drops are free
// and the cost is entirely in the part the probe kept. Saying nothing there
// leaves an 8-second query with no explanation at all.
func TestProbeAccountingForTheWholeQueryIsStated(t *testing.T) {
	opts := doctorOptions{Pages: doctorPages{Accuracy: true}, Probe: true, SlowMS: 1000}
	rows := doctorQuery{
		Name: "rows", Table: "price_predictions", DurationMS: 7503.7, Rows: 1001,
		UsesIndex: "idx_price_predictions_due",
		probeGap:  "joining prediction_runs and stations onto the capped page",
		Probe: &doctorQueryProbe{Name: "page only", DurationMS: 7872.0, Rows: 1001,
			UsesIndex: "idx_price_predictions_due", Comparable: true},
	}
	got := renderFindings(accuracyProbeFindings(rows, rows.probeGap, opts))
	for _, want := range []string{
		"query rows costs 7504 ms and its probe alone costs 7872 ms",
		"joining prediction_runs and stations onto the capped page adds nothing measurable",
		"what the probe itself does is the whole cost",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("findings are missing %q:\n%s", want, got)
		}
	}
	// A probe that accounts for only part of the query keeps the lookup wording.
	partial := rows
	partial.Probe = &doctorQueryProbe{Name: "page only", DurationMS: 100, Rows: 1001,
		UsesIndex: "idx_price_predictions_due", Comparable: true}
	got = renderFindings(accuracyProbeFindings(partial, partial.probeGap, opts))
	if strings.Contains(got, "adds nothing measurable") {
		t.Errorf("a real gap must not be reported as nothing:\n%s", got)
	}
	if !strings.Contains(got, "spends 7404 ms of its 7504 ms") {
		t.Errorf("a real gap must be attributed:\n%s", got)
	}
}

// TestTimeQueryKeepsTheFastestAndTheSpread pins what --runs means. The fastest
// is the summary because every disturbance can only make a run slower, and the
// spread is kept because a difference smaller than a query's own variance is not
// a measurement of anything.
func TestTimeQueryKeepsTheFastestAndTheSpread(t *testing.T) {
	_, db := seedDoctorDB(t)
	defer db.Close()
	ctx := context.Background()

	single := timeQuery(ctx, db, "SELECT 1 FROM price_predictions", nil, 1)
	if single.Err != nil {
		t.Fatalf("single run: %v", single.Err)
	}
	if single.SpreadMS != 0 {
		t.Errorf("one run cannot have a spread, got %v", single.SpreadMS)
	}
	if single.Rows == 0 {
		t.Error("the seeded table has rows")
	}

	many := timeQuery(ctx, db, "SELECT 1 FROM price_predictions", nil, 5)
	if many.Err != nil {
		t.Fatalf("five runs: %v", many.Err)
	}
	if many.SpreadMS < 0 {
		t.Errorf("spread must be slowest minus fastest, got %v", many.SpreadMS)
	}
	if many.Rows != single.Rows {
		t.Errorf("row count changed between runs: %d then %d", single.Rows, many.Rows)
	}
	// The reported timing is the fastest, so more samples can only lower it.
	if many.DurationMS > single.DurationMS+single.DurationMS {
		t.Errorf("five runs reported %v against one run's %v; the fastest should not be slower",
			many.DurationMS, single.DurationMS)
	}

	// A failing query is reported once, not N times, and carries its error.
	bad := timeQuery(ctx, db, "SELECT * FROM no_such_table", nil, 4)
	if bad.Err == nil {
		t.Error("a broken query must report its error")
	}
	if bad.SpreadMS != 0 {
		t.Errorf("a failed measurement has no spread, got %v", bad.SpreadMS)
	}
	// runs below one is treated as one rather than producing no measurement.
	if zero := timeQuery(ctx, db, "SELECT 1 FROM price_predictions", nil, 0); zero.Err != nil || zero.Rows == 0 {
		t.Errorf("--runs 0 must still measure once, got %+v", zero)
	}
}

// TestProbeGapMustClearTheQuerysOwnVariance is the run-1 artefact as a test. The
// production report once attributed 7.7 s to row lookups because that query's
// single sample came in at 16 s where three later runs put it at 8.3 s. A gap
// the timing moved by anyway is not evidence about the probe's missing work.
func TestProbeGapMustClearTheQuerysOwnVariance(t *testing.T) {
	opts := doctorOptions{Pages: doctorPages{Accuracy: true}, Probe: true, SlowMS: 1000, Runs: 3}
	rows := doctorQuery{
		Name: "rows", Table: "price_predictions", DurationMS: 16030.9, Rows: 1001,
		UsesIndex: "idx_price_predictions_due", SpreadMS: 7800,
		probeGap: "joining prediction_runs and stations onto the capped page",
		Probe: &doctorQueryProbe{Name: "page only", DurationMS: 8308.9, Rows: 1001,
			UsesIndex: "idx_price_predictions_due", Comparable: true},
	}
	if _, ok := probeLookupFinding(rows, rows.probeGap, opts); ok {
		t.Error("a gap inside the query's own spread must not be attributed to the probe's missing work")
	}
	// The same shape with a steady timing is a real finding.
	steady := rows
	steady.SpreadMS = 200
	if _, ok := probeLookupFinding(steady, steady.probeGap, opts); !ok {
		t.Error("a gap well clear of the spread is a measurement and must be reported")
	}
	// The probe's own variance counts too — it is half the comparison.
	noisyProbe := steady
	noisyProbe.Probe = &doctorQueryProbe{Name: "page only", DurationMS: 8308.9, Rows: 1001,
		UsesIndex: "idx_price_predictions_due", Comparable: true, SpreadMS: 9000}
	if _, ok := probeLookupFinding(noisyProbe, noisyProbe.probeGap, opts); ok {
		t.Error("a probe that varies more than the gap cannot establish it either")
	}
}

// TestSpreadFindingSaysWhatWasAndWasNotMeasured covers both modes: with repeats
// it reports the band, and without them it says there is none rather than
// letting one sample read as the truth.
func TestSpreadFindingSaysWhatWasAndWasNotMeasured(t *testing.T) {
	queries := []doctorQuery{
		{Name: "summary", DurationMS: 2308, SpreadMS: 120},
		{Name: "rows", DurationMS: 6000, SpreadMS: 1800}, // 30%
		{Name: "skipped", Skipped: true, SpreadMS: 9999},
		// Below the floor: a 58 ms query moved 484% on production, and letting
		// that set the page's band dismissed every real difference as noise.
		{Name: "decisions", DurationMS: 58, SpreadMS: 281},
	}
	finding, ok := spreadFinding(queries, 3)
	if !ok {
		t.Fatal("repeats must produce a band")
	}
	for _, want := range []string{"widest spread on any query above 250 ms was 30% (rows)",
		"treat differences below that as noise", "the fastest of the 3"} {
		if !strings.Contains(finding.Message, want) {
			t.Errorf("finding is missing %q:\n%s", want, finding.Message)
		}
	}

	if strings.Contains(finding.Message, "decisions") {
		t.Errorf("a 58 ms query must not set the page's band:\n%s", finding.Message)
	}

	single, ok := spreadFinding(queries, 1)
	if !ok {
		t.Fatal("a single-sample run must say so")
	}
	for _, want := range []string{"single sample", "--runs 3", "absorbs cache warming"} {
		if !strings.Contains(single.Message, want) {
			t.Errorf("single-run finding is missing %q:\n%s", want, single.Message)
		}
	}
}

// TestBreakdownsOnePassMatchesFourQueries is the correctness check the collapse
// needs. The three breakdown tables and the chart series used to be four grouped
// queries computing AVG in SQL; they are now one pass returning SUM and COUNT
// per (target_start, confidence, lead bucket), with each table summed out of it
// afterwards. This runs both forms against real rows and compares what the page
// would show.
func TestBreakdownsOnePassMatchesFourQueries(t *testing.T) {
	_, db := seedDoctorDB(t)
	defer db.Close()
	ctx := context.Background()

	filters := doctorFilters{Fuel: "diesel", Confidence: "all",
		From: "2026-07-01T00:00:00Z", To: "2026-07-31T23:59:59Z"}
	where := "pp.actual_price IS NOT NULL AND pp.fuel = ? AND pp.target_start >= ? AND pp.target_start <= ?"
	args := []any{filters.Fuel, filters.From, filters.To}
	leadBucket := "CASE" +
		" WHEN pp.lead_minutes < 60 THEN '0-1h'" +
		" WHEN pp.lead_minutes < 180 THEN '1-3h'" +
		" WHEN pp.lead_minutes < 360 THEN '3-6h'" +
		" WHEN pp.lead_minutes < 720 THEN '6-12h'" +
		" WHEN pp.lead_minutes < 1440 THEN '12-24h'" +
		" ELSE '24h+' END"
	hourExpr := "SUBSTR(pp.target_start, 12, 2)"

	type agg struct {
		count int
		mae   float64
		bias  float64
		floor int
	}
	// The old shape: one grouped query per dimension, averaging in SQL.
	perDimension := func(key string) map[string]agg {
		rows, err := db.QueryContext(ctx, "SELECT "+key+" AS k, COUNT(*), AVG(ABS(pp.error)), AVG(pp.error), "+
			"MIN(pp.lead_minutes) FROM price_predictions pp WHERE "+where+" GROUP BY "+key, args...)
		if err != nil {
			t.Fatalf("grouped by %s: %v", key, err)
		}
		defer rows.Close()
		out := map[string]agg{}
		for rows.Next() {
			var k string
			var a agg
			if err := rows.Scan(&k, &a.count, &a.mae, &a.bias, &a.floor); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out[k] = a
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		return out
	}

	// The new shape: one pass, summed down each dimension the way the page does.
	byConfidence, byLead, byHour := map[string]agg{}, map[string]agg{}, map[string]agg{}
	sums := map[string]map[string][3]float64{
		"confidence": {}, "bucket": {}, "hour": {},
	}
	floors := map[string]int{}
	seriesGot := map[string][3]float64{} // target_start -> {count, sum predicted, sum actual}
	rows, err := db.QueryContext(ctx, "SELECT pp.target_start, pp.confidence, "+leadBucket+", "+
		"MIN(pp.lead_minutes), COUNT(*), SUM(ABS(pp.error)), SUM(pp.error), "+
		"SUM(pp.predicted_price), SUM(pp.actual_price) "+
		"FROM price_predictions pp WHERE "+where+
		" GROUP BY pp.target_start, pp.confidence, "+leadBucket, args...)
	if err != nil {
		t.Fatalf("one pass: %v", err)
	}
	defer rows.Close()
	groups := 0
	for rows.Next() {
		var target, confidence, bucket string
		var floor, n int
		var absError, sumError, sumPredicted, sumActual float64
		if err := rows.Scan(&target, &confidence, &bucket, &floor, &n,
			&absError, &sumError, &sumPredicted, &sumActual); err != nil {
			t.Fatalf("scan: %v", err)
		}
		groups++
		// The hour the page derives in PHP: characters 12-13 of target_start.
		hour := target[11:13]
		for dim, key := range map[string]string{"confidence": confidence, "bucket": bucket, "hour": hour} {
			prev := sums[dim][key]
			sums[dim][key] = [3]float64{prev[0] + float64(n), prev[1] + absError, prev[2] + sumError}
		}
		if prev, ok := floors[bucket]; !ok || floor < prev {
			floors[bucket] = floor
		}
		point := seriesGot[target]
		seriesGot[target] = [3]float64{point[0] + float64(n), point[1] + sumPredicted, point[2] + sumActual}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if groups == 0 {
		t.Fatal("the seeded range has rows, so the one pass must return groups")
	}
	for dim, into := range map[string]map[string]agg{
		"confidence": byConfidence, "bucket": byLead, "hour": byHour,
	} {
		for key, totals := range sums[dim] {
			n := int(totals[0])
			into[key] = agg{count: n, mae: totals[1] / totals[0], bias: totals[2] / totals[0], floor: floors[key]}
		}
	}

	// Both forms must agree on every table, to floating-point tolerance: the
	// sums are re-added in a different order, which is the only difference
	// permitted.
	const tolerance = 1e-9
	for _, tc := range []struct {
		name string
		key  string
		got  map[string]agg
	}{
		{"by_confidence", "pp.confidence", byConfidence},
		{"by_lead", leadBucket, byLead},
		{"by_hour", hourExpr, byHour},
	} {
		want := perDimension(tc.key)
		if len(want) == 0 {
			t.Fatalf("%s: the reference query returned nothing to compare", tc.name)
		}
		if len(want) != len(tc.got) {
			t.Errorf("%s: one pass produced %d groups, three queries produced %d", tc.name, len(tc.got), len(want))
		}
		for key, wantAgg := range want {
			gotAgg, ok := tc.got[key]
			if !ok {
				t.Errorf("%s: one pass lost group %q", tc.name, key)
				continue
			}
			if gotAgg.count != wantAgg.count {
				t.Errorf("%s[%s]: count %d, want %d", tc.name, key, gotAgg.count, wantAgg.count)
			}
			if math.Abs(gotAgg.mae-wantAgg.mae) > tolerance {
				t.Errorf("%s[%s]: mae %v, want %v", tc.name, key, gotAgg.mae, wantAgg.mae)
			}
			if math.Abs(gotAgg.bias-wantAgg.bias) > tolerance {
				t.Errorf("%s[%s]: bias %v, want %v", tc.name, key, gotAgg.bias, wantAgg.bias)
			}
			// The lead table's sort key must survive the collapse.
			if tc.name == "by_lead" && gotAgg.floor != wantAgg.floor {
				t.Errorf("%s[%s]: lead_floor %d, want %d", tc.name, key, gotAgg.floor, wantAgg.floor)
			}
		}
	}

	// And the chart, which was the fourth query: mean predicted against mean
	// actual per window.
	seriesRows, err := db.QueryContext(ctx, "SELECT pp.target_start, AVG(pp.predicted_price), "+
		"AVG(pp.actual_price), COUNT(*) FROM price_predictions pp WHERE "+where+
		" GROUP BY pp.target_start", args...)
	if err != nil {
		t.Fatalf("series reference: %v", err)
	}
	defer seriesRows.Close()
	points := 0
	for seriesRows.Next() {
		var target string
		var wantPredicted, wantActual float64
		var wantCount int
		if err := seriesRows.Scan(&target, &wantPredicted, &wantActual, &wantCount); err != nil {
			t.Fatalf("scan series: %v", err)
		}
		points++
		got, ok := seriesGot[target]
		if !ok {
			t.Errorf("the one pass lost chart window %s", target)
			continue
		}
		if int(got[0]) != wantCount {
			t.Errorf("series[%s]: count %d, want %d", target, int(got[0]), wantCount)
		}
		if math.Abs(got[1]/got[0]-wantPredicted) > tolerance {
			t.Errorf("series[%s]: mean predicted %v, want %v", target, got[1]/got[0], wantPredicted)
		}
		if math.Abs(got[2]/got[0]-wantActual) > tolerance {
			t.Errorf("series[%s]: mean actual %v, want %v", target, got[2]/got[0], wantActual)
		}
	}
	if err := seriesRows.Err(); err != nil {
		t.Fatalf("series rows: %v", err)
	}
	if points == 0 {
		t.Fatal("the reference series returned nothing to compare")
	}
	if len(seriesGot) != points {
		t.Errorf("the one pass produced %d chart windows, the reference %d", len(seriesGot), points)
	}
}

// TestAccuracyRowsCapAtATiedBoundary covers what changed when the inner select
// started reading the index backwards on both keys. The cap can only fall in the
// middle of an hour that several stations share, so which of them it keeps is
// what the tie-break decides — and whatever it decides, the page's contract is
// the same: exactly the cap, nothing older than the boundary hour, and displayed
// newest-first with stations ascending.
func TestAccuracyRowsCapAtATiedBoundary(t *testing.T) {
	ctx := context.Background()
	db, err := openDB(filepath.Join(t.TempDir(), "tied.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	if err := initSchema(ctx, db, dialectSQLite); err != nil {
		t.Fatalf("initSchema: %v", err)
	}
	// 20 stations x 60 hours = 1200 rows, so the 1001-row cap lands inside an
	// hour that all 20 stations share.
	const stations, hours = 20, 60
	for i := 0; i < stations; i++ {
		if _, err := db.ExecContext(ctx, `INSERT INTO stations
			(id, name, lat, lng, first_seen_at, last_seen_at) VALUES (?, ?, 52.0, 13.0, '', '')`,
			fmt.Sprintf("st-%02d", i), fmt.Sprintf("Station %02d", i)); err != nil {
			t.Fatalf("insert station: %v", err)
		}
	}
	res, err := db.ExecContext(ctx, `INSERT INTO prediction_runs
		(run_at, city_name, fuel, range_km, history_days, predict_days)
		VALUES ('2026-07-01T00:00:00Z', 'Berlin', 'diesel', 5, 30, 3)`)
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	runID, _ := res.LastInsertId()
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	for h := 0; h < hours; h++ {
		target := base.Add(time.Duration(h) * time.Hour)
		for i := 0; i < stations; i++ {
			if _, err := db.ExecContext(ctx, `INSERT INTO price_predictions
				(run_id, station_id, fuel, target_start, target_end, predicted_price, confidence,
				 sample_count, is_suggestion, lead_minutes, applied_correction, actual_price, error, evaluated_at)
				VALUES (?, ?, 'diesel', ?, ?, 1.7, 'medium', 20, 0, 60, 0.0, 1.71, 0.01, ?)`,
				runID, fmt.Sprintf("st-%02d", i), target.Format(time.RFC3339),
				target.Add(time.Hour).Format(time.RFC3339), target.Format(time.RFC3339)); err != nil {
				t.Fatalf("insert prediction: %v", err)
			}
		}
	}

	filters := doctorFilters{Fuel: "diesel", Confidence: "all",
		From: "2026-01-01T00:00:00Z", To: "2027-01-01T23:59:59Z"}
	var spec accuracyQuerySpec
	for _, candidate := range pageSpecs(filters, false) {
		if candidate.name == "rows" {
			spec = candidate
		}
	}
	rows, err := db.QueryContext(ctx, spec.sql, spec.args...)
	if err != nil {
		t.Fatalf("rows: %v", err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatalf("columns: %v", err)
	}
	type seen struct {
		target  string
		station string
	}
	var got []seen
	for rows.Next() {
		cells := make([]any, len(columns))
		for i := range cells {
			cells[i] = new(sql.NullString)
		}
		if err := rows.Scan(cells...); err != nil {
			t.Fatalf("scan: %v", err)
		}
		var entry seen
		for i, name := range columns {
			value := cells[i].(*sql.NullString).String
			switch name {
			case "target_start":
				entry.target = value
			case "station_id":
				entry.station = value
			}
		}
		got = append(got, entry)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	if len(got) != 1001 {
		t.Fatalf("returned %d rows, want the 1001 cap", len(got))
	}
	// Newest-first for display, stations ascending inside an hour. This is the
	// outer ORDER BY and it must hold whichever direction the inner select read.
	for i := 1; i < len(got); i++ {
		prev, cur := got[i-1], got[i]
		if cur.target > prev.target {
			t.Fatalf("row %d target %s is newer than the row before it (%s)", i, cur.target, prev.target)
		}
		if cur.target == prev.target && cur.station < prev.station {
			t.Fatalf("row %d station %s precedes %s within one hour", i, cur.station, prev.station)
		}
	}
	// Nothing older than the boundary hour, and the boundary hour is partial —
	// which is the case that only exists because stations tie there.
	boundary := got[len(got)-1].target
	counts := map[string]int{}
	for _, entry := range got {
		if entry.target < boundary {
			t.Fatalf("row older than the boundary hour %s slipped in: %s", boundary, entry.target)
		}
		counts[entry.target]++
	}
	if counts[boundary] == stations {
		t.Fatalf("the boundary hour is complete, so the cap did not land on a tie; the test proves nothing")
	}
	// Every hour above the boundary is complete: the cap took whole hours until
	// it ran out, which is what "keep the newest" means here.
	for target, n := range counts {
		if target != boundary && n != stations {
			t.Errorf("hour %s kept %d of %d stations; only the boundary hour may be partial",
				target, n, stations)
		}
	}
	// And the newest hour in the data is present, not the oldest.
	newest := base.Add(time.Duration(hours-1) * time.Hour).Format(time.RFC3339)
	if counts[newest] != stations {
		t.Errorf("the newest hour %s is not fully present", newest)
	}
	if _, ok := counts[base.Format(time.RFC3339)]; ok {
		t.Error("the oldest hour was kept; the cap must keep the newest")
	}
}
