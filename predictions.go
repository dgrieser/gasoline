package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"
)

// Persistent-prediction tuning. Structural like the forecast thresholds; not
// user settings.
const (
	// predictionRetentionDays bounds how long persisted predictions are kept.
	predictionRetentionDays = 30
	// predictionBiasWindowDays is the evaluation window the bias learns from.
	predictionBiasWindowDays = 14
	// predictionBiasHalfLifeDays is the recency e-fold for bias samples.
	predictionBiasHalfLifeDays = 7.0
	// predictionBiasMinSamples gates the correction until enough evaluated
	// predictions exist.
	predictionBiasMinSamples = 5
	// predictionBiasMaxAbs caps the learned correction in euro.
	predictionBiasMaxAbs = 0.03
	// predictionBiasMaxLeadMinutes restricts learning to short-lead
	// predictions: long-lead errors are dominated by unknowable future jumps,
	// not systematic model bias.
	predictionBiasMaxLeadMinutes = 360
	// evaluateBatchLimit bounds how many due predictions one run settles, so
	// a run after long downtime stays cheap.
	evaluateBatchLimit = 5000
	// persistInsertBatch rows per multi-row INSERT. At 11 placeholders per
	// row this stays under SQLite's historical 999-variable limit, so the
	// insert also works against builds without the modern 32766 default.
	persistInsertBatch = 80
	// decisionInsertBatch rows per multi-row INSERT for check decisions. At
	// 17 placeholders per row this stays under the same 999-variable limit.
	decisionInsertBatch = 40
	// evaluateOutcomeBatchLimit bounds how many decisions one run settles
	// against the completed pricing day, mirroring evaluateBatchLimit.
	evaluateOutcomeBatchLimit = 5000
)

// evaluateDuePredictions fills actual_price and error for stored predictions
// whose target window has passed, using the price in effect at the window
// midpoint. Predictions without usable price data (station closed, no
// snapshot) are marked evaluated with a NULL actual so they are not retried
// forever. Returns how many predictions received an actual price.
func evaluateDuePredictions(ctx context.Context, db *sql.DB, fuel string, now time.Time) (int, error) {
	column, err := suggestFuelColumn(fuel)
	if err != nil {
		return 0, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	// The whole evaluation runs in one transaction so a concurrent run cannot
	// pick up the same due rows between select and update.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		SELECT id, station_id, target_start, target_end, predicted_price
		FROM price_predictions
		WHERE fuel = ?
			AND evaluated_at IS NULL
			AND target_end <= ?
		ORDER BY target_end ASC
		LIMIT `+fmt.Sprint(evaluateBatchLimit),
		fuel, now.UTC().Format(time.RFC3339))
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	type duePrediction struct {
		ID        int64
		StationID string
		Midpoint  time.Time
		Predicted float64
	}
	var due []duePrediction
	for rows.Next() {
		var (
			id                 int64
			stationID          string
			startText, endText string
			predicted          float64
		)
		if err := rows.Scan(&id, &stationID, &startText, &endText, &predicted); err != nil {
			return 0, err
		}
		start, err := time.Parse(time.RFC3339, startText)
		if err != nil {
			return 0, fmt.Errorf("parse target_start %q: %w", startText, err)
		}
		end, err := time.Parse(time.RFC3339, endText)
		if err != nil {
			return 0, fmt.Errorf("parse target_end %q: %w", endText, err)
		}
		due = append(due, duePrediction{
			ID:        id,
			StationID: stationID,
			Midpoint:  start.Add(end.Sub(start) / 2),
			Predicted: predicted,
		})
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(due) == 0 {
		return 0, nil
	}

	snapshotStmt, err := tx.PrepareContext(ctx, fmt.Sprintf(`
		SELECT is_open, %s
		FROM price_snapshots
		WHERE station_id = ? AND recorded_at <= ?
		ORDER BY recorded_at DESC, id DESC
		LIMIT 1
	`, column))
	if err != nil {
		return 0, err
	}
	defer snapshotStmt.Close()
	updateStmt, err := tx.PrepareContext(ctx, `
		UPDATE price_predictions
		SET actual_price = ?, error = ?, evaluated_at = ?
		WHERE id = ? AND evaluated_at IS NULL
	`)
	if err != nil {
		return 0, err
	}
	defer updateStmt.Close()

	evaluatedAt := now.UTC().Format(time.RFC3339)
	measured := 0
	for _, prediction := range due {
		var (
			isOpen bool
			price  sql.NullFloat64
		)
		err := snapshotStmt.QueryRowContext(ctx,
			prediction.StationID, prediction.Midpoint.UTC().Format(time.RFC3339)).Scan(&isOpen, &price)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
		actual := sql.NullFloat64{}
		predictionError := sql.NullFloat64{}
		if err == nil && isOpen && price.Valid {
			actual = sql.NullFloat64{Float64: price.Float64, Valid: true}
			predictionError = sql.NullFloat64{Float64: price.Float64 - prediction.Predicted, Valid: true}
			measured++
		}
		if _, err := updateStmt.ExecContext(ctx, actual, predictionError, evaluatedAt, prediction.ID); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return measured, nil
}

// evaluateCheckOutcomes scores logged check decisions against the cheapest
// price their pricing day turned out to offer, so "the model said buy" can be
// compared with "the price really was near the day's floor".
//
// The floor is taken over the pricing day — the 24h window anchored at the
// market-wide daily jump hour recorded on the decision's run — because that is
// the window inside which prices form one regime. A calendar day would
// straddle two.
//
// A decision only becomes due once its pricing day has finished. The pricing
// day containing target_start ends at most 24h after it, so requiring
// target_start <= now-24h settles rows without needing the anchor in SQL.
// Decisions without usable snapshot data are still marked evaluated, with a
// NULL floor, so they are not retried forever.
func evaluateCheckOutcomes(ctx context.Context, db *sql.DB, fuel string, now time.Time, location *time.Location) (int, error) {
	column, err := suggestFuelColumn(fuel)
	if err != nil {
		return 0, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if location == nil {
		location = time.Local
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	due := now.Add(-24 * time.Hour).UTC().Format(time.RFC3339)
	rows, err := tx.QueryContext(ctx, `
		SELECT d.id, d.station_id, d.target_start, d.observed_price, r.jump_anchor_hour
		FROM price_check_decisions d
		JOIN prediction_runs r ON r.id = d.run_id
		WHERE d.fuel = ?
			AND d.outcome_evaluated_at IS NULL
			AND d.target_start <= ?
		ORDER BY d.target_start ASC
		LIMIT `+fmt.Sprint(evaluateOutcomeBatchLimit),
		fuel, due)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	type dueDecision struct {
		ID        int64
		StationID string
		Observed  float64
		DayStart  time.Time
		DayEnd    time.Time
	}
	var pending []dueDecision
	for rows.Next() {
		var (
			id         int64
			stationID  string
			startText  string
			observed   float64
			anchorHour int
		)
		if err := rows.Scan(&id, &stationID, &startText, &observed, &anchorHour); err != nil {
			return 0, err
		}
		start, err := time.Parse(time.RFC3339, startText)
		if err != nil {
			return 0, fmt.Errorf("parse target_start %q: %w", startText, err)
		}
		startLocal := start.In(location)
		day, err := time.ParseInLocation("2006-01-02", pricingDay(startLocal, anchorHour), location)
		if err != nil {
			return 0, fmt.Errorf("parse pricing day for decision %d: %w", id, err)
		}
		dayStart := day.Add(time.Duration(anchorHour) * time.Hour)
		pending = append(pending, dueDecision{
			ID:        id,
			StationID: stationID,
			Observed:  observed,
			DayStart:  dayStart,
			DayEnd:    dayStart.AddDate(0, 0, 1),
		})
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(pending) == 0 {
		return 0, nil
	}

	floorStmt, err := tx.PrepareContext(ctx, fmt.Sprintf(`
		SELECT %s, recorded_at
		FROM price_snapshots
		WHERE station_id = ?
			AND is_open = 1
			AND %s IS NOT NULL
			AND recorded_at >= ?
			AND recorded_at < ?
		ORDER BY %s ASC, recorded_at ASC
		LIMIT 1
	`, column, column, column))
	if err != nil {
		return 0, err
	}
	defer floorStmt.Close()
	updateStmt, err := tx.PrepareContext(ctx, `
		UPDATE price_check_decisions
		SET day_floor_price = ?, day_floor_at = ?, regret = ?, outcome_evaluated_at = ?
		WHERE id = ? AND outcome_evaluated_at IS NULL
	`)
	if err != nil {
		return 0, err
	}
	defer updateStmt.Close()

	evaluatedAt := now.UTC().Format(time.RFC3339)
	measured := 0
	for _, decision := range pending {
		var (
			floorPrice float64
			floorAt    string
		)
		err := floorStmt.QueryRowContext(ctx, decision.StationID,
			decision.DayStart.UTC().Format(time.RFC3339),
			decision.DayEnd.UTC().Format(time.RFC3339)).Scan(&floorPrice, &floorAt)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
		var (
			floor  sql.NullFloat64
			at     sql.NullString
			regret sql.NullFloat64
		)
		if err == nil {
			floor = sql.NullFloat64{Float64: floorPrice, Valid: true}
			at = sql.NullString{String: floorAt, Valid: true}
			regret = sql.NullFloat64{Float64: decision.Observed - floorPrice, Valid: true}
			measured++
		}
		if _, err := updateStmt.ExecContext(ctx, floor, at, regret, evaluatedAt, decision.ID); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return measured, nil
}

// applyPredictionBias loads the learned per-station corrections and attaches
// them to the model, so every consumer of scoreForecast (suggest, check,
// notify) benefits. With no persisted evaluation data this is a no-op.
func applyPredictionBias(ctx context.Context, db *sql.DB, model *forecastModel, fuel string, now time.Time) error {
	bias, err := loadPredictionBias(ctx, db, fuel, now)
	if err != nil {
		return err
	}
	for stationID, correction := range bias {
		station, ok := model.Stations[stationID]
		if !ok {
			continue
		}
		station.BiasCorrection = correction
		model.Stations[stationID] = station
	}
	return nil
}

// loadPredictionBias computes a recency-weighted median of recent short-lead
// prediction errors per station. The bias is applied on top of predictions,
// which closes the loop: once corrected predictions are persisted and
// evaluated, their residual errors shrink and the bias converges instead of
// compounding.
func loadPredictionBias(ctx context.Context, db *sql.DB, fuel string, now time.Time) (map[string]float64, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	windowStart := now.AddDate(0, 0, -predictionBiasWindowDays).UTC().Format(time.RFC3339)
	rows, err := db.QueryContext(ctx, `
		SELECT station_id, error, evaluated_at
		FROM price_predictions
		WHERE fuel = ?
			AND error IS NOT NULL
			AND evaluated_at >= ?
			AND lead_minutes <= ?
	`, fuel, windowStart, predictionBiasMaxLeadMinutes)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	samples := make(map[string][]priceSample)
	for rows.Next() {
		var (
			stationID       string
			predictionError float64
			evaluatedAtText string
		)
		if err := rows.Scan(&stationID, &predictionError, &evaluatedAtText); err != nil {
			return nil, err
		}
		evaluatedAt, err := time.Parse(time.RFC3339, evaluatedAtText)
		if err != nil {
			return nil, fmt.Errorf("parse evaluated_at %q: %w", evaluatedAtText, err)
		}
		ageDays := now.Sub(evaluatedAt).Hours() / 24
		if ageDays < 0 {
			ageDays = 0
		}
		samples[stationID] = append(samples[stationID], priceSample{
			Price:  predictionError,
			Weight: math.Exp(-ageDays / predictionBiasHalfLifeDays),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	bias := make(map[string]float64)
	for stationID, stationSamples := range samples {
		if len(stationSamples) < predictionBiasMinSamples {
			continue
		}
		median, ok := weightedMedianPrice(stationSamples)
		if !ok {
			continue
		}
		if median > predictionBiasMaxAbs {
			median = predictionBiasMaxAbs
		}
		if median < -predictionBiasMaxAbs {
			median = -predictionBiasMaxAbs
		}
		if median == 0 {
			continue
		}
		bias[stationID] = median
	}
	return bias, nil
}

// persistPredictionRun stores one prediction_runs row plus the full forecast
// grid: every (station, future hour) the model can score within the predict
// window. Rows covered by a printed suggestion are flagged. Newer runs
// supersede older ones for the same target hour — readers should take the
// latest run — while older rows remain as learning history.
//
// The run's id is returned alongside the row count so sibling records — the
// check decisions — can hang off the same run and inherit its city, fuel and
// parameter context instead of duplicating it.
func persistPredictionRun(ctx context.Context, db *sql.DB, computation *suggestComputation, opts suggestOptions) (int64, int, error) {
	nowLocal := computation.Now.In(computation.Location)
	start := nextLocalHour(nowLocal)
	end := localDayStart(start).AddDate(0, 0, opts.PredictDays)
	windows := suggestionWindows(computation.Suggestions, computation.Location)

	stationIDs := make([]string, 0, len(computation.Model.Stations))
	for stationID := range computation.Model.Stations {
		stationIDs = append(stationIDs, stationID)
	}
	sort.Strings(stationIDs)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `
		INSERT INTO prediction_runs (run_at, city_name, fuel, range_km, history_days, predict_days, jump_anchor_hour, station_count)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, computation.Now.UTC().Format(time.RFC3339), computation.CityName, opts.Fuel, opts.RangeKM,
		opts.HistoryDays, opts.PredictDays, computation.Model.JumpAnchorHour, len(stationIDs))
	if err != nil {
		return 0, 0, err
	}
	runID, err := result.LastInsertId()
	if err != nil {
		return 0, 0, err
	}

	insertPrefix := `
		INSERT INTO price_predictions (run_id, station_id, fuel, target_start, target_end, predicted_price, baseline, confidence, sample_count, is_suggestion, lead_minutes, evaluated_at)
		VALUES `
	var (
		placeholders string
		args         []any
		total        int
	)
	flush := func() error {
		if len(args) == 0 {
			return nil
		}
		if _, err := tx.ExecContext(ctx, insertPrefix+placeholders, args...); err != nil {
			return err
		}
		placeholders = ""
		args = args[:0]
		return nil
	}

	for candidateStart := start; candidateStart.Before(end); candidateStart = candidateStart.Add(time.Hour) {
		candidateEnd := candidateStart.Add(time.Hour)
		for _, stationID := range stationIDs {
			score, ok := scoreForecast(computation.Model, stationID, candidateStart)
			if !ok {
				continue
			}
			station := computation.Model.Stations[stationID]
			baseline := sql.NullFloat64{Float64: station.BaselineForecast, Valid: station.OffsetMode}
			isSuggestion := 0
			if windowsCover(windows[stationID], candidateStart) {
				isSuggestion = 1
			}
			if placeholders != "" {
				placeholders += ", "
			}
			placeholders += "(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)"
			args = append(args,
				runID,
				stationID,
				opts.Fuel,
				candidateStart.UTC().Format(time.RFC3339),
				candidateEnd.UTC().Format(time.RFC3339),
				score.PredictedPrice,
				baseline,
				score.Confidence,
				score.SampleCount,
				isSuggestion,
				int(candidateStart.Sub(nowLocal).Minutes()),
			)
			total++
			if total%persistInsertBatch == 0 {
				if err := flush(); err != nil {
					return 0, 0, err
				}
			}
		}
	}
	if err := flush(); err != nil {
		return 0, 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return runID, total, nil
}

// persistCheckDecisions stores what the check path would decide right now:
// one row per open station with a usable price, carrying the observed price,
// the model's current-hour reference price, and the verdict/recommendation
// derived from them.
//
// This is the only record of the numbers that drive low-price notifications —
// the notify path itself computes them and throws them away. The rows are
// produced by generatePriceChecks, the same pure function the check and notify
// paths use, so there is no duplicated verdict logic to drift.
//
// Unlike a forecast, the "actual" is already known here: it is the observed
// price in the same snapshot, so error is stored at insert time and no
// deferred evaluation pass is needed. The outcome columns are the exception —
// they need the pricing day to finish and are filled by evaluateCheckOutcomes.
func persistCheckDecisions(ctx context.Context, db *sql.DB, computation *suggestComputation, opts suggestOptions, runID int64) (int, error) {
	// limit 0 keeps every station: the admin CheckLimit truncation is a
	// delivery concern, and measurement wants the full picture.
	checks := generatePriceChecks(computation.Model, computation.Snapshots, opts.Fuel,
		computation.Now, computation.Location, opts.PredictDays, 0, opts.Thresholds)
	if len(checks) == 0 {
		// Every station closed or without a usable price. Nothing to record;
		// unlike checkGas this is not an error.
		return 0, nil
	}

	nowLocal := computation.Now.In(computation.Location)
	targetStart := localHourStart(nowLocal)
	targetEnd := targetStart.Add(time.Hour)
	decidedAt := computation.Now.UTC().Format(time.RFC3339)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	insertPrefix := `
		INSERT INTO price_check_decisions (run_id, station_id, fuel, decided_at, target_start, target_end,
			observed_price, observed_at, predicted_price, error, history_percentile, confidence, sample_count,
			verdict, recommendation, expected_lower, expected_drop)
		VALUES `
	var (
		placeholders string
		args         []any
		total        int
	)
	flush := func() error {
		if len(args) == 0 {
			return nil
		}
		if _, err := tx.ExecContext(ctx, insertPrefix+placeholders, args...); err != nil {
			return err
		}
		placeholders = ""
		args = args[:0]
		return nil
	}

	for _, check := range checks {
		expectedLower := 0
		if check.ExpectedLower {
			expectedLower = 1
		}
		// ExpectedDrop is only set when a cheaper future window exists.
		expectedDrop := sql.NullFloat64{Float64: check.ExpectedDrop, Valid: check.ExpectedDrop > 0}
		if placeholders != "" {
			placeholders += ", "
		}
		placeholders += "(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"
		args = append(args,
			runID,
			check.StationID,
			opts.Fuel,
			decidedAt,
			targetStart.UTC().Format(time.RFC3339),
			targetEnd.UTC().Format(time.RFC3339),
			check.CurrentPrice,
			check.RecordedAt,
			check.PredictedCurrentPrice,
			check.CurrentPrice-check.PredictedCurrentPrice,
			check.HistoryPercentile,
			check.Confidence,
			check.SampleCount,
			check.Verdict,
			check.Recommendation,
			expectedLower,
			expectedDrop,
		)
		total++
		if total%decisionInsertBatch == 0 {
			if err := flush(); err != nil {
				return 0, err
			}
		}
	}
	if err := flush(); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return total, nil
}

// suggestionWindows converts the printed suggestion rows (local date + time
// strings, possibly merged across hours) back into concrete time windows per
// station.
func suggestionWindows(suggestions []suggestionRow, location *time.Location) map[string][][2]time.Time {
	windows := make(map[string][][2]time.Time)
	for _, suggestion := range suggestions {
		start, err := time.ParseInLocation("2006-01-02 15:04", suggestion.Date+" "+suggestion.StartTime, location)
		if err != nil {
			continue
		}
		end, err := time.ParseInLocation("2006-01-02 15:04", suggestion.Date+" "+suggestion.EndTime, location)
		if err != nil {
			continue
		}
		if !end.After(start) {
			// An end time at or before the start wraps past midnight (a
			// window ending 00:00 belongs to the next day).
			end = end.AddDate(0, 0, 1)
		}
		windows[suggestion.StationID] = append(windows[suggestion.StationID], [2]time.Time{start, end})
	}
	return windows
}

func windowsCover(windows [][2]time.Time, t time.Time) bool {
	for _, window := range windows {
		if !t.Before(window[0]) && t.Before(window[1]) {
			return true
		}
	}
	return false
}

// prunePredictions enforces the retention window and drops runs whose
// predictions are all gone.
func prunePredictions(ctx context.Context, db *sql.DB, now time.Time) (int, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	cutoff := now.AddDate(0, 0, -predictionRetentionDays).UTC().Format(time.RFC3339)
	result, err := db.ExecContext(ctx, `DELETE FROM price_predictions WHERE target_end < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	pruned, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	// A run is only orphaned once neither predictions nor decisions point at
	// it; dropping it earlier would break the decisions' foreign key.
	if _, err := db.ExecContext(ctx, `
		DELETE FROM prediction_runs
		WHERE NOT EXISTS (SELECT 1 FROM price_predictions pp WHERE pp.run_id = prediction_runs.id)
			AND NOT EXISTS (SELECT 1 FROM price_check_decisions pd WHERE pd.run_id = prediction_runs.id)
	`); err != nil {
		return 0, err
	}
	return int(pruned), nil
}

// pruneCheckDecisions enforces the same retention window on decision rows.
// Runs it leaves orphaned are collected by prunePredictions.
func pruneCheckDecisions(ctx context.Context, db *sql.DB, now time.Time) (int, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	cutoff := now.AddDate(0, 0, -predictionRetentionDays).UTC().Format(time.RFC3339)
	result, err := db.ExecContext(ctx, `DELETE FROM price_check_decisions WHERE target_end < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	pruned, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(pruned), nil
}
