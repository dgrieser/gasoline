package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
)

// copyBatchSize rows per multi-row INSERT while copying to MySQL.
const copyBatchSize = 500

var migrationTables = []struct {
	name    string
	columns []string
}{
	{"cities", []string{"name", "normalized_name", "display_name", "lat", "lng", "created_at"}},
	{"stations", []string{"id", "name", "name_override", "brand", "street", "house_number", "post_code", "place", "lat", "lng", "first_seen_at", "last_seen_at"}},
	// id is copied so ties in recorded_at keep their original order
	// (compaction and latest-snapshot queries break ties by id).
	{"price_snapshots", []string{"id", "station_id", "city_name", "recorded_at", "search_radius_km", "is_open", "e5", "e10", "diesel"}},
}

type mysqlMigrationResult struct {
	Source         string `json:"source"`
	Target         string `json:"target"`
	Cities         int    `json:"cities"`
	Stations       int    `json:"stations"`
	PriceSnapshots int    `json:"price_snapshots"`
	Overwritten    bool   `json:"overwritten"`
}

func runMigrateToMySQL(args []string) error {
	fs := flag.NewFlagSet("migrate-to-mysql", flag.ContinueOnError)
	dbf := addDBFlags(fs)
	overwrite := fs.Bool("overwrite", false, "Delete existing rows in the MySQL target before copying")
	outputLong, outputShort := addOutputFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	output, err := resolveOutputMode(*outputLong, *outputShort)
	if err != nil {
		return err
	}
	if driver := strings.TrimSpace(*dbf.driver); driver != "" && dialect(strings.ToLower(driver)) != dialectMySQL {
		return errors.New("migrate-to-mysql always reads from the SQLite file (--db) and writes to MySQL; --db-driver is not needed")
	}

	sourcePath := resolveDBPath(fs, *dbf.path)
	if _, err := os.Stat(sourcePath); err != nil {
		return fmt.Errorf("source SQLite database %s does not exist", sourcePath)
	}

	env, err := newEnvLookup()
	if err != nil {
		return err
	}
	dsn, err := resolveMySQLDSN(dbf, env)
	if err != nil {
		return err
	}
	targetCfg := dbConfig{Driver: dialectMySQL, MySQLDSN: dsn}

	ctx := context.Background()

	src, err := openDB(sourcePath)
	if err != nil {
		return err
	}
	defer src.Close()
	// Bring the source up to the current schema so both sides copy the same columns.
	if err := initSchema(ctx, src, dialectSQLite); err != nil {
		return err
	}

	dst, err := openDatabase(ctx, targetCfg)
	if err != nil {
		return err
	}
	defer dst.Close()
	if err := initSchema(ctx, dst, dialectMySQL); err != nil {
		return err
	}

	result, err := copySQLiteToMySQL(ctx, src, dst, *overwrite)
	if err != nil {
		return err
	}
	result.Source = sourcePath
	result.Target = targetCfg.Description()

	if output == outputJSON {
		return writeJSON(result)
	}
	fmt.Fprintf(stdout, "migrated %s to %s\n", result.Source, result.Target)
	fmt.Fprintf(stdout, "cities: %d\n", result.Cities)
	fmt.Fprintf(stdout, "stations: %d\n", result.Stations)
	fmt.Fprintf(stdout, "price snapshots: %d\n", result.PriceSnapshots)
	return nil
}

func copySQLiteToMySQL(ctx context.Context, src, dst *sql.DB, overwrite bool) (mysqlMigrationResult, error) {
	tx, err := dst.BeginTx(ctx, nil)
	if err != nil {
		return mysqlMigrationResult{}, err
	}
	defer tx.Rollback()

	existing := make(map[string]int, len(migrationTables))
	total := 0
	for _, table := range migrationTables {
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table.name).Scan(&count); err != nil {
			return mysqlMigrationResult{}, err
		}
		existing[table.name] = count
		total += count
	}
	if total > 0 && !overwrite {
		return mysqlMigrationResult{}, fmt.Errorf(
			"target already contains data (cities=%d stations=%d price_snapshots=%d); rerun with --overwrite to replace it",
			existing["cities"], existing["stations"], existing["price_snapshots"])
	}
	overwritten := total > 0
	if overwritten {
		// Delete children before parents to satisfy the station foreign key.
		for i := len(migrationTables) - 1; i >= 0; i-- {
			if _, err := tx.ExecContext(ctx, "DELETE FROM "+migrationTables[i].name); err != nil {
				return mysqlMigrationResult{}, err
			}
		}
	}

	counts := make(map[string]int, len(migrationTables))
	for _, table := range migrationTables {
		copied, err := copyTable(ctx, src, tx, table.name, table.columns)
		if err != nil {
			return mysqlMigrationResult{}, fmt.Errorf("copy %s: %w", table.name, err)
		}
		counts[table.name] = copied
	}

	if err := tx.Commit(); err != nil {
		return mysqlMigrationResult{}, err
	}
	return mysqlMigrationResult{
		Cities:         counts["cities"],
		Stations:       counts["stations"],
		PriceSnapshots: counts["price_snapshots"],
		Overwritten:    overwritten,
	}, nil
}

func copyTable(ctx context.Context, src *sql.DB, tx *sql.Tx, table string, columns []string) (int, error) {
	columnList := strings.Join(columns, ", ")
	rows, err := src.QueryContext(ctx, fmt.Sprintf("SELECT %s FROM %s", columnList, table))
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	rowPlaceholder := "(" + strings.TrimSuffix(strings.Repeat("?, ", len(columns)), ", ") + ")"
	var (
		args      []any
		batchRows int
		copied    int
	)
	flush := func() error {
		if batchRows == 0 {
			return nil
		}
		placeholders := strings.TrimSuffix(strings.Repeat(rowPlaceholder+", ", batchRows), ", ")
		insert := fmt.Sprintf("INSERT INTO %s (%s) VALUES %s", table, columnList, placeholders)
		if _, err := tx.ExecContext(ctx, insert, args...); err != nil {
			return err
		}
		copied += batchRows
		args = args[:0]
		batchRows = 0
		return nil
	}

	// Scan buffers are reused across rows: append copies the interface values
	// into args, and database/sql clones []byte sources scanned into *any.
	values := make([]any, len(columns))
	pointers := make([]any, len(columns))
	for i := range values {
		pointers[i] = &values[i]
	}
	for rows.Next() {
		if err := rows.Scan(pointers...); err != nil {
			return 0, err
		}
		args = append(args, values...)
		batchRows++
		if batchRows >= copyBatchSize {
			if err := flush(); err != nil {
				return 0, err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if err := flush(); err != nil {
		return 0, err
	}
	return copied, nil
}
