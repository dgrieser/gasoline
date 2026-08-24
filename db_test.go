package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

// clearDBEnv isolates a test from ambient database configuration (process
// environment and the repo-root .env file).
func clearDBEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		envDBPathName,
		envDBDriverName,
		envMySQLDSNName,
		envMySQLHostName,
		envMySQLPortName,
		envMySQLUserName,
		envMySQLPasswordName,
		envMySQLDatabaseName,
		envMySQLTLSName,
	} {
		t.Setenv(name, "")
	}
	t.Chdir(t.TempDir())
}

func parseDBFlags(t *testing.T, args ...string) (*flag.FlagSet, *dbFlags) {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	dbf := addDBFlags(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	return fs, dbf
}

func TestResolveDBConfigDefaultsToSQLite(t *testing.T) {
	clearDBEnv(t)

	fs, dbf := parseDBFlags(t)
	cfg, err := resolveDBConfig(fs, dbf)
	if err != nil {
		t.Fatalf("resolveDBConfig: %v", err)
	}
	if cfg.Driver != dialectSQLite {
		t.Fatalf("Driver = %q, want %q", cfg.Driver, dialectSQLite)
	}
	if cfg.Path != defaultDBPath {
		t.Fatalf("Path = %q, want %q", cfg.Path, defaultDBPath)
	}
}

func TestResolveDBConfigMySQLFromFlags(t *testing.T) {
	clearDBEnv(t)

	fs, dbf := parseDBFlags(t,
		"--db-driver", "mysql",
		"--mysql-host", "db.example.com",
		"--mysql-user", "gas",
		"--mysql-password", "secret",
		"--mysql-database", "gasoline",
	)
	cfg, err := resolveDBConfig(fs, dbf)
	if err != nil {
		t.Fatalf("resolveDBConfig: %v", err)
	}
	if cfg.Driver != dialectMySQL {
		t.Fatalf("Driver = %q, want %q", cfg.Driver, dialectMySQL)
	}
	parsed, err := mysql.ParseDSN(cfg.MySQLDSN)
	if err != nil {
		t.Fatalf("ParseDSN(%q): %v", cfg.MySQLDSN, err)
	}
	if parsed.Addr != "db.example.com:3306" {
		t.Fatalf("Addr = %q, want %q", parsed.Addr, "db.example.com:3306")
	}
	if parsed.User != "gas" || parsed.Passwd != "secret" || parsed.DBName != "gasoline" {
		t.Fatalf("DSN user/password/database = %q/%q/%q", parsed.User, parsed.Passwd, parsed.DBName)
	}
}

func TestResolveDBConfigMySQLDSNFlagImpliesDriver(t *testing.T) {
	clearDBEnv(t)

	fs, dbf := parseDBFlags(t, "--mysql-dsn", "gas:secret@tcp(db.example.com:3307)/gasoline")
	cfg, err := resolveDBConfig(fs, dbf)
	if err != nil {
		t.Fatalf("resolveDBConfig: %v", err)
	}
	if cfg.Driver != dialectMySQL {
		t.Fatalf("Driver = %q, want %q", cfg.Driver, dialectMySQL)
	}
	parsed, err := mysql.ParseDSN(cfg.MySQLDSN)
	if err != nil {
		t.Fatalf("ParseDSN(%q): %v", cfg.MySQLDSN, err)
	}
	if parsed.Addr != "db.example.com:3307" || parsed.DBName != "gasoline" {
		t.Fatalf("Addr/DBName = %q/%q", parsed.Addr, parsed.DBName)
	}
}

func TestResolveDBConfigMySQLFromEnv(t *testing.T) {
	clearDBEnv(t)
	t.Setenv(envDBDriverName, "mysql")
	t.Setenv(envMySQLUserName, "gas")
	t.Setenv(envMySQLDatabaseName, "gasoline")

	fs, dbf := parseDBFlags(t)
	cfg, err := resolveDBConfig(fs, dbf)
	if err != nil {
		t.Fatalf("resolveDBConfig: %v", err)
	}
	if cfg.Driver != dialectMySQL {
		t.Fatalf("Driver = %q, want %q", cfg.Driver, dialectMySQL)
	}
	parsed, err := mysql.ParseDSN(cfg.MySQLDSN)
	if err != nil {
		t.Fatalf("ParseDSN(%q): %v", cfg.MySQLDSN, err)
	}
	if parsed.Addr != defaultMySQLHost+":"+defaultMySQLPort {
		t.Fatalf("Addr = %q, want default %s:%s", parsed.Addr, defaultMySQLHost, defaultMySQLPort)
	}
}

func TestResolveDBConfigMySQLFromDotEnv(t *testing.T) {
	clearDBEnv(t)
	dotEnv := strings.Join([]string{
		"GASOLINE_DB_DRIVER=mysql",
		"GASOLINE_MYSQL_HOST=dotenv-host",
		"GASOLINE_MYSQL_USER=gas",
		"GASOLINE_MYSQL_DATABASE=gasoline",
	}, "\n")
	if err := os.WriteFile(".env", []byte(dotEnv), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}

	fs, dbf := parseDBFlags(t)
	cfg, err := resolveDBConfig(fs, dbf)
	if err != nil {
		t.Fatalf("resolveDBConfig: %v", err)
	}
	if cfg.Driver != dialectMySQL {
		t.Fatalf("Driver = %q, want %q", cfg.Driver, dialectMySQL)
	}
	if !strings.Contains(cfg.MySQLDSN, "dotenv-host:3306") {
		t.Fatalf("DSN = %q, want host from .env", cfg.MySQLDSN)
	}
}

func TestResolveDBConfigMySQLFlagsOverrideEnvDSN(t *testing.T) {
	clearDBEnv(t)
	t.Setenv(envDBDriverName, "mysql")
	t.Setenv(envMySQLDSNName, "gas:secret@tcp(env-host:3306)/envdb")

	fs, dbf := parseDBFlags(t, "--mysql-host", "flag-host", "--mysql-database", "flagdb")
	cfg, err := resolveDBConfig(fs, dbf)
	if err != nil {
		t.Fatalf("resolveDBConfig: %v", err)
	}
	parsed, err := mysql.ParseDSN(cfg.MySQLDSN)
	if err != nil {
		t.Fatalf("ParseDSN(%q): %v", cfg.MySQLDSN, err)
	}
	if parsed.Addr != "flag-host:3306" {
		t.Fatalf("Addr = %q, want %q", parsed.Addr, "flag-host:3306")
	}
	if parsed.DBName != "flagdb" {
		t.Fatalf("DBName = %q, want %q", parsed.DBName, "flagdb")
	}
	// Fields without a flag keep the environment DSN's values.
	if parsed.User != "gas" || parsed.Passwd != "secret" {
		t.Fatalf("user/password = %q/%q, want gas/secret from env DSN", parsed.User, parsed.Passwd)
	}
}

func TestResolveDBConfigMySQLPortFlagOverridesEnvDSN(t *testing.T) {
	clearDBEnv(t)
	t.Setenv(envDBDriverName, "mysql")
	t.Setenv(envMySQLDSNName, "gas:secret@tcp(env-host:3306)/envdb")

	fs, dbf := parseDBFlags(t, "--mysql-port", "3307")
	cfg, err := resolveDBConfig(fs, dbf)
	if err != nil {
		t.Fatalf("resolveDBConfig: %v", err)
	}
	parsed, err := mysql.ParseDSN(cfg.MySQLDSN)
	if err != nil {
		t.Fatalf("ParseDSN(%q): %v", cfg.MySQLDSN, err)
	}
	if parsed.Addr != "env-host:3307" {
		t.Fatalf("Addr = %q, want %q", parsed.Addr, "env-host:3307")
	}
}

func TestResolveDBConfigMySQLFlagBeatsEnv(t *testing.T) {
	clearDBEnv(t)
	t.Setenv(envDBDriverName, "mysql")
	t.Setenv(envMySQLHostName, "env-host")
	t.Setenv(envMySQLUserName, "env-user")
	t.Setenv(envMySQLDatabaseName, "envdb")

	fs, dbf := parseDBFlags(t, "--mysql-host", "flag-host")
	cfg, err := resolveDBConfig(fs, dbf)
	if err != nil {
		t.Fatalf("resolveDBConfig: %v", err)
	}
	if !strings.Contains(cfg.MySQLDSN, "flag-host:3306") {
		t.Fatalf("DSN = %q, want host from flag", cfg.MySQLDSN)
	}
}

func TestResolveDBConfigMySQLRequiresUserAndDatabase(t *testing.T) {
	clearDBEnv(t)

	fs, dbf := parseDBFlags(t, "--db-driver", "mysql", "--mysql-database", "gasoline")
	if _, err := resolveDBConfig(fs, dbf); err == nil || !strings.Contains(err.Error(), "no user configured") {
		t.Fatalf("err = %v, want missing user error", err)
	}

	fs, dbf = parseDBFlags(t, "--db-driver", "mysql", "--mysql-user", "gas")
	if _, err := resolveDBConfig(fs, dbf); err == nil || !strings.Contains(err.Error(), "no database configured") {
		t.Fatalf("err = %v, want missing database error", err)
	}
}

func TestResolveDBConfigRejectsUnknownDriver(t *testing.T) {
	clearDBEnv(t)

	fs, dbf := parseDBFlags(t, "--db-driver", "postgres")
	if _, err := resolveDBConfig(fs, dbf); err == nil || !strings.Contains(err.Error(), "unsupported database driver") {
		t.Fatalf("err = %v, want unsupported driver error", err)
	}
}

func TestNormalizeMySQLDSNRequiresDatabase(t *testing.T) {
	if _, err := normalizeMySQLDSN("gas:secret@tcp(host:3306)/", ""); err == nil || !strings.Contains(err.Error(), "database name") {
		t.Fatalf("err = %v, want missing database name error", err)
	}
}

func TestResolveDBConfigMySQLTLSFlag(t *testing.T) {
	clearDBEnv(t)

	fs, dbf := parseDBFlags(t, "--db-driver", "mysql",
		"--mysql-user", "gas", "--mysql-database", "gasoline", "--mysql-tls", "skip-verify")
	cfg, err := resolveDBConfig(fs, dbf)
	if err != nil {
		t.Fatalf("resolveDBConfig: %v", err)
	}
	parsed, err := mysql.ParseDSN(cfg.MySQLDSN)
	if err != nil {
		t.Fatalf("ParseDSN(%q): %v", cfg.MySQLDSN, err)
	}
	if parsed.TLSConfig != "skip-verify" {
		t.Fatalf("TLSConfig = %q, want skip-verify", parsed.TLSConfig)
	}
}

func TestResolveDBConfigMySQLTLSFlagOverridesDSN(t *testing.T) {
	clearDBEnv(t)

	fs, dbf := parseDBFlags(t,
		"--mysql-dsn", "gas:secret@tcp(host:3306)/gasoline?tls=true",
		"--mysql-tls", "skip-verify")
	cfg, err := resolveDBConfig(fs, dbf)
	if err != nil {
		t.Fatalf("resolveDBConfig: %v", err)
	}
	parsed, err := mysql.ParseDSN(cfg.MySQLDSN)
	if err != nil {
		t.Fatalf("ParseDSN(%q): %v", cfg.MySQLDSN, err)
	}
	if parsed.TLSConfig != "skip-verify" {
		t.Fatalf("TLSConfig = %q, want skip-verify to override the DSN's tls=true", parsed.TLSConfig)
	}
}

func TestResolveDBConfigMySQLTLSEnv(t *testing.T) {
	clearDBEnv(t)
	t.Setenv(envDBDriverName, "mysql")
	t.Setenv(envMySQLUserName, "gas")
	t.Setenv(envMySQLDatabaseName, "gasoline")
	t.Setenv(envMySQLTLSName, "true")

	fs, dbf := parseDBFlags(t)
	cfg, err := resolveDBConfig(fs, dbf)
	if err != nil {
		t.Fatalf("resolveDBConfig: %v", err)
	}
	parsed, err := mysql.ParseDSN(cfg.MySQLDSN)
	if err != nil {
		t.Fatalf("ParseDSN(%q): %v", cfg.MySQLDSN, err)
	}
	if parsed.TLSConfig != "true" {
		t.Fatalf("TLSConfig = %q, want true", parsed.TLSConfig)
	}
}

func TestResolveDBConfigMySQLTLSRejectsInvalid(t *testing.T) {
	clearDBEnv(t)

	fs, dbf := parseDBFlags(t, "--db-driver", "mysql",
		"--mysql-user", "gas", "--mysql-database", "gasoline", "--mysql-tls", "yes")
	if _, err := resolveDBConfig(fs, dbf); err == nil || !strings.Contains(err.Error(), "invalid mysql TLS mode") {
		t.Fatalf("err = %v, want invalid TLS mode error", err)
	}
}

func TestDBConfigDescriptionRedactsPassword(t *testing.T) {
	cfg := dbConfig{Driver: dialectMySQL, MySQLDSN: "gas:supersecret@tcp(db.example.com:3306)/gasoline"}
	desc := cfg.Description()
	if strings.Contains(desc, "supersecret") {
		t.Fatalf("Description() = %q leaks the password", desc)
	}
	if desc != "mysql://gas@db.example.com:3306/gasoline" {
		t.Fatalf("Description() = %q", desc)
	}
}

func TestQueryLimit(t *testing.T) {
	if got := queryLimit(dialectSQLite, 0); got != -1 {
		t.Fatalf("queryLimit(sqlite, 0) = %d, want -1", got)
	}
	if got := queryLimit(dialectMySQL, 0); got != math.MaxInt64 {
		t.Fatalf("queryLimit(mysql, 0) = %d, want MaxInt64", got)
	}
	if got := queryLimit(dialectMySQL, 25); got != 25 {
		t.Fatalf("queryLimit(mysql, 25) = %d, want 25", got)
	}
}

// TestCopyDatabaseData exercises the migrate-to-mysql copy pipeline against a
// SQLite target: the copy statements (COUNT, DELETE, batched INSERT) are
// dialect-neutral, so this verifies row transfer, the non-empty guard, and
// --overwrite without needing a MySQL server.
func TestCopyDatabaseData(t *testing.T) {
	ctx := context.Background()
	src := openTestDB(t)

	city := cachedCity{QueryName: "Berlin", Name: "Berlin", DisplayName: "Berlin, Germany", Lat: 52.52, Lng: 13.405}
	diesel := 1.599
	stations := []tankerStation{
		{ID: "station-1", Name: "Station One", Brand: "BrandA", Street: "Main St", Place: "Berlin", Lat: 52.5, Lng: 13.4, Diesel: &diesel, IsOpen: true},
		{ID: "station-2", Name: "Station Two", Brand: "BrandB", Street: "Side St", Place: "Berlin", Lat: 52.6, Lng: 13.5, IsOpen: false},
	}
	if err := persistUpdate(ctx, src, dialectSQLite, city, stations, time.Date(2026, 7, 1, 8, 0, 0, 0, time.UTC), 5); err != nil {
		t.Fatalf("persistUpdate: %v", err)
	}

	// A prediction run plus a check decision, so the copy is exercised for the
	// tables that carry foreign keys onto each other.
	runID := insertPredictionRunRow(t, src, time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC))
	if _, err := src.ExecContext(ctx, `
		INSERT INTO price_check_decisions (run_id, station_id, fuel, decided_at, target_start, target_end,
			observed_price, observed_at, predicted_price, error, history_percentile, confidence, sample_count,
			verdict, recommendation, expected_lower, expected_drop)
		VALUES (?, 'station-1', 'diesel', '2026-07-01T09:00:00Z', '2026-07-01T09:00:00Z', '2026-07-01T10:00:00Z',
			1.60, '2026-07-01T08:55:00Z', 1.62, -0.02, 20, 'high', 9, 'low', 'buy', 0, NULL)
	`, runID); err != nil {
		t.Fatalf("insert decision: %v", err)
	}

	dstPath := filepath.Join(t.TempDir(), "target.db")
	dst, err := openDB(dstPath)
	if err != nil {
		t.Fatalf("openDB target: %v", err)
	}
	t.Cleanup(func() { _ = dst.Close() })
	if err := initSchema(ctx, dst, dialectSQLite); err != nil {
		t.Fatalf("initSchema target: %v", err)
	}

	result, err := copySQLiteToMySQL(ctx, src, dst, false)
	if err != nil {
		t.Fatalf("copySQLiteToMySQL: %v", err)
	}
	if result.CheckDecisions != 1 {
		t.Fatalf("check decisions copied = %d, want 1", result.CheckDecisions)
	}
	var copiedVerdict string
	if err := dst.QueryRowContext(ctx, `SELECT verdict FROM price_check_decisions`).Scan(&copiedVerdict); err != nil {
		t.Fatalf("read copied decision: %v", err)
	}
	if copiedVerdict != "low" {
		t.Fatalf("copied verdict = %q, want low", copiedVerdict)
	}
	if result.Cities != 1 || result.Stations != 2 || result.PriceSnapshots != 2 {
		t.Fatalf("copied cities/stations/snapshots = %d/%d/%d, want 1/2/2", result.Cities, result.Stations, result.PriceSnapshots)
	}
	if result.Overwritten {
		t.Fatal("Overwritten = true, want false for empty target")
	}

	var snapshotCount int
	if err := dst.QueryRowContext(ctx, `SELECT COUNT(*) FROM price_snapshots`).Scan(&snapshotCount); err != nil {
		t.Fatalf("count target snapshots: %v", err)
	}
	if snapshotCount != 2 {
		t.Fatalf("target snapshot count = %d, want 2", snapshotCount)
	}

	// Snapshot ids must survive the copy (tie-breaking depends on them).
	var srcIDs, dstIDs string
	if err := src.QueryRowContext(ctx, `SELECT GROUP_CONCAT(id) FROM (SELECT id FROM price_snapshots ORDER BY id)`).Scan(&srcIDs); err != nil {
		t.Fatalf("source ids: %v", err)
	}
	if err := dst.QueryRowContext(ctx, `SELECT GROUP_CONCAT(id) FROM (SELECT id FROM price_snapshots ORDER BY id)`).Scan(&dstIDs); err != nil {
		t.Fatalf("target ids: %v", err)
	}
	if srcIDs != dstIDs {
		t.Fatalf("snapshot ids differ after copy: source %s, target %s", srcIDs, dstIDs)
	}

	if _, err := copySQLiteToMySQL(ctx, src, dst, false); err == nil || !strings.Contains(err.Error(), "--overwrite") {
		t.Fatalf("err = %v, want non-empty target error", err)
	}

	result, err = copySQLiteToMySQL(ctx, src, dst, true)
	if err != nil {
		t.Fatalf("copySQLiteToMySQL overwrite: %v", err)
	}
	if !result.Overwritten {
		t.Fatal("Overwritten = false, want true")
	}
	if result.Cities != 1 || result.Stations != 2 || result.PriceSnapshots != 2 {
		t.Fatalf("overwrite copy counts = %d/%d/%d, want 1/2/2", result.Cities, result.Stations, result.PriceSnapshots)
	}
}

// TestCopyTableBatching pushes more rows than one batch to cover the flush loop.
func TestCopyTableBatching(t *testing.T) {
	ctx := context.Background()
	src := openTestDB(t)

	tx, err := src.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	createdAt := time.Now().UTC().Format(time.RFC3339)
	for i := 0; i < copyBatchSize+7; i++ {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO cities (name, normalized_name, display_name, lat, lng, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
			fmt.Sprintf("city-%d", i), "city", "city", 1.0, 2.0, createdAt); err != nil {
			t.Fatalf("insert city %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	dst, err := openDB(filepath.Join(t.TempDir(), "target.db"))
	if err != nil {
		t.Fatalf("openDB target: %v", err)
	}
	t.Cleanup(func() { _ = dst.Close() })
	if err := initSchema(ctx, dst, dialectSQLite); err != nil {
		t.Fatalf("initSchema target: %v", err)
	}

	dstTx, err := dst.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin target tx: %v", err)
	}
	defer dstTx.Rollback()
	copied, err := copyTable(ctx, src, dstTx, "cities", []string{"name", "normalized_name", "display_name", "lat", "lng", "created_at"})
	if err != nil {
		t.Fatalf("copyTable: %v", err)
	}
	if err := dstTx.Commit(); err != nil {
		t.Fatalf("commit target: %v", err)
	}
	if copied != copyBatchSize+7 {
		t.Fatalf("copied = %d, want %d", copied, copyBatchSize+7)
	}

	var count sql.NullInt64
	if err := dst.QueryRowContext(ctx, `SELECT COUNT(*) FROM cities`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if int(count.Int64) != copyBatchSize+7 {
		t.Fatalf("target count = %d, want %d", count.Int64, copyBatchSize+7)
	}
}

// knownNestedCommandSites are functions that issue a command while one of their
// own result sets is still open. See TestNoCommandsWhileAResultSetIsOpen for why
// that is a bug; these predate the check (migrateCitiesDeduplicate came in with
// MySQL support) and are recorded rather than silently tolerated.
var knownNestedCommandSites = map[string]bool{
	"migrateCitiesDeduplicate": true,
}

// TestNoCommandsWhileAResultSetIsOpen fails when a function starts another
// command on a transaction whose previous result set is still being read.
//
// A transaction is pinned to one connection, and go-sql-driver/mysql talks to it
// synchronously through a single buffer, so it cannot begin a command while rows
// are outstanding — it returns "commands out of sync" instead. SQLite tolerates
// it, and CI has no MySQL service, so nothing else in this suite would notice.
// Read the rows into a slice, close them, then issue the next command.
//
// Calls to package functions that themselves query are flagged too, because that
// is the shape the bug actually took: a helper taking the same transaction, one
// call away from the loop.
func TestNoCommandsWhileAResultSetIsOpen(t *testing.T) {
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		files[path] = file
	}

	// Package functions whose body issues a command, so a call to one inside a
	// rows loop is as unsafe as the command itself.
	queries := map[string]bool{}
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if sel, ok := callSelector(n); ok && isCommand(sel) {
					queries[fn.Name.Name] = true
				}
				return true
			})
		}
	}

	for path, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || knownNestedCommandSites[fn.Name.Name] {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				loop, ok := n.(*ast.ForStmt)
				if !ok || loop.Cond == nil {
					return true
				}
				cond, ok := loop.Cond.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := cond.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Next" {
					return true
				}
				ast.Inspect(loop.Body, func(inner ast.Node) bool {
					call, ok := inner.(*ast.CallExpr)
					if !ok {
						return true
					}
					switch fun := call.Fun.(type) {
					case *ast.SelectorExpr:
						if isCommand(fun.Sel.Name) {
							t.Errorf("%s: %s calls %s while its rows are still open; buffer the rows "+
								"and close them first (MySQL: commands out of sync)",
								fset.Position(call.Pos()), fn.Name.Name, fun.Sel.Name)
						}
					case *ast.Ident:
						if queries[fun.Name] && fun.Name != fn.Name.Name {
							t.Errorf("%s: %s calls %s() — which queries — while its rows are still open; "+
								"buffer the rows and close them first (MySQL: commands out of sync)",
								fset.Position(call.Pos()), fn.Name.Name, fun.Name)
						}
					}
					return true
				})
				return true
			})
		}
		_ = path
	}
}

func callSelector(n ast.Node) (string, bool) {
	call, ok := n.(*ast.CallExpr)
	if !ok {
		return "", false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	return sel.Sel.Name, true
}

func isCommand(name string) bool {
	switch name {
	case "QueryContext", "QueryRowContext", "ExecContext":
		return true
	}
	return false
}

// Every table has to be declared for both engines. A table added to one dialect
// and not the other installs cleanly on the developer's SQLite file and leaves
// the MySQL deployment without it, which the viewer then refuses to start on.
func TestSchemaDeclaresTheSameTablesForBothEngines(t *testing.T) {
	sqlite := append([]string{}, schemaTableNames(dialectSQLite)...)
	mysql := append([]string{}, schemaTableNames(dialectMySQL)...)
	if len(sqlite) == 0 {
		t.Fatal("schemaTableNames found no tables, so the pattern no longer matches the DDL")
	}
	sort.Strings(sqlite)
	sort.Strings(mysql)
	if !slices.Equal(sqlite, mysql) {
		t.Errorf("the two dialects declare different tables:\n sqlite: %v\n  mysql: %v", sqlite, mysql)
	}
	// The viewer refuses to start without these four, so name them explicitly
	// rather than trusting that both lists happen to agree about them.
	for _, want := range []string{"users", "user_filters", "settings", "update_targets"} {
		if !slices.Contains(sqlite, want) {
			t.Errorf("schemaStatements no longer creates %q, which the viewer's schema guard requires", want)
		}
	}
}

// `migrate` on a database that is current except for one missing table used to
// print "no migrations needed": ensureSchema creates it with CREATE TABLE IF
// NOT EXISTS, which says nothing, and the migration list was the only thing the
// command reported. An operator who had just been told to run migrate then had
// no way to tell it apart from a no-op.
func TestMigrateReportsTablesItCreates(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `DROP TABLE user_filters`); err != nil {
		t.Fatalf("drop user_filters: %v", err)
	}
	created, err := ensureSchema(ctx, db, dialectSQLite)
	if err != nil {
		t.Fatalf("ensureSchema: %v", err)
	}
	if !slices.Contains(created, "user_filters") {
		t.Errorf("ensureSchema reported %v, want the table it had to create", created)
	}
	exists, err := tableExists(ctx, db, dialectSQLite, "user_filters")
	if err != nil {
		t.Fatalf("tableExists: %v", err)
	}
	if !exists {
		t.Error("ensureSchema did not actually create user_filters")
	}

	// And a second pass reports nothing, so the line really means "this is new".
	created, err = ensureSchema(ctx, db, dialectSQLite)
	if err != nil {
		t.Fatalf("ensureSchema again: %v", err)
	}
	if len(created) != 0 {
		t.Errorf("ensureSchema reported %v on an unchanged database, want nothing", created)
	}
}

// The PHP viewer keeps the durable half of a login in user_sessions: without
// that table every visit falls back to a plain PHP session, which the host
// garbage-collects, and the viewer starts asking for the password again. So
// `gasoline migrate` has to create it, with the columns the viewer writes.
func TestMigrateCreatesUserSessions(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	columns := map[string]bool{}
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(user_sessions)`)
	if err != nil {
		t.Fatalf("table_info: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid        int
			name, typ  string
			notNull    int
			defaultVal sql.NullString
			pk         int
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultVal, &pk); err != nil {
			t.Fatalf("scan table_info: %v", err)
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("table_info rows: %v", err)
	}
	if len(columns) == 0 {
		t.Fatal("migrate no longer creates user_sessions, so the PHP viewer cannot keep anyone signed in")
	}
	for _, want := range []string{"user_id", "selector", "validator_hash", "created_at", "last_used_at", "expires_at"} {
		if !columns[want] {
			t.Errorf("user_sessions is missing the %q column the PHP viewer writes", want)
		}
	}

	// One token per browser, and the selector is the lookup key: two rows may
	// never share one, or a stolen selector would match another browser's row.
	for _, email := range []string{"one@example.com", "two@example.com"} {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO users (email, password_hash, status, created_at) VALUES (?, 'x', 'approved', '2026-01-01T00:00:00Z')`,
			email,
		); err != nil {
			t.Fatalf("insert user: %v", err)
		}
	}
	insert := `INSERT INTO user_sessions (user_id, selector, validator_hash, created_at, last_used_at, expires_at)
		VALUES ((SELECT id FROM users WHERE email = ?), ?, ?, ?, ?, ?)`
	if _, err := db.ExecContext(ctx, insert, "one@example.com", "abc", "hash", "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z", "2026-02-01T00:00:00Z"); err != nil {
		t.Fatalf("insert token: %v", err)
	}
	if _, err := db.ExecContext(ctx, insert, "two@example.com", "abc", "hash2", "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z", "2026-02-01T00:00:00Z"); err == nil {
		t.Error("user_sessions accepts a duplicate selector, so one browser's cookie could resolve to another's token")
	}

	// Deleting the account has to take its tokens with it, or a deleted user's
	// cookie would still resolve to a row.
	if _, err := db.ExecContext(ctx, `DELETE FROM users WHERE email = 'one@example.com'`); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	var left int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM user_sessions`).Scan(&left); err != nil {
		t.Fatalf("count tokens: %v", err)
	}
	if left != 0 {
		t.Errorf("%d persistent logins outlived their deleted user", left)
	}
}

// Both dialects have to declare the same columns: a login issued against MySQL
// and one issued against SQLite are read back by the same PHP.
func TestUserSessionsSchemaMatchesAcrossDialects(t *testing.T) {
	for _, want := range []string{"user_id", "selector", "validator_hash", "created_at", "last_used_at", "expires_at"} {
		for _, d := range []dialect{dialectSQLite, dialectMySQL} {
			var table string
			for _, stmt := range schemaStatements(d) {
				if strings.Contains(stmt, "CREATE TABLE IF NOT EXISTS user_sessions") {
					table = stmt
				}
			}
			if table == "" {
				t.Fatalf("%v schema no longer creates user_sessions", d)
			}
			if !strings.Contains(table, want) {
				t.Errorf("%v user_sessions is missing the %q column", d, want)
			}
		}
	}
}

// The viewer is PHP and cannot be exercised from a Go test, so the SQL it runs
// against user_sessions is checked here instead: a column renamed on the Go
// side has to be renamed in the viewer too, or every login silently falls back
// to a session-only one.
func TestViewerPersistentLoginUsesTheSchemaColumns(t *testing.T) {
	viewer, err := os.ReadFile(filepath.Join("web", "index.php"))
	if err != nil {
		t.Fatalf("read web/index.php: %v", err)
	}
	source := string(viewer)

	if !strings.Contains(source, "INSERT INTO user_sessions (user_id, selector, validator_hash, created_at, last_used_at, expires_at)") {
		t.Error("web/index.php no longer issues persistent-login tokens with the schema's column list")
	}
	if !strings.Contains(source, "SELECT * FROM user_sessions WHERE selector = :selector") {
		t.Error("web/index.php no longer looks a persistent login up by its selector")
	}
	// The validator must never be stored as it travels in the cookie.
	if !strings.Contains(source, "hash('sha256', $validator)") {
		t.Error("web/index.php no longer hashes the cookie's validator before comparing or storing it")
	}
	if !strings.Contains(source, "hash_equals((string) $row['validator_hash'], hash('sha256', $validator))") {
		t.Error("web/index.php no longer compares the validator in constant time")
	}
	// Expired rows have to go, both as a login check and as table hygiene.
	if !strings.Contains(source, "DELETE FROM user_sessions WHERE expires_at < :now") {
		t.Error("web/index.php no longer purges expired persistent logins")
	}
	if !strings.Contains(source, "DELETE FROM user_sessions WHERE user_id = :user_id") {
		t.Error("web/index.php no longer revokes a user's persistent logins on password change or deletion")
	}
}
