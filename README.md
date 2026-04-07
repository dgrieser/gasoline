# gasoline

Small Go CLI that stores Tankerkönig gas station prices historically in SQLite.

## Setup

The CLI reads the Tankerkönig API key from the `TANKER_KOENIG_API_KEY` environment variable or from the local `.env` file.
The SQLite database path can be set with `GASOLINE_DB_PATH`.

Install dependencies:

```bash
go mod tidy
```

Run tests:

```bash
make test
```

Install the binary to `/usr/local/bin/gasoline` and copy the PHP viewer into `/var/www/html/gasoline`:

```bash
sudo make install
```

## Commands

Persist a new snapshot for a place:

```bash
gasoline update --city "Berlin, Germany" --radius 5
```

Compact existing snapshots in-place without fetching new data:

```bash
gasoline compact
```

List cached city geocodes:

```bash
gasoline cities
```

List known stations and their latest stored prices:

```bash
gasoline stations --city "Berlin, Germany"
```

Show historical prices for a station:

```bash
gasoline history --station-id 474e5046-deaf-4f9b-9a32-9797b778f047 --fuel diesel
```

## PHP Viewer

There is also a small PHP viewer at `web/index.php` that reads `GASOLINE_DB_PATH` when set, otherwise `gasoline.db`, shows historical prices as a graph, and supports filtering by date range, city, fuel, and selected stations.

Serve it locally from the repo root with PHP's built-in server:

```bash
php -S 127.0.0.1:8080 -t web
```

Then open `http://127.0.0.1:8080/`.

Build release binaries for Linux `amd64`, `arm64`, and `armv7`:

```bash
make release
```

Push a version tag like `v1.2.3` to trigger a GitHub Release named after the tag, with auto-generated release notes and those binaries attached.

## Notes

- City geocoding is cached in SQLite, so Nominatim is only queried once per city unless you delete the cache row.
- `update` stores only changed snapshots and the adjacent unchanged snapshots needed for price graphs. Run `compact` once to apply the same compaction to older databases.
- Default SQLite file is `gasoline.db`. Override it with `GASOLINE_DB_PATH` or `--db`.
