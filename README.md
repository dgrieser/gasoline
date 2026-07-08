# gasoline

Small Go CLI that stores Tankerkönig gas station prices historically in SQLite or an external MySQL server and ships with a lightweight PHP viewer for browsing the collected data.

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

### Migrating an existing SQLite database to MySQL

`migrate-to-mysql` copies all cities, stations, and price snapshots from a SQLite file into a MySQL server (creating the tables if needed). Snapshot ids are preserved, so history ordering stays identical:

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

Compact existing snapshots in place:

```bash
gasoline compact
```

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
gasoline suggest --city "Berlin" --range-km 10 --fuel diesel --history-days 21 --predict-days 3 --limit-per-day 3
```

The suggestion algorithm uses open historical prices within the range, reconstructs compacted price intervals, buckets them by local weekday and hour, and ranks future day/time windows with a recency-weighted median forecast.

Useful `suggest` flags:

- `--range-km` maximum station distance from the cached city coordinates
- `--fuel diesel|e5|e10`
- `--history-days` amount of historic data to use
- `--predict-days` amount of calendar days to suggest, including today when future hours remain
- `--limit-per-day` maximum suggestions per day
- `--output json` or `-o json`

Suggestion output includes the day, time window, predicted price, confidence, distance, and full persisted station metadata. JSON output keeps the existing top-level station fields and also includes a nested `station` object with address, brand, street, house number, post code, place, coordinates, and first/last seen timestamps.

Check whether the latest stored prices are low right now:

```bash
gasoline check --city "Berlin" --range-km 10 --fuel diesel --history-days 21 --predict-days 3 --limit 5
```

The check command uses the same historical model as `suggest`, compares each open station's latest stored price with recent station history, and scans the coming forecast window for a lower expected price. It prints the station, current price, low/typical/high verdict, buy/wait/hold recommendation, confidence, and best lower future window when one is expected. Run `gasoline update` first when you need fresh current prices.

Set a persistent display-name override for a station — useful when the Tankerkönig name is uninformative. Subsequent `update` runs keep the canonical name in sync but never touch the override, and every output path (CLI, JSON, PHP viewer, watcher notifications) prefers the override when set:

```bash
gasoline rename 474e5046-deaf-4f9b-9a32-9797b778f047 "Pumpe Ecke Bäckerstraße"
gasoline rename --clear 474e5046-deaf-4f9b-9a32-9797b778f047
```

Run continuous buy/suggestion notifications:

```bash
./gasoline-watch.sh \
  --city "Berlin" \
  --radius-km 10 \
  --fuel diesel \
  --history-days 21 \
  --predict-days 3 \
  --check-minutes 15 \
  --suggest-time 07:30 \
  --reset-time 00:00 \
  --check-command 'notify --message {{message}}' \
  --suggest-command 'notify --message {{message}}'
```

The watcher runs `gasoline check` every `--check-minutes` and `gasoline suggest` once per local day after `--suggest-time`. It sends only medium/high-confidence rows: check notifications require `recommendation=buy`; suggestion notifications include all medium/high-confidence suggestions. Rows are sorted ascending by price (current price for check, predicted price for suggest) so the first row is the cheapest. Command templates can use `{{message}}` for the full multiline message, row placeholders such as `{{price}}`, `{{fuel}}`, `{{station_name}}`, `{{distance}}`, `{{confidence}}`, `{{date}}`, `{{start_time}}`, `{{end_time}}`, scalar placeholders `{{cheapest_<field>}}` (sourced from the cheapest row, e.g. `{{cheapest_price}}`, `{{cheapest_station_name}}`), or `{{count}}` for the number of rows. Scalar placeholders substitute once, which makes them useful for non-repeating notification titles.

Each price placeholder has a `*_formatted` variant (`current_price_formatted`, `predicted_price_formatted`, `predicted_current_price_formatted`, `best_future_price_formatted`, `price_formatted`) that truncates after the second decimal without rounding (e.g. `1.685` → `1.68`, `1.7` → `1.70`). The decimal separator follows the active locale (`LC_ALL` / `LC_NUMERIC` / `LANG`), so e.g. `LANG=de_DE.UTF-8` renders `1,68` instead of `1.68`. The `fuel_formatted` variant capitalizes the first letter (`diesel` → `Diesel`, `e5` → `E5`).

Any row placeholder also has an `*_onchange` variant (including formatted ones, e.g. `{{fuel_onchange}}`, `{{date_onchange}}`, `{{weekday_short_formatted_onchange}}`) that renders the value on the first row but stays blank on later rows whenever it matches the previous row — handy for collapsing repeated dates or fuel labels in a multi-row notification. When a template line's only value-producing placeholders are `*_onchange` variants and they all blank out, that whole line is skipped instead of printing a line of leftover static characters or spaces. A line keeps printing as long as any placeholder on it produces a value — an `*_onchange` value that did change, or any regular `{{field}}` — and lines with no placeholders at all are always kept. Time-of-day placeholders (`start_time`, `end_time`, and the `best_future_start_time` / `best_future_end_time` variants) are day-aware: their `*_onchange` form reprints whenever the day it refers to changes, even if the time itself is unchanged (so e.g. an identical `11:00 12:00` window still prints again under a new weekday).

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

Use `--limit 0` with `list stations` or `list history` to return all matching rows.

The grouped commands above are the canonical interface shown by `gasoline help`. The older top-level forms `cities`, `stations`, `history`, and `import-cities` are still accepted as aliases.

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

Serve it locally from the repo root:

```bash
php -S 127.0.0.1:8080 -t web
```

Then open `http://127.0.0.1:8080/`.

## Releases

Build local release binaries for Linux `amd64`, `arm64`, and `armv7`:

```bash
make release
```

Pushing a tag that matches `v*` triggers the GitHub Actions release workflow. It runs tests, builds those three Linux binaries, and publishes a GitHub Release with generated notes.

## Notes

- City geocoding is cached in the database, so Nominatim is only queried once per place unless the cached row is cleared or refreshed.
- `update` stores only changed snapshots plus the adjacent unchanged snapshots needed to preserve price graphs.
- Distance-only changes do not create a new snapshot, but open/closed changes do.
- `import cities` downloads populated-place data from GeoNames and keeps only matching entries for the requested 2-letter country code.
