package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func TestParseHHMM(t *testing.T) {
	if minutes, err := parseHHMM("07:30"); err != nil || minutes != 450 {
		t.Fatalf("parseHHMM(07:30) = %d, %v", minutes, err)
	}
	for _, invalid := range []string{"7:30", "24:00", "12:60", "12", "", "ab:cd"} {
		if _, err := parseHHMM(invalid); err == nil {
			t.Errorf("parseHHMM(%q) succeeded, want error", invalid)
		}
	}
}

func TestParseWindowsAndDaysAndTimes(t *testing.T) {
	windows, err := parseWindows("07:00-12:00, 14:00-21:00")
	if err != nil || len(windows) != 2 || windows[1].From != 840 {
		t.Fatalf("parseWindows = %+v, %v", windows, err)
	}
	if _, err := parseWindows("07:00"); err == nil {
		t.Fatal("parseWindows without dash succeeded")
	}
	days, err := parseDaySet("mon, TUE,sun")
	if err != nil || len(days) != 3 || !days[time.Tuesday] {
		t.Fatalf("parseDaySet = %+v, %v", days, err)
	}
	if _, err := parseDaySet("mon,funday"); err == nil {
		t.Fatal("parseDaySet with invalid day succeeded")
	}
	times, err := parseTimesList("13:00,08:00")
	if err != nil || len(times) != 2 || times[0] != "08:00" {
		t.Fatalf("parseTimesList = %v, %v (want sorted)", times, err)
	}
}

func TestSubscriptionValidity(t *testing.T) {
	located := notifyUser{NotifyLat: 52.5, NotifyLng: 13.4, NotifyRadiusKM: 10}
	if !located.subscription().valid() {
		t.Fatal("a user with a radius has a subscription")
	}
	if (notifyUser{NotifyLat: 52.5, NotifyLng: 13.4}).subscription().valid() {
		t.Fatal("a zero radius is not a subscription")
	}
	if (notifyUser{NotifyRadiusKM: -1}).subscription().valid() {
		t.Fatal("a negative radius is not a subscription")
	}
}

func TestDistinctSubscriptionsDeduplicatesAreas(t *testing.T) {
	users := []notifyUser{
		{NotifyLat: 52.5, NotifyLng: 13.4, NotifyRadiusKM: 10},
		{NotifyLat: 52.5, NotifyLng: 13.4, NotifyRadiusKM: 10}, // same area
		{NotifyLat: 52.5, NotifyLng: 13.4, NotifyRadiusKM: 25}, // same point, wider
		{NotifyLat: 53.5, NotifyLng: 9.9, NotifyRadiusKM: 10},
		{NotifyLat: 53.5, NotifyLng: 9.9}, // no radius: skipped
	}
	subs := distinctSubscriptions(users)
	if len(subs) != 3 {
		t.Fatalf("distinct subscriptions = %+v, want 3", subs)
	}
}

func TestScheduleActive(t *testing.T) {
	// 2026-04-27 is a Monday.
	monday10 := time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC)
	monday23 := time.Date(2026, 4, 27, 23, 0, 0, 0, time.UTC)

	if active, err := scheduleActive(monday10, "mon,tue", "07:00-21:00"); err != nil || !active {
		t.Fatalf("in-window Monday = %v, %v, want active", active, err)
	}
	if active, _ := scheduleActive(monday10, "sat,sun", "07:00-21:00"); active {
		t.Fatal("wrong weekday must block")
	}
	if active, _ := scheduleActive(monday23, "mon", "07:00-21:00"); active {
		t.Fatal("outside window must block")
	}
	if active, _ := scheduleActive(monday23, "mon", "22:00-06:00"); !active {
		t.Fatal("overnight wrap 22:00-06:00 must include 23:00")
	}
	early := time.Date(2026, 4, 27, 5, 0, 0, 0, time.UTC)
	if active, _ := scheduleActive(early, "mon", "22:00-06:00"); !active {
		t.Fatal("overnight wrap 22:00-06:00 must include 05:00")
	}
	if active, _ := scheduleActive(monday10, "mon", "07:00-09:00,09:30-11:00"); !active {
		t.Fatal("multi-range window must include 10:00")
	}
	// Empty specs fall back to the built-in every-day 07:00-21:00 default.
	if active, err := scheduleActive(monday10, "", ""); err != nil || !active {
		t.Fatalf("default schedule = %v, %v, want active at Monday 10:00", active, err)
	}
	if active, _ := scheduleActive(monday23, "", ""); active {
		t.Fatal("default schedule must block at 23:00")
	}
}

func TestDueSuggestSlot(t *testing.T) {
	slots := []string{"08:00", "13:00"}
	at := func(hhmm string) time.Time {
		return time.Date(2026, 4, 27, 0, 0, 0, 0, time.UTC).
			Add(time.Duration(mustParseHHMM(t, hhmm)) * time.Minute)
	}

	if _, due := dueSuggestSlot(at("07:00"), slots, ""); due {
		t.Fatal("before the first slot nothing is due")
	}
	if slot, due := dueSuggestSlot(at("08:05"), slots, ""); !due || slot != "08:00" {
		t.Fatalf("slot = %q due=%v, want 08:00", slot, due)
	}
	if _, due := dueSuggestSlot(at("09:00"), slots, "2026-04-27T08:00"); due {
		t.Fatal("fired slot must not re-fire")
	}
	if slot, due := dueSuggestSlot(at("13:30"), slots, "2026-04-27T08:00"); !due || slot != "13:00" {
		t.Fatalf("slot = %q due=%v, want 13:00", slot, due)
	}
	// Cron gap: both slots missed collapse into the latest one.
	if slot, due := dueSuggestSlot(at("15:00"), slots, ""); !due || slot != "13:00" {
		t.Fatalf("slot = %q due=%v, want single collapse to 13:00", slot, due)
	}
	// Next-day rollover: yesterday's marker does not block today.
	if slot, due := dueSuggestSlot(at("08:05"), slots, "2026-04-26T13:00"); !due || slot != "08:00" {
		t.Fatalf("slot = %q due=%v, want 08:00 after rollover", slot, due)
	}
}

func mustParseHHMM(t *testing.T, s string) int {
	t.Helper()
	minutes, err := parseHHMM(s)
	if err != nil {
		t.Fatalf("parseHHMM(%q): %v", s, err)
	}
	return minutes
}

// --- notifyOnce end-to-end (SQLite) ---

// notifyFixtureNow is inside the default 07:00-21:00 window; the matching
// weekday is Sunday (2026-04-26).
var notifyFixtureNow = time.Date(2026, 4, 26, 15, 30, 0, 0, time.UTC)

// notifyFixtureLat/Lng is where seedNotifyFixture puts both the city and its
// station, so a default subscription around it covers that station exactly.
const (
	notifyFixtureLat = 52.517389
	notifyFixtureLng = 13.395131
)

// notifyFixtureBaseline is the baseline key for a user on the default fixture
// area. Built through the production helper because these assertions are about
// whether a baseline advanced, not about how the key is spelled.
func notifyFixtureBaseline(userID int64, fuel string) string {
	return subscription{Lat: notifyFixtureLat, Lng: notifyFixtureLng, RadiusKM: 5}.baselineKey(userID, fuel)
}

// seedNotifyFixture builds a database where checkGas returns a buy
// recommendation and suggestGas returns forecast rows with medium/high
// confidence: four weeks of hourly history plus a fresh low snapshot. The
// history window setting is widened to cover all of it (confidence requires
// >= 3 same-weekday samples per hour bucket).
func seedNotifyFixture(t *testing.T, db *sql.DB) {
	t.Helper()
	insertSuggestCity(t, db, cachedCity{
		QueryName:   "Berlin",
		Name:        "Berlin",
		DisplayName: "Berlin",
		Lat:         52.517389,
		Lng:         13.395131,
	})
	insertSuggestStation(t, db, "station-1", "Station 1", 52.517389, 13.395131)
	for daysAgo := 28; daysAgo >= 1; daysAgo-- {
		dayStart := notifyFixtureNow.Truncate(24*time.Hour).AddDate(0, 0, -daysAgo)
		for hour := 0; hour < 24; hour++ {
			insertSuggestSnapshot(t, db, "station-1", "Berlin", dayStart.Add(time.Duration(hour)*time.Hour), 2.100, true)
		}
	}
	insertSuggestSnapshot(t, db, "station-1", "Berlin", notifyFixtureNow.Add(-10*time.Minute), 2.000, true)

	ctx := context.Background()
	if _, err := db.ExecContext(ctx,
		`INSERT INTO update_targets (city, radius_km, created_at) VALUES (?, ?, ?)`,
		"Berlin", 5.0, "2026-04-01T00:00:00Z"); err != nil {
		t.Fatalf("insert target: %v", err)
	}
}

// seedNotifySecondCity adds a second update target (Hamburg) with its own
// station, four weeks of hourly history, and a fresh low snapshot, so
// notifyOnce produces check and suggest rows for two cities.
func seedNotifySecondCity(t *testing.T, db *sql.DB) {
	t.Helper()
	insertSuggestCity(t, db, cachedCity{
		QueryName:   "Hamburg",
		Name:        "Hamburg",
		DisplayName: "Hamburg",
		Lat:         53.550556,
		Lng:         9.993333,
	})
	insertSuggestStation(t, db, "station-2", "Station 2", 53.550556, 9.993333)
	for daysAgo := 28; daysAgo >= 1; daysAgo-- {
		dayStart := notifyFixtureNow.Truncate(24*time.Hour).AddDate(0, 0, -daysAgo)
		for hour := 0; hour < 24; hour++ {
			insertSuggestSnapshot(t, db, "station-2", "Hamburg", dayStart.Add(time.Duration(hour)*time.Hour), 2.100, true)
		}
	}
	insertSuggestSnapshot(t, db, "station-2", "Hamburg", notifyFixtureNow.Add(-10*time.Minute), 1.900, true)

	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO update_targets (city, radius_km, created_at) VALUES (?, ?, ?)`,
		"Hamburg", 5.0, "2026-04-01T00:00:00Z"); err != nil {
		t.Fatalf("insert target: %v", err)
	}
}

type notifyUserFixture struct {
	Email        string
	UserKey      string
	Token        string
	Days         string
	Windows      string
	SuggestTimes string
	CheckEnabled bool
	// SuggestDisabled turns off suggestion notifications for this user. The
	// zero value keeps suggestions enabled, matching the schema default and
	// the pre-toggle behavior, so existing fixtures need no changes.
	SuggestDisabled bool
	LastSuggest     string
	Status          string
	Method          string
	Fuel            string // notify_fuel; empty defaults to diesel
	// The subscription area. The zero value is the Berlin fixture point with a
	// 5 km radius, which covers seedNotifyFixture's station, so existing
	// fixtures need no changes. NoLocation leaves it unset instead.
	City       string
	Lat        float64
	Lng        float64
	RadiusKM   float64
	NoLocation bool
}

func seedNotifyUser(t *testing.T, db *sql.DB, u notifyUserFixture) {
	t.Helper()
	if u.Status == "" {
		u.Status = "approved"
	}
	if u.Method == "" {
		u.Method = "pushover"
	}
	if u.Fuel == "" {
		u.Fuel = "diesel"
	}
	if !u.NoLocation && u.RadiusKM == 0 {
		u.City, u.Lat, u.Lng, u.RadiusKM = "Berlin", notifyFixtureLat, notifyFixtureLng, 5
	}
	ctx := context.Background()
	_, err := db.ExecContext(ctx, `
		INSERT INTO users (
			email, password_hash, is_admin, status, created_at, approved_at,
			notify_method, pushover_app_name, pushover_user_key, pushover_token,
			notify_days, notify_windows, notify_suggest_times, notify_check_enabled,
			notify_suggest_enabled, notify_last_suggest, notify_fuel,
			notify_city, notify_lat, notify_lng, notify_radius_km
		) VALUES (?, ?, 0, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		u.Email, "x", u.Status, "2026-04-01T00:00:00Z", "2026-04-01T00:00:00Z",
		u.Method, "gasoline", u.UserKey, u.Token,
		u.Days, u.Windows, u.SuggestTimes, boolToInt(u.CheckEnabled),
		boolToInt(!u.SuggestDisabled), u.LastSuggest, u.Fuel,
		u.City, u.Lat, u.Lng, u.RadiusKM)
	if err != nil {
		t.Fatalf("insert user %s: %v", u.Email, err)
	}
}

type capturedPush struct {
	Token   string
	User    string
	Title   string
	Message string
	URL     string
}

// stubPushover intercepts Pushover API calls; requests to other hosts fail.
func stubPushover(t *testing.T, fail func(user string) bool) *[]capturedPush {
	t.Helper()
	var pushes []capturedPush
	restore := stubDefaultTransport(t, func(req *http.Request) (*http.Response, error) {
		if !strings.HasPrefix(req.URL.String(), pushoverMessagesURL) {
			return nil, fmt.Errorf("unexpected request URL: %s", req.URL.String())
		}
		body, _ := io.ReadAll(req.Body)
		form, err := url.ParseQuery(string(body))
		if err != nil {
			return nil, err
		}
		if fail != nil && fail(form.Get("user")) {
			return jsonResponse(http.StatusBadRequest, `{"status":0,"errors":["user identifier is invalid"]}`), nil
		}
		pushes = append(pushes, capturedPush{
			Token:   form.Get("token"),
			User:    form.Get("user"),
			Title:   form.Get("title"),
			Message: form.Get("message"),
			URL:     form.Get("url"),
		})
		return jsonResponse(http.StatusOK, `{"status":1,"request":"r"}`), nil
	})
	t.Cleanup(restore)
	return &pushes
}

func runNotifyOnce(t *testing.T, db *sql.DB, dryRun bool) notifyResult {
	t.Helper()
	result, err := notifyOnce(context.Background(), db, dialectSQLite, notifyOptions{
		Now:      notifyFixtureNow,
		Location: time.UTC,
		DryRun:   dryRun,
	})
	if err != nil {
		t.Fatalf("notifyOnce: %v", err)
	}
	return result
}

func TestNotifyOnceSendsCheckAndSuggestPerSchedule(t *testing.T) {
	withDecimalSeparator(t, ".")
	db := openTestDB(t)
	seedNotifyFixture(t, db)
	seedNotifyUser(t, db, notifyUserFixture{
		Email: "in@example.com", UserKey: "user-in", Token: "token-in",
		SuggestTimes: "08:00,13:00", CheckEnabled: true,
	})
	seedNotifyUser(t, db, notifyUserFixture{
		Email: "out@example.com", UserKey: "user-out", Token: "token-out",
		Windows: "02:00-03:00", SuggestTimes: "08:00", CheckEnabled: true,
	})
	seedNotifyUser(t, db, notifyUserFixture{
		Email: "wrongday@example.com", UserKey: "user-day", Token: "token-day",
		Days: "mon,tue", CheckEnabled: true,
	})
	seedNotifyUser(t, db, notifyUserFixture{
		Email: "pending@example.com", UserKey: "user-p", Token: "token-p",
		Status: "pending", CheckEnabled: true,
	})
	seedNotifyUser(t, db, notifyUserFixture{
		Email: "nokey@example.com", UserKey: "", Token: "token-nk",
		CheckEnabled: true,
	})

	pushes := stubPushover(t, nil)
	result := runNotifyOnce(t, db, false)

	if len(result.Failed) != 0 {
		t.Fatalf("failed sends: %+v", result.Failed)
	}
	// Only in@example.com is schedule-active with credentials: one check
	// (fresh low price) and one suggest (13:00 slot due at 15:30).
	var gotKinds []string
	for _, rec := range result.Sent {
		if rec.Email != "in@example.com" {
			t.Fatalf("unexpected recipient: %+v", rec)
		}
		gotKinds = append(gotKinds, rec.Kind)
	}
	if len(gotKinds) != 2 || !containsString(gotKinds, "check") || !containsString(gotKinds, "suggest") {
		t.Fatalf("sent kinds = %v, want check+suggest", gotKinds)
	}
	if len(*pushes) != 2 {
		t.Fatalf("pushover calls = %d, want 2", len(*pushes))
	}
	for _, push := range *pushes {
		if push.User != "user-in" || push.Token != "token-in" || push.Title != "gasoline" {
			t.Fatalf("push credentials = %+v", push)
		}
		if push.Message == "" {
			t.Fatal("empty push message")
		}
		if push.URL != "" {
			t.Fatalf("push url = %q, want none without a base URL", push.URL)
		}
	}

	var lastSuggest string
	if err := db.QueryRowContext(context.Background(),
		`SELECT notify_last_suggest FROM users WHERE email = 'in@example.com'`).Scan(&lastSuggest); err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if lastSuggest != "2026-04-26T13:00" {
		t.Fatalf("notify_last_suggest = %q, want 2026-04-26T13:00", lastSuggest)
	}
}

func TestNotifyOnceRespectsPerKindToggles(t *testing.T) {
	withDecimalSeparator(t, ".")
	db := openTestDB(t)
	seedNotifyFixture(t, db)
	// Four users cover every combination of the two per-user toggles. They
	// share the same schedule and a due 13:00 suggestion slot, so the only
	// thing that differs is which notification kinds each opted into.
	seedNotifyUser(t, db, notifyUserFixture{
		Email: "both@example.com", UserKey: "user-both", Token: "token-both",
		SuggestTimes: "08:00,13:00", CheckEnabled: true, // suggest enabled by default
	})
	seedNotifyUser(t, db, notifyUserFixture{
		Email: "suggestonly@example.com", UserKey: "user-sug", Token: "token-sug",
		SuggestTimes: "08:00,13:00", CheckEnabled: false,
	})
	seedNotifyUser(t, db, notifyUserFixture{
		Email: "checkonly@example.com", UserKey: "user-chk", Token: "token-chk",
		SuggestTimes: "08:00,13:00", CheckEnabled: true, SuggestDisabled: true,
	})
	seedNotifyUser(t, db, notifyUserFixture{
		Email: "none@example.com", UserKey: "user-none", Token: "token-none",
		SuggestTimes: "08:00,13:00", CheckEnabled: false, SuggestDisabled: true,
	})

	stubPushover(t, nil)
	result := runNotifyOnce(t, db, false)
	if len(result.Failed) != 0 {
		t.Fatalf("failed sends: %+v", result.Failed)
	}

	gotKinds := map[string][]string{}
	for _, rec := range result.Sent {
		gotKinds[rec.Email] = append(gotKinds[rec.Email], rec.Kind)
	}
	assertKinds := func(email string, want ...string) {
		t.Helper()
		got := gotKinds[email]
		if len(got) != len(want) {
			t.Fatalf("%s sent kinds = %v, want %v", email, got, want)
		}
		for _, w := range want {
			if !containsString(got, w) {
				t.Fatalf("%s sent kinds = %v, want %v", email, got, want)
			}
		}
	}
	assertKinds("both@example.com", "check", "suggest")
	assertKinds("suggestonly@example.com", "suggest")
	assertKinds("checkonly@example.com", "check")
	assertKinds("none@example.com") // nothing at all

	// A user who disabled suggestions must not have their slot marker
	// advanced, so re-enabling later resumes cleanly.
	var checkOnlyMarker string
	if err := db.QueryRowContext(context.Background(),
		`SELECT notify_last_suggest FROM users WHERE email = 'checkonly@example.com'`).Scan(&checkOnlyMarker); err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if checkOnlyMarker != "" {
		t.Fatalf("checkonly notify_last_suggest = %q, want empty (suggestions disabled)", checkOnlyMarker)
	}
}

func TestNotifyOnceFiltersByUserSubscriptionArea(t *testing.T) {
	withDecimalSeparator(t, ".")
	db := openTestDB(t)
	seedNotifyFixture(t, db)
	seedNotifySecondCity(t, db)
	// Berlin and Hamburg are ~255 km apart, so a tight radius around either one
	// sees only its own station, and a wide one sees both.
	seedNotifyUser(t, db, notifyUserFixture{
		Email: "berlin@example.com", UserKey: "user-berlin", Token: "token-b",
		SuggestTimes: "08:00,13:00", CheckEnabled: true,
		City: "Berlin", Lat: notifyFixtureLat, Lng: notifyFixtureLng, RadiusKM: 10,
	})
	seedNotifyUser(t, db, notifyUserFixture{
		Email: "hamburg@example.com", UserKey: "user-hamburg", Token: "token-h",
		SuggestTimes: "08:00,13:00", CheckEnabled: true,
		City: "Hamburg", Lat: 53.550556, Lng: 9.993333, RadiusKM: 10,
	})
	seedNotifyUser(t, db, notifyUserFixture{
		Email: "all@example.com", UserKey: "user-all", Token: "token-a",
		SuggestTimes: "08:00,13:00", CheckEnabled: true,
		City: "Berlin", Lat: notifyFixtureLat, Lng: notifyFixtureLng, RadiusKM: 400,
	})

	pushes := stubPushover(t, nil)
	result := runNotifyOnce(t, db, false)
	if len(result.Failed) != 0 {
		t.Fatalf("failed sends: %+v", result.Failed)
	}

	// Check and suggest messages are told apart by the default templates: a
	// check row starts with "Buy", a suggestion with its date.
	kinds := map[string]map[string]string{}
	for _, push := range *pushes {
		kind := "suggest"
		if strings.HasPrefix(push.Message, "Buy ") {
			kind = "check"
		}
		if kinds[push.User] == nil {
			kinds[push.User] = map[string]string{}
		}
		kinds[push.User][kind] = push.Message
	}
	assertOnlyStation := func(user, wantStation, filteredStation string) {
		t.Helper()
		if len(kinds[user]) != 2 {
			t.Fatalf("%s got %v, want a check and a suggest", user, kinds[user])
		}
		for kind, msg := range kinds[user] {
			if !strings.Contains(msg, wantStation) {
				t.Fatalf("%s %s message %q misses %s", user, kind, msg, wantStation)
			}
			if strings.Contains(msg, filteredStation) {
				t.Fatalf("%s %s message %q contains out-of-range %s", user, kind, msg, filteredStation)
			}
		}
	}
	// A tight radius sees only its own city's station.
	assertOnlyStation("user-berlin", "Station 1", "Station 2")
	assertOnlyStation("user-hamburg", "Station 2", "Station 1")

	// The wide radius reaches both, and the check lists them cheapest-first —
	// Hamburg's 1.900 beats Berlin's 2.000 even though it is 255 km away.
	wideCheck := kinds["user-all"]["check"]
	if !strings.Contains(wideCheck, "Station 1") || !strings.Contains(wideCheck, "Station 2") {
		t.Fatalf("wide-radius check %q must cover both stations", wideCheck)
	}
	if strings.Index(wideCheck, "Station 2") > strings.Index(wideCheck, "Station 1") {
		t.Fatalf("wide-radius check %q must list the cheaper station first", wideCheck)
	}
	// Its suggestion picks the nearer station: both forecast the same price, so
	// distance from the subscriber breaks the tie.
	wideSuggest := kinds["user-all"]["suggest"]
	if !strings.Contains(wideSuggest, "Station 1") {
		t.Fatalf("wide-radius suggestion %q should prefer the nearby station", wideSuggest)
	}
}

func TestNotifyOnceStartsANewBaselineWhenTheAreaChanges(t *testing.T) {
	withDecimalSeparator(t, ".")
	db := openTestDB(t)
	seedNotifyFixture(t, db)
	seedNotifyUser(t, db, notifyUserFixture{
		Email: "mover@example.com", UserKey: "user-mover", Token: "token-m",
		CheckEnabled: true, SuggestDisabled: true,
	})
	ctx := context.Background()

	stubPushover(t, nil)
	if result := runNotifyOnce(t, db, false); len(result.Sent) != 1 {
		t.Fatalf("first run sent = %+v, want one check", result.Sent)
	}
	// The same price cannot alert again in the same area today.
	if result := runNotifyOnce(t, db, false); len(result.Sent) != 0 {
		t.Fatalf("repeat run sent = %+v, want nothing at an unchanged price", result.Sent)
	}

	// The user widens their area. That is a different question — "cheapest
	// around here" now covers different ground — so the old area's minimum must
	// not go on suppressing alerts until midnight.
	if _, err := db.ExecContext(ctx, `UPDATE users SET notify_radius_km = 25 WHERE email = 'mover@example.com'`); err != nil {
		t.Fatalf("widen area: %v", err)
	}
	result := runNotifyOnce(t, db, false)
	if result.BaselineReset {
		t.Fatal("the day's reset must not be what let this through")
	}
	if len(result.Sent) != 1 || result.Sent[0].Kind != "check" {
		t.Fatalf("after the area change sent = %+v, want a check for the new area", result.Sent)
	}
}

func TestNotifyOnceDoesNotWarnAboutPausedUsersWithoutAnArea(t *testing.T) {
	db := openTestDB(t)
	seedNotifyFixture(t, db)
	// Both kinds off is how a user pauses notifications. They have no area and
	// do not need one, so every run must not nag about it.
	seedNotifyUser(t, db, notifyUserFixture{
		Email: "paused@example.com", UserKey: "user-paused", Token: "token-p",
		SuggestDisabled: true, NoLocation: true,
	})

	stubPushover(t, nil)
	stderr := captureStderr(t, func() {
		runNotifyOnce(t, db, false)
	})
	if strings.Contains(stderr, "paused@example.com") {
		t.Fatalf("a paused user was warned about: %q", stderr)
	}

	// A user who does want notifications is still told.
	seedNotifyUser(t, db, notifyUserFixture{
		Email: "wants@example.com", UserKey: "user-wants", Token: "token-w",
		CheckEnabled: true, SuggestDisabled: true, NoLocation: true,
	})
	stderr = captureStderr(t, func() {
		runNotifyOnce(t, db, false)
	})
	if !strings.Contains(stderr, "wants@example.com") {
		t.Fatalf("a user expecting notifications was not warned: %q", stderr)
	}
}

func TestNotifyOnceSkipsUsersWithoutASubscription(t *testing.T) {
	withDecimalSeparator(t, ".")
	db := openTestDB(t)
	seedNotifyFixture(t, db)
	seedNotifyUser(t, db, notifyUserFixture{
		Email: "located@example.com", UserKey: "user-located", Token: "token-l",
		SuggestTimes: "13:00", CheckEnabled: true,
	})
	seedNotifyUser(t, db, notifyUserFixture{
		Email: "nowhere@example.com", UserKey: "user-nowhere", Token: "token-n",
		SuggestTimes: "13:00", CheckEnabled: true, NoLocation: true,
	})

	pushes := stubPushover(t, nil)
	result := runNotifyOnce(t, db, false)

	// A subscription is an area; without one there is nothing to send, and the
	// user with one is unaffected.
	if len(result.Sent) != 2 {
		t.Fatalf("sent = %+v, want the located user's check and suggest", result.Sent)
	}
	for _, push := range *pushes {
		if push.User == "user-nowhere" {
			t.Fatalf("user without a location was notified: %+v", push)
		}
	}
}

func TestNotifyOnceMeasuresDistanceFromTheSubscriber(t *testing.T) {
	withDecimalSeparator(t, ".")
	db := openTestDB(t)
	seedNotifyFixture(t, db)
	// About 8 km west of the station, well inside the radius.
	seedNotifyUser(t, db, notifyUserFixture{
		Email: "west@example.com", UserKey: "user-west", Token: "token-w",
		CheckEnabled: true, SuggestDisabled: true,
		City: "Berlin", Lat: notifyFixtureLat, Lng: notifyFixtureLng - 0.118, RadiusKM: 25,
	})

	pushes := stubPushover(t, nil)
	if result := runNotifyOnce(t, db, false); len(result.Sent) != 1 {
		t.Fatalf("sent = %+v, want one check", result.Sent)
	}
	// The default template renders {{distance}}; it has to be the distance from
	// the subscriber, not from any city centre.
	msg := (*pushes)[0].Message
	if strings.Contains(msg, "(0.0 km)") {
		t.Fatalf("distance is not measured from the subscriber: %q", msg)
	}
	if !strings.Contains(msg, "(8.0 km)") {
		t.Fatalf("message = %q, want the ~8 km distance from the subscriber", msg)
	}
}

func TestNotifyBaseURLRequiresHTTPScheme(t *testing.T) {
	t.Chdir(t.TempDir()) // keep any developer .env out of the fallback lookup

	t.Setenv(envBaseURLName, "https://gasoline.example.com")
	if got := notifyBaseURL(); got != "https://gasoline.example.com" {
		t.Fatalf("notifyBaseURL = %q, want the configured URL", got)
	}
	// Pushover rejects scheme-less urls with "url is invalid", which would
	// block every notification: the link must be dropped instead.
	t.Setenv(envBaseURLName, "gasoline.example.com")
	if got := notifyBaseURL(); got != "" {
		t.Fatalf("notifyBaseURL = %q, want empty for a scheme-less value", got)
	}
	t.Setenv(envBaseURLName, "")
	if got := notifyBaseURL(); got != "" {
		t.Fatalf("notifyBaseURL = %q, want empty when unset", got)
	}
	if err := os.WriteFile(".env", []byte(envBaseURLName+"=https://dotenv.example.com\n"), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	if got := notifyBaseURL(); got != "https://dotenv.example.com" {
		t.Fatalf("notifyBaseURL = %q, want the .env fallback", got)
	}
}

func TestNotifyOnceSendsBaseURLLinkAndUnescapesTemplateNewlines(t *testing.T) {
	withDecimalSeparator(t, ".")
	db := openTestDB(t)
	seedNotifyFixture(t, db)
	seedNotifyUser(t, db, notifyUserFixture{
		Email: "in@example.com", UserKey: "user-in", Token: "token-in",
		SuggestTimes: "08:00,13:00", CheckEnabled: true,
	})
	ctx := context.Background()
	// The template is stored the way the admin form sends it: a literal
	// backslash-n, which must arrive at Pushover as a real line break.
	if _, err := db.ExecContext(ctx, kvUpsertSQL(dialectSQLite, "settings"),
		settingCheckTemplate, `{{station_name}}\n{{current_price}} EUR`, "2026-04-01T00:00:00Z"); err != nil {
		t.Fatalf("insert setting: %v", err)
	}

	pushes := stubPushover(t, nil)
	result, err := notifyOnce(ctx, db, dialectSQLite, notifyOptions{
		Now:      notifyFixtureNow,
		Location: time.UTC,
		BaseURL:  "https://gasoline.example.com",
	})
	if err != nil {
		t.Fatalf("notifyOnce: %v", err)
	}
	if len(result.Failed) != 0 {
		t.Fatalf("failed sends: %+v", result.Failed)
	}
	if len(*pushes) != 2 {
		t.Fatalf("pushover calls = %d, want check+suggest", len(*pushes))
	}
	foundCheck := false
	for _, push := range *pushes {
		if push.URL != "https://gasoline.example.com" {
			t.Fatalf("push url = %q, want the base URL", push.URL)
		}
		if strings.Contains(push.Message, "Station 1\n2.000 EUR") {
			foundCheck = true
		}
		if strings.Contains(push.Message, `\n`) {
			t.Fatalf("message contains a raw \\n: %q", push.Message)
		}
	}
	if !foundCheck {
		t.Fatalf("no check message with a real line break in %+v", *pushes)
	}
}

func TestNotifyOnceUsesTitleTemplates(t *testing.T) {
	withDecimalSeparator(t, ".")
	db := openTestDB(t)
	seedNotifyFixture(t, db)
	seedNotifyUser(t, db, notifyUserFixture{
		Email: "in@example.com", UserKey: "user-in", Token: "token-in",
		SuggestTimes: "08:00,13:00", CheckEnabled: true,
	})
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, kvUpsertSQL(dialectSQLite, "settings"),
		settingCheckTitleTemplate, "Tanken für {{cheapest_current_price_formatted}} EUR", "2026-04-01T00:00:00Z"); err != nil {
		t.Fatalf("insert setting: %v", err)
	}
	if _, err := db.ExecContext(ctx, kvUpsertSQL(dialectSQLite, "settings"),
		settingSuggestTitleTemplate, "Tanken {{weekday_short_formatted}} {{start_time}} ({{count}})", "2026-04-01T00:00:00Z"); err != nil {
		t.Fatalf("insert setting: %v", err)
	}

	pushes := stubPushover(t, nil)
	result := runNotifyOnce(t, db, false)
	if len(result.Failed) != 0 {
		t.Fatalf("failed sends: %+v", result.Failed)
	}
	if len(*pushes) != 2 {
		t.Fatalf("pushover calls = %d, want 2", len(*pushes))
	}
	titles := map[string]string{}
	for _, push := range *pushes {
		if strings.Contains(push.Message, "predicted") {
			titles["suggest"] = push.Title
		} else {
			titles["check"] = push.Title
		}
	}
	// The fixture's fresh snapshot is 2.000 EUR, so the check title renders
	// the cheapest row's truncated price.
	if titles["check"] != "Tanken für 2.00 EUR" {
		t.Fatalf("check title = %q", titles["check"])
	}
	if titles["suggest"] == "" || titles["suggest"] == "gasoline" || strings.Contains(titles["suggest"], "{{") {
		t.Fatalf("suggest title = %q, want rendered template", titles["suggest"])
	}
}

func TestNotifyOnceBaselineBlocksRepeatsAndResetsDaily(t *testing.T) {
	withDecimalSeparator(t, ".")
	db := openTestDB(t)
	seedNotifyFixture(t, db)
	seedNotifyUser(t, db, notifyUserFixture{
		Email: "in@example.com", UserKey: "user-in", Token: "token-in",
		SuggestTimes: "08:00", CheckEnabled: true,
		LastSuggest: "2026-04-26T08:00", // suggestion already fired today
	})

	pushes := stubPushover(t, nil)

	first := runNotifyOnce(t, db, false)
	if len(first.Sent) != 1 || first.Sent[0].Kind != "check" {
		t.Fatalf("first run sent = %+v, want one check", first.Sent)
	}
	if !first.BaselineReset {
		t.Fatal("first run must reset the (empty) baseline for the day")
	}

	second := runNotifyOnce(t, db, false)
	if len(second.Sent) != 0 || len(second.Failed) != 0 {
		t.Fatalf("second run sent = %+v failed = %+v, want nothing (baseline blocks)", second.Sent, second.Failed)
	}

	// Force yesterday's reset marker: the day rollover clears baselines and
	// the same price notifies again.
	if err := setNotificationState(context.Background(), db, dialectSQLite, "check_baseline_reset_date", "2026-04-25"); err != nil {
		t.Fatalf("set state: %v", err)
	}
	third := runNotifyOnce(t, db, false)
	if !third.BaselineReset {
		t.Fatal("third run must reset the baseline for the new day")
	}
	if len(third.Sent) != 1 || third.Sent[0].Kind != "check" {
		t.Fatalf("third run sent = %+v, want one check after reset", third.Sent)
	}
	if len(*pushes) != 2 {
		t.Fatalf("pushover calls = %d, want 2", len(*pushes))
	}
}

func TestNotifyOnceSuggestFiresOncePerSlot(t *testing.T) {
	withDecimalSeparator(t, ".")
	db := openTestDB(t)
	seedNotifyFixture(t, db)
	seedNotifyUser(t, db, notifyUserFixture{
		Email: "in@example.com", UserKey: "user-in", Token: "token-in",
		SuggestTimes: "08:00,13:00",
	})

	stubPushover(t, nil)

	morning := notifyFixtureNow.Add(-7 * time.Hour) // 08:30 local
	first, err := notifyOnce(context.Background(), db, dialectSQLite, notifyOptions{Now: morning, Location: time.UTC})
	if err != nil {
		t.Fatalf("notifyOnce: %v", err)
	}
	if len(first.Sent) != 1 || first.Sent[0].Kind != "suggest" {
		t.Fatalf("morning sent = %+v, want one suggest", first.Sent)
	}

	// Same slot again: nothing.
	repeat, err := notifyOnce(context.Background(), db, dialectSQLite, notifyOptions{Now: morning.Add(10 * time.Minute), Location: time.UTC})
	if err != nil {
		t.Fatalf("notifyOnce: %v", err)
	}
	if len(repeat.Sent) != 0 {
		t.Fatalf("repeat sent = %+v, want nothing", repeat.Sent)
	}

	// Afternoon: the 13:00 slot fires separately.
	afternoon := runNotifyOnce(t, db, false)
	if len(afternoon.Sent) != 1 || afternoon.Sent[0].Kind != "suggest" {
		t.Fatalf("afternoon sent = %+v, want one suggest", afternoon.Sent)
	}
}

func TestNotifyOnceSendFailureRetriesAndIsolatesUsers(t *testing.T) {
	withDecimalSeparator(t, ".")
	db := openTestDB(t)
	seedNotifyFixture(t, db)
	seedNotifyUser(t, db, notifyUserFixture{
		Email: "bad@example.com", UserKey: "user-bad", Token: "token-bad",
		SuggestTimes: "08:00",
	})
	seedNotifyUser(t, db, notifyUserFixture{
		Email: "good@example.com", UserKey: "user-good", Token: "token-good",
		SuggestTimes: "08:00",
	})

	pushes := stubPushover(t, func(user string) bool { return user == "user-bad" })
	result := runNotifyOnce(t, db, false)

	if len(result.Failed) != 1 || result.Failed[0].Email != "bad@example.com" {
		t.Fatalf("failed = %+v, want bad@example.com", result.Failed)
	}
	if !strings.Contains(result.Failed[0].Error, "user identifier is invalid") {
		t.Fatalf("error = %q, want pushover API error text", result.Failed[0].Error)
	}
	if len(result.Sent) != 1 || result.Sent[0].Email != "good@example.com" {
		t.Fatalf("sent = %+v, want good@example.com", result.Sent)
	}
	if len(*pushes) != 1 {
		t.Fatalf("pushover successes = %d, want 1", len(*pushes))
	}

	var badMarker, goodMarker string
	if err := db.QueryRowContext(context.Background(),
		`SELECT notify_last_suggest FROM users WHERE email = 'bad@example.com'`).Scan(&badMarker); err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if err := db.QueryRowContext(context.Background(),
		`SELECT notify_last_suggest FROM users WHERE email = 'good@example.com'`).Scan(&goodMarker); err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if badMarker != "" {
		t.Fatalf("failed user's marker = %q, want empty for retry", badMarker)
	}
	if goodMarker == "" {
		t.Fatal("successful user's marker must advance")
	}

	// Next run retries only the failed user.
	retry := runNotifyOnce(t, db, false)
	if len(retry.Failed) != 1 || len(retry.Sent) != 0 {
		t.Fatalf("retry = sent %+v failed %+v, want one failed retry", retry.Sent, retry.Failed)
	}
}

func TestNotifyOnceDryRunSendsNothingAndWritesNoState(t *testing.T) {
	withDecimalSeparator(t, ".")
	db := openTestDB(t)
	seedNotifyFixture(t, db)
	seedNotifyUser(t, db, notifyUserFixture{
		Email: "in@example.com", UserKey: "user-in", Token: "token-in",
		SuggestTimes: "08:00,13:00", CheckEnabled: true,
	})

	restore := stubDefaultTransport(t, func(req *http.Request) (*http.Response, error) {
		t.Fatalf("dry run must not call the network: %s", req.URL.String())
		return nil, nil
	})
	defer restore()

	result := runNotifyOnce(t, db, true)
	if !result.DryRun || len(result.Sent) != 2 {
		t.Fatalf("dry run result = %+v, want 2 would-be sends", result)
	}

	var stateCount int
	if err := db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM notification_state`).Scan(&stateCount); err != nil {
		t.Fatalf("count state: %v", err)
	}
	if stateCount != 0 {
		t.Fatalf("notification_state rows = %d, want 0 after dry run", stateCount)
	}
	var marker string
	if err := db.QueryRowContext(context.Background(),
		`SELECT notify_last_suggest FROM users WHERE email = 'in@example.com'`).Scan(&marker); err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if marker != "" {
		t.Fatalf("marker = %q, want empty after dry run", marker)
	}
}

func TestNotifyOnceNoTargetsOrUsersIsNoop(t *testing.T) {
	db := openTestDB(t)
	result := runNotifyOnce(t, db, false)
	if result.Stations != 0 || result.Users != 0 || len(result.Sent) != 0 {
		t.Fatalf("result = %+v, want noop", result)
	}
}

func TestSendPushoverErrorBody(t *testing.T) {
	restore := stubDefaultTransport(t, func(req *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusBadRequest, `{"status":0,"errors":["user identifier is invalid"]}`), nil
	})
	defer restore()

	err := sendPushover(context.Background(), pushoverMessagesURL, pushoverMessage{
		Token: "t", UserKey: "u", Title: "x", Message: "m",
	})
	if err == nil || !strings.Contains(err.Error(), "user identifier is invalid") {
		t.Fatalf("err = %v, want API error text", err)
	}
}

func TestNotifyOnceBaselineResetCatchesUpAfterDowntime(t *testing.T) {
	withDecimalSeparator(t, ".")
	db := openTestDB(t)
	seedNotifyFixture(t, db)
	seedNotifyUser(t, db, notifyUserFixture{
		Email: "in@example.com", UserKey: "user-in", Token: "token-in",
		SuggestTimes: "08:00", CheckEnabled: true,
		LastSuggest: "2026-04-26T08:00",
	})
	// A marker from three days ago means the process was down over the last
	// midnight boundary: the reset must catch up rather than keep a stale
	// baseline.
	if err := setNotificationState(context.Background(), db, dialectSQLite, "check_baseline_reset_date", "2026-04-23"); err != nil {
		t.Fatalf("set state: %v", err)
	}
	if err := setNotificationState(context.Background(), db, dialectSQLite, notifyFixtureBaseline(1, "diesel"), "1.000"); err != nil {
		t.Fatalf("set stale baseline: %v", err)
	}

	stubPushover(t, nil)
	result := runNotifyOnce(t, db, false)
	if !result.BaselineReset {
		t.Fatal("missed reset boundary must be caught up")
	}
	// The stale 1.000 baseline would have blocked the 2.000 check row.
	if len(result.Sent) != 1 || result.Sent[0].Kind != "check" {
		t.Fatalf("sent = %+v, want one check after catch-up reset", result.Sent)
	}
	// The reset is at local midnight, which today's fixture time (15:30) is
	// already past, so catching up lands on today.
	value, found, err := getNotificationState(context.Background(), db, "check_baseline_reset_date")
	if err != nil || !found || value != "2026-04-26" {
		t.Fatalf("reset marker = %q found=%v err=%v, want today 2026-04-26", value, found, err)
	}
}

func TestNotifyOnceCheckBaselineNotAdvancedWhenAllSendsFail(t *testing.T) {
	withDecimalSeparator(t, ".")
	db := openTestDB(t)
	seedNotifyFixture(t, db)
	seedNotifyUser(t, db, notifyUserFixture{
		Email: "bad@example.com", UserKey: "user-bad", Token: "token-bad",
		SuggestTimes: "08:00", CheckEnabled: true,
		LastSuggest: "2026-04-26T08:00",
	})

	pushes := stubPushover(t, func(user string) bool { return user == "user-bad" })

	first := runNotifyOnce(t, db, false)
	if len(first.Failed) != 1 || len(first.Sent) != 0 {
		t.Fatalf("first run = sent %+v failed %+v, want one failure", first.Sent, first.Failed)
	}
	if _, found, _ := getNotificationState(context.Background(), db, notifyFixtureBaseline(1, "diesel")); found {
		t.Fatal("baseline must not advance when every send failed")
	}

	// Once delivery works again, the same rows are retried and the baseline
	// advances.
	*pushes = nil
	restore := stubDefaultTransport(t, func(req *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"status":1}`), nil
	})
	defer restore()
	second := runNotifyOnce(t, db, false)
	if len(second.Sent) != 1 || second.Sent[0].Kind != "check" {
		t.Fatalf("second run sent = %+v, want retried check", second.Sent)
	}
	if _, found, _ := getNotificationState(context.Background(), db, notifyFixtureBaseline(1, "diesel")); !found {
		t.Fatal("baseline must advance after a successful delivery")
	}
}

func TestSendPushoverTruncatesByRunes(t *testing.T) {
	var got string
	restore := stubDefaultTransport(t, func(req *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(req.Body)
		form, err := url.ParseQuery(string(body))
		if err != nil {
			return nil, err
		}
		got = form.Get("message")
		return jsonResponse(http.StatusOK, `{"status":1}`), nil
	})
	defer restore()

	long := strings.Repeat("ü", pushoverMessageLimit+10)
	err := sendPushover(context.Background(), pushoverMessagesURL, pushoverMessage{
		Token: "t", UserKey: "u", Message: long,
	})
	if err != nil {
		t.Fatalf("sendPushover: %v", err)
	}
	runes := []rune(got)
	if len(runes) != pushoverMessageLimit {
		t.Fatalf("message runes = %d, want %d", len(runes), pushoverMessageLimit)
	}
	for _, r := range runes {
		if r != 'ü' {
			t.Fatal("truncation corrupted a multi-byte character")
		}
	}
}

func TestNotifyOnceCheckBaselinesArePerUser(t *testing.T) {
	withDecimalSeparator(t, ".")
	db := openTestDB(t)
	seedNotifyFixture(t, db)
	// Both users are schedule-active with checks enabled; deliveries to the
	// second user fail on the first run.
	seedNotifyUser(t, db, notifyUserFixture{
		Email: "a@example.com", UserKey: "user-a", Token: "token-a",
		SuggestTimes: "08:00", CheckEnabled: true, LastSuggest: "2026-04-26T08:00",
	})
	seedNotifyUser(t, db, notifyUserFixture{
		Email: "b@example.com", UserKey: "user-b", Token: "token-b",
		SuggestTimes: "08:00", CheckEnabled: true, LastSuggest: "2026-04-26T08:00",
	})

	stubPushover(t, func(user string) bool { return user == "user-b" })
	first := runNotifyOnce(t, db, false)
	if len(first.Sent) != 1 || first.Sent[0].Email != "a@example.com" {
		t.Fatalf("first run sent = %+v, want only a@example.com", first.Sent)
	}
	if len(first.Failed) != 1 || first.Failed[0].Email != "b@example.com" {
		t.Fatalf("first run failed = %+v, want b@example.com", first.Failed)
	}
	if _, found, _ := getNotificationState(context.Background(), db, notifyFixtureBaseline(1, "diesel")); !found {
		t.Fatal("user A's baseline must advance after their delivery")
	}
	if _, found, _ := getNotificationState(context.Background(), db, notifyFixtureBaseline(2, "diesel")); found {
		t.Fatal("user B's baseline must not advance while their sends fail")
	}

	// Delivery recovers: user A's advanced baseline blocks a repeat for A,
	// while user B is retried and now receives the notification.
	restore := stubDefaultTransport(t, func(req *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, `{"status":1}`), nil
	})
	defer restore()
	second := runNotifyOnce(t, db, false)
	if len(second.Sent) != 1 || second.Sent[0].Email != "b@example.com" {
		t.Fatalf("second run sent = %+v, want only the retried b@example.com", second.Sent)
	}
	if len(second.Failed) != 0 {
		t.Fatalf("second run failed = %+v, want none", second.Failed)
	}
	if _, found, _ := getNotificationState(context.Background(), db, notifyFixtureBaseline(2, "diesel")); !found {
		t.Fatal("user B's baseline must advance after their successful retry")
	}
}

func TestNotifyOncePerUserFuel(t *testing.T) {
	withDecimalSeparator(t, ".")
	db := openTestDB(t)
	seedNotifyFixture(t, db)
	// Every fuel is computed, so each user's single choice is always served.
	seedNotifyUser(t, db, notifyUserFixture{
		Email: "diesel@example.com", UserKey: "user-diesel", Token: "token-d",
		SuggestTimes: "13:00", CheckEnabled: true, Fuel: "diesel",
	})
	seedNotifyUser(t, db, notifyUserFixture{
		Email: "e5@example.com", UserKey: "user-e5", Token: "token-e5",
		SuggestTimes: "13:00", CheckEnabled: true, Fuel: "e5",
	})
	seedNotifyUser(t, db, notifyUserFixture{
		Email: "e10@example.com", UserKey: "user-e10", Token: "token-e10",
		SuggestTimes: "13:00", CheckEnabled: true, Fuel: "e10",
	})

	pushes := stubPushover(t, nil)
	result := runNotifyOnce(t, db, false)
	if len(result.Failed) != 0 {
		t.Fatalf("failed sends: %+v", result.Failed)
	}

	// Every fuel is computed, so each user hears about exactly the one they
	// chose — including e10, which no admin setting can switch off any more.
	fuelByUser := map[string]string{"user-diesel": "diesel", "user-e5": "e5", "user-e10": "e10"}
	seen := map[string]int{}
	for _, push := range *pushes {
		wantFuel, ok := fuelByUser[push.User]
		if !ok {
			t.Fatalf("unexpected recipient: %+v", push)
		}
		if !strings.Contains(push.Message, wantFuel) {
			t.Fatalf("message for %s = %q, want it to mention %q", push.User, push.Message, wantFuel)
		}
		if wantFuel == "diesel" && strings.Contains(push.Message, "e5") {
			t.Fatalf("diesel user message leaked e5 rows: %q", push.Message)
		}
		seen[push.User]++
	}
	if seen["user-diesel"] != 2 || seen["user-e5"] != 2 || seen["user-e10"] != 2 {
		t.Fatalf("push counts = %v, want 2 each for all three fuel users", seen)
	}

	// Baselines are fuel-scoped, so the two users at the same city do not
	// collide.
	if _, found, _ := getNotificationState(context.Background(), db, notifyFixtureBaseline(1, "diesel")); !found {
		t.Fatal("diesel user's diesel baseline must advance")
	}
	if _, found, _ := getNotificationState(context.Background(), db, notifyFixtureBaseline(2, "e5")); !found {
		t.Fatal("e5 user's e5 baseline must advance")
	}
}

func TestNotifyOnceSkipsTargetsWithoutACachedCity(t *testing.T) {
	withDecimalSeparator(t, ".")
	db := openTestDB(t)
	seedNotifyFixture(t, db)
	seedNotifyUser(t, db, notifyUserFixture{
		Email: "one@example.com", UserKey: "user-one", Token: "token-one",
		SuggestTimes: "13:00", CheckEnabled: true,
	})
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO update_targets (city, radius_km, created_at) VALUES (?, ?, ?)`,
		"Nowhere", 5.0, "2026-04-02T00:00:00Z"); err != nil {
		t.Fatalf("insert target: %v", err)
	}

	stubPushover(t, nil)
	result := runNotifyOnce(t, db, false)
	// The uncached target is skipped with a warning; the good one is unaffected.
	if len(result.Sent) != 2 {
		t.Fatalf("sent = %+v, want the cached target still delivered", result.Sent)
	}
}

// captureStderr collects what a function writes to the process's stderr, which
// is where notify reports users it had to skip.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	original := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()
	fn()
	os.Stderr = original
	if err := w.Close(); err != nil {
		t.Fatalf("close pipe: %v", err)
	}
	out := <-done
	if err := r.Close(); err != nil {
		t.Fatalf("close reader: %v", err)
	}
	return out
}
