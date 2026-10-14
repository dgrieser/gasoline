package main

import (
	"context"
	"database/sql"
	"math"
	"testing"
	"time"
)

func insertPriceEvent(t *testing.T, db *sql.DB, effectiveAt, fuel string, delta float64) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), `
		INSERT INTO price_events (effective_at, fuel, delta_euro, note, created_at)
		VALUES (?, ?, ?, '', '2026-10-14T00:00:00Z')
	`, effectiveAt, fuel, delta); err != nil {
		t.Fatalf("insert price event: %v", err)
	}
}

func TestLoadPriceEventsParsesAndSkipsBadRows(t *testing.T) {
	db := openTestDB(t)
	insertPriceEvent(t, db, "2027-01-01T12:00", "all", 0.167)
	insertPriceEvent(t, db, "2026-10-01T00:00", " Diesel ", -0.167)
	insertPriceEvent(t, db, "tomorrow", "e5", 0.05)

	events, err := loadPriceEvents(context.Background(), db, time.UTC)
	if err != nil {
		t.Fatalf("loadPriceEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("events = %+v, want the two parseable rows", events)
	}
	if !events[0].EffectiveAt.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) || events[0].Fuel != "diesel" {
		t.Fatalf("first event = %+v, want the Oct 1 diesel cut, fuel normalized", events[0])
	}
	if got := priceEventsForFuel(events, "e5"); len(got) != 1 || got[0].Delta != 0.167 {
		t.Fatalf("e5 events = %+v, want only the all-fuel event", got)
	}
	if got := priceEventsForFuel(events, "diesel"); len(got) != 2 {
		t.Fatalf("diesel events = %+v, want both", got)
	}
}

func TestRestateIntervalsForEventsSplitsAndShifts(t *testing.T) {
	day := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	intervals := []priceInterval{
		{StationID: "s", Start: day.Add(-6 * time.Hour), End: day, Price: 2.40},
		{StationID: "s", Start: day, End: day.Add(6 * time.Hour), Price: 2.30},
		{StationID: "s", Start: day.Add(6 * time.Hour), End: day.Add(9 * time.Hour), Price: 2.20},
	}
	events := []priceEvent{
		{EffectiveAt: day.Add(3 * time.Hour), Delta: -0.15},
		// Still in the future at now: must not touch the history.
		{EffectiveAt: day.Add(48 * time.Hour), Delta: 0.17},
	}
	now := day.Add(9 * time.Hour)

	got := restateIntervalsForEvents(intervals, events, now)
	want := []priceInterval{
		{StationID: "s", Start: day.Add(-6 * time.Hour), End: day, Price: 2.25},
		{StationID: "s", Start: day, End: day.Add(3 * time.Hour), Price: 2.15},
		{StationID: "s", Start: day.Add(3 * time.Hour), End: day.Add(6 * time.Hour), Price: 2.30},
		{StationID: "s", Start: day.Add(6 * time.Hour), End: day.Add(9 * time.Hour), Price: 2.20},
	}
	if len(got) != len(want) {
		t.Fatalf("restated = %+v, want %+v", got, want)
	}
	for i := range want {
		if !got[i].Start.Equal(want[i].Start) || !got[i].End.Equal(want[i].End) || math.Abs(got[i].Price-want[i].Price) > 1e-9 {
			t.Fatalf("piece %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if intervals[1].Price != 2.30 || !intervals[1].End.Equal(day.Add(6*time.Hour)) {
		t.Fatalf("input modified: %+v", intervals[1])
	}
	if same := restateIntervalsForEvents(intervals, events[1:], now); len(same) != len(intervals) || same[0].Price != 2.40 {
		t.Fatalf("future-only events changed the history: %+v", same)
	}
}

// TestRestateIntervalsForEventsBoundaries pins the edges: an event at an
// interval's start belongs to the interval (no split, no shift), one at its end
// shifts the whole interval, and one exactly at now has taken effect.
func TestRestateIntervalsForEventsBoundaries(t *testing.T) {
	day := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	interval := priceInterval{StationID: "s", Start: day, End: day.Add(6 * time.Hour), Price: 2.30}
	now := day.Add(6 * time.Hour)
	for _, tc := range []struct {
		name  string
		at    time.Time
		price float64
	}{
		{"event at the interval start", day, 2.30},
		{"event at the interval end", day.Add(6 * time.Hour), 2.15},
		{"event exactly at now", now, 2.15},
	} {
		got := restateIntervalsForEvents([]priceInterval{interval}, []priceEvent{{EffectiveAt: tc.at, Delta: -0.15}}, now)
		if len(got) != 1 || !got[0].Start.Equal(interval.Start) || !got[0].End.Equal(interval.End) || math.Abs(got[0].Price-tc.price) > 1e-9 {
			t.Fatalf("%s: restated = %+v, want one unsplit piece at %.2f", tc.name, got, tc.price)
		}
	}
	if got := restateIntervalsForEvents([]priceInterval{interval}, []priceEvent{{EffectiveAt: now.Add(time.Second), Delta: -0.15}}, now); got[0].Price != 2.30 {
		t.Fatalf("an event a second after now restated the history: %+v", got)
	}
}

func TestScoreForecastAppliesFutureEvent(t *testing.T) {
	firstDay := time.Date(2026, 12, 18, 0, 0, 0, 0, time.UTC)
	intervals := sawtoothIntervals("s1", firstDay, 14, func(int) float64 { return 2.10 })
	now := time.Date(2027, 1, 1, 0, 30, 0, 0, time.UTC)
	model := buildForecastModel(intervals, now, time.UTC)
	withEvent := model
	withEvent.FutureEvents = []priceEvent{{EffectiveAt: time.Date(2027, 1, 1, 12, 0, 0, 0, time.UTC), Delta: 0.167}}

	for _, tc := range []struct {
		target time.Time
		shift  float64
	}{
		{time.Date(2027, 1, 1, 11, 0, 0, 0, time.UTC), 0},
		{time.Date(2027, 1, 1, 12, 0, 0, 0, time.UTC), 0.167},
		{time.Date(2027, 1, 2, 9, 0, 0, 0, time.UTC), 0.167},
	} {
		base, ok := scoreForecast(model, "s1", tc.target)
		if !ok {
			t.Fatalf("scoreForecast(%s) returned !ok", tc.target)
		}
		shifted, ok := scoreForecast(withEvent, "s1", tc.target)
		if !ok {
			t.Fatalf("scoreForecast(%s) with event returned !ok", tc.target)
		}
		if diff := shifted.PredictedPrice - base.PredictedPrice; math.Abs(diff-tc.shift) > 1e-9 {
			t.Fatalf("%s: event moved the forecast by %.4f, want %.4f", tc.target, diff, tc.shift)
		}
		if shifted.LearnedCorrection != base.LearnedCorrection {
			t.Fatalf("%s: event leaked into LearnedCorrection", tc.target)
		}
	}
}

// TestBuildCorrectedForecastModelAbsorbsPastEvent is the Oct 1 2026 case: a
// tax cut lands mid pricing day, an hour before the run. Without the event the
// open day is 18 hours of the old level against one of the new, so the model
// keeps the old level; with it the history is restated and the forecast is on
// the new level at once.
func TestBuildCorrectedForecastModelAbsorbsPastEvent(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	firstDay := time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC)
	cut := time.Date(2026, 10, 1, 6, 0, 0, 0, time.UTC)
	var intervals []priceInterval
	for _, interval := range sawtoothIntervals("s1", firstDay, 15, func(int) float64 { return 2.40 }) {
		if !interval.Start.Before(cut) {
			interval.Price -= 0.15
			intervals = append(intervals, interval)
			continue
		}
		if interval.End.After(cut) {
			after := interval
			after.Start = cut
			after.Price -= 0.15
			interval.End = cut
			intervals = append(intervals, interval, after)
			continue
		}
		intervals = append(intervals, interval)
	}
	now := time.Date(2026, 10, 1, 7, 0, 0, 0, time.UTC)
	// Nothing after now has been recorded yet.
	var recorded []priceInterval
	for _, interval := range intervals {
		if !interval.Start.Before(now) {
			continue
		}
		if interval.End.After(now) {
			interval.End = now
		}
		recorded = append(recorded, interval)
	}
	intervals = recorded
	targets := []time.Time{
		time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC),
	}

	plain, err := buildCorrectedForecastModel(ctx, db, intervals, "diesel", now, time.UTC)
	if err != nil {
		t.Fatalf("build without events: %v", err)
	}
	insertPriceEvent(t, db, "2026-10-01T06:00", "all", -0.15)
	restated, err := buildCorrectedForecastModel(ctx, db, intervals, "diesel", now, time.UTC)
	if err != nil {
		t.Fatalf("build with event: %v", err)
	}
	if len(restated.FutureEvents) != 0 {
		t.Fatalf("a past event stayed in FutureEvents: %+v", restated.FutureEvents)
	}
	for _, target := range targets {
		before, ok := scoreForecast(plain, "s1", target)
		if !ok {
			t.Fatalf("scoreForecast(%s) returned !ok", target)
		}
		after, ok := scoreForecast(restated, "s1", target)
		if !ok {
			t.Fatalf("scoreForecast(%s) with event returned !ok", target)
		}
		// The sawtooth's 08:00-11:00 segment is base-0.10: 2.30 before the
		// cut, 2.15 after it.
		if after.PredictedPrice < 2.14 || after.PredictedPrice > 2.16 {
			t.Fatalf("%s with event = %.4f, want ~2.15 on the new level", target, after.PredictedPrice)
		}
		if before.PredictedPrice < 2.25 {
			t.Fatalf("%s without event = %.4f, want the old level the fixture is built to show", target, before.PredictedPrice)
		}
	}
}

// marketFixture builds 15 days of the regulated sawtooth for several stations,
// lets the given stations step by delta at shiftAt, and keeps only what was
// recorded before now.
func marketFixture(stations []string, shifted map[string]bool, shiftAt time.Time, delta float64, now time.Time) []priceInterval {
	firstDay := time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC)
	var out []priceInterval
	for i, station := range stations {
		base := 2.30 + float64(i)*0.02
		for _, interval := range sawtoothIntervals(station, firstDay, 15, func(int) float64 { return base }) {
			if !interval.Start.Before(now) {
				continue
			}
			if interval.End.After(now) {
				interval.End = now
			}
			if !shifted[station] {
				out = append(out, interval)
				continue
			}
			switch {
			case !interval.Start.Before(shiftAt):
				interval.Price += delta
				out = append(out, interval)
			case interval.End.After(shiftAt):
				after := interval
				after.Start = shiftAt
				after.Price += delta
				interval.End = shiftAt
				out = append(out, interval, after)
			default:
				out = append(out, interval)
			}
		}
	}
	return out
}

func TestDetectMarketShiftReanchorsWithinTheHour(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	stations := []string{"s1", "s2", "s3", "s4", "s5", "s6"}
	all := map[string]bool{}
	for _, station := range stations {
		all[station] = true
	}
	shiftAt := time.Date(2026, 10, 1, 6, 0, 0, 0, time.UTC)
	now := time.Date(2026, 10, 1, 7, 0, 0, 0, time.UTC)
	intervals := marketFixture(stations, all, shiftAt, -0.12, now)

	model := buildForecastModel(intervals, now, time.UTC)
	shift, ok := detectMarketShift(model, intervals, now, time.UTC)
	if !ok {
		t.Fatal("market-wide -12 ct step was not detected")
	}
	if !shift.EffectiveAt.Equal(shiftAt) || math.Abs(shift.Delta+0.12) > 0.005 {
		t.Fatalf("detected %+v, want -0.12 at %s", shift, shiftAt)
	}

	corrected, err := buildCorrectedForecastModel(ctx, db, intervals, "diesel", now, time.UTC)
	if err != nil {
		t.Fatalf("buildCorrectedForecastModel: %v", err)
	}
	target := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	for i, station := range stations {
		// 08:00-11:00 is base-0.10; after the step that is base-0.22.
		want := 2.30 + float64(i)*0.02 - 0.22
		score, ok := scoreForecast(corrected, station, target)
		if !ok {
			t.Fatalf("%s: scoreForecast returned !ok", station)
		}
		if math.Abs(score.PredictedPrice-want) > 0.01 {
			t.Fatalf("%s 09:00 = %.4f, want ~%.4f on the new level", station, score.PredictedPrice, want)
		}
	}
}

func TestDetectMarketShiftIgnoresSingleStationsAndQuietDays(t *testing.T) {
	stations := []string{"s1", "s2", "s3", "s4", "s5", "s6"}
	shiftAt := time.Date(2026, 10, 1, 6, 0, 0, 0, time.UTC)
	now := time.Date(2026, 10, 1, 7, 0, 0, 0, time.UTC)

	quiet := marketFixture(stations, nil, shiftAt, 0, now)
	if shift, ok := detectMarketShift(buildForecastModel(quiet, now, time.UTC), quiet, now, time.UTC); ok {
		t.Fatalf("quiet day detected a shift: %+v", shift)
	}

	// Two of six stations repricing is a brand move, not the market.
	partial := marketFixture(stations, map[string]bool{"s1": true, "s2": true}, shiftAt, -0.12, now)
	if shift, ok := detectMarketShift(buildForecastModel(partial, now, time.UTC), partial, now, time.UTC); ok {
		t.Fatalf("two-station move detected as market shift: %+v", shift)
	}

	// Too few stations to call anything a market.
	few := marketFixture(stations[:3], map[string]bool{"s1": true, "s2": true, "s3": true}, shiftAt, -0.12, now)
	if shift, ok := detectMarketShift(buildForecastModel(few, now, time.UTC), few, now, time.UTC); ok {
		t.Fatalf("three-station market detected a shift: %+v", shift)
	}
}

func TestBestStepSplit(t *testing.T) {
	if got := bestStepSplit(nil); got != 0 {
		t.Fatalf("empty split = %d, want 0", got)
	}
	if got := bestStepSplit([]float64{0, 0, 0, 0, -0.12, -0.12}); got != 4 {
		t.Fatalf("split = %d, want 4", got)
	}
	if got := bestStepSplit([]float64{0.01, -0.01, 0.0, 0.08, 0.09, 0.07, 0.08}); got != 3 {
		t.Fatalf("split = %d, want 3", got)
	}
}

// TestDetectMarketShiftIgnoresTheWeekendShape pins the weekday-specific
// de-shaping: weekends here raise far less at noon than weekdays, which against
// the all-week pattern reads as a step of several cents inside the pricing day.
// It is the stations' normal weekend, not a market move — on a plain Saturday,
// and on a holiday Saturday (Oct 3), which prices like a Sunday.
func TestDetectMarketShiftIgnoresTheWeekendShape(t *testing.T) {
	type segment struct {
		startHour int
		offset    float64
	}
	weekday := []segment{{0, -0.05}, {8, -0.10}, {11, -0.12}, {12, 0.25}, {14, 0.10}, {18, 0.02}, {22, -0.05}}
	weekend := []segment{{0, -0.05}, {8, -0.07}, {11, -0.08}, {12, 0.04}, {14, 0.00}, {18, -0.02}, {22, -0.05}}
	for _, now := range []time.Time{
		time.Date(2026, 10, 10, 19, 0, 0, 0, time.UTC), // Saturday
		time.Date(2026, 10, 3, 19, 0, 0, 0, time.UTC),  // Saturday, Tag der Deutschen Einheit
	} {
		firstDay := now.Truncate(24*time.Hour).AddDate(0, 0, -28)
		var intervals []priceInterval
		for i, station := range []string{"s1", "s2", "s3", "s4", "s5", "s6"} {
			base := 2.30 + float64(i)*0.02
			for day := 0; day <= 28; day++ {
				dayStart := firstDay.AddDate(0, 0, day)
				segments := weekday
				if isWeekendLike(dayStart) {
					segments = weekend
				}
				for j, seg := range segments {
					start := dayStart.Add(time.Duration(seg.startHour) * time.Hour)
					end := dayStart.AddDate(0, 0, 1)
					if j+1 < len(segments) {
						end = dayStart.Add(time.Duration(segments[j+1].startHour) * time.Hour)
					}
					if !start.Before(now) {
						continue
					}
					if end.After(now) {
						end = now
					}
					intervals = append(intervals, priceInterval{StationID: station, StationName: station, Start: start, End: end, Price: base + seg.offset})
				}
			}
		}
		model := buildForecastModel(intervals, now, time.UTC)
		if model.JumpAnchorHour != 12 {
			t.Fatalf("%s: anchor = %d, want 12", now, model.JumpAnchorHour)
		}
		if shift, ok := detectMarketShift(model, intervals, now, time.UTC); ok {
			t.Fatalf("%s: an ordinary weekend day was detected as a market shift: %+v", now, shift)
		}
	}
}
