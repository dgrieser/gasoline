package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// resolveNotifyFuel returns the single fuel a user should be notified about.
// A blank or unrecognized stored value falls back to the first tracked fuel.
// Every fuel is always computed, so a user's choice is always served.
func resolveNotifyFuel(u notifyUser) string {
	f := strings.ToLower(strings.TrimSpace(u.NotifyFuel))
	if isSuggestFuelType(f) {
		return f
	}
	return suggestFuels[0]
}

// notifyUser is one approved web user with a usable Pushover configuration.
type notifyUser struct {
	ID              int64
	Email           string
	PushoverAppName string
	PushoverUserKey string
	PushoverToken   string
	NotifyDays      string
	NotifyWindows   string
	SuggestTimes    string
	CheckEnabled    bool
	SuggestEnabled  bool
	LastSuggest     string // YYYY-MM-DDTHH:MM of the last fired suggestion slot
	NotifyFuel      string // the single fuel this user is notified about
}

type notifyOptions struct {
	Now      time.Time
	Location *time.Location
	DryRun   bool
	APIURL   string // "" -> pushoverMessagesURL
	BaseURL  string // viewer base URL sent as the notification link; "" -> no link
}

// notifyBaseURL resolves the viewer base URL attached to notifications as a
// supplementary link, from the environment or the .env file (same precedence
// as the API key). A value without an HTTP/HTTPS scheme would make Pushover
// reject every send with "url is invalid", so it is dropped with a warning
// instead of blocking all notifications.
func notifyBaseURL() string {
	rawURL := strings.TrimSpace(os.Getenv(envBaseURLName))
	if rawURL == "" {
		values, err := loadDotEnv(".env")
		if err != nil {
			return ""
		}
		rawURL = strings.TrimSpace(values[envBaseURLName])
	}
	if rawURL == "" {
		return ""
	}
	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		fmt.Fprintf(os.Stderr, "warning: %s %q is not an absolute HTTP/HTTPS URL; omitting the notification link\n", envBaseURLName, rawURL)
		return ""
	}
	return rawURL
}

type notifySendRecord struct {
	Email string `json:"email"`
	Kind  string `json:"kind"`
	Error string `json:"error,omitempty"`
}

type notifyResult struct {
	Targets       int                `json:"targets"`
	Users         int                `json:"users"`
	CheckRows     int                `json:"check_rows"`
	SuggestRows   int                `json:"suggest_rows"`
	BaselineReset bool               `json:"baseline_reset"`
	DryRun        bool               `json:"dry_run"`
	Sent          []notifySendRecord `json:"sent"`
	Failed        []notifySendRecord `json:"failed"`
	DBPath        string             `json:"db_path"`
}

func runNotify(args []string) error {
	fs := flag.NewFlagSet("notify", flag.ContinueOnError)
	dbf := addDBFlags(fs)
	dryRun := fs.Bool("dry-run", false, "Render notifications and report recipients without sending or writing state")
	outputLong, outputShort := addOutputFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	dbCfg, err := resolveDBConfig(fs, dbf)
	if err != nil {
		return err
	}
	output, err := resolveOutputMode(*outputLong, *outputShort)
	if err != nil {
		return err
	}

	ctx := context.Background()
	db, err := openDatabase(ctx, dbCfg)
	if err != nil {
		return err
	}
	defer db.Close()

	if err := initSchema(ctx, db, dbCfg.Driver); err != nil {
		return err
	}

	result, err := notifyOnce(ctx, db, dbCfg.Driver, notifyOptions{
		Now:      time.Now().UTC(),
		Location: time.Local,
		DryRun:   *dryRun,
		BaseURL:  notifyBaseURL(),
	})
	if err != nil {
		return err
	}
	result.DBPath = dbCfg.Description()

	if output == outputJSON {
		return writeJSON(result)
	}
	printNotifyResultText(result)
	if len(result.Failed) > 0 && len(result.Sent) == 0 {
		return fmt.Errorf("all %d notification sends failed", len(result.Failed))
	}
	return nil
}

func printNotifyResultText(result notifyResult) {
	fmt.Fprintf(stdout, "targets: %d, users: %d, check rows: %d, suggest rows: %d\n",
		result.Targets, result.Users, result.CheckRows, result.SuggestRows)
	if result.BaselineReset {
		fmt.Fprintln(stdout, "check baseline reset for the new day")
	}
	if result.DryRun {
		fmt.Fprintln(stdout, "dry run: nothing was sent and no state was written")
	}
	for _, rec := range result.Sent {
		fmt.Fprintf(stdout, "sent %s notification to %s\n", rec.Kind, rec.Email)
	}
	for _, rec := range result.Failed {
		fmt.Fprintf(stdout, "failed %s notification to %s: %s\n", rec.Kind, rec.Email, rec.Error)
	}
	if len(result.Sent) == 0 && len(result.Failed) == 0 {
		fmt.Fprintln(stdout, "nothing to send")
	}
}

func loadNotifyUsers(ctx context.Context, db *sql.DB) ([]notifyUser, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, email, pushover_app_name, pushover_user_key, pushover_token,
			notify_days, notify_windows, notify_suggest_times, notify_check_enabled,
			notify_suggest_enabled, notify_last_suggest, notify_fuel
		FROM users
		WHERE status = 'approved' AND notify_method = 'pushover'
			AND pushover_user_key <> '' AND pushover_token <> ''
		ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []notifyUser
	for rows.Next() {
		var u notifyUser
		var checkEnabled, suggestEnabled int
		if err := rows.Scan(&u.ID, &u.Email, &u.PushoverAppName, &u.PushoverUserKey, &u.PushoverToken,
			&u.NotifyDays, &u.NotifyWindows, &u.SuggestTimes, &checkEnabled, &suggestEnabled,
			&u.LastSuggest, &u.NotifyFuel); err != nil {
			return nil, err
		}
		u.CheckEnabled = checkEnabled != 0
		u.SuggestEnabled = suggestEnabled != 0
		users = append(users, u)
	}
	return users, rows.Err()
}

func setUserLastSuggest(ctx context.Context, db *sql.DB, userID int64, value string) error {
	_, err := db.ExecContext(ctx, `UPDATE users SET notify_last_suggest = ? WHERE id = ?`, value, userID)
	return err
}

// --- schedule helpers ---

var hhmmPattern = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)

// parseHHMM returns minutes since midnight.
func parseHHMM(s string) (int, error) {
	if !hhmmPattern.MatchString(s) {
		return 0, fmt.Errorf("invalid time %q (expected HH:MM)", s)
	}
	h, _ := strconv.Atoi(s[:2])
	m, _ := strconv.Atoi(s[3:])
	return h*60 + m, nil
}

type timeWindow struct {
	From int // minutes since midnight
	To   int
}

// parseWindows parses a comma-separated list of HH:MM-HH:MM ranges.
func parseWindows(s string) ([]timeWindow, error) {
	var windows []timeWindow
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		from, to, ok := strings.Cut(part, "-")
		if !ok {
			return nil, fmt.Errorf("invalid time window %q (expected HH:MM-HH:MM)", part)
		}
		fromMin, err := parseHHMM(strings.TrimSpace(from))
		if err != nil {
			return nil, err
		}
		toMin, err := parseHHMM(strings.TrimSpace(to))
		if err != nil {
			return nil, err
		}
		windows = append(windows, timeWindow{From: fromMin, To: toMin})
	}
	return windows, nil
}

// parseTimesList parses a comma-separated list of HH:MM values, sorted.
func parseTimesList(s string) ([]string, error) {
	var times []string
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if _, err := parseHHMM(part); err != nil {
			return nil, err
		}
		times = append(times, part)
	}
	sort.Strings(times)
	return times, nil
}

// loadUserCitySelections returns each user's notification city selection from
// user_notify_cities. Users without rows are absent from the map: a nil set
// means "all cities" (see citySelected). The foreign key onto
// update_targets(city) guarantees stored values match target cities verbatim
// and removes selections when a target is deleted.
func loadUserCitySelections(ctx context.Context, db *sql.DB) (map[int64]map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT user_id, city FROM user_notify_cities`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	selections := map[int64]map[string]bool{}
	for rows.Next() {
		var userID int64
		var city string
		if err := rows.Scan(&userID, &city); err != nil {
			return nil, err
		}
		if selections[userID] == nil {
			selections[userID] = map[string]bool{}
		}
		selections[userID][city] = true
	}
	return selections, rows.Err()
}

// citySelected reports whether a user's selection covers a city. A nil set (no
// rows for that user) selects every city, including targets added later.
//
// A city can be named by more than one update target, and users select target
// names; picking any of them selects the city.
func citySelected(set map[string]bool, targets []string) bool {
	if set == nil {
		return true
	}
	for _, target := range targets {
		if set[target] {
			return true
		}
	}
	return false
}

var weekdayNames = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

// parseDaySet parses a comma-separated weekday list ("mon,tue,...").
func parseDaySet(s string) (map[time.Weekday]bool, error) {
	days := map[time.Weekday]bool{}
	for _, part := range strings.Split(s, ",") {
		part = strings.ToLower(strings.TrimSpace(part))
		if part == "" {
			continue
		}
		wd, ok := weekdayNames[part]
		if !ok {
			return nil, fmt.Errorf("invalid weekday %q", part)
		}
		days[wd] = true
	}
	return days, nil
}

// scheduleActive reports whether local time now falls on an enabled weekday
// and inside at least one window. Empty specs fall back to the provided
// defaults; from > to windows wrap over midnight.
func scheduleActive(now time.Time, daysSpec, windowsSpec string) (bool, error) {
	if strings.TrimSpace(daysSpec) == "" {
		daysSpec = defaultNotifyDays
	}
	if strings.TrimSpace(windowsSpec) == "" {
		windowsSpec = defaultNotifyWindows
	}
	days, err := parseDaySet(daysSpec)
	if err != nil {
		return false, err
	}
	if len(days) > 0 && !days[now.Weekday()] {
		return false, nil
	}
	windows, err := parseWindows(windowsSpec)
	if err != nil {
		return false, err
	}
	if len(windows) == 0 {
		return true, nil
	}
	minute := now.Hour()*60 + now.Minute()
	for _, w := range windows {
		if w.From <= w.To {
			if minute >= w.From && minute <= w.To {
				return true, nil
			}
		} else if minute >= w.From || minute <= w.To {
			// Overnight wrap, e.g. 22:00-06:00.
			return true, nil
		}
	}
	return false, nil
}

// dueSuggestSlot returns the latest suggestion slot that is due now. A slot
// is due when it is <= now and was not fired yet today (lastFired is
// YYYY-MM-DDTHH:MM). Missed slots collapse into the latest one, so a cron gap
// never bursts multiple notifications.
func dueSuggestSlot(now time.Time, slots []string, lastFired string) (string, bool) {
	today := now.Format("2006-01-02")
	nowHHMM := now.Format("15:04")
	lastSlot := ""
	if lastFired != "" {
		date, slot, ok := strings.Cut(lastFired, "T")
		if ok && date == today {
			lastSlot = slot
		}
	}
	due := ""
	for _, slot := range slots {
		if slot <= nowHHMM && (lastSlot == "" || slot > lastSlot) {
			due = slot
		}
	}
	return due, due != ""
}

// --- notify orchestration ---

// notifyTitle renders the admin-configured title template of a notification.
// An unset template — or one that renders to nothing — falls back to the
// user's configured application name, matching the pre-template behavior.
func notifyTitle(template string, kind notifyKind, cheapest *notifyRow, rowCount int, fallback string) string {
	if strings.TrimSpace(template) == "" {
		return fallback
	}
	if title := renderNotifyTitle(template, kind, cheapest, rowCount); title != "" {
		return title
	}
	return fallback
}

func notifyOnce(ctx context.Context, db *sql.DB, d dialect, opts notifyOptions) (notifyResult, error) {
	apiURL := opts.APIURL
	if apiURL == "" {
		apiURL = pushoverMessagesURL
	}
	result := notifyResult{DryRun: opts.DryRun}

	settings, err := loadSettings(ctx, db)
	if err != nil {
		return result, err
	}
	targets, err := loadUpdateTargets(ctx, db)
	if err != nil {
		return result, err
	}
	users, err := loadNotifyUsers(ctx, db)
	if err != nil {
		return result, err
	}
	citySelections, err := loadUserCitySelections(ctx, db)
	if err != nil {
		return result, err
	}
	result.Targets = len(targets)
	result.Users = len(users)
	if len(targets) == 0 || len(users) == 0 {
		return result, nil
	}

	localNow := opts.Now.In(opts.Location)
	today := localNow.Format("2006-01-02")

	// Daily baseline reset, mirroring the watcher's --reset-time behavior.
	// The reset applies once per reset boundary: the target date is today
	// when the reset time has passed, otherwise yesterday. Comparing dates
	// (instead of requiring a run between the reset time and midnight)
	// catches up after downtime instead of keeping a stale baseline.
	resetMin, err := parseHHMM(checkBaselineResetTime)
	if err != nil {
		return result, fmt.Errorf("invalid checkBaselineResetTime %q: %v", checkBaselineResetTime, err)
	}
	nowMin := localNow.Hour()*60 + localNow.Minute()
	targetResetDate := today
	if nowMin < resetMin {
		targetResetDate = localNow.AddDate(0, 0, -1).Format("2006-01-02")
	}
	lastReset, _, err := getNotificationState(ctx, db, "check_baseline_reset_date")
	if err != nil {
		return result, err
	}
	if lastReset < targetResetDate {
		result.BaselineReset = true
		if !opts.DryRun {
			if err := clearCheckBaselines(ctx, db); err != nil {
				return result, err
			}
			if err := setNotificationState(ctx, db, d, "check_baseline_reset_date", targetResetDate); err != nil {
				return result, err
			}
		}
	}

	// Split users by what they can receive right now.
	var checkUsers, suggestUsers []notifyUser
	var suggestSlots = map[int64]string{}
	for _, u := range users {
		days := u.NotifyDays
		if strings.TrimSpace(days) == "" {
			days = defaultNotifyDays
		}
		windows := u.NotifyWindows
		if strings.TrimSpace(windows) == "" {
			windows = defaultNotifyWindows
		}
		active, err := scheduleActive(localNow, days, windows)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: skipping %s: %v\n", u.Email, err)
			continue
		}
		if !active {
			continue
		}
		if u.CheckEnabled {
			checkUsers = append(checkUsers, u)
		}
		if !u.SuggestEnabled {
			continue
		}
		timesSpec := u.SuggestTimes
		if strings.TrimSpace(timesSpec) == "" {
			timesSpec = defaultSuggestTimes
		}
		slots, err := parseTimesList(timesSpec)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: skipping suggestions for %s: %v\n", u.Email, err)
			continue
		}
		if slot, due := dueSuggestSlot(localNow, slots, u.LastSuggest); due {
			suggestUsers = append(suggestUsers, u)
			suggestSlots[u.ID] = slot
		}
	}

	// The price history is read once and shared by both phases and every fuel.
	var (
		checksByFuel   map[string][]cityCheckRows
		suggestsByFuel map[string][]citySuggestRows
	)
	if len(checkUsers) > 0 || len(suggestUsers) > 0 {
		cities := resolveTargetCities(ctx, db, targets)
		scan, err := loadSnapshotScan(ctx, db, opts.Now.AddDate(0, 0, -modelHistoryDays), opts.Now)
		if err != nil {
			return result, err
		}
		if len(checkUsers) > 0 {
			checksByFuel = collectChecks(ctx, db, scan, cities, opts)
		}
		if len(suggestUsers) > 0 {
			suggestsByFuel = collectSuggestions(ctx, db, scan, cities, opts)
		}
	}

	// Check phase: the price data is computed once per fuel, but the
	// cheaper-than-baseline filter runs per user against per-user baseline
	// keys, and a user's baselines advance only after their own delivery
	// succeeded. One user's schedule or a partial send failure therefore
	// never suppresses another user's notification, and failed sends are
	// retried on the next run.
	if len(checkUsers) > 0 {
		for _, u := range checkUsers {
			fuel := resolveNotifyFuel(u)
			userRows, userBaselines, err := userCheckRows(ctx, db, fuel, checksByFuel[fuel], u.ID, citySelections[u.ID])
			if err != nil {
				return result, err
			}
			if len(userRows) == 0 {
				continue
			}
			result.CheckRows += len(userRows)
			cheapest := userRows[0]
			message := renderNotifyMessage(settings.CheckTemplate, notifyKindCheck, userRows, &cheapest)
			title := notifyTitle(settings.CheckTitleTemplate, notifyKindCheck, &cheapest, len(userRows), u.PushoverAppName)
			rec := notifySendRecord{Email: u.Email, Kind: "check"}
			if opts.DryRun {
				result.Sent = append(result.Sent, rec)
				continue
			}
			if err := sendPushover(ctx, apiURL, pushoverMessage{
				Token: u.PushoverToken, UserKey: u.PushoverUserKey,
				Title: title, Message: message, URL: opts.BaseURL,
			}); err != nil {
				// Leave this user's baselines untouched so the next run
				// retries them.
				rec.Error = err.Error()
				result.Failed = append(result.Failed, rec)
				continue
			}
			for name, value := range userBaselines {
				if err := setNotificationState(ctx, db, d, name, value); err != nil {
					return result, err
				}
			}
			result.Sent = append(result.Sent, rec)
		}
	}

	// Suggest phase: computed once per fuel (identical options for all users),
	// assemble the rows per user from their city selection, deliver, and
	// advance each user's slot marker.
	if len(suggestUsers) > 0 {
		for _, u := range suggestUsers {
			marker := today + "T" + suggestSlots[u.ID]
			fuel := resolveNotifyFuel(u)
			userRows := userSuggestRows(suggestsByFuel[fuel], citySelections[u.ID])
			result.SuggestRows += len(userRows)
			if len(userRows) == 0 {
				// Nothing to say: still advance the marker so the empty
				// result is not retried until the next slot.
				if !opts.DryRun {
					if err := setUserLastSuggest(ctx, db, u.ID, marker); err != nil {
						return result, err
					}
				}
				continue
			}
			cheapest := cheapestSuggestRow(userRows)
			message := renderNotifyMessage(settings.SuggestTemplate, notifyKindSuggest, userRows, &cheapest)
			rec := notifySendRecord{Email: u.Email, Kind: "suggest"}
			if opts.DryRun {
				result.Sent = append(result.Sent, rec)
				continue
			}
			if err := sendPushover(ctx, apiURL, pushoverMessage{
				Token: u.PushoverToken, UserKey: u.PushoverUserKey,
				Title:   notifyTitle(settings.SuggestTitleTemplate, notifyKindSuggest, &cheapest, len(userRows), u.PushoverAppName),
				Message: message,
				URL:     opts.BaseURL,
			}); err != nil {
				// Leave the marker untouched so the next run retries.
				rec.Error = err.Error()
				result.Failed = append(result.Failed, rec)
				continue
			}
			if err := setUserLastSuggest(ctx, db, u.ID, marker); err != nil {
				return result, err
			}
			result.Sent = append(result.Sent, rec)
		}
	}

	return result, nil
}

// targetCity is one cached city paired with every update target that names it.
//
// Normalized is the geocoder's name for the place, which is what
// price_snapshots.city_name records. Targets are the strings admins typed, stored
// verbatim in update_targets.city and referenced by user_notify_cities — a target
// added as "Berlin, Germany" owns snapshots filed under "Berlin", so matching
// stations against the raw target string would silently deliver nothing.
//
// Targets is a list because update_targets.city is unique per string, not per
// place: an admin can configure "Berlin" and "Berlin, Germany" as two targets of
// the same city. They own exactly the same stations, so they are one city with
// two names — see resolveTargetCities.
type targetCity struct {
	Normalized string
	Targets    []string
}

// Key is the city's stable identity for per-city state: the first configured
// target that names it. Check baselines track one price series per city, so the
// key must not depend on which of several spellings a given user selected.
func (c targetCity) Key() string { return c.Targets[0] }

// resolveTargetCities pairs each update target with its cached city, grouping
// targets that resolve to the same place.
//
// A target whose city is not cached cannot own a snapshot yet, so it is reported
// and skipped rather than quietly matching nothing.
func resolveTargetCities(ctx context.Context, db *sql.DB, targets []updateTarget) []targetCity {
	resolved := make([]targetCity, 0, len(targets))
	index := map[string]int{}
	for _, target := range targets {
		normalized, err := lookupCityNormalizedName(ctx, db, target.City)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: skipping update target %s: %v\n", target.City, err)
			continue
		}
		if at, ok := index[normalized]; ok {
			// Two targets name the same place. Keeping them apart would put the
			// same stations in two buckets, so every user who selected both — or
			// selected nothing, which means all cities — would receive each row
			// twice, against two separate baselines.
			resolved[at].Targets = append(resolved[at].Targets, target.City)
			continue
		}
		index[normalized] = len(resolved)
		resolved = append(resolved, targetCity{Normalized: normalized, Targets: []string{target.City}})
	}
	return resolved
}

// cityCheckRows is the pre-filtered price check of the stations one city owns:
// buy recommendations with medium/high confidence, sorted cheapest first. It is
// computed once per run and shared by all users.
type cityCheckRows struct {
	city targetCity
	rows []priceCheckRow
}

// collectChecks runs the check once per fuel over every station currently being
// fed, then groups the deliverable rows by the update target that owns each
// station so each user's city selection can be applied afterwards.
//
// The computation itself is city-independent, and every station has exactly one
// owner, so no station can land in two buckets — not for targets whose radii
// overlap, and not for two targets naming the same city (resolveTargetCities
// folds those together). A fuel whose model has too little data is skipped with a
// warning rather than failing the whole run.
func collectChecks(ctx context.Context, db *sql.DB, scan snapshotScan, cities []targetCity, opts notifyOptions) map[string][]cityCheckRows {
	byFuel := map[string][]cityCheckRows{}
	for _, fuel := range suggestFuels {
		// Limit 0: the per-city row limit is applied after grouping, so one
		// busy city cannot crowd another out of a user's message.
		checks, err := checkGasFromScan(ctx, db, scan, checkOptions{
			Fuel:        fuel,
			HistoryDays: modelHistoryDays,
			PredictDays: forecastPredictDays,
			Limit:       0,
			Now:         opts.Now,
			Location:    opts.Location,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: check for %s failed: %v\n", fuel, err)
			continue
		}
		var byCity []cityCheckRows
		for _, city := range cities {
			var matching []priceCheckRow
			for _, row := range checks {
				if row.Station.City != city.Normalized {
					continue
				}
				if row.Recommendation == "buy" && (row.Confidence == "medium" || row.Confidence == "high") {
					matching = append(matching, row)
				}
			}
			if len(matching) == 0 {
				continue
			}
			sort.SliceStable(matching, func(i, j int) bool {
				return matching[i].CurrentPrice < matching[j].CurrentPrice
			})
			if len(matching) > checkRowLimit {
				matching = matching[:checkRowLimit]
			}
			byCity = append(byCity, cityCheckRows{city: city, rows: matching})
		}
		byFuel[fuel] = byCity
	}
	return byFuel
}

// userCheckRows filters the shared target rows against one user's city
// selection and baselines (check_baseline:<user_id>:<fuel>:<city>) and
// returns the rows strictly cheaper than that user's running minimum, sorted
// cheapest-first, plus the baseline updates to persist after a successful
// delivery to that user.
func userCheckRows(ctx context.Context, db *sql.DB, fuel string, cityChecks []cityCheckRows, userID int64, cities map[string]bool) ([]notifyRow, map[string]string, error) {
	var rows []notifyRow
	baselines := map[string]string{}
	for _, tc := range cityChecks {
		if !citySelected(cities, tc.city.Targets) {
			continue
		}
		baselineKey := fmt.Sprintf("check_baseline:%d:%s:%s", userID, fuel, tc.city.Key())
		baselineValue, hasBaseline, err := getNotificationState(ctx, db, baselineKey)
		if err != nil {
			return nil, nil, err
		}
		baseline := 0.0
		if hasBaseline {
			baseline, err = strconv.ParseFloat(baselineValue, 64)
			if err != nil {
				hasBaseline = false
			}
		}
		var cheaper []priceCheckRow
		for i := range tc.rows {
			if !hasBaseline || tc.rows[i].CurrentPrice < baseline {
				cheaper = append(cheaper, tc.rows[i])
			}
		}
		if len(cheaper) == 0 {
			continue
		}
		baselines[baselineKey] = strconv.FormatFloat(cheaper[0].CurrentPrice, 'f', -1, 64)
		for i := range cheaper {
			rows = append(rows, notifyRow{check: &cheaper[i]})
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return rows[i].check.CurrentPrice < rows[j].check.CurrentPrice
	})
	return rows, baselines, nil
}

// citySuggestRows is the pre-filtered suggestion run for the stations one city
// owns: forecast rows with medium/high confidence. It is computed once per run
// and shared by all users.
type citySuggestRows struct {
	city targetCity
	rows []notifyRow
}

// collectSuggestions builds the forecast model once per fuel over every station
// currently being fed, then picks each update target's windows from its own
// stations.
//
// The per-day limit is why the selection is per city rather than global: it is a
// delivery concern, and one city's cheap stations must not use up the slots of
// another city a user also selected. The model is built once and only filtered
// per city (see forecastModel.forCity), so this costs candidate scoring, not a
// second pass over the history.
func collectSuggestions(ctx context.Context, db *sql.DB, scan snapshotScan, cities []targetCity, opts notifyOptions) map[string][]citySuggestRows {
	byFuel := map[string][]citySuggestRows{}
	for _, fuel := range suggestFuels {
		model, _, err := buildFuelForecast(ctx, db, scan, suggestOptions{
			Fuel:        fuel,
			HistoryDays: modelHistoryDays,
			PredictDays: forecastPredictDays,
			LimitPerDay: suggestLimitPerDay,
			Now:         opts.Now,
			Location:    opts.Location,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: suggest for %s failed: %v\n", fuel, err)
			continue
		}
		var byCity []citySuggestRows
		for _, city := range cities {
			cityModel := model.forCity(city.Normalized)
			if len(cityModel.Stations) == 0 {
				continue
			}
			suggestions := mergeSuggestions(generateSuggestions(cityModel, fuel,
				opts.Now, opts.Location, forecastPredictDays, suggestLimitPerDay))
			var rows []notifyRow
			for i := range suggestions {
				if suggestions[i].Confidence == "medium" || suggestions[i].Confidence == "high" {
					rows = append(rows, notifyRow{suggest: &suggestions[i]})
				}
			}
			if len(rows) == 0 {
				continue
			}
			byCity = append(byCity, citySuggestRows{city: city, rows: rows})
		}
		byFuel[fuel] = byCity
	}
	return byFuel
}

// userSuggestRows flattens the shared per-city rows down to one user's city
// selection, sorted by date, start time, and station name like the watcher
// does.
func userSuggestRows(citySuggests []citySuggestRows, cities map[string]bool) []notifyRow {
	var rows []notifyRow
	for _, ts := range citySuggests {
		if !citySelected(cities, ts.city.Targets) {
			continue
		}
		rows = append(rows, ts.rows...)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i].suggest, rows[j].suggest
		if a.Date != b.Date {
			return a.Date < b.Date
		}
		if a.StartTime != b.StartTime {
			return a.StartTime < b.StartTime
		}
		return a.StationName < b.StationName
	})
	return rows
}

func cheapestSuggestRow(rows []notifyRow) notifyRow {
	cheapest := rows[0]
	for _, row := range rows[1:] {
		if row.suggest.PredictedPrice < cheapest.suggest.PredictedPrice {
			cheapest = row
		}
	}
	return cheapest
}
