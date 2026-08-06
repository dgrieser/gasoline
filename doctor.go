package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

// doctor inspects a live database instead of changing it: what the prediction
// tables cost, which indexes exist, and — the part that matters for the admin
// accuracy page — what the planner actually does with that page's queries and
// how long each one takes. Everything it runs is read-only, so it is safe
// against production.
//
// The queries live here rather than being read out of web/index.php because
// the page builds them in PHP. accuracyQuerySpecs documents that duplication
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
	Filters  doctorFilters   `json:"filters"`
	Queries  []doctorQuery   `json:"queries"`
	Findings []doctorFinding `json:"findings"`
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

type doctorFilters struct {
	Fuel       string `json:"fuel"`
	City       string `json:"city"`
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
	if f.City != "" {
		joinRuns = "JOIN prediction_runs pr ON pr.id = pp.run_id "
		where += " AND pr.city_name = ?"
		args = append(args, f.City)
	}
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
		if f.City != "" {
			decJoin = "JOIN prediction_runs pr ON pr.id = d.run_id "
			decWhere += " AND pr.city_name = ?"
			decArgs = append(decArgs, f.City)
		}
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

// doctorTableStats reports size and row counts. SQLite counts exactly (cheap
// enough, and dbstat gives real byte sizes); MySQL reads information_schema so
// that diagnosing a huge table does not require scanning it.
func doctorTableStats(ctx context.Context, db *sql.DB, d dialect) ([]doctorTable, error) {
	out := make([]doctorTable, 0, len(doctorTables))
	for _, name := range doctorTables {
		table := doctorTable{Name: name}
		exists, err := tableExists(ctx, db, d, name)
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
			err := db.QueryRowContext(ctx, `
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
			if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+name).Scan(&table.Rows); err != nil {
				return nil, err
			}
			if size, ok := sqliteBtreeBytes(ctx, db, name); ok {
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
	prefix := "EXPLAIN QUERY PLAN "
	if d == dialectMySQL {
		prefix = "EXPLAIN "
		if analyze {
			prefix = "EXPLAIN ANALYZE "
		}
	}
	rows, err := db.QueryContext(ctx, prefix+query, args...)
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
		// SQLite's plan is one meaningful column; printing it bare reads far
		// better than labelling it.
		if d != dialectMySQL && len(columns) == 4 {
			out = append(out, cells[3].String)
			continue
		}
		// EXPLAIN ANALYZE returns a multi-line blob; keep its own line breaks
		// and leave the cells empty so classification falls back to text.
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

// classifyPlan pulls the two facts that decide whether the accuracy page is
// healthy out of a rendered plan: which of the table's indexes the planner
// chose, and whether it is reading table rows anyway.
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
			// EXPLAIN ANALYZE fallback: no cells to read, so match the text.
			if strings.Contains(line, "Using index") && !strings.Contains(line, "Using index condition") {
				covering = true
			}
			if strings.Contains(line, "type=ALL") {
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

	tables, err := doctorTableStats(ctx, db, d)
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

	// Without the table there is nothing to time, and running the queries anyway
	// would report the same missing-table error eight times over the one finding
	// that matters.
	if !opts.SkipQueries && !hasPredictions {
		result.Queries = append(result.Queries, doctorQuery{
			Name:    "accuracy_page",
			Purpose: "skipped: price_predictions does not exist — run `gasoline migrate`",
			Skipped: true,
		})
	} else if !opts.SkipQueries {
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
	} else {
		result.Queries = append(result.Queries, doctorQuery{
			Name:    "accuracy_page",
			Purpose: "skipped via --skip-queries",
			Skipped: true,
		})
	}

	result.Findings = doctorFindings(result, d, opts)
	return result, nil
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

type doctorOptions struct {
	Filters doctorFilters
	// TryIndex names an index to force on price_predictions for a second,
	// side-by-side timing of every query. Read-only: it changes nothing but
	// what this one run measures.
	TryIndex    string
	SkipQueries bool
	Analyze     bool
	ShowSQL     bool
	SlowMS      float64
}

func nullInt64Ptr(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	value := v.Int64
	return &value
}

func runDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	dbf := addDBFlags(fs)
	fuel := fs.String("fuel", "diesel", "Fuel the accuracy-page queries filter on: diesel, e5 or e10")
	city := fs.String("city", "", "Restrict the accuracy-page queries to one city (default: all cities)")
	confidence := fs.String("confidence", "all", "Confidence filter: all or medium_high")
	rangeName := fs.String("range", "", "Target-date range: 7d, 14d (default) or 30d")
	from := fs.String("from", "", "Range start as YYYY-MM-DD (needs --to)")
	to := fs.String("to", "", "Range end as YYYY-MM-DD (needs --from)")
	skipQueries := fs.Bool("skip-queries", false, "Only report schema, sizes and indexes; do not run the accuracy-page queries")
	tryIndex := fs.String("try-index", "", "Also time every price_predictions query with this index forced, to measure what a different index choice would cost (read-only)")
	analyze := fs.Bool("analyze", false, "Use EXPLAIN ANALYZE for real per-step timings (MySQL 8.0.18+; ignored on SQLite)")
	explain := fs.Bool("explain", false, "Print the full query plan for each query")
	showSQL := fs.Bool("sql", false, "Print the SQL of each query")
	slowMS := fs.Float64("slow-ms", 1000, "Flag queries at or above this duration in milliseconds")
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
	if !isSuggestFuelType(*fuel) {
		return errors.New("--fuel must be one of: diesel, e5, e10")
	}
	if *confidence != "all" && *confidence != "medium_high" {
		return errors.New("--confidence must be one of: all, medium_high")
	}
	fromTS, toTS, err := resolveDoctorRange(*rangeName, *from, *to, time.Now())
	if err != nil {
		return err
	}

	// Opening a SQLite path creates the file, which every other command wants
	// and this one must not: doctor is a diagnostic, and conjuring an empty
	// database would both leave a stray file behind and answer "everything is
	// absent" when the truth is that there is no database at this path.
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
	opts := doctorOptions{
		Filters: doctorFilters{
			Fuel:       *fuel,
			City:       strings.TrimSpace(*city),
			Confidence: *confidence,
			From:       fromTS,
			To:         toTS,
		},
		TryIndex:    strings.TrimSpace(*tryIndex),
		SkipQueries: *skipQueries,
		Analyze:     *analyze,
		ShowSQL:     *showSQL,
		SlowMS:      *slowMS,
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

	cityLabel := r.Filters.City
	if cityLabel == "" {
		cityLabel = "all cities"
	}
	fmt.Fprintf(stdout, "\naccuracy page queries: fuel=%s, %s, confidence=%s, %s .. %s\n",
		r.Filters.Fuel, cityLabel, r.Filters.Confidence, r.Filters.From, r.Filters.To)
	for _, q := range r.Queries {
		if q.Skipped {
			fmt.Fprintf(stdout, "  %-16s %s\n", q.Name, q.Purpose)
			continue
		}
		note := "no index"
		switch {
		case q.Error != "":
			note = "failed: " + q.Error
		case q.FullScan:
			note = "TABLE SCAN"
		case q.UsesIndex != "" && q.CoveringHit:
			note = "covering " + q.UsesIndex
		case q.UsesIndex != "":
			note = q.UsesIndex
		}
		fmt.Fprintf(stdout, "  %-16s %9.1f ms %6d rows  %s\n", q.Name, q.DurationMS, q.Rows, note)
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

	fmt.Fprintln(stdout, "\nfindings:")
	if len(r.Findings) == 0 {
		fmt.Fprintln(stdout, "  none")
		return
	}
	for _, f := range r.Findings {
		fmt.Fprintf(stdout, "  %-4s %s\n", f.Severity, f.Message)
	}
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
