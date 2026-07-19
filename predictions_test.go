package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func insertPredictionRunRow(t *testing.T, db *sql.DB, runAt time.Time) int64 {
	t.Helper()
	result, err := db.ExecContext(context.Background(), `
		INSERT INTO prediction_runs (run_at, city_name, fuel, range_km, history_days, predict_days, jump_anchor_hour, station_count)
		VALUES (?, 'Berlin', 'diesel', 5, 30, 3, 12, 1)
	`, runAt.UTC().Format(time.RFC3339))
	if err != nil {
		t.Fatalf("insert prediction run: %v", err)
	}
	runID, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("run id: %v", err)
	}
	return runID
}

func insertPredictionRow(t *testing.T, db *sql.DB, runID int64, stationID string, targetStart time.Time, predicted float64, leadMinutes int) int64 {
	t.Helper()
	result, err := db.ExecContext(context.Background(), `
		INSERT INTO price_predictions (run_id, station_id, fuel, target_start, target_end, predicted_price, confidence, sample_count, is_suggestion, lead_minutes)
		VALUES (?, ?, 'diesel', ?, ?, ?, 'low', 1, 0, ?)
	`, runID, stationID, targetStart.UTC().Format(time.RFC3339), targetStart.Add(time.Hour).UTC().Format(time.RFC3339), predicted, leadMinutes)
	if err != nil {
		t.Fatalf("insert prediction: %v", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("prediction id: %v", err)
	}
	return id
}

func markPredictionEvaluated(t *testing.T, db *sql.DB, id int64, predictionError float64, evaluatedAt time.Time) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), `
		UPDATE price_predictions SET actual_price = predicted_price + ?, error = ?, evaluated_at = ? WHERE id = ?
	`, predictionError, predictionError, evaluatedAt.UTC().Format(time.RFC3339), id); err != nil {
		t.Fatalf("mark evaluated: %v", err)
	}
}

func TestPersistPredictionRunStoresGridAndFlagsSuggestions(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	city := cachedCity{QueryName: "Berlin", Name: "Berlin", DisplayName: "Berlin", Lat: 52.517389, Lng: 13.395131}
	insertSuggestCity(t, db, city)
	insertSuggestStation(t, db, "station-1", "Station 1", 52.517389, 13.395131)
	for day := 10; day <= 24; day++ {
		insertSawtoothDay(t, db, "station-1", "Berlin", time.Date(2026, 4, day, 0, 0, 0, 0, time.UTC), 2.00)
	}

	opts := suggestOptions{
		City:        "Berlin",
		RangeKM:     5,
		Fuel:        "diesel",
		HistoryDays: 30,
		PredictDays: 1,
		LimitPerDay: 1,
		Now:         time.Date(2026, 4, 25, 9, 30, 0, 0, time.UTC),
		Location:    time.UTC,
	}
	computation, err := computeSuggestions(ctx, db, opts)
	if err != nil {
		t.Fatalf("computeSuggestions: %v", err)
	}
	persisted, err := persistPredictionRun(ctx, db, computation, opts)
	if err != nil {
		t.Fatalf("persistPredictionRun: %v", err)
	}
	// 10:00 through 23:00 of the remaining day.
	if persisted != 14 {
		t.Fatalf("persisted = %d, want 14 grid rows", persisted)
	}

	var (
		anchor, stationCount int
		cityName             string
	)
	if err := db.QueryRowContext(ctx, `SELECT jump_anchor_hour, station_count, city_name FROM prediction_runs`).Scan(&anchor, &stationCount, &cityName); err != nil {
		t.Fatalf("read run: %v", err)
	}
	if anchor != 12 || stationCount != 1 || cityName != "Berlin" {
		t.Fatalf("run = anchor %d, stations %d, city %s; want 12/1/Berlin", anchor, stationCount, cityName)
	}

	rows, err := db.QueryContext(ctx, `SELECT target_start, lead_minutes, baseline FROM price_predictions WHERE is_suggestion = 1`)
	if err != nil {
		t.Fatalf("read suggestions: %v", err)
	}
	defer rows.Close()
	var flagged []string
	for rows.Next() {
		var (
			targetStart string
			leadMinutes int
			baseline    sql.NullFloat64
		)
		if err := rows.Scan(&targetStart, &leadMinutes, &baseline); err != nil {
			t.Fatalf("scan suggestion row: %v", err)
		}
		flagged = append(flagged, targetStart)
		if leadMinutes != 90 {
			t.Fatalf("lead_minutes = %d, want 90 (09:30 -> 11:00)", leadMinutes)
		}
		if !baseline.Valid || baseline.Float64 < 1.94 || baseline.Float64 > 1.96 {
			t.Fatalf("baseline = %+v, want ~1.95", baseline)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(flagged) != 1 || flagged[0] != "2026-04-25T11:00:00Z" {
		t.Fatalf("flagged suggestion targets = %v, want exactly the 11:00 window", flagged)
	}
}

func TestEvaluateDuePredictionsFillsActualsAndErrors(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	insertSuggestStation(t, db, "station-1", "Station 1", 52.517389, 13.395131)
	day := time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC)
	insertSuggestSnapshot(t, db, "station-1", "Berlin", day.Add(8*time.Hour), 1.80, true)
	// Station closes at noon: predictions in the closed window get no actual.
	insertSuggestSnapshot(t, db, "station-1", "Berlin", day.Add(12*time.Hour), 1.80, false)

	runID := insertPredictionRunRow(t, db, day.Add(7*time.Hour))
	duePast := insertPredictionRow(t, db, runID, "station-1", day.Add(8*time.Hour), 1.85, 60)
	dueClosed := insertPredictionRow(t, db, runID, "station-1", day.Add(12*time.Hour), 1.85, 300)
	future := insertPredictionRow(t, db, runID, "station-1", day.Add(20*time.Hour), 1.85, 780)

	now := day.Add(13*time.Hour + 30*time.Minute)
	measured, err := evaluateDuePredictions(ctx, db, "diesel", now)
	if err != nil {
		t.Fatalf("evaluateDuePredictions: %v", err)
	}
	if measured != 1 {
		t.Fatalf("measured = %d, want 1", measured)
	}

	var (
		actual, predictionError sql.NullFloat64
		evaluatedAt             sql.NullString
	)
	readRow := func(id int64) {
		t.Helper()
		if err := db.QueryRowContext(ctx, `SELECT actual_price, error, evaluated_at FROM price_predictions WHERE id = ?`, id).
			Scan(&actual, &predictionError, &evaluatedAt); err != nil {
			t.Fatalf("read prediction %d: %v", id, err)
		}
	}

	readRow(duePast)
	if !actual.Valid || actual.Float64 != 1.80 {
		t.Fatalf("actual = %+v, want 1.80", actual)
	}
	if !predictionError.Valid || predictionError.Float64 < -0.051 || predictionError.Float64 > -0.049 {
		t.Fatalf("error = %+v, want -0.05", predictionError)
	}
	if !evaluatedAt.Valid {
		t.Fatal("due prediction not marked evaluated")
	}

	readRow(dueClosed)
	if actual.Valid || predictionError.Valid {
		t.Fatalf("closed-window prediction got actual %+v error %+v, want NULL", actual, predictionError)
	}
	if !evaluatedAt.Valid {
		t.Fatal("closed-window prediction must still be marked evaluated")
	}

	readRow(future)
	if evaluatedAt.Valid {
		t.Fatal("future prediction must stay unevaluated")
	}
}

func TestLoadPredictionBiasCapsAndFilters(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	for _, station := range []string{"biased", "sparse", "long-lead", "stale"} {
		insertSuggestStation(t, db, station, station, 52.5, 13.4)
	}
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	runID := insertPredictionRunRow(t, db, now.AddDate(0, 0, -2))

	target := now.AddDate(0, 0, -1)
	for i := 0; i < 6; i++ {
		// Consistent +5 cent under-prediction: bias must engage but stay capped.
		id := insertPredictionRow(t, db, runID, "biased", target.Add(time.Duration(i)*time.Hour), 2.00, 60)
		markPredictionEvaluated(t, db, id, 0.05, now.AddDate(0, 0, -1))
	}
	for i := 0; i < predictionBiasMinSamples-1; i++ {
		id := insertPredictionRow(t, db, runID, "sparse", target.Add(time.Duration(i)*time.Hour), 2.00, 60)
		markPredictionEvaluated(t, db, id, 0.05, now.AddDate(0, 0, -1))
	}
	for i := 0; i < 6; i++ {
		id := insertPredictionRow(t, db, runID, "long-lead", target.Add(time.Duration(i)*time.Hour), 2.00, 1000)
		markPredictionEvaluated(t, db, id, 0.05, now.AddDate(0, 0, -1))
	}
	for i := 0; i < 6; i++ {
		id := insertPredictionRow(t, db, runID, "stale", target.Add(time.Duration(i)*time.Hour), 2.00, 60)
		markPredictionEvaluated(t, db, id, 0.05, now.AddDate(0, 0, -20))
	}

	bias, err := loadPredictionBias(ctx, db, "diesel", now)
	if err != nil {
		t.Fatalf("loadPredictionBias: %v", err)
	}
	if len(bias) != 1 {
		t.Fatalf("bias = %+v, want only the consistently biased station", bias)
	}
	if got := bias["biased"]; got != predictionBiasMaxAbs {
		t.Fatalf("bias = %.4f, want capped at %.2f", got, predictionBiasMaxAbs)
	}

	model := forecastModel{Stations: map[string]forecastStation{"biased": {}}}
	if err := applyPredictionBias(ctx, db, &model, "diesel", now); err != nil {
		t.Fatalf("applyPredictionBias: %v", err)
	}
	if got := model.Stations["biased"].BiasCorrection; got != predictionBiasMaxAbs {
		t.Fatalf("BiasCorrection = %.4f, want %.2f", got, predictionBiasMaxAbs)
	}
}

func TestPrunePredictionsEnforcesRetention(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	insertSuggestStation(t, db, "station-1", "Station 1", 52.5, 13.4)
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	oldRun := insertPredictionRunRow(t, db, now.AddDate(0, 0, -40))
	insertPredictionRow(t, db, oldRun, "station-1", now.AddDate(0, 0, -35), 2.00, 60)
	freshRun := insertPredictionRunRow(t, db, now.AddDate(0, 0, -1))
	insertPredictionRow(t, db, freshRun, "station-1", now.AddDate(0, 0, -1), 2.00, 60)

	pruned, err := prunePredictions(ctx, db, now)
	if err != nil {
		t.Fatalf("prunePredictions: %v", err)
	}
	if pruned != 1 {
		t.Fatalf("pruned = %d, want 1", pruned)
	}

	var predictions, runs int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM price_predictions`).Scan(&predictions); err != nil {
		t.Fatalf("count predictions: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM prediction_runs`).Scan(&runs); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if predictions != 1 || runs != 1 {
		t.Fatalf("remaining predictions/runs = %d/%d, want 1/1 (empty run pruned)", predictions, runs)
	}
}

func TestRunSuggestPersistEndToEnd(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "persist.db")
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	ctx := context.Background()
	if err := initSchema(ctx, db, dialectSQLite); err != nil {
		t.Fatalf("initSchema: %v", err)
	}
	city := cachedCity{QueryName: "Berlin", Name: "Berlin", DisplayName: "Berlin", Lat: 52.517389, Lng: 13.395131}
	insertSuggestCity(t, db, city)
	// Two stations and three predict days push the grid past one insert
	// batch, so the flush boundary is exercised too.
	insertSuggestStation(t, db, "station-1", "Station 1", 52.517389, 13.395131)
	insertSuggestStation(t, db, "station-2", "Station 2", 52.518000, 13.396000)

	nowLocal := time.Now().In(time.Local)
	for daysAgo := 15; daysAgo >= 1; daysAgo-- {
		day := localDayStart(nowLocal).AddDate(0, 0, -daysAgo)
		insertSawtoothDay(t, db, "station-1", "Berlin", day.In(time.UTC), 2.00)
		insertSawtoothDay(t, db, "station-2", "Berlin", day.In(time.UTC), 2.10)
	}
	// A prediction from a past run that is due for evaluation now.
	pastRun := insertPredictionRunRow(t, db, nowLocal.Add(-3*time.Hour))
	due := insertPredictionRow(t, db, pastRun, "station-1", localHourStart(nowLocal).Add(-2*time.Hour), 1.95, 60)
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	output := captureStdout(t, func() error {
		return run([]string{"suggest", "--db", dbPath, "--persist", "--city", "Berlin", "--fuel", "diesel", "--history-days", "30", "--predict-days", "3", "--limit-per-day", "2", "--output", "json"})
	})
	var suggestions []suggestionRow
	if err := json.Unmarshal([]byte(output), &suggestions); err != nil {
		t.Fatalf("unmarshal suggest output: %v\noutput=%s", err, output)
	}
	if len(suggestions) == 0 {
		t.Fatal("no suggestions returned")
	}

	db, err = openDB(dbPath)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer db.Close()

	var runs int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM prediction_runs`).Scan(&runs); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runs != 2 {
		t.Fatalf("prediction_runs = %d, want 2 (seeded + persisted)", runs)
	}
	var futureRows, flagged int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(is_suggestion), 0) FROM price_predictions WHERE evaluated_at IS NULL`).Scan(&futureRows, &flagged); err != nil {
		t.Fatalf("count grid rows: %v", err)
	}
	if futureRows <= persistInsertBatch {
		t.Fatalf("future grid rows = %d, want more than one insert batch (%d)", futureRows, persistInsertBatch)
	}
	if flagged == 0 {
		t.Fatal("no persisted rows flagged as suggestions")
	}
	var evaluatedAt sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT evaluated_at FROM price_predictions WHERE id = ?`, due).Scan(&evaluatedAt); err != nil {
		t.Fatalf("read due prediction: %v", err)
	}
	if !evaluatedAt.Valid {
		t.Fatal("due prediction from the past run was not evaluated")
	}
}
