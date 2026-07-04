package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
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
	if _, err := normalizeMySQLDSN("gas:secret@tcp(host:3306)/"); err == nil || !strings.Contains(err.Error(), "database name") {
		t.Fatalf("err = %v, want missing database name error", err)
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
