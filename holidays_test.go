package main

import (
	"testing"
	"time"
)

func TestEasterSundayKnownYears(t *testing.T) {
	// Reference dates for Gregorian Easter, including the 2038 late extreme
	// and the 2285 earliest-possible date (22 March).
	cases := map[int]string{
		2024: "2024-03-31",
		2025: "2025-04-20",
		2026: "2026-04-05",
		2027: "2027-03-28",
		2028: "2028-04-16", // leap year
		2030: "2030-04-21",
		2038: "2038-04-25", // latest possible date
		2285: "2285-03-22", // earliest possible date
	}
	for year, want := range cases {
		got := easterSunday(year).Format("2006-01-02")
		if got != want {
			t.Errorf("easterSunday(%d) = %s, want %s", year, got, want)
		}
		if wd := easterSunday(year).Weekday(); wd != time.Sunday {
			t.Errorf("easterSunday(%d) falls on %s, want Sunday", year, wd)
		}
	}
}

func TestIsGermanHoliday(t *testing.T) {
	cases := []struct {
		date string
		want bool
		name string
	}{
		{"2026-01-01", true, "Neujahr"},
		{"2026-04-03", true, "Karfreitag (Easter 2026-04-05 minus 2)"},
		{"2026-04-06", true, "Ostermontag"},
		{"2026-05-01", true, "Tag der Arbeit"},
		{"2026-05-14", true, "Christi Himmelfahrt (Easter plus 39)"},
		{"2026-05-25", true, "Pfingstmontag (Easter plus 50)"},
		{"2026-10-03", true, "Tag der Deutschen Einheit"},
		{"2026-12-25", true, "1. Weihnachtstag"},
		{"2026-12-26", true, "2. Weihnachtstag"},
		// A different year, to prove the movable feasts really move.
		{"2025-04-18", true, "Karfreitag 2025"},
		{"2025-04-03", false, "ordinary Thursday in 2025 (Karfreitag 2026 date)"},
		// Ordinary days and deliberate near-misses.
		{"2026-04-05", false, "Ostersonntag is a Sunday, not a separate bucket"},
		{"2026-04-07", false, "day after Ostermontag"},
		{"2026-12-24", false, "Heiligabend is not a public holiday"},
		{"2026-12-31", false, "Silvester is not a public holiday"},
		{"2026-10-31", false, "Reformationstag is state-specific, not nationwide"},
		{"2026-06-04", false, "Fronleichnam is state-specific, not nationwide"},
		{"2026-07-15", false, "ordinary Wednesday"},
	}
	for _, tc := range cases {
		day, err := time.Parse("2006-01-02", tc.date)
		if err != nil {
			t.Fatalf("parse %s: %v", tc.date, err)
		}
		if got := isGermanHoliday(day); got != tc.want {
			t.Errorf("isGermanHoliday(%s) = %v, want %v (%s)", tc.date, got, tc.want, tc.name)
		}
	}
}

func TestIsGermanHolidayIgnoresTimeOfDay(t *testing.T) {
	for _, hour := range []int{0, 9, 23} {
		day := time.Date(2026, 5, 1, hour, 30, 0, 0, time.UTC)
		if !isGermanHoliday(day) {
			t.Errorf("isGermanHoliday at %02d:30 on Tag der Arbeit = false, want true", hour)
		}
	}
}

func TestBuildForecastModelKeepsHolidaysOutOfWeekdayBuckets(t *testing.T) {
	// 2026-05-01 (Tag der Arbeit) is a Friday. Build a model over Fridays only
	// so the weekday bucket is unambiguous.
	var intervals []priceInterval
	for _, day := range []int{10, 17, 24} { // ordinary Fridays in April 2026
		intervals = append(intervals, sawtoothIntervals("s1",
			time.Date(2026, 4, day, 0, 0, 0, 0, time.UTC), 1, func(int) float64 { return 2.00 })...)
	}
	holiday := sawtoothIntervals("s1", time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC), 1,
		func(int) float64 { return 2.00 })
	intervals = append(intervals, holiday...)

	now := time.Date(2026, 5, 2, 0, 30, 0, 0, time.UTC)
	model := buildForecastModel(intervals, now, time.UTC)

	// Every weekday sample must come from an ordinary Friday.
	for key, samples := range model.WeekdayHour {
		for _, sample := range samples {
			if sample.Date == "2026-05-01" {
				t.Fatalf("holiday sample leaked into weekday bucket %+v", key)
			}
		}
	}
	// The holiday's prices are still available in the hour and recent sets.
	foundHour := false
	for _, samples := range model.Hour {
		for _, sample := range samples {
			if sample.Date == "2026-05-01" {
				foundHour = true
			}
		}
	}
	if !foundHour {
		t.Fatal("holiday samples missing from the hour buckets: price data was dropped, not redirected")
	}
}

func TestScoreForecastOnHolidayIgnoresWeekdayBucket(t *testing.T) {
	// A full month of history: enough same-weekday samples for the weekday
	// branch (4 Fridays) and enough same-hour samples to reach "medium".
	// 2026-05-01 and 2026-05-08 are both Fridays; only the first is a holiday,
	// so the two targets differ in nothing but their holiday status.
	var intervals []priceInterval
	for day := 3; day <= 30; day++ {
		intervals = append(intervals, sawtoothIntervals("s1",
			time.Date(2026, 4, day, 0, 0, 0, 0, time.UTC), 1, func(int) float64 { return 2.00 })...)
	}
	now := time.Date(2026, 4, 30, 23, 30, 0, 0, time.UTC)
	model := buildForecastModel(intervals, now, time.UTC)

	holidayScore, ok := scoreForecast(model, "s1", time.Date(2026, 5, 1, 11, 0, 0, 0, time.UTC))
	if !ok {
		t.Fatal("scoreForecast returned !ok for the holiday target")
	}
	ordinaryScore, ok := scoreForecast(model, "s1", time.Date(2026, 5, 8, 11, 0, 0, 0, time.UTC))
	if !ok {
		t.Fatal("scoreForecast returned !ok for the ordinary Friday target")
	}
	// The ordinary Friday takes the weekday branch and clears "low"; the
	// holiday must fall back to the hour/recent blend, which cannot.
	if ordinaryScore.Confidence == "low" {
		t.Fatalf("ordinary Friday confidence = %q, want above low: the fixture must exercise the weekday branch",
			ordinaryScore.Confidence)
	}
	if holidayScore.Confidence != "low" {
		t.Fatalf("holiday confidence = %q, want low (weekday bucket must be bypassed)", holidayScore.Confidence)
	}
}
