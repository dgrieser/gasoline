package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
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
	// applied_correction 0: the row is version-compatible with the current
	// learning (its raw model error equals its stored error).
	result, err := db.ExecContext(context.Background(), `
		INSERT INTO price_predictions (run_id, station_id, fuel, target_start, target_end, predicted_price, confidence, sample_count, is_suggestion, lead_minutes, applied_correction)
		VALUES (?, ?, 'diesel', ?, ?, ?, 'low', 1, 0, ?, 0)
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

func setPredictionAppliedCorrection(t *testing.T, db *sql.DB, id int64, correction any) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), `
		UPDATE price_predictions SET applied_correction = ? WHERE id = ?
	`, correction, id); err != nil {
		t.Fatalf("set applied_correction: %v", err)
	}
}

func markPredictionEvaluated(t *testing.T, db *sql.DB, id int64, predictionError float64, evaluatedAt time.Time) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), `
		UPDATE price_predictions SET actual_price = predicted_price + ?, error = ?, evaluated_at = ? WHERE id = ?
	`, predictionError, predictionError, evaluatedAt.UTC().Format(time.RFC3339), id); err != nil {
		t.Fatalf("mark evaluated: %v", err)
	}
}

func markPredictionSuggestion(t *testing.T, db *sql.DB, id int64) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), `
		UPDATE price_predictions SET is_suggestion = 1 WHERE id = ?
	`, id); err != nil {
		t.Fatalf("mark suggestion: %v", err)
	}
}

// buildDecisionFixture sets up one sawtooth station and returns the persisted
// run id plus the options used, so decision tests share one arrangement.
func buildDecisionFixture(t *testing.T, db *sql.DB, now time.Time) (int64, *suggestComputation, suggestOptions) {
	t.Helper()
	ctx := context.Background()
	city := cachedCity{QueryName: "Berlin", Name: "Berlin", DisplayName: "Berlin", Lat: 52.517389, Lng: 13.395131}
	insertSuggestCity(t, db, city)
	insertSuggestStation(t, db, "station-1", "Station 1", 52.517389, 13.395131)
	for day := 10; day <= 24; day++ {
		insertSawtoothDay(t, db, "station-1", "Berlin", time.Date(2026, 4, day, 0, 0, 0, 0, time.UTC), 2.00)
	}
	opts := suggestOptions{
		Fuel:        "diesel",
		HistoryDays: 30,
		PredictDays: 1,
		LimitPerDay: 1,
		Now:         now,
		Location:    time.UTC,
	}
	computation, err := computeSuggestions(ctx, db, opts)
	if err != nil {
		t.Fatalf("computeSuggestions: %v", err)
	}
	runID, _, err := persistPredictionRun(ctx, db, computation, opts)
	if err != nil {
		t.Fatalf("persistPredictionRun: %v", err)
	}
	return runID, computation, opts
}

func TestPersistCheckDecisionsRecordsVerdictAndError(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 4, 25, 9, 30, 0, 0, time.UTC)
	runID, computation, opts := buildDecisionFixture(t, db, now)

	stored, err := persistCheckDecisions(ctx, db, computation, opts, runID)
	if err != nil {
		t.Fatalf("persistCheckDecisions: %v", err)
	}
	if stored != 1 {
		t.Fatalf("stored = %d, want 1 decision", stored)
	}

	var (
		gotRunID                      int64
		fuel, verdict, recommendation string
		targetStart, targetEnd        string
		observed, predicted, errValue float64
		outcomeEvaluated              sql.NullString
		floor                         sql.NullFloat64
	)
	if err := db.QueryRowContext(ctx, `
		SELECT run_id, fuel, verdict, recommendation, target_start, target_end,
			observed_price, predicted_price, error, outcome_evaluated_at, day_floor_price
		FROM price_check_decisions`).Scan(&gotRunID, &fuel, &verdict, &recommendation,
		&targetStart, &targetEnd, &observed, &predicted, &errValue, &outcomeEvaluated, &floor); err != nil {
		t.Fatalf("read decision: %v", err)
	}
	if gotRunID != runID {
		t.Fatalf("run_id = %d, want %d (decisions must hang off the same run)", gotRunID, runID)
	}
	if fuel != "diesel" {
		t.Fatalf("fuel = %q, want diesel", fuel)
	}
	// The decision targets the current hour, not the next one.
	if targetStart != "2026-04-25T09:00:00Z" || targetEnd != "2026-04-25T10:00:00Z" {
		t.Fatalf("window = %s..%s, want 09:00..10:00", targetStart, targetEnd)
	}
	if got := observed - predicted; math.Abs(errValue-got) > 1e-9 {
		t.Fatalf("error = %v, want observed-predicted = %v", errValue, got)
	}
	if !isSuggestVerdict(verdict) {
		t.Fatalf("verdict = %q, want one of low/typical/high", verdict)
	}
	if recommendation != "buy" && recommendation != "hold" && recommendation != "wait" {
		t.Fatalf("recommendation = %q, want buy/hold/wait", recommendation)
	}
	// Outcome columns stay empty until the pricing day is complete.
	if outcomeEvaluated.Valid || floor.Valid {
		t.Fatalf("outcome prefilled: evaluated=%+v floor=%+v", outcomeEvaluated, floor)
	}
}

func isSuggestVerdict(v string) bool {
	return v == "low" || v == "typical" || v == "high"
}

func TestEvaluateCheckOutcomesScoresAgainstPricingDayFloor(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 4, 25, 9, 30, 0, 0, time.UTC)
	runID, computation, opts := buildDecisionFixture(t, db, now)
	if _, err := persistCheckDecisions(ctx, db, computation, opts, runID); err != nil {
		t.Fatalf("persistCheckDecisions: %v", err)
	}

	// Not due yet: the pricing day containing 09:00 has not finished.
	settled, err := evaluateCheckOutcomes(ctx, db, "diesel", now.Add(2*time.Hour), time.UTC)
	if err != nil {
		t.Fatalf("evaluateCheckOutcomes (early): %v", err)
	}
	if settled != 0 {
		t.Fatalf("settled = %d before the pricing day closed, want 0", settled)
	}

	// The anchor is 12:00, so the decision at 09:00 belongs to the pricing day
	// starting 2026-04-24T12:00Z. Put a clear floor inside that window and a
	// cheaper price outside it that must be ignored.
	insertSuggestSnapshot(t, db, "station-1", "Berlin", time.Date(2026, 4, 25, 5, 0, 0, 0, time.UTC), 1.500, true)
	insertSuggestSnapshot(t, db, "station-1", "Berlin", time.Date(2026, 4, 25, 13, 0, 0, 0, time.UTC), 1.000, true)
	// A closed station must not supply the floor either.
	insertSuggestSnapshot(t, db, "station-1", "Berlin", time.Date(2026, 4, 25, 6, 0, 0, 0, time.UTC), 1.100, false)

	later := time.Date(2026, 4, 27, 0, 0, 0, 0, time.UTC)
	settled, err = evaluateCheckOutcomes(ctx, db, "diesel", later, time.UTC)
	if err != nil {
		t.Fatalf("evaluateCheckOutcomes: %v", err)
	}
	if settled != 1 {
		t.Fatalf("settled = %d, want 1", settled)
	}

	var (
		observed, floor, regret float64
		floorAt, evaluatedAt    string
	)
	if err := db.QueryRowContext(ctx, `
		SELECT observed_price, day_floor_price, day_floor_at, regret, outcome_evaluated_at
		FROM price_check_decisions`).Scan(&observed, &floor, &floorAt, &regret, &evaluatedAt); err != nil {
		t.Fatalf("read outcome: %v", err)
	}
	if floor != 1.500 {
		t.Fatalf("day_floor_price = %v, want 1.500 (13:00 and the closed row are outside the pricing day)", floor)
	}
	if floorAt != "2026-04-25T05:00:00Z" {
		t.Fatalf("day_floor_at = %s, want the 05:00 snapshot", floorAt)
	}
	if math.Abs(regret-(observed-floor)) > 1e-9 {
		t.Fatalf("regret = %v, want observed-floor = %v", regret, observed-floor)
	}
	if evaluatedAt == "" {
		t.Fatal("outcome_evaluated_at not set")
	}

	// A second pass must not re-settle the same row.
	settled, err = evaluateCheckOutcomes(ctx, db, "diesel", later, time.UTC)
	if err != nil {
		t.Fatalf("evaluateCheckOutcomes (repeat): %v", err)
	}
	if settled != 0 {
		t.Fatalf("settled = %d on repeat, want 0", settled)
	}
}

func TestEvaluateCheckOutcomesMarksRowsWithoutData(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 4, 25, 9, 30, 0, 0, time.UTC)
	runID, computation, opts := buildDecisionFixture(t, db, now)
	if _, err := persistCheckDecisions(ctx, db, computation, opts, runID); err != nil {
		t.Fatalf("persistCheckDecisions: %v", err)
	}
	// Drop every snapshot so the pricing day has no usable price at all.
	if _, err := db.ExecContext(ctx, `DELETE FROM price_snapshots`); err != nil {
		t.Fatalf("clear snapshots: %v", err)
	}

	later := time.Date(2026, 4, 27, 0, 0, 0, 0, time.UTC)
	measured, err := evaluateCheckOutcomes(ctx, db, "diesel", later, time.UTC)
	if err != nil {
		t.Fatalf("evaluateCheckOutcomes: %v", err)
	}
	if measured != 0 {
		t.Fatalf("measured = %d, want 0 without price data", measured)
	}
	var (
		evaluatedAt sql.NullString
		floor       sql.NullFloat64
	)
	if err := db.QueryRowContext(ctx,
		`SELECT outcome_evaluated_at, day_floor_price FROM price_check_decisions`).Scan(&evaluatedAt, &floor); err != nil {
		t.Fatalf("read outcome: %v", err)
	}
	if !evaluatedAt.Valid {
		t.Fatal("row left unevaluated: it would be retried forever")
	}
	if floor.Valid {
		t.Fatalf("day_floor_price = %+v, want NULL", floor)
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
	_, persisted, err := persistPredictionRun(ctx, db, computation, opts)
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
	// city_name is a legacy column: a run is no longer scoped to one city.
	if anchor != 12 || stationCount != 1 || cityName != "" {
		t.Fatalf("run = anchor %d, stations %d, city %q; want 12/1 and no city", anchor, stationCount, cityName)
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

	corrections, err := loadLearnedCorrections(ctx, db, "diesel", now, time.UTC)
	if err != nil {
		t.Fatalf("loadLearnedCorrections: %v", err)
	}
	if len(corrections.StationBias) != 1 {
		t.Fatalf("station bias = %+v, want only the consistently biased station", corrections.StationBias)
	}
	if got := corrections.StationBias["biased"]; got != predictionBiasMaxAbs {
		t.Fatalf("bias = %.4f, want capped at %.2f", got, predictionBiasMaxAbs)
	}

	model := forecastModel{Stations: map[string]forecastStation{"biased": {}}}
	if err := applyLearnedCorrections(ctx, db, &model, "diesel", now, time.UTC); err != nil {
		t.Fatalf("applyLearnedCorrections: %v", err)
	}
	if got := model.Stations["biased"].BiasCorrection; got != predictionBiasMaxAbs {
		t.Fatalf("BiasCorrection = %.4f, want %.2f", got, predictionBiasMaxAbs)
	}
}

func TestLoadLearnedCorrectionsHourLeadGrid(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	insertSuggestStation(t, db, "station-1", "Station 1", 52.5, 13.4)
	now := time.Date(2026, 4, 27, 15, 0, 0, 0, time.UTC) // Monday
	runID := insertPredictionRunRow(t, db, now.AddDate(0, 0, -8))

	// 60 weekday noon targets with a consistent +6 ct miss (leads in the
	// 1-6h bucket), on two plain Mondays (no weekends, no holidays).
	weekday := time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC) // Monday noon
	for i := 0; i < 60; i++ {
		target := weekday.AddDate(0, 0, -(i%2)*7).Add(time.Duration(i%3) * time.Minute)
		id := insertPredictionRow(t, db, runID, "station-1", target, 2.00, 120+i%60)
		markPredictionEvaluated(t, db, id, 0.06, now.AddDate(0, 0, -1))
	}
	// 60 weekend noon targets with no miss: the weekend cell must stay
	// separate instead of blending into the weekday one.
	weekend := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC) // Saturday noon
	for i := 0; i < 60; i++ {
		target := weekend.Add(time.Duration(i%3) * time.Minute)
		id := insertPredictionRow(t, db, runID, "station-1", target, 2.00, 120+i%60)
		markPredictionEvaluated(t, db, id, 0.0, now.AddDate(0, 0, -1))
	}

	corrections, err := loadLearnedCorrections(ctx, db, "diesel", now, time.UTC)
	if err != nil {
		t.Fatalf("loadLearnedCorrections: %v", err)
	}
	weekdayCell := hourLeadKey{Hour: 12, Lead: leadBucket1to6h, Weekend: false}
	if got := corrections.HourLeadBias[weekdayCell]; got < 0.059 || got > 0.061 {
		t.Fatalf("weekday noon cell = %.4f, want ~0.06: %+v", got, corrections.HourLeadBias)
	}
	weekendCell := hourLeadKey{Hour: 12, Lead: leadBucket1to6h, Weekend: true}
	if got, ok := corrections.HourLeadBias[weekendCell]; ok && got != 0 {
		t.Fatalf("weekend noon cell = %.4f, want absent or 0", got)
	}

	// scoreForecast must add the cell to matching targets, reuse the 6-24h
	// cell beyond 24h, and stay inert without NowLocal.
	model := forecastModel{
		Stations: map[string]forecastStation{"s": {}},
		Hour: map[stationHourKey][]priceSample{
			{StationID: "s", Hour: 12}: {{Price: 2.00, Weight: 60}},
		},
		Recent: map[string][]priceSample{
			"s": {{Price: 2.00, Weight: 60}},
		},
		NowLocal: now,
		HourLeadBias: map[hourLeadKey]float64{
			{Hour: 12, Lead: leadBucket1to6h, Weekend: false}:  0.06,
			{Hour: 12, Lead: leadBucket6to24h, Weekend: false}: 0.05,
		},
	}
	score, ok := scoreForecast(model, "s", time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC))
	if !ok {
		t.Fatal("scoreForecast returned !ok")
	}
	if score.PredictedPrice < 2.049 || score.PredictedPrice > 2.051 {
		t.Fatalf("tomorrow-noon prediction = %.4f, want 2.05 (6-24h cell)", score.PredictedPrice)
	}
	score, ok = scoreForecast(model, "s", time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC))
	if !ok {
		t.Fatal("scoreForecast returned !ok")
	}
	if score.PredictedPrice < 2.049 || score.PredictedPrice > 2.051 {
		t.Fatalf(">24h prediction = %.4f, want 2.05 (reused 6-24h cell)", score.PredictedPrice)
	}
	inert := model
	inert.NowLocal = time.Time{}
	score, ok = scoreForecast(inert, "s", time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC))
	if !ok || score.PredictedPrice != 2.00 {
		t.Fatalf("zero-NowLocal prediction = %.4f, want raw 2.00", score.PredictedPrice)
	}
}

func TestLoadLearnedCorrectionsCalibratesConfidence(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	insertSuggestStation(t, db, "precise", "Precise", 52.5, 13.4)
	insertSuggestStation(t, db, "noisy", "Noisy", 52.5, 13.4)
	now := time.Date(2026, 4, 27, 15, 0, 0, 0, time.UTC)
	runID := insertPredictionRunRow(t, db, now.AddDate(0, 0, -3))

	target := now.AddDate(0, 0, -1)
	for i := 0; i < predictionConfidenceMinSamples; i++ {
		id := insertPredictionRow(t, db, runID, "precise", target.Add(time.Duration(i)*time.Minute), 2.00, 30)
		markPredictionEvaluated(t, db, id, 0, now.AddDate(0, 0, -1))
		id = insertPredictionRow(t, db, runID, "noisy", target.Add(time.Duration(i)*time.Minute), 2.00, 30)
		// Alternating large errors: no learnable bias, wide residuals.
		errValue := 0.06
		if i%2 == 0 {
			errValue = -0.06
		}
		markPredictionEvaluated(t, db, id, errValue, now.AddDate(0, 0, -1))
	}

	corrections, err := loadLearnedCorrections(ctx, db, "diesel", now, time.UTC)
	if err != nil {
		t.Fatalf("loadLearnedCorrections: %v", err)
	}
	preciseKey := stationLeadKey{StationID: "precise", Lead: leadBucket0to1h}
	if got := corrections.ConfidenceByLead[preciseKey]; got != "high" {
		t.Fatalf("precise confidence = %q, want high", got)
	}
	noisyKey := stationLeadKey{StationID: "noisy", Lead: leadBucket0to1h}
	if got := corrections.ConfidenceByLead[noisyKey]; got != "low" {
		t.Fatalf("noisy confidence = %q, want low", got)
	}

	// The calibrated label overrides the heuristic in scoreForecast, and a
	// >24h target cannot claim more than medium from a <=24h measurement.
	model := forecastModel{
		Stations: map[string]forecastStation{"precise": {}},
		Hour: map[stationHourKey][]priceSample{
			// Hour 15: both targets below (now+30m and now+2d) hit 15:00.
			{StationID: "precise", Hour: 15}: {
				{Price: 2.00, Weight: 60}, {Price: 2.00, Weight: 60}, {Price: 2.00, Weight: 60},
				{Price: 2.00, Weight: 60}, {Price: 2.00, Weight: 60},
			},
		},
		Recent:   map[string][]priceSample{"precise": {{Price: 2.00, Weight: 60}}},
		NowLocal: now,
		ConfidenceByLead: map[stationLeadKey]string{
			{StationID: "precise", Lead: leadBucket0to1h}:  "high",
			{StationID: "precise", Lead: leadBucket6to24h}: "high",
		},
	}
	score, ok := scoreForecast(model, "precise", now.Add(30*time.Minute))
	if !ok || score.Confidence != "high" {
		t.Fatalf("short-lead confidence = %q, want calibrated high", score.Confidence)
	}
	score, ok = scoreForecast(model, "precise", now.AddDate(0, 0, 2))
	if !ok || score.Confidence != "medium" {
		t.Fatalf(">24h confidence = %q, want high capped to medium", score.Confidence)
	}
}

func TestPersistPredictionRunStoresAppliedCorrection(t *testing.T) {
	db := openTestDB(t)
	now := time.Date(2026, 4, 25, 9, 30, 0, 0, time.UTC)
	buildDecisionFixture(t, db, now)

	var total, missing int
	if err := db.QueryRowContext(context.Background(), `
		SELECT COUNT(*), SUM(CASE WHEN applied_correction IS NULL THEN 1 ELSE 0 END)
		FROM price_predictions
	`).Scan(&total, &missing); err != nil {
		t.Fatalf("count predictions: %v", err)
	}
	if total == 0 {
		t.Fatal("fixture persisted no predictions")
	}
	// Every persisted row must record the correction it carried — a NULL
	// marks a row as untrainable (older model version), and the persist path
	// must never produce one.
	if missing != 0 {
		t.Fatalf("%d of %d persisted predictions lack applied_correction", missing, total)
	}
}

func TestLoadLearnedCorrectionsReconstructsRawModelError(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	insertSuggestStation(t, db, "corrected", "Corrected", 52.5, 13.4)
	insertSuggestStation(t, db, "legacy", "Legacy", 52.5, 13.4)
	now := time.Date(2026, 4, 27, 15, 0, 0, 0, time.UTC)
	runID := insertPredictionRunRow(t, db, now.AddDate(0, 0, -2))

	target := now.AddDate(0, 0, -1)
	for i := 0; i < 6; i++ {
		// The stored prediction already carried a +1 ct correction and still
		// came out 1 ct low: the raw model gap is +2 ct, and that — not the
		// residual — is what the loop must learn, or it would shrink its own
		// correction every cycle.
		id := insertPredictionRow(t, db, runID, "corrected", target.Add(time.Duration(i)*time.Hour), 2.00, 60)
		markPredictionEvaluated(t, db, id, 0.01, now.AddDate(0, 0, -1))
		setPredictionAppliedCorrection(t, db, id, 0.01)

		// Same errors on a station whose rows predate the column: produced by
		// an older model version, they must not train anything.
		id = insertPredictionRow(t, db, runID, "legacy", target.Add(time.Duration(i)*time.Hour), 2.00, 60)
		markPredictionEvaluated(t, db, id, 0.01, now.AddDate(0, 0, -1))
		setPredictionAppliedCorrection(t, db, id, nil)
	}

	corrections, err := loadLearnedCorrections(ctx, db, "diesel", now, time.UTC)
	if err != nil {
		t.Fatalf("loadLearnedCorrections: %v", err)
	}
	if got := corrections.StationBias["corrected"]; got < 0.0199 || got > 0.0201 {
		t.Fatalf("bias = %.4f, want 0.02 (stored error 0.01 + applied correction 0.01)", got)
	}
	if _, ok := corrections.StationBias["legacy"]; ok {
		t.Fatalf("legacy rows without applied_correction trained a bias: %+v", corrections.StationBias)
	}
}

func TestLoadLearnedCorrectionsMeasuresSuggestionBias(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	insertSuggestStation(t, db, "station-1", "Station 1", 52.5, 13.4)
	now := time.Date(2026, 4, 27, 15, 0, 0, 0, time.UTC)
	runID := insertPredictionRunRow(t, db, now.AddDate(0, 0, -8))

	target := now.AddDate(0, 0, -2)
	for i := 0; i < predictionSuggestionBiasMinSamples; i++ {
		// Long leads on purpose: suggested windows usually sit days out, and
		// they must still feed the measurement.
		id := insertPredictionRow(t, db, runID, "station-1", target.Add(time.Duration(i)*time.Minute), 2.00, 2000)
		markPredictionEvaluated(t, db, id, 0.03, now.AddDate(0, 0, -1))
		markPredictionSuggestion(t, db, id)
	}

	corrections, err := loadLearnedCorrections(ctx, db, "diesel", now, time.UTC)
	if err != nil {
		t.Fatalf("loadLearnedCorrections: %v", err)
	}
	if corrections.SuggestionBias < 0.029 || corrections.SuggestionBias > 0.031 {
		t.Fatalf("suggestion bias = %.4f, want ~0.03", corrections.SuggestionBias)
	}
}

func TestGenerateSuggestionsAppliesSuggestionBiasToDisplayOnly(t *testing.T) {
	firstDay := time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC)
	intervals := sawtoothIntervals("s1", firstDay, 14, func(int) float64 { return 2.00 })
	now := time.Date(2026, 4, 24, 9, 30, 0, 0, time.UTC)
	model := buildForecastModel(intervals, now, time.UTC)

	baseline := generateSuggestions(model, "diesel", now, time.UTC, 1, 1)
	if len(baseline) == 0 {
		t.Fatal("no baseline suggestions")
	}
	// Deliberately not cent-aligned: measured biases never are, and a raw
	// price near a cent boundary must not round into a different selection.
	model.SuggestionBias = 0.0279
	shifted := generateSuggestions(model, "diesel", now, time.UTC, 1, 1)
	if len(shifted) != len(baseline) {
		t.Fatalf("suggestion count changed: %d vs %d", len(shifted), len(baseline))
	}
	if shifted[0].StartTime != baseline[0].StartTime || shifted[0].StationID != baseline[0].StationID {
		t.Fatalf("selection changed: %+v vs %+v", shifted[0], baseline[0])
	}
	window, err := time.ParseInLocation("2006-01-02 15:04", baseline[0].Date+" "+baseline[0].StartTime, time.UTC)
	if err != nil {
		t.Fatalf("parse suggested window: %v", err)
	}
	rawScore, ok := scoreForecast(model, "s1", window)
	if !ok {
		t.Fatal("scoreForecast returned !ok for the suggested window")
	}
	want := roundTo(rawScore.PredictedPrice+0.0279, 2)
	if shifted[0].PredictedPrice != want {
		t.Fatalf("displayed price = %.3f, want %.3f (raw %.4f + bias, rounded once)", shifted[0].PredictedPrice, want, rawScore.PredictedPrice)
	}

	// The persisted grid must not carry the display correction: the stored
	// price comes straight from scoreForecast.
	score, ok := scoreForecast(model, "s1", time.Date(2026, 4, 24, 11, 0, 0, 0, time.UTC))
	if !ok {
		t.Fatal("scoreForecast returned !ok")
	}
	rawModel := model
	rawModel.SuggestionBias = 0
	unbiased, ok := scoreForecast(rawModel, "s1", time.Date(2026, 4, 24, 11, 0, 0, 0, time.UTC))
	if !ok {
		t.Fatal("scoreForecast returned !ok")
	}
	if score.PredictedPrice != unbiased.PredictedPrice {
		t.Fatalf("scoreForecast shifted by SuggestionBias: %.4f vs %.4f", score.PredictedPrice, unbiased.PredictedPrice)
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
		return run([]string{"suggest", "--db", dbPath, "--persist", "--output", "json"})
	})
	var results []fuelSuggestResult
	if err := json.Unmarshal([]byte(output), &results); err != nil {
		t.Fatalf("unmarshal suggest output: %v\noutput=%s", err, output)
	}
	if len(results) == 0 || results[0].Fuel != "diesel" || len(results[0].Suggestions) == 0 {
		t.Fatalf("results = %+v, want diesel suggestions first", results)
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
	// One run per fuel, plus the seeded past run.
	if want := len(suggestFuels) + 1; runs != want {
		t.Fatalf("prediction_runs = %d, want %d (seeded + one per fuel)", runs, want)
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

func TestRunSuggestPersistQuiet(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "quiet.db")
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
	insertSuggestStation(t, db, "station-1", "Station 1", 52.517389, 13.395131)

	nowLocal := time.Now().In(time.Local)
	for daysAgo := 15; daysAgo >= 1; daysAgo-- {
		day := localDayStart(nowLocal).AddDate(0, 0, -daysAgo)
		insertSawtoothDay(t, db, "station-1", "Berlin", day.In(time.UTC), 2.00)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	if err := run([]string{"suggest", "--db", dbPath, "--quiet", "--city", "Berlin", "--fuel", "diesel"}); err == nil {
		t.Fatal("suggest --quiet without --persist succeeded, want error")
	}

	output := captureStdout(t, func() error {
		return run([]string{"suggest", "--db", dbPath, "--persist", "--quiet"})
	})
	if output != "" {
		t.Fatalf("suggest --persist --quiet printed output: %q", output)
	}

	db, err = openDB(dbPath)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer db.Close()

	var predictions int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM price_predictions`).Scan(&predictions); err != nil {
		t.Fatalf("count predictions: %v", err)
	}
	if predictions == 0 {
		t.Fatal("no predictions stored despite --persist --quiet")
	}
}

func insertUpdateTargetRow(t *testing.T, db *sql.DB, city string, radiusKM float64) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO update_targets (city, radius_km, created_at) VALUES (?, ?, ?)`,
		city, radiusKM, "2026-04-20T00:00:00Z"); err != nil {
		t.Fatalf("insert update target %q: %v", city, err)
	}
}

func TestRunSuggestPersistsOneRunPerFuelOverEveryFedStation(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "multi.db")
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	ctx := context.Background()
	if err := initSchema(ctx, db, dialectSQLite); err != nil {
		t.Fatalf("initSchema: %v", err)
	}
	insertSuggestCity(t, db, cachedCity{QueryName: "Berlin", Name: "Berlin", DisplayName: "Berlin", Lat: 52.517389, Lng: 13.395131})
	insertSuggestCity(t, db, cachedCity{QueryName: "Hamburg", Name: "Hamburg", DisplayName: "Hamburg", Lat: 53.550556, Lng: 9.993333})
	insertSuggestStation(t, db, "station-b", "Station B", 52.517389, 13.395131)
	insertSuggestStation(t, db, "station-h", "Station H", 53.550556, 9.993333)
	nowLocal := time.Now().In(time.Local)
	for daysAgo := 15; daysAgo >= 1; daysAgo-- {
		day := localDayStart(nowLocal).AddDate(0, 0, -daysAgo)
		insertSawtoothDay(t, db, "station-b", "Berlin", day.In(time.UTC), 2.00)
		insertSawtoothDay(t, db, "station-h", "Hamburg", day.In(time.UTC), 1.90)
	}
	insertUpdateTargetRow(t, db, "Berlin", 5)
	insertUpdateTargetRow(t, db, "Hamburg", 5)
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	output := captureStdout(t, func() error {
		return run([]string{"suggest", "--db", dbPath, "--persist", "--output", "json"})
	})
	var results []fuelSuggestResult
	if err := json.Unmarshal([]byte(output), &results); err != nil {
		t.Fatalf("unmarshal suggest output: %v\noutput=%s", err, output)
	}
	if len(results) != len(suggestFuels) {
		t.Fatalf("got %d fuel results, want %d", len(results), len(suggestFuels))
	}
	for i, want := range suggestFuels {
		if results[i].Fuel != want {
			t.Fatalf("results[%d].Fuel = %q, want %q", i, results[i].Fuel, want)
		}
		if results[i].Error != "" {
			t.Fatalf("results[%d] unexpected error: %s", i, results[i].Error)
		}
		if len(results[i].Suggestions) == 0 {
			t.Fatalf("results[%d] has no suggestions", i)
		}
	}

	db, err = openDB(dbPath)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer db.Close()
	// One run per fuel, each covering both cities' stations, so the station
	// count is the whole fed set rather than one city's slice.
	var runs, fuels, stations int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*), COUNT(DISTINCT fuel), MAX(station_count) FROM prediction_runs`).Scan(&runs, &fuels, &stations); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runs != len(suggestFuels) || fuels != len(suggestFuels) {
		t.Fatalf("prediction_runs = %d over %d fuels, want %d over %d", runs, fuels, len(suggestFuels), len(suggestFuels))
	}
	if stations != 2 {
		t.Fatalf("station_count = %d, want both fed stations in one run", stations)
	}
}

func TestRunSuggestIsBestEffortAcrossFuels(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "besteffort.db")
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	ctx := context.Background()
	if err := initSchema(ctx, db, dialectSQLite); err != nil {
		t.Fatalf("initSchema: %v", err)
	}
	insertSuggestCity(t, db, cachedCity{QueryName: "Berlin", Name: "Berlin", DisplayName: "Berlin", Lat: 52.517389, Lng: 13.395131})
	insertSuggestStation(t, db, "station-b", "Station B", 52.517389, 13.395131)
	nowLocal := time.Now().In(time.Local)
	for daysAgo := 15; daysAgo >= 1; daysAgo-- {
		day := localDayStart(nowLocal).AddDate(0, 0, -daysAgo)
		insertSawtoothDayDieselOnly(t, db, "station-b", "Berlin", day.In(time.UTC), 2.00)
	}
	// Only diesel has stored prices, so e5 and e10 must fail on their own.
	insertSuggestSnapshotDieselOnly(t, db, "station-b", "Berlin", time.Now().UTC(), 2.000, true)
	insertUpdateTargetRow(t, db, "Berlin", 5)
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	old := stdout
	var buf bytes.Buffer
	stdout = &buf
	runErr := run([]string{"suggest", "--db", dbPath, "--persist"})
	stdout = old
	if runErr == nil || !strings.Contains(runErr.Error(), "2 of 3 fuels failed") {
		t.Fatalf("run error = %v, want '2 of 3 fuels failed'", runErr)
	}
	output := buf.String()
	for _, fuel := range suggestFuels {
		if !strings.Contains(output, "fuel: "+fuel) {
			t.Fatalf("output missing section for %s:\n%s", fuel, output)
		}
	}
	if !strings.Contains(output, "error:") {
		t.Fatalf("output missing error line for the failed fuels:\n%s", output)
	}

	db, err = openDB(dbPath)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer db.Close()
	var runs int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM prediction_runs WHERE fuel = 'diesel'`).Scan(&runs); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runs != 1 {
		t.Fatalf("prediction_runs for diesel = %d, want 1 (the good fuel persisted despite the failures)", runs)
	}
}

// TestPersistCheckDecisionsStoresUnroundedPrices is the regression test for
// storing display-rounded prices in the decision log. German pump prices carry
// three decimals, so rounding the observed price to two moved it by up to half
// a cent against a day floor read at full precision — enough to make the regret
// of a decision taken exactly at the day's low come out negative.
func TestPersistCheckDecisionsStoresUnroundedPrices(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 4, 25, 9, 30, 0, 0, time.UTC)
	runID, _, opts := buildDecisionFixture(t, db, now)

	// A three-decimal price that rounds *down* to two decimals, placed so it
	// is both the latest snapshot at decision time and the cheapest price of
	// the pricing day. Regret must therefore be exactly zero.
	const lowPrice = 1.754
	insertSuggestSnapshot(t, db, "station-1", "Berlin", time.Date(2026, 4, 25, 9, 0, 0, 0, time.UTC), lowPrice, true)

	// Rebuild against the snapshot just inserted.
	computation, err := computeSuggestions(ctx, db, opts)
	if err != nil {
		t.Fatalf("computeSuggestions: %v", err)
	}
	if _, err := persistCheckDecisions(ctx, db, computation, opts, runID); err != nil {
		t.Fatalf("persistCheckDecisions: %v", err)
	}

	var observed, predicted, storedErr float64
	if err := db.QueryRowContext(ctx,
		`SELECT observed_price, predicted_price, error FROM price_check_decisions`).
		Scan(&observed, &predicted, &storedErr); err != nil {
		t.Fatalf("read decision: %v", err)
	}
	if math.Abs(observed-lowPrice) > 1e-9 {
		t.Fatalf("observed_price = %v, want the unrounded %v (rounding to 2 decimals gives %v)",
			observed, lowPrice, roundTo(lowPrice, 2))
	}
	if math.Abs(storedErr-(observed-predicted)) > 1e-9 {
		t.Fatalf("error = %v, want observed-predicted = %v", storedErr, observed-predicted)
	}

	later := time.Date(2026, 4, 27, 0, 0, 0, 0, time.UTC)
	if _, err := evaluateCheckOutcomes(ctx, db, "diesel", later, time.UTC); err != nil {
		t.Fatalf("evaluateCheckOutcomes: %v", err)
	}
	var floor, regret float64
	if err := db.QueryRowContext(ctx,
		`SELECT day_floor_price, regret FROM price_check_decisions`).Scan(&floor, &regret); err != nil {
		t.Fatalf("read outcome: %v", err)
	}
	if math.Abs(floor-lowPrice) > 1e-9 {
		t.Fatalf("day_floor_price = %v, want %v", floor, lowPrice)
	}
	if math.Abs(regret) > 1e-9 {
		t.Fatalf("regret = %v, want exactly 0 for a decision taken at the day's low", regret)
	}
	if regret < 0 {
		t.Fatalf("regret = %v is negative: the observed price cannot be below its own day's floor", regret)
	}
}

// TestEvaluateCheckOutcomesReusesFloorPerPricingDay pins the memoization: the
// hourly persist timer produces many decisions per station per pricing day, and
// they must resolve to one floor lookup, all agreeing.
func TestEvaluateCheckOutcomesReusesFloorPerPricingDay(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 4, 25, 9, 30, 0, 0, time.UTC)
	runID, computation, opts := buildDecisionFixture(t, db, now)

	// Three runs' worth of decisions for the same station and pricing day.
	for i := 0; i < 3; i++ {
		if _, err := persistCheckDecisions(ctx, db, computation, opts, runID); err != nil {
			t.Fatalf("persistCheckDecisions: %v", err)
		}
	}
	var pending int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM price_check_decisions`).Scan(&pending); err != nil {
		t.Fatalf("count decisions: %v", err)
	}
	if pending != 3 {
		t.Fatalf("decisions = %d, want 3", pending)
	}

	later := time.Date(2026, 4, 27, 0, 0, 0, 0, time.UTC)
	measured, err := evaluateCheckOutcomes(ctx, db, "diesel", later, time.UTC)
	if err != nil {
		t.Fatalf("evaluateCheckOutcomes: %v", err)
	}
	if measured != 3 {
		t.Fatalf("measured = %d, want all 3 settled", measured)
	}
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT day_floor_price, day_floor_at FROM price_check_decisions`)
	if err != nil {
		t.Fatalf("query floors: %v", err)
	}
	defer rows.Close()
	distinct := 0
	for rows.Next() {
		distinct++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if distinct != 1 {
		t.Fatalf("distinct floors = %d, want 1: decisions sharing a pricing day must agree", distinct)
	}
}

// Since prediction runs went global, is_suggestion marks the run's *globally*
// cheapest windows — a dashboard filtered to one area almost never contains
// them, which is exactly the bug this pins: the card read the flags and showed
// nothing. The dashboard has to pick windows for its own scope from the stored
// grid, mirroring the notifier's per-area picker. The picker is PHP and cannot
// be executed here, so this checks its source against the Go constants it
// mirrors.
func TestDashboardPredictionsArePickedPerScopeNotFromGlobalFlags(t *testing.T) {
	viewer, err := os.ReadFile(filepath.Join("web", "index.php"))
	if err != nil {
		t.Fatalf("read web/index.php: %v", err)
	}
	source := string(viewer)

	start := strings.Index(source, "function loadFilteredPredictions")
	if start < 0 {
		t.Fatal("web/index.php no longer defines loadFilteredPredictions")
	}
	end := strings.Index(source[start:], "\nfunction ")
	if end < 0 {
		t.Fatal("cannot delimit loadFilteredPredictions")
	}
	picker := source[start : start+end]

	if strings.Contains(picker, "is_suggestion") {
		t.Error("the dashboard picker reads the persisted is_suggestion flags, which mark global picks a filtered scope almost never contains")
	}
	if want := fmt.Sprintf("$limitPerDay = %d", suggestLimitPerDay); !strings.Contains(picker, want) {
		t.Errorf("the dashboard picker no longer mirrors suggestLimitPerDay (%d): %q missing", suggestLimitPerDay, want)
	}
	// duplicatesNearbyStationWindow skips windows less than two hours apart at
	// one station; the PHP mirror spells the same bound in seconds.
	if !strings.Contains(picker, "$dupWindowSeconds = 2 * 3600") {
		t.Error("the dashboard picker no longer mirrors the two-hour same-station duplicate window")
	}
	// The notifier picks cheapest-first and only then filters to medium/high
	// (notify.go collectSuggestions); the picker has to keep that order, so
	// the candidate query must not pre-filter confidence.
	if strings.Contains(picker, "confidence IN") {
		t.Error("the dashboard picker filters confidence in SQL, before selection — a cheap low-confidence window must consume a slot like it does in notify")
	}
	for _, want := range []string{"!== 'medium'", "!== 'high'"} {
		if !strings.Contains(picker, want) {
			t.Errorf("the dashboard picker no longer applies the notifier's medium/high filter after selection: %q missing", want)
		}
	}
	// suggestionCandidateLess ordering: price first, then confidence.
	if !strings.Contains(picker, "$a['price'] <=> $b['price']") || !strings.Contains(picker, "$confidenceRank") {
		t.Error("the dashboard picker no longer orders candidates by price then confidence like suggestionCandidateLess")
	}
}
