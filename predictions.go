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
	// persistInsertBatch rows per multi-row INSERT.
	persistInsertBatch = 200
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
	rows, err := db.QueryContext(ctx, `
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

	snapshotQuery := fmt.Sprintf(`
		SELECT is_open, %s
		FROM price_snapshots
		WHERE station_id = ? AND recorded_at <= ?
		ORDER BY recorded_at DESC, id DESC
		LIMIT 1
	`, column)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	evaluatedAt := now.UTC().Format(time.RFC3339)
	measured := 0
	for _, prediction := range due {
		var (
			isOpen bool
			price  sql.NullFloat64
		)
		err := tx.QueryRowContext(ctx, snapshotQuery,
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
		if _, err := tx.ExecContext(ctx, `
			UPDATE price_predictions
			SET actual_price = ?, error = ?, evaluated_at = ?
			WHERE id = ?
		`, actual, predictionError, evaluatedAt, prediction.ID); err != nil {
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
func persistPredictionRun(ctx context.Context, db *sql.DB, computation *suggestComputation, opts suggestOptions) (int, error) {
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
		return 0, err
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `
		INSERT INTO prediction_runs (run_at, city_name, fuel, range_km, history_days, predict_days, jump_anchor_hour, station_count)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, computation.Now.UTC().Format(time.RFC3339), computation.CityName, opts.Fuel, opts.RangeKM,
		opts.HistoryDays, opts.PredictDays, computation.Model.JumpAnchorHour, len(stationIDs))
	if err != nil {
		return 0, err
	}
	runID, err := result.LastInsertId()
	if err != nil {
		return 0, err
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
			score, ok := scoreForecast(computation.Model, stationID, candidateStart.Weekday(), candidateStart.Hour())
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
					return 0, err
				}
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
	if _, err := db.ExecContext(ctx, `
		DELETE FROM prediction_runs
		WHERE NOT EXISTS (SELECT 1 FROM price_predictions pp WHERE pp.run_id = prediction_runs.id)
	`); err != nil {
		return 0, err
	}
	return int(pruned), nil
}
