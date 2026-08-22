package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Canonical admin settings keys stored in the settings table. Only the
// notification texts are configurable: they are the one piece of operational
// configuration with no universal answer. Everything the suggest/check models
// need is a constant below.
const (
	settingCheckTemplate   = "check_template"
	settingSuggestTemplate = "suggest_template"

	// Notification title templates. Empty means: fall back to the user's
	// pushover_app_name, preserving pre-existing behavior.
	settingCheckTitleTemplate   = "check_title_template"
	settingSuggestTitleTemplate = "suggest_title_template"
)

// obsoleteSettings are keys earlier versions stored in the settings table and
// that no longer have any effect: the station scope is global, every fuel is
// always computed, the model parameters and delivery limits are constants, and
// the per-user schedule fields carry their own defaults. `migrate` deletes
// them so the table stops advertising configuration that does nothing.
var obsoleteSettings = []string{
	"fuel",
	"range_km",
	"history_days",
	"predict_days",
	"limit_per_day",
	"check_limit",
	"suggest_times",
	"check_reset_time",
	"notify_days",
	"notify_windows",
	"check_delta_mode",
	"check_delta_fraction",
}

// Model and delivery parameters. Each of these was an admin setting; none of
// them has a per-install answer, so they are constants with one place to change.
const (
	// modelHistoryDays is the training window handed to the forecast model.
	modelHistoryDays = 30
	// forecastPredictDays is how many calendar days ahead suggestions and the
	// "a cheaper window is coming" veto look, including today.
	forecastPredictDays = 3
	// suggestLimitPerDay caps how many suggestions one day contributes to a
	// notification, and checkRowLimit caps the check rows in one message.
	// Both are delivery concerns: the persisted measurement path deliberately
	// keeps every station (see persistCheckDecisions).
	suggestLimitPerDay = 3
	checkRowLimit      = 5
	// checkBaselineResetTime is the local time at which notify clears the
	// per-user price baselines, so the first cheaper price after it re-arms
	// buy-now alerts.
	checkBaselineResetTime = "00:00"
	// stationFreshness bounds the station universe: a station is in scope for
	// as long as it still receives price updates. This is what excludes
	// stations that stopped being fed when an update target was removed or
	// its radius shrank, without needing a radius at computation time.
	stationFreshness = 48 * time.Hour
	// commandRunRetentionDays is how long recorded command runs are kept.
	// `gasoline compact` does the pruning: the recording commands run on
	// minute-scale timers and should not each pay for a sweep, and compact is
	// already the housekeeping pass.
	commandRunRetentionDays = 30
)

const (
	// defaultCheckDelta is the margin, in euro, used by the low/high verdict
	// and by the "a cheaper window is coming" veto.
	defaultCheckDelta = 0.020
)

// Default row templates, identical to gasoline-watch.sh's CHECK_ROW_TEMPLATE
// and SUGGEST_ROW_TEMPLATE so both notification paths speak the same language.
const (
	defaultCheckTemplate   = "Buy {{fuel}} at {{station_name}} ({{distance}} km): {{current_price}} EUR, confidence {{confidence}}, verdict {{verdict}}"
	defaultSuggestTemplate = "{{date}} {{start_time}}-{{end_time}} {{fuel}} at {{station_name}} ({{distance}} km): predicted {{predicted_price}} EUR, confidence {{confidence}}"
)

// Fallbacks for the per-user notification schedule. Every user edits their own
// values in the web UI (My Account → Notifications); these apply while a field
// is still blank.
const (
	defaultNotifyDays    = "mon,tue,wed,thu,fri,sat,sun"
	defaultNotifyWindows = "07:00-21:00"
	defaultSuggestTimes  = "08:00,13:00"
)

// appSettings is the admin configuration that drives the notify command: the
// notification texts, and nothing else.
type appSettings struct {
	CheckTemplate        string
	SuggestTemplate      string
	CheckTitleTemplate   string
	SuggestTitleTemplate string
}

// defaultAppSettings matches the templates gasoline-watch.sh ships with.
func defaultAppSettings() appSettings {
	return appSettings{
		CheckTemplate:   defaultCheckTemplate,
		SuggestTemplate: defaultSuggestTemplate,
	}
}

// seededSettings returns the name/value pairs the migration inserts when
// missing. Order is stable for deterministic migrations.
func seededSettings() [][2]string {
	d := defaultAppSettings()
	return [][2]string{
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

// migrateDropObsoleteSettings removes settings rows that no longer have any
// effect. loadSettings already ignores unknown names, so this is housekeeping:
// it keeps the table from presenting dead configuration to whoever reads it
// next.
func migrateDropObsoleteSettings(ctx context.Context, tx *sql.Tx, d dialect, result *migrateResult) error {
	dropped := false
	for _, name := range obsoleteSettings {
		res, err := tx.ExecContext(ctx, `DELETE FROM settings WHERE name = ?`, name)
		if err != nil {
			return fmt.Errorf("drop obsolete setting %s: %w", name, err)
		}
		if n, err := res.RowsAffected(); err == nil && n > 0 {
			dropped = true
		}
	}
	if dropped {
		result.Applied = append(result.Applied, "settings.drop_obsolete")
	}
	return nil
}

type settingsQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// loadSettings overlays the settings table onto the built-in defaults, so an
// empty or partially filled table still yields today's behavior. Unknown rows
// are ignored.
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
		// Templates are unescaped here (\n, \t, \\) so the single-line
		// settings fields can express multi-line notifications.
		switch name {
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
// `gasoline update` when no --city/--radius flags are given. The radius is the
// collection boundary — it decides which stations are fed — and is the only
// radius in the system: suggest, check and notify cover whatever was fed.
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
