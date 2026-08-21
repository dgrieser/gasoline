<?php
/**
 * Executes the dashboard's prediction picker against seeded SQLite databases.
 *
 * web/index.php is a monolith that runs a page when included, so the functions
 * under test are lifted out of it by name and evaluated on their own. That is
 * worth the small amount of machinery: the picker decides what the dashboard's
 * fill-up card shows, and the Go side can only assert about its source text.
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
foreach (['raisedNinePrice', 'loadFilteredPredictions'] as $name) {
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

if ($failures > 0) {
    printf("web_picker_test: %d failed\n", $failures);
    exit(1);
}
echo "web_picker_test: ok\n";
