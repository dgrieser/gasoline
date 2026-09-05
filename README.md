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
| `GASOLINE_GEOCODE` | Set to `false`/`off`/`0` to switch off the viewer's outbound address lookups. The location filter then offers only the places already cached in the database, and the locate button falls back to labelling the position by its coordinates. |
| `GASOLINE_NOMINATIM_URL` | Base URL of the geocoder the viewer asks (default `https://nominatim.openstreetmap.org`). Point it at your own Nominatim instance to keep the lookups in-house. |
| `GASOLINE_USER_AGENT` | `User-Agent` the viewer sends with those lookups (default `gasoline-web/1.0 (viewer)`), the web-side twin of the CLI's `--user-agent`. |

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
- `--radius` in km, up to 50 (see [Radii wider than the API serves](#radii-wider-than-the-api-serves))
- `--request-delay` and `--request-burst` pace the requests a wide radius needs (one per 37 s by default)
- `--user-agent "your-app/1.0"`
- `--output json` or `-o json`

`--city` is repeatable, and cities with overlapping radii are handled as one sweep: every target is fetched first, then a station reported by more than one of them is stored **once**, owned by the target whose centre is nearest. The prices stored are the freshest ones seen in that sweep, even if a farther target observed them — targets are fetched one after another, so a price can change mid-sweep. Per-city output reports both `fetched_count` (what the API returned) and `stored_count` (what that city wrote); the text output notes when a target lost stations to a nearer one. This keeps a shared station from defeating snapshot compaction — without it, overlapping targets add a row per city on every run even when prices never change.

Ownership is compared against the city that already owns a station, not only against the targets in the current run, so a station stays with its nearest city when you update a single city, when a nearer target's fetch fails, or when cities are updated in separate invocations. It moves only when a strictly nearer city fetches it, or when the owning city is no longer cached.

#### Radii wider than the API serves

Tankerkönig's station list serves a radius of at most **25 km**. `--radius` accepts up to **50**, and anything above 25 is covered internally by several overlapping 25 km queries — a query on the city centre plus one ring around it, sized so the ring's tiles overlap each other and still reach back to the centre tile. There are no gaps, and every tile is placed with about 750 m to spare so a station sitting on a seam is returned by at least one query rather than by none.

What that costs, since it is a request budget and not just a wait:

| `--radius` | API requests | added time per sweep |
| --- | --- | --- |
| up to 25 | 1 | none |
| up to 28 | 4 | 1:51 |
| up to 34 | 5 | 2:28 |
| up to 41 | 6 | 3:05 |
| up to 48 | 7 | 3:42 |
| up to 50 | 8 | 4:19 |

The requests are paced at `--request-burst` (default 1) per `--request-delay` (default 37s), so they go out one at a time, a window apart. Pacing is armed only when something in the sweep actually needs tiling: a sweep whose every target fits in 25 km issues one request per city with no waiting at all, exactly as before. `--request-delay 0` removes the pacing entirely — useful against your own key, unwise against a shared one.

**Why 37 s, and what it costs you.** The default pace is the slowest one that still lets the widest sweep *that answers first time* finish inside a five-minute schedule, which is what the packaged cron entry and systemd timer both use. A 50 km target is 8 requests, whose last goes out at 4:19 — inside the 4:50 the sweep is budgeted (`sweepBudget` in `tiling.go`, asserted by `TestDefaultPaceFitsSweepBudget`).

A sweep that **retries does not fit**, and that is the trade rather than an oversight. One retry is 9 requests and 4:56; in practice Tankerkönig answers a paced sweep with 503s often enough that several retries in one sweep are ordinary, and those run well past five minutes. Such a run loses the following tick to `flock` — prices land ten minutes apart instead of five. A pace where retries do fit is available (`--request-burst 2` spends the same budget two requests at a time) and is deliberately not the default: it is bursty in exactly the way an API that is already refusing is asking us not to be. A retry is the API asking to be left alone, and widening the window is the only lever this program has for that. `tile_retries` on the Statistics page is how you tell whether it is working.

Two more things are worth knowing:

- **More than one tiled city in the same sweep overruns too**, and by much more than a retry does — the pace belongs to the API key, not to the city, so two 50 km targets are 16 requests and about 9:15. `flock -n` covers it the same way: the overrunning run finishes and the next tick is dropped. If you tile more than one city on a five-minute schedule, either raise `--request-burst` or give the sweep its own, slower timer.
- **Widening the radius ceiling** re-opens this arithmetic. `TestDefaultPaceFitsSweepBudget` fails in both directions — too slow for the budget, and a whole window slower than it needs to be — so the default has to be re-derived rather than drifting. It deliberately says nothing about retries: pinning today's overrun as an expectation would fail the day someone makes retries fit again, which would be an improvement.

Three things make a wide radius behave like a single narrow one:

- **One snapshot per station.** The tiles overlap, so most stations are reported several times; they are de-duplicated by station id, and the query nearest the city centre is the one that wins.
- **One timestamp per city.** The requests are deliberately spread over minutes, so stamping each station with the query that happened to see it would spread one city's readings across that window and read like a price history. Every station of a tiled city therefore carries the instant its first request went out.
- **The radius you asked for.** The overlapping tiles bulge slightly past it; stations in the bulge are dropped, so `search_radius_km` stays honest and station ownership does not drift between sweeps.

If a query other than the centre one fails, it is retried once and then given up on: the city is still stored with everything the other queries saw, `tiles_failed` reports the loss, and the run is recorded as `partial`. The stations only that query could see go unrefreshed until the next sweep, which is well inside the 48-hour window the model already tolerates. A failing **centre** query fails the city, because that failure is almost always systemic — a rejected key, no network — and a city assembled purely out of its own edges is worse than no update.

A 50 km target covers four times the area of a 25 km one. `suggest`, `check` and `notify` cover every station still being fed, and `suggest --persist` stores the full forecast grid per station per fuel, so that multiplies `price_predictions` growth and `suggest` runtime too — see [Diagnosing a slow database](#diagnosing-a-slow-database-gasoline-doctor).

Compact existing snapshots in place:

```bash
gasoline compact
```

Run this once after upgrading if you have been updating cities with overlapping radii: earlier versions stored a row per city for every shared station on every run, and `compact` collapses those into the single row the current `update` maintains.

`compact` is also the housekeeping pass for [command run statistics](#command-run-statistics): it drops recorded runs older than 30 days and reports how many went. The commands that record them fire every few minutes and should not each pay for a retention sweep.

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

Administrators configure two things in the web UI (hamburger menu → Settings): the **update targets** (city + radius pairs) that decide which stations are collected, and the **notification texts**. A target's radius is editable in place — each row has its own radius field and save button, up to 50 km, and the change takes effect on the next `gasoline update`. The city is the target's identity and is not editable: changing it means removing the target and adding the new city.

That is deliberately all of it. The station scope, the fuels, the model parameters and the delivery limits used to be settings and are now fixed, because none of them had a per-install answer:

- `gasoline update` invoked **without any** `--city`/`--radius` flags updates every configured update target with its per-target radius, as a single de-duplicated sweep: targets whose radii overlap share stations, and each shared station is stored once under its nearest target. Passing explicit flags ignores the targets entirely. `radius_km` is the only radius in the system, and may be up to 50 km — a target over 25 km costs several paced API requests per sweep, see [Radii wider than the API serves](#radii-wider-than-the-api-serves).
- `gasoline suggest`, `gasoline check` and `notify` take no scope or fuel arguments. They cover every station still being fed and compute all three fuels, so nothing that gets delivered goes unmeasured. Each user picks the one fuel they are notified about (see below).
- The fixed parameters are 30 days of history, a 3-day forecast horizon, 3 suggestions per day, 5 check rows, a flat 2 ct price margin, a 48-hour station freshness window, and a baseline reset at local midnight. The per-user notification schedule defaults (every day, 07:00–21:00, suggestions at 08:00 and 13:00) apply only until a user sets their own.

Upgrading to per-user notification areas: `migrate` carries a user's old city selection over whenever it says exactly what one area can express, using the legacy `range_km` setting as the radius — that is what the old notification path measured with, whereas a target's radius only ever decided what got collected. A single selected city becomes that city's centre at that range, and so does the old default of selecting nothing whenever there is exactly one update target, since "all cities" and "that city" are then the same area. Only genuinely ambiguous cases — several selected cities, or none with several targets — are left without an area and named on stderr as `needs_area`; those users receive nothing until they pick a city and radius, which is better than silently changing what they receive.

Run `gasoline migrate` once to create the tables and seed the notification templates. It reports the tables it had to create (`- user_filters.created`) alongside the column and index migrations it applied, and prints `no migrations needed` only when it genuinely changed nothing. Seeding never overwrites existing rows, so admin edits survive re-runs. `migrate` also deletes the settings rows that older versions stored for the parameters listed above, so the table stops advertising configuration that no longer does anything.

`migrate` also backfills the covering index the admin **Prediction accuracy** page aggregates over (`idx_price_predictions_accuracy`); `gasoline doctor` reports whether it is present, see [Diagnosing a slow database](#diagnosing-a-slow-database-gasoline-doctor). On an install that has already accrued a large `price_predictions` table this is the one migration step that takes a noticeable while — tens of seconds per few million rows — and it grows the database by roughly the size of the table's own data. MySQL builds the index in place without blocking reads or writes, so a `suggest --persist` run may overlap it.

It backfills two smaller things the dashboard needs as well: `idx_cities_normalized` for its city filter, and `cities.normalized_lower` plus `idx_cities_search` for the city dropdown's typeahead. Both are quick even on a `cities` table holding a whole country — the row count is one per known place, not one per prediction. Until the typeahead's column is backfilled the dropdown finds nothing, so run `migrate` after upgrading rather than only when a schema error says to; `gasoline doctor dashboard` reports either index as missing.

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

### Command run statistics

`update`, `suggest`, `check` and `notify` each record what they did, so a timer that quietly stopped firing — or started failing — is visible without shell access to the server. Every invocation writes one row to `command_runs` with its start and end time, duration, status and error, plus the counters it already computes as name/value pairs in `command_run_metrics`. `update` additionally writes one row per Tankerkönig request to `command_run_tiles` (see [The individual requests of a run](#the-individual-requests-of-a-run)). All three tables are created automatically; run `gasoline migrate` once on an existing database to get them before the next scheduled run.

The row is written **before** the work starts and completed afterwards, so a run that is killed, runs out of memory, or hangs still leaves a trace. Such a run keeps `status = 'running'` forever — nothing sweeps it up later — and the web UI counts one older than six hours as *interrupted*. The status of a finished run is `ok`, `error`, or `partial`: the last is the best-effort case where some units of work failed and the rest succeeded ("2 of 5 cities failed"), which the command still reports as an error on the command line.

Two things are deliberately not recorded. A failure **before the database is open** — an unparseable flag, a wrong DSN, an unreachable MySQL server — leaves no row, because no work ran and there is nowhere to write it; those still go to stderr and the journal as they always did. And `notify --dry-run` records nothing at all: it sends nothing and writes nothing, so counting it would mix rehearsals into the delivery numbers.

Recording never fails a command. If the statistics write cannot happen, the command prints one `command stats:` line to stderr and carries on with its real work unchanged.

The metric names per command, which are what the web UI renders:

| Command | Metrics |
| --- | --- |
| `update` | `cities`, `cities_failed`, `stations_fetched`, `snapshots_stored`, `tile_requests`, `tile_retries`, `tile_slowest_ms`, `tile_wait_ms`; when a target was tiled also `tiles`, `tiles_failed`; `tile_requests_unlogged` only when a sweep made more requests than the per-run log keeps |
| `suggest` | `persist` (0/1), `fuels`, `fuels_failed`, `stations`, `snapshots_scanned`; with `--persist` also `predictions_stored`, `decisions_stored`, `predictions_evaluated`, `outcomes_scored`, `stations_bias_corrected`, `pruned_predictions`, `pruned_decisions`, `unfed_stations`, `unfed_predictions`, `unfed_decisions` |
| `check` | `fuels`, `fuels_failed`, `stations`, `snapshots_scanned`, `check_rows` |
| `notify` | `stations`, `users`, `check_rows`, `suggest_rows`, `sent`, `failed`, `baseline_reset` (0/1) |

They are the same numbers the commands print when you run them by hand; `stations` and `snapshots_scanned` are the size of the shared history scan, which is what explains a `suggest` or `check` run's duration.

`update`'s four request counters are what explains *its* duration, and they split it the only way that is actionable: `tile_wait_ms` is the pacing obeying `--request-delay`, and `tile_slowest_ms` is the API being slow. The first is yours to tune, the second is not. `tile_retries` counts the requests that were not a tile's first try — the number that separates a slow Tankerkönig from a failing one, and the reason a sweep can take a window longer than its tile count suggests.

#### The individual requests of a run

`update` also writes one row per Tankerkönig request to `command_run_tiles`: which city and tile it was for, which attempt, the instant the pacing released it, how long it had been held, how long the request itself took, and whether it answered, was retried, or failed. A retried attempt is kept **as its own row** rather than folded into the try that replaced it — the whole point is that a tile needing two attempts cost two pacing windows, which a log showing only the winner hides.

The metrics table holds one number per name per run, so none of that can live there: the shape of a sweep needs a row per request. This is the one thing that makes a 4-minute sweep legible — a slow tile, a retried one, and a sweep that simply has eight tiles to get through all look the same from the run's own duration.

Every sweep writes these, tiled or not: a narrow sweep is one request per city rather than none, and a retry there is worth seeing for the same reason. They are pruned with their run, and copied by `migrate-to-mysql` along with it.

Runs are kept for 30 days and pruned by `gasoline compact`, which takes each run's metrics and requests with it. The web UI reads them on the admin **Statistics** page (hamburger menu → Statistics).

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

The timer uses `OnUnitInactiveSec`, which counts the interval from when a run *finishes*. `OnUnitActiveSec` counts from when it started, so any sweep longer than the interval re-fires the instant it ends — a busy loop rather than a schedule. That matters once a target's radius is wide enough to be collected as several paced requests (see [Radii wider than the API serves](#radii-wider-than-the-api-serves)).

Prefer cron? `examples/cron/gasoline-update.cron` holds a ready-to-use line — add it with `crontab -e`. Unlike systemd, cron starts with an empty environment, so the line sources the env file first (`set -a` exports every variable it defines). It also wraps the command in `flock -n`: cron fires on its schedule whether the previous run finished or not, and two sweeps at once would race each other for the API rate limit and for the database write lock.

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

Every query carries a probe as well — the same measurement the dashboard checks use, see [Probes](#probes-what-a-querys-time-is-actually-spent-on). For the aggregates it is `probe/rows walked`: the index entries the filter matches and what walking them alone costs, so a slow pass can be told apart from a wide `--range`. `summary_latest` and `rows` get structural probes instead, dropping the self-join and the metadata joins respectively, because for those two a projection-only probe could not tell the join apart from the rest. `--probe=false` skips them.

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

##### `--runs N`: one timing is one sample

Every timing in the report is one sample of a noisy process, and on a production
database the noise is not small: the same statement re-run minutes later came back
60% faster, and the first query measured reliably absorbs cache warming and reads
slower than the rest. A probe gap taken across two such samples once attributed
7.7 seconds to row lookups that three later runs put at 0.2 seconds.

`--runs N` takes N samples of each timing and reports the fastest, which is the
right summary because every disturbance — a cold cache, another query on the box,
a scheduler hiccup — can only make a run slower. The spread between fastest and
slowest is kept as that query's own variance, shown on its line as `+N%`, and no
difference smaller than it is attributed to anything:

```bash
gasoline doctor --runs 3            # before deciding anything from a difference
```

With a single run the report says so rather than letting one sample read as the
truth. Use `--runs 3` or more whenever a number is going to change a decision.

The band ignores queries under 250 ms. Interference costs a fixed number of
milliseconds, so as a share it explodes on small timings: on one production run a
58 ms query varied by 484%, and taking that as the page's band would have
dismissed every real difference in the report as noise.

##### Repeats, and the noise band they measure

Naming an index a query **already** forces produces byte-identical SQL, so its second timing is the same statement run twice. Those pairs are labelled `repeat:` rather than `forced:`, they are kept out of the verdict, and they are put to work instead: the widest move they make is this database's own variance, measured in the same run under the same conditions.

That matters more than it sounds. On a production MySQL, `--try-index idx_price_predictions_accuracy` re-ran identical SQL for the five queries the page already hints — and those repeats moved by −10%, −3%, −0%, +1% and **−27%**. The one query whose SQL genuinely changed moved −28%. Read on its own that looks like a result; read against a −27% repeat of unchanged SQL it is indistinguishable from the afternoon:

```
info 5 queries already force idx_price_predictions_accuracy, so their second timing
     re-ran identical SQL: those repeats moved up to 27%, which is this database's own
     variance and the bar any real difference below has to clear
info forcing idx_price_predictions_accuracy moved the queries (rows, series) by -24%
     (7400 ms against 9692 ms), which is inside the 27% a repeat of unchanged SQL moved
     on its own — not established either way; repeat the run before acting on it
```

Before this, the verdict averaged all seven pairs together and reported "little difference" — a single number over five comparisons that had compared nothing. If every query already forces the index, the report now says so outright rather than producing a verdict from no comparisons at all.

If forcing the covering index is much faster, the optimizer is mis-costing it. Refreshing the statistics it reasons from is worth trying first — `ANALYZE TABLE price_predictions`, or `ANALYZE TABLE price_predictions UPDATE HISTOGRAM ON target_start` when the range estimate looks wrong (a `filtered` value pinned near 10% is the tell). Where that does not change the choice, the accuracy page forces the index itself: five of its aggregate queries carry `FORCE INDEX`/`INDEXED BY`, which measured 66–73% faster per query on a live MySQL after both of those statistics commands had failed to move it. The hint is omitted when the index is absent, so an un-migrated database still renders the page. The three breakdown tables — by confidence, by lead-time bucket and by hour — and the predicted-vs-actual chart come out of **one** query rather than four. They are four groupings of the same rows, and as four queries each walked the whole filtered slice again: on a production database 1.4M index entries read four times to produce 3, 6, 24 and ~570 rows. One pass grouped by `(target_start, confidence, lead_bucket)` gives all four — the hour is a substring of `target_start`, and the chart is the sum per `target_start`. That requires the query to return `SUM` and `COUNT` rather than `AVG`, since the mean of means is not the mean unless every group is the same size; dividing once per table afterwards is exact.

Both steps were measured on SQLite over ~1.48M rows, and neither matched the estimate the shared walk suggested:

| | before | after | |
| --- | --- | --- | --- |
| three grouped tables → one pass | 7.00 s | 4.81 s | −31% |
| chart folded into that pass | 4.57 s | 3.83 s | −16% |

Grouping is not free — the key is evaluated for every row in the slice, and the walk itself is only 0.68 s of it — so the first step returned a third rather than the two-thirds a pure walk-sharing argument predicts. The second was the opposite surprise: adding `target_start` to the key cost nothing measurable, so the chart's entire pass came for free. That difference is also why the lead bucket is grouped as a small integer and labelled in PHP: the string form cost 674 ms for exactly the same 336 groups.

On MySQL the balance is inverted — its probes put walking at 1.5–2.0 s of a 1.9–2.9 s query — so both savings should be larger there. Measure with `--runs 3` rather than taking any of these numbers on faith.

Two of the page's queries were sorting or grouping against the grain of that index, which the report made visible once `rows` and the breakdowns stopped dominating it:

- **The raw-row query ordered `target_start DESC, station_id ASC`.** Within one fuel the index is ordered `(target_start, station_id)` ascending, so no single scan direction gives descending targets *and* ascending stations — the server had to materialise all 1.4M rows in the range and sort them to find 1001. Ordering both keys descending is that index read backwards, so the `LIMIT` stops after 1001 entries. SQLite's plan drops `USE TEMP B-TREE FOR LAST TERM OF ORDER BY` entirely. The page re-sorts stations ascending for display afterwards, so the only difference is which stations fall inside the cap at its oldest hour — a boundary that is arbitrary either way.
- **The latest-run subquery grouped `station_id, target_start`.** Same groups either way, but `target_start, station_id` is the index's own order with `run_id` the column immediately after both, which is the shape a server can stream or even answer with a loose index scan. SQLite streamed it both ways, so this went out unverified on the strength of the index shape alone — and on MySQL it took the query from 3.06 s to 2.31 s, its inner group-by from 2.51 s to 1.87 s.

Every remaining query carries the hint. `series` used to be the exception — forcing the index there measured −13%, +4%, −1% and +5% over four runs, which straddles zero — but it is no longer a query of its own.

The raw-row query was in that group too, and stopped belonging there. It was measured at −2% when `price_predictions` was smaller; past 8M rows the optimizer began driving it from `idx_price_predictions_due`, which leads with `fuel` and then serves neither the `target_start` range nor the sort, and it became the slowest query on the page at 8.3–8.8 s against 6.0–6.5 s forced. That is the case for re-running `--try-index` after a schema or data-shape change rather than trusting an old number: a hint decision has a shelf life, and this one expired without anything failing.

Read the verdict against the noise band in the same run, not on its own. Across those four runs the reported delta on that query ranged from −28% to −60% while the band ranged from 11% to 60%, and one run called it not established — the conclusion came from the two distributions never overlapping (lowest unhinted 8271 ms, highest forced 6485 ms), which is what four runs buy you that one cannot.

#### Why the dashboard is slow (`doctor dashboard`)

The dashboard is slow for different reasons than the accuracy page, so it gets its own mirror of its own SQL. A load issues five queries worth timing, and `doctor dashboard` reproduces all of them — including the station list the page inlines into `IN (...)`, because the length of that list is part of the cost:

| query | what the page does with it |
| --- | --- |
| `city_search` | the location field's typeahead, measured with the first three letters of the named place |
| `scope_stations` | the stations inside the radius that are still being fed (`loadScopeStations`) |
| `nearby_latest` | the current price at each of the 40 nearest stations, prefetched for the station dialog (`loadNearbyPrices`) |
| `snapshots` | the price history every card, the chart and the table are drawn from (`buildSnapshotQuery`) |
| `predictions_grid` | the future forecast windows for the scope, reduced to the newest run per station in PHP (`loadFilteredPredictions`) |

The reader's stored filters are the sixth thing a load reads and the one query not listed: it is a single primary-key row out of `user_filters`, and timing it would tell an operator nothing. Because the location comes off that row already resolved, `--city` is doctor's own stand-in for it — a name an operator can type, resolved to the coordinates the filter row would have held.

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

With no `--city` it measures the city with the most stations in scope, which is the slowest dashboard anyone can load, and says which one it picked. `--radius` accepts only the radii the dropdown offers (5, 10, 20) and `--fuel` defaults to the page's own `all`, which expands to three fuels and so to three times the prediction rows. `--no-city` reproduces the unscoped view, where the page loads the station list for the sidebar and skips the nearby, snapshot and prediction queries entirely — `doctor` skips them with it rather than inventing a load the page never issues.

The output shows how the scope narrowed, then one line per query:

```
dashboard queries: city=Lübbecke (auto), radius=5 km, fuel=all (e5+e10+diesel), 2026-08-14T00:00:00Z .. now
  scope: 10 in the bounding box, 8 within the radius, 8 queried
  city                     9.1 ms        1 rows  TABLE SCAN
  city_search             22.5 ms       20 rows  TABLE SCAN
  scope_stations           0.4 ms       10 rows  idx_stations_lat_lng
  snapshots                6.9 ms     2164 rows  idx_price_snapshots_station_recorded
    probe/keys only        2.8 ms     2164 rows  covering idx_price_snapshots_station_recorded
  predictions_latest  181085.1 ms       23 rows  idx_price_predictions_station_fuel_target
    probe/rows walked     256.3 ms   610978 rows  covering idx_price_predictions_station_fuel_target
  predictions_grid       196.2 ms    40290 rows  idx_price_predictions_station_fuel_target
    probe/keys only       52.5 ms    40290 rows  covering idx_price_predictions_station_fuel_target
```

Those are real numbers from a production MySQL 8.4, taken **before** the fixes below — and before the nearby read existed or the filters moved onto the account, which is why a `city` lookup appears that no load performs any more — and they are the reason the probes exist. One query was 99.87% of the load — and the probe beside it showed that reading its 610,978 rows took 256 ms, so the row count was *not* what cost three minutes. The same report is now 257 ms in total: `predictions_latest` no longer exists and neither of the `cities` scans does. See [what this found](#what-doctor-dashboard-found-and-what-was-done-about-it).

##### Probes: what a query's time is actually spent on

A verdict of `covering <index>` or a bare index name says which index was used, but not why a query that used the right index still took three seconds. The `probe/` lines answer that. Each is the same query with a narrower projection, run alongside the real one:

- **`probe/keys only`** projects just the indexed columns. The query and the probe read exactly the same rows via exactly the same index, so the difference between their timings is what fetching the *unindexed* columns from table rows costs. When that difference is most of the query's time, an index carrying those columns would make the read index-only — which is precisely what `idx_price_predictions_accuracy` did for the accuracy page.
- **`probe/rows walked`** counts the rows an aggregate reduces, and how long walking them takes — so an aggregate returning a handful of rows off millions can be told apart from one that is genuinely cheap.
- **Structural probes** drop a join rather than a projection, where the join is what a projection-only probe could not price: `probe/inner group only` runs the accuracy page's latest-run group-by without the self-join back into `price_predictions`, and `probe/page only` runs its capped row page without the `prediction_runs` and `stations` joins. Which side of such a query dominates is not predictable — on one database the group-by was a fifth of `summary_latest` and on another it was all of it — which is the point of measuring rather than reasoning.

A probe is only a floor for its query if it reads the same rows the same way, so each one is **pinned to the index its query actually chose** rather than to the hint the page emitted. That distinction is not academic: the two queries the accuracy page leaves unhinted are free to be costed differently once a probe narrows their projection, and before pinning, `series`'s probe picked another index and came out four times *slower* than the query it was measuring. Where the plans still differ the probe is labelled `(different plan, not comparable)`, no finding is drawn from it, and its rows stay out of the page totals.

Where a query is answered from an index alone, the gap to its probe is the aggregation rather than row lookups, and the findings say so — calling it lookups would send you after an index the query is already using. And where there is no gap at all, that is the finding: whatever the probe drops is free, so the cost is in the part it kept. That is what ruled the metadata joins out of the accuracy page's slowest query and left its index choice as the only thing still to explain.

Because the accuracy page runs several independent aggregate passes over the same `(fuel, target_start)` slice, `doctor` states that slice once for the page rather than under each query:

```
info 5 of the page's queries each walk the same 1,476,360 rows this filter selects
     (by_confidence, by_hour, by_lead, series, summary), 2022 ms of the total spent
     walking them over again; a narrower --range is what shrinks that slice
```

Where a probe shows the query paying for something its index could have carried, the finding also reports the cost **per row** in microseconds. That is the number that decides the fix: a few microseconds per lookup is a buffer-pool hit and the row count is the thing to reduce; hundreds of microseconds is a disk seek, and then the table has outgrown the cache and rewriting one query will not hide it.

Probes are read-only and cost roughly one extra read each. `--probe=false` turns them off, and the report then says it cannot account for where the time went.

##### The findings this produces

- **A query paying a table lookup per index entry.** Where the probe reads the same rows through the same index far faster than the query does, the gap is the columns the index does not carry, fetched a row at a time. The finding names the index, the gap, and the per-lookup rate.
- **`snapshots` is the standing example.** `idx_price_snapshots_station_recorded` stops at `(station_id, recorded_at)`, and the projection needs `is_open`, `e5`, `e10` and `diesel`, so every matching row costs a second lookup into the table. This scales with the date filter and the radius — and on a small scope it is single-digit milliseconds, which is why the finding needs an absolute floor and not just a share of the query.
- **A missing index.** Both `cities` indexes are expected, so an install that has not run `gasoline migrate` is told which one is absent rather than left to infer it from `TABLE SCAN` on the city lookup or the typeahead.

`predictions_grid` gets a note, not a finding: it returns every future window for the scope and PHP reduces them to the newest run per station afterwards, so the run filter lands after the rows have crossed the wire. It is bounded by `target_start > now`, which is what keeps it small.

#### What `doctor dashboard` found, and what was done about it

The production run above is what this command exists to produce, so it is worth recording what it changed.

**`predictions_latest` was deleted.** `loadFilteredPredictions` used to resolve the newest run per station and fuel with its own aggregate over `price_predictions`, bounded by station and fuel but not in time — so it read every prediction the scope had accumulated across the whole retention window to produce a couple of dozen rows. That was 158 of the page's 158.5 seconds. The probe is what identified the cause: walking those 612,665 index entries took 262 ms, and the rest was one lookup into the 1.5 GB clustered index per entry, at ~258 µs each, to fetch the `run_id` that `(station_id, fuel, target_start)` does not carry. The page now takes the maximum `run_id` from the forecast-grid rows it already reads, so the answer comes out of one query instead of two and the aggregate is gone.

Two things changed with it, both improvements. Runs are compared by `run_id` rather than by `run_at`, so two runs recorded in the same second no longer both count as newest. And a station the newest run stored no future window for — too little history to forecast it, say — now shows the most recent run that did cover it, where before it showed nothing at all. `web_picker_test.php` pins both.

**`idx_cities_normalized` was added.** `resolveCity` runs on every dashboard load and matched `normalized_name`, which had no index; where `import cities` had loaded a country's populated places that was a 54,280-row scan, twice per load. `gasoline migrate` adds it — small and quick, unlike the accuracy index.

**The city typeahead was made seekable.** `city_search` matched on `LOWER(normalized_name)`, and a column inside a function cannot be seeked, so no index could help it: 41.6 ms of scanning 54,280 rows on every keystroke past the third, which is interactive latency rather than page latency. Folding the column in the query is what made it unindexable, so the fold moved into storage — `cities.normalized_lower`, written by `citySearchKey`, backfilled by `gasoline migrate` and indexed by `idx_cities_search`. The query became a half-open range over it, which is a plain index seek: measured on SQLite over the same 54,280 rows, 5.139 ms → 0.024 ms.

It also fixed a case-folding bug. The search term was folded in PHP with byte-based `strtolower` while the column was folded by the database, and the two disagree beyond ASCII — on SQLite, whose `lower()` is ASCII-only, a city stored as `LÜBZ` could not be found by typing its name at all. There is now one fold on both sides: `strings.ToLower` in the CLI, `mb_strtolower` in the viewer, pinned together by `TestCitySearchKeyIsMirroredByTheViewer`. That is also why the migration backfills in Go rather than with `UPDATE ... SET normalized_lower = LOWER(normalized_name)`, which would have baked the engines' disagreement into the data.

**A covering index for `snapshots` was measured and deliberately not added.** The probe prices its row lookups at ~5 ms of an 8 ms query, about 2 µs each: a lot of cheap buffer-pool reads. An index carrying the four price columns would remove them, at the cost of tens of megabytes and slower `update` writes, to save five milliseconds. `doctor` reports it as information rather than a warning for exactly that reason. Re-measure with a wider radius and date range, or once the finding escalates to a warning, and reconsider then.

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
- A `TABLE SCAN` verdict on a small table is reported as information, not a warning: below roughly 100k rows a scan is often the cheapest plan, and flagging it buries the findings that matter. The same applies to the time a query spends on row lookups: a share of a query is only reported once it is also worth a person's attention in absolute terms, since half of a seven-millisecond query is still nothing.
- On MySQL the plan is read from `EXPLAIN FORMAT=TRADITIONAL`, so the index verdicts come from the `key` and `Extra` columns whatever the server's `explain_format` default is. Where only a tree plan is available — `--analyze` always is one, and MariaDB does not know the `TRADITIONAL` keyword — the verdicts are read from the tree's own wording instead.
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
- filter by location — a city, a postal code, a street with a house number, or the browser's own position
- filter by fuel type
- compare multiple stations
- see what the stations around that location cost, ordered by price or by distance
- inspect summary stats and historical price points

**The filters belong to the account.** They are stored in `user_filters`, one row per user, written whenever the sidebar changes and read back on every load — so signing in from a second browser, or from a phone, lands on the same dashboard. Nothing in the URL selects data any more: a dashboard link is just a link to the dashboard, and two people sharing a browser no longer share a view. **Reset** deletes the row and returns to the defaults.

**Entering a location.** Typing searches the database and never leaves the host, because Nominatim's usage policy rules out a lookup per keystroke; it answers from two sources, the cities the CLI has geocoded and the postal codes read off the station addresses themselves (`10115` offers *10115 Berlin*, centred on the mean of its filling stations). A street with a house number is in neither, so the dropdown ends with a **Search address "…"** row that spends one geocoder lookup, and the locate button beside the field takes one position fix from the browser and reverse-geocodes it. Both apply immediately and both leave an ordinary editable label behind — a reverse-geocoded house number that came back wrong is corrected by typing over it and pressing Enter. What they resolve is stored on the reader's own filter row as a label and a point; the `cities` table stays what the CLI made it, one row per place it collects. `GASOLINE_GEOCODE=false` switches the outbound lookups off, and the locate button then labels the position by its coordinates.

The **stations card** leads the page and answers the two questions a reader arrives with — what is cheapest, and what is close — with a toggle in its top right corner saying which of them is currently ranking it. It is built like every other card: a column per fuel, the leading station's price large, the rest as ranked rows, eight before a **Show more**. The toggle is two icon-only pills — a falling price and a map pin, the two marks the page draws an order with — because the words for them are wider than a narrow phone has left once the heading and the radius have had theirs, and a toggle that wraps costs the card a whole row; the words stay on as the buttons' labels for a screen reader and as their hover titles. Sorting by price titles the card *Cheapest now* (German: *Günstigste*); sorting by distance titles it *Nearby* (*Umgebung*) and names the radius beside it — the radius and not the location, because where the reader lives is theirs and a card gets screenshotted. The choice is kept in a `gasoline_card_sort` cookie, like the collapsed filter panel: it belongs to the browser in the reader's hand rather than to the account, so a phone and a desktop can each keep the order that suits them.

Both orders rank one roster, and that roster is the same filtered snapshot history the chart is drawn from — so the location and its radius, the date range, the station picker and the fuel all narrow this card exactly as they narrow the chart. Each station appears once, at the newest price the filters admit, with the distance the location scope measured for it; a station that is shut still lists, since its price is the one it will reopen with, and says so next to its name. Every row across every card ends with the distance in its own column at the right edge, so a long station name ellipsizes rather than swallowing it. Without a location there are no distances to rank by: the card falls back to price, greys the distance half of the toggle out rather than hiding it, and says under the list what would make it available. Tapping a row opens the station detail dialog, navigation link included.

That dialog is why the viewer still reads the current price at the nearest stations separately, bounded to the same 48-hour freshness window the station scope uses, for up to the 40 nearest: nothing on the page draws it, but a station the date range or the picker kept out of the snapshot rows — one the recommended fill-ups card named, say — has to open with a price all the same. `gasoline doctor dashboard` times that read with the rest and says whose cost it is.

The station list, the price rows and the recommended fill-ups card all cover the **stations currently being fed** — the same 48-hour freshness rule the CLI applies (`GASOLINE_STATION_FRESHNESS_HOURS` mirrors Go's `stationFreshness`). A station whose update target was removed therefore disappears from the dashboard instead of showing its last known price as though it were current; its snapshots stay in the database and reappear if the station is collected again.

Serve it locally from the repo root:

```bash
GASOLINE_ADMIN_EMAIL=you@example.com php -S 127.0.0.1:8080 -t web
```

Then open `http://127.0.0.1:8080/`.

### Web viewer & user accounts

The viewer requires a login. Accounts are registered with an email address (which is the username) and a self-chosen password:

1. Run `gasoline migrate` once so the database has the `users`/`user_filters`/`settings`/`update_targets` tables — the viewer shows a hint page until then.
2. Set `GASOLINE_ADMIN_EMAIL` in the web server's environment and register with that exact address: the account is approved immediately and has administrator rights.
3. Everyone else who registers starts out **pending**: they receive a "waiting for approval" email (when SMTP is configured), cannot log in yet, and appear in the admin's Users page. Approving them sends an "account approved" email and unlocks the login.

The hamburger menu in the header opens:

- **My Account** — change the password, configure Pushover (application name, user key, API token), define the notification schedule (weekdays, time windows, daily suggestion times), choose which notification kinds to receive (suggestions and/or buy-now alerts), or delete the account. The last remaining administrator cannot delete their own account.
- **Users** (admins) — approve pending registrations, promote/demote administrators (never yourself, so one admin always remains), and delete accounts.
- **Stations** (admins) — set the same persistent display-name overrides as `gasoline rename`: search a station by name or address, enter a new name, and apply. All existing renames are listed with their original name and address, editable inline or removable to restore the Tankerkönig name.
- **Settings** (admins) — manage the update targets (cities + radii updated automatically by the CLI), the suggestion/check parameters, the notification templates, and the schedule defaults. These are the values the CLI picks up as described in [Server-stored configuration](#server-stored-configuration-admin-settings); notification templates are admin-only and never editable by regular users.
- **Prediction accuracy** (admins) — compare past predicted prices with the actual prices recorded for those windows, from the evaluated `price_predictions` data (see [Persistent predictions and learning](#persistent-predictions-and-learning-suggest---persist)). Filter by fuel, city, target date range, and confidence; view accuracy statistics (count, MAE, bias, RMSE, share within ±1/±2 ct, and a per-confidence breakdown), a predicted-vs-actual graph (timeline with an error band, or a predicted-vs-actual scatter), and the raw evaluated rows. The page renders before it reads anything: the filters, the empty tables and their spinners paint immediately and `?action=prediction_accuracy` fills them in, so a click opens a page rather than sitting on the previous one. That endpoint also picks which fuel to open on — the first with anything evaluated, preferring diesel — and the picker adopts its answer when the first payload lands.
- **Statistics** (admins) — what the scheduled commands actually did, from the recorded `command_runs` data (see [Command run statistics](#command-run-statistics)). Filter by command and time range; view run counts and success rate, median and p95 duration, how long ago each command last ran, a stacked runs-over-time chart with the average duration, a per-command summary, the summed counters, and the most recent runs with their metrics and error text. All three tables sort by any column — from the header on a desktop, from a **Sort by** control that is there on a phone too, where the header is not. The per-command table filters by status — a row there is a command and its tallies, so **Failed** keeps the commands that have a failure among their runs rather than the failures themselves. The counter table filters by command and by counter name, and the runs table by status, duration (a threshold, or outliers at or above the p95 the tile reports), host and command — the last of these intersecting with the page-wide command filter rather than replacing it, since on a phone that one is several screens above the table it narrows. Those run filters are applied in SQL over the whole selected range rather than in the browser over the rows on screen, so **Failed** finds the failures in a range whose newest few hundred runs were all green; the table still lists at most the newest 200 that match, while the tiles and the two tables above it always cover the range as a whole.

Every table's filters and sorting sit in a section that starts collapsed under a **Filters** header, so a card opens as its heading and its table and nothing else; the reset inside is a `↺` rather than a label, its name carried on the button's title and aria-label. The counter table also stays a **real table** on a phone rather than becoming one card per row — four short columns fit where four cards do not — and because it keeps its header at every width it has no sort control at all: tap a column to sort by it, tap again to reverse. The runs table keeps its card-per-row layout, which suits a row carrying an error and a metric list, and on a phone its controls sit three to a row: status, duration and host, then command, sorting and the reset. An `update` run carries a toggle reading e.g. `8 requests, 1 retried` — so a sweep that had to retry is visible without expanding anything — and expanding it lists that run's individual requests: tile, attempt, when it went out, what the request took, and what the pacing added on top. The list is read on demand for the one run rather than shipped with all 200, and a database written by a binary older than the page says so instead of showing an empty panel. Failure reasons are listed **under** that table rather than as rows in it, keyed by the request numbers they belong to and with identical messages listed once — a sweep that meets a failing API meets the same failure on every tile, so five copies of one sentence become one line naming five requests. Each is one line, ellipsised against the panel's width, and opens in place when tapped.

Signing in lasts: alongside the PHP session the viewer sets a second, long-lived cookie (`gasoline_remember`, 30 days by default — see `GASOLINE_SESSION_DAYS`) whose token is stored hashed in the `user_sessions` table. Closing the browser, an idle afternoon, or the host clearing out PHP's session files no longer means retyping the password — the token restores the login and its expiry slides forward with use. Signing out drops the token for that browser only; changing the password drops it everywhere else and keeps the browser you changed it in. Run `gasoline migrate` once to create `user_sessions`; until then the viewer simply keeps using plain sessions.

Each account carries its own dashboard: the sidebar's filters — location, radius, date range, fuel and the hand-picked stations — live in `user_filters` rather than in the URL or a cookie, so they follow the login from one browser to the next.

## Releases

Build local release binaries for Linux `amd64`, `arm64`, and `armv7`:

```bash
make release
```

Pushing a tag that matches `v*` triggers the GitHub Actions release workflow. It runs tests, builds those three Linux binaries, and publishes a GitHub Release with generated notes.

## Notes

- City geocoding is cached in the database, so Nominatim is only queried once per place unless the cached row is cleared or refreshed. The viewer never writes to that cache: an address or position a reader resolves is stored on their own `user_filters` row, so `cities` holds only the places the CLI collects.
- `update` stores only changed snapshots plus the adjacent unchanged snapshots needed to preserve price graphs.
- A station inside two update targets' radii belongs to the nearest one, and `price_snapshots.city_name` records that owner. It is provenance: `gasoline stations --city` filters on it, and `suggest`/`check` use it to report a distance when nothing better is available. Notifications do not — a subscriber's distance is measured from their own location.
- The `cities` table is a **geocode cache**, nothing more. `update` needs a city's coordinates for the Tankerkönig call and caches them there; the web UI reads it as an autocomplete source when a user or visitor picks a city, resolving the coordinates once at that moment. Neither the notification path nor the forecast reads it at run time. `gasoline import cities <CC>` fills it in bulk so the autocomplete covers a whole country.
- Distance-only changes do not create a new snapshot, but open/closed changes do.
- `import cities` downloads populated-place data from GeoNames and keeps only matching entries for the requested 2-letter country code.
