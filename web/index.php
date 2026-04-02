<?php

declare(strict_types=1);

$dbPath = realpath(__DIR__ . '/../gasoline.db') ?: (__DIR__ . '/../gasoline.db');
$errors = [];
$stations = [];
$rows = [];
$summary = [
    'points' => 0,
    'stations' => 0,
    'first_recorded_at' => null,
    'last_recorded_at' => null,
];

$selectedStationIds = array_values(array_filter(
    array_map(
        static fn ($value): string => trim((string) $value),
        (array) ($_GET['station_ids'] ?? [])
    ),
    static fn (string $value): bool => $value !== ''
));

$fromDate = trim((string) ($_GET['from'] ?? ''));
$toDate = trim((string) ($_GET['to'] ?? ''));
$selectedFuel = trim((string) ($_GET['fuel'] ?? 'all'));
$selectedCity = trim((string) ($_GET['city'] ?? ''));
$validFuels = ['all', 'diesel', 'e5', 'e10'];
if (!in_array($selectedFuel, $validFuels, true)) {
    $selectedFuel = 'all';
}

if (!file_exists($dbPath)) {
    $errors[] = sprintf('SQLite database not found at %s', $dbPath);
}

$cities = [];

if ($errors === []) {
    try {
        $pdo = new PDO('sqlite:' . $dbPath);
        $pdo->setAttribute(PDO::ATTR_ERRMODE, PDO::ERRMODE_EXCEPTION);
        $pdo->setAttribute(PDO::ATTR_DEFAULT_FETCH_MODE, PDO::FETCH_ASSOC);

        $stations = $pdo->query(
            <<<'SQL'
            SELECT
                s.id,
                s.name,
                COALESCE(NULLIF(TRIM(s.brand), ''), 'Unknown brand') AS brand,
                TRIM(COALESCE(s.street, '')) AS street,
                TRIM(COALESCE(s.house_number, '')) AS house_number,
                TRIM(COALESCE(s.place, '')) AS place,
                s.last_seen_at
            FROM stations s
            ORDER BY s.name ASC, s.id ASC
            SQL
        )->fetchAll();

        $cities = $pdo->query(
            <<<'SQL'
            SELECT DISTINCT city_name
            FROM price_snapshots
            ORDER BY city_name ASC
            SQL
        )->fetchAll(PDO::FETCH_COLUMN);

        $where = [];
        $params = [];

        if ($fromDate !== '') {
            $from = DateTimeImmutable::createFromFormat('Y-m-d', $fromDate, new DateTimeZone('UTC'));
            if ($from === false) {
                $errors[] = 'Invalid from date.';
            } else {
                $where[] = 'ps.recorded_at >= :from_recorded_at';
                $params[':from_recorded_at'] = $from->setTime(0, 0, 0)->format(DateTimeInterface::RFC3339);
            }
        }

        if ($toDate !== '') {
            $to = DateTimeImmutable::createFromFormat('Y-m-d', $toDate, new DateTimeZone('UTC'));
            if ($to === false) {
                $errors[] = 'Invalid to date.';
            } else {
                $where[] = 'ps.recorded_at <= :to_recorded_at';
                $params[':to_recorded_at'] = $to->setTime(23, 59, 59)->format(DateTimeInterface::RFC3339);
            }
        }

        if ($selectedCity !== '') {
            $where[] = 'ps.city_name = :city_name';
            $params[':city_name'] = $selectedCity;
        }

        if ($selectedStationIds !== []) {
            $placeholders = [];
            foreach ($selectedStationIds as $index => $stationId) {
                $placeholder = ':station_id_' . $index;
                $placeholders[] = $placeholder;
                $params[$placeholder] = $stationId;
            }
            $where[] = 'ps.station_id IN (' . implode(', ', $placeholders) . ')';
        }

        $sql = <<<'SQL'
            SELECT
                ps.station_id,
                s.name AS station_name,
                COALESCE(NULLIF(TRIM(s.brand), ''), 'Unknown brand') AS brand,
                TRIM(COALESCE(s.street, '')) AS street,
                TRIM(COALESCE(s.house_number, '')) AS house_number,
                TRIM(COALESCE(s.place, '')) AS place,
                ps.city_name,
                ps.recorded_at,
                ps.dist_km,
                ps.is_open,
                ps.e5,
                ps.e10,
                ps.diesel
            FROM price_snapshots ps
            INNER JOIN stations s ON s.id = ps.station_id
        SQL;

        if ($where !== []) {
            $sql .= "\nWHERE " . implode("\n  AND ", $where);
        }

        $sql .= "\nORDER BY ps.recorded_at ASC, s.name ASC";

        if ($errors === []) {
            $statement = $pdo->prepare($sql);
            foreach ($params as $key => $value) {
                $statement->bindValue($key, $value);
            }
            $statement->execute();
            $rows = $statement->fetchAll();
        }

        if ($rows !== []) {
            $summary['points'] = count($rows);
            $summary['stations'] = count(array_unique(array_column($rows, 'station_id')));
            $summary['first_recorded_at'] = $rows[0]['recorded_at'];
            $summary['last_recorded_at'] = $rows[count($rows) - 1]['recorded_at'];
        }
    } catch (Throwable $e) {
        $errors[] = $e->getMessage();
    }
}

$chartRows = array_map(static function (array $row): array {
    return [
        'station_id' => $row['station_id'],
        'station_name' => $row['station_name'],
        'brand' => $row['brand'],
        'city_name' => $row['city_name'],
        'recorded_at' => $row['recorded_at'],
        'dist_km' => (float) $row['dist_km'],
        'is_open' => (bool) $row['is_open'],
        'e5' => $row['e5'] !== null ? (float) $row['e5'] : null,
        'e10' => $row['e10'] !== null ? (float) $row['e10'] : null,
        'diesel' => $row['diesel'] !== null ? (float) $row['diesel'] : null,
    ];
}, $rows);

function h(?string $value): string
{
    return htmlspecialchars((string) $value, ENT_QUOTES, 'UTF-8');
}

function stationLabel(array $station): string
{
    $parts = [$station['name']];
    if (($station['brand'] ?? '') !== '' && $station['brand'] !== 'Unknown brand') {
        $parts[] = $station['brand'];
    }

    $address = trim(implode(' ', array_filter([
        $station['street'] ?? '',
        $station['house_number'] ?? '',
        $station['place'] ?? '',
    ])));
    if ($address !== '') {
        $parts[] = $address;
    }

    return implode(' | ', $parts);
}

function formatPrice(mixed $value): string
{
    if ($value === null || $value === '') {
        return '-';
    }
    return number_format((float) $value, 3);
}

?>
<!doctype html>
<html lang="en">
<head>
    <meta charset="utf-8">
    <meta name="viewport" content="width=device-width, initial-scale=1">
    <title>Gasoline History</title>
    <style>
        :root {
            color-scheme: light;
            --bg: #f5f1e8;
            --panel: rgba(255, 252, 246, 0.92);
            --panel-strong: #fffdf8;
            --ink: #1f2a2e;
            --muted: #667377;
            --accent: #b4462a;
            --accent-soft: rgba(180, 70, 42, 0.12);
            --line: rgba(31, 42, 46, 0.12);
            --shadow: 0 18px 50px rgba(49, 36, 22, 0.08);
        }

        * {
            box-sizing: border-box;
        }

        body {
            margin: 0;
            font-family: Georgia, "Iowan Old Style", "Palatino Linotype", serif;
            color: var(--ink);
            background:
                radial-gradient(circle at top left, rgba(180, 70, 42, 0.18), transparent 28%),
                radial-gradient(circle at top right, rgba(46, 95, 107, 0.12), transparent 26%),
                linear-gradient(180deg, #f7f2e9 0%, #efe5d6 100%);
        }

        .page {
            width: min(1200px, calc(100vw - 2rem));
            margin: 2rem auto 3rem;
        }

        .hero, .panel {
            background: var(--panel);
            border: 1px solid rgba(255, 255, 255, 0.6);
            box-shadow: var(--shadow);
            backdrop-filter: blur(10px);
            border-radius: 24px;
        }

        .hero {
            padding: 2rem;
            margin-bottom: 1.25rem;
        }

        h1, h2 {
            margin: 0;
            font-weight: 600;
            letter-spacing: 0.01em;
        }

        h1 {
            font-size: clamp(2rem, 4vw, 3.8rem);
        }

        .lede {
            margin: 0.75rem 0 0;
            max-width: 58rem;
            color: var(--muted);
            line-height: 1.55;
            font-size: 1.02rem;
        }

        .meta {
            margin-top: 1rem;
            display: flex;
            flex-wrap: wrap;
            gap: 0.75rem;
        }

        .chip {
            display: inline-flex;
            align-items: center;
            gap: 0.4rem;
            padding: 0.55rem 0.8rem;
            border-radius: 999px;
            background: var(--accent-soft);
            color: var(--accent);
            font-size: 0.92rem;
        }

        .grid {
            display: grid;
            grid-template-columns: minmax(280px, 360px) minmax(0, 1fr);
            gap: 1.25rem;
        }

        .panel {
            padding: 1.25rem;
        }

        .filters form {
            display: grid;
            gap: 0.95rem;
        }

        label {
            display: grid;
            gap: 0.45rem;
            font-size: 0.94rem;
        }

        input, select, button {
            width: 100%;
            border: 1px solid var(--line);
            border-radius: 14px;
            padding: 0.8rem 0.9rem;
            font: inherit;
            color: var(--ink);
            background: var(--panel-strong);
        }

        select[multiple] {
            min-height: 16rem;
        }

        button {
            cursor: pointer;
            border: none;
            background: linear-gradient(135deg, #b4462a 0%, #8b2f18 100%);
            color: #fff8f0;
            font-weight: 600;
        }

        .secondary-link {
            display: inline-block;
            margin-top: 0.25rem;
            color: var(--muted);
            text-decoration: none;
        }

        .summary {
            display: grid;
            grid-template-columns: repeat(4, minmax(0, 1fr));
            gap: 0.85rem;
            margin-bottom: 1rem;
        }

        .stat {
            padding: 1rem;
            border-radius: 18px;
            background: rgba(255, 255, 255, 0.6);
            border: 1px solid rgba(31, 42, 46, 0.08);
        }

        .stat strong {
            display: block;
            font-size: 1.45rem;
            margin-bottom: 0.25rem;
        }

        .chart-shell {
            padding: 1rem;
            border-radius: 20px;
            background: linear-gradient(180deg, rgba(255,255,255,0.8), rgba(255,255,255,0.55));
            border: 1px solid rgba(31, 42, 46, 0.08);
        }

        .chart-toolbar {
            display: flex;
            flex-wrap: wrap;
            gap: 0.5rem;
            margin-bottom: 0.85rem;
        }

        .toggle {
            width: auto;
            padding: 0.55rem 0.75rem;
            border-radius: 999px;
            border: 1px solid var(--line);
            background: white;
            cursor: pointer;
        }

        .toggle.active {
            background: var(--ink);
            color: white;
        }

        #chart {
            width: 100%;
            min-height: 440px;
            display: block;
        }

        .legend {
            display: flex;
            flex-wrap: wrap;
            gap: 0.75rem;
            margin-top: 0.9rem;
            font-size: 0.92rem;
            color: var(--muted);
        }

        .legend span {
            display: inline-flex;
            align-items: center;
            gap: 0.45rem;
        }

        .legend i {
            width: 12px;
            height: 12px;
            border-radius: 999px;
            display: inline-block;
        }

        .table-wrap {
            margin-top: 1rem;
            overflow: auto;
            border-radius: 18px;
            border: 1px solid rgba(31, 42, 46, 0.08);
        }

        table {
            width: 100%;
            border-collapse: collapse;
            background: rgba(255,255,255,0.62);
        }

        th, td {
            padding: 0.8rem 0.9rem;
            border-bottom: 1px solid rgba(31, 42, 46, 0.08);
            text-align: left;
            vertical-align: top;
        }

        th {
            font-size: 0.85rem;
            text-transform: uppercase;
            letter-spacing: 0.08em;
            color: var(--muted);
        }

        .error {
            padding: 0.9rem 1rem;
            border-radius: 16px;
            background: rgba(159, 35, 12, 0.12);
            color: #8a210d;
            margin-bottom: 0.8rem;
        }

        .empty {
            padding: 1rem 0 0;
            color: var(--muted);
        }

        @media (max-width: 900px) {
            .grid {
                grid-template-columns: 1fr;
            }

            .summary {
                grid-template-columns: repeat(2, minmax(0, 1fr));
            }
        }

        @media (max-width: 600px) {
            .page {
                width: min(100vw - 1rem, 1200px);
                margin: 0.5rem auto 2rem;
            }

            .hero, .panel {
                border-radius: 18px;
            }

            .summary {
                grid-template-columns: 1fr;
            }
        }
    </style>
</head>
<body>
<main class="page">
    <section class="hero">
        <h1>Gasoline price history</h1>
        <p class="lede">
            Filter your SQLite snapshots by date, city, and selected stations. The chart updates client-side from the filtered result set and the table below lists the exact historical rows behind it.
        </p>
        <div class="meta">
            <span class="chip">DB: <?= h($dbPath) ?></span>
            <span class="chip">Fuel view: <?= h(strtoupper($selectedFuel)) ?></span>
        </div>
    </section>

    <div class="grid">
        <aside class="panel filters">
            <h2>Filters</h2>
            <form method="get">
                <label>
                    City
                    <select name="city">
                        <option value="">All cities</option>
                        <?php foreach ($cities as $city): ?>
                            <option value="<?= h((string) $city) ?>" <?= $selectedCity === $city ? 'selected' : '' ?>>
                                <?= h((string) $city) ?>
                            </option>
                        <?php endforeach; ?>
                    </select>
                </label>

                <label>
                    From
                    <input type="date" name="from" value="<?= h($fromDate) ?>">
                </label>

                <label>
                    To
                    <input type="date" name="to" value="<?= h($toDate) ?>">
                </label>

                <label>
                    Fuel
                    <select name="fuel">
                        <?php foreach ($validFuels as $fuel): ?>
                            <option value="<?= h($fuel) ?>" <?= $selectedFuel === $fuel ? 'selected' : '' ?>>
                                <?= h(strtoupper($fuel)) ?>
                            </option>
                        <?php endforeach; ?>
                    </select>
                </label>

                <label>
                    Stations
                    <select name="station_ids[]" multiple>
                        <?php foreach ($stations as $station): ?>
                            <?php $stationId = (string) $station['id']; ?>
                            <option value="<?= h($stationId) ?>" <?= in_array($stationId, $selectedStationIds, true) ? 'selected' : '' ?>>
                                <?= h(stationLabel($station)) ?>
                            </option>
                        <?php endforeach; ?>
                    </select>
                </label>

                <button type="submit">Apply filters</button>
                <a class="secondary-link" href="<?= h(strtok($_SERVER['REQUEST_URI'] ?? '/web/index.php', '?') ?: '/web/index.php') ?>">Reset filters</a>
            </form>
        </aside>

        <section class="panel">
            <?php foreach ($errors as $error): ?>
                <div class="error"><?= h($error) ?></div>
            <?php endforeach; ?>

            <div class="summary">
                <div class="stat">
                    <strong><?= h((string) $summary['points']) ?></strong>
                    Matching snapshots
                </div>
                <div class="stat">
                    <strong><?= h((string) $summary['stations']) ?></strong>
                    Stations shown
                </div>
                <div class="stat">
                    <strong><?= h($summary['first_recorded_at'] ? substr((string) $summary['first_recorded_at'], 0, 10) : '-') ?></strong>
                    First timestamp
                </div>
                <div class="stat">
                    <strong><?= h($summary['last_recorded_at'] ? substr((string) $summary['last_recorded_at'], 0, 10) : '-') ?></strong>
                    Last timestamp
                </div>
            </div>

            <div class="chart-shell">
                <div class="chart-toolbar">
                    <button type="button" class="toggle active" data-fuel="e5">E5</button>
                    <button type="button" class="toggle active" data-fuel="e10">E10</button>
                    <button type="button" class="toggle active" data-fuel="diesel">Diesel</button>
                </div>
                <svg id="chart" viewBox="0 0 960 440" preserveAspectRatio="none" aria-label="Fuel price history chart"></svg>
                <div class="legend" id="legend"></div>
                <?php if ($rows === [] && $errors === []): ?>
                    <div class="empty">No matching snapshots for the current filters.</div>
                <?php endif; ?>
            </div>

            <div class="table-wrap">
                <table>
                    <thead>
                    <tr>
                        <th>Recorded At</th>
                        <th>Station</th>
                        <th>City</th>
                        <th>Open</th>
                        <th>Distance</th>
                        <th>E5</th>
                        <th>E10</th>
                        <th>Diesel</th>
                    </tr>
                    </thead>
                    <tbody>
                    <?php foreach (array_reverse($rows) as $row): ?>
                        <tr>
                            <td><?= h((string) $row['recorded_at']) ?></td>
                            <td><?= h((string) $row['station_name']) ?></td>
                            <td><?= h((string) $row['city_name']) ?></td>
                            <td><?= !empty($row['is_open']) ? 'yes' : 'no' ?></td>
                            <td><?= h(number_format((float) $row['dist_km'], 2)) ?> km</td>
                            <td><?= h(formatPrice($row['e5'])) ?></td>
                            <td><?= h(formatPrice($row['e10'])) ?></td>
                            <td><?= h(formatPrice($row['diesel'])) ?></td>
                        </tr>
                    <?php endforeach; ?>
                    </tbody>
                </table>
            </div>
        </section>
    </div>
</main>

<script>
const chartData = <?= json_encode($chartRows, JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE | JSON_THROW_ON_ERROR) ?>;
const selectedFuel = <?= json_encode($selectedFuel, JSON_THROW_ON_ERROR) ?>;

const fuelConfig = {
    e5: { label: 'E5', color: '#d66b32' },
    e10: { label: 'E10', color: '#2f7d6b' },
    diesel: { label: 'Diesel', color: '#215c8f' },
};

const chart = document.getElementById('chart');
const legend = document.getElementById('legend');
const toggles = [...document.querySelectorAll('.toggle')];
const activeFuels = new Set(selectedFuel === 'all' ? ['e5', 'e10', 'diesel'] : [selectedFuel]);

toggles.forEach((toggle) => {
    const fuel = toggle.dataset.fuel;
    const shouldEnable = activeFuels.has(fuel);
    toggle.classList.toggle('active', shouldEnable);
    toggle.disabled = selectedFuel !== 'all' && fuel !== selectedFuel;
    if (toggle.disabled) {
        toggle.classList.remove('active');
    }
});

function renderChart() {
    chart.innerHTML = '';
    legend.innerHTML = '';

    if (chartData.length === 0) {
        return;
    }

    const visibleRows = chartData.filter((row) => [...activeFuels].some((fuel) => row[fuel] !== null));
    if (visibleRows.length === 0) {
        return;
    }

    const margin = { top: 20, right: 18, bottom: 50, left: 64 };
    const width = 960;
    const height = 440;
    const innerWidth = width - margin.left - margin.right;
    const innerHeight = height - margin.top - margin.bottom;

    const timestamps = visibleRows.map((row) => Date.parse(row.recorded_at));
    const allValues = [];
    for (const fuel of activeFuels) {
        for (const row of visibleRows) {
            if (row[fuel] !== null) {
                allValues.push(row[fuel]);
            }
        }
    }
    if (allValues.length === 0) {
        return;
    }

    let minX = Math.min(...timestamps);
    let maxX = Math.max(...timestamps);
    if (minX === maxX) {
        maxX += 3600 * 1000;
    }

    let minY = Math.min(...allValues);
    let maxY = Math.max(...allValues);
    const paddingY = Math.max((maxY - minY) * 0.12, 0.02);
    minY -= paddingY;
    maxY += paddingY;

    const x = (value) => margin.left + ((value - minX) / (maxX - minX)) * innerWidth;
    const y = (value) => margin.top + innerHeight - ((value - minY) / (maxY - minY)) * innerHeight;

    const ns = 'http://www.w3.org/2000/svg';
    const append = (tag, attrs = {}, text = '') => {
        const el = document.createElementNS(ns, tag);
        for (const [key, value] of Object.entries(attrs)) {
            el.setAttribute(key, String(value));
        }
        if (text) {
            el.textContent = text;
        }
        chart.appendChild(el);
        return el;
    };

    append('rect', {
        x: 0, y: 0, width, height, rx: 24, fill: 'rgba(255,255,255,0.2)'
    });

    for (let i = 0; i <= 4; i += 1) {
        const value = minY + ((maxY - minY) / 4) * i;
        const yPos = y(value);
        append('line', {
            x1: margin.left, y1: yPos, x2: width - margin.right, y2: yPos,
            stroke: 'rgba(31,42,46,0.12)', 'stroke-width': 1
        });
        append('text', {
            x: margin.left - 12, y: yPos + 4, 'text-anchor': 'end',
            'font-size': 12, fill: '#667377'
        }, value.toFixed(3));
    }

    const tickCount = Math.min(6, visibleRows.length);
    for (let i = 0; i < tickCount; i += 1) {
        const index = tickCount === 1 ? 0 : Math.round((visibleRows.length - 1) * (i / (tickCount - 1)));
        const row = visibleRows[index];
        const xPos = x(Date.parse(row.recorded_at));
        append('line', {
            x1: xPos, y1: margin.top, x2: xPos, y2: height - margin.bottom,
            stroke: 'rgba(31,42,46,0.08)', 'stroke-width': 1
        });
        append('text', {
            x: xPos, y: height - 18, 'text-anchor': 'middle',
            'font-size': 12, fill: '#667377'
        }, row.recorded_at.slice(0, 10));
    }

    append('line', {
        x1: margin.left, y1: height - margin.bottom, x2: width - margin.right, y2: height - margin.bottom,
        stroke: 'rgba(31,42,46,0.35)', 'stroke-width': 1.2
    });
    append('line', {
        x1: margin.left, y1: margin.top, x2: margin.left, y2: height - margin.bottom,
        stroke: 'rgba(31,42,46,0.35)', 'stroke-width': 1.2
    });

    const stations = [...new Set(visibleRows.map((row) => row.station_id))];
    const stationStroke = new Map(stations.map((stationId, index) => {
        const hue = (index * 83) % 360;
        return [stationId, `hsl(${hue} 55% 42%)`];
    }));

    for (const stationId of stations) {
        const stationRows = visibleRows.filter((row) => row.station_id === stationId);
        for (const fuel of activeFuels) {
            const series = stationRows.filter((row) => row[fuel] !== null);
            if (series.length === 0) {
                continue;
            }
            const d = series.map((row, index) => {
                const xPos = x(Date.parse(row.recorded_at));
                const yPos = y(row[fuel]);
                return `${index === 0 ? 'M' : 'L'} ${xPos.toFixed(2)} ${yPos.toFixed(2)}`;
            }).join(' ');

            append('path', {
                d,
                fill: 'none',
                stroke: fuelConfig[fuel].color,
                'stroke-width': 2.5,
                'stroke-linejoin': 'round',
                'stroke-linecap': 'round',
                opacity: 0.88
            });

            for (const row of series) {
                const xPos = x(Date.parse(row.recorded_at));
                const yPos = y(row[fuel]);
                const circle = append('circle', {
                    cx: xPos,
                    cy: yPos,
                    r: 3.8,
                    fill: stationStroke.get(stationId),
                    stroke: '#ffffff',
                    'stroke-width': 1.4
                });
                const title = document.createElementNS(ns, 'title');
                title.textContent = `${row.station_name} | ${fuelConfig[fuel].label} ${row[fuel].toFixed(3)} | ${row.recorded_at}`;
                circle.appendChild(title);
            }
        }
    }

    for (const fuel of activeFuels) {
        const item = document.createElement('span');
        const dot = document.createElement('i');
        dot.style.background = fuelConfig[fuel].color;
        item.appendChild(dot);
        item.append(`Fuel line: ${fuelConfig[fuel].label}`);
        legend.appendChild(item);
    }

    for (const stationId of stations) {
        const sample = visibleRows.find((row) => row.station_id === stationId);
        const item = document.createElement('span');
        const dot = document.createElement('i');
        dot.style.background = stationStroke.get(stationId);
        item.appendChild(dot);
        item.append(`Point color: ${sample.station_name}`);
        legend.appendChild(item);
    }
}

if (selectedFuel === 'all') {
    toggles.forEach((toggle) => {
        toggle.addEventListener('click', () => {
            const fuel = toggle.dataset.fuel;
            if (activeFuels.has(fuel)) {
                activeFuels.delete(fuel);
            } else {
                activeFuels.add(fuel);
            }
            if (activeFuels.size === 0) {
                activeFuels.add(fuel);
            }
            toggles.forEach((button) => {
                button.classList.toggle('active', activeFuels.has(button.dataset.fuel));
            });
            renderChart();
        });
    });
}

renderChart();
</script>
</body>
</html>
