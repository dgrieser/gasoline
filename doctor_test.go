package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
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

// TestRunDoctorLeavesDatabaseUntouched pins doctor as read-only. It must not
// create or migrate the schema: an operator diagnosing a database should not
// have it changed under them, and a missing table is itself a finding.
func TestRunDoctorLeavesDatabaseUntouched(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "empty.db")
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
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
		uses, covering, fullScan := classifyPlan(plan, dialectSQLite, indexes, "pp")
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
		uses, covering, fullScan := classifyPlan(plan, dialectSQLite, indexes, "pp")
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
		uses, _, fullScan := classifyPlan(plan, dialectSQLite, indexes, "pp")
		if uses != "" {
			t.Fatalf("uses = %q, want none", uses)
		}
		if !fullScan {
			t.Fatal("SCAN pp with no index is a table scan and must be reported")
		}
	})

	t.Run("mysql covering read", func(t *testing.T) {
		plan := []string{
			"id=1 select_type=SIMPLE table=pp type=range key=idx_price_predictions_accuracy rows=412000 Extra=Using where; Using index",
		}
		uses, covering, fullScan := classifyPlan(plan, dialectMySQL, indexes, "pp")
		if uses != "idx_price_predictions_accuracy" || !covering || fullScan {
			t.Fatalf("uses=%q covering=%v fullScan=%v, want accuracy index, covering, no scan", uses, covering, fullScan)
		}
	})

	t.Run("mysql full scan", func(t *testing.T) {
		plan := []string{"id=1 select_type=SIMPLE table=pp type=ALL rows=9700000 Extra=Using where"}
		_, _, fullScan := classifyPlan(plan, dialectMySQL, indexes, "pp")
		if !fullScan {
			t.Fatal("type=ALL on the driving table is a full scan")
		}
	})

	t.Run("mysql index condition is not covering", func(t *testing.T) {
		plan := []string{
			"id=1 select_type=SIMPLE table=pp type=range key=idx_price_predictions_due Extra=Using index condition",
		}
		_, covering, _ := classifyPlan(plan, dialectMySQL, indexes, "pp")
		if covering {
			t.Fatal("\"Using index condition\" still reads rows and is not a covering read")
		}
	})

	t.Run("mysql scan of another table is ignored", func(t *testing.T) {
		plan := []string{
			"id=1 select_type=SIMPLE table=s type=ALL rows=90",
			"id=1 select_type=SIMPLE table=pp type=ref key=idx_price_predictions_station_fuel_target rows=40",
		}
		_, _, fullScan := classifyPlan(plan, dialectMySQL, indexes, "pp")
		if fullScan {
			t.Fatal("a scan of the stations table must not be blamed on price_predictions")
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
		},
		"by_confidence": {"GROUP BY pp.confidence"},
		"by_lead": {
			"WHEN pp.lead_minutes < 360 THEN '3-6h'",
			"MIN(pp.lead_minutes)",
		},
		"by_hour": {"SUBSTR(pp.target_start, 12, 2)"},
		"series":  {"AVG(pp.predicted_price)", "GROUP BY pp.target_start"},
		"rows":    {"COALESCE(s.name_override, s.name)", "ORDER BY pp.target_start DESC, pp.station_id ASC"},
		"decisions": {
			"SUM(CASE WHEN d.regret <= 0.01 THEN 1 ELSE 0 END)",
			"GROUP BY d.recommendation",
		},
	}

	specs := map[string]accuracyQuerySpec{}
	for _, spec := range accuracyQuerySpecs(doctorFilters{Fuel: "diesel", Confidence: "all",
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
	mediumHigh := accuracyQuerySpecs(doctorFilters{Fuel: "diesel", Confidence: "medium_high"}, false)
	if !strings.Contains(mediumHigh[0].sql, "pp.confidence IN ('medium', 'high')") {
		t.Error("doctor ignores --confidence medium_high")
	}
	withCity := accuracyQuerySpecs(doctorFilters{Fuel: "diesel", Confidence: "all", City: "Berlin"}, false)
	if !strings.Contains(withCity[0].sql, "JOIN prediction_runs pr ON pr.id = pp.run_id") ||
		!strings.Contains(withCity[0].sql, "pr.city_name = ?") {
		t.Errorf("doctor ignores --city:\n%s", withCity[0].sql)
	}
	// The city filter adds a bound parameter; a mismatch here would make every
	// query fail at execution time.
	if len(withCity[0].args) != 4 {
		t.Fatalf("city-filtered query has %d args, want 4", len(withCity[0].args))
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
