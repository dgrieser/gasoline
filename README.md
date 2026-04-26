# gasoline

Small Go CLI that stores Tankerkönig gas station prices historically in SQLite and ships with a lightweight PHP viewer for browsing the collected data.

## Requirements

- Go 1.24+
- A Tankerkönig API key
- PHP with SQLite support if you want to use the web viewer

## Configuration

The CLI reads the Tankerkönig API key from `TANKER_KOENIG_API_KEY`. If that variable is unset, it falls back to a local `.env` file in the repo root.

The SQLite database path defaults to `gasoline.db`. You can override it with either:

- `GASOLINE_DB_PATH`
- `--db /path/to/file.db`

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

Use `--limit 0` with `list stations` or `list history` to return all matching rows.

The grouped commands above are the canonical interface shown by `gasoline help`. The older top-level forms `cities`, `stations`, `history`, and `import-cities` are still accepted as aliases.

## Output Formats

Most commands print human-readable text by default and also support structured JSON output:

```bash
gasoline list stations -o json
gasoline update --city "Berlin, Germany" --output json
```

## PHP Viewer

The viewer lives in `web/index.php`. It reads `GASOLINE_DB_PATH` when set; otherwise it opens `gasoline.db` next to the repo.

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

- City geocoding is cached in SQLite, so Nominatim is only queried once per place unless the cached row is cleared or refreshed.
- `update` stores only changed snapshots plus the adjacent unchanged snapshots needed to preserve price graphs.
- Distance-only changes do not create a new snapshot, but open/closed changes do.
- `import cities` downloads populated-place data from GeoNames and keeps only matching entries for the requested 2-letter country code.
