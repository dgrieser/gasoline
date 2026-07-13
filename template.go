package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// This file is a Go port of the gasoline-watch.sh template engine
// (PLACEHOLDERS, row_value, build_message, expand_scalar_placeholders,
// build_notification_command). The notify command renders the same
// {{placeholder}} language — including *_onchange variants, day-scoped
// change detection, line skipping, {{message}}, {{cheapest_*}} and
// {{count}} — except that the result is the Pushover message text itself
// rather than a shell command, so no shell quoting is applied.

type notifyKind string

const (
	notifyKindCheck   notifyKind = "check"
	notifyKindSuggest notifyKind = "suggest"
)

// notifyRow adapts either result-row type to the placeholder resolver.
// Exactly one of the fields is set.
type notifyRow struct {
	check   *priceCheckRow
	suggest *suggestionRow
}

// templatePlaceholders mirrors the PLACEHOLDERS list in gasoline-watch.sh.
var templatePlaceholders = []string{
	"address",
	"best_future_date",
	"best_future_end_time",
	"best_future_price",
	"best_future_price_formatted",
	"best_future_start_time",
	"best_future_weekday",
	"best_future_weekday_formatted",
	"best_future_weekday_short",
	"best_future_weekday_short_formatted",
	"brand",
	"confidence",
	"current_price",
	"current_price_formatted",
	"date",
	"distance",
	"distance_km",
	"end_time",
	"expected_drop",
	"expected_lower",
	"first_seen_at",
	"fuel",
	"fuel_formatted",
	"history_percentile",
	"house_number",
	"lat",
	"lng",
	"place",
	"post_code",
	"predicted_current_price",
	"predicted_current_price_formatted",
	"predicted_price",
	"predicted_price_formatted",
	"price",
	"price_formatted",
	"recommendation",
	"recorded_at",
	"sample_count",
	"start_time",
	"station_id",
	"station_name",
	"street",
	"verdict",
	"weekday",
	"weekday_formatted",
	"weekday_short",
	"weekday_short_formatted",
}

// onchangeDayRef mirrors ONCHANGE_DAY_REF: time-of-day keys whose onchange
// signature also carries the referenced day, so a repeated time still counts
// as changed when the day it refers to changed.
var onchangeDayRef = map[string]string{
	"start_time":             "date",
	"end_time":               "date",
	"best_future_start_time": "best_future_date",
	"best_future_end_time":   "best_future_date",
}

// --- locale caches (counterparts of the watcher's decimal-separator and
// weekday-name lookups, implemented in pure Go for portability; resolved
// lazily and overridable in tests) ---

// localeLanguage returns the lowercase language code ("de", "en", ...) of the
// first non-empty environment variable, honoring POSIX precedence (LC_ALL
// beats the category variable, which beats LANG).
func localeLanguage(categoryVar string) string {
	for _, name := range []string{"LC_ALL", categoryVar, "LANG"} {
		value := strings.TrimSpace(os.Getenv(name))
		if value == "" {
			continue
		}
		// "de_DE.UTF-8" / "de_DE@euro" / "de" -> "de".
		lang := strings.ToLower(value)
		if i := strings.IndexAny(lang, "_.@"); i >= 0 {
			lang = lang[:i]
		}
		return lang
	}
	return ""
}

var localeDecimalSep string

func localeDecimalSeparator() string {
	if localeDecimalSep == "" {
		// Tankerkönig is a German service: the locales of practical
		// interest are German (comma) and English (dot).
		localeDecimalSep = "."
		if localeLanguage("LC_NUMERIC") == "de" {
			localeDecimalSep = ","
		}
	}
	return localeDecimalSep
}

var englishWeekdays = []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}
var germanWeekdaysLong = []string{"Sonntag", "Montag", "Dienstag", "Mittwoch", "Donnerstag", "Freitag", "Samstag"}
var germanWeekdaysShort = []string{"So", "Mo", "Di", "Mi", "Do", "Fr", "Sa"}

var (
	localeWeekdayLong  map[string]string
	localeWeekdayShort map[string]string
)

func localeWeekdays() (map[string]string, map[string]string) {
	if localeWeekdayLong == nil {
		long := make(map[string]string, 7)
		short := make(map[string]string, 7)
		german := localeLanguage("LC_TIME") == "de"
		for i, en := range englishWeekdays {
			long[en] = en
			short[en] = en[:3]
			if german {
				long[en] = germanWeekdaysLong[i]
				short[en] = germanWeekdaysShort[i]
			}
		}
		localeWeekdayLong = long
		localeWeekdayShort = short
	}
	return localeWeekdayLong, localeWeekdayShort
}

// formatNumber mirrors number_value: fixed decimals, C locale (dot).
func formatNumber(v float64, decimals int) string {
	return strconv.FormatFloat(v, 'f', decimals, 64)
}

// truncateDecimals mirrors truncate_number_value: string truncation without
// rounding (1.789 -> 1.78, 1.7 -> 1.70) using the locale decimal separator.
func truncateDecimals(v float64, decimals int) string {
	value := strconv.FormatFloat(v, 'f', -1, 64)
	intPart := value
	fracPart := ""
	if dot := strings.IndexByte(value, '.'); dot >= 0 {
		intPart = value[:dot]
		fracPart = value[dot+1:]
	}
	if len(fracPart) > decimals {
		fracPart = fracPart[:decimals]
	}
	for len(fracPart) < decimals {
		fracPart += "0"
	}
	if decimals == 0 {
		return intPart
	}
	return intPart + localeDecimalSeparator() + fracPart
}

func capitalizeFirst(s string) string {
	if s == "" {
		return s
	}
	r, size := utf8.DecodeRuneInString(s)
	return string(unicode.ToUpper(r)) + s[size:]
}

func formatWeekday(mode, wd string) string {
	if wd == "" {
		return ""
	}
	long, short := localeWeekdays()
	switch mode {
	case "short":
		if len(wd) > 2 {
			return wd[:2]
		}
		return wd
	case "long_formatted":
		if v, ok := long[wd]; ok {
			return v
		}
		return wd
	case "short_formatted":
		if v, ok := short[wd]; ok {
			return v
		}
		if len(wd) > 3 {
			return wd[:3]
		}
		return wd
	}
	return ""
}

func (r notifyRow) station() suggestionStationRow {
	if r.check != nil {
		return r.check.Station
	}
	if r.suggest != nil {
		return r.suggest.Station
	}
	return suggestionStationRow{}
}

// weekdaySource mirrors _weekday_source.
func weekdaySource(kind notifyKind, r notifyRow, field string) string {
	if field == "best_future" || kind == notifyKindCheck {
		if r.check != nil {
			return r.check.BestFutureWeekday
		}
		return ""
	}
	if r.suggest != nil {
		return r.suggest.Weekday
	}
	return ""
}

// rowValue mirrors row_value: it resolves one placeholder key for one row.
// Keys that do not apply to the row's kind resolve to "" just like the jq
// lookups in the watcher (absent JSON key -> empty string).
func rowValue(kind notifyKind, r notifyRow, key string) string {
	st := r.station()
	c := r.check
	s := r.suggest
	switch key {
	case "address":
		return st.Address
	case "best_future_date":
		if c != nil {
			return c.BestFutureDate
		}
		return ""
	case "best_future_end_time":
		if c != nil {
			return c.BestFutureEndTime
		}
		return ""
	case "best_future_price":
		if c != nil && c.BestFuturePrice != 0 {
			return formatNumber(c.BestFuturePrice, 3)
		}
		return ""
	case "best_future_price_formatted":
		if c != nil && c.BestFuturePrice != 0 {
			return truncateDecimals(c.BestFuturePrice, 2)
		}
		return ""
	case "best_future_start_time":
		if c != nil {
			return c.BestFutureStartTime
		}
		return ""
	case "best_future_weekday":
		if c != nil {
			return c.BestFutureWeekday
		}
		return ""
	case "best_future_weekday_formatted":
		return formatWeekday("long_formatted", weekdaySource(kind, r, "best_future"))
	case "best_future_weekday_short":
		return formatWeekday("short", weekdaySource(kind, r, "best_future"))
	case "best_future_weekday_short_formatted":
		return formatWeekday("short_formatted", weekdaySource(kind, r, "best_future"))
	case "brand":
		return st.Brand
	case "confidence":
		if c != nil {
			return c.Confidence
		}
		if s != nil {
			return s.Confidence
		}
		return ""
	case "current_price":
		if c != nil {
			return formatNumber(c.CurrentPrice, 3)
		}
		return ""
	case "current_price_formatted":
		if c != nil {
			return truncateDecimals(c.CurrentPrice, 2)
		}
		return ""
	case "date":
		if kind == notifyKindCheck {
			if c == nil {
				return ""
			}
			if c.BestFutureDate != "" {
				return c.BestFutureDate
			}
			return c.RecordedAt
		}
		if s != nil {
			return s.Date
		}
		return ""
	case "distance", "distance_km":
		if c != nil {
			return formatNumber(c.DistanceKM, 1)
		}
		if s != nil {
			return formatNumber(s.DistanceKM, 1)
		}
		return ""
	case "end_time":
		if kind == notifyKindCheck {
			if c != nil {
				return c.BestFutureEndTime
			}
			return ""
		}
		if s != nil {
			return s.EndTime
		}
		return ""
	case "expected_drop":
		if c != nil && c.ExpectedDrop != 0 {
			return formatNumber(c.ExpectedDrop, 2)
		}
		return ""
	case "expected_lower":
		if c != nil {
			return strconv.FormatBool(c.ExpectedLower)
		}
		return ""
	case "first_seen_at":
		return st.FirstSeenAt
	case "fuel":
		if c != nil {
			return c.Fuel
		}
		if s != nil {
			return s.Fuel
		}
		return ""
	case "fuel_formatted":
		return capitalizeFirst(rowValue(kind, r, "fuel"))
	case "history_percentile":
		if c != nil {
			return formatNumber(c.HistoryPercentile, 1)
		}
		return ""
	case "house_number":
		return st.HouseNumber
	case "lat":
		return formatNumber(st.Lat, 6)
	case "lng":
		return formatNumber(st.Lng, 6)
	case "place":
		return st.Place
	case "post_code":
		return strconv.Itoa(st.PostCode)
	case "predicted_current_price":
		if c != nil {
			return formatNumber(c.PredictedCurrentPrice, 3)
		}
		return ""
	case "predicted_current_price_formatted":
		if c != nil {
			return truncateDecimals(c.PredictedCurrentPrice, 2)
		}
		return ""
	case "predicted_price":
		if s != nil {
			return formatNumber(s.PredictedPrice, 3)
		}
		if c != nil {
			return formatNumber(c.PredictedCurrentPrice, 3)
		}
		return ""
	case "predicted_price_formatted":
		if s != nil {
			return truncateDecimals(s.PredictedPrice, 2)
		}
		if c != nil {
			return truncateDecimals(c.PredictedCurrentPrice, 2)
		}
		return ""
	case "price":
		if kind == notifyKindCheck {
			if c != nil {
				return formatNumber(c.CurrentPrice, 3)
			}
			return ""
		}
		if s != nil {
			return formatNumber(s.PredictedPrice, 3)
		}
		return ""
	case "price_formatted":
		if kind == notifyKindCheck {
			if c != nil {
				return truncateDecimals(c.CurrentPrice, 2)
			}
			return ""
		}
		if s != nil {
			return truncateDecimals(s.PredictedPrice, 2)
		}
		return ""
	case "recommendation":
		if c != nil {
			return c.Recommendation
		}
		return ""
	case "recorded_at":
		if c != nil {
			return c.RecordedAt
		}
		return ""
	case "sample_count":
		if c != nil {
			return strconv.Itoa(c.SampleCount)
		}
		if s != nil {
			return strconv.Itoa(s.SampleCount)
		}
		return ""
	case "start_time":
		if kind == notifyKindCheck {
			if c != nil {
				return c.BestFutureStartTime
			}
			return ""
		}
		if s != nil {
			return s.StartTime
		}
		return ""
	case "station_id":
		if c != nil {
			return c.StationID
		}
		if s != nil {
			return s.StationID
		}
		return ""
	case "station_name":
		if c != nil {
			return c.StationName
		}
		if s != nil {
			return s.StationName
		}
		return ""
	case "street":
		return st.Street
	case "verdict":
		if c != nil {
			return c.Verdict
		}
		return ""
	case "weekday":
		if kind == notifyKindCheck {
			if c != nil {
				return c.BestFutureWeekday
			}
			return ""
		}
		if s != nil {
			return s.Weekday
		}
		return ""
	case "weekday_formatted":
		return formatWeekday("long_formatted", weekdaySource(kind, r, "auto"))
	case "weekday_short":
		return formatWeekday("short", weekdaySource(kind, r, "auto"))
	case "weekday_short_formatted":
		return formatWeekday("short_formatted", weekdaySource(kind, r, "auto"))
	}
	return ""
}

func containsRowPlaceholder(template string) bool {
	for _, key := range templatePlaceholders {
		if strings.Contains(template, "{{"+key+"}}") || strings.Contains(template, "{{"+key+"_onchange}}") {
			return true
		}
	}
	return false
}

// buildMessage mirrors build_message: it applies the row template to every
// row (line by line, with onchange collapse and line skipping) and joins the
// surviving row blocks with newlines.
func buildMessage(kind notifyKind, rowTemplate string, rows []notifyRow) string {
	// Pre-scan the template once so the per-line loops below only touch
	// placeholders that actually occur in it.
	var onchangeKeys []string
	var activePlaceholders []string
	for _, key := range templatePlaceholders {
		if strings.Contains(rowTemplate, "{{"+key+"_onchange}}") {
			onchangeKeys = append(onchangeKeys, key)
		}
		if strings.Contains(rowTemplate, "{{"+key+"}}") {
			activePlaceholders = append(activePlaceholders, key)
		}
	}

	lines := strings.Split(rowTemplate, "\n")
	prevSig := map[string]string{}
	havePrev := false
	var message strings.Builder
	firstEmitted := false

	for _, row := range rows {
		// Resolve onchange values once per row so change tracking stays
		// correct regardless of which physical lines end up skipped.
		onchangeEffective := map[string]string{}
		dayCache := map[string]string{}
		for _, key := range onchangeKeys {
			value := rowValue(kind, row, key)
			sig := value
			if dayref, ok := onchangeDayRef[key]; ok {
				dayval, cached := dayCache[dayref]
				if !cached {
					dayval = rowValue(kind, row, dayref)
					dayCache[dayref] = dayval
				}
				sig = dayval + "\x1f" + value
			}
			if havePrev && prevSig[key] == sig {
				onchangeEffective[key] = ""
			} else {
				onchangeEffective[key] = value
			}
			prevSig[key] = sig
		}

		var rowBlock strings.Builder
		rowHasLine := false
		for _, line := range lines {
			rendered := line
			lineHasOnchange := false
			lineHasValue := false

			for _, key := range onchangeKeys {
				token := "{{" + key + "_onchange}}"
				if strings.Contains(line, token) {
					lineHasOnchange = true
					if onchangeEffective[key] != "" {
						lineHasValue = true
					}
					rendered = strings.ReplaceAll(rendered, token, onchangeEffective[key])
				}
			}

			for _, key := range activePlaceholders {
				token := "{{" + key + "}}"
				if strings.Contains(line, token) {
					value := rowValue(kind, row, key)
					if value != "" {
						lineHasValue = true
					}
					rendered = strings.ReplaceAll(rendered, token, value)
				}
			}

			if lineHasOnchange && !lineHasValue {
				continue
			}
			if rowHasLine {
				rowBlock.WriteString("\n")
			}
			rowBlock.WriteString(rendered)
			rowHasLine = true
		}

		if rowHasLine {
			if firstEmitted {
				message.WriteString("\n")
			}
			message.WriteString(rowBlock.String())
			firstEmitted = true
		}
		if len(onchangeKeys) > 0 {
			havePrev = true
		}
	}
	return message.String()
}

// buildValueLines mirrors build_value_lines: one resolved value per row,
// newline-joined.
func buildValueLines(kind notifyKind, key string, rows []notifyRow) string {
	values := make([]string, 0, len(rows))
	for _, row := range rows {
		values = append(values, rowValue(kind, row, key))
	}
	return strings.Join(values, "\n")
}

// expandScalars mirrors expand_scalar_placeholders (without shell quoting).
func expandScalars(template string, kind notifyKind, cheapest *notifyRow, rowCount int) string {
	result := template
	if strings.Contains(result, "{{count}}") {
		result = strings.ReplaceAll(result, "{{count}}", strconv.Itoa(rowCount))
	}
	if strings.Contains(result, "{{cheapest_") {
		for _, key := range templatePlaceholders {
			token := "{{cheapest_" + key + "}}"
			if strings.Contains(result, token) {
				value := ""
				if cheapest != nil {
					value = rowValue(kind, *cheapest, key)
				}
				result = strings.ReplaceAll(result, token, value)
			}
		}
	}
	return result
}

func defaultRowTemplate(kind notifyKind) string {
	if kind == notifyKindCheck {
		return defaultCheckTemplate
	}
	return defaultSuggestTemplate
}

// renderNotifyTitle renders a notification title template. A title is a
// single line, so row placeholders resolve against the cheapest row instead
// of expanding once per row; scalar placeholders ({{count}}, {{cheapest_*}})
// work exactly like in message templates. Newlines collapse to spaces. An
// empty result means "no title" and callers fall back to the user's
// configured application name.
func renderNotifyTitle(template string, kind notifyKind, cheapest *notifyRow, rowCount int) string {
	result := expandScalars(template, kind, cheapest, rowCount)
	for _, key := range templatePlaceholders {
		value := ""
		if cheapest != nil {
			value = rowValue(kind, *cheapest, key)
		}
		// There is no per-row change tracking in a single-line title, so
		// *_onchange variants resolve like their plain counterparts.
		for _, token := range []string{"{{" + key + "}}", "{{" + key + "_onchange}}"} {
			if strings.Contains(result, token) {
				result = strings.ReplaceAll(result, token, value)
			}
		}
	}
	return strings.TrimSpace(strings.ReplaceAll(result, "\n", " "))
}

// renderNotifyMessage mirrors build_notification_command, minus the shell:
// the template renders straight to the notification text. The three layering
// branches are preserved: a {{message}} template gets the default multi-row
// message plus newline-joined per-row value lists; a template with row
// placeholders keeps its literal prefix and renders the rest per row; a
// template without placeholders is followed by the default message.
func renderNotifyMessage(template string, kind notifyKind, rows []notifyRow, cheapest *notifyRow) string {
	result := expandScalars(template, kind, cheapest, len(rows))

	if strings.Contains(result, "{{message}}") {
		message := buildMessage(kind, defaultRowTemplate(kind), rows)
		result = strings.ReplaceAll(result, "{{message}}", message)
		for _, key := range templatePlaceholders {
			token := "{{" + key + "}}"
			if strings.Contains(result, token) {
				result = strings.ReplaceAll(result, token, buildValueLines(kind, key, rows))
			}
		}
		return result
	}

	if containsRowPlaceholder(result) {
		idx := strings.Index(result, "{{")
		prefix := result[:idx]
		return prefix + buildMessage(kind, result[idx:], rows)
	}

	message := buildMessage(kind, defaultRowTemplate(kind), rows)
	if message == "" {
		return result
	}
	return fmt.Sprintf("%s\n%s", result, message)
}
