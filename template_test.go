package main

import (
	"strings"
	"testing"
)

// withDecimalSeparator pins the locale decimal separator cache for a test.
func withDecimalSeparator(t *testing.T, sep string) {
	t.Helper()
	old := localeDecimalSep
	localeDecimalSep = sep
	t.Cleanup(func() { localeDecimalSep = old })
}

func withWeekdayNames(t *testing.T, long, short map[string]string) {
	t.Helper()
	oldLong, oldShort := localeWeekdayLong, localeWeekdayShort
	localeWeekdayLong, localeWeekdayShort = long, short
	t.Cleanup(func() {
		localeWeekdayLong, localeWeekdayShort = oldLong, oldShort
	})
}

func checkRowFixture(station, date, start string, price float64) notifyRow {
	return notifyRow{check: &priceCheckRow{
		RecordedAt:  "2026-04-26T15:00:00Z",
		StationID:   "id-" + station,
		StationName: station,
		DistanceKM:  1.25,
		Station: suggestionStationRow{
			ID: "id-" + station, Name: station, Brand: "ARAL", Street: "Test Street",
			HouseNumber: "1", PostCode: 10115, Place: "Berlin", Lat: 52.5, Lng: 13.4,
			FirstSeenAt: "2026-04-20T00:00:00Z", Address: "Test Street 1, 10115 Berlin",
			DistanceKM: 1.25,
		},
		Fuel:                  "diesel",
		CurrentPrice:          price,
		PredictedCurrentPrice: price + 0.05,
		HistoryPercentile:     12.5,
		Verdict:               "low",
		Recommendation:        "buy",
		ExpectedLower:         false,
		BestFutureDate:        date,
		BestFutureWeekday:     "Monday",
		BestFutureStartTime:   start,
		BestFutureEndTime:     "12:00",
		BestFuturePrice:       price - 0.02,
		ExpectedDrop:          0.02,
		Confidence:            "high",
		SampleCount:           42,
	}}
}

func suggestRowFixture(station, date, weekday, start string, price float64) notifyRow {
	return notifyRow{suggest: &suggestionRow{
		Date:        date,
		Weekday:     weekday,
		StartTime:   start,
		EndTime:     "12:00",
		StationID:   "id-" + station,
		StationName: station,
		DistanceKM:  2.5,
		Station: suggestionStationRow{
			ID: "id-" + station, Name: station, Brand: "ESSO", Street: "Other Street",
			HouseNumber: "2", PostCode: 10117, Place: "Berlin", Lat: 52.6, Lng: 13.5,
			FirstSeenAt: "2026-04-20T00:00:00Z", Address: "Other Street 2, 10117 Berlin",
			DistanceKM: 2.5,
		},
		Fuel:           "diesel",
		PredictedPrice: price,
		Confidence:     "medium",
		SampleCount:    7,
	}}
}

func TestTruncateDecimals(t *testing.T) {
	withDecimalSeparator(t, ".")
	cases := []struct {
		value    float64
		decimals int
		want     string
	}{
		{1.789, 2, "1.78"},
		{1.7, 2, "1.70"},
		{1.685, 2, "1.68"},
		{2, 2, "2.00"},
		{-1.239, 2, "-1.23"},
		{5, 0, "5"},
	}
	for _, tc := range cases {
		if got := truncateDecimals(tc.value, tc.decimals); got != tc.want {
			t.Errorf("truncateDecimals(%v, %d) = %q, want %q", tc.value, tc.decimals, got, tc.want)
		}
	}

	withDecimalSeparator(t, ",")
	if got := truncateDecimals(1.7, 2); got != "1,70" {
		t.Errorf("locale separator: got %q, want 1,70", got)
	}
}

func TestRowValueCheckVsSuggestMapping(t *testing.T) {
	withDecimalSeparator(t, ".")
	check := checkRowFixture("Station 1", "2026-04-27", "11:00", 1.659)
	suggest := suggestRowFixture("Station 2", "2026-04-28", "Tuesday", "10:00", 1.599)

	cases := []struct {
		kind notifyKind
		row  notifyRow
		key  string
		want string
	}{
		{notifyKindCheck, check, "price", "1.659"},
		{notifyKindCheck, check, "price_formatted", "1.65"},
		{notifyKindCheck, check, "date", "2026-04-27"},
		{notifyKindCheck, check, "start_time", "11:00"},
		{notifyKindCheck, check, "weekday", "Monday"},
		{notifyKindCheck, check, "current_price", "1.659"},
		{notifyKindCheck, check, "expected_drop", "0.02"},
		{notifyKindCheck, check, "expected_lower", "false"},
		{notifyKindCheck, check, "distance", "1.2"},
		{notifyKindCheck, check, "history_percentile", "12.5"},
		{notifyKindCheck, check, "lat", "52.500000"},
		{notifyKindCheck, check, "address", "Test Street 1, 10115 Berlin"},
		{notifyKindCheck, check, "fuel_formatted", "Diesel"},
		{notifyKindSuggest, suggest, "price", "1.599"},
		{notifyKindSuggest, suggest, "date", "2026-04-28"},
		{notifyKindSuggest, suggest, "start_time", "10:00"},
		{notifyKindSuggest, suggest, "end_time", "12:00"},
		{notifyKindSuggest, suggest, "weekday", "Tuesday"},
		{notifyKindSuggest, suggest, "predicted_price", "1.599"},
		{notifyKindSuggest, suggest, "sample_count", "7"},
		// Check-only keys resolve empty on suggest rows, like absent JSON keys.
		{notifyKindSuggest, suggest, "current_price", ""},
		{notifyKindSuggest, suggest, "verdict", ""},
		{notifyKindSuggest, suggest, "recommendation", ""},
		{notifyKindSuggest, suggest, "best_future_price", ""},
	}
	for _, tc := range cases {
		if got := rowValue(tc.kind, tc.row, tc.key); got != tc.want {
			t.Errorf("rowValue(%s, %s) = %q, want %q", tc.kind, tc.key, got, tc.want)
		}
	}
}

func TestRowValueCheckDateFallsBackToRecordedAt(t *testing.T) {
	row := checkRowFixture("Station 1", "", "", 1.659)
	row.check.BestFutureDate = ""
	if got := rowValue(notifyKindCheck, row, "date"); got != "2026-04-26T15:00:00Z" {
		t.Fatalf("date = %q, want recorded_at fallback", got)
	}
}

func TestRenderNotifyMessageRowTemplateAndScalars(t *testing.T) {
	withDecimalSeparator(t, ".")
	rows := []notifyRow{
		checkRowFixture("Station 1", "2026-04-27", "11:00", 1.599),
		checkRowFixture("Station 2", "2026-04-27", "11:00", 1.659),
	}
	template := "Cheapest {{cheapest_price_formatted}} EUR ({{count}} stations)\n{{station_name}}: {{price}} EUR"
	got := renderNotifyMessage(template, notifyKindCheck, rows, &rows[0])
	want := "Cheapest 1.59 EUR (2 stations)\nStation 1: 1.599 EUR\nStation 2: 1.659 EUR"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestRenderNotifyMessageMessagePlaceholder(t *testing.T) {
	withDecimalSeparator(t, ".")
	rows := []notifyRow{
		checkRowFixture("Station 1", "2026-04-27", "11:00", 1.599),
	}
	got := renderNotifyMessage("Alert!\n{{message}}\nPrices: {{price}}", notifyKindCheck, rows, &rows[0])
	if !strings.HasPrefix(got, "Alert!\nBuy diesel at Station 1 (1.2 km): 1.599 EUR") {
		t.Fatalf("message placeholder not expanded to default template: %q", got)
	}
	if !strings.HasSuffix(got, "Prices: 1.599") {
		t.Fatalf("remaining row placeholder not expanded to value lines: %q", got)
	}
}

func TestRenderNotifyMessageWithoutPlaceholdersAppendsDefault(t *testing.T) {
	withDecimalSeparator(t, ".")
	rows := []notifyRow{checkRowFixture("Station 1", "2026-04-27", "11:00", 1.599)}
	got := renderNotifyMessage("Cheap gas!", notifyKindCheck, rows, &rows[0])
	if !strings.HasPrefix(got, "Cheap gas!\nBuy diesel at Station 1") {
		t.Fatalf("default message not appended: %q", got)
	}
}

func TestRenderNotifyMessageUnknownPlaceholderStaysLiteral(t *testing.T) {
	rows := []notifyRow{checkRowFixture("Station 1", "2026-04-27", "11:00", 1.599)}
	got := renderNotifyMessage("{{station_name}} {{bogus}}", notifyKindCheck, rows, &rows[0])
	if got != "Station 1 {{bogus}}" {
		t.Fatalf("got %q, want unknown placeholder literal", got)
	}
}

func TestBuildMessageOnchangeCollapsesRepeats(t *testing.T) {
	rows := []notifyRow{
		suggestRowFixture("Station 1", "2026-04-27", "Monday", "10:00", 1.599),
		suggestRowFixture("Station 2", "2026-04-27", "Monday", "10:00", 1.649),
		suggestRowFixture("Station 3", "2026-04-28", "Tuesday", "10:00", 1.699),
	}
	got := buildMessage(notifyKindSuggest, "{{date_onchange}} {{station_name}}", rows)
	want := "2026-04-27 Station 1\n Station 2\n2026-04-28 Station 3"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestBuildMessageOnchangeDayScopedTimeReprints(t *testing.T) {
	rows := []notifyRow{
		suggestRowFixture("Station 1", "2026-04-27", "Monday", "11:00", 1.599),
		// Same start time but a new day: the day-scoped signature must
		// force a reprint even though the time string is unchanged.
		suggestRowFixture("Station 2", "2026-04-28", "Tuesday", "11:00", 1.649),
	}
	got := buildMessage(notifyKindSuggest, "{{start_time_onchange}} {{station_name}}", rows)
	want := "11:00 Station 1\n11:00 Station 2"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestBuildMessageLineSkipRules(t *testing.T) {
	rows := []notifyRow{
		suggestRowFixture("Station 1", "2026-04-27", "Monday", "10:00", 1.599),
		suggestRowFixture("Station 2", "2026-04-27", "Monday", "10:00", 1.649),
	}
	// Line 1 has only an onchange placeholder: skipped on the repeat row.
	// Line 2 has a regular placeholder: always kept.
	// Line 3 has no placeholders: always kept.
	template := "{{date_onchange}}\n{{station_name}} {{price}}\n---"
	got := buildMessage(notifyKindSuggest, template, rows)
	want := "2026-04-27\nStation 1 1.599\n---\nStation 2 1.649\n---"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestBuildMessageOnchangeLineKeptWhenRegularPlaceholderHasValue(t *testing.T) {
	rows := []notifyRow{
		suggestRowFixture("Station 1", "2026-04-27", "Monday", "10:00", 1.599),
		suggestRowFixture("Station 2", "2026-04-27", "Monday", "10:00", 1.649),
	}
	// The onchange value blanks out on row 2, but {{station_name}} on the
	// same line still produces a value, so the line must be kept.
	got := buildMessage(notifyKindSuggest, "{{date_onchange}} {{station_name}}", rows)
	if !strings.Contains(got, " Station 2") {
		t.Fatalf("line with regular placeholder was dropped: %q", got)
	}
}

func TestFormatWeekdayUsesLocaleTables(t *testing.T) {
	withWeekdayNames(t,
		map[string]string{"Monday": "Montag"},
		map[string]string{"Monday": "Mo."},
	)
	row := suggestRowFixture("Station 1", "2026-04-27", "Monday", "10:00", 1.599)
	if got := rowValue(notifyKindSuggest, row, "weekday_formatted"); got != "Montag" {
		t.Fatalf("weekday_formatted = %q, want Montag", got)
	}
	if got := rowValue(notifyKindSuggest, row, "weekday_short_formatted"); got != "Mo." {
		t.Fatalf("weekday_short_formatted = %q, want Mo.", got)
	}
	if got := rowValue(notifyKindSuggest, row, "weekday_short"); got != "Mo" {
		t.Fatalf("weekday_short = %q, want Mo (first two chars, unlocalized)", got)
	}
	// Unknown weekday falls back to the raw value / prefix.
	other := suggestRowFixture("Station 1", "2026-04-27", "Funday", "10:00", 1.599)
	if got := rowValue(notifyKindSuggest, other, "weekday_formatted"); got != "Funday" {
		t.Fatalf("fallback weekday_formatted = %q, want Funday", got)
	}
	if got := rowValue(notifyKindSuggest, other, "weekday_short_formatted"); got != "Fun" {
		t.Fatalf("fallback weekday_short_formatted = %q, want Fun", got)
	}
}

func TestRenderNotifyMessageSuggestDefaultTemplate(t *testing.T) {
	withDecimalSeparator(t, ".")
	rows := []notifyRow{
		suggestRowFixture("Station 1", "2026-04-27", "Monday", "10:00", 1.599),
	}
	got := renderNotifyMessage(defaultSuggestTemplate, notifyKindSuggest, rows, &rows[0])
	want := "2026-04-27 10:00-12:00 diesel at Station 1 (2.5 km): predicted 1.599 EUR, confidence medium"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
