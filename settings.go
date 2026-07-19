package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Canonical admin settings keys stored in the settings table. The seeded
// values mirror today's hardcoded CLI defaults, so DB-driven configuration is
// behavior-preserving until an admin edits it (via the web UI).
const (
	settingFuel            = "fuel"
	settingRangeKM         = "range_km"
	settingHistoryDays     = "history_days"
	settingPredictDays     = "predict_days"
	settingLimitPerDay     = "limit_per_day"
	settingCheckLimit      = "check_limit"
	settingSuggestTimes    = "suggest_times"
	settingCheckResetTime  = "check_reset_time"
	settingNotifyDays      = "notify_days"
	settingNotifyWindows   = "notify_windows"
	settingCheckTemplate   = "check_template"
	settingSuggestTemplate = "suggest_template"

	// Notification title templates. Empty means: fall back to the user's
	// pushover_app_name, preserving pre-existing behavior.
	settingCheckTitleTemplate   = "check_title_template"
	settingSuggestTitleTemplate = "suggest_title_template"
)

// Default row templates, identical to gasoline-watch.sh's CHECK_ROW_TEMPLATE
// and SUGGEST_ROW_TEMPLATE so both notification paths speak the same language.
const (
	defaultCheckTemplate   = "Buy {{fuel}} at {{station_name}} ({{distance}} km): {{current_price}} EUR, confidence {{confidence}}, verdict {{verdict}}"
	defaultSuggestTemplate = "{{date}} {{start_time}}-{{end_time}} {{fuel}} at {{station_name}} ({{distance}} km): predicted {{predicted_price}} EUR, confidence {{confidence}}"
)

const (
	defaultNotifyDays    = "mon,tue,wed,thu,fri,sat,sun"
	defaultNotifyWindows = "07:00-21:00"
	defaultSuggestTimes  = "08:00,13:00"
)

// appSettings is the admin configuration that drives update/suggest/check
// defaults and the notify command.
type appSettings struct {
	Fuel                 string
	RangeKM              float64
	HistoryDays          int
	PredictDays          int
	LimitPerDay          int
	CheckLimit           int
	SuggestTimes         string
	CheckResetTime       string
	NotifyDays           string
	NotifyWindows        string
	CheckTemplate        string
	SuggestTemplate      string
	CheckTitleTemplate   string
	SuggestTitleTemplate string
}

// defaultAppSettings matches the hardcoded flag defaults of suggest/check.
func defaultAppSettings() appSettings {
	return appSettings{
		Fuel:            "diesel",
		RangeKM:         5,
		HistoryDays:     30,
		PredictDays:     3,
		LimitPerDay:     3,
		CheckLimit:      5,
		SuggestTimes:    defaultSuggestTimes,
		CheckResetTime:  "00:00",
		NotifyDays:      defaultNotifyDays,
		NotifyWindows:   defaultNotifyWindows,
		CheckTemplate:   defaultCheckTemplate,
		SuggestTemplate: defaultSuggestTemplate,
	}
}

// seededSettings returns the name/value pairs the migration inserts when
// missing. Order is stable for deterministic migrations.
func seededSettings() [][2]string {
	d := defaultAppSettings()
	return [][2]string{
		{settingFuel, d.Fuel},
		{settingRangeKM, strconv.FormatFloat(d.RangeKM, 'f', -1, 64)},
		{settingHistoryDays, strconv.Itoa(d.HistoryDays)},
		{settingPredictDays, strconv.Itoa(d.PredictDays)},
		{settingLimitPerDay, strconv.Itoa(d.LimitPerDay)},
		{settingCheckLimit, strconv.Itoa(d.CheckLimit)},
		{settingSuggestTimes, d.SuggestTimes},
		{settingCheckResetTime, d.CheckResetTime},
		{settingNotifyDays, d.NotifyDays},
		{settingNotifyWindows, d.NotifyWindows},
		{settingCheckTemplate, d.CheckTemplate},
		{settingSuggestTemplate, d.SuggestTemplate},
		{settingCheckTitleTemplate, d.CheckTitleTemplate},
		{settingSuggestTitleTemplate, d.SuggestTitleTemplate},
	}
}

// migrateSeedDefaultSettings inserts any missing default settings rows. It
// never overwrites existing values, so admin edits survive re-runs.
func migrateSeedDefaultSettings(ctx context.Context, tx *sql.Tx, d dialect, result *migrateResult) error {
	insertSQL := kvInsertIgnoreSQL(d, "settings")
	now := time.Now().UTC().Format(time.RFC3339)
	seeded := false
	for _, kv := range seededSettings() {
		res, err := tx.ExecContext(ctx, insertSQL, kv[0], kv[1], now)
		if err != nil {
			return fmt.Errorf("seed setting %s: %w", kv[0], err)
		}
		if n, err := res.RowsAffected(); err == nil && n > 0 {
			seeded = true
		}
	}
	if seeded {
		result.Applied = append(result.Applied, "settings.seed_defaults")
	}
	return nil
}

type settingsQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// loadSettings overlays the settings table onto the built-in defaults, so an
// empty or partially filled table still yields today's behavior. Unknown rows
// are ignored; unparsable numeric values fail loudly, naming the setting.
func loadSettings(ctx context.Context, q settingsQuerier) (appSettings, error) {
	s := defaultAppSettings()
	rows, err := q.QueryContext(ctx, `SELECT name, value FROM settings`)
	if err != nil {
		return appSettings{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var name, value string
		if err := rows.Scan(&name, &value); err != nil {
			return appSettings{}, err
		}
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		switch name {
		case settingFuel:
			s.Fuel = value
		case settingRangeKM:
			f, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return appSettings{}, fmt.Errorf("invalid setting %s: %q is not a number", name, value)
			}
			s.RangeKM = f
		case settingHistoryDays, settingPredictDays, settingLimitPerDay, settingCheckLimit:
			n, err := strconv.Atoi(value)
			if err != nil {
				return appSettings{}, fmt.Errorf("invalid setting %s: %q is not an integer", name, value)
			}
			switch name {
			case settingHistoryDays:
				s.HistoryDays = n
			case settingPredictDays:
				s.PredictDays = n
			case settingLimitPerDay:
				s.LimitPerDay = n
			case settingCheckLimit:
				s.CheckLimit = n
			}
		case settingSuggestTimes:
			s.SuggestTimes = value
		case settingCheckResetTime:
			s.CheckResetTime = value
		case settingNotifyDays:
			s.NotifyDays = value
		case settingNotifyWindows:
			s.NotifyWindows = value
		// Templates are unescaped here (\n, \t, \\) so the single-line
		// settings fields can express multi-line notifications.
		case settingCheckTemplate:
			s.CheckTemplate = unescapeTemplate(value)
		case settingSuggestTemplate:
			s.SuggestTemplate = unescapeTemplate(value)
		case settingCheckTitleTemplate:
			s.CheckTitleTemplate = unescapeTemplate(value)
		case settingSuggestTitleTemplate:
			s.SuggestTitleTemplate = unescapeTemplate(value)
		}
	}
	return s, rows.Err()
}

// updateTarget is one city+radius pair updated automatically by
// `gasoline update` when no --city/--radius flags are given.
type updateTarget struct {
	City     string
	RadiusKM float64
}

func loadUpdateTargets(ctx context.Context, q settingsQuerier) ([]updateTarget, error) {
	rows, err := q.QueryContext(ctx, `SELECT city, radius_km FROM update_targets ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var targets []updateTarget
	for rows.Next() {
		var t updateTarget
		if err := rows.Scan(&t.City, &t.RadiusKM); err != nil {
			return nil, err
		}
		targets = append(targets, t)
	}
	return targets, rows.Err()
}

func getNotificationState(ctx context.Context, db *sql.DB, name string) (string, bool, error) {
	var value string
	err := db.QueryRowContext(ctx, `SELECT value FROM notification_state WHERE name = ?`, name).Scan(&value)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

func setNotificationState(ctx context.Context, db *sql.DB, d dialect, name, value string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.ExecContext(ctx, kvUpsertSQL(d, "notification_state"), name, value, now)
	return err
}

// clearCheckBaselines removes every per-fuel/city check baseline; run once per
// local day so the first cheaper price after the reset re-arms notifications.
func clearCheckBaselines(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `DELETE FROM notification_state WHERE name LIKE 'check_baseline:%'`)
	return err
}

// applySuggestSettings overlays DB-configured defaults onto suggest options
// for every flag the user did not set explicitly. Explicit flags always win.
func applySuggestSettings(ctx context.Context, db *sql.DB, fs *flag.FlagSet, opts *suggestOptions) error {
	s, err := loadSettings(ctx, db)
	if err != nil {
		return err
	}
	if !flagWasSet(fs, "fuel") {
		opts.Fuel = s.Fuel
	}
	if !flagWasSet(fs, "range-km") {
		opts.RangeKM = s.RangeKM
	}
	if !flagWasSet(fs, "history-days") {
		opts.HistoryDays = s.HistoryDays
	}
	if !flagWasSet(fs, "predict-days") {
		opts.PredictDays = s.PredictDays
	}
	if !flagWasSet(fs, "limit-per-day") {
		opts.LimitPerDay = s.LimitPerDay
	}
	if !flagWasSet(fs, "city") && strings.TrimSpace(opts.City) == "" {
		targets, err := loadUpdateTargets(ctx, db)
		if err != nil {
			return err
		}
		if len(targets) > 0 {
			opts.City = targets[0].City
		}
	}
	return nil
}

// applyCheckSettings is the check-command counterpart of applySuggestSettings.
func applyCheckSettings(ctx context.Context, db *sql.DB, fs *flag.FlagSet, opts *checkOptions) error {
	s, err := loadSettings(ctx, db)
	if err != nil {
		return err
	}
	if !flagWasSet(fs, "fuel") {
		opts.Fuel = s.Fuel
	}
	if !flagWasSet(fs, "range-km") {
		opts.RangeKM = s.RangeKM
	}
	if !flagWasSet(fs, "history-days") {
		opts.HistoryDays = s.HistoryDays
	}
	if !flagWasSet(fs, "predict-days") {
		opts.PredictDays = s.PredictDays
	}
	if !flagWasSet(fs, "limit") {
		opts.Limit = s.CheckLimit
	}
	if !flagWasSet(fs, "city") && strings.TrimSpace(opts.City) == "" {
		targets, err := loadUpdateTargets(ctx, db)
		if err != nil {
			return err
		}
		if len(targets) > 0 {
			opts.City = targets[0].City
		}
	}
	return nil
}
