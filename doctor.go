package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

// doctor inspects a live database instead of changing it: what the tables cost,
// which indexes exist, which stations are still in scope, and — the part that
// answers "why is this page slow" — what the planner actually does with one
// web page's queries and how long each one takes. Everything it runs is
// read-only, so it is safe against production.
//
// Which page is measured is a subcommand (resolveDoctorPages): bare `doctor`
// measures the admin accuracy page, `doctor dashboard` the dashboard, and
// `doctor all` both. A page's section costs about what one load of that page
// costs, which is why it is asked for by name rather than measured every time.
// The dashboard's half lives in doctor_dashboard.go.
//
// The single exception to read-only is --optimize, which rebuilds tables to hand
// freed pages back to the filesystem. It writes, it is opt-in, and it runs after
// every measurement so the report still describes the database as it was found.
//
// The queries live here rather than being read out of web/index.php because
// the page builds them in PHP. accuracyQuerySpecsFor documents that duplication
// and TestDoctorAccuracyQueriesMatchViewer guards it against drift.

// datePattern matches the YYYY-MM-DD form --from/--to accept, mirroring the
// viewer's own validation of those query parameters.
var datePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// tableExists reports whether a table is present. doctor runs against
// databases of any vintage, including ones predating price_check_decisions, so
// every lookup has to tolerate an absent table rather than erroring.
func tableExists(ctx context.Context, q queryer, d dialect, name string) (bool, error) {
	var stmt string
	if d == dialectMySQL {
		stmt = `SELECT COUNT(*) FROM information_schema.tables
			WHERE table_schema = DATABASE() AND table_name = ?`
	} else {
		stmt = `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`
	}
	var count int
	if err := q.QueryRowContext(ctx, stmt, name).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

// doctorTables are the tables doctor reports on, largest-first in practice.
var doctorTables = []string{
	"price_predictions",
	"price_check_decisions",
	"prediction_runs",
	"price_snapshots",
	"stations",
	// cities is here because the dashboard resolves its city filter against it
	// on every load and `gasoline import cities` can grow it by five orders of
	// magnitude, so both its row count and whether idx_cities_normalized is
	// present belong in the report.
	"cities",
	// The command-run tables are small by design (30 days of timer runs), but
	// they back the admin Statistics page, so a missing index there is worth
	// naming before it turns into a slow page.
	"command_run_metrics",
	"command_runs",
}

// doctorExpectedIndexes lists the indexes schemaStatements installs, so doctor
// can name a missing one instead of leaving an operator to infer it from a bad
// query plan. The run_id indexes are SQLite-only: MySQL gets an equivalent
// index implicitly from the foreign key, under a server-chosen name.
func doctorExpectedIndexes(d dialect) map[string][]string {
	expected := map[string][]string{
		"price_predictions": {
			"idx_price_predictions_station_fuel_target",
			"idx_price_predictions_due",
			"idx_price_predictions_accuracy",
		},
		"price_check_decisions": {
			"idx_price_check_decisions_station_fuel_target",
			"idx_price_check_decisions_due",
		},
		"prediction_runs": {"idx_prediction_runs_city_fuel_run"},
		"price_snapshots": {
			"idx_price_snapshots_station_recorded",
			"idx_price_snapshots_city_recorded",
		},
		"stations": {"idx_stations_lat_lng"},
		// Both cities indexes serve the dashboard: one its city filter, the
		// other its typeahead.
		"cities": {"idx_cities_normalized", "idx_cities_search"},
		"command_runs": {
			"idx_command_runs_command_started",
			"idx_command_runs_started",
		},
		// Unlike the run_id indexes above, this one is not the foreign key's:
		// it is (run_id, name), so MySQL declares it explicitly too.
		"command_run_metrics": {"idx_command_run_metrics_run"},
	}
	if d != dialectMySQL {
		expected["price_predictions"] = append(expected["price_predictions"], "idx_price_predictions_run")
		expected["price_check_decisions"] = append(expected["price_check_decisions"], "idx_price_check_decisions_run")
	}
	return expected
}

type doctorResult struct {
	Database doctorDatabase  `json:"database"`
	Tables   []doctorTable   `json:"tables"`
	Indexes  []doctorIndex   `json:"indexes"`
	Scope    doctorScope     `json:"scope"`
	Optimize *doctorOptimize `json:"optimize,omitempty"`
	Filters  doctorFilters   `json:"filters"`
	Queries  []doctorQuery   `json:"queries"`
	// Dashboard is the dashboard page's measurements, present only when that
	// page was selected. The accuracy page's live in Queries above; the two
	// pages share nothing but the schema, so they are reported apart.
	Dashboard *doctorDashboard `json:"dashboard,omitempty"`
	Findings  []doctorFinding  `json:"findings"`
}

type doctorDatabase struct {
	Target  string `json:"target"`
	Driver  string `json:"driver"`
	Version string `json:"version"`
}

type doctorTable struct {
	Name string `json:"name"`
	Rows int64  `json:"rows"`
	// RowsApproximate marks InnoDB's estimate, which can be off by a large
	// factor; an exact count would mean a full scan of the table doctor is
	// being asked to diagnose.
	RowsApproximate bool   `json:"rows_approximate"`
	DataBytes       *int64 `json:"data_bytes"`
	IndexBytes      *int64 `json:"index_bytes"`
	Missing         bool   `json:"missing"`
}

type doctorIndex struct {
	Table   string   `json:"table"`
	Name    string   `json:"name"`
	Columns []string `json:"columns"`
	Bytes   *int64   `json:"bytes"`
	Present bool     `json:"present"`
	// Unexpected marks an index doctor found but does not know about: usually
	// a MySQL foreign-key index, sometimes a hand-added one.
	Unexpected bool `json:"unexpected"`
}

// doctorScope is the station universe suggest, check and notify work from,
// held against the update targets that are supposed to define it.
//
// It answers the question the rest of doctor cannot: why a city nobody feeds
// any more still shows up in prediction output. There are two different answers
// and they need different fixes. Either its stations are genuinely still in
// scope — they had a price update within stationFreshness, so every computation
// still covers them — or only stored rows remain, which the next
// `suggest --persist` run drops (pruneUnfedStations). The first is a live
// collection problem; the second clears itself within the hour.
type doctorScope struct {
	FreshnessHours float64 `json:"freshness_hours"`
	RetentionDays  int     `json:"retention_days"`
	// Configured is whether update_targets has any rows. Without them a sweep
	// is driven entirely by `update --city` flags, so a city that is not a
	// target says nothing about whether it should be there.
	Configured bool                `json:"configured"`
	Targets    []doctorScopeTarget `json:"targets"`
	Cities     []doctorScopeCity   `json:"cities"`
	LatestRun  *doctorScopeRun     `json:"latest_run"`
	// ProbeLimited marks a report that stopped looking up stored predictions
	// after doctorScopeProbeLimit stations, so the newest-prediction column is
	// incomplete rather than wrong.
	ProbeLimited bool `json:"probe_limited"`
	// Skipped covers a database too old — or too empty — to have the tables the
	// scope picture is built from.
	Skipped bool   `json:"skipped"`
	Reason  string `json:"reason,omitempty"`
}

// doctorScopeTarget is one configured update target, resolved through the
// cities cache the way the update sweep resolves it.
type doctorScopeTarget struct {
	// City is the string an admin typed; Normalized is the geocoder's name for
	// the same place, which is what snapshots record as their owner.
	City       string  `json:"city"`
	Normalized string  `json:"normalized"`
	RadiusKM   float64 `json:"radius_km"`
	// Geocoded is false when the city has never been resolved, so the target
	// has never fetched anything.
	Geocoded bool `json:"geocoded"`
}

// doctorScopeCity is one owning city — every station whose newest snapshot
// names it — counted against what the pipeline currently does with it.
type doctorScopeCity struct {
	City   string `json:"city"`
	Target bool   `json:"target"`
	// RadiusKM is the configured target's radius, zero for a city that is not
	// one.
	RadiusKM float64 `json:"radius_km,omitempty"`
	// Geocoded is only meaningful for a target: false means the target names a
	// place the cities cache does not know.
	Geocoded bool `json:"geocoded"`
	Stations int  `json:"stations"`
	// InScope counts the owned stations fed within stationFreshness, which is
	// exactly the set suggest, check and notify compute over.
	InScope int `json:"in_scope"`
	// InLatestRun counts the owned stations the newest prediction run actually
	// stored predictions for.
	InLatestRun    int    `json:"in_latest_run"`
	NewestSnapshot string `json:"newest_snapshot,omitempty"`
	// NewestPrediction is the latest target window stored for any of this
	// city's out-of-scope stations, for the doctor's --fuel. Rows should not
	// outlive scope by more than one persist run, so anything here that is not
	// from the last hour means pruneUnfedStations is not running.
	NewestPrediction string `json:"newest_prediction,omitempty"`
}

// doctorScopeRun is the newest prediction run, which is the pipeline's own
// record of what it last considered in scope.
type doctorScopeRun struct {
	ID    int64  `json:"id"`
	RunAt string `json:"run_at"`
	Fuel  string `json:"fuel"`
	// StationCount is what the run row claims; Stations is how many distinct
	// stations its prediction rows actually name.
	StationCount int `json:"station_count"`
	Stations     int `json:"stations"`
	// OutOfScope counts stations the run covered that have had no price update
	// within stationFreshness. On a run younger than that window this should be
	// zero, and anything else means the run drew from something other than the
	// fed universe.
	OutOfScope int `json:"out_of_scope"`
}

// doctorOptimize is the one thing doctor does that writes: rebuilding a table
// to hand its freed pages back to the filesystem.
//
// Deleting rows does not shrink a table. InnoDB keeps the emptied pages for
// reuse, and SQLite keeps them on its free list, so after a large prune — a
// removed update target taking half the prediction rows with it — the size
// doctor reports stays where it was until the table is rebuilt. That rebuild is
// exactly what OPTIMIZE TABLE (MySQL) and VACUUM (SQLite) do, and it is opt-in
// because it is the only part of doctor that is not read-only.
type doctorOptimize struct {
	// Statement is the form doctor issued, so an operator can audit or repeat it
	// by hand.
	Statement string                `json:"statement"`
	Tables    []doctorOptimizeTable `json:"tables"`
	// FileBytesBefore / FileBytesAfter bracket the SQLite database file, which
	// is where that engine's reclaimed space actually becomes visible: VACUUM
	// rewrites the whole file, not one table.
	FileBytesBefore *int64 `json:"file_bytes_before,omitempty"`
	FileBytesAfter  *int64 `json:"file_bytes_after,omitempty"`
	Skipped         bool   `json:"skipped"`
	Reason          string `json:"reason,omitempty"`
}

// doctorOptimizeTable is one rebuilt table, measured on both sides of the
// rebuild. On SQLite there is a single entry named for the database, because
// VACUUM cannot be pointed at one table.
type doctorOptimizeTable struct {
	Name             string  `json:"name"`
	DurationMS       float64 `json:"duration_ms"`
	DataBytesBefore  *int64  `json:"data_bytes_before,omitempty"`
	DataBytesAfter   *int64  `json:"data_bytes_after,omitempty"`
	IndexBytesBefore *int64  `json:"index_bytes_before,omitempty"`
	IndexBytesAfter  *int64  `json:"index_bytes_after,omitempty"`
	// Notes carry the engine's own commentary. InnoDB answers OPTIMIZE TABLE
	// with "Table does not support optimize, doing recreate + analyze instead",
	// which is the rebuild working as intended rather than a refusal, and an
	// operator who has not seen it before reads it as an error.
	Notes []string `json:"notes,omitempty"`
	Error string   `json:"error,omitempty"`
}

type doctorFilters struct {
	Fuel       string `json:"fuel"`
	Confidence string `json:"confidence"`
	From       string `json:"from"`
	To         string `json:"to"`
}

type doctorQuery struct {
	Name    string `json:"name"`
	Purpose string `json:"purpose"`
	// Table is the large table the query drives from, which is what the
	// full-scan and index verdicts are about.
	Table      string   `json:"table"`
	SQL        string   `json:"sql"`
	DurationMS float64  `json:"duration_ms"`
	Rows       int      `json:"rows"`
	Plan       []string `json:"plan"`
	UsesIndex  string   `json:"uses_index"`
	// Considered are the indexes the planner weighed but did not choose
	// (MySQL's possible_keys). A better index sitting unchosen here usually
	// means the optimizer's statistics are stale rather than that the index is
	// wrong, which is worth telling an operator apart.
	Considered  []string `json:"considered"`
	CoveringHit bool     `json:"covering"`
	FullScan    bool     `json:"full_scan"`
	Error       string   `json:"error,omitempty"`
	Skipped     bool     `json:"skipped"`
	// Hinted is the same query re-run with an index forced, present only under
	// --try-index. It answers "would a different index choice actually be
	// faster here" with a measurement rather than a guess.
	Hinted *doctorQueryHint `json:"hinted,omitempty"`
	// Probe is a derived measurement of part of this query's cost, used by the
	// dashboard checks; see dashboardProbeSpec.
	Probe *doctorQueryProbe `json:"probe,omitempty"`
}

// doctorQueryProbe is one derived, read-only measurement taken beside a query:
// the same access path with a narrower projection, so the gap between the two
// prices whatever the narrower one leaves out.
type doctorQueryProbe struct {
	Name        string   `json:"name"`
	Purpose     string   `json:"purpose"`
	SQL         string   `json:"sql"`
	DurationMS  float64  `json:"duration_ms"`
	Rows        int      `json:"rows"`
	Plan        []string `json:"plan"`
	UsesIndex   string   `json:"uses_index"`
	CoveringHit bool     `json:"covering"`
	Error       string   `json:"error,omitempty"`
}

type doctorQueryHint struct {
	Index       string   `json:"index"`
	DurationMS  float64  `json:"duration_ms"`
	Rows        int      `json:"rows"`
	Plan        []string `json:"plan"`
	UsesIndex   string   `json:"uses_index"`
	CoveringHit bool     `json:"covering"`
	Error       string   `json:"error,omitempty"`
}

type doctorFinding struct {
	Severity string `json:"severity"` // "warn" | "info"
	Message  string `json:"message"`
}

// accuracyQuerySpec is one query the admin accuracy page issues.
type accuracyQuerySpec struct {
	name    string
	purpose string
	sql     string
	args    []any
	// table and alias name the large table the query drives from, so plan
	// classification can be scoped to it (see classifyPlan).
	table string
	alias string
}

// accuracyQuerySpecs mirrors the queries in web/index.php's
// action=prediction_accuracy handler. Keep the SQL shapes in step with that
// handler: the numbering matches its section comments, and
// TestDoctorAccuracyQueriesMatchViewer fails if the two drift apart.
// accuracyQueryContext is everything that shapes the page's SQL: the filters,
// the dialect (hint syntax differs), whether the decisions table exists, and
// whether the covering index is there to be hinted at.
type accuracyQueryContext struct {
	Filters      doctorFilters
	Dialect      dialect
	HasDecisions bool
	// AccuracyIndexPresent mirrors the page's own guard: with the index absent
	// the page emits no hint, because forcing an unknown key errors.
	AccuracyIndexPresent bool
	// ForceIndex overrides the page's per-query hint policy and forces this
	// index on every price_predictions query. Set only by --try-index.
	ForceIndex string
}

// pageHintedQueries are the queries the accuracy page forces
// idx_price_predictions_accuracy on (see gasolineAccuracyIndexHint in
// web/index.php). series and rows are deliberately absent: measured on the live
// database the hint moved them by +1% and -2%, so they keep the plain reference
// and leave the optimizer free.
var pageHintedQueries = map[string]bool{
	"summary":        true,
	"summary_latest": true,
	"by_confidence":  true,
	"by_lead":        true,
	"by_hour":        true,
}

// accuracyQuerySpecsHinted builds the page's queries, optionally overriding
// which index they force.
//
// With an empty override the SQL mirrors the page exactly, hints included, so
// doctor's baseline is what a page load actually costs. A non-empty override
// forces that index on every price_predictions query instead — this is what
// `doctor --try-index` measures against the baseline, and it replaces the
// page's own hint rather than stacking with it.
func accuracyQuerySpecsFor(qc accuracyQueryContext) []accuracyQuerySpec {
	f := qc.Filters
	hasDecisions := qc.HasDecisions
	plain := "price_predictions pp "
	// predictionsFor resolves the table reference for one query by name.
	predictionsFor := func(name string) string {
		if qc.ForceIndex != "" {
			return plain + indexHintSyntax(qc.Dialect, qc.ForceIndex) + " "
		}
		if pageHintedQueries[name] && qc.AccuracyIndexPresent {
			return plain + indexHintSyntax(qc.Dialect, "idx_price_predictions_accuracy") + " "
		}
		return plain
	}
	joinRuns := ""
	where := "pp.actual_price IS NOT NULL AND pp.fuel = ? AND pp.target_start >= ? AND pp.target_start <= ?"
	args := []any{f.Fuel, f.From, f.To}
	if f.Confidence == "medium_high" {
		where += " AND pp.confidence IN ('medium', 'high')"
	}
	// Each spec repeats the filter, so the bound arguments repeat with it.
	argsFor := func(times int) []any {
		out := make([]any, 0, len(args)*times)
		for i := 0; i < times; i++ {
			out = append(out, args...)
		}
		return out
	}

	leadBucket := "CASE" +
		" WHEN pp.lead_minutes < 60 THEN '0-1h'" +
		" WHEN pp.lead_minutes < 180 THEN '1-3h'" +
		" WHEN pp.lead_minutes < 360 THEN '3-6h'" +
		" WHEN pp.lead_minutes < 720 THEN '6-12h'" +
		" WHEN pp.lead_minutes < 1440 THEN '12-24h'" +
		" ELSE '24h+' END"
	hourExpr := "SUBSTR(pp.target_start, 12, 2)"

	specs := []accuracyQuerySpec{
		{
			name:    "summary",
			purpose: "overall accuracy tiles (all stored predictions)",
			sql: "SELECT COUNT(*) AS n, COUNT(DISTINCT pp.station_id) AS stations, " +
				"MIN(pp.target_start) AS first_t, MAX(pp.target_start) AS last_t, " +
				"AVG(ABS(pp.error)) AS mae, AVG(pp.error) AS bias, AVG(pp.error * pp.error) AS mse, " +
				"MIN(pp.error) AS min_err, MAX(pp.error) AS max_err, " +
				"SUM(CASE WHEN ABS(pp.error) <= 0.01 THEN 1 ELSE 0 END) AS within1, " +
				"SUM(CASE WHEN ABS(pp.error) <= 0.02 THEN 1 ELSE 0 END) AS within2 " +
				"FROM " + predictionsFor("summary") + joinRuns + "WHERE " + where,
			args:  argsFor(1),
			table: "price_predictions",
			alias: "pp",
		},
		{
			name:    "summary_latest",
			purpose: "latest-run-per-window tiles (added in #44)",
			sql: "SELECT COUNT(*) AS n, AVG(ABS(pp.error)) AS mae, AVG(pp.error) AS bias, " +
				"SUM(CASE WHEN ABS(pp.error) <= 0.02 THEN 1 ELSE 0 END) AS within2 " +
				"FROM " + predictionsFor("summary_latest") + "JOIN (" +
				"SELECT pp.station_id AS station_id, pp.target_start AS target_start, MAX(pp.run_id) AS run_id " +
				"FROM " + predictionsFor("summary_latest") + joinRuns + "WHERE " + where +
				" GROUP BY pp.station_id, pp.target_start" +
				") latest ON latest.station_id = pp.station_id" +
				" AND latest.target_start = pp.target_start" +
				" AND latest.run_id = pp.run_id" +
				// Redundant by the join keys, but it is what makes the
				// covering index usable for the outer lookup; see the page.
				" WHERE pp.fuel = ?",
			args:  append(argsFor(1), f.Fuel),
			table: "price_predictions",
			alias: "pp",
		},
		{
			name:    "by_confidence",
			purpose: "accuracy by confidence tier",
			sql: "SELECT pp.confidence AS confidence, COUNT(*) AS n, " +
				"AVG(ABS(pp.error)) AS mae, AVG(pp.error) AS bias " +
				"FROM " + predictionsFor("by_confidence") + joinRuns + "WHERE " + where + " GROUP BY pp.confidence",
			args:  argsFor(1),
			table: "price_predictions",
			alias: "pp",
		},
		{
			name:    "by_lead",
			purpose: "accuracy by lead-time bucket",
			sql: "SELECT " + leadBucket + " AS bucket, COUNT(*) AS n, " +
				"AVG(ABS(pp.error)) AS mae, AVG(pp.error) AS bias, MIN(pp.lead_minutes) AS lead_floor " +
				"FROM " + predictionsFor("by_lead") + joinRuns + "WHERE " + where + " GROUP BY " + leadBucket,
			args:  argsFor(1),
			table: "price_predictions",
			alias: "pp",
		},
		{
			name:    "by_hour",
			purpose: "accuracy by target hour (UTC)",
			sql: "SELECT " + hourExpr + " AS hour, COUNT(*) AS n, " +
				"AVG(ABS(pp.error)) AS mae, AVG(pp.error) AS bias " +
				"FROM " + predictionsFor("by_hour") + joinRuns + "WHERE " + where +
				" GROUP BY " + hourExpr + " ORDER BY hour ASC",
			args:  argsFor(1),
			table: "price_predictions",
			alias: "pp",
		},
		{
			name:    "series",
			purpose: "predicted-vs-actual timeline",
			sql: "SELECT pp.target_start AS t, AVG(pp.predicted_price) AS p, AVG(pp.actual_price) AS a, COUNT(*) AS n " +
				"FROM " + predictionsFor("series") + joinRuns + "WHERE " + where +
				" GROUP BY pp.target_start ORDER BY pp.target_start ASC",
			args:  argsFor(1),
			table: "price_predictions",
			alias: "pp",
		},
		{
			name:    "rows",
			purpose: "raw evaluated rows for the table (capped)",
			sql: "SELECT page.station_id, page.fuel, pr.run_at, page.target_start, page.target_end, " +
				"page.predicted_price, page.actual_price, page.error, page.confidence, page.lead_minutes, page.is_suggestion, " +
				"COALESCE(s.name_override, s.name) AS name, s.brand, s.street, s.house_number, s.post_code, s.place " +
				"FROM (" +
				"SELECT pp.run_id, pp.station_id, pp.fuel, pp.target_start, pp.target_end, " +
				"pp.predicted_price, pp.actual_price, pp.error, pp.confidence, pp.lead_minutes, pp.is_suggestion " +
				"FROM " + predictionsFor("rows") + joinRuns + "WHERE " + where +
				" ORDER BY pp.target_start DESC, pp.station_id ASC LIMIT 1001" +
				") page " +
				"JOIN prediction_runs pr ON pr.id = page.run_id " +
				"JOIN stations s ON s.id = page.station_id " +
				" ORDER BY page.target_start DESC, page.station_id ASC",
			args:  argsFor(1),
			table: "price_predictions",
			alias: "pp",
		},
	}

	if hasDecisions {
		decWhere := "d.outcome_evaluated_at IS NOT NULL AND d.regret IS NOT NULL" +
			" AND d.fuel = ? AND d.target_start >= ? AND d.target_start <= ?"
		decArgs := []any{f.Fuel, f.From, f.To}
		decJoin := ""
		if f.Confidence == "medium_high" {
			decWhere += " AND d.confidence IN ('medium', 'high')"
		}
		specs = append(specs, accuracyQuerySpec{
			name:    "decisions",
			purpose: "alert outcomes (regret per recommendation)",
			sql: "SELECT d.recommendation AS recommendation, COUNT(*) AS n, " +
				"AVG(d.regret) AS avg_regret, " +
				"SUM(CASE WHEN d.regret <= 0.01 THEN 1 ELSE 0 END) AS within1, " +
				"SUM(CASE WHEN d.regret <= 0.02 THEN 1 ELSE 0 END) AS within2 " +
				"FROM price_check_decisions d " + decJoin +
				"WHERE " + decWhere + " GROUP BY d.recommendation",
			args:  decArgs,
			table: "price_check_decisions",
			alias: "d",
		})
	}
	return specs
}

// resolveDoctorRange mirrors the page's date defaults: a named range, or an
// explicit from/to, or the last 14 days.
func resolveDoctorRange(rangeName, from, to string, now time.Time) (string, string, error) {
	now = now.UTC()
	rangeName = strings.TrimSpace(rangeName)
	from = strings.TrimSpace(from)
	to = strings.TrimSpace(to)

	if rangeName != "" && (from != "" || to != "") {
		return "", "", errors.New("--range cannot be combined with --from/--to")
	}
	if from != "" || to != "" {
		if from == "" || to == "" {
			return "", "", errors.New("--from and --to must be given together")
		}
		if !datePattern.MatchString(from) || !datePattern.MatchString(to) {
			return "", "", errors.New("--from and --to must be YYYY-MM-DD dates")
		}
		return from + "T00:00:00Z", to + "T23:59:59Z", nil
	}

	days := 14
	switch rangeName {
	case "", "14d":
	case "7d":
		days = 7
	case "30d":
		days = 30
	default:
		return "", "", errors.New("--range must be one of: 7d, 14d, 30d")
	}
	start := now.AddDate(0, 0, -days)
	return start.Format("2006-01-02") + "T00:00:00Z", now.Format("2006-01-02") + "T23:59:59Z", nil
}

// serverVersion reports the engine version, which decides which plan features
// are even available (EXPLAIN ANALYZE needs MySQL 8.0.18+, for instance).
func serverVersion(ctx context.Context, q queryer, d dialect) string {
	stmt := "SELECT sqlite_version()"
	if d == dialectMySQL {
		stmt = "SELECT VERSION()"
	}
	var v sql.NullString
	if err := q.QueryRowContext(ctx, stmt).Scan(&v); err != nil {
		return "unknown"
	}
	return v.String
}

// mysqlStatsConn pins one connection and disables information_schema's
// statistics cache on it.
//
// MySQL 8 answers data_length and index_length from a snapshot it keeps for
// information_schema_stats_expiry seconds — 86400 by default. A table that was
// just pruned, or just rebuilt, therefore keeps reporting its old size for up to
// a day, which would make both the sizes doctor prints and the space --optimize
// reports it returned quietly wrong. The setting is per session, and a pooled
// *sql.DB hands out whichever connection is free, so the session and the queries
// that rely on it have to be the same connection.
//
// Servers without the variable (MariaDB) reject the SET; the connection is still
// usable and their statistics are not cached this way, so the error is ignored.
func mysqlStatsConn(ctx context.Context, db *sql.DB) (*sql.Conn, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	_, _ = conn.ExecContext(ctx, "SET SESSION information_schema_stats_expiry = 0")
	return conn, nil
}

// doctorTableStats reports size and row counts. SQLite counts exactly (cheap
// enough, and dbstat gives real byte sizes); MySQL reads information_schema so
// that diagnosing a huge table does not require scanning it.
func doctorTableStats(ctx context.Context, q queryer, d dialect) ([]doctorTable, error) {
	out := make([]doctorTable, 0, len(doctorTables))
	for _, name := range doctorTables {
		table := doctorTable{Name: name}
		exists, err := tableExists(ctx, q, d, name)
		if err != nil {
			return nil, err
		}
		if !exists {
			table.Missing = true
			out = append(out, table)
			continue
		}
		if d == dialectMySQL {
			var rows, dataLen, indexLen sql.NullInt64
			err := q.QueryRowContext(ctx, `
				SELECT table_rows, data_length, index_length
				FROM information_schema.tables
				WHERE table_schema = DATABASE() AND table_name = ?
			`, name).Scan(&rows, &dataLen, &indexLen)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
			table.Rows = rows.Int64
			table.RowsApproximate = true
			table.DataBytes = nullInt64Ptr(dataLen)
			table.IndexBytes = nullInt64Ptr(indexLen)
		} else {
			if err := q.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+name).Scan(&table.Rows); err != nil {
				return nil, err
			}
			if size, ok := sqliteBtreeBytes(ctx, q, name); ok {
				table.DataBytes = &size
			}
		}
		out = append(out, table)
	}
	return out, nil
}

// sqliteBtreeBytes reads a b-tree's size from dbstat, which is compiled in for
// some builds only; a missing dbstat just means doctor omits sizes.
func sqliteBtreeBytes(ctx context.Context, q queryer, name string) (int64, bool) {
	var size sql.NullInt64
	if err := q.QueryRowContext(ctx, `SELECT SUM(pgsize) FROM dbstat WHERE name = ?`, name).Scan(&size); err != nil {
		return 0, false
	}
	if !size.Valid {
		return 0, false
	}
	return size.Int64, true
}

// doctorIndexReport lists the indexes on each reported table, flags the
// expected ones that are absent, and attaches per-index sizes where the engine
// exposes them.
func doctorIndexReport(ctx context.Context, db *sql.DB, d dialect) ([]doctorIndex, error) {
	expected := doctorExpectedIndexes(d)
	var out []doctorIndex
	for _, table := range doctorTables {
		exists, err := tableExists(ctx, db, d, table)
		if err != nil {
			return nil, err
		}
		if !exists {
			continue
		}
		found, err := indexColumns(ctx, db, d, table)
		if err != nil {
			return nil, err
		}
		sizes := indexSizes(ctx, db, d, table)

		names := make([]string, 0, len(found))
		for name := range found {
			names = append(names, name)
		}
		sort.Strings(names)

		wanted := map[string]bool{}
		for _, name := range expected[table] {
			wanted[name] = true
		}
		for _, name := range names {
			idx := doctorIndex{Table: table, Name: name, Columns: found[name], Present: true}
			idx.Unexpected = !wanted[name]
			if size, ok := sizes[name]; ok {
				idx.Bytes = &size
			}
			out = append(out, idx)
		}
		for _, name := range expected[table] {
			if _, ok := found[name]; !ok {
				out = append(out, doctorIndex{Table: table, Name: name, Present: false})
			}
		}
	}
	return out, nil
}

// indexColumns maps index name to its key columns in order.
func indexColumns(ctx context.Context, q queryer, d dialect, table string) (map[string][]string, error) {
	out := map[string][]string{}
	if d == dialectMySQL {
		rows, err := q.QueryContext(ctx, `
			SELECT index_name, column_name
			FROM information_schema.statistics
			WHERE table_schema = DATABASE() AND table_name = ?
			ORDER BY index_name, seq_in_index
		`, table)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var name, column string
			if err := rows.Scan(&name, &column); err != nil {
				return nil, err
			}
			out[name] = append(out[name], column)
		}
		return out, rows.Err()
	}

	rows, err := q.QueryContext(ctx, fmt.Sprintf("PRAGMA index_list(%s)", table))
	if err != nil {
		return nil, err
	}
	var names []string
	for rows.Next() {
		var (
			seq     int
			name    string
			unique  int
			origin  string
			partial int
		)
		if err := rows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			rows.Close()
			return nil, err
		}
		// Skip the implicit indexes SQLite creates for UNIQUE constraints;
		// they are not part of what doctor reasons about.
		if strings.HasPrefix(name, "sqlite_autoindex_") {
			continue
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	for _, name := range names {
		info, err := q.QueryContext(ctx, fmt.Sprintf("PRAGMA index_info(%s)", name))
		if err != nil {
			return nil, err
		}
		var columns []string
		for info.Next() {
			var (
				seqno int
				cid   int
				cname sql.NullString
			)
			if err := info.Scan(&seqno, &cid, &cname); err != nil {
				info.Close()
				return nil, err
			}
			if cname.Valid {
				columns = append(columns, cname.String)
			}
		}
		if err := info.Err(); err != nil {
			info.Close()
			return nil, err
		}
		info.Close()
		out[name] = columns
	}
	return out, nil
}

// indexSizes is best-effort: MySQL exposes per-index sizes only through
// mysql.innodb_index_stats, which a least-privilege account may not read, and
// SQLite needs the optional dbstat module. Either way a missing size is
// cosmetic, so failures are swallowed.
func indexSizes(ctx context.Context, q queryer, d dialect, table string) map[string]int64 {
	out := map[string]int64{}
	if d == dialectMySQL {
		rows, err := q.QueryContext(ctx, `
			SELECT s.index_name, s.stat_value * @@innodb_page_size
			FROM mysql.innodb_index_stats s
			WHERE s.database_name = DATABASE() AND s.table_name = ? AND s.stat_name = 'size'
		`, table)
		if err != nil {
			return out
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			var size int64
			if err := rows.Scan(&name, &size); err != nil {
				return out
			}
			out[name] = size
		}
		return out
	}

	names, err := indexColumns(ctx, q, d, table)
	if err != nil {
		return out
	}
	for name := range names {
		if size, ok := sqliteBtreeBytes(ctx, q, name); ok {
			out[name] = size
		}
	}
	return out
}

// explainPlan runs EXPLAIN and returns a rendered form for display plus, where
// the engine answers in a table, the parsed cells. Classification reads the
// cells, so a verdict comes from the actual key/type/Extra fields instead of
// substring-matching the rendered row — that row also carries possible_keys
// and select_type, and matching those reports the wrong index.
//
// No column layout is assumed: SQLite returns four fixed columns, MySQL a
// dozen that vary by version, and EXPLAIN ANALYZE a single text blob.
func explainPlan(ctx context.Context, db *sql.DB, d dialect, query string, args []any, analyze bool) ([]string, []map[string]string, error) {
	if d != dialectMySQL {
		return renderPlan(ctx, db, "EXPLAIN QUERY PLAN "+query, args)
	}
	if analyze {
		return renderPlan(ctx, db, "EXPLAIN ANALYZE "+query, args)
	}
	// FORMAT=TRADITIONAL pins the tabular plan whatever the server's
	// explain_format is set to. It matters more than it looks: on a server
	// configured for TREE (MySQL 8.3+ made that settable) a plain EXPLAIN
	// answers with one text column, and then there is no `key`, no `Extra` and
	// no `possible_keys` to read — so doctor reported "no index" for a table
	// scan and never saw a covering read at all. MariaDB does not know the
	// keyword, hence the fallback.
	plan, cells, err := renderPlan(ctx, db, "EXPLAIN FORMAT=TRADITIONAL "+query, args)
	if err == nil {
		return plan, cells, nil
	}
	return renderPlan(ctx, db, "EXPLAIN "+query, args)
}

// renderPlan runs one already-prefixed EXPLAIN and returns it both as text and,
// where the server answered in columns, as parsed cells.
func renderPlan(ctx context.Context, db *sql.DB, stmt string, args []any) ([]string, []map[string]string, error) {
	rows, err := db.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		return nil, nil, err
	}
	var out []string
	var parsed []map[string]string
	for rows.Next() {
		cells := make([]sql.NullString, len(columns))
		targets := make([]any, len(columns))
		for i := range cells {
			targets[i] = &cells[i]
		}
		if err := rows.Scan(targets...); err != nil {
			return nil, nil, err
		}
		// SQLite's plan is four columns of which one is meaningful; printing it
		// bare reads far better than labelling it.
		if len(columns) == 4 && columns[0] == "id" && columns[3] == "detail" {
			out = append(out, cells[3].String)
			continue
		}
		// A tree plan (EXPLAIN ANALYZE, or a server set to explain_format=TREE)
		// is one multi-line blob; keep its own line breaks and leave the cells
		// empty so classification falls back to reading the text.
		if len(columns) == 1 {
			out = append(out, strings.Split(strings.TrimRight(cells[0].String, "\n"), "\n")...)
			continue
		}
		row := make(map[string]string, len(columns))
		var parts []string
		for i, c := range cells {
			if !c.Valid || c.String == "" {
				continue
			}
			row[columns[i]] = c.String
			parts = append(parts, columns[i]+"="+c.String)
		}
		parsed = append(parsed, row)
		out = append(out, strings.Join(parts, " "))
	}
	return out, parsed, rows.Err()
}

// mysqlExtraHas reports whether MySQL's semicolon-separated Extra column holds
// a note exactly. Substring matching cannot tell "Using index" (a covering
// read) from "Using index condition", which still fetches rows.
func mysqlExtraHas(extra, note string) bool {
	for _, part := range strings.Split(extra, ";") {
		if strings.TrimSpace(part) == note {
			return true
		}
	}
	return false
}

// classifyPlan pulls the two facts that decide whether a page is healthy out of
// a rendered plan: which of the table's indexes the planner chose, and whether
// it is reading table rows anyway.
//
// It reads the parsed cells where the server answered in columns, and falls back
// to the text otherwise. The fallback is not a rare path — EXPLAIN ANALYZE is
// always a tree, and so is a plain EXPLAIN on a server whose explain_format
// says so — so it has to know the tree vocabulary as well as the tabular one.
//
// Every judgement is scoped to the alias of the big table the query drives
// from, because these plans also touch things whose scans are harmless — a
// materialised subquery ("SCAN latest"), or the 90-row stations table
// ("SCAN s"). Reading the plan as one blob reports those as table scans.
func classifyPlan(plan []string, cells []map[string]string, d dialect, indexNames []string, alias string) (uses string, covering bool, fullScan bool) {
	// MySQL answers in a table, so read the fields rather than the text. `key`
	// is the index it committed to; `possible_keys` is only what it considered
	// and must never be reported as the choice.
	if d == dialectMySQL && len(cells) > 0 {
		for _, row := range cells {
			if alias != "" && row["table"] != alias {
				continue
			}
			if uses == "" {
				uses = row["key"]
			}
			if mysqlExtraHas(row["Extra"], "Using index") {
				covering = true
			}
			if row["type"] == "ALL" {
				fullScan = true
			}
		}
		return uses, covering, fullScan
	}

	for _, line := range plan {
		if !planLineTouches(line, d, alias) {
			continue
		}
		for _, name := range indexNames {
			if uses == "" && strings.Contains(line, name) {
				uses = name
			}
		}
		if d == dialectMySQL {
			// No cells to read, so the verdict comes out of the text. Two
			// vocabularies land here and both have to be understood: a rendered
			// traditional row ("type=ALL", "Extra=Using index") and a tree plan
			// from EXPLAIN ANALYZE or a server set to explain_format=TREE, which
			// says none of those words. Teaching this branch only the
			// traditional ones is what made every tree plan read as "no index".
			switch {
			case strings.Contains(line, "Covering index"):
				covering = true
			case strings.Contains(line, "Using index") && !strings.Contains(line, "Using index condition"):
				covering = true
			}
			if strings.Contains(line, "type=ALL") || strings.Contains(line, "Table scan on ") {
				fullScan = true
			}
			continue
		}
		if strings.Contains(line, "COVERING INDEX") {
			covering = true
		}
		// "SCAN pp" with no USING clause is a bare table scan.
		if strings.HasPrefix(line, "SCAN ") && !strings.Contains(line, "USING") {
			fullScan = true
		}
	}
	return uses, covering, fullScan
}

// planLineTouches reports whether a plan line concerns the given table alias.
func planLineTouches(line string, d dialect, alias string) bool {
	if alias == "" {
		return true
	}
	// A rendered classic-EXPLAIN row names its table in a field. EXPLAIN
	// ANALYZE's tree form has no such field and writes "... on pp using ...",
	// so fall through to word matching there.
	if d == dialectMySQL && strings.Contains(line, "table=") {
		return strings.Contains(line, "table="+alias+" ") || strings.HasSuffix(line, "table="+alias)
	}
	// SQLite writes "SEARCH pp USING ..." / "SCAN pp", so the alias appears as
	// its own word. Matching on the whole word keeps "pp" from also matching a
	// column or index name that merely contains it.
	for _, field := range strings.Fields(line) {
		if field == alias {
			return true
		}
	}
	return false
}

// runDoctorChecks executes the read-only inspection and returns the report.
func runDoctorChecks(ctx context.Context, db *sql.DB, d dialect, target string, opts doctorOptions) (doctorResult, error) {
	result := doctorResult{
		Database: doctorDatabase{Target: target, Driver: string(d), Version: serverVersion(ctx, db, d)},
		Filters:  opts.Filters,
	}

	// Sizes are read over one pinned connection so MySQL's cached
	// information_schema statistics cannot make them stale (mysqlStatsConn).
	stats := queryer(db)
	if d == dialectMySQL {
		if conn, connErr := mysqlStatsConn(ctx, db); connErr == nil {
			defer conn.Close()
			stats = conn
		}
	}

	tables, err := doctorTableStats(ctx, stats, d)
	if err != nil {
		return doctorResult{}, err
	}
	result.Tables = tables

	indexes, err := doctorIndexReport(ctx, db, d)
	if err != nil {
		return doctorResult{}, err
	}
	result.Indexes = indexes

	// Candidate index names per table, so a query's plan is judged against the
	// indexes of the table it actually drives from.
	indexesByTable := map[string][]string{}
	for _, idx := range indexes {
		if idx.Present {
			indexesByTable[idx.Table] = append(indexesByTable[idx.Table], idx.Name)
		}
	}

	hasDecisions := false
	for _, t := range tables {
		if t.Name == "price_check_decisions" && !t.Missing {
			hasDecisions = true
		}
	}
	accuracyIndexPresent := false
	for _, idx := range indexes {
		if idx.Name == "idx_price_predictions_accuracy" && idx.Present {
			accuracyIndexPresent = true
		}
	}
	hasPredictions := false
	for _, t := range tables {
		if t.Name == "price_predictions" && !t.Missing {
			hasPredictions = true
		}
	}

	present := map[string]bool{}
	for _, t := range tables {
		present[t.Name] = !t.Missing
	}
	result.Scope = doctorScopeReport(ctx, db, d, opts, present, time.Now().UTC())

	// Without the table there is nothing to time, and running the queries anyway
	// would report the same missing-table error eight times over the one finding
	// that matters.
	if opts.Pages.Accuracy && !opts.SkipQueries && !hasPredictions {
		result.Queries = append(result.Queries, doctorQuery{
			Name:    "accuracy_page",
			Purpose: "skipped: price_predictions does not exist — run `gasoline migrate`",
			Skipped: true,
		})
	} else if opts.Pages.Accuracy && !opts.SkipQueries {
		qc := accuracyQueryContext{
			Filters:      opts.Filters,
			Dialect:      d,
			HasDecisions: hasDecisions,
			// Faithfulness matters more than optimism here: if the index is
			// missing, the page emits no hint, so neither does doctor.
			AccuracyIndexPresent: accuracyIndexPresent,
		}
		hinted := map[string]accuracyQuerySpec{}
		if opts.TryIndex != "" {
			forced := qc
			forced.ForceIndex = opts.TryIndex
			for _, spec := range accuracyQuerySpecsFor(forced) {
				hinted[spec.name] = spec
			}
		}
		for _, spec := range accuracyQuerySpecsFor(qc) {
			q := doctorQuery{Name: spec.name, Purpose: spec.purpose, SQL: spec.sql}

			q.Table = spec.table
			plan, cells, err := explainPlan(ctx, db, d, spec.sql, spec.args, opts.Analyze)
			if err != nil {
				q.Error = err.Error()
			} else {
				q.Plan = plan
				q.UsesIndex, q.CoveringHit, q.FullScan = classifyPlan(plan, cells, d, indexesByTable[spec.table], spec.alias)
				q.Considered = consideredIndexes(cells, spec.alias, q.UsesIndex)
			}

			started := time.Now()
			count, err := countQueryRows(ctx, db, spec.sql, spec.args)
			q.DurationMS = float64(time.Since(started).Microseconds()) / 1000
			if err != nil {
				q.Error = err.Error()
			}
			q.Rows = count

			// Only price_predictions queries can be steered by the hint; the
			// decisions query reads a different table.
			if alt, ok := hinted[spec.name]; ok && spec.table == "price_predictions" {
				q.Hinted = measureHinted(ctx, db, d, alt, opts, indexesByTable[spec.table])
			}
			result.Queries = append(result.Queries, q)
		}
	} else if opts.Pages.Accuracy {
		result.Queries = append(result.Queries, doctorQuery{
			Name:    "accuracy_page",
			Purpose: "skipped via --skip-queries",
			Skipped: true,
		})
	}

	if opts.Pages.Dashboard {
		result.Dashboard = runDashboardChecks(ctx, db, d, opts, present, indexesByTable, result.Scope, time.Now().UTC())
	}

	// Last, and only on request: this is the one step that writes, so everything
	// reported above it describes the database as the operator found it.
	if opts.Optimize {
		result.Optimize = runDoctorOptimize(ctx, db, stats, d, opts, tables)
	}

	result.Findings = doctorFindings(result, d, opts)
	return result, nil
}

// doctorScopeProbeLimit bounds how many out-of-scope stations are looked up in
// price_predictions. One lookup is a single index seek, but a database that has
// collected many cities over time could hold thousands of dropped stations, and
// a diagnostic must not turn into the slowest thing on the box.
const doctorScopeProbeLimit = 500

// stationScopeRow is one station's newest snapshot: the city that owns it and
// when it was last fed.
type stationScopeRow struct {
	StationID  string
	City       string
	RecordedAt time.Time
}

// doctorScopeReport builds the scope picture. It is cheap by construction: one
// indexed seek per station for the snapshots, one run row, one pass over that
// run's predictions, and a capped number of seeks for dropped stations. Nothing
// here scans price_predictions.
func doctorScopeReport(ctx context.Context, db *sql.DB, d dialect, opts doctorOptions, present map[string]bool, now time.Time) doctorScope {
	scope := doctorScope{
		FreshnessHours: stationFreshness.Hours(),
		RetentionDays:  predictionRetentionDays,
	}
	if !present["stations"] || !present["price_snapshots"] {
		scope.Skipped = true
		scope.Reason = "stations and price_snapshots are needed to tell what is in scope — run `gasoline migrate`"
		return scope
	}

	hasTargets, err := tableExists(ctx, db, d, "update_targets")
	if err != nil {
		scope.Skipped = true
		scope.Reason = err.Error()
		return scope
	}
	if hasTargets {
		targets, err := doctorScopeTargets(ctx, db)
		if err != nil {
			scope.Skipped = true
			scope.Reason = err.Error()
			return scope
		}
		scope.Targets = targets
		scope.Configured = len(targets) > 0
	}

	stations, err := loadStationScope(ctx, db)
	if err != nil {
		scope.Skipped = true
		scope.Reason = err.Error()
		return scope
	}

	// The newest run is the pipeline's own answer to "what is in scope", which
	// is worth comparing against the snapshots' answer.
	runStations := map[string]bool{}
	if present["prediction_runs"] && present["price_predictions"] {
		run, ids, err := loadLatestRunStations(ctx, db)
		if err != nil {
			scope.Skipped = true
			scope.Reason = err.Error()
			return scope
		}
		scope.LatestRun = run
		runStations = ids
	}

	cities := map[string]*doctorScopeCity{}
	cityFor := func(name string) *doctorScopeCity {
		city, ok := cities[name]
		if !ok {
			city = &doctorScopeCity{City: name}
			cities[name] = city
		}
		return city
	}
	// Targets come first so one that owns nothing at all still gets a row: a
	// target feeding no station is precisely the kind of thing to notice.
	for _, target := range scope.Targets {
		name := target.Normalized
		if name == "" {
			name = target.City
		}
		city := cityFor(name)
		city.Target = true
		city.Geocoded = target.Geocoded
		if target.RadiusKM > city.RadiusKM {
			city.RadiusKM = target.RadiusKM
		}
	}

	freshCutoff := now.Add(-stationFreshness)
	retentionCutoff := now.AddDate(0, 0, -predictionRetentionDays)
	var dropped []stationScopeRow
	for _, station := range stations {
		city := cityFor(station.City)
		city.Stations++
		if station.RecordedAt.After(city.newestSnapshot()) {
			city.NewestSnapshot = station.RecordedAt.UTC().Format(time.RFC3339)
		}
		inScope := !station.RecordedAt.Before(freshCutoff)
		if inScope {
			city.InScope++
		}
		if runStations[station.StationID] {
			city.InLatestRun++
			if !inScope && scope.LatestRun != nil {
				scope.LatestRun.OutOfScope++
			}
		}
		// Only stations that left scope need their stored predictions looked
		// up: for anything still fed, "there are predictions" is the expected
		// state and says nothing.
		if !inScope && station.RecordedAt.After(retentionCutoff) {
			dropped = append(dropped, station)
		}
	}

	if present["price_predictions"] {
		sort.Slice(dropped, func(i, j int) bool {
			return dropped[i].RecordedAt.After(dropped[j].RecordedAt)
		})
		if len(dropped) > doctorScopeProbeLimit {
			dropped = dropped[:doctorScopeProbeLimit]
			scope.ProbeLimited = true
		}
		for _, station := range dropped {
			target, err := newestPredictionTarget(ctx, db, station.StationID, opts.Filters.Fuel)
			if err != nil {
				scope.Reason = err.Error()
				break
			}
			city := cityFor(station.City)
			if target > city.NewestPrediction {
				city.NewestPrediction = target
			}
		}
	}

	scope.Cities = make([]doctorScopeCity, 0, len(cities))
	for _, city := range cities {
		scope.Cities = append(scope.Cities, *city)
	}
	sort.Slice(scope.Cities, func(i, j int) bool {
		return scope.Cities[i].City < scope.Cities[j].City
	})
	return scope
}

// newestSnapshot re-reads the stored timestamp so the running maximum needs no
// second field. An unparsable value would have failed loadStationScope, and an
// empty one is older than anything.
func (c *doctorScopeCity) newestSnapshot() time.Time {
	if c.NewestSnapshot == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339, c.NewestSnapshot)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

// doctorScopeTargets reads update_targets and resolves each city the way the
// update sweep does, so the names line up with the owners snapshots record.
func doctorScopeTargets(ctx context.Context, db *sql.DB) ([]doctorScopeTarget, error) {
	rows, err := db.QueryContext(ctx, `SELECT city, radius_km FROM update_targets ORDER BY city ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var targets []doctorScopeTarget
	for rows.Next() {
		var target doctorScopeTarget
		if err := rows.Scan(&target.City, &target.RadiusKM); err != nil {
			return nil, err
		}
		targets = append(targets, target)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Resolved after the result set is closed: the MySQL driver speaks to one
	// connection synchronously, so querying while these rows are open would
	// fail the whole report.
	for i, target := range targets {
		normalized, _, _, found, err := cachedCityFor(ctx, db, target.City)
		if err != nil {
			return nil, err
		}
		targets[i].Normalized = normalized
		targets[i].Geocoded = found
	}
	return targets, nil
}

// loadStationScope reads the newest snapshot of every station. The owner is
// taken from that one row, which is how loadSnapshotScan attributes a station
// too: ownership can move to a nearer centre, and the last row wins.
func loadStationScope(ctx context.Context, db *sql.DB) ([]stationScopeRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT s.id, ps.city_name, ps.recorded_at
		FROM stations s
		JOIN price_snapshots ps ON ps.id = (
			SELECT newest.id
			FROM price_snapshots newest
			WHERE newest.station_id = s.id
			ORDER BY newest.recorded_at DESC, newest.id DESC
			LIMIT 1
		)
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var scope []stationScopeRow
	for rows.Next() {
		var row stationScopeRow
		var recordedAt string
		if err := rows.Scan(&row.StationID, &row.City, &recordedAt); err != nil {
			return nil, err
		}
		parsed, err := time.Parse(time.RFC3339, recordedAt)
		if err != nil {
			return nil, fmt.Errorf("parse recorded_at %q: %w", recordedAt, err)
		}
		row.RecordedAt = parsed
		scope = append(scope, row)
	}
	return scope, rows.Err()
}

// loadLatestRunStations reads the newest prediction run and the stations it
// stored predictions for.
func loadLatestRunStations(ctx context.Context, db *sql.DB) (*doctorScopeRun, map[string]bool, error) {
	var run doctorScopeRun
	err := db.QueryRowContext(ctx, `
		SELECT id, run_at, fuel, station_count
		FROM prediction_runs
		ORDER BY run_at DESC, id DESC
		LIMIT 1
	`).Scan(&run.ID, &run.RunAt, &run.Fuel, &run.StationCount)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT station_id FROM price_predictions WHERE run_id = ?`, run.ID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	ids := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, nil, err
		}
		ids[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	run.Stations = len(ids)
	return &run, ids, nil
}

// newestPredictionTarget returns the latest target window stored for one
// station and fuel, or "" when there is none. Reading it in target order off
// idx_price_predictions_station_fuel_target makes this a single seek rather
// than an aggregate over the station's whole prediction history.
func newestPredictionTarget(ctx context.Context, db *sql.DB, stationID, fuel string) (string, error) {
	var target string
	err := db.QueryRowContext(ctx, `
		SELECT target_start
		FROM price_predictions
		WHERE station_id = ? AND fuel = ?
		ORDER BY target_start DESC
		LIMIT 1
	`, stationID, fuel).Scan(&target)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return target, nil
}

// runDoctorOptimize rebuilds the requested tables and measures what came back.
//
// It runs last, after every measurement, so the sizes and query timings above it
// describe the database an operator actually complained about rather than the one
// doctor just rewrote.
//
// Cost warning worth knowing before running it: both engines rebuild by writing
// a fresh copy, so each needs free space on the order of the object being
// rebuilt — one table for MySQL, the whole database for SQLite. MySQL performs
// it online (concurrent reads and writes keep working); SQLite's VACUUM takes an
// exclusive lock for its duration.
func runDoctorOptimize(ctx context.Context, db *sql.DB, stats queryer, d dialect, opts doctorOptions, before []doctorTable) *doctorOptimize {
	report := &doctorOptimize{}
	sizes := map[string]doctorTable{}
	for _, table := range before {
		sizes[table.Name] = table
	}

	if d != dialectMySQL {
		// VACUUM is whole-database and cannot be pointed at one table, so a
		// narrowing flag would be a promise the engine cannot keep.
		report.Statement = "VACUUM"
		if len(opts.OptimizeTables) > 0 {
			report.Reason = "SQLite reclaims space with VACUUM, which rewrites the whole database; --optimize-table cannot narrow it"
		}
		entry := doctorOptimizeTable{Name: "database"}
		entry.DataBytesBefore = fileSizeBytes(opts.SQLitePath)
		report.FileBytesBefore = entry.DataBytesBefore
		fmt.Fprintf(os.Stderr, "optimize: VACUUM %s\n", opts.SQLitePath)
		started := time.Now()
		if _, err := db.ExecContext(ctx, "VACUUM"); err != nil {
			entry.Error = err.Error()
		}
		entry.DurationMS = float64(time.Since(started).Microseconds()) / 1000
		if entry.Error == "" {
			// VACUUM rebuilds the b-trees but leaves the statistics alone, and
			// doctor's whole purpose is telling an operator why the planner
			// chose what it chose.
			if _, err := db.ExecContext(ctx, "ANALYZE"); err != nil {
				entry.Notes = append(entry.Notes, "ANALYZE failed: "+err.Error())
			} else {
				report.Statement = "VACUUM; ANALYZE"
			}
		}
		entry.DataBytesAfter = fileSizeBytes(opts.SQLitePath)
		report.FileBytesAfter = entry.DataBytesAfter
		report.Tables = append(report.Tables, entry)
		return report
	}

	report.Statement = "OPTIMIZE TABLE <table>"
	targets := opts.OptimizeTables
	if len(targets) == 0 {
		targets = doctorTables
	}
	for _, name := range targets {
		table, known := sizes[name]
		if !known {
			// Only reachable if doctorTables and the stats disagree; treated as
			// absent rather than guessed at.
			report.Tables = append(report.Tables, doctorOptimizeTable{
				Name:  name,
				Error: "not reported by this database",
			})
			continue
		}
		if table.Missing {
			report.Tables = append(report.Tables, doctorOptimizeTable{
				Name:  name,
				Error: "table does not exist",
			})
			continue
		}
		entry := doctorOptimizeTable{
			Name:             name,
			DataBytesBefore:  table.DataBytes,
			IndexBytesBefore: table.IndexBytes,
		}
		// A rebuild of a multi-gigabyte table takes minutes, and a CLI that
		// prints nothing for that long reads as hung. Progress goes to stderr so
		// --output json stays a single document.
		fmt.Fprintf(os.Stderr, "optimize: rebuilding %s (%s data, %s indexes)\n",
			name, formatBytesPtr(table.DataBytes), formatBytesPtr(table.IndexBytes))
		started := time.Now()
		notes, err := optimizeMySQLTable(ctx, db, name)
		entry.DurationMS = float64(time.Since(started).Microseconds()) / 1000
		entry.Notes = notes
		if err != nil {
			entry.Error = err.Error()
		}
		report.Tables = append(report.Tables, entry)
	}

	// Re-read the sizes once, after every rebuild: information_schema is the
	// same source the tables section used, so before and after are comparable.
	after, err := doctorTableStats(ctx, stats, d)
	if err != nil {
		report.Reason = "sizes after the rebuild could not be read: " + err.Error()
		return report
	}
	afterByName := map[string]doctorTable{}
	for _, table := range after {
		afterByName[table.Name] = table
	}
	for i, entry := range report.Tables {
		table, ok := afterByName[entry.Name]
		if !ok || table.Missing {
			continue
		}
		report.Tables[i].DataBytesAfter = table.DataBytes
		report.Tables[i].IndexBytesAfter = table.IndexBytes
	}
	return report
}

// optimizeMySQLTable runs OPTIMIZE TABLE and collects the engine's own report
// rows. The statement answers with a result set rather than a plain OK, and
// InnoDB's "recreate + analyze instead" line arrives as one of those rows, so it
// has to be drained and kept.
//
// The name is interpolated because a table name cannot be a bound parameter; it
// is validated against doctorTables in runDoctor before it reaches here.
func optimizeMySQLTable(ctx context.Context, db *sql.DB, name string) ([]string, error) {
	rows, err := db.QueryContext(ctx, "OPTIMIZE TABLE "+name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var notes []string
	for rows.Next() {
		cells := make([]sql.NullString, len(columns))
		targets := make([]any, len(columns))
		for i := range cells {
			targets[i] = &cells[i]
		}
		if err := rows.Scan(targets...); err != nil {
			return notes, err
		}
		// Msg_type / Msg_text is the pair worth keeping; "status: OK" is the
		// uninteresting normal case and would only pad the output.
		var msgType, msgText string
		for i, column := range columns {
			switch column {
			case "Msg_type":
				msgType = cells[i].String
			case "Msg_text":
				msgText = cells[i].String
			}
		}
		if msgType == "status" && strings.EqualFold(msgText, "OK") {
			continue
		}
		if msgType != "" || msgText != "" {
			notes = append(notes, strings.TrimSpace(msgType+": "+msgText))
		}
	}
	return notes, rows.Err()
}

// fileSizeBytes is the database file's size, or nil when it cannot be measured
// (MySQL, or a path the process cannot stat).
func fileSizeBytes(path string) *int64 {
	if path == "" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	size := info.Size()
	return &size
}

// doctorOptimizeFindings reports what the rebuild bought, in the terms that
// prompted it: bytes handed back, and anything that refused to rebuild.
func doctorOptimizeFindings(o *doctorOptimize) []doctorFinding {
	if o == nil {
		return nil
	}
	var findings []doctorFinding
	warn := func(format string, a ...any) {
		findings = append(findings, doctorFinding{Severity: "warn", Message: fmt.Sprintf(format, a...)})
	}
	info := func(format string, a ...any) {
		findings = append(findings, doctorFinding{Severity: "info", Message: fmt.Sprintf(format, a...)})
	}

	if o.Skipped {
		info("optimize skipped: %s", o.Reason)
		return findings
	}
	if o.Reason != "" {
		info("optimize: %s", o.Reason)
	}
	var reclaimed int64
	var rebuilt int
	var measured bool
	for _, table := range o.Tables {
		if table.Error != "" {
			warn("optimize %s failed after %.0f ms: %s", table.Name, table.DurationMS, table.Error)
			continue
		}
		rebuilt++
		if delta, ok := optimizeReclaimed(table); ok {
			measured = true
			reclaimed += delta
		}
	}
	switch {
	case rebuilt == 0:
	case !measured:
		info("optimize rebuilt %d %s; this database does not report sizes, so the space returned cannot be measured",
			rebuilt, plural(rebuilt, "table", "tables"))
	case reclaimed > 0:
		info("optimize returned %s to the filesystem across %d %s", formatBytes(reclaimed), rebuilt,
			plural(rebuilt, "table", "tables"))
	case reclaimed == 0:
		info("optimize rebuilt %d %s and returned nothing: there was no free space held back",
			rebuilt, plural(rebuilt, "table", "tables"))
	default:
		// A rebuild can end up larger — a freshly built index is denser but
		// fill-factor and statistics both move — and reporting that as a
		// negative reclaim is more honest than hiding it.
		info("optimize rebuilt %d %s and the database grew by %s", rebuilt,
			plural(rebuilt, "table", "tables"), formatBytes(-reclaimed))
	}
	return findings
}

// optimizeReclaimed is how many bytes one rebuild gave back, and whether both
// sides of it were measurable at all.
func optimizeReclaimed(table doctorOptimizeTable) (int64, bool) {
	var delta int64
	var measured bool
	for _, pair := range [2][2]*int64{
		{table.DataBytesBefore, table.DataBytesAfter},
		{table.IndexBytesBefore, table.IndexBytesAfter},
	} {
		if pair[0] == nil || pair[1] == nil {
			continue
		}
		measured = true
		delta += *pair[0] - *pair[1]
	}
	return delta, measured
}

// writeDoctorOptimizeText prints the rebuild: what it cost and what it returned.
func writeDoctorOptimizeText(o *doctorOptimize) {
	if o == nil {
		return
	}
	fmt.Fprintf(stdout, "\noptimize (%s):\n", o.Statement)
	if o.Skipped {
		fmt.Fprintf(stdout, "  skipped: %s\n", o.Reason)
		return
	}
	if o.Reason != "" {
		fmt.Fprintf(stdout, "  note: %s\n", o.Reason)
	}
	// On SQLite the single entry measures the database file, not a table's data
	// pages, and calling that "data" would misname the one number that matters.
	sizeLabel := "data"
	if o.FileBytesBefore != nil || o.FileBytesAfter != nil {
		sizeLabel = "file"
	}
	for _, table := range o.Tables {
		// Assembled rather than printed field by field: the size columns are
		// optional, so the padding of whichever one comes last would otherwise
		// trail off the end of the line.
		line := fmt.Sprintf("  %-24s %9s", table.Name, formatDurationMS(table.DurationMS))
		if table.Error != "" {
			fmt.Fprintf(stdout, "%s  failed: %s\n", line, table.Error)
			continue
		}
		if table.DataBytesBefore != nil && table.DataBytesAfter != nil {
			line += fmt.Sprintf("  %s %8s -> %-8s", sizeLabel, formatBytesPtr(table.DataBytesBefore), formatBytesPtr(table.DataBytesAfter))
		}
		if table.IndexBytesBefore != nil && table.IndexBytesAfter != nil {
			line += fmt.Sprintf("  indexes %8s -> %-8s", formatBytesPtr(table.IndexBytesBefore), formatBytesPtr(table.IndexBytesAfter))
		}
		fmt.Fprintln(stdout, strings.TrimRight(line, " "))
		for _, note := range table.Notes {
			fmt.Fprintf(stdout, "      | %s\n", note)
		}
	}
}

// formatDurationMS keeps a rebuild's cost readable at both ends of its range: a
// vacuum of an empty database takes milliseconds, a multi-gigabyte table takes
// minutes, and "0.0 s" for the first one looks like nothing ran.
func formatDurationMS(ms float64) string {
	switch {
	case ms < 1000:
		return fmt.Sprintf("%.0f ms", ms)
	case ms < 60_000:
		return fmt.Sprintf("%.1f s", ms/1000)
	default:
		return fmt.Sprintf("%.1f min", ms/60_000)
	}
}

// formatBytesPtr renders an optional size, so a database that cannot report one
// prints a dash instead of a zero that reads as "empty".
func formatBytesPtr(bytes *int64) string {
	if bytes == nil {
		return "-"
	}
	return formatBytes(*bytes)
}

// countQueryRows runs a query for its cost and drains it, discarding values:
// doctor measures how long the page's SQL takes, not what it returns.
func countQueryRows(ctx context.Context, db *sql.DB, query string, args []any) (int, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		count++
	}
	return count, rows.Err()
}

// doctorFindings turns the raw report into the short actionable list an
// operator actually reads first.
func doctorFindings(r doctorResult, d dialect, opts doctorOptions) []doctorFinding {
	var findings []doctorFinding
	warn := func(format string, a ...any) {
		findings = append(findings, doctorFinding{Severity: "warn", Message: fmt.Sprintf(format, a...)})
	}
	info := func(format string, a ...any) {
		findings = append(findings, doctorFinding{Severity: "info", Message: fmt.Sprintf(format, a...)})
	}

	for _, idx := range r.Indexes {
		if !idx.Present {
			warn("index %s on %s is missing — run `gasoline migrate`", idx.Name, idx.Table)
		}
	}

	findings = append(findings, doctorScopeFindings(r.Scope)...)
	findings = append(findings, doctorDashboardFindings(r.Dashboard, r.Tables, opts)...)
	findings = append(findings, doctorOptimizeFindings(r.Optimize)...)

	// scanWorthWarningAbout keeps a full scan from being reported as a problem
	// when the table is small enough that scanning it is the right plan. On a
	// few thousand rows the optimizer routinely prefers a scan to an index, and
	// warning about it buries the findings that matter.
	const scanRowsThreshold = 100_000
	rowsByTable := map[string]int64{}
	for _, t := range r.Tables {
		rowsByTable[t.Name] = t.Rows
	}

	var slowest doctorQuery
	var total float64
	for _, q := range r.Queries {
		if q.Skipped {
			continue
		}
		total += q.DurationMS
		if q.DurationMS > slowest.DurationMS {
			slowest = q
		}
		if q.Error != "" {
			warn("query %s failed: %s", q.Name, q.Error)
			continue
		}
		if q.DurationMS >= opts.SlowMS {
			warn("query %s took %.0f ms (%s)", q.Name, q.DurationMS, q.Purpose)
		}
		switch {
		case q.FullScan && (rowsByTable[q.Table] >= scanRowsThreshold || q.DurationMS >= opts.SlowMS):
			warn("query %s scans %s (%s rows) instead of using an index",
				q.Name, q.Table, formatCount(rowsByTable[q.Table]))
		case q.FullScan:
			info("query %s scans %s, which is small enough for that to be reasonable", q.Name, q.Table)
		case q.UsesIndex == "":
			info("query %s uses no %s index (plan: %s)", q.Name, q.Table, strings.Join(q.Plan, " | "))
		case !q.CoveringHit:
			info("query %s uses %s but still reads table rows", q.Name, q.UsesIndex)
		}
		// An index the planner weighed and passed over, on a query that is not
		// covering, is worth naming — but only as something to measure. On the
		// live database this pattern survived both ANALYZE TABLE and a
		// histogram on target_start, so pointing at stale statistics would have
		// been a confident wrong answer; --try-index settles it instead.
		if q.UsesIndex != "" && !q.CoveringHit && q.Hinted == nil {
			for _, candidate := range q.Considered {
				if candidate == "idx_price_predictions_accuracy" {
					info("query %s chose %s over %s, which covers it — measure the difference "+
						"with `--try-index %s`", q.Name, q.UsesIndex, candidate, candidate)
				}
			}
		}
	}
	if total > 0 {
		info("accuracy page SQL total %.0f ms; slowest %s at %.0f ms", total, slowest.Name, slowest.DurationMS)
	}

	// --try-index only pays off if it produces a verdict, so state one.
	if opts.TryIndex != "" {
		var hintedTotal, comparable float64
		var mismatch bool
		for _, q := range r.Queries {
			h := q.Hinted
			if h == nil || h.Error != "" {
				continue
			}
			if h.Rows != q.Rows {
				mismatch = true
			}
			comparable += q.DurationMS
			hintedTotal += h.DurationMS
		}
		switch {
		case mismatch:
			warn("forcing %s changed a query's row count — the comparison is not measuring "+
				"the same work, so ignore its timings", opts.TryIndex)
		case comparable == 0:
			info("no query could be compared with %s forced", opts.TryIndex)
		case hintedTotal < comparable*0.8:
			warn("forcing %s would cut those queries from %.0f ms to %.0f ms (%.0f%% less) — "+
				"the optimizer is picking the slower index on its own",
				opts.TryIndex, comparable, hintedTotal, (comparable-hintedTotal)/comparable*100)
		case hintedTotal > comparable*1.2:
			info("forcing %s is slower (%.0f ms vs %.0f ms); the optimizer's choice is the better one",
				opts.TryIndex, hintedTotal, comparable)
		default:
			info("forcing %s makes little difference (%.0f ms vs %.0f ms)",
				opts.TryIndex, hintedTotal, comparable)
		}
	}

	for _, t := range r.Tables {
		if t.Missing {
			info("table %s does not exist yet", t.Name)
		}
	}
	if d == dialectMySQL {
		info("MySQL row counts are InnoDB estimates, not exact")
	}
	return findings
}

// doctorScopeFindings turns the scope picture into the two verdicts an operator
// needs: a city that should have left scope but has not, and a city that has
// left scope but whose stored rows are still on display.
func doctorScopeFindings(s doctorScope) []doctorFinding {
	var findings []doctorFinding
	warn := func(format string, a ...any) {
		findings = append(findings, doctorFinding{Severity: "warn", Message: fmt.Sprintf(format, a...)})
	}
	info := func(format string, a ...any) {
		findings = append(findings, doctorFinding{Severity: "info", Message: fmt.Sprintf(format, a...)})
	}

	if s.Skipped {
		info("scope check skipped: %s", s.Reason)
		return findings
	}
	if s.Reason != "" {
		warn("scope check incomplete: %s", s.Reason)
	}
	freshness := fmt.Sprintf("%.0fh", s.FreshnessHours)

	var inScope, total int
	for _, city := range s.Cities {
		inScope += city.InScope
		total += city.Stations
		switch {
		case city.Target && !city.Geocoded:
			warn("update target %s has never been geocoded, so no sweep has ever fetched it — check the geocoder and the spelling", city.City)
		case city.Target && city.Stations == 0:
			warn("update target %s owns no station; a sweep has never stored a snapshot for it", city.City)
		case city.Target && city.InScope == 0:
			warn("update target %s has had no price update since %s, so all %d of its %s left scope — `gasoline update` is not reaching it",
				city.City, city.NewestSnapshot, city.Stations, plural(city.Stations, "station has", "stations have"))
		case city.Target && city.InScope < city.Stations:
			info("update target %s owns %d stations but only %d are in scope; the rest moved to a nearer target or fell out of its radius",
				city.City, city.Stations, city.InScope)
		case !city.Target && city.InScope > 0 && s.Configured:
			warn("%s is not an update target, yet %d of its %d %s in scope (newest price update %s) — suggest, check and notify still cover it",
				city.City, city.InScope, city.Stations, plural(city.Stations, "station is", "stations are"), city.NewestSnapshot)
		case !city.Target && city.NewestPrediction != "":
			info("%s is no longer fed (newest price update %s) and none of its %d %s in scope; predictions stored up to %s are still there and the next `gasoline suggest --persist` run drops them",
				city.City, city.NewestSnapshot, city.Stations, plural(city.Stations, "station is", "stations are"),
				city.NewestPrediction)
		}
	}
	if total > 0 {
		info("scope: %d of %d stations in %d cities are in scope (fed within %s)", inScope, total, len(s.Cities), freshness)
	}
	if !s.Configured {
		info("no update targets are configured, so every city here was fed by an ad-hoc `update --city` run rather than by the schedule")
	}
	if run := s.LatestRun; run != nil {
		if run.OutOfScope > 0 {
			warn("the newest prediction run (%s, %s) covered %d stations that have had no price update within %s — that run drew from something other than the fed universe",
				run.RunAt, run.Fuel, run.OutOfScope, freshness)
		} else {
			info("the newest prediction run (%s, %s) covered %d stations, all of them in scope", run.RunAt, run.Fuel, run.Stations)
		}
	}
	if s.ProbeLimited {
		info("stopped looking up stored predictions after %d out-of-scope stations, newest first; the newest-prediction column is incomplete below that", doctorScopeProbeLimit)
	}
	return findings
}

// doctorPages selects which page's queries a run measures. Each page costs
// about what one of its loads costs, so this is opt-in per page rather than
// everything every time.
type doctorPages struct {
	Accuracy  bool
	Dashboard bool
}

type doctorOptions struct {
	// Pages is which page's queries to measure.
	Pages doctorPages
	// Dashboard is the request a dashboard load would have carried, used only
	// when Pages.Dashboard is set.
	Dashboard doctorDashboardFilters
	// DashboardNoCity reproduces the unscoped dashboard instead of letting
	// doctor pick the busiest city.
	DashboardNoCity bool
	// Probe runs the derived measurements that price the row lookups and the
	// rows an aggregate walks. Read-only, roughly one extra read per probed
	// query, and the numbers that decide whether an index would help.
	Probe   bool
	Filters doctorFilters
	// TryIndex names an index to force on price_predictions for a second,
	// side-by-side timing of every query. Read-only: it changes nothing but
	// what this one run measures.
	TryIndex    string
	SkipQueries bool
	Analyze     bool
	ShowSQL     bool
	SlowMS      float64
	// Optimize rebuilds the reported tables after everything else has been
	// measured. The only write doctor performs, and only on request.
	Optimize bool
	// OptimizeTables narrows the rebuild to these tables; empty means every
	// table doctor reports on. Validated against doctorTables before use,
	// because a table name cannot be a bound parameter.
	OptimizeTables []string
	// SQLitePath is the database file, needed to measure what VACUUM gave back.
	SQLitePath string
}

func nullInt64Ptr(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	value := v.Int64
	return &value
}

// resolveOptimizeTables validates --optimize-table against the tables doctor
// knows. A table name cannot be a bound parameter, so this whitelist is what
// keeps an arbitrary string out of the OPTIMIZE statement — and it also catches
// the ordinary typo before a long rebuild starts.
func resolveOptimizeTables(list string, optimize bool) ([]string, error) {
	list = strings.TrimSpace(list)
	if list == "" {
		return nil, nil
	}
	if !optimize {
		return nil, errors.New("--optimize-table requires --optimize")
	}
	var tables []string
	seen := map[string]bool{}
	for _, name := range strings.Split(list, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if !slices.Contains(doctorTables, name) {
			return nil, fmt.Errorf("--optimize-table %q is not one of: %s", name, strings.Join(doctorTables, ", "))
		}
		if !seen[name] {
			seen[name] = true
			tables = append(tables, name)
		}
	}
	if len(tables) == 0 {
		return nil, errors.New("--optimize-table needs at least one table name")
	}
	return tables, nil
}

// resolveDoctorPages reads the optional page subcommand. Bare `doctor` keeps
// measuring the accuracy page, which is what it has always done; the other
// pages have to be asked for by name.
func resolveDoctorPages(args []string) (doctorPages, []string, error) {
	pages := doctorPages{Accuracy: true}
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return pages, args, nil
	}
	switch args[0] {
	case "accuracy":
		return doctorPages{Accuracy: true}, args[1:], nil
	case "dashboard":
		return doctorPages{Dashboard: true}, args[1:], nil
	case "all":
		return doctorPages{Accuracy: true, Dashboard: true}, args[1:], nil
	}
	return doctorPages{}, nil, fmt.Errorf("unknown doctor subcommand %q: use accuracy, dashboard or all", args[0])
}

func runDoctor(args []string) error {
	pages, args, err := resolveDoctorPages(args)
	if err != nil {
		return err
	}

	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	dbf := addDBFlags(fs)
	fuelUsage := "Fuel the accuracy-page queries filter on: diesel, e5 or e10"
	fuelDefault := "diesel"
	if pages.Dashboard && !pages.Accuracy {
		fuelUsage = "Fuel filter the dashboard was loaded with: all (default), diesel, e5 or e10"
		fuelDefault = "all"
	}
	fuel := fs.String("fuel", fuelDefault, fuelUsage)
	confidence := fs.String("confidence", "all", "Confidence filter: all or medium_high")
	rangeUsage := "Target-date range: 7d, 14d (default) or 30d"
	if pages.Dashboard {
		rangeUsage = "Date range the page was loaded with: 7d, 14d or 30d (the dashboard defaults to 7d, the accuracy page to 14d)"
	}
	rangeName := fs.String("range", "", rangeUsage)
	from := fs.String("from", "", "Range start as YYYY-MM-DD (the accuracy page needs --to with it)")
	to := fs.String("to", "", "Range end as YYYY-MM-DD (the accuracy page needs --from with it)")
	skipQueries := fs.Bool("skip-queries", false, "Only report schema, sizes, indexes and scope; do not run the page queries")

	// Dashboard-only filters, registered only where they mean something so
	// `doctor accuracy --city` fails at the flag rather than being ignored.
	var city, station *string
	var radius *int
	var noCity, probe *bool
	if pages.Dashboard {
		city = fs.String("city", "", "City filter the dashboard was loaded with, as its normalized name; default is the city with the most stations in scope")
		noCity = fs.Bool("no-city", false, "Reproduce the unscoped dashboard, which loads the station list and skips the snapshot and prediction queries")
		radius = fs.Int("radius", dashboardRadiusOptions[0], "Radius in km around the city, one of the radii the dashboard offers: 5, 10 or 20")
		station = fs.String("station", "", "Station ids the dashboard's picker had selected (comma-separated); narrows the in-scope list the same way the page does")
		probe = fs.Bool("probe", true, "Also run the derived probe queries that price the row lookups and the rows an aggregate walks (read-only, roughly one extra read each)")
	}
	tryIndex := fs.String("try-index", "", "Also time every price_predictions query with this index forced, to measure what a different index choice would cost (read-only)")
	analyze := fs.Bool("analyze", false, "Use EXPLAIN ANALYZE for real per-step timings (MySQL 8.0.18+; ignored on SQLite)")
	explain := fs.Bool("explain", false, "Print the full query plan for each query")
	showSQL := fs.Bool("sql", false, "Print the SQL of each query")
	slowMS := fs.Float64("slow-ms", 1000, "Flag queries at or above this duration in milliseconds")
	optimize := fs.Bool("optimize", false, "Rebuild the reported tables afterwards to return freed space to the filesystem (OPTIMIZE TABLE on MySQL, VACUUM on SQLite). The only part of doctor that writes")
	optimizeTable := fs.String("optimize-table", "", "Restrict --optimize to these tables (comma-separated); MySQL only, since VACUUM rewrites the whole SQLite database")
	outputLong, outputShort := addOutputFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(fs.Args()) > 0 {
		return fmt.Errorf("doctor takes no positional arguments, got %q", fs.Args()[0])
	}
	dbCfg, err := resolveDBConfig(fs, dbf)
	if err != nil {
		return err
	}
	output, err := resolveOutputMode(*outputLong, *outputShort)
	if err != nil {
		return err
	}
	// Only the dashboard has an "all fuels" setting, and it is that page's
	// default; the accuracy page's queries each filter one fuel.
	dashboardOnly := pages.Dashboard && !pages.Accuracy
	if !isSuggestFuelType(*fuel) && !(dashboardOnly && *fuel == "all") {
		if pages.Dashboard && *fuel == "all" {
			return errors.New("--fuel all only works for `doctor dashboard`; the accuracy page filters one fuel, so name one")
		}
		if pages.Dashboard {
			return errors.New("--fuel must be one of: all, diesel, e5, e10")
		}
		return errors.New("--fuel must be one of: diesel, e5, e10")
	}
	if *confidence != "all" && *confidence != "medium_high" {
		return errors.New("--confidence must be one of: all, medium_high")
	}

	now := time.Now()
	var fromTS, toTS string
	if pages.Accuracy {
		fromTS, toTS, err = resolveDoctorRange(*rangeName, *from, *to, now)
		if err != nil {
			return err
		}
	}
	dashFilters := doctorDashboardFilters{Fuel: *fuel}
	if pages.Dashboard {
		dashFilters.From, dashFilters.To, err = resolveDashboardRange(*rangeName, *from, *to, now)
		if err != nil {
			return err
		}
		if *noCity && strings.TrimSpace(*city) != "" {
			return errors.New("--no-city cannot be combined with --city")
		}
		dashFilters.City = strings.TrimSpace(*city)
		dashFilters.Stations = parseStationList(*station)
		if dashFilters.RadiusKM, err = resolveDashboardRadius(*radius); err != nil {
			return err
		}
	}

	// --try-index steers the accuracy page's price_predictions queries and
	// nothing else, so accepting it where no such query runs would report a
	// comparison that never happened.
	if strings.TrimSpace(*tryIndex) != "" && !pages.Accuracy {
		return errors.New("--try-index measures the accuracy page's queries, so it needs `doctor`, `doctor accuracy` or `doctor all`")
	}

	optimizeTables, err := resolveOptimizeTables(*optimizeTable, *optimize)
	if err != nil {
		return err
	}

	// Opening a SQLite path creates the file, which every other command wants
	// and this one must not: doctor is a diagnostic, and conjuring an empty
	// database would both leave a stray file behind and answer "everything is
	// absent" when the truth is that there is no database at this path.
	sqlitePath := ""
	if dbCfg.Driver != dialectMySQL {
		sqlitePath = dbCfg.Path
	}
	if dbCfg.Driver != dialectMySQL {
		if _, statErr := os.Stat(dbCfg.Path); errors.Is(statErr, os.ErrNotExist) {
			return fmt.Errorf("no database at %s — pass --db, set GASOLINE_DB_PATH, "+
				"or select MySQL with --db-driver mysql", dbCfg.Path)
		} else if statErr != nil {
			return statErr
		}
	}

	ctx := context.Background()
	db, err := openDatabase(ctx, dbCfg)
	if err != nil {
		return err
	}
	defer db.Close()

	// Deliberately no initSchema: doctor is a read-only diagnostic, and
	// creating or migrating the schema underneath the operator would change
	// the very thing they are asking about.

	// The scope report reads the newest stored prediction per fuel, so it needs
	// a single one even on a dashboard run where the filter is "all fuels".
	scopeFuel := *fuel
	if !isSuggestFuelType(scopeFuel) {
		scopeFuel = "diesel"
	}
	opts := doctorOptions{
		Pages:           pages,
		Dashboard:       dashFilters,
		DashboardNoCity: noCity != nil && *noCity,
		Probe:           probe != nil && *probe,
		Filters: doctorFilters{
			Fuel:       scopeFuel,
			Confidence: *confidence,
			From:       fromTS,
			To:         toTS,
		},
		TryIndex:       strings.TrimSpace(*tryIndex),
		SkipQueries:    *skipQueries,
		Analyze:        *analyze,
		ShowSQL:        *showSQL,
		SlowMS:         *slowMS,
		Optimize:       *optimize,
		OptimizeTables: optimizeTables,
		SQLitePath:     sqlitePath,
	}
	result, err := runDoctorChecks(ctx, db, dbCfg.Driver, dbCfg.Description(), opts)
	if err != nil {
		return err
	}

	if output == outputJSON {
		return writeJSON(result)
	}
	writeDoctorText(result, opts, *explain)
	return nil
}

func writeDoctorText(r doctorResult, opts doctorOptions, explain bool) {
	fmt.Fprintf(stdout, "database: %s (%s %s)\n", r.Database.Target, r.Database.Driver, r.Database.Version)

	fmt.Fprintln(stdout, "\ntables:")
	for _, t := range r.Tables {
		if t.Missing {
			fmt.Fprintf(stdout, "  %-24s (absent)\n", t.Name)
			continue
		}
		rows := formatCount(t.Rows)
		if t.RowsApproximate {
			rows = "~" + rows
		}
		fmt.Fprintf(stdout, "  %-24s %14s rows", t.Name, rows)
		if t.DataBytes != nil {
			fmt.Fprintf(stdout, "  data %8s", formatBytes(*t.DataBytes))
		}
		if t.IndexBytes != nil {
			fmt.Fprintf(stdout, "  indexes %8s", formatBytes(*t.IndexBytes))
		}
		fmt.Fprintln(stdout)
	}

	fmt.Fprintln(stdout, "\nindexes:")
	currentTable := ""
	for _, idx := range r.Indexes {
		if idx.Table != currentTable {
			currentTable = idx.Table
			fmt.Fprintf(stdout, "  %s:\n", currentTable)
		}
		if !idx.Present {
			fmt.Fprintf(stdout, "    MISSING  %s\n", idx.Name)
			continue
		}
		size := ""
		if idx.Bytes != nil {
			size = formatBytes(*idx.Bytes)
		}
		marker := " "
		if idx.Unexpected {
			marker = "?"
		}
		fmt.Fprintf(stdout, "    %s %-46s %8s  (%s)\n", marker, idx.Name, size, strings.Join(idx.Columns, ", "))
	}

	writeDoctorScopeText(r.Scope)

	if opts.Pages.Accuracy {
		fmt.Fprintf(stdout, "\naccuracy page queries: fuel=%s, confidence=%s, %s .. %s\n",
			r.Filters.Fuel, r.Filters.Confidence, r.Filters.From, r.Filters.To)
	}
	for _, q := range r.Queries {
		if q.Skipped {
			fmt.Fprintf(stdout, "  %-16s %s\n", q.Name, q.Purpose)
			continue
		}
		fmt.Fprintf(stdout, "  %-16s %9.1f ms %6d rows  %s\n", q.Name, q.DurationMS, q.Rows, queryNote(q))
		if h := q.Hinted; h != nil {
			hintNote := h.UsesIndex
			switch {
			case h.Error != "":
				hintNote = "failed: " + h.Error
			case h.UsesIndex != "" && h.CoveringHit:
				hintNote = "covering " + h.UsesIndex
			case h.UsesIndex == "":
				hintNote = "hint not taken"
			}
			delta := ""
			if h.Error == "" && h.DurationMS > 0 {
				delta = fmt.Sprintf("  (%+.0f%%)", (h.DurationMS-q.DurationMS)/q.DurationMS*100)
			}
			fmt.Fprintf(stdout, "  %-16s %9.1f ms %6d rows  %s%s\n",
				"  forced:", h.DurationMS, h.Rows, hintNote, delta)
		}
		if opts.ShowSQL {
			fmt.Fprintf(stdout, "      sql: %s\n", q.SQL)
		}
		if explain {
			for _, line := range q.Plan {
				fmt.Fprintf(stdout, "      | %s\n", line)
			}
		}
	}

	writeDoctorDashboardText(r.Dashboard, opts, explain)
	writeDoctorOptimizeText(r.Optimize)

	fmt.Fprintln(stdout, "\nfindings:")
	if len(r.Findings) == 0 {
		fmt.Fprintln(stdout, "  none")
		return
	}
	for _, f := range r.Findings {
		fmt.Fprintf(stdout, "  %-4s %s\n", f.Severity, f.Message)
	}
}

// writeDoctorScopeText prints the station universe: one row per owning city,
// with the update target that is meant to feed it alongside what the data says
// it actually feeds.
func writeDoctorScopeText(s doctorScope) {
	fmt.Fprintf(stdout, "\nscope: stations are in scope while fed within %.0fh; in-scope predictions are kept %d days\n",
		s.FreshnessHours, s.RetentionDays)
	if s.Skipped {
		fmt.Fprintf(stdout, "  skipped: %s\n", s.Reason)
		return
	}
	if len(s.Cities) == 0 {
		fmt.Fprintln(stdout, "  no station has a snapshot yet")
		return
	}
	fmt.Fprintf(stdout, "  %-28s %12s %9s %9s %11s  %-22s %s\n",
		"city", "target", "stations", "in scope", "latest run", "newest price update", "newest prediction")
	for _, city := range s.Cities {
		target := "-"
		switch {
		case city.Target && !city.Geocoded:
			target = "NOT GEOCODED"
		case city.Target:
			target = fmt.Sprintf("%.1f km", city.RadiusKM)
		}
		newest := city.NewestSnapshot
		if newest == "" {
			newest = "never fed"
		}
		prediction := city.NewestPrediction
		if prediction == "" {
			prediction = "-"
		}
		fmt.Fprintf(stdout, "  %-28s %12s %9d %9d %11d  %-22s %s\n",
			city.City, target, city.Stations, city.InScope, city.InLatestRun, newest, prediction)
	}
	if run := s.LatestRun; run != nil {
		fmt.Fprintf(stdout, "  newest run: %s %s, %d stations", run.RunAt, run.Fuel, run.Stations)
		if run.OutOfScope > 0 {
			fmt.Fprintf(stdout, ", %d of them out of scope", run.OutOfScope)
		}
		fmt.Fprintln(stdout)
	}
}

// plural picks between two phrasings so a finding about one station does not
// read like a bug in the finding.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// formatCount groups digits so an eight-figure row count is readable at a
// glance, which is the whole point of printing it.
func formatCount(n int64) string {
	s := fmt.Sprintf("%d", n)
	if n < 0 {
		return s
	}
	var parts []string
	for len(s) > 3 {
		parts = append([]string{s[len(s)-3:]}, parts...)
		s = s[:len(s)-3]
	}
	return strings.Join(append([]string{s}, parts...), ",")
}

func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value := float64(n)
	for _, suffix := range []string{"KB", "MB", "GB", "TB"} {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%.1f PB", value)
}

// consideredIndexes lists the indexes MySQL weighed for the driving table and
// did not pick. SQLite's plan does not expose the alternatives, so this is
// empty there.
func consideredIndexes(cells []map[string]string, alias, chosen string) []string {
	var out []string
	seen := map[string]bool{chosen: true}
	for _, row := range cells {
		if alias != "" && row["table"] != alias {
			continue
		}
		for _, name := range strings.Split(row["possible_keys"], ",") {
			name = strings.TrimSpace(name)
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// indexHintSyntax renders a "use this index" hint for the dialect. MySQL's
// FORCE INDEX makes the optimizer prefer the index over a table scan; SQLite's
// INDEXED BY is stricter and errors when the index cannot serve the query,
// which is itself a useful answer.
func indexHintSyntax(d dialect, index string) string {
	if index == "" {
		return ""
	}
	if d == dialectMySQL {
		return "FORCE INDEX (" + index + ")"
	}
	return "INDEXED BY " + index
}

// measureHinted times one query with the index forced, for comparison against
// how the page actually runs it. A hint cannot change results, so a differing
// row count means the comparison is not measuring the same work and the caller
// is told rather than shown a misleading speedup.
func measureHinted(ctx context.Context, db *sql.DB, d dialect, spec accuracyQuerySpec, opts doctorOptions, indexNames []string) *doctorQueryHint {
	out := &doctorQueryHint{Index: opts.TryIndex}
	plan, cells, err := explainPlan(ctx, db, d, spec.sql, spec.args, opts.Analyze)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	out.Plan = plan
	out.UsesIndex, out.CoveringHit, _ = classifyPlan(plan, cells, d, indexNames, spec.alias)

	started := time.Now()
	count, err := countQueryRows(ctx, db, spec.sql, spec.args)
	out.DurationMS = float64(time.Since(started).Microseconds()) / 1000
	if err != nil {
		out.Error = err.Error()
	}
	out.Rows = count
	return out
}
