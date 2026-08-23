<?php
/**
 * Exercises the viewer's decision functions: the dashboard's prediction picker
 * against seeded SQLite databases, and the statistics page's chart bucketing.
 *
 * web/index.php is a monolith that runs a page when included, so the functions
 * under test are lifted out of it by name and evaluated on their own. That is
 * worth the small amount of machinery: these decide what the dashboard's
 * fill-up card and the statistics chart show, and the Go side can only assert
 * about their source text.
 *
 * Run directly (`php web_picker_test.php`) or via `make test`.
 */
declare(strict_types=1);
date_default_timezone_set('UTC');

if (!extension_loaded('pdo_sqlite')) {
    fwrite(STDERR, "web_picker_test: skipping, pdo_sqlite is not available\n");
    exit(0);
}

/** Lift one function's source out of the viewer by counting braces from its body. */
function extractFunction(string $source, string $name): string
{
    $start = strpos($source, "function {$name}(");
    if ($start === false) {
        fwrite(STDERR, "web/index.php no longer defines {$name}\n");
        exit(2);
    }
    $open = strpos($source, '{', $start);
    $depth = 0;
    for ($i = $open, $len = strlen($source); $i < $len; $i++) {
        if ($source[$i] === '{') {
            $depth++;
        } elseif ($source[$i] === '}') {
            $depth--;
            if ($depth === 0) {
                return substr($source, $start, $i - $start + 1);
            }
        }
    }
    fwrite(STDERR, "cannot delimit {$name}\n");
    exit(2);
}

$viewer = file_get_contents(__DIR__ . '/web/index.php');
foreach (['raisedNinePrice', 'loadFilteredPredictions', 'gasolineCommandStatsSeries',
    'gasolineLeadBucketLabels', 'gasolineLeadBucketSql', 'gasolineBreakdownTables'] as $name) {
    eval(extractFunction($viewer, $name));
}

/** Build a throwaway database holding the given runs and their forecast grids. */
function seedRuns(array $runs): PDO
{
    $pdo = new PDO('sqlite::memory:', null, null, [PDO::ATTR_ERRMODE => PDO::ERRMODE_EXCEPTION]);
    $pdo->exec('CREATE TABLE prediction_runs (id INTEGER PRIMARY KEY AUTOINCREMENT, run_at TEXT NOT NULL,
        city_name TEXT NOT NULL, fuel TEXT NOT NULL, suggestion_bias REAL NOT NULL DEFAULT 0)');
    $pdo->exec('CREATE TABLE price_predictions (id INTEGER PRIMARY KEY AUTOINCREMENT, run_id INT NOT NULL,
        station_id TEXT NOT NULL, fuel TEXT NOT NULL, target_start TEXT NOT NULL, target_end TEXT NOT NULL,
        predicted_price REAL NOT NULL, confidence TEXT NOT NULL)');
    foreach ($runs as $run) {
        $stmt = $pdo->prepare('INSERT INTO prediction_runs (run_at, city_name, fuel, suggestion_bias)
            VALUES (?, ?, ?, ?)');
        $stmt->execute([$run['run_at'], 'berlin', $run['fuel'] ?? 'diesel', $run['bias'] ?? 0.0]);
        $runId = (int) $pdo->lastInsertId();
        foreach ($run['rows'] as $row) {
            $ins = $pdo->prepare('INSERT INTO price_predictions (run_id, station_id, fuel, target_start,
                target_end, predicted_price, confidence) VALUES (?, ?, ?, ?, ?, ?, ?)');
            $ins->execute([$runId, $row['station'], $run['fuel'] ?? 'diesel', $row['start'], $row['end'],
                $row['price'], $row['conf'] ?? 'high']);
        }
    }
    return $pdo;
}

/** One forecast window for a station, at a whole hour on the reference day. */
function window(string $station, int $hour, float $price, string $conf = 'high'): array
{
    return [
        'station' => $station,
        'start'   => sprintf('2026-08-21T%02d:00:00Z', $hour),
        'end'     => sprintf('2026-08-21T%02d:00:00Z', $hour + 1),
        'price'   => $price,
        'conf'    => $conf,
    ];
}

const NOW = '2026-08-21T12:00:00+00:00';

$failures = 0;
function check(string $name, $got, $want): void
{
    global $failures;
    if ($got === $want) {
        printf("  ok   %s\n", $name);
        return;
    }
    printf("  FAIL %s\n       got  %s\n       want %s\n", $name, json_encode($got), json_encode($want));
    $failures++;
}

/** Displayed price per station, for assertions that do not care about ordering. */
function pricesByStation(array $out): array
{
    $byStation = [];
    foreach ($out['rows'] as $row) {
        $byStation[$row['s']] = $row['price'];
    }
    ksort($byStation);
    return $byStation;
}

echo "web_picker_test: loadFilteredPredictions\n";

// A later run supersedes an earlier one for the same station.
$pdo = seedRuns([
    ['run_at' => '2026-08-21T10:00:00Z', 'rows' => [window('a', 15, 1.500), window('a', 16, 1.510)]],
    ['run_at' => '2026-08-21T11:00:00Z', 'rows' => [window('a', 15, 1.700), window('a', 16, 1.710)]],
]);
$out = loadFilteredPredictions($pdo, ['a'], ['a' => 1.0], 'diesel', NOW);
check('the newest run wins where both runs cover a station',
    array_map(static fn(array $r): float => $r['price'], $out['rows']), [1.709]);
check('as_of reports the newest run', $out['as_of'], ['diesel' => '2026-08-21T11:00:00Z']);

// The newest run stored nothing future for one station — too little history to
// forecast it, say. That station keeps the last forecast that does exist rather
// than dropping off the card, which is what happened while the newest run was
// resolved by a separate unbounded query.
$pdo = seedRuns([
    ['run_at' => '2026-08-21T10:00:00Z', 'rows' => [window('a', 15, 1.500), window('b', 15, 1.600)]],
    ['run_at' => '2026-08-21T11:00:00Z', 'rows' => [window('a', 15, 1.700)]],
]);
$out = loadFilteredPredictions($pdo, ['a', 'b'], ['a' => 1.0, 'b' => 2.0], 'diesel', NOW);
check('a station the newest run skipped keeps its last forecast',
    pricesByStation($out), ['a' => 1.709, 'b' => 1.609]);

// Two runs recorded in the same second used to both count as newest, because
// runs were compared by run_at; they are compared by run_id now.
$pdo = seedRuns([
    ['run_at' => '2026-08-21T11:00:00Z', 'rows' => [window('a', 15, 1.500)]],
    ['run_at' => '2026-08-21T11:00:00Z', 'rows' => [window('a', 16, 1.800)]],
]);
$out = loadFilteredPredictions($pdo, ['a'], ['a' => 1.0], 'diesel', NOW);
check('two runs in the same second resolve to one, not both',
    array_map(static fn(array $r): float => $r['price'], $out['rows']), [1.809]);

// Only upcoming windows are candidates.
$pdo = seedRuns([
    ['run_at' => '2026-08-21T11:00:00Z', 'rows' => [window('a', 9, 1.400), window('a', 15, 1.700)]],
]);
$out = loadFilteredPredictions($pdo, ['a'], ['a' => 1.0], 'diesel', NOW);
check('windows already past are excluded',
    array_map(static fn(array $r): string => $r['start'], $out['rows']), ['2026-08-21T15:00:00Z']);
$pdo = seedRuns([['run_at' => '2026-08-21T11:00:00Z', 'rows' => [window('a', 9, 1.400)]]]);
check('a scope with nothing upcoming returns nothing',
    loadFilteredPredictions($pdo, ['a'], ['a' => 1.0], 'diesel', NOW), ['rows' => [], 'as_of' => []]);

// The run's recorded display correction reaches the quoted price, and the
// result is normalized to the raised-9 board style.
$pdo = seedRuns([
    ['run_at' => '2026-08-21T11:00:00Z', 'bias' => -0.004, 'rows' => [window('a', 15, 1.700)]],
]);
$out = loadFilteredPredictions($pdo, ['a'], ['a' => 1.0], 'diesel', NOW);
check('suggestion_bias is applied, then the price is raised-9 normalized',
    array_map(static fn(array $r): float => $r['price'], $out['rows']), [1.699]);

// Low confidence still consumes a selection slot before being dropped, like the
// notifier: the cheap low-confidence window is picked first and then filtered,
// so the dearer medium window does not take its place.
$pdo = seedRuns([
    ['run_at' => '2026-08-21T11:00:00Z', 'rows' => [
        window('a', 13, 1.400, 'low'), window('a', 16, 1.500, 'low'), window('a', 19, 1.600, 'low'),
        window('a', 22, 1.900, 'medium'),
    ]],
]);
$out = loadFilteredPredictions($pdo, ['a'], ['a' => 1.0], 'diesel', NOW);
check('cheap low-confidence windows consume slots and are then dropped', $out['rows'], []);

// Out-of-scope stations are never read, whatever the grid holds.
$pdo = seedRuns([
    ['run_at' => '2026-08-21T11:00:00Z', 'rows' => [window('a', 15, 1.700), window('z', 15, 1.100)]],
]);
$out = loadFilteredPredictions($pdo, ['a'], ['a' => 1.0], 'diesel', NOW);
check('a station outside the scope is not shown', pricesByStation($out), ['a' => 1.709]);

// The whole point of the rewrite: one query, where there used to be two.
$counting = new class('sqlite::memory:') extends PDO {
    public int $prepared = 0;
    public function prepare(string $query, array $options = []): PDOStatement|false
    {
        $this->prepared++;
        return parent::prepare($query, $options);
    }
};
$counting->setAttribute(PDO::ATTR_ERRMODE, PDO::ERRMODE_EXCEPTION);
$counting->exec('CREATE TABLE prediction_runs (id INTEGER PRIMARY KEY AUTOINCREMENT, run_at TEXT,
    city_name TEXT, fuel TEXT, suggestion_bias REAL DEFAULT 0)');
$counting->exec('CREATE TABLE price_predictions (id INTEGER PRIMARY KEY AUTOINCREMENT, run_id INT,
    station_id TEXT, fuel TEXT, target_start TEXT, target_end TEXT, predicted_price REAL, confidence TEXT)');
$counting->exec("INSERT INTO prediction_runs (run_at, city_name, fuel) VALUES ('2026-08-21T11:00:00Z', 'berlin', 'diesel')");
$counting->exec("INSERT INTO price_predictions (run_id, station_id, fuel, target_start, target_end,
    predicted_price, confidence) VALUES (1, 'a', 'diesel', '2026-08-21T15:00:00Z', '2026-08-21T16:00:00Z', 1.7, 'high')");
loadFilteredPredictions($counting, ['a'], ['a' => 1.0], 'diesel', NOW);
check('the picker issues one query, not two', $counting->prepared, 1);

// ── Statistics page: chart bucketing ─────────────────────────────────────────
//
// The chart lays every returned bucket out in one equal-width slot, so a period
// with no runs has to come back as a zero. Left out, an outage takes up no
// width at all and the runs on either side render adjacent — hiding exactly the
// stopped timer the page exists to surface.

echo "web_picker_test: gasolineCommandStatsSeries\n";

$statsNow = new DateTimeImmutable('2026-08-21T12:30:00Z', new DateTimeZone('UTC'));

/** One row in the shape the bucket query returns. */
function bucketRow(string $bucket, int $ok, int $partial, int $error, int $running, ?float $avg): array
{
    return ['bucket' => $bucket, 'ok' => $ok, 'partial' => $partial,
        'error_count' => $error, 'running' => $running, 'avg_ms' => $avg];
}

/** The bucket keys of a series, which is what the equal-width layout consumes. */
function bucketKeys(array $series): array
{
    return array_map(static fn (array $b): string => $b['t'], $series);
}

// A four-hour outage in the middle of a six-hour window.
$series = gasolineCommandStatsSeries([
    bucketRow('2026-08-21T07', 3, 0, 0, 0, 500.0),
    bucketRow('2026-08-21T12', 2, 0, 0, 0, 700.0),
], $statsNow, 6, false);
check('an hourly window emits every bucket, gap included', bucketKeys($series), [
    '2026-08-21T06', '2026-08-21T07', '2026-08-21T08', '2026-08-21T09',
    '2026-08-21T10', '2026-08-21T11', '2026-08-21T12',
]);
check('the buckets inside a gap are zeroed', array_slice($series, 2, 3), [
    ['t' => '2026-08-21T08', 'ok' => 0, 'partial' => 0, 'error' => 0, 'running' => 0, 'avg_ms' => null],
    ['t' => '2026-08-21T09', 'ok' => 0, 'partial' => 0, 'error' => 0, 'running' => 0, 'avg_ms' => null],
    ['t' => '2026-08-21T10', 'ok' => 0, 'partial' => 0, 'error' => 0, 'running' => 0, 'avg_ms' => null],
]);
check('a bucket that has runs keeps its counts', $series[1], [
    't' => '2026-08-21T07', 'ok' => 3, 'partial' => 0, 'error' => 0, 'running' => 0, 'avg_ms' => 500.0,
]);

// Daily buckets use the shorter key, matching SUBSTR(started_at, 1, 10).
$series = gasolineCommandStatsSeries([
    bucketRow('2026-08-19', 10, 1, 0, 0, 900.0),
], $statsNow, 72, true);
check('a daily window emits one bucket per day', bucketKeys($series), [
    '2026-08-18', '2026-08-19', '2026-08-20', '2026-08-21',
]);
check('an empty day is zeroed, not dropped', $series[3], [
    't' => '2026-08-21', 'ok' => 0, 'partial' => 0, 'error' => 0, 'running' => 0, 'avg_ms' => null,
]);

// AVG(duration_ms) is NULL when nothing in the bucket finished. Coercing that
// to zero drew the duration line down to "instant" over the hung runs it is
// supposed to be flagging.
$series = gasolineCommandStatsSeries([
    bucketRow('2026-08-21T11', 0, 0, 0, 2, null),
    bucketRow('2026-08-21T12', 1, 0, 0, 1, 1500.0),
], $statsNow, 2, false);
check('an all-unfinished bucket keeps a null duration', $series[1], [
    't' => '2026-08-21T11', 'ok' => 0, 'partial' => 0, 'error' => 0, 'running' => 2, 'avg_ms' => null,
]);
check('a bucket with one finished run reports its duration', $series[2]['avg_ms'], 1500.0);

// Nothing recorded at all still yields a full window, so the chart shows the
// silence rather than nothing.
$series = gasolineCommandStatsSeries([], $statsNow, 3, false);
check('an empty result still spans the window', count($series), 4);
check('every bucket of an empty window is zeroed', array_sum(array_map(
    static fn (array $b): int => $b['ok'] + $b['partial'] + $b['error'] + $b['running'],
    $series
)), 0);

// Buckets the query returns but the window no longer covers are not invented
// back in: the series is the window, in order.
$series = gasolineCommandStatsSeries([
    bucketRow('2026-08-01T05', 9, 0, 0, 0, 100.0),
    bucketRow('2026-08-21T12', 1, 0, 0, 0, 200.0),
], $statsNow, 1, false);
check('the series covers the window and nothing else', bucketKeys($series), [
    '2026-08-21T11', '2026-08-21T12',
]);

echo "web_picker_test: gasolineBreakdownTables\n";

// One pass grouped by (confidence, bucket, hour) summed down each dimension has
// to produce what three separate grouped queries produced. The group sizes below
// are deliberately unequal, because that is exactly when the mean of means
// differs from the mean — the mistake returning SUM and COUNT exists to avoid.
$brRow = static function (string $conf, int $bucket, string $hour, int $n,
    float $abs, float $sum, int $floor, float $predicted = 0.0, float $actual = 0.0): array {
    // hour is a substring of target_start now, so the fixture builds a window at
    // that hour rather than passing the hour separately.
    return ['t' => '2026-07-01T' . $hour . ':00:00Z', 'confidence' => $conf, 'bucket' => $bucket,
        'n' => $n, 'abs_error' => $abs, 'sum_error' => $sum, 'lead_floor' => $floor,
        'sum_predicted' => $predicted, 'sum_actual' => $actual];
};
$tables = gasolineBreakdownTables([
    $brRow('high', 0, '08', 10, 0.100, 0.050, 5),
    $brRow('high', 1, '08', 1, 0.500, -0.500, 61),
    $brRow('low', 0, '09', 4, 0.080, 0.040, 12),
    $brRow('low', 0, '08', 5, 0.250, -0.250, 3),
]);
$byKey = static function (array $list, string $key): array {
    $out = [];
    foreach ($list as $entry) {
        $out[(string) $entry[$key]] = $entry;
    }
    return $out;
};

// high: 11 rows, abs 0.6, sum -0.45. Averaging the two group means instead would
// give 0.3 and -0.225, nothing like it.
$conf = $byKey($tables['by_confidence'], 'confidence');
check('confidence counts are summed across groups', $conf['high']['count'], 11);
check('confidence mae divides summed error by summed count',
    round($conf['high']['mae'], 9), round(0.6 / 11, 9));
check('confidence bias divides summed error by summed count',
    round($conf['high']['bias'], 9), round(-0.45 / 11, 9));
check('the other confidence stays separate', $conf['low']['count'], 9);

// 0-1h spans three input groups across both confidences and two hours.
$lead = $byKey($tables['by_lead'], 'bucket');
check('lead bucket sums across confidences and hours', $lead['0-1h']['count'], 19);
check('bucket indexes are labelled for the client',
    array_map(static fn(array $r): string => $r['bucket'], $tables['by_lead']), ['0-1h', '1-3h']);
check('lead bucket mae', round($lead['0-1h']['mae'], 9), round(0.43 / 19, 9));
check('lead_floor is the minimum across the bucket, not the last seen',
    $lead['0-1h']['lead_floor'], 3);
check('a bucket built from one group keeps its floor', $lead['1-3h']['lead_floor'], 61);

// Hour 08 spans three groups.
$hour = $byKey($tables['by_hour'], 'hour');
check('hour sums across confidences and buckets', $hour['8']['count'], 16);
check('hour mae', round($hour['8']['mae'], 9), round(0.85 / 16, 9));
check('hours come back in ascending order for the chart',
    array_map(static fn(array $r): int => $r['hour'], $tables['by_hour']), [8, 9]);
check('a zero-padded hour becomes an integer',
    array_map(static fn(array $r): string => gettype($r['hour']), $tables['by_hour']),
    ['integer', 'integer']);

check('no rows produces four empty tables', gasolineBreakdownTables([]),
    ['by_confidence' => [], 'by_lead' => [], 'by_hour' => [], 'series' => []]);

// The chart series comes out of the same pass: mean predicted against mean
// actual per window, summed across that window's confidence and bucket groups.
$chart = gasolineBreakdownTables([
    $brRow('high', 0, '09', 2, 0.2, 0.1, 5, 3.40, 3.50),
    $brRow('low', 1, '09', 1, 0.5, -0.5, 61, 1.70, 1.20),
    $brRow('high', 0, '08', 1, 0.1, 0.1, 7, 1.80, 1.90),
])['series'];
check('the series has one point per window', count($chart), 2);
check('windows are ordered oldest first for the chart',
    array_map(static fn(array $p): string => $p['t'], $chart),
    ['2026-07-01T08:00:00Z', '2026-07-01T09:00:00Z']);
check('a window sums its groups before dividing', $chart[1]['n'], 3);
// (3.40 + 1.70) / 3 = 1.7, (3.50 + 1.20) / 3 = 1.5667
check('mean predicted divides the summed price by the summed count', $chart[1]['p'], 1.7);
check('mean actual likewise', $chart[1]['a'], round(4.70 / 3, 4));
check('a single-group window still averages', [$chart[0]['p'], $chart[0]['a'], $chart[0]['n']],
    [1.8, 1.9, 1]);

// The SQL emits one index per label, so the two cannot drift apart silently.
$labels = gasolineLeadBucketLabels();
check('every bucket index the SQL emits has a label',
    substr_count(gasolineLeadBucketSql(), 'WHEN') + 1, count($labels));
check('the labels are the ones the client renders', $labels,
    ['0-1h', '1-3h', '3-6h', '6-12h', '12-24h', '24h+']);
// An index past the end is shown rather than silently dropped.
check('an unlabelled bucket index is still reported',
    gasolineBreakdownTables([$brRow('high', 99, '08', 1, 0.1, 0.1, 7)])['by_lead'][0]['bucket'],
    'bucket 99');

if ($failures > 0) {
    printf("web_picker_test: %d failed\n", $failures);
    exit(1);
}
echo "web_picker_test: ok\n";
