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
	LastSuggest     string // YYYY-MM-DDTHH:MM of the last fired suggestion slot
}

type notifyOptions struct {
	Now      time.Time
	Location *time.Location
	DryRun   bool
	APIURL   string // "" -> pushoverMessagesURL
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
			notify_days, notify_windows, notify_suggest_times, notify_check_enabled, notify_last_suggest
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
		var checkEnabled int
		if err := rows.Scan(&u.ID, &u.Email, &u.PushoverAppName, &u.PushoverUserKey, &u.PushoverToken,
			&u.NotifyDays, &u.NotifyWindows, &u.SuggestTimes, &checkEnabled, &u.LastSuggest); err != nil {
			return nil, err
		}
		u.CheckEnabled = checkEnabled != 0
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
	resetMin, err := parseHHMM(settings.CheckResetTime)
	if err != nil {
		return result, fmt.Errorf("invalid setting %s: %v", settingCheckResetTime, err)
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
			days = settings.NotifyDays
		}
		windows := u.NotifyWindows
		if strings.TrimSpace(windows) == "" {
			windows = settings.NotifyWindows
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
		timesSpec := u.SuggestTimes
		if strings.TrimSpace(timesSpec) == "" {
			timesSpec = settings.SuggestTimes
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

	// Check phase: the price data is computed once per target, but the
	// cheaper-than-baseline filter runs per user against per-user baseline
	// keys, and a user's baselines advance only after their own delivery
	// succeeded. One user's schedule or a partial send failure therefore
	// never suppresses another user's notification, and failed sends are
	// retried on the next run.
	if len(checkUsers) > 0 {
		targetChecks, err := collectTargetChecks(ctx, db, settings, targets, opts)
		if err != nil {
			return result, err
		}
		for _, u := range checkUsers {
			userRows, userBaselines, err := userCheckRows(ctx, db, settings, targetChecks, u.ID)
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
				Title: title, Message: message,
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

	// Suggest phase: compute lazily once (identical options for all users),
	// deliver per user, and advance each user's slot marker.
	if len(suggestUsers) > 0 {
		suggestRows, err := collectSuggestRows(ctx, db, settings, targets, opts)
		if err != nil {
			return result, err
		}
		result.SuggestRows = len(suggestRows)
		message := ""
		var cheapest *notifyRow
		if len(suggestRows) > 0 {
			c := cheapestSuggestRow(suggestRows)
			cheapest = &c
			message = renderNotifyMessage(settings.SuggestTemplate, notifyKindSuggest, suggestRows, cheapest)
		}
		for _, u := range suggestUsers {
			marker := today + "T" + suggestSlots[u.ID]
			if len(suggestRows) == 0 {
				// Nothing to say: still advance the marker so the empty
				// result is not retried until the next slot.
				if !opts.DryRun {
					if err := setUserLastSuggest(ctx, db, u.ID, marker); err != nil {
						return result, err
					}
				}
				continue
			}
			rec := notifySendRecord{Email: u.Email, Kind: "suggest"}
			if opts.DryRun {
				result.Sent = append(result.Sent, rec)
				continue
			}
			if err := sendPushover(ctx, apiURL, pushoverMessage{
				Token: u.PushoverToken, UserKey: u.PushoverUserKey,
				Title:   notifyTitle(settings.SuggestTitleTemplate, notifyKindSuggest, cheapest, len(suggestRows), u.PushoverAppName),
				Message: message,
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

// targetCheckRows is the pre-filtered result of one update target's price
// check: buy recommendations with medium/high confidence, sorted cheapest
// first. It is computed once per run and shared by all users.
type targetCheckRows struct {
	target updateTarget
	rows   []priceCheckRow
}

// collectTargetChecks runs the check across all update targets and applies
// the user-independent part of the watcher's notification filter.
func collectTargetChecks(ctx context.Context, db *sql.DB, settings appSettings, targets []updateTarget, opts notifyOptions) ([]targetCheckRows, error) {
	var all []targetCheckRows
	for _, target := range targets {
		checks, err := checkGas(ctx, db, checkOptions{
			City:        target.City,
			RangeKM:     settings.RangeKM,
			Fuel:        settings.Fuel,
			HistoryDays: settings.HistoryDays,
			PredictDays: settings.PredictDays,
			Limit:       settings.CheckLimit,
			Now:         opts.Now,
			Location:    opts.Location,
		})
		if err != nil {
			// A stale or unknown city must not kill the whole run.
			fmt.Fprintf(os.Stderr, "warning: check for %s failed: %v\n", target.City, err)
			continue
		}
		var matching []priceCheckRow
		for _, row := range checks {
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
		all = append(all, targetCheckRows{target: target, rows: matching})
	}
	return all, nil
}

// userCheckRows filters the shared target rows against one user's baselines
// (check_baseline:<user_id>:<fuel>:<city>) and returns the rows strictly
// cheaper than that user's running minimum, sorted cheapest-first, plus the
// baseline updates to persist after a successful delivery to that user.
func userCheckRows(ctx context.Context, db *sql.DB, settings appSettings, targetChecks []targetCheckRows, userID int64) ([]notifyRow, map[string]string, error) {
	var rows []notifyRow
	baselines := map[string]string{}
	for _, tc := range targetChecks {
		baselineKey := fmt.Sprintf("check_baseline:%d:%s:%s", userID, settings.Fuel, tc.target.City)
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

// collectSuggestRows runs the suggestion across all update targets, keeps
// medium/high confidence rows, and sorts them by date, start time, and
// station name like the watcher does.
func collectSuggestRows(ctx context.Context, db *sql.DB, settings appSettings, targets []updateTarget, opts notifyOptions) ([]notifyRow, error) {
	var rows []notifyRow
	for _, target := range targets {
		suggestions, err := suggestGas(ctx, db, suggestOptions{
			City:        target.City,
			RangeKM:     settings.RangeKM,
			Fuel:        settings.Fuel,
			HistoryDays: settings.HistoryDays,
			PredictDays: settings.PredictDays,
			LimitPerDay: settings.LimitPerDay,
			Now:         opts.Now,
			Location:    opts.Location,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: suggest for %s failed: %v\n", target.City, err)
			continue
		}
		for i := range suggestions {
			if suggestions[i].Confidence == "medium" || suggestions[i].Confidence == "high" {
				rows = append(rows, notifyRow{suggest: &suggestions[i]})
			}
		}
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
	return rows, nil
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
