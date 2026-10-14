package main

import (
	"context"
	"database/sql"
	"math"
	"sort"
	"time"
)

// Market-wide price level moves.
//
// The forecast learns a station's intraday shape on top of a per-pricing-day
// level, and holds that level flat into the future: nothing in the price
// history says when the whole market will jump. Two kinds of jump are handled
// here.
//
// Known events (a tax or levy change, see priceEvent) are announced ahead of
// time. Before one takes effect, targets after it carry its delta. Once it has
// taken effect, the history before it is restated into the new regime — every
// price before the event moves by the event's delta — so the level estimate,
// the day-over-day drift and the intraday offsets never see the step. Without
// that the open pricing day mixes both regimes and the level estimate keeps the
// old one until the post-event minutes outweigh the pre-event ones.
//
// Unannounced moves (an oil-driven cut, a late-passed tax change) are detected
// from the open pricing day itself: when most stations' de-shaped level stepped
// the same way at the same hour, the step is treated like a known event that
// took effect at that hour, for this run only (see detectMarketShift).

// Same-day shift detection thresholds. Structural, like the forecast model's.
const (
	// marketShiftMinMoveEuro is the smallest level step, per station and for
	// the market median, that counts as a shift. The intraday shape the model
	// expects for that day is removed before measuring, so ordinary sawtooth
	// moves stay well below it.
	marketShiftMinMoveEuro = 0.03
	// marketShiftAgreeShare is the share of stations that must have stepped
	// by at least marketShiftMinMoveEuro in the market's direction. One brand
	// repricing its stations is not a market move.
	marketShiftAgreeShare = 0.6
	// marketShiftMinStations is the fewest stations with enough data on both
	// sides of the step for a detection.
	marketShiftMinStations = 5
	// marketShiftMinBeforeMinutes / MinAfterMinutes are the coverage one
	// station needs before and after the step to report a move.
	marketShiftMinBeforeMinutes = 60
	marketShiftMinAfterMinutes  = 30
	// marketShiftMaxMoveEuro rejects steps no market makes in an hour; one
	// that size is a data problem, and restating the history by it would
	// spread the problem into every forecast.
	marketShiftMaxMoveEuro = 0.30
)

// priceEventsForFuel returns the events that move fuel, ordered by effective
// time.
func priceEventsForFuel(events []priceEvent, fuel string) []priceEvent {
	var out []priceEvent
	for _, event := range events {
		if event.Fuel == priceEventAllFuels || event.Fuel == fuel {
			out = append(out, event)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].EffectiveAt.Before(out[j].EffectiveAt)
	})
	return out
}

// eventShiftBetween sums the deltas of the events that take effect in
// (from, to].
func eventShiftBetween(events []priceEvent, from, to time.Time) float64 {
	var shift float64
	for _, event := range events {
		if event.EffectiveAt.After(from) && !event.EffectiveAt.After(to) {
			shift += event.Delta
		}
	}
	return shift
}

// futurePriceEvents keeps the events that have not taken effect at now.
func futurePriceEvents(events []priceEvent, now time.Time) []priceEvent {
	var out []priceEvent
	for _, event := range events {
		if event.EffectiveAt.After(now) {
			out = append(out, event)
		}
	}
	return out
}

// restateIntervalsForEvents moves the history into the price regime in force
// at now. An interval that straddles an event is split at it, and every piece
// gains the deltas of the events that took effect after it began and no later
// than now. Events still in the future change nothing here; scoreForecast adds
// them to the targets they precede. The input is not modified.
func restateIntervalsForEvents(intervals []priceInterval, events []priceEvent, now time.Time) []priceInterval {
	var past []priceEvent
	for _, event := range events {
		if !event.EffectiveAt.After(now) {
			past = append(past, event)
		}
	}
	if len(past) == 0 {
		return intervals
	}
	sort.SliceStable(past, func(i, j int) bool {
		return past[i].EffectiveAt.Before(past[j].EffectiveAt)
	})

	out := make([]priceInterval, 0, len(intervals))
	for _, interval := range intervals {
		start := interval.Start
		for _, event := range past {
			if !event.EffectiveAt.After(start) || !event.EffectiveAt.Before(interval.End) {
				continue
			}
			piece := interval
			piece.Start, piece.End = start, event.EffectiveAt
			piece.Price += eventShiftBetween(past, piece.Start, now)
			out = append(out, piece)
			start = event.EffectiveAt
		}
		piece := interval
		piece.Start = start
		piece.Price += eventShiftBetween(past, piece.Start, now)
		out = append(out, piece)
	}
	return out
}

// buildCorrectedForecastModel is the one way suggest, check and notify build a
// fuel's model: the known price events are applied to the history and to the
// targets, and the learned corrections are attached.
func buildCorrectedForecastModel(ctx context.Context, db *sql.DB, intervals []priceInterval, fuel string, now time.Time, location *time.Location) (forecastModel, error) {
	allEvents, err := loadPriceEvents(ctx, db, location)
	if err != nil {
		return forecastModel{}, err
	}
	events := priceEventsForFuel(allEvents, fuel)
	restated := restateIntervalsForEvents(intervals, events, now)
	model := buildForecastModel(restated, now, location)
	if shift, ok := detectMarketShift(model, restated, now, location); ok {
		// Rebuild once with the detected step folded into the history, so the
		// level estimate and the offsets see a single regime. The step is
		// re-detected from the data on every run, never stored.
		model = buildForecastModel(restateIntervalsForEvents(restated, []priceEvent{shift}, now), now, location)
	}
	model.FutureEvents = futurePriceEvents(events, now)
	if err := applyLearnedCorrections(ctx, db, &model, fuel, now, location); err != nil {
		return forecastModel{}, err
	}
	return model, nil
}

// detectMarketShift looks for a market-wide level step inside the open pricing
// day. Each offset-mode station's buckets are de-shaped by its learned hourly
// offsets, which leaves its level; the market index for an hour is the median,
// across stations, of that level relative to the station's own open-day median.
// The step's hour is the best two-segment split of that index. It is accepted
// when at least marketShiftMinStations stations have data on both sides and
// marketShiftAgreeShare of them moved at least marketShiftMinMoveEuro the same
// way; the returned event takes effect at the step's hour with the market median
// move as its delta.
func detectMarketShift(model forecastModel, intervals []priceInterval, now time.Time, location *time.Location) (priceEvent, bool) {
	nowLocal := now.In(location)
	currentDay := pricingDay(nowLocal, model.JumpAnchorHour)

	// Only the open pricing day matters, and it is at most a day long.
	recentFrom := now.Add(-25 * time.Hour)
	var recent []priceInterval
	for _, interval := range intervals {
		if interval.End.After(recentFrom) {
			recent = append(recent, interval)
		}
	}

	type hourLevel struct {
		hour    time.Time
		minutes float64
		level   float64
	}
	byStation := make(map[string][]hourLevel)
	for _, bucket := range collectHourBuckets(recent, now, location) {
		if pricingDay(bucket.Start, model.JumpAnchorHour) != currentDay || !model.Stations[bucket.StationID].OffsetMode {
			continue
		}
		offset, ok := expectedHourOffset(model, bucket.StationID, bucket.Start, currentDay)
		if !ok {
			continue
		}
		byStation[bucket.StationID] = append(byStation[bucket.StationID], hourLevel{
			hour: bucket.Start, minutes: bucket.Minutes, level: bucket.Price - offset,
		})
	}
	if len(byStation) < marketShiftMinStations {
		return priceEvent{}, false
	}

	// Market index per hour: each station's level relative to its open-day
	// median, so stations at different price levels can be pooled.
	relative := make(map[time.Time][]priceSample)
	for _, levels := range byStation {
		all := make([]priceSample, 0, len(levels))
		for _, l := range levels {
			all = append(all, priceSample{Price: l.level, Weight: l.minutes})
		}
		reference, ok := weightedMedianPrice(all)
		if !ok {
			continue
		}
		for _, l := range levels {
			relative[l.hour] = append(relative[l.hour], priceSample{Price: l.level - reference, Weight: l.minutes})
		}
	}
	var hours []time.Time
	for hour := range relative {
		hours = append(hours, hour)
	}
	if len(hours) < 2 {
		return priceEvent{}, false
	}
	sort.Slice(hours, func(i, j int) bool { return hours[i].Before(hours[j]) })
	index := make([]float64, 0, len(hours))
	for _, hour := range hours {
		value, _ := weightedMedianPrice(relative[hour])
		index = append(index, value)
	}
	split := bestStepSplit(index)
	if split <= 0 {
		return priceEvent{}, false
	}
	stepAt := hours[split]

	// Each station's own move across the step.
	var moves []float64
	for _, levels := range byStation {
		var before, after []priceSample
		var beforeMinutes, afterMinutes float64
		for _, l := range levels {
			sample := priceSample{Price: l.level, Weight: l.minutes}
			if l.hour.Before(stepAt) {
				before = append(before, sample)
				beforeMinutes += l.minutes
			} else {
				after = append(after, sample)
				afterMinutes += l.minutes
			}
		}
		if beforeMinutes < marketShiftMinBeforeMinutes || afterMinutes < marketShiftMinAfterMinutes {
			continue
		}
		beforeLevel, _ := weightedMedianPrice(before)
		afterLevel, _ := weightedMedianPrice(after)
		moves = append(moves, afterLevel-beforeLevel)
	}
	if len(moves) < marketShiftMinStations {
		return priceEvent{}, false
	}
	moveSamples := make([]priceSample, len(moves))
	for i, move := range moves {
		moveSamples[i] = priceSample{Price: move, Weight: 1}
	}
	delta, _ := weightedMedianPrice(moveSamples)
	if math.Abs(delta) < marketShiftMinMoveEuro || math.Abs(delta) > marketShiftMaxMoveEuro {
		return priceEvent{}, false
	}
	agree := 0
	for _, move := range moves {
		if math.Abs(move) >= marketShiftMinMoveEuro && (move > 0) == (delta > 0) {
			agree++
		}
	}
	if float64(agree) < marketShiftAgreeShare*float64(len(moves)) {
		return priceEvent{}, false
	}
	return priceEvent{EffectiveAt: stepAt, Fuel: priceEventAllFuels, Delta: delta, Note: "detected market shift"}, true
}

// bestStepSplit returns the index k (1 <= k < len(values)) that best splits
// values into two constant segments, values[:k] and values[k:], by least
// squares; 0 when there is nothing to split.
func bestStepSplit(values []float64) int {
	n := len(values)
	if n < 2 {
		return 0
	}
	prefix := make([]float64, n+1)
	prefixSq := make([]float64, n+1)
	for i, v := range values {
		prefix[i+1] = prefix[i] + v
		prefixSq[i+1] = prefixSq[i] + v*v
	}
	segmentCost := func(from, to int) float64 {
		count := float64(to - from)
		sum := prefix[to] - prefix[from]
		return prefixSq[to] - prefixSq[from] - sum*sum/count
	}
	best, bestCost := 0, math.Inf(1)
	for k := 1; k < n; k++ {
		if cost := segmentCost(0, k) + segmentCost(k, n); cost < bestCost-1e-12 {
			best, bestCost = k, cost
		}
	}
	return best
}

// expectedHourOffset is the intraday offset expected for a station at a local
// hour: the same weekday's hour where at least three samples exist, the plain
// hour otherwise. A public holiday prices like a Sunday (see isWeekendLike) and
// is kept out of its own weekday's samples, so it uses Sunday's. Removing the
// offset leaves the station's level. The weekday matters: against the all-week
// pattern a weekend's weaker noon raise reads as a level step of several cents.
//
// Samples from the open pricing day itself are left out. The model already
// holds them as offsets from the old level, so a step inside that day would
// otherwise partly reappear as shape and partly cancel itself.
func expectedHourOffset(model forecastModel, stationID string, at time.Time, openDay string) (float64, bool) {
	hour := at.Hour()
	// The one calendar date whose hour falls into the open pricing day: the
	// day's own date or the next one, decided exactly as pricingDay decides it.
	openDate := ""
	if day, err := time.ParseInLocation("2006-01-02", openDay, at.Location()); err == nil {
		for _, candidate := range []time.Time{day, day.AddDate(0, 0, 1)} {
			if pricingDay(candidate.Add(time.Duration(hour)*time.Hour), model.JumpAnchorHour) == openDay {
				openDate = candidate.Format("2006-01-02")
				break
			}
		}
	}
	settled := func(samples []priceSample) []priceSample {
		out := make([]priceSample, 0, len(samples))
		for _, sample := range samples {
			if sample.Date != openDate {
				out = append(out, sample)
			}
		}
		return out
	}
	hourOffset, ok := weightedMedianPrice(settled(model.Hour[stationHourKey{StationID: stationID, Hour: hour}]))
	if !ok {
		return 0, false
	}
	weekday := at.Weekday()
	if isGermanHoliday(at) {
		weekday = time.Sunday
	}
	sameWeekday := settled(model.WeekdayHour[stationWeekdayHourKey{StationID: stationID, Weekday: weekday, Hour: hour}])
	if len(sameWeekday) < 3 {
		return hourOffset, true
	}
	if weekdayOffset, ok := weightedMedianPrice(sameWeekday); ok {
		return weekdayOffset, true
	}
	return hourOffset, true
}
