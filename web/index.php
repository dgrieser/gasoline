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
    <title>Gasoline — Price History</title>
    <link rel="preconnect" href="https://fonts.googleapis.com">
    <link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
    <link href="https://fonts.googleapis.com/css2?family=Syne:wght@400;700;800&family=DM+Mono:wght@400;500&display=swap" rel="stylesheet">
    <style>
        :root {
            --bg:          #0d0e11;
            --surface:     #13151a;
            --surface-hi:  #1a1d24;
            --border:      rgba(255,255,255,0.07);
            --border-hi:   rgba(255,255,255,0.14);
            --ink:         #e8eaed;
            --muted:       #6b7280;
            --amber:       #f5a623;
            --amber-dim:   rgba(245,166,35,0.12);
            --amber-glow:  rgba(245,166,35,0.25);
            --e5:          #f5a623;
            --e10:         #34d399;
            --diesel:      #60a5fa;
            --red:         #f87171;
            --mono:        'DM Mono', 'Fira Mono', monospace;
            --sans:        'Syne', system-ui, sans-serif;
        }

        *, *::before, *::after { box-sizing: border-box; margin: 0; }

        html { scroll-behavior: smooth; }

        body {
            font-family: var(--sans);
            background: var(--bg);
            color: var(--ink);
            min-height: 100dvh;
            /* noise texture */
            background-image:
                url("data:image/svg+xml,%3Csvg viewBox='0 0 256 256' xmlns='http://www.w3.org/2000/svg'%3E%3Cfilter id='n'%3E%3CfeTurbulence type='fractalNoise' baseFrequency='0.9' numOctaves='4' stitchTiles='stitch'/%3E%3C/filter%3E%3Crect width='100%25' height='100%25' filter='url(%23n)' opacity='0.04'/%3E%3C/svg%3E"),
                radial-gradient(ellipse 80% 50% at 10% -10%, rgba(245,166,35,0.07) 0%, transparent 60%),
                radial-gradient(ellipse 60% 40% at 90% 110%, rgba(96,165,250,0.05) 0%, transparent 60%),
                var(--bg);
        }

        /* ── Layout ────────────────────────────────────────────── */
        .page {
            width: min(1340px, 100vw - 2rem);
            margin: 0 auto;
            padding: 2rem 0 4rem;
            display: grid;
            gap: 1.5rem;
        }

        /* ── Header ────────────────────────────────────────────── */
        .header {
            display: flex;
            align-items: flex-end;
            justify-content: space-between;
            gap: 1.5rem;
            flex-wrap: wrap;
            padding-bottom: 1.5rem;
            border-bottom: 1px solid var(--border);
        }

        .brand {
            display: flex;
            align-items: center;
            gap: 1rem;
        }

        .brand-icon {
            width: 48px;
            height: 48px;
            border-radius: 14px;
            background: var(--amber-dim);
            border: 1px solid var(--amber-glow);
            display: grid;
            place-items: center;
            flex-shrink: 0;
        }

        .brand-icon svg { width: 24px; height: 24px; }

        h1 {
            font-size: clamp(1.6rem, 3vw, 2.4rem);
            font-weight: 800;
            letter-spacing: -0.03em;
            line-height: 1;
            color: var(--ink);
        }

        h1 em {
            font-style: normal;
            color: var(--amber);
        }

        .tagline {
            font-size: 0.85rem;
            color: var(--muted);
            font-family: var(--mono);
            margin-top: 0.35rem;
        }

        .header-meta {
            display: flex;
            gap: 0.6rem;
            flex-wrap: wrap;
            align-items: center;
        }

        .badge {
            font-family: var(--mono);
            font-size: 0.75rem;
            padding: 0.35rem 0.7rem;
            border-radius: 6px;
            border: 1px solid var(--border-hi);
            color: var(--muted);
            background: var(--surface);
            white-space: nowrap;
        }

        .badge.amber { border-color: var(--amber-glow); color: var(--amber); background: var(--amber-dim); }

        /* ── Two-column body ───────────────────────────────────── */
        .layout {
            display: grid;
            grid-template-columns: 300px minmax(0, 1fr);
            gap: 1.5rem;
            align-items: start;
        }

        /* ── Sidebar ───────────────────────────────────────────── */
        .sidebar {
            position: sticky;
            top: 1.5rem;
            display: grid;
            gap: 1px;
            border-radius: 16px;
            overflow: hidden;
            border: 1px solid var(--border);
            background: var(--border);
        }

        .sidebar-head {
            background: var(--surface);
            padding: 1rem 1.25rem;
            display: flex;
            align-items: center;
            gap: 0.6rem;
        }

        .sidebar-head h2 {
            font-size: 0.78rem;
            text-transform: uppercase;
            letter-spacing: 0.12em;
            font-weight: 700;
            color: var(--muted);
            font-family: var(--mono);
        }

        .sidebar form {
            background: var(--surface);
            padding: 1.25rem;
            display: grid;
            gap: 1rem;
        }

        .field {
            display: grid;
            gap: 0.4rem;
        }

        .field label {
            font-size: 0.72rem;
            text-transform: uppercase;
            letter-spacing: 0.1em;
            color: var(--muted);
            font-family: var(--mono);
            font-weight: 500;
        }

        .field input,
        .field select {
            width: 100%;
            background: var(--surface-hi);
            border: 1px solid var(--border-hi);
            border-radius: 8px;
            padding: 0.65rem 0.8rem;
            font-family: var(--mono);
            font-size: 0.85rem;
            color: var(--ink);
            appearance: none;
            transition: border-color 0.15s;
            outline: none;
        }

        .field input:focus,
        .field select:focus {
            border-color: var(--amber);
            box-shadow: 0 0 0 3px var(--amber-dim);
        }

        .field select[multiple] {
            min-height: 13rem;
            padding: 0.4rem;
        }

        .field select[multiple] option {
            padding: 0.35rem 0.5rem;
            border-radius: 5px;
        }

        .field select[multiple] option:checked {
            background: var(--amber-dim);
            color: var(--amber);
        }

        .sidebar-actions {
            background: var(--surface);
            padding: 1rem 1.25rem;
            display: grid;
            gap: 0.6rem;
        }

        .btn-primary {
            display: block;
            width: 100%;
            padding: 0.75rem 1rem;
            border-radius: 8px;
            border: none;
            background: var(--amber);
            color: #0d0e11;
            font-family: var(--mono);
            font-size: 0.85rem;
            font-weight: 500;
            cursor: pointer;
            letter-spacing: 0.04em;
            text-align: center;
            transition: opacity 0.15s, box-shadow 0.15s;
        }

        .btn-primary:hover {
            opacity: 0.9;
            box-shadow: 0 0 20px var(--amber-glow);
        }

        .btn-reset {
            display: block;
            width: 100%;
            padding: 0.65rem 1rem;
            border-radius: 8px;
            border: 1px solid var(--border-hi);
            background: transparent;
            color: var(--muted);
            font-family: var(--mono);
            font-size: 0.82rem;
            cursor: pointer;
            text-align: center;
            text-decoration: none;
            transition: border-color 0.15s, color 0.15s;
        }

        .btn-reset:hover {
            border-color: var(--border-hi);
            color: var(--ink);
        }

        /* ── Main content ──────────────────────────────────────── */
        .content {
            display: grid;
            gap: 1.25rem;
        }

        /* ── Stats row ─────────────────────────────────────────── */
        .stats {
            display: grid;
            grid-template-columns: repeat(4, 1fr);
            gap: 1px;
            border-radius: 14px;
            overflow: hidden;
            border: 1px solid var(--border);
            background: var(--border);
        }

        .stat {
            background: var(--surface);
            padding: 1.1rem 1.25rem;
        }

        .stat-label {
            font-size: 0.7rem;
            text-transform: uppercase;
            letter-spacing: 0.1em;
            color: var(--muted);
            font-family: var(--mono);
            margin-bottom: 0.5rem;
        }

        .stat-value {
            font-family: var(--mono);
            font-size: 1.5rem;
            font-weight: 500;
            color: var(--amber);
            line-height: 1;
        }

        /* ── Chart card ────────────────────────────────────────── */
        .chart-card {
            background: var(--surface);
            border: 1px solid var(--border);
            border-radius: 14px;
            overflow: hidden;
        }

        .chart-header {
            display: flex;
            align-items: center;
            justify-content: space-between;
            padding: 1rem 1.25rem;
            border-bottom: 1px solid var(--border);
            flex-wrap: wrap;
            gap: 0.75rem;
        }

        .chart-title {
            font-size: 0.78rem;
            text-transform: uppercase;
            letter-spacing: 0.12em;
            font-weight: 700;
            color: var(--muted);
            font-family: var(--mono);
        }

        .fuel-toggles {
            display: flex;
            gap: 0.5rem;
        }

        .fuel-toggle {
            font-family: var(--mono);
            font-size: 0.75rem;
            padding: 0.35rem 0.7rem;
            border-radius: 6px;
            border: 1px solid var(--border-hi);
            background: transparent;
            color: var(--muted);
            cursor: pointer;
            transition: all 0.15s;
            letter-spacing: 0.05em;
        }

        .fuel-toggle[data-fuel="e5"].active  { border-color: var(--e5);     color: var(--e5);     background: rgba(245,166,35,0.1); }
        .fuel-toggle[data-fuel="e10"].active  { border-color: var(--e10);   color: var(--e10);    background: rgba(52,211,153,0.1); }
        .fuel-toggle[data-fuel="diesel"].active { border-color: var(--diesel); color: var(--diesel); background: rgba(96,165,250,0.1); }

        .chart-body {
            padding: 1rem 1.25rem;
        }

        #chart {
            width: 100%;
            display: block;
            min-height: 380px;
        }

        .chart-legend {
            display: flex;
            flex-wrap: wrap;
            gap: 1rem;
            padding: 0.85rem 1.25rem;
            border-top: 1px solid var(--border);
        }

        .legend-item {
            display: flex;
            align-items: center;
            gap: 0.5rem;
            font-family: var(--mono);
            font-size: 0.75rem;
            color: var(--muted);
        }

        .legend-dot {
            width: 8px;
            height: 8px;
            border-radius: 50%;
            flex-shrink: 0;
        }

        .chart-empty {
            padding: 3rem 1.25rem;
            text-align: center;
            font-family: var(--mono);
            font-size: 0.85rem;
            color: var(--muted);
        }

        /* ── Table card ────────────────────────────────────────── */
        .table-card {
            background: var(--surface);
            border: 1px solid var(--border);
            border-radius: 14px;
            overflow: hidden;
        }

        .table-card-header {
            padding: 1rem 1.25rem;
            border-bottom: 1px solid var(--border);
        }

        .table-card-title {
            font-size: 0.78rem;
            text-transform: uppercase;
            letter-spacing: 0.12em;
            font-weight: 700;
            color: var(--muted);
            font-family: var(--mono);
        }

        .table-wrap {
            overflow-x: auto;
        }

        table {
            width: 100%;
            border-collapse: collapse;
        }

        thead th {
            font-family: var(--mono);
            font-size: 0.68rem;
            text-transform: uppercase;
            letter-spacing: 0.1em;
            color: var(--muted);
            padding: 0.75rem 1rem;
            border-bottom: 1px solid var(--border);
            text-align: left;
            white-space: nowrap;
            background: var(--surface-hi);
            font-weight: 500;
        }

        tbody tr {
            border-bottom: 1px solid var(--border);
            transition: background 0.1s;
        }

        tbody tr:last-child { border-bottom: none; }
        tbody tr:hover { background: var(--surface-hi); }

        tbody td {
            font-family: var(--mono);
            font-size: 0.82rem;
            padding: 0.7rem 1rem;
            color: var(--ink);
            vertical-align: middle;
        }

        .td-muted { color: var(--muted); }

        .price-e5     { color: var(--e5); }
        .price-e10    { color: var(--e10); }
        .price-diesel { color: var(--diesel); }

        .open-yes { color: var(--e10); }
        .open-no  { color: var(--muted); }

        /* ── Errors ────────────────────────────────────────────── */
        .error-box {
            background: rgba(248,113,113,0.08);
            border: 1px solid rgba(248,113,113,0.25);
            border-radius: 10px;
            padding: 0.85rem 1rem;
            font-family: var(--mono);
            font-size: 0.82rem;
            color: var(--red);
            margin-bottom: 0.5rem;
        }

        /* ── Responsive ────────────────────────────────────────── */
        @media (max-width: 900px) {
            .layout { grid-template-columns: 1fr; }
            .sidebar { position: static; }
            .stats { grid-template-columns: repeat(2, 1fr); }
        }

        @media (max-width: 560px) {
            .page { width: 100vw; padding: 1rem 0.75rem 3rem; }
            .stats { grid-template-columns: 1fr 1fr; }
            .header { flex-direction: column; align-items: flex-start; }
        }

        /* ── Load animation ────────────────────────────────────── */
        @keyframes fade-up {
            from { opacity: 0; transform: translateY(12px); }
            to   { opacity: 1; transform: translateY(0); }
        }

        .page > * {
            animation: fade-up 0.4s ease both;
        }
        .page > *:nth-child(1) { animation-delay: 0s; }
        .page > *:nth-child(2) { animation-delay: 0.06s; }
        .page > *:nth-child(3) { animation-delay: 0.12s; }
    </style>
</head>
<body>
<main class="page">

    <!-- Header -->
    <header class="header">
        <div class="brand">
            <div class="brand-icon">
                <svg viewBox="0 0 24 24" fill="none" stroke="#f5a623" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round">
                    <path d="M3 22V8l6-6h6l6 6v14"/>
                    <path d="M3 13h18"/>
                    <path d="M9 22v-4a3 3 0 0 1 6 0v4"/>
                    <path d="M19 7l2 2v4"/>
                </svg>
            </div>
            <div>
                <h1>Gasoline <em>/</em> Price History</h1>
                <p class="tagline">Tankerkönig SQLite snapshot viewer</p>
            </div>
        </div>
    </header>

    <!-- Main layout -->
    <div class="layout">

        <!-- Sidebar / filters -->
        <aside class="sidebar">
            <div class="sidebar-head">
                <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" style="color:var(--muted)"><line x1="4" y1="6" x2="20" y2="6"/><line x1="8" y1="12" x2="16" y2="12"/><line x1="11" y1="18" x2="13" y2="18"/></svg>
                <h2>Filters</h2>
            </div>

            <form method="get">
                <div class="field">
                    <label for="f-city">City</label>
                    <select name="city" id="f-city">
                        <option value="">— all cities —</option>
                        <?php foreach ($cities as $city): ?>
                            <option value="<?= h((string) $city) ?>" <?= $selectedCity === $city ? 'selected' : '' ?>>
                                <?= h((string) $city) ?>
                            </option>
                        <?php endforeach; ?>
                    </select>
                </div>

                <div class="field">
                    <label for="f-from">From</label>
                    <input type="date" name="from" id="f-from" value="<?= h($fromDate) ?>">
                </div>

                <div class="field">
                    <label for="f-to">To</label>
                    <input type="date" name="to" id="f-to" value="<?= h($toDate) ?>">
                </div>

                <div class="field">
                    <label for="f-fuel">Fuel type</label>
                    <select name="fuel" id="f-fuel">
                        <?php foreach ($validFuels as $fuel): ?>
                            <option value="<?= h($fuel) ?>" <?= $selectedFuel === $fuel ? 'selected' : '' ?>>
                                <?= h(strtoupper($fuel)) ?>
                            </option>
                        <?php endforeach; ?>
                    </select>
                </div>

                <div class="field">
                    <label>Stations <span style="color:var(--border-hi)">(hold Ctrl to multi-select)</span></label>
                    <select name="station_ids[]" multiple>
                        <?php foreach ($stations as $station): ?>
                            <?php $stationId = (string) $station['id']; ?>
                            <option value="<?= h($stationId) ?>" <?= in_array($stationId, $selectedStationIds, true) ? 'selected' : '' ?>>
                                <?= h(stationLabel($station)) ?>
                            </option>
                        <?php endforeach; ?>
                    </select>
                </div>
            </form>

            <div class="sidebar-actions">
                <button type="submit" form="" onclick="this.closest('aside').querySelector('form').submit()" class="btn-primary">Apply filters</button>
                <a class="btn-reset" href="<?= h(strtok($_SERVER['REQUEST_URI'] ?? '/web/index.php', '?') ?: '/web/index.php') ?>">Reset</a>
            </div>
        </aside>

        <!-- Right column -->
        <div class="content">

            <?php foreach ($errors as $error): ?>
                <div class="error-box"><?= h($error) ?></div>
            <?php endforeach; ?>

            <!-- Stats -->
            <div class="stats">
                <div class="stat">
                    <div class="stat-label">Snapshots</div>
                    <div class="stat-value"><?= h((string) $summary['points']) ?></div>
                </div>
                <div class="stat">
                    <div class="stat-label">Stations</div>
                    <div class="stat-value"><?= h((string) $summary['stations']) ?></div>
                </div>
                <div class="stat">
                    <div class="stat-label">First recorded</div>
                    <div class="stat-value" style="font-size:1rem"><?= h($summary['first_recorded_at'] ? substr((string) $summary['first_recorded_at'], 0, 10) : '—') ?></div>
                </div>
                <div class="stat">
                    <div class="stat-label">Last recorded</div>
                    <div class="stat-value" style="font-size:1rem"><?= h($summary['last_recorded_at'] ? substr((string) $summary['last_recorded_at'], 0, 10) : '—') ?></div>
                </div>
            </div>

            <!-- Chart -->
            <div class="chart-card">
                <div class="chart-header">
                    <span class="chart-title">Price timeline</span>
                    <div class="fuel-toggles">
                        <button type="button" class="fuel-toggle active" data-fuel="e5">E5</button>
                        <button type="button" class="fuel-toggle active" data-fuel="e10">E10</button>
                        <button type="button" class="fuel-toggle active" data-fuel="diesel">Diesel</button>
                    </div>
                </div>
                <?php if ($rows !== [] || $errors !== []): ?>
                    <div class="chart-body">
                        <svg id="chart" viewBox="0 0 960 380" preserveAspectRatio="none" aria-label="Fuel price history chart"></svg>
                    </div>
                    <div class="chart-legend" id="legend"></div>
                <?php else: ?>
                    <div class="chart-empty">No snapshots match the current filters.</div>
                <?php endif; ?>
            </div>

            <!-- Table -->
            <div class="table-card">
                <div class="table-card-header">
                    <span class="table-card-title">Raw snapshots</span>
                </div>
                <div class="table-wrap">
                    <table>
                        <thead>
                        <tr>
                            <th>Recorded at</th>
                            <th>Station</th>
                            <th>City</th>
                            <th>Open</th>
                            <th>Dist</th>
                            <th>E5</th>
                            <th>E10</th>
                            <th>Diesel</th>
                        </tr>
                        </thead>
                        <tbody>
                        <?php foreach (array_reverse($rows) as $row): ?>
                            <tr>
                                <td class="td-muted"><?= h((string) $row['recorded_at']) ?></td>
                                <td><?= h((string) $row['station_name']) ?></td>
                                <td class="td-muted"><?= h((string) $row['city_name']) ?></td>
                                <td class="<?= !empty($row['is_open']) ? 'open-yes' : 'open-no' ?>"><?= !empty($row['is_open']) ? 'open' : 'closed' ?></td>
                                <td class="td-muted"><?= h(number_format((float) $row['dist_km'], 2)) ?> km</td>
                                <td class="price-e5"><?= h(formatPrice($row['e5'])) ?></td>
                                <td class="price-e10"><?= h(formatPrice($row['e10'])) ?></td>
                                <td class="price-diesel"><?= h(formatPrice($row['diesel'])) ?></td>
                            </tr>
                        <?php endforeach; ?>
                        <?php if ($rows === []): ?>
                            <tr><td colspan="8" style="text-align:center;color:var(--muted);padding:2rem;font-family:var(--mono);font-size:.82rem">No data</td></tr>
                        <?php endif; ?>
                        </tbody>
                    </table>
                </div>
            </div>

        </div><!-- /.content -->
    </div><!-- /.layout -->
</main>

<script>
const chartData = <?= json_encode($chartRows, JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE | JSON_THROW_ON_ERROR) ?>;
const selectedFuel = <?= json_encode($selectedFuel, JSON_THROW_ON_ERROR) ?>;

const fuelConfig = {
    e5:     { label: 'E5',     color: '#f5a623', glow: 'rgba(245,166,35,0.18)' },
    e10:    { label: 'E10',    color: '#34d399', glow: 'rgba(52,211,153,0.15)' },
    diesel: { label: 'Diesel', color: '#60a5fa', glow: 'rgba(96,165,250,0.15)' },
};

const chartEl = document.getElementById('chart');
const legendEl = document.getElementById('legend');
const toggles = [...document.querySelectorAll('.fuel-toggle')];

if (!chartEl) {
    // No chart in DOM (empty state)
} else {
    const activeFuels = new Set(selectedFuel === 'all' ? ['e5', 'e10', 'diesel'] : [selectedFuel]);

    toggles.forEach((toggle) => {
        const fuel = toggle.dataset.fuel;
        toggle.classList.toggle('active', activeFuels.has(fuel));
        toggle.disabled = selectedFuel !== 'all' && fuel !== selectedFuel;
        if (toggle.disabled) toggle.classList.remove('active');
    });

    function renderChart() {
        chartEl.innerHTML = '';
        legendEl.innerHTML = '';

        if (chartData.length === 0) return;

        const visibleRows = chartData.filter((row) => [...activeFuels].some((f) => row[f] !== null));
        if (visibleRows.length === 0) return;

        const margin = { top: 24, right: 24, bottom: 48, left: 68 };
        const W = 960, H = 380;
        const iW = W - margin.left - margin.right;
        const iH = H - margin.top - margin.bottom;
        const ns = 'http://www.w3.org/2000/svg';

        const mk = (tag, attrs = {}, parent = chartEl) => {
            const el = document.createElementNS(ns, tag);
            for (const [k, v] of Object.entries(attrs)) el.setAttribute(k, String(v));
            parent.appendChild(el);
            return el;
        };

        // defs for gradients
        const defs = mk('defs');

        const timestamps = visibleRows.map((r) => Date.parse(r.recorded_at));
        const allVals = [];
        for (const f of activeFuels)
            for (const r of visibleRows)
                if (r[f] !== null) allVals.push(r[f]);

        if (!allVals.length) return;

        let minX = Math.min(...timestamps), maxX = Math.max(...timestamps);
        if (minX === maxX) maxX += 3_600_000;

        let minY = Math.min(...allVals), maxY = Math.max(...allVals);
        const padY = Math.max((maxY - minY) * 0.15, 0.02);
        minY -= padY; maxY += padY;

        const px = (v) => margin.left + ((v - minX) / (maxX - minX)) * iW;
        const py = (v) => margin.top + iH - ((v - minY) / (maxY - minY)) * iH;

        // Background
        mk('rect', { x: 0, y: 0, width: W, height: H, fill: '#13151a' });

        // Grid lines
        for (let i = 0; i <= 5; i++) {
            const val = minY + ((maxY - minY) / 5) * i;
            const yp = py(val);
            mk('line', { x1: margin.left, y1: yp, x2: W - margin.right, y2: yp,
                stroke: 'rgba(255,255,255,0.05)', 'stroke-width': 1 });
            mk('text', { x: margin.left - 10, y: yp + 4, 'text-anchor': 'end',
                'font-size': 11, 'font-family': "'DM Mono', monospace", fill: '#6b7280' },
            ).textContent = val.toFixed(3);
        }

        // X ticks
        const tickCount = Math.min(7, visibleRows.length);
        for (let i = 0; i < tickCount; i++) {
            const idx = tickCount === 1 ? 0 : Math.round((visibleRows.length - 1) * (i / (tickCount - 1)));
            const row = visibleRows[idx];
            const xp = px(Date.parse(row.recorded_at));
            mk('line', { x1: xp, y1: margin.top, x2: xp, y2: H - margin.bottom,
                stroke: 'rgba(255,255,255,0.04)', 'stroke-width': 1 });
            mk('text', { x: xp, y: H - 16, 'text-anchor': 'middle',
                'font-size': 10, 'font-family': "'DM Mono', monospace", fill: '#6b7280' },
            ).textContent = row.recorded_at.slice(0, 10);
        }

        // Axes
        mk('line', { x1: margin.left, y1: H - margin.bottom, x2: W - margin.right, y2: H - margin.bottom,
            stroke: 'rgba(255,255,255,0.12)', 'stroke-width': 1 });
        mk('line', { x1: margin.left, y1: margin.top, x2: margin.left, y2: H - margin.bottom,
            stroke: 'rgba(255,255,255,0.12)', 'stroke-width': 1 });

        const stations = [...new Set(visibleRows.map((r) => r.station_id))];
        const stationColors = new Map(stations.map((id, i) => {
            const hue = (i * 97 + 30) % 360;
            return [id, `hsl(${hue} 70% 62%)`];
        }));

        // Area fill + line for each fuel/station combo
        for (const fuel of activeFuels) {
            const cfg = fuelConfig[fuel];
            const gradId = `grad-${fuel}`;
            const grad = mk('linearGradient', { id: gradId, x1: 0, y1: 0, x2: 0, y2: 1 }, defs);
            mk('stop', { offset: '0%',   'stop-color': cfg.color, 'stop-opacity': 0.25 }, grad);
            mk('stop', { offset: '100%', 'stop-color': cfg.color, 'stop-opacity': 0 }, grad);

            for (const stationId of stations) {
                const series = visibleRows.filter((r) => r.station_id === stationId && r[fuel] !== null);
                if (series.length < 2) continue;

                const pts = series.map((r) => [px(Date.parse(r.recorded_at)), py(r[fuel])]);
                const linePath = pts.map(([x, y], j) => `${j === 0 ? 'M' : 'L'}${x.toFixed(2)},${y.toFixed(2)}`).join(' ');
                const bottomLeft = `L${pts.at(-1)[0].toFixed(2)},${(H - margin.bottom).toFixed(2)} L${pts[0][0].toFixed(2)},${(H - margin.bottom).toFixed(2)} Z`;
                const areaPath = linePath + ' ' + bottomLeft;

                mk('path', { d: areaPath, fill: `url(#${gradId})` });
                mk('path', { d: linePath, fill: 'none', stroke: cfg.color,
                    'stroke-width': 2, 'stroke-linejoin': 'round', 'stroke-linecap': 'round', opacity: 0.9 });
            }
        }

        // Dots on top
        for (const fuel of activeFuels) {
            const cfg = fuelConfig[fuel];
            for (const stationId of stations) {
                const series = visibleRows.filter((r) => r.station_id === stationId && r[fuel] !== null);
                for (const row of series) {
                    const xp = px(Date.parse(row.recorded_at));
                    const yp = py(row[fuel]);
                    const g = mk('g', { style: 'cursor:pointer' });
                    mk('circle', { cx: xp, cy: yp, r: 5, fill: '#13151a', stroke: cfg.color, 'stroke-width': 1.5 }, g);
                    const t = document.createElementNS(ns, 'title');
                    t.textContent = `${row.station_name} · ${cfg.label} ${row[fuel].toFixed(3)} € · ${row.recorded_at.slice(0, 16)}`;
                    g.appendChild(t);
                    chartEl.appendChild(g);
                }
            }
        }

        // Legend
        for (const fuel of activeFuels) {
            const cfg = fuelConfig[fuel];
            const item = document.createElement('div');
            item.className = 'legend-item';
            item.innerHTML = `<span class="legend-dot" style="background:${cfg.color}"></span>${cfg.label}`;
            legendEl.appendChild(item);
        }
        for (const stationId of stations) {
            const sample = visibleRows.find((r) => r.station_id === stationId);
            const item = document.createElement('div');
            item.className = 'legend-item';
            item.innerHTML = `<span class="legend-dot" style="background:${stationColors.get(stationId)}"></span>${sample.station_name}`;
            legendEl.appendChild(item);
        }
    }

    if (selectedFuel === 'all') {
        toggles.forEach((toggle) => {
            toggle.addEventListener('click', () => {
                const fuel = toggle.dataset.fuel;
                activeFuels.has(fuel) ? activeFuels.delete(fuel) : activeFuels.add(fuel);
                if (activeFuels.size === 0) activeFuels.add(fuel);
                toggles.forEach((b) => b.classList.toggle('active', activeFuels.has(b.dataset.fuel)));
                renderChart();
            });
        });
    }

    renderChart();
}
</script>
</body>
</html>
