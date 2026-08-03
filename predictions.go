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
	// predictionBiasMaxLeadMinutes restricts the per-station level learning
	// to short-lead predictions: long-lead errors are dominated by unknowable
	// future level moves, not station-specific bias.
	predictionBiasMaxLeadMinutes = 360
	// predictionHourBiasMaxLeadMinutes bounds which leads train the
	// hour-of-day correction grid. The intraday shape error is visible at
	// every lead, but beyond a day the level surprise would drown it out.
	predictionHourBiasMaxLeadMinutes = 1440
	// predictionHourBiasMinSamples gates one grid cell. Cells are
	// market-wide, so this is easily met wherever the correction matters.
	predictionHourBiasMinSamples = 50
	// predictionHourBiasMaxAbs caps one grid cell in euro. Wider than the
	// station cap because the residual it corrects (the noon spike miss) is
	// itself several cents.
	predictionHourBiasMaxAbs = 0.08
	// predictionLearnErrorClampEuro winsorizes evaluated errors before any
	// learning. Outages and midpoint artifacts produce 30+ ct outliers that
	// must not steer corrections; the clamp keeps their sign but not their
	// leverage.
	predictionLearnErrorClampEuro = 0.15
	// predictionConfidenceMinSamples gates one empirical confidence cell.
	predictionConfidenceMinSamples = 30
	// predictionConfidenceHighMaxErrEuro / MediumMaxErrEuro map a cell's p80
	// absolute residual onto labels: a "high" confidence prediction is wrong
	// by more than 2 ct at most one time in five.
	predictionConfidenceHighMaxErrEuro   = 0.02
	predictionConfidenceMediumMaxErrEuro = 0.04
	// predictionSuggestionBiasMinSamples / MaxAbs gate and cap the measured
	// selection bias of suggested windows.
	predictionSuggestionBiasMinSamples = 30
	predictionSuggestionBiasMaxAbs     = 0.05
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
	// The floor depends only on (station, pricing day), and the persist timer
	// runs hourly, so a station accumulates roughly 24 decisions sharing one
	// window. Memoizing collapses those to a single query each.
	type floorKey struct {
		StationID string
		DayStart  time.Time
	}
	type floorResult struct {
		Price float64
		At    string
		Found bool
	}
	floors := make(map[floorKey]floorResult)

	for _, decision := range pending {
		key := floorKey{StationID: decision.StationID, DayStart: decision.DayStart}
		result, cached := floors[key]
		if !cached {
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
			result = floorResult{Price: floorPrice, At: floorAt, Found: err == nil}
			floors[key] = result
		}
		var (
			floor  sql.NullFloat64
			at     sql.NullString
			regret sql.NullFloat64
		)
		if result.Found {
			floor = sql.NullFloat64{Float64: result.Price, Valid: true}
			at = sql.NullString{String: result.At, Valid: true}
			// Signed on purpose. The observed price normally lies inside the
			// pricing day, so regret is >= 0; a negative value means the
			// decision was taken against a snapshot older than the day it was
			// scored against. That is worth seeing — observed_at reveals it —
			// so it is recorded rather than clamped away.
			regret = sql.NullFloat64{Float64: decision.Observed - result.Price, Valid: true}
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

// learnedCorrections bundles everything the evaluate-and-correct loop feeds
// back into the model: the hour-of-day grid, per-station level bias,
// empirical confidence, and the suggestion selection bias. All of it derives
// from the same evaluated price_predictions rows.
type learnedCorrections struct {
	StationBias      map[string]float64
	HourLeadBias     map[hourLeadKey]float64
	ConfidenceByLead map[stationLeadKey]string
	SuggestionBias   float64
}

// applyLearnedCorrections loads the learned corrections and attaches them to
// the model, so every consumer of scoreForecast (suggest, check, notify)
// benefits. With no persisted evaluation data this is a no-op.
func applyLearnedCorrections(ctx context.Context, db *sql.DB, model *forecastModel, fuel string, now time.Time, location *time.Location) error {
	corrections, err := loadLearnedCorrections(ctx, db, fuel, now, location)
	if err != nil {
		return err
	}
	for stationID, correction := range corrections.StationBias {
		station, ok := model.Stations[stationID]
		if !ok {
			continue
		}
		station.BiasCorrection = correction
		model.Stations[stationID] = station
	}
	model.HourLeadBias = corrections.HourLeadBias
	model.ConfidenceByLead = corrections.ConfidenceByLead
	model.SuggestionBias = corrections.SuggestionBias
	return nil
}

// evaluatedError is one evaluated prediction row prepared for learning: the
// error is winsorized, the weight decays with evaluation age, and the cell
// locates the row on the hour-lead correction grid.
type evaluatedError struct {
	StationID    string
	Lead         leadBucket
	Cell         hourLeadKey
	IsSuggestion bool
	Error        float64
	Weight       float64
}

// loadLearnedCorrections derives every learned correction from recent
// evaluated predictions in one pass. The corrections are applied on top of
// future predictions, which closes the loop: once corrected predictions are
// persisted and evaluated, their residual errors shrink and each correction
// converges instead of compounding. Later corrections are learned from
// residuals after earlier ones (grid first, then station bias, then
// suggestion bias) so they never double-count the same error.
func loadLearnedCorrections(ctx context.Context, db *sql.DB, fuel string, now time.Time, location *time.Location) (learnedCorrections, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if location == nil {
		location = time.Local
	}
	corrections := learnedCorrections{
		StationBias:      make(map[string]float64),
		HourLeadBias:     make(map[hourLeadKey]float64),
		ConfidenceByLead: make(map[stationLeadKey]string),
	}
	windowStart := now.AddDate(0, 0, -predictionBiasWindowDays).UTC().Format(time.RFC3339)
	// Suggestion rows are included at any lead: suggestions routinely target
	// windows days ahead, and their selection bias must be measured where
	// they actually live.
	rows, err := db.QueryContext(ctx, `
		SELECT station_id, target_start, lead_minutes, is_suggestion, error, evaluated_at
		FROM price_predictions
		WHERE fuel = ?
			AND error IS NOT NULL
			AND evaluated_at >= ?
			AND (lead_minutes <= ? OR is_suggestion = 1)
	`, fuel, windowStart, predictionHourBiasMaxLeadMinutes)
	if err != nil {
		return learnedCorrections{}, err
	}
	defer rows.Close()

	var evaluated []evaluatedError
	hourSamples := make(map[hourLeadKey][]priceSample)
	for rows.Next() {
		var (
			stationID       string
			targetStartText string
			leadMinutes     int
			isSuggestion    int
			predictionError float64
			evaluatedAtText string
		)
		if err := rows.Scan(&stationID, &targetStartText, &leadMinutes, &isSuggestion, &predictionError, &evaluatedAtText); err != nil {
			return learnedCorrections{}, err
		}
		targetStart, err := time.Parse(time.RFC3339, targetStartText)
		if err != nil {
			return learnedCorrections{}, fmt.Errorf("parse target_start %q: %w", targetStartText, err)
		}
		evaluatedAt, err := time.Parse(time.RFC3339, evaluatedAtText)
		if err != nil {
			return learnedCorrections{}, fmt.Errorf("parse evaluated_at %q: %w", evaluatedAtText, err)
		}
		if predictionError > predictionLearnErrorClampEuro {
			predictionError = predictionLearnErrorClampEuro
		}
		if predictionError < -predictionLearnErrorClampEuro {
			predictionError = -predictionLearnErrorClampEuro
		}
		ageDays := now.Sub(evaluatedAt).Hours() / 24
		if ageDays < 0 {
			ageDays = 0
		}
		targetLocal := targetStart.In(location)
		lead := leadBucketFor(float64(leadMinutes))
		cell := lead
		if cell == leadBucketBeyond24h {
			// Mirrors scoreForecast: long leads reuse the 6-24h grid cell.
			cell = leadBucket6to24h
		}
		row := evaluatedError{
			StationID:    stationID,
			Lead:         lead,
			Cell:         hourLeadKey{Hour: targetLocal.Hour(), Lead: cell, Weekend: isWeekendLike(targetLocal)},
			IsSuggestion: isSuggestion != 0,
			Error:        predictionError,
			Weight:       math.Exp(-ageDays / predictionBiasHalfLifeDays),
		}
		evaluated = append(evaluated, row)
		if leadMinutes <= predictionHourBiasMaxLeadMinutes {
			hourSamples[row.Cell] = append(hourSamples[row.Cell], priceSample{Price: row.Error, Weight: row.Weight})
		}
	}
	if err := rows.Err(); err != nil {
		return learnedCorrections{}, err
	}

	// 1) Hour-lead grid: the market-wide shape residual per local target
	// hour, lead bucket and day class.
	for cell, samples := range hourSamples {
		if len(samples) < predictionHourBiasMinSamples {
			continue
		}
		median, ok := weightedMedianPrice(samples)
		if !ok {
			continue
		}
		median = clampAbs(median, predictionHourBiasMaxAbs)
		if median == 0 {
			continue
		}
		corrections.HourLeadBias[cell] = median
	}

	// 2) Per-station level bias, from short-lead residuals after the grid.
	// Short leads only: past 6h the residual is dominated by level moves the
	// station could not have known, not by station-specific bias.
	stationSamples := make(map[string][]priceSample)
	for _, row := range evaluated {
		if row.Lead != leadBucket0to1h && row.Lead != leadBucket1to6h {
			continue
		}
		residual := row.Error - corrections.HourLeadBias[row.Cell]
		stationSamples[row.StationID] = append(stationSamples[row.StationID], priceSample{Price: residual, Weight: row.Weight})
	}
	for stationID, samples := range stationSamples {
		if len(samples) < predictionBiasMinSamples {
			continue
		}
		median, ok := weightedMedianPrice(samples)
		if !ok {
			continue
		}
		median = clampAbs(median, predictionBiasMaxAbs)
		if median == 0 {
			continue
		}
		corrections.StationBias[stationID] = median
	}

	// 3) Empirical confidence: the p80 absolute residual (after grid and
	// station corrections) per station and lead bucket, mapped onto labels.
	// Measured accuracy replaces the sample-count heuristic, which the data
	// showed inverted (its "low" beat its "medium").
	confidenceResiduals := make(map[stationLeadKey][]float64)
	for _, row := range evaluated {
		if row.Lead == leadBucketBeyond24h {
			continue
		}
		residual := row.Error - corrections.HourLeadBias[row.Cell] - corrections.StationBias[row.StationID]
		key := stationLeadKey{StationID: row.StationID, Lead: row.Lead}
		confidenceResiduals[key] = append(confidenceResiduals[key], math.Abs(residual))
	}
	for key, residuals := range confidenceResiduals {
		if len(residuals) < predictionConfidenceMinSamples {
			continue
		}
		sort.Float64s(residuals)
		p80 := residuals[(len(residuals)*8)/10]
		switch {
		case p80 <= predictionConfidenceHighMaxErrEuro:
			corrections.ConfidenceByLead[key] = "high"
		case p80 <= predictionConfidenceMediumMaxErrEuro:
			corrections.ConfidenceByLead[key] = "medium"
		default:
			corrections.ConfidenceByLead[key] = "low"
		}
	}

	// 4) Suggestion selection bias: what remains of suggested rows' error
	// after all model-level corrections is the winner's curse of picking the
	// minimum across many noisy candidates. Persisted predictions never
	// include it (they store the raw model), so this stays a pure
	// measurement and cannot feed back on itself.
	var suggestionSamples []priceSample
	for _, row := range evaluated {
		if !row.IsSuggestion {
			continue
		}
		residual := row.Error - corrections.HourLeadBias[row.Cell] - corrections.StationBias[row.StationID]
		suggestionSamples = append(suggestionSamples, priceSample{Price: residual, Weight: row.Weight})
	}
	if len(suggestionSamples) >= predictionSuggestionBiasMinSamples {
		if median, ok := weightedMedianPrice(suggestionSamples); ok {
			corrections.SuggestionBias = clampAbs(median, predictionSuggestionBiasMaxAbs)
		}
	}
	return corrections, nil
}

func clampAbs(value, limit float64) float64 {
	if value > limit {
		return limit
	}
	if value < -limit {
		return -limit
	}
	return value
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
			// The unrounded values the verdict was decided on, not the
			// display-rounded ones: the day floor this is later compared
			// against comes straight from price_snapshots at full precision,
			// so rounding here would put a half-cent artifact into both the
			// stored error and the regret.
			check.rawCurrentPrice,
			check.RecordedAt,
			check.rawPredictedPrice,
			check.rawCurrentPrice-check.rawPredictedPrice,
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
