package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	_ "modernc.org/sqlite"
)

type dialect string

const (
	dialectSQLite dialect = "sqlite"
	dialectMySQL  dialect = "mysql"
)

const (
	envDBDriverName      = "GASOLINE_DB_DRIVER"
	envMySQLDSNName      = "GASOLINE_MYSQL_DSN"
	envMySQLHostName     = "GASOLINE_MYSQL_HOST"
	envMySQLPortName     = "GASOLINE_MYSQL_PORT"
	envMySQLUserName     = "GASOLINE_MYSQL_USER"
	envMySQLPasswordName = "GASOLINE_MYSQL_PASSWORD"
	envMySQLDatabaseName = "GASOLINE_MYSQL_DATABASE"
	envMySQLTLSName      = "GASOLINE_MYSQL_TLS"

	defaultMySQLHost = "127.0.0.1"
	defaultMySQLPort = "3306"
)

// dbFlags holds the database connection flags shared by every command.
type dbFlags struct {
	path          *string
	driver        *string
	mysqlDSN      *string
	mysqlHost     *string
	mysqlPort     *string
	mysqlUser     *string
	mysqlPassword *string
	mysqlDatabase *string
	mysqlTLS      *string
}

func addDBFlags(fs *flag.FlagSet) *dbFlags {
	return &dbFlags{
		path:          fs.String("db", defaultDBPath, "SQLite database file"),
		driver:        fs.String("db-driver", "", "Database driver: sqlite or mysql (default sqlite)"),
		mysqlDSN:      fs.String("mysql-dsn", "", "MySQL DSN, e.g. user:pass@tcp(host:3306)/gasoline (implies --db-driver mysql)"),
		mysqlHost:     fs.String("mysql-host", "", "MySQL host (default "+defaultMySQLHost+")"),
		mysqlPort:     fs.String("mysql-port", "", "MySQL port (default "+defaultMySQLPort+")"),
		mysqlUser:     fs.String("mysql-user", "", "MySQL user"),
		mysqlPassword: fs.String("mysql-password", "", "MySQL password"),
		mysqlDatabase: fs.String("mysql-database", "", "MySQL database name"),
		mysqlTLS:      fs.String("mysql-tls", "", "MySQL TLS mode: true, skip-verify, preferred, or false"),
	}
}

// dbConfig is the resolved database target of a command invocation.
type dbConfig struct {
	Driver   dialect
	Path     string // SQLite database file
	MySQLDSN string // go-sql-driver DSN
}

// Description is a human-readable identifier of the database, safe to print
// (never contains the MySQL password).
func (c dbConfig) Description() string {
	if c.Driver != dialectMySQL {
		return c.Path
	}
	cfg, err := mysql.ParseDSN(c.MySQLDSN)
	if err != nil {
		return "mysql database"
	}
	if cfg.User != "" {
		return fmt.Sprintf("mysql://%s@%s/%s", cfg.User, cfg.Addr, cfg.DBName)
	}
	return fmt.Sprintf("mysql://%s/%s", cfg.Addr, cfg.DBName)
}

// resolveDBConfig merges database settings with precedence flag > environment
// > .env file > default. The driver defaults to sqlite; passing --mysql-dsn on
// the command line selects mysql without needing an explicit --db-driver.
func resolveDBConfig(fs *flag.FlagSet, f *dbFlags) (dbConfig, error) {
	env, err := newEnvLookup()
	if err != nil {
		return dbConfig{}, err
	}

	driverValue := strings.TrimSpace(*f.driver)
	if driverValue == "" && flagWasSet(fs, "mysql-dsn") {
		driverValue = string(dialectMySQL)
	}
	if driverValue == "" {
		driverValue = env.get(envDBDriverName)
	}
	if driverValue == "" {
		driverValue = string(dialectSQLite)
	}

	switch dialect(strings.ToLower(driverValue)) {
	case dialectSQLite:
		return dbConfig{Driver: dialectSQLite, Path: resolveDBPath(fs, *f.path)}, nil
	case dialectMySQL:
		dsn, err := resolveMySQLDSN(f, env)
		if err != nil {
			return dbConfig{}, err
		}
		return dbConfig{Driver: dialectMySQL, MySQLDSN: dsn}, nil
	default:
		return dbConfig{}, fmt.Errorf("unsupported database driver %q (expected sqlite or mysql)", driverValue)
	}
}

func resolveMySQLDSN(f *dbFlags, env envLookup) (string, error) {
	pick := func(flagValue string, envName string) string {
		if v := strings.TrimSpace(flagValue); v != "" {
			return v
		}
		return env.get(envName)
	}

	tls, err := normalizeTLSMode(pick(*f.mysqlTLS, envMySQLTLSName))
	if err != nil {
		return "", err
	}

	if dsn := strings.TrimSpace(*f.mysqlDSN); dsn != "" {
		return normalizeMySQLDSN(dsn, tls)
	}
	if dsn := env.get(envMySQLDSNName); dsn != "" {
		return overrideMySQLDSN(dsn, f, tls)
	}

	user := pick(*f.mysqlUser, envMySQLUserName)
	password := pick(*f.mysqlPassword, envMySQLPasswordName)
	database := pick(*f.mysqlDatabase, envMySQLDatabaseName)
	host := pick(*f.mysqlHost, envMySQLHostName)
	port := pick(*f.mysqlPort, envMySQLPortName)
	if host == "" {
		host = defaultMySQLHost
	}
	if port == "" {
		port = defaultMySQLPort
	}
	if user == "" {
		return "", fmt.Errorf("mysql driver selected but no user configured; set --mysql-user or %s (or provide --mysql-dsn / %s)", envMySQLUserName, envMySQLDSNName)
	}
	if database == "" {
		return "", fmt.Errorf("mysql driver selected but no database configured; set --mysql-database or %s (or provide --mysql-dsn / %s)", envMySQLDatabaseName, envMySQLDSNName)
	}

	cfg := mysql.NewConfig()
	cfg.User = user
	cfg.Passwd = password
	cfg.Net = "tcp"
	cfg.Addr = host + ":" + port
	cfg.DBName = database
	if tls != "" {
		cfg.TLSConfig = tls
	}
	return cfg.FormatDSN(), nil
}

// normalizeTLSMode validates a --mysql-tls / GASOLINE_MYSQL_TLS value against
// the TLS modes go-sql-driver understands out of the box. An empty value leaves
// the DSN's own tls setting (if any) untouched.
func normalizeTLSMode(v string) (string, error) {
	switch mode := strings.ToLower(strings.TrimSpace(v)); mode {
	case "":
		return "", nil
	case "true", "false", "skip-verify", "preferred":
		return mode, nil
	default:
		return "", fmt.Errorf("invalid mysql TLS mode %q (expected true, false, skip-verify, or preferred)", v)
	}
}

// normalizeMySQLDSN validates a user-supplied DSN and rebuilds it through
// mysql.Config so driver defaults (native passwords, collation) apply. A
// non-empty tls overrides any tls setting already in the DSN.
func normalizeMySQLDSN(dsn, tls string) (string, error) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return "", fmt.Errorf("invalid mysql DSN: %w", err)
	}
	if cfg.DBName == "" {
		return "", errors.New("mysql DSN must include a database name, e.g. user:pass@tcp(host:3306)/gasoline")
	}
	if tls != "" {
		cfg.TLSConfig = tls
	}
	return cfg.FormatDSN(), nil
}

// overrideMySQLDSN overlays individual --mysql-* flags onto a DSN configured
// via the environment, so each field keeps the documented flag-beats-environment
// precedence.
func overrideMySQLDSN(dsn string, f *dbFlags, tls string) (string, error) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return "", fmt.Errorf("invalid mysql DSN: %w", err)
	}

	host := strings.TrimSpace(*f.mysqlHost)
	port := strings.TrimSpace(*f.mysqlPort)
	if host != "" || port != "" {
		currentHost, currentPort := defaultMySQLHost, defaultMySQLPort
		if cfg.Net == "tcp" {
			if h, p, err := net.SplitHostPort(cfg.Addr); err == nil {
				currentHost, currentPort = h, p
			} else if cfg.Addr != "" {
				currentHost = cfg.Addr
			}
		}
		if host == "" {
			host = currentHost
		}
		if port == "" {
			port = currentPort
		}
		cfg.Net = "tcp"
		cfg.Addr = net.JoinHostPort(host, port)
	}
	if user := strings.TrimSpace(*f.mysqlUser); user != "" {
		cfg.User = user
	}
	if password := strings.TrimSpace(*f.mysqlPassword); password != "" {
		cfg.Passwd = password
	}
	if database := strings.TrimSpace(*f.mysqlDatabase); database != "" {
		cfg.DBName = database
	}
	if tls != "" {
		cfg.TLSConfig = tls
	}
	if cfg.DBName == "" {
		return "", errors.New("mysql DSN must include a database name, e.g. user:pass@tcp(host:3306)/gasoline")
	}
	return cfg.FormatDSN(), nil
}

// envLookup reads configuration from the process environment with a fallback
// to the local .env file, mirroring how the Tankerkönig API key is loaded.
type envLookup struct {
	dotEnv map[string]string
}

func newEnvLookup() (envLookup, error) {
	values, err := loadDotEnv(".env")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return envLookup{}, err
	}
	return envLookup{dotEnv: values}, nil
}

func (e envLookup) get(name string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return strings.TrimSpace(e.dotEnv[name])
}

func openDatabase(ctx context.Context, cfg dbConfig) (*sql.DB, error) {
	switch cfg.Driver {
	case dialectMySQL:
		return openMySQL(ctx, cfg.MySQLDSN)
	default:
		return openDB(cfg.Path)
	}
}

func openDB(path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil && filepath.Dir(path) != "." {
		return nil, err
	}
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(%d)&_pragma=foreign_keys(1)", path, sqliteBusyTimeout)
	return sql.Open("sqlite", dsn)
}

func openMySQL(ctx context.Context, dsn string) (*sql.DB, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	// go-sql-driver recommended pool settings.
	db.SetConnMaxLifetime(3 * time.Minute)
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		db.Close()
		return nil, fmt.Errorf("cannot connect to mysql server: %w", err)
	}
	return db, nil
}

// schemaStatements returns the CREATE statements for the given dialect. The
// logical schema is identical; only column types and index syntax differ
// (MySQL needs bounded VARCHARs for indexed columns and does not support
// CREATE INDEX IF NOT EXISTS, so indexes are declared inline).
func schemaStatements(d dialect) []string {
	if d == dialectMySQL {
		return []string{
			`CREATE TABLE IF NOT EXISTS cities (
				name VARCHAR(255) PRIMARY KEY,
				normalized_name VARCHAR(255) NOT NULL,
				display_name TEXT NOT NULL,
				lat DOUBLE NOT NULL,
				lng DOUBLE NOT NULL,
				created_at VARCHAR(64) NOT NULL
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
			`CREATE TABLE IF NOT EXISTS stations (
				id VARCHAR(64) PRIMARY KEY,
				name TEXT NOT NULL,
				name_override TEXT,
				brand TEXT,
				street TEXT,
				house_number TEXT,
				post_code INTEGER,
				place TEXT,
				lat DOUBLE NOT NULL,
				lng DOUBLE NOT NULL,
				first_seen_at VARCHAR(64) NOT NULL,
				last_seen_at VARCHAR(64) NOT NULL,
				INDEX idx_stations_lat_lng (lat, lng)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
			`CREATE TABLE IF NOT EXISTS price_snapshots (
				id BIGINT PRIMARY KEY AUTO_INCREMENT,
				station_id VARCHAR(64) NOT NULL,
				city_name VARCHAR(255) NOT NULL,
				recorded_at VARCHAR(64) NOT NULL,
				search_radius_km DOUBLE NOT NULL DEFAULT 5,
				is_open TINYINT NOT NULL,
				e5 DOUBLE,
				e10 DOUBLE,
				diesel DOUBLE,
				INDEX idx_price_snapshots_station_recorded (station_id, recorded_at DESC),
				INDEX idx_price_snapshots_city_recorded (city_name, recorded_at DESC),
				FOREIGN KEY (station_id) REFERENCES stations(id)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
			`CREATE TABLE IF NOT EXISTS users (
				id BIGINT PRIMARY KEY AUTO_INCREMENT,
				email VARCHAR(255) NOT NULL UNIQUE,
				password_hash VARCHAR(255) NOT NULL,
				is_admin TINYINT NOT NULL DEFAULT 0,
				status VARCHAR(16) NOT NULL DEFAULT 'pending',
				created_at VARCHAR(64) NOT NULL,
				approved_at VARCHAR(64),
				notify_method VARCHAR(32) NOT NULL DEFAULT 'pushover',
				pushover_app_name VARCHAR(255) NOT NULL DEFAULT 'gasoline',
				pushover_user_key VARCHAR(64) NOT NULL DEFAULT '',
				pushover_token VARCHAR(64) NOT NULL DEFAULT '',
				notify_days VARCHAR(32) NOT NULL DEFAULT 'mon,tue,wed,thu,fri,sat,sun',
				notify_windows VARCHAR(255) NOT NULL DEFAULT '07:00-21:00',
				notify_suggest_times VARCHAR(255) NOT NULL DEFAULT '08:00,13:00',
				notify_check_enabled TINYINT NOT NULL DEFAULT 0,
				notify_suggest_enabled TINYINT NOT NULL DEFAULT 1,
				notify_last_suggest VARCHAR(20) NOT NULL DEFAULT '',
				notify_fuel VARCHAR(16) NOT NULL DEFAULT 'diesel'
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
			`CREATE TABLE IF NOT EXISTS settings (
				name VARCHAR(191) PRIMARY KEY,
				value TEXT NOT NULL,
				updated_at VARCHAR(64) NOT NULL
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
			`CREATE TABLE IF NOT EXISTS update_targets (
				id BIGINT PRIMARY KEY AUTO_INCREMENT,
				city VARCHAR(255) NOT NULL UNIQUE,
				radius_km DOUBLE NOT NULL DEFAULT 5,
				created_at VARCHAR(64) NOT NULL
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
			`CREATE TABLE IF NOT EXISTS user_notify_cities (
				user_id BIGINT NOT NULL,
				city VARCHAR(255) NOT NULL,
				created_at VARCHAR(64) NOT NULL,
				PRIMARY KEY (user_id, city),
				FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
				FOREIGN KEY (city) REFERENCES update_targets(city) ON DELETE CASCADE
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
			`CREATE TABLE IF NOT EXISTS notification_state (
				name VARCHAR(191) PRIMARY KEY,
				value TEXT NOT NULL,
				updated_at VARCHAR(64) NOT NULL
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
			`CREATE TABLE IF NOT EXISTS prediction_runs (
				id BIGINT PRIMARY KEY AUTO_INCREMENT,
				run_at VARCHAR(64) NOT NULL,
				city_name VARCHAR(255) NOT NULL,
				fuel VARCHAR(16) NOT NULL,
				range_km DOUBLE NOT NULL,
				history_days INTEGER NOT NULL,
				predict_days INTEGER NOT NULL,
				jump_anchor_hour INTEGER NOT NULL DEFAULT 0,
				station_count INTEGER NOT NULL DEFAULT 0,
				INDEX idx_prediction_runs_city_fuel_run (city_name, fuel, run_at DESC)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
			`CREATE TABLE IF NOT EXISTS price_predictions (
				id BIGINT PRIMARY KEY AUTO_INCREMENT,
				run_id BIGINT NOT NULL,
				station_id VARCHAR(64) NOT NULL,
				fuel VARCHAR(16) NOT NULL,
				target_start VARCHAR(64) NOT NULL,
				target_end VARCHAR(64) NOT NULL,
				predicted_price DOUBLE NOT NULL,
				baseline DOUBLE,
				confidence VARCHAR(16) NOT NULL,
				sample_count INTEGER NOT NULL DEFAULT 0,
				is_suggestion TINYINT NOT NULL DEFAULT 0,
				lead_minutes INTEGER NOT NULL DEFAULT 0,
				actual_price DOUBLE,
				error DOUBLE,
				evaluated_at VARCHAR(64),
				INDEX idx_price_predictions_station_fuel_target (station_id, fuel, target_start DESC),
				INDEX idx_price_predictions_due (fuel, evaluated_at, target_end),
				FOREIGN KEY (run_id) REFERENCES prediction_runs(id),
				FOREIGN KEY (station_id) REFERENCES stations(id)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`,
		}
	}
	return []string{
		`CREATE TABLE IF NOT EXISTS cities (
			name TEXT PRIMARY KEY,
			normalized_name TEXT NOT NULL,
			display_name TEXT NOT NULL,
			lat REAL NOT NULL,
			lng REAL NOT NULL,
			created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS stations (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			name_override TEXT,
			brand TEXT,
			street TEXT,
			house_number TEXT,
			post_code INTEGER,
			place TEXT,
			lat REAL NOT NULL,
			lng REAL NOT NULL,
			first_seen_at TEXT NOT NULL,
			last_seen_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS price_snapshots (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			station_id TEXT NOT NULL,
			city_name TEXT NOT NULL,
			recorded_at TEXT NOT NULL,
			search_radius_km REAL NOT NULL DEFAULT 5,
			is_open INTEGER NOT NULL,
			e5 REAL,
			e10 REAL,
			diesel REAL,
			FOREIGN KEY (station_id) REFERENCES stations(id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_price_snapshots_station_recorded
			ON price_snapshots(station_id, recorded_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_price_snapshots_city_recorded
			ON price_snapshots(city_name, recorded_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_stations_lat_lng
			ON stations(lat, lng)`,
		`CREATE TABLE IF NOT EXISTS users (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			email TEXT NOT NULL UNIQUE,
			password_hash TEXT NOT NULL,
			is_admin INTEGER NOT NULL DEFAULT 0,
			status TEXT NOT NULL DEFAULT 'pending',
			created_at TEXT NOT NULL,
			approved_at TEXT,
			notify_method TEXT NOT NULL DEFAULT 'pushover',
			pushover_app_name TEXT NOT NULL DEFAULT 'gasoline',
			pushover_user_key TEXT NOT NULL DEFAULT '',
			pushover_token TEXT NOT NULL DEFAULT '',
			notify_days TEXT NOT NULL DEFAULT 'mon,tue,wed,thu,fri,sat,sun',
			notify_windows TEXT NOT NULL DEFAULT '07:00-21:00',
			notify_suggest_times TEXT NOT NULL DEFAULT '08:00,13:00',
			notify_check_enabled INTEGER NOT NULL DEFAULT 0,
			notify_suggest_enabled INTEGER NOT NULL DEFAULT 1,
			notify_last_suggest TEXT NOT NULL DEFAULT '',
			notify_fuel TEXT NOT NULL DEFAULT 'diesel'
		)`,
		`CREATE TABLE IF NOT EXISTS settings (
			name TEXT PRIMARY KEY,
			value TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS update_targets (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			city TEXT NOT NULL UNIQUE,
			radius_km REAL NOT NULL DEFAULT 5,
			created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS user_notify_cities (
			user_id INTEGER NOT NULL,
			city TEXT NOT NULL,
			created_at TEXT NOT NULL,
			PRIMARY KEY (user_id, city),
			FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
			FOREIGN KEY (city) REFERENCES update_targets(city) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS notification_state (
			name TEXT PRIMARY KEY,
			value TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS prediction_runs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			run_at TEXT NOT NULL,
			city_name TEXT NOT NULL,
			fuel TEXT NOT NULL,
			range_km REAL NOT NULL,
			history_days INTEGER NOT NULL,
			predict_days INTEGER NOT NULL,
			jump_anchor_hour INTEGER NOT NULL DEFAULT 0,
			station_count INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_prediction_runs_city_fuel_run
			ON prediction_runs(city_name, fuel, run_at DESC)`,
		`CREATE TABLE IF NOT EXISTS price_predictions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			run_id INTEGER NOT NULL,
			station_id TEXT NOT NULL,
			fuel TEXT NOT NULL,
			target_start TEXT NOT NULL,
			target_end TEXT NOT NULL,
			predicted_price REAL NOT NULL,
			baseline REAL,
			confidence TEXT NOT NULL,
			sample_count INTEGER NOT NULL DEFAULT 0,
			is_suggestion INTEGER NOT NULL DEFAULT 0,
			lead_minutes INTEGER NOT NULL DEFAULT 0,
			actual_price REAL,
			error REAL,
			evaluated_at TEXT,
			FOREIGN KEY (run_id) REFERENCES prediction_runs(id),
			FOREIGN KEY (station_id) REFERENCES stations(id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_price_predictions_station_fuel_target
			ON price_predictions(station_id, fuel, target_start DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_price_predictions_due
			ON price_predictions(fuel, evaluated_at, target_end)`,
		`CREATE INDEX IF NOT EXISTS idx_price_predictions_run
			ON price_predictions(run_id)`,
	}
}

// stationsUpsertSQL upserts one station keyed by id.
func stationsUpsertSQL(d dialect) string {
	if d == dialectMySQL {
		return `
			INSERT INTO stations (
				id, name, brand, street, house_number, post_code, place, lat, lng, first_seen_at, last_seen_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON DUPLICATE KEY UPDATE
				name = VALUES(name),
				brand = VALUES(brand),
				street = VALUES(street),
				house_number = VALUES(house_number),
				post_code = VALUES(post_code),
				place = VALUES(place),
				lat = VALUES(lat),
				lng = VALUES(lng),
				last_seen_at = VALUES(last_seen_at)`
	}
	return `
		INSERT INTO stations (
			id, name, brand, street, house_number, post_code, place, lat, lng, first_seen_at, last_seen_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			name = excluded.name,
			brand = excluded.brand,
			street = excluded.street,
			house_number = excluded.house_number,
			post_code = excluded.post_code,
			place = excluded.place,
			lat = excluded.lat,
			lng = excluded.lng,
			last_seen_at = excluded.last_seen_at`
}

// citiesUpsertSQL upserts one city keyed by name.
func citiesUpsertSQL(d dialect) string {
	if d == dialectMySQL {
		return `
			INSERT INTO cities (name, normalized_name, display_name, lat, lng, created_at)
			VALUES (?, ?, ?, ?, ?, ?)
			ON DUPLICATE KEY UPDATE
				normalized_name = VALUES(normalized_name),
				display_name = VALUES(display_name),
				lat = VALUES(lat),
				lng = VALUES(lng)`
	}
	return `
		INSERT INTO cities (name, normalized_name, display_name, lat, lng, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			normalized_name = excluded.normalized_name,
			display_name = excluded.display_name,
			lat = excluded.lat,
			lng = excluded.lng`
}

// citiesInsertIgnoreSQL inserts a city only if its name is not cached yet.
func citiesInsertIgnoreSQL(d dialect) string {
	if d == dialectMySQL {
		return `INSERT IGNORE INTO cities (name, normalized_name, display_name, lat, lng, created_at) VALUES (?, ?, ?, ?, ?, ?)`
	}
	return `INSERT OR IGNORE INTO cities (name, normalized_name, display_name, lat, lng, created_at) VALUES (?, ?, ?, ?, ?, ?)`
}

// kvUpsertSQL upserts one name/value row into a key-value table (settings or
// notification_state).
func kvUpsertSQL(d dialect, table string) string {
	if d == dialectMySQL {
		return `INSERT INTO ` + table + ` (name, value, updated_at) VALUES (?, ?, ?)
			ON DUPLICATE KEY UPDATE value = VALUES(value), updated_at = VALUES(updated_at)`
	}
	return `INSERT INTO ` + table + ` (name, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`
}

// kvInsertIgnoreSQL inserts a name/value row only when the name is not present
// yet — used to seed defaults without overwriting admin edits.
func kvInsertIgnoreSQL(d dialect, table string) string {
	if d == dialectMySQL {
		return `INSERT IGNORE INTO ` + table + ` (name, value, updated_at) VALUES (?, ?, ?)`
	}
	return `INSERT OR IGNORE INTO ` + table + ` (name, value, updated_at) VALUES (?, ?, ?)`
}

// queryLimit converts "0 = no limit" into the dialect's unlimited LIMIT value.
func queryLimit(d dialect, limit int) int64 {
	if limit != 0 {
		return int64(limit)
	}
	if d == dialectMySQL {
		return math.MaxInt64
	}
	return -1
}
