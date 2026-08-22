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
	// The notification subscription: a point and a radius around it, chosen by
	// the user and independent of the admin's update targets. NotifyCity is the
	// label the coordinates were resolved from, for display only.
	NotifyCity     string
	NotifyLat      float64
	NotifyLng      float64
	NotifyRadiusKM float64
}

// subscription is the area a user is notified about.
func (u notifyUser) subscription() subscription {
	return subscription{Lat: u.NotifyLat, Lng: u.NotifyLng, RadiusKM: u.NotifyRadiusKM}
}

// subscription is a notification area: every station within RadiusKM of the
// point. Distances reported to the user are measured from it, so it is also
// what makes the {{distance}} placeholder mean "how far from me".
type subscription struct {
	Lat      float64
	Lng      float64
	RadiusKM float64
}

// valid reports whether the subscription describes an area. A user without one
// cannot be served: there is no longer an "everywhere" default, because the
// area is the whole of what they asked for.
func (s subscription) valid() bool { return s.RadiusKM > 0 }

// baselineKey names the running cheapest price for one user, fuel and area.
//
// The area is part of the key because the baseline answers "what is the cheapest
// I have seen here today": moving to a different place asks a different question,
// and inheriting the old area's minimum would suppress alerts in the new one
// until the next reset. Keys for areas nobody uses any more are removed by the
// daily reset, which deletes the whole check_baseline prefix.
func (s subscription) baselineKey(userID int64, fuel string) string {
	return fmt.Sprintf("check_baseline:%d:%s:%.4f,%.4f,%.1f", userID, fuel, s.Lat, s.Lng, s.RadiusKM)
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
	// Stations is how many stations were in scope: everything still being fed.
	Stations      int                `json:"stations"`
	Users         int                `json:"users"`
	CheckRows     int                `json:"check_rows"`
	SuggestRows   int                `json:"suggest_rows"`
	BaselineReset bool               `json:"baseline_reset"`
	DryRun        bool               `json:"dry_run"`
	Sent          []notifySendRecord `json:"sent"`
	Failed        []notifySendRecord `json:"failed"`
	DBPath        string             `json:"db_path"`
}

func runNotify(args []string) (err error) {
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

	// A dry run writes nothing and sends nothing, so it is not a delivery and
	// does not belong in the delivery statistics: recording it would mix
	// operator rehearsals into the counts the Statistics page reports.
	var stats *commandRun
	if !*dryRun {
		stats = beginCommandRun(ctx, db, "notify")
		defer func() { stats.finish(ctx, err) }()
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

	stats.set("stations", float64(result.Stations))
	stats.set("users", float64(result.Users))
	stats.set("check_rows", float64(result.CheckRows))
	stats.set("suggest_rows", float64(result.SuggestRows))
	stats.set("sent", float64(len(result.Sent)))
	stats.set("failed", float64(len(result.Failed)))
	stats.setBool("baseline_reset", result.BaselineReset)
	// Some recipients got their notification and some did not: the command
	// only returns an error when every send failed.
	if len(result.Failed) > 0 && len(result.Sent) > 0 {
		stats.markPartial()
	}

	// Every send failed. That is the command's result whichever way it prints,
	// so build it before the output branch: computing it only on the text path
	// left JSON runs exiting 0 and recorded as 'ok' on a run that delivered
	// nothing.
	var sendErr error
	if len(result.Failed) > 0 && len(result.Sent) == 0 {
		sendErr = fmt.Errorf("all %d notification sends failed", len(result.Failed))
	}

	if output == outputJSON {
		if err := writeJSON(result); err != nil {
			return err
		}
		return sendErr
	}
	printNotifyResultText(result)
	return sendErr
}

func printNotifyResultText(result notifyResult) {
	fmt.Fprintf(stdout, "stations: %d, users: %d, check rows: %d, suggest rows: %d\n",
		result.Stations, result.Users, result.CheckRows, result.SuggestRows)
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
			notify_suggest_enabled, notify_last_suggest, notify_fuel,
			notify_city, notify_lat, notify_lng, notify_radius_km
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
			&u.LastSuggest, &u.NotifyFuel,
			&u.NotifyCity, &u.NotifyLat, &u.NotifyLng, &u.NotifyRadiusKM); err != nil {
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
	users, err := loadNotifyUsers(ctx, db)
	if err != nil {
		return result, err
	}
	result.Users = len(users)
	if len(users) == 0 {
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
		if !u.CheckEnabled && !u.SuggestEnabled {
			// Both kinds off is how a user pauses everything; nothing to do and
			// nothing to warn about.
			continue
		}
		if !u.subscription().valid() {
			// A subscription is an area, and there is no "everywhere" default:
			// without one there is nothing to send. Reported rather than
			// silently dropped, because this user does want notifications and
			// would otherwise wonder why none arrive.
			fmt.Fprintf(os.Stderr,
				"warning: skipping %s: no notification location set (My Account -> Notifications)\n", u.Email)
			continue
		}
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
	// Nothing here consults the cities cache or the update targets: the station
	// universe is whatever is being fed, and who receives which station is
	// decided per user by their own subscription area.
	var (
		checksByFuel   map[string]fuelChecks
		suggestsByFuel map[string]map[subscription][]notifyRow
	)
	if len(checkUsers) > 0 || len(suggestUsers) > 0 {
		scan, err := loadSnapshotScan(ctx, db, opts.Now.AddDate(0, 0, -modelHistoryDays), opts.Now)
		if err != nil {
			return result, err
		}
		result.Stations = len(scan.Stations)
		if len(checkUsers) > 0 {
			checksByFuel = collectChecks(ctx, db, scan, opts)
		}
		if len(suggestUsers) > 0 {
			suggestsByFuel = collectSuggestions(ctx, db, scan, distinctSubscriptions(suggestUsers), opts)
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
			userRows, userBaselines, err := userCheckRows(ctx, db, fuel, checksByFuel[fuel], u)
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
			userRows := userSuggestRows(suggestsByFuel[fuel], u)
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

// fuelChecks is one fuel's deliverable price check over every station currently
// being fed: buy recommendations with medium/high confidence, unfiltered and
// unsorted. It is computed once per run and narrowed to each subscriber's area
// afterwards.
type fuelChecks struct {
	rows []priceCheckRow
}

// collectChecks runs the check once per fuel over every station currently being
// fed and keeps the rows worth delivering. Which of them a given user sees is a
// question of their own subscription area, applied per user in userCheckRows, so
// nothing here is city-scoped.
//
// A fuel whose model has too little data is skipped with a warning rather than
// failing the whole run.
func collectChecks(ctx context.Context, db *sql.DB, scan snapshotScan, opts notifyOptions) map[string]fuelChecks {
	byFuel := map[string]fuelChecks{}
	for _, fuel := range suggestFuels {
		// Limit 0: the row limit is a per-subscriber concern, applied after the
		// area filter so a distant city cannot crowd out a user's own stations.
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
		var matching []priceCheckRow
		for _, row := range checks {
			if row.Recommendation == "buy" && (row.Confidence == "medium" || row.Confidence == "high") {
				matching = append(matching, row)
			}
		}
		byFuel[fuel] = fuelChecks{rows: matching}
	}
	return byFuel
}

// userCheckRows narrows one fuel's check rows to a user's subscription area and
// then to the prices strictly cheaper than that user's running minimum, sorted
// cheapest-first. Returns the rows to send plus the baseline update to persist
// after a successful delivery.
//
// The baseline is per user, fuel and area (see subscription.baselineKey): a
// subscription is one area, so it tracks one price series, and moving the area
// starts a new one.
func userCheckRows(ctx context.Context, db *sql.DB, fuel string, checks fuelChecks, u notifyUser) ([]notifyRow, map[string]string, error) {
	sub := u.subscription()
	if !sub.valid() {
		return nil, nil, nil
	}
	inRange := rowsWithinSubscription(checks.rows, sub)
	if len(inRange) == 0 {
		return nil, nil, nil
	}
	sort.SliceStable(inRange, func(i, j int) bool {
		return inRange[i].CurrentPrice < inRange[j].CurrentPrice
	})

	baselineKey := sub.baselineKey(u.ID, fuel)
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
	for i := range inRange {
		if !hasBaseline || inRange[i].CurrentPrice < baseline {
			cheaper = append(cheaper, inRange[i])
		}
	}
	if len(cheaper) == 0 {
		return nil, nil, nil
	}
	if len(cheaper) > checkRowLimit {
		cheaper = cheaper[:checkRowLimit]
	}
	rows := make([]notifyRow, 0, len(cheaper))
	for i := range cheaper {
		rows = append(rows, notifyRow{check: &cheaper[i]})
	}
	return rows, map[string]string{
		baselineKey: strconv.FormatFloat(cheaper[0].CurrentPrice, 'f', -1, 64),
	}, nil
}

// rowsWithinSubscription keeps the rows inside a subscription's area and
// restates each one's distance as the distance from that subscriber.
func rowsWithinSubscription(rows []priceCheckRow, sub subscription) []priceCheckRow {
	var out []priceCheckRow
	for _, row := range rows {
		distance := haversineKM(sub.Lat, sub.Lng, row.Station.Lat, row.Station.Lng)
		if distance > sub.RadiusKM {
			continue
		}
		row.DistanceKM = roundTo(distance, 1)
		row.Station.DistanceKM = row.DistanceKM
		out = append(out, row)
	}
	return out
}

// collectSuggestions builds the forecast model once per fuel over every station
// currently being fed. Windows are picked per subscription rather than globally,
// because the per-day limit is a delivery concern: one subscriber's cheap
// stations must not use up the slots of an area someone else asked about.
//
// Subscriptions are deduplicated, so users who asked about the same area share
// one candidate pass. The model itself is built once and only filtered (see
// forecastModel.withinRadius), so this costs candidate scoring, not another pass
// over the history.
func collectSuggestions(ctx context.Context, db *sql.DB, scan snapshotScan, subs []subscription, opts notifyOptions) map[string]map[subscription][]notifyRow {
	byFuel := map[string]map[subscription][]notifyRow{}
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
		bySub := map[subscription][]notifyRow{}
		for _, sub := range subs {
			areaModel := model.withinRadius(sub.Lat, sub.Lng, sub.RadiusKM)
			if len(areaModel.Stations) == 0 {
				continue
			}
			suggestions := mergeSuggestions(generateSuggestions(areaModel, fuel,
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
			bySub[sub] = rows
		}
		byFuel[fuel] = bySub
	}
	return byFuel
}

// distinctSubscriptions returns each area exactly once, so a candidate pass is
// shared by every user who asked about the same place.
func distinctSubscriptions(users []notifyUser) []subscription {
	var subs []subscription
	seen := map[subscription]bool{}
	for _, u := range users {
		sub := u.subscription()
		if !sub.valid() || seen[sub] {
			continue
		}
		seen[sub] = true
		subs = append(subs, sub)
	}
	return subs
}

// userSuggestRows returns one user's suggestion rows, sorted by date, start
// time, and station name like the watcher does.
func userSuggestRows(bySub map[subscription][]notifyRow, u notifyUser) []notifyRow {
	sub := u.subscription()
	if !sub.valid() {
		return nil
	}
	rows := append([]notifyRow(nil), bySub[sub]...)
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
