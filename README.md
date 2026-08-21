# gasoline

Small Go CLI that stores Tankerkönig gas station prices historically in SQLite or an external MySQL server and ships with a lightweight, login-protected PHP viewer for browsing the collected data, managing users, and configuring per-user Pushover notifications.

## Requirements

- Go 1.24+
- A Tankerkönig API key
- `jq` for the optional watcher script
- PHP with SQLite (or MySQL) support if you want to use the web viewer
- Optionally a MySQL 8.0+ (or MariaDB 10.5+) server if you don't want the local SQLite file

## Configuration

The CLI reads the Tankerkönig API key from `TANKER_KOENIG_API_KEY`. If that variable is unset, it falls back to a local `.env` file in the repo root.

### SQLite (default)

The SQLite database path defaults to `gasoline.db`. You can override it with either:

- `GASOLINE_DB_PATH`
- `--db /path/to/file.db`

### MySQL

Every command can store its data on an external MySQL server instead of the local SQLite file. Select the driver with `--db-driver mysql` or `GASOLINE_DB_DRIVER=mysql`, then provide the connection settings either as one DSN or as individual values. Each setting can come from a command-line flag, the environment, or the `.env` file (flag beats environment beats `.env`):

| Flag | Environment / `.env` | Default |
| --- | --- | --- |
| `--db-driver` | `GASOLINE_DB_DRIVER` | `sqlite` |
| `--mysql-dsn` | `GASOLINE_MYSQL_DSN` | — |
| `--mysql-host` | `GASOLINE_MYSQL_HOST` | `127.0.0.1` |
| `--mysql-port` | `GASOLINE_MYSQL_PORT` | `3306` |
| `--mysql-user` | `GASOLINE_MYSQL_USER` | — (required) |
| `--mysql-password` | `GASOLINE_MYSQL_PASSWORD` | empty |
| `--mysql-database` | `GASOLINE_MYSQL_DATABASE` | — (required) |
| `--mysql-tls` | `GASOLINE_MYSQL_TLS` | — |

The DSN uses the [go-sql-driver format](https://github.com/go-sql-driver/mysql#dsn-data-source-name) and must include a database name, e.g. `user:pass@tcp(db.example.com:3306)/gasoline`. Passing `--mysql-dsn` on the command line implies `--db-driver mysql`. When a DSN comes from `GASOLINE_MYSQL_DSN`, individual `--mysql-*` flags still override the matching part of it (e.g. `--mysql-host` retargets the host while keeping the DSN's credentials). The database itself must already exist; all tables and indexes are created automatically on first use.

`--mysql-tls` controls transport encryption and accepts `true` (encrypt and verify the server certificate), `skip-verify` (encrypt without verifying the certificate, e.g. self-signed certs), `preferred` (encrypt only if the server offers it), or `false` (plaintext). It applies whether you configure MySQL via `--mysql-dsn` or the individual `--mysql-*` values, and overrides any `tls=` already present in a DSN. Set it when the server rejects plaintext connections — for example a ProxySQL frontend returning `Error 1045 (28000): ... SSL is required`.

Example `.env` for a fully MySQL-backed setup:

```dotenv
TANKER_KOENIG_API_KEY=your-key
GASOLINE_DB_DRIVER=mysql
GASOLINE_MYSQL_HOST=db.example.com
GASOLINE_MYSQL_USER=gasoline
GASOLINE_MYSQL_PASSWORD=secret
GASOLINE_MYSQL_DATABASE=gasoline
# GASOLINE_MYSQL_TLS=skip-verify   # uncomment when the server requires SSL
```

### Web UI & notification settings

The PHP viewer requires a login (see [Web viewer & user accounts](#web-viewer--user-accounts)) and reads these additional variables from the web server's environment:

| Environment | Purpose |
| --- | --- |
| `GASOLINE_ADMIN_EMAIL` | Initial administrator: registering with exactly this email creates an approved admin account immediately. |
| `GASOLINE_BASE_URL` | Absolute base URL of the viewer, used for links in emails (derived from the request when unset). `gasoline notify` also reads it (environment or `.env`) and attaches it as the link of every Pushover notification. |
| `GASOLINE_SMTP_HOST` | SMTP relay for registration/approval emails. When unset, emails are skipped (logged) and the flows still work. |
| `GASOLINE_SMTP_PORT` | SMTP port (default `587`). |
| `GASOLINE_SMTP_USER` / `GASOLINE_SMTP_PASSWORD` | SMTP credentials (AUTH LOGIN/PLAIN); leave the user empty for an unauthenticated relay. |
| `GASOLINE_SMTP_FROM` | Sender address. |
| `GASOLINE_SMTP_TLS` | `starttls` (default for port 587), `implicit` (SMTPS, default for 465), or `none`. |
| `GASOLINE_SESSION_DAYS` | How long a signed-in browser stays signed in without retyping the password (default `30`, clamped to 1–365). |
| `GASOLINE_SESSION_PATH` | Directory for PHP session files (default `<temp>/gasoline-sessions`). Falls back to PHP's own path when it cannot be created or written. |

### Migrating an existing SQLite database to MySQL

`migrate-to-mysql` copies all cities, stations, price snapshots, and persisted predictions from a SQLite file into a MySQL server (creating the tables if needed). Snapshot and prediction ids are preserved, so history ordering and run references stay identical:

```bash
gasoline migrate-to-mysql --db gasoline.db \
  --mysql-host db.example.com --mysql-user gasoline \
  --mysql-password secret --mysql-database gasoline
```

Add `--mysql-tls skip-verify` (or `true`) if the target server requires SSL.

The command refuses to write into a MySQL database that already contains data; add `--overwrite` to replace the existing rows. The copy runs in a single transaction, so an interrupted migration leaves the target unchanged. After migrating, point the CLI (and viewer) at MySQL as shown above.

## Setup

Install dependencies:

```bash
go mod tidy
```

Build the binary:

```bash
make build
```

Run tests:

```bash
make test
```

Install the CLI to `/usr/local/bin/gasoline` and the PHP viewer to `/var/www/html/gasoline`:

```bash
sudo make install
```

You can override those install paths with `BINDIR` and `WEB_INSTALL_DIR`.

## CLI Usage

Show help:

```bash
gasoline help
```

Fetch the current station list for a city and persist snapshots:

```bash
gasoline update --city "Berlin, Germany" --radius 5
```

Useful `update` flags:

- `--fuel all|diesel|e5|e10`
- `--sort dist|price`
- `--user-agent "your-app/1.0"`
- `--output json` or `-o json`

`--city` is repeatable, and cities with overlapping radii are handled as one sweep: every target is fetched first, then a station reported by more than one of them is stored **once**, owned by the target whose centre is nearest. The prices stored are the freshest ones seen in that sweep, even if a farther target observed them — targets are fetched one after another, so a price can change mid-sweep. Per-city output reports both `fetched_count` (what the API returned) and `stored_count` (what that city wrote); the text output notes when a target lost stations to a nearer one. This keeps a shared station from defeating snapshot compaction — without it, overlapping targets add a row per city on every run even when prices never change.

Ownership is compared against the city that already owns a station, not only against the targets in the current run, so a station stays with its nearest city when you update a single city, when a nearer target's fetch fails, or when cities are updated in separate invocations. It moves only when a strictly nearer city fetches it, or when the owning city is no longer cached.

Compact existing snapshots in place:

```bash
gasoline compact
```

Run this once after upgrading if you have been updating cities with overlapping radii: earlier versions stored a row per city for every shared station on every run, and `compact` collapses those into the single row the current `update` maintains.

List cached cities:

```bash
gasoline list cities
```

Bulk-import city names from GeoNames for a country:

```bash
gasoline import cities DE
```

Clear the cached city table:

```bash
gasoline clear cities
```

List known stations and their latest stored prices:

```bash
gasoline list stations --city "Berlin" --limit 20
```

Show historical prices, optionally filtered to one station:

```bash
gasoline list history --fuel diesel --limit 0
gasoline list history --station-id 474e5046-deaf-4f9b-9a32-9797b778f047 --fuel diesel --limit 100
```

Suggest cheap fueling windows for the coming days:

```bash
gasoline suggest
```

`suggest` takes no scope flags: it covers **every station currently being fed** — whatever the configured update targets collect — and computes all three fuels in one run. A station leaves scope once it stops receiving price updates for 48 hours, which is what happens when an update target is removed or its radius shrinks. Each station is attributed to the city that owns it (the nearest fed centre, recorded at collection time as the geocoder's normalized name), and the reported distance is measured to that centre. `notify` resolves each update target to the same normalized name before it filters, so a target added as `Berlin, Germany` still matches the snapshots recorded under `Berlin`.

The suggestion algorithm uses open historical prices, reconstructs compacted price intervals, and decomposes each station's history into a per-day price level plus an intraday pattern:

- It first infers the **daily jump hour** — the local hour where upward price moves concentrate across all stations (with the current German once-per-day-raise regulation that is typically 12:00). Nothing is hardcoded: if the regulation changes or disappears, the inferred anchor and the learned pattern follow the data.
- Each 24h window between jumps ("pricing day") gets a duration-weighted median **baseline**; the samples bucketed by local weekday and hour are stored as **offsets** from that baseline, so the once-per-day sawtooth is learned independently of the absolute price level.
- Future hours are ranked with a recency-weighted median forecast of those offsets, added on top of the **current level**, which is estimated from the data recorded since the last jump (de-shaped by the learned hourly pattern) — so a market-wide price shift (market moves, temporary tax cuts) is picked up within hours instead of being averaged away over weeks.
- Stations with fewer than three usable pricing days fall back to the previous absolute-price behavior.

Useful `suggest` flags:

- `--persist` store the full prediction grid, evaluate past predictions, and learn from the errors (see below)
- `--quiet` or `-q` suppress the suggestion output — store only; requires `--persist`
- `--output json` or `-o json`

Suggestion output includes the day, time window, predicted price, confidence, distance, and full persisted station metadata. JSON output keeps the existing top-level station fields and also includes a nested `station` object with address, brand, street, house number, post code, place, coordinates, and first/last seen timestamps.

Both commands fan out over the three fuels: the text output has one `fuel: <name>` section each, and the JSON output is an array of `{fuel, suggestions}` (`{fuel, checks}` for check) objects. A fuel without enough stored history carries an `error` string instead of results and does not stop the others; the exit code reports how many failed.

The model parameters and row limits are not configurable: 30 days of history, a 3-day forecast horizon, 3 suggestions per day and 5 check rows. `update_targets.radius_km` is the only radius in the system — it decides what gets collected, and everything downstream covers what was collected.

Check whether the latest stored prices are low right now:

```bash
gasoline check
```

The check command uses the same historical model as `suggest`, compares each open station's latest stored price with recent station history, and scans the coming forecast window for a lower expected price. It prints the station, current price, low/typical/high verdict, buy/wait/hold recommendation, confidence, and best lower future window when one is expected. Run `gasoline update` first when you need fresh current prices.

The reported `history_percentile` is regime-relative for stations with enough history: it ranks the current price within the station's intraday pattern (its position in the daily sawtooth), not among raw absolute prices — so a market-wide jump at the daily raise does not make every station read "high" for weeks.

Public holidays are kept out of the weekday model: a holiday does not price like its calendar weekday, so holiday days are excluded from the per-weekday buckets and a holiday target is scored from the station's hour-of-day and recent history instead. Only the nine nationwide German holidays are recognized — state-specific ones such as Fronleichnam or Reformationstag would require mapping each station to its Bundesland, which the station data does not provide.

The margin that decides "low", and that suppresses a buy when a cheaper window is coming, is a flat 2 ct for every station.

### Persistent predictions and learning (`suggest --persist`)

`gasoline suggest --persist` additionally stores the **full forecast grid** — every station and future hour it can score, with the printed suggestions flagged — plus a record of what the price check would decide right now, in three tables (`prediction_runs`, `price_predictions`, `price_check_decisions`, created automatically on first use). Run it on a timer (e.g. hourly, next to `gasoline update`); each run:

1. **Evaluates** stored predictions whose target hour has passed, filling in the actual price (the price in effect at the window midpoint) and the prediction error.
2. **Scores** logged check decisions whose pricing day has finished against the cheapest price that day actually offered, recording the day's floor, when it occurred, and the *regret* — how much more than the floor the price was at the moment of the decision.
3. **Learns** from the evaluated errors and applies the result to all new predictions — `suggest`, `check`, and `notify` all pick it up automatically, also without `--persist`. Every persisted prediction records the learned correction it carried (`applied_correction`), and training first reconstructs the **raw model error** from it — stored errors always measure the corrected prediction, and training on them directly would make each loop correct its own output. Rows persisted before this column existed (older model versions) are excluded, so after an upgrade the corrections rebuild from fresh evaluations within a day or two instead of training on errors of a different model. All inputs are winsorized at ±15 ct so outages cannot steer them, recency-weighted over the last 14 days:
   - an **hour-of-day correction grid** keyed by local target hour, lead bucket (0–1h / 1–6h / 6–24h, with longer leads reusing the 6–24h cell) and weekday vs. weekend/holiday, for the parts of the daily price curve the shape model systematically misses (at least 50 samples per cell, capped at ±8 ct);
   - a **per-station bias** from short-lead residuals after the grid (at least 5 samples, capped at ±3 ct);
   - an **empirical confidence** per station and lead bucket — the p80 absolute residual mapped onto high (≤2 ct) / medium (≤4 ct) / low, replacing the sample-count heuristic wherever at least 30 evaluated predictions exist (a >24h target is capped at medium, since the calibration was measured at shorter leads);
   - a **suggestion price correction**: picking the minimum predicted window across many candidates preferentially picks predictions that erred low, so the printed suggestion price runs optimistic; the measured median residual of past suggested windows (at least 30 samples, capped at ±5 ct) is added to displayed suggestion prices only — each run records the correction it displayed with (`prediction_runs.suggestion_bias`) so the dashboard's fill-up card quotes the same corrected prices as the notifier, while the persisted grid keeps storing the raw model so the measurement never feeds back on itself.
   The forecast additionally extrapolates a **damped baseline drift** (median day-over-day baseline move across stations of the last 7 pricing days, halved and capped at ±2 ct/day), so predictions crossing pricing-day boundaries no longer assume a perfectly flat market.
4. **Persists** the new grid; newer runs supersede older ones for the same target hour, older rows remain as learning history.
5. **Records** the check decisions taken against that same model: per open station the observed price, the model's reference price for the current hour, the history percentile, and the resulting verdict and recommendation.
6. **Prunes** predictions and decisions older than 30 days, and everything stored for stations that have **left scope** — a station with no price update for 48 hours, which is what happens when an update target is removed or its radius shrinks. Retention alone would keep a removed city in the accuracy statistics for 30 more days even though nothing recomputes it, and its remaining predictions can never be evaluated because evaluation needs a recorded actual price. The station row and its price snapshots stay, so a station that comes back is modelled from its own history again; only the measurement rows go, and 48 hours without an update means 48 consecutive failed sweeps, so a transient fetch failure cannot trigger it.

The recorded decisions exist because the notification path itself keeps no record: `notify` computes each verdict and discards it, so without this there is no way to tell whether the numbers that trigger low-price alerts are any good. They are a faithful **proxy**, not a delivery log — the same model and bias as `notify` uses, for every fuel `notify` delivers, but computed on the suggestion timer's schedule and before `notify` applies its row limit, per-user city selection, notification windows and repeat-suppression baseline. Read them as "what the model decided", not "what users received".

The normal suggestion output is unchanged; a one-line summary (`persist: stored N predictions, evaluated M, ..., pruned A/B by retention, C/D for E stations that left scope`) goes to stderr. Pass `--quiet` (or `-q`) to suppress the suggestion output entirely and only store — useful for timer runs whose stdout nobody reads. One invocation persists a run per fuel over every fed station, so all of them accrue evaluation data for the bias learning; per-fuel failures are reported on stderr and via the exit code. The accrued evaluation data also feeds the bias learning and is surfaced in the web UI on the admin **Prediction accuracy** page (hamburger menu → Prediction accuracy), which compares each past predicted price with the actual price recorded for that window — raw rows, accuracy statistics (MAE, bias, RMSE, share within ±1/±2 ct, per-confidence breakdown), breakdowns by lead time and by hour of day, the alert outcomes described above, and a predicted-vs-actual graph.

### Server-stored configuration (admin settings)

Administrators configure two things in the web UI (hamburger menu → Settings): the **update targets** (city + radius pairs) that decide which stations are collected, and the **notification texts**.

That is deliberately all of it. The station scope, the fuels, the model parameters and the delivery limits used to be settings and are now fixed, because none of them had a per-install answer:

- `gasoline update` invoked **without any** `--city`/`--radius` flags updates every configured update target with its per-target radius, as a single de-duplicated sweep: targets whose radii overlap share stations, and each shared station is stored once under its nearest target. Passing explicit flags ignores the targets entirely. `radius_km` is the only radius in the system.
- `gasoline suggest`, `gasoline check` and `notify` take no scope or fuel arguments. They cover every station still being fed and compute all three fuels, so nothing that gets delivered goes unmeasured. Each user picks the one fuel they are notified about (see below).
- The fixed parameters are 30 days of history, a 3-day forecast horizon, 3 suggestions per day, 5 check rows, a flat 2 ct price margin, a 48-hour station freshness window, and a baseline reset at local midnight. The per-user notification schedule defaults (every day, 07:00–21:00, suggestions at 08:00 and 13:00) apply only until a user sets their own.

Upgrading to per-user notification areas: `migrate` carries a user's old city selection over whenever it says exactly what one area can express, using the legacy `range_km` setting as the radius — that is what the old notification path measured with, whereas a target's radius only ever decided what got collected. A single selected city becomes that city's centre at that range, and so does the old default of selecting nothing whenever there is exactly one update target, since "all cities" and "that city" are then the same area. Only genuinely ambiguous cases — several selected cities, or none with several targets — are left without an area and named on stderr as `needs_area`; those users receive nothing until they pick a city and radius, which is better than silently changing what they receive.

Run `gasoline migrate` once to create the tables and seed the notification templates. Seeding never overwrites existing rows, so admin edits survive re-runs. `migrate` also deletes the settings rows that older versions stored for the parameters listed above, so the table stops advertising configuration that no longer does anything.

`migrate` also backfills the covering index the admin **Prediction accuracy** page aggregates over (`idx_price_predictions_accuracy`); `gasoline doctor` reports whether it is present, see [Diagnosing a slow database](#diagnosing-a-slow-database-gasoline-doctor). On an install that has already accrued a large `price_predictions` table this is the one migration step that takes a noticeable while — tens of seconds per few million rows — and it grows the database by roughly the size of the table's own data. MySQL builds the index in place without blocking reads or writes, so a `suggest --persist` run may overlap it.

Send Pushover notifications to the web UI's users:

```bash
gasoline notify            # from cron or a systemd timer, e.g. every 5 minutes
gasoline notify --dry-run  # render and report what would be sent, write nothing
```

`notify` reads the notification templates, runs the check/suggest models once per fuel over every fed station, and delivers Pushover messages to every approved user who has configured a Pushover user key and API token in the web UI (My Account → Notifications). It needs no Tankerkönig API key — it only reads the database, so run `gasoline update` on a timer next to it. Per user it honors:

- the **notification schedule**: enabled weekdays and one or more time windows (default: every day, 07:00–21:00). Outside the schedule nothing is delivered.
- the **notification area**: a city and a radius around it (My Account → Notifications, stored as `users.notify_city` plus `notify_lat`/`notify_lng`/`notify_radius_km`). Notifications cover every tracked station within that distance, computed geometrically at run time, and the reported distance is measured from there — so `{{distance}}` means "how far from me". The area is the user's own: it has nothing to do with which cities the administrator collects, beyond the obvious fact that a station has to be collected before it can be notified about. Picking a city only resolves its coordinates; nothing consults the geocode cache afterwards. A user with no area set receives nothing, and `notify` says so on stderr rather than staying silent — unless they have both notification kinds off, which is how you pause everything. The buy-alert baseline is per user, fuel **and** area, so moving or resizing your area starts a fresh one instead of inheriting the old area's minimum for the rest of the day.
- the **fuel selection**: each user picks the single fuel they want to be notified about (My Account → Notifications, stored in `users.notify_fuel`). All three are always computed, so every choice is served.
- the **notification kinds**: two independent opt-ins (My Account → Notifications) select which of the two notification types the user receives — suggestions, buy-now alerts, both, or none. Suggestions are on by default (`users.notify_suggest_enabled`); buy-now alerts are opt-in (`users.notify_check_enabled`).
- the **daily suggestion times** (default 08:00 and 13:00): when suggestions are enabled, each slot fires one suggestion notification per day; missed slots collapse into one on the next run instead of bursting.
- the **buy-now alerts** opt-in: when enabled, check notifications fire only for buy recommendations with medium/high confidence that are strictly cheaper than the day's running baseline (reset daily at local midnight), mirroring `gasoline-watch.sh`.

The notification texts come from the admin-configured templates and support the full `gasoline-watch.sh` placeholder language — per-row placeholders such as `{{station_name}}`, `{{price}}`, `{{date}}`, `{{start_time}}`, all `*_formatted` variants (locale-aware decimal separator and weekday names), all `*_onchange` variants with window-aware time reprinting and line skipping, plus `{{count}}`, `{{cheapest_<field>}}`, and `{{message}}`. The only difference: the template renders directly into the Pushover message text instead of a shell command, so no quoting is involved. A literal `\n` (and `\t`) in a template is unescaped into a real line break (tab), so multi-line rows can be configured in the single-line settings fields; write `\\n` for a literal backslash-n. Message titles come from the admin-configured title templates (`check_title_template` / `suggest_title_template`), which support the same placeholder language rendered against the cheapest row; when no title template is set, the title falls back to each user's configured notification title.

Ready-to-use scheduling examples: `examples/systemd/gasoline-notify.service` + `gasoline-notify.timer` and `examples/cron/gasoline-notify.cron`.

Set a persistent display-name override for a station — useful when the Tankerkönig name is uninformative. Subsequent `update` runs keep the canonical name in sync but never touch the override, and every output path (CLI, JSON, PHP viewer, watcher notifications) prefers the override when set:

```bash
gasoline rename 474e5046-deaf-4f9b-9a32-9797b778f047 "Pumpe Ecke Bäckerstraße"
gasoline rename --clear 474e5046-deaf-4f9b-9a32-9797b778f047
```

Administrators can manage the same overrides in the web UI (hamburger menu → Stations).

Merge duplicate station identities — the Tankerkönig API sometimes lists the same physical station under several ids with identical prices, which multiplies every statistic and splits the learned corrections' samples across ids:

```bash
gasoline merge-stations --detect                 # list candidates (same coordinates or address)
gasoline merge-stations --into <canonical-id> <duplicate-id> [<duplicate-id>...]
```

A merge moves the duplicates' price history, predictions and check decisions onto the canonical station and marks the duplicates as aliases; future `update` sweeps record the aliased ids' prices under the canonical station automatically, so the merge is sticky even though the API keeps returning the old ids. Prediction and decision rows that duplicate one logical measurement after the rewrite — one run had scored several identities of the same station — are collapsed to a single row (the evaluated one survives), so statistics and learning stop multi-counting the station immediately rather than after the retention window. Run `gasoline compact` afterwards to collapse overlapping snapshots from the merged histories.

Run continuous buy/suggestion notifications:

```bash
./gasoline-watch.sh \
  --fuel diesel \
  --check-minutes 15 \
  --suggest-time 07:30 \
  --reset-time 00:00 \
  --check-command 'notify --message {{message}}' \
  --suggest-command 'notify --message {{message}}'
```

The watcher runs `gasoline check` every `--check-minutes` and `gasoline suggest` once per local day after `--suggest-time`. Both cover every station the update targets feed, so the watcher takes no city, radius or model parameters — `--fuel` only selects which fuel's rows it notifies about. Since those commands cover every fuel and exit non-zero when any of them lacks history, a non-zero exit does not stop the watcher: it notifies whenever its own fuel produced rows, and logs the reason only when that fuel is the one that failed. It sends only medium/high-confidence rows: check notifications require `recommendation=buy`; suggestion notifications include all medium/high-confidence suggestions. Rows are sorted ascending by price (current price for check, predicted price for suggest) so the first row is the cheapest. Command templates can use `{{message}}` for the full multiline message, row placeholders such as `{{price}}`, `{{fuel}}`, `{{station_name}}`, `{{distance}}`, `{{confidence}}`, `{{date}}`, `{{start_time}}`, `{{end_time}}`, scalar placeholders `{{cheapest_<field>}}` (sourced from the cheapest row, e.g. `{{cheapest_price}}`, `{{cheapest_station_name}}`), or `{{count}}` for the number of rows. Scalar placeholders substitute once, which makes them useful for non-repeating notification titles.

Each price placeholder has a `*_formatted` variant (`current_price_formatted`, `predicted_price_formatted`, `predicted_current_price_formatted`, `best_future_price_formatted`, `price_formatted`) that truncates after the second decimal without rounding (e.g. `1.685` → `1.68`, `1.7` → `1.70`). The decimal separator follows the active locale (`LC_ALL` / `LC_NUMERIC` / `LANG`), so e.g. `LANG=de_DE.UTF-8` renders `1,68` instead of `1.68`. The `fuel_formatted` variant capitalizes the first letter (`diesel` → `Diesel`, `e5` → `E5`).

Any row placeholder also has an `*_onchange` variant (including formatted ones, e.g. `{{fuel_onchange}}`, `{{date_onchange}}`, `{{weekday_short_formatted_onchange}}`) that renders the value on the first row but stays blank on later rows whenever it matches the previous row — handy for collapsing repeated dates or fuel labels in a multi-row notification. When a template line's only value-producing placeholders are `*_onchange` variants and they all blank out, that whole line is skipped instead of printing a line of leftover static characters or spaces. A line keeps printing as long as any placeholder on it produces a value — an `*_onchange` value that did change, or any regular `{{field}}` — and lines with no placeholders at all are always kept. Time-of-day placeholders (`start_time`, `end_time`, and the `best_future_start_time` / `best_future_end_time` variants) are window-aware: their `*_onchange` form reprints whenever the window it belongs to changes — the day it refers to, or the other boundary of the same window — even if the time itself is unchanged. So an identical `11:00 12:00` window still prints again under a new weekday, and a `09:00 10:00` window following `09:00 12:00` on the same day prints both times instead of a dangling ` 10:00`.

Check notifications track a single global lowest reported price for the configured fuel. A new check notification only fires for stations whose current price is strictly cheaper than that running baseline, and the baseline drops to the new minimum after each notification. `--reset-time HH:MM` (default `00:00`) clears the baseline once per local day, so the next check after the reset re-establishes the day's cheapest-price baseline.

An example systemd user service is available at `examples/systemd/gasoline-watch.service`, with its configuration in a companion `examples/systemd/gasoline.env`. The service reads all storage, API-key, and MySQL settings from that file via `EnvironmentFile=`, so switching between SQLite and MySQL is just an edit there — no unit changes. Set it up with:

```bash
# 1. Install the environment file and lock it down (it holds the API key and DB password).
sudo install -D -m 600 examples/systemd/gasoline.env /etc/gasoline/gasoline.env
sudo editor /etc/gasoline/gasoline.env        # fill in the API key; for MySQL, uncomment the block

# 2. Install the unit, adjust the command templates/paths, then enable it.
cp examples/systemd/gasoline-watch.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now gasoline-watch.service
```

The `EnvironmentFile=` path in the unit (`/etc/gasoline/gasoline.env`) must match where you installed the file. For a system-wide service under `/etc/systemd/system/` use `systemctl` without `--user`.

#### Watcher via cron (no systemd)

On hosts without systemd — or where you can't keep a long-running service alive — run the watcher from cron instead. Pass `--once` so it does a single pass and exits, and `--state-file PATH` so the price baseline and last-suggest date survive between runs (without it, every run would restart from a blank baseline and re-notify). In `--once` mode cron sets the cadence, so `--check-minutes` is ignored. A ready-to-use line is in `examples/cron/gasoline-watch.cron`:

```bash
mkdir -p /var/lib/gasoline                    # writable state dir for the cron user
crontab -e                                    # paste the line from examples/cron/gasoline-watch.cron
```

Edit the fuel and the `--check-command` / `--suggest-command` templates to taste, and confirm the paths to `gasoline`, `gasoline-watch`, and `/etc/gasoline/gasoline.env` match your install.

### Continuous updates with a timer

To keep prices fresh without the full watcher, run `gasoline update` on a schedule. A oneshot service plus a timer live at `examples/systemd/gasoline-update.service` and `examples/systemd/gasoline-update.timer`; the timer fires the service every 5 minutes:

```bash
# 1. Install the environment file (skip if already done for the watcher above).
sudo install -D -m 600 examples/systemd/gasoline.env /etc/gasoline/gasoline.env
sudo editor /etc/gasoline/gasoline.env        # fill in the API key; for MySQL, uncomment the block

# 2. Install the service + timer, then enable the timer.
cp examples/systemd/gasoline-update.service examples/systemd/gasoline-update.timer ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now gasoline-update.timer
```

The service runs `gasoline update --radius 25 --city 'Luebbecke'`; edit the `ExecStart` line to change city/radius. Check status and the next scheduled run with `systemctl --user list-timers gasoline-update.timer` and see past runs with `journalctl --user -u gasoline-update.service`. For a system-wide timer under `/etc/systemd/system/` use `systemctl` without `--user`.

Prefer cron? `examples/cron/gasoline-update.cron` holds a ready-to-use line — add it with `crontab -e`. Unlike systemd, cron starts with an empty environment, so the line sources the env file first (`set -a` exports every variable it defines).

Use `--limit 0` with `list stations` or `list history` to return all matching rows.

The grouped commands above are the canonical interface shown by `gasoline help`. The older top-level forms `cities`, `stations`, `history`, and `import-cities` are still accepted as aliases.

### Diagnosing a slow database (`gasoline doctor`)

`gasoline doctor` inspects a live database without changing it. It reports table sizes, every index with its key columns and on-disk size, which stations are in scope and why, and then — the reason it exists — reproduces the SQL behind one of the web pages, timing each query and showing what the planner did with it.

Which page it measures is a subcommand. Bare `doctor` measures the admin **Prediction accuracy** page, `doctor dashboard` the dashboard, and `doctor all` both:

```bash
gasoline doctor                                     # accuracy page, 14-day window, fuel diesel
gasoline doctor --db-driver mysql --explain         # print the full plan per query
gasoline doctor --analyze                           # real per-step timings (MySQL 8.0.18+)
gasoline doctor --range 30d --fuel e5               # reproduce a specific page filter
gasoline doctor --skip-queries                      # schema, sizes, indexes and scope only
gasoline doctor -o json | jq '.findings'            # machine-readable
gasoline doctor -o json | jq '.scope'               # the station universe, city by city
gasoline doctor --optimize                          # rebuild the tables to reclaim freed space
gasoline doctor dashboard                           # the dashboard's SQL instead
gasoline doctor all                                 # both pages in one report
```

Each page costs about what one of its loads costs, which is why they are asked for by name rather than measured together every time. Everything below the subcommand — the table, index and scope sections, `--explain`, `--analyze`, `--sql`, `--slow-ms`, `--skip-queries`, `-o json`, `--optimize` — is shared.

#### Why a city you removed still appears

The `scope` section answers that, and the answer is one of two things that need opposite responses. `suggest`, `check` and `notify` cover every station fed within the last 48 hours (see [`gasoline suggest`](#cli-usage)), so `doctor` lists each city that owns stations next to the update target meant to feed it:

```
scope: stations are in scope while fed within 48h; in-scope predictions are kept 30 days
  city                               target  stations  in scope  latest run  newest price update    newest prediction
  lübbecke                          25.0 km       140       140         140  2026-08-17T16:00:22Z   -
  mönsheim                                -        37         0           0  2026-08-14T09:00:11Z   2026-08-16T12:00:00Z
  uchte                             25.0 km        51        51          51  2026-08-17T16:00:31Z   -
  newest run: 2026-08-17T15:08:17Z diesel, 191 stations
```

- **`in scope` above zero for a city that is not a target** is a live problem: something is still feeding it — an ad-hoc `update --city` in a cron entry, or a target whose spelling resolves to a different normalized name than you expect — and every computation still covers it. The finding is a warning.
- **`in scope` at zero, with a `newest prediction`** is history, not scope: the city left the station universe when it stopped being fed, and what remains are rows stored while it was still being collected. Nothing is recomputing it, and the next `suggest --persist` run drops those rows (see [above](#persistent-predictions-and-learning-suggest---persist)), so a `newest prediction` older than the last persist run means that pruning is not running. The finding is informational and names the date the last of it was predicted for.

A configured target with no stations in scope is the inverse failure — the sweep is not reaching it — and a target that owns nothing at all has never been geocoded or has never fetched. Both are warnings. The `latest run` column is the pipeline's own answer to the same question: it counts the stations the newest `suggest --persist` run actually stored predictions for, so a station appearing there while out of scope means that run drew from something other than the fed universe.

The section is built from indexed lookups only — one seek per station, plus one pass over the newest run's predictions — so it runs even under `--skip-queries` and costs nothing next to the page timings. Stored predictions are looked up only for stations that have *left* scope, newest first and capped at 500 stations.

Its filter flags (`--fuel`, `--confidence`, `--range`, or `--from`/`--to`) mirror the page's own controls, so you can reproduce exactly the filter that felt slow in the browser. Each query line ends in a verdict: `covering <index>` means the query was answered from an index alone, a bare index name means it used that index but still fetched table rows, and `TABLE SCAN` means it read the whole table. The `findings` section collects the actionable parts — a missing index, a query over `--slow-ms` (default 1000), a table scan.

When a query is slow but the index it needs exists, the usual cause is the optimizer passing that index over. `doctor` reports the index MySQL actually committed to (its `key`), not the ones it merely weighed, and warns when it chose a non-covering index while the covering one was among the candidates.

`--try-index` settles that case with a measurement instead of a guess. It re-runs every `price_predictions` query a second time with the named index forced and prints both timings side by side:

```bash
gasoline doctor --try-index idx_price_predictions_accuracy
```

```
  by_confidence       3627.4 ms      3 rows  idx_price_predictions_station_fuel_target
    forced:           1827.7 ms      3 rows  covering idx_price_predictions_accuracy  (-50%)
```

It ends with a verdict — whether forcing the index would be faster, slower, or much the same. This stays read-only: the hint applies to that one run, never to the page. Because an index hint changes the plan and not the answer, `doctor` compares row counts between the two runs and tells you to disregard the timings if they ever disagree.

If forcing the covering index is much faster, the optimizer is mis-costing it. Refreshing the statistics it reasons from is worth trying first — `ANALYZE TABLE price_predictions`, or `ANALYZE TABLE price_predictions UPDATE HISTOGRAM ON target_start` when the range estimate looks wrong (a `filtered` value pinned near 10% is the tell). Where that does not change the choice, the accuracy page forces the index itself: five of its aggregate queries carry `FORCE INDEX`/`INDEXED BY`, which measured 66–73% faster per query on a live MySQL after both of those statistics commands had failed to move it. The hint is omitted when the index is absent, so an un-migrated database still renders the page. `series` and the raw-row query are deliberately left unhinted — the hint measured +1% and −2% there, so there is nothing to buy. Re-run `--try-index` after a schema or data-shape change: if forcing stops winning, the hint should go rather than be kept on faith.

#### Why the dashboard is slow (`doctor dashboard`)

The dashboard is slow for different reasons than the accuracy page, so it gets its own mirror of its own SQL. A load issues six queries, and `doctor dashboard` reproduces all of them — including the station list the page inlines into `IN (...)`, because the length of that list is part of the cost:

| query | what the page does with it |
| --- | --- |
| `city` | resolves the selected city (`resolveCity`) |
| `city_search` | the city dropdown's typeahead, measured with the first three letters of the selected city |
| `scope_stations` | the stations inside the radius that are still being fed (`loadScopeStations`) |
| `snapshots` | the price history the chart and table are drawn from (`buildSnapshotQuery`) |
| `predictions_latest` | the newest run per station and fuel (`loadFilteredPredictions`) |
| `predictions_grid` | the future forecast windows for the scope, filtered to the newest run in PHP |

Its flags mirror the page's own controls, so you can reproduce the load that felt slow in the browser:

```bash
gasoline doctor dashboard                                  # busiest city, 5 km, all fuels, 7 days
gasoline doctor dashboard --city berlin --radius 20        # a specific scope
gasoline doctor dashboard --range 30d --fuel diesel        # a specific date filter and fuel
gasoline doctor dashboard --station abc-123,def-456        # what the station picker had selected
gasoline doctor dashboard --no-city                        # the unscoped view
gasoline doctor dashboard --explain --sql                  # plans and SQL per query
gasoline doctor dashboard -o json | jq '.dashboard'        # machine-readable
```

With no `--city` it measures the city with the most stations in scope, which is the slowest dashboard anyone can load, and says which one it picked. `--radius` accepts only the radii the dropdown offers (5, 10, 20) and `--fuel` defaults to the page's own `all`, which expands to three fuels and so to three times the prediction rows. `--no-city` reproduces the unscoped view, where the page loads the station list for the sidebar and skips the snapshot and prediction queries entirely — `doctor` skips them with it rather than inventing a load the page never issues.

The output shows how the scope narrowed, then one line per query:

```
dashboard queries: city=berlin (auto), radius=5 km, fuel=all (e5+e10+diesel), 2026-08-14T00:00:00Z .. now
  scope: 120 in the bounding box, 20 within the radius, 20 queried
  city                    41.2 ms        1 rows  TABLE SCAN
  city_search             38.7 ms       20 rows  TABLE SCAN
  scope_stations           9.4 ms       20 rows  idx_stations_lat_lng
  snapshots             3401.2 ms    58204 rows  idx_price_snapshots_station_recorded
    probe/keys only      502.1 ms    58204 rows  covering idx_price_snapshots_station_recorded
  predictions_latest    8100.0 ms       60 rows  idx_price_predictions_station_fuel_target
    probe/rows walked   3900.0 ms  3104928 rows  covering idx_price_predictions_station_fuel_target
  predictions_grid      1200.0 ms    30248 rows  idx_price_predictions_station_fuel_target
    probe/keys only      180.0 ms    30248 rows  covering idx_price_predictions_station_fuel_target
```

##### Probes: what a query's time is actually spent on

A verdict of `covering <index>` or a bare index name says which index was used, but not why a query that used the right index still took three seconds. The `probe/` lines answer that. Each is the same query with a narrower projection, run alongside the real one:

- **`probe/keys only`** projects just the indexed columns. The query and the probe read exactly the same rows via exactly the same index, so the difference between their timings is what fetching the *unindexed* columns from table rows costs. When that difference is most of the query's time, an index carrying those columns would make the read index-only — which is precisely what `idx_price_predictions_accuracy` did for the accuracy page.
- **`probe/rows walked`** counts the rows an aggregate reduces. `predictions_latest` returns one row per station and fuel; the probe says how many stored predictions it walked to get there.

Probes are read-only and cost roughly one extra read each. `--probe=false` turns them off, and the report then says it cannot account for where the time went.

##### The three findings this produces

- **`predictions_latest` walks the whole retention window.** It bounds station and fuel but nothing in time, so it reads every prediction the scope has accumulated over the 30 days predictions are kept — millions of rows — to produce one row per station and fuel, of which only the newest run's is used. This scales with how often `suggest --persist` runs, not with anything the visitor chose, so it is the one cost a wider date filter or a smaller radius will not reduce.
- **`snapshots` pays for a row lookup per row.** `idx_price_snapshots_station_recorded` stops at `(station_id, recorded_at)`, and the projection needs `is_open`, `e5`, `e10` and `diesel`, so every matching row costs a second lookup into the table. The probe prices it. This one *does* scale with the date filter and the radius.
- **`cities` has no index to use.** The dashboard resolves its city filter against `normalized_name`, which carries no index, and the typeahead wraps it in `LOWER()` besides — so both read the whole table on every load. On a hand-fed install that is a few rows; after `gasoline import cities DE` it is the whole of a country's populated places, and the finding is a warning rather than a note.

`predictions_grid` gets a note rather than a finding: it returns every future window for the scope and PHP then discards the rows that are not from that station's newest run, so the run filter is applied after the rows have crossed the wire. It is bounded by `target_start > now`, so it is smaller than `predictions_latest` by roughly the ratio of the forecast horizon to the retention window.

#### Reclaiming space after a large prune (`--optimize`)

Deleting rows does not shrink a table. InnoDB keeps the emptied pages for reuse and SQLite keeps them on its free list, so after a prune that drops a lot at once — a removed update target taking half of `price_predictions` with it — the sizes above stay where they were. `--optimize` rebuilds the tables so that space goes back to the filesystem:

```bash
gasoline doctor --optimize                                     # every reported table
gasoline doctor --optimize --optimize-table price_predictions   # just the big one (MySQL)
gasoline doctor --skip-queries --optimize                       # skip the page timings first
```

```
optimize (OPTIMIZE TABLE <table>):
  price_predictions          4.6 min  data   1.5 GB -> 712.4 MB  indexes   2.1 GB -> 1.0 GB
      | note: Table does not support optimize, doing recreate + analyze instead
  price_snapshots             38.2 s  data 218.7 MB -> 214.1 MB  indexes 330.0 MB -> 268.9 MB
```

- On **MySQL** each table is rebuilt with `OPTIMIZE TABLE`, which InnoDB performs as a recreate plus `ANALYZE` — the `note:` line above is that substitution being reported, not a refusal. It runs online (concurrent reads and writes keep working) and needs free space on the order of the table being rebuilt. Because it re-analyses as it goes, it also refreshes the statistics the planner reasons from, which is the other reason to reach for it when an index is being mis-costed.
- On **SQLite** the equivalent is `VACUUM`, which rewrites the whole database file and therefore cannot be narrowed to one table — `--optimize-table` is reported as inapplicable rather than silently ignored. It takes an exclusive lock for its duration and needs free space on the order of the database. `ANALYZE` follows it, since `VACUUM` alone leaves the statistics untouched.
- Sizes are measured on both sides of the rebuild and the findings state what came back (`optimize returned 2.0 GB to the filesystem across 2 tables`). MySQL 8 normally answers size questions from a statistics snapshot it keeps for a day (`information_schema_stats_expiry`), which would report the pre-rebuild size afterwards, so `doctor` pins one connection and disables that cache on it — the sizes it prints are the live ones, before and after.
- This is the one part of `doctor` that writes. It runs last, after every measurement, so the table sizes and query timings in the same report still describe the database you were complaining about rather than the one that was just rewritten. Everything else stays read-only, so a plain `gasoline doctor` remains safe to point at production.

Notes on reading the output:

- `doctor` never creates or migrates anything — not the schema, and not the database. Run without arguments it uses the same SQLite path or MySQL settings as every other command (`--db`, `GASOLINE_DB_PATH`, `--db-driver mysql`, …); if that SQLite file does not exist it says so and stops, rather than leaving an empty database behind and reporting every table as absent. On a database that exists but has not been migrated it reports the missing tables and indexes and skips the query timings — run `gasoline migrate` for that.
- A `TABLE SCAN` verdict on a small table is reported as information, not a warning: below roughly 100k rows a scan is often the cheapest plan, and flagging it buries the findings that matter.
- Row counts are exact on SQLite and InnoDB estimates on MySQL, which can be off by a large factor; the text output prefixes the estimates with `~`.
- Per-index sizes need `mysql.innodb_index_stats` on MySQL and the optional `dbstat` module on SQLite. Where the account or build lacks them the sizes are simply omitted.
- Timings include running each query for real, so on a large database each page's section costs about what one load of that page costs — `doctor all` costs both, and probes add roughly one extra read per probed query. Use `--skip-queries` when you only want the schema picture.
- The tables section includes `cities`, which carries no index beyond its primary key. That is deliberate: the dashboard filters on `normalized_name` on every load, and `gasoline import cities` can grow this table by five orders of magnitude, so its row count is worth seeing next to the queries that scan it.

## Output Formats

Most commands print human-readable text by default and also support structured JSON output:

```bash
gasoline list stations -o json
gasoline update --city "Berlin, Germany" --output json
```

## PHP Viewer

The viewer lives in `web/index.php`. It reads `GASOLINE_DB_PATH` when set; otherwise it opens `gasoline.db` next to the repo. To browse a MySQL-backed database instead, set `GASOLINE_DB_DRIVER=mysql` together with `GASOLINE_MYSQL_HOST`, `GASOLINE_MYSQL_PORT`, `GASOLINE_MYSQL_USER`, `GASOLINE_MYSQL_PASSWORD`, and `GASOLINE_MYSQL_DATABASE` in the web server's environment (the viewer uses these individual variables, not `GASOLINE_MYSQL_DSN`). If the server requires SSL, set `GASOLINE_MYSQL_TLS` (`true`, `skip-verify`, or `preferred`); with `true` you can point `GASOLINE_MYSQL_SSL_CA` at a CA bundle to validate the certificate.

Features:

- filter by date range
- filter by city
- filter by fuel type
- compare multiple stations
- inspect summary stats and historical price points

The station list, the price rows and the recommended fill-ups card all cover the **stations currently being fed** — the same 48-hour freshness rule the CLI applies (`GASOLINE_STATION_FRESHNESS_HOURS` mirrors Go's `stationFreshness`). A station whose update target was removed therefore disappears from the dashboard instead of showing its last known price as though it were current; its snapshots stay in the database and reappear if the station is collected again.

Serve it locally from the repo root:

```bash
GASOLINE_ADMIN_EMAIL=you@example.com php -S 127.0.0.1:8080 -t web
```

Then open `http://127.0.0.1:8080/`.

### Web viewer & user accounts

The viewer requires a login. Accounts are registered with an email address (which is the username) and a self-chosen password:

1. Run `gasoline migrate` once so the database has the `users`/`settings`/`update_targets` tables — the viewer shows a hint page until then.
2. Set `GASOLINE_ADMIN_EMAIL` in the web server's environment and register with that exact address: the account is approved immediately and has administrator rights.
3. Everyone else who registers starts out **pending**: they receive a "waiting for approval" email (when SMTP is configured), cannot log in yet, and appear in the admin's Users page. Approving them sends an "account approved" email and unlocks the login.

The hamburger menu in the header opens:

- **My Account** — change the password, configure Pushover (application name, user key, API token), define the notification schedule (weekdays, time windows, daily suggestion times), choose which notification kinds to receive (suggestions and/or buy-now alerts), or delete the account. The last remaining administrator cannot delete their own account.
- **Users** (admins) — approve pending registrations, promote/demote administrators (never yourself, so one admin always remains), and delete accounts.
- **Stations** (admins) — set the same persistent display-name overrides as `gasoline rename`: search a station by name or address, enter a new name, and apply. All existing renames are listed with their original name and address, editable inline or removable to restore the Tankerkönig name.
- **Settings** (admins) — manage the update targets (cities + radii updated automatically by the CLI), the suggestion/check parameters, the notification templates, and the schedule defaults. These are the values the CLI picks up as described in [Server-stored configuration](#server-stored-configuration-admin-settings); notification templates are admin-only and never editable by regular users.
- **Prediction accuracy** (admins) — compare past predicted prices with the actual prices recorded for those windows, from the evaluated `price_predictions` data (see [Persistent predictions and learning](#persistent-predictions-and-learning-suggest---persist)). Filter by fuel, city, target date range, and confidence; view accuracy statistics (count, MAE, bias, RMSE, share within ±1/±2 ct, and a per-confidence breakdown), a predicted-vs-actual graph (timeline with an error band, or a predicted-vs-actual scatter), and the raw evaluated rows.

Signing in lasts: alongside the PHP session the viewer sets a second, long-lived cookie (`gasoline_remember`, 30 days by default — see `GASOLINE_SESSION_DAYS`) whose token is stored hashed in the `user_sessions` table. Closing the browser, an idle afternoon, or the host clearing out PHP's session files no longer means retyping the password — the token restores the login and its expiry slides forward with use. Signing out drops the token for that browser only; changing the password drops it everywhere else and keeps the browser you changed it in. Run `gasoline migrate` once to create `user_sessions`; until then the viewer simply keeps using plain sessions.

The dashboard itself is unchanged — same filters, chart, and tables as before, now behind the login.

## Releases

Build local release binaries for Linux `amd64`, `arm64`, and `armv7`:

```bash
make release
```

Pushing a tag that matches `v*` triggers the GitHub Actions release workflow. It runs tests, builds those three Linux binaries, and publishes a GitHub Release with generated notes.

## Notes

- City geocoding is cached in the database, so Nominatim is only queried once per place unless the cached row is cleared or refreshed.
- `update` stores only changed snapshots plus the adjacent unchanged snapshots needed to preserve price graphs.
- A station inside two update targets' radii belongs to the nearest one, and `price_snapshots.city_name` records that owner. It is provenance: `gasoline stations --city` filters on it, and `suggest`/`check` use it to report a distance when nothing better is available. Notifications do not — a subscriber's distance is measured from their own location.
- The `cities` table is a **geocode cache**, nothing more. `update` needs a city's coordinates for the Tankerkönig call and caches them there; the web UI reads it as an autocomplete source when a user or visitor picks a city, resolving the coordinates once at that moment. Neither the notification path nor the forecast reads it at run time. `gasoline import cities <CC>` fills it in bulk so the autocomplete covers a whole country.
- Distance-only changes do not create a new snapshot, but open/closed changes do.
- `import cities` downloads populated-place data from GeoNames and keeps only matching entries for the requested 2-letter country code.
