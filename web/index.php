<?php

declare(strict_types=1);

$envDBPath = trim((string) getenv('GASOLINE_DB_PATH'));
$defaultDBPath = realpath(__DIR__ . '/../gasoline.db') ?: (__DIR__ . '/../gasoline.db');
$dbPath = $envDBPath !== '' ? $envDBPath : $defaultDBPath;
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
                COALESCE(NULLIF(TRIM(s.brand), ''), '') AS brand,
                TRIM(COALESCE(s.street, '')) AS street,
                TRIM(COALESCE(s.house_number, '')) AS house_number,
                TRIM(COALESCE(s.place, '')) AS place,
                s.last_seen_at,
                (
                    SELECT ps.dist_km
                    FROM price_snapshots ps
                    WHERE ps.station_id = s.id
                    ORDER BY ps.recorded_at DESC
                    LIMIT 1
                ) AS dist_km
            FROM stations s
            ORDER BY dist_km ASC, s.name ASC, s.id ASC
            SQL
        )->fetchAll();

        $cities = $pdo->query(
            <<<'SQL'
            SELECT
                city_name,
                MIN(dist_km) AS dist_km,
                (
                    SELECT ps2.search_radius_km
                    FROM price_snapshots ps2
                    WHERE ps2.city_name = price_snapshots.city_name
                    ORDER BY ps2.recorded_at DESC
                    LIMIT 1
                ) AS search_radius_km
            FROM price_snapshots
            GROUP BY city_name
            ORDER BY dist_km ASC, city_name ASC
            SQL
        )->fetchAll();

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
                COALESCE(NULLIF(TRIM(s.brand), ''), '') AS brand,
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
        'street' => trim(implode(' ', array_filter([(string) $row['street'], (string) $row['house_number']]))),
        'place' => trim((string) $row['place']),
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
    $name = trim($station['name']);

    $place = trim($station['place'] ?? '');

    $dist = '';
    if ($station['dist_km'] !== null) {
        $dist = number_format((float) $station['dist_km'], 1) . ' km';
    }

    $suffix = implode(' ', array_filter([$place, $dist !== '' ? "({$dist})" : '']));

    return $suffix !== '' ? "{$name}, {$suffix}" : $name;
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
    <script>
        (function () {
            const t = localStorage.getItem('theme') ||
                (window.matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark');
            document.documentElement.setAttribute('data-theme', t);
        })();
    </script>
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

        /* ── Cheapest card ─────────────────────────────────────── */
        .cheapest-card {
            background: var(--surface);
            border: 1px solid var(--border);
            border-radius: 14px;
            overflow: hidden;
        }

        .cheapest-header {
            display: flex;
            align-items: center;
            gap: 0.5rem;
            padding: 0.9rem 1.25rem;
            border-bottom: 1px solid var(--border);
        }

        .cheapest-title {
            font-size: 0.78rem;
            text-transform: uppercase;
            letter-spacing: 0.12em;
            font-weight: 700;
            color: var(--muted);
            font-family: var(--mono);
        }

        .cheapest-grid {
            display: grid;
            grid-template-columns: repeat(3, 1fr);
            gap: 1px;
            background: var(--border);
        }

        .cheapest-grid.single   { grid-template-columns: 1fr; }
        .cheapest-grid.two-col  { grid-template-columns: repeat(2, 1fr); }

        .cheapest-cell {
            background: var(--surface);
            padding: 1.1rem 1.25rem;
        }

        .cheapest-fuel-label {
            font-size: 0.68rem;
            font-family: var(--mono);
            text-transform: uppercase;
            letter-spacing: 0.12em;
            color: var(--muted);
            margin-bottom: 0.45rem;
        }

        .cheapest-price {
            font-family: var(--mono);
            font-size: 1.75rem;
            font-weight: 500;
            line-height: 1;
            margin-bottom: 0.5rem;
            letter-spacing: -0.02em;
        }

        .cheapest-station {
            font-family: var(--mono);
            font-size: 0.75rem;
            color: var(--ink);
            white-space: nowrap;
            overflow: hidden;
            text-overflow: ellipsis;
        }

        .cheapest-time {
            font-family: var(--mono);
            font-size: 0.68rem;
            color: var(--muted);
            margin-top: 0.2rem;
            opacity: 0.7;
        }

        .cheapest-empty {
            padding: 2rem 1.25rem;
            font-family: var(--mono);
            font-size: 0.85rem;
            color: var(--muted);
            text-align: center;
        }

        @media (max-width: 560px) {
            .cheapest-grid,
            .cheapest-grid.two-col { grid-template-columns: 1fr; }
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

        /* ── Light mode overrides ──────────────────────────────── */
        html[data-theme="light"] {
            --bg:         #f4f2ed;
            --surface:    #ffffff;
            --surface-hi: #ece9e2;
            --border:     rgba(0,0,0,0.08);
            --border-hi:  rgba(0,0,0,0.15);
            --ink:        #1c1c1e;
            --muted:      #6e6e73;
            --amber-dim:  rgba(194,120,10,0.08);
            --amber-glow: rgba(194,120,10,0.2);
        }

        html[data-theme="light"] body {
            background-image:
                url("data:image/svg+xml,%3Csvg viewBox='0 0 256 256' xmlns='http://www.w3.org/2000/svg'%3E%3Cfilter id='n'%3E%3CfeTurbulence type='fractalNoise' baseFrequency='0.9' numOctaves='4' stitchTiles='stitch'/%3E%3C/filter%3E%3Crect width='100%25' height='100%25' filter='url(%23n)' opacity='0.025'/%3E%3C/svg%3E"),
                radial-gradient(ellipse 80% 50% at 10% -10%, rgba(245,166,35,0.06) 0%, transparent 60%),
                radial-gradient(ellipse 60% 40% at 90% 110%, rgba(96,165,250,0.04) 0%, transparent 60%),
                var(--bg);
        }

        /* ── Header controls ───────────────────────────────────── */
        .header-controls {
            display: flex;
            align-items: center;
            gap: 0.6rem;
        }

        .lang-picker {
            display: flex;
            border: 1px solid var(--border-hi);
            border-radius: 8px;
            overflow: hidden;
        }

        .lang-btn {
            background: transparent;
            border: none;
            border-right: 1px solid var(--border-hi);
            padding: 0.38rem 0.65rem;
            font-family: var(--mono);
            font-size: 0.72rem;
            color: var(--muted);
            cursor: pointer;
            letter-spacing: 0.07em;
            transition: background 0.15s, color 0.15s;
        }

        .lang-btn:last-child { border-right: none; }

        .lang-btn.active {
            background: var(--amber-dim);
            color: var(--amber);
        }

        .lang-btn:hover:not(.active) { color: var(--ink); }

        .theme-toggle {
            width: 34px;
            height: 34px;
            border-radius: 8px;
            border: 1px solid var(--border-hi);
            background: transparent;
            color: var(--muted);
            cursor: pointer;
            display: grid;
            place-items: center;
            transition: color 0.15s, border-color 0.15s;
            flex-shrink: 0;
        }

        .theme-toggle:hover { color: var(--amber); border-color: var(--amber-glow); }
        .theme-toggle svg { width: 16px; height: 16px; pointer-events: none; }

        /* ── Price tooltip ─────────────────────────────────────── */
        #price-tooltip {
            position: fixed;
            z-index: 200;
            background: var(--surface);
            border: 1px solid var(--border-hi);
            border-radius: 10px;
            padding: 0.6rem 0.9rem;
            font-family: var(--mono);
            font-size: 0.8rem;
            color: var(--ink);
            pointer-events: none;
            line-height: 1.55;
            box-shadow: 0 6px 28px rgba(0,0,0,0.35), 0 1px 6px rgba(0,0,0,0.2);
            display: none;
            min-width: 130px;
            max-width: 240px;
        }

        #price-tooltip .tt-price {
            font-size: 1rem;
            font-weight: 500;
            letter-spacing: 0.03em;
        }

        #price-tooltip .tt-meta {
            color: var(--muted);
            font-size: 0.72rem;
            margin-top: 2px;
        }
    </style>
</head>
<body>
<div id="price-tooltip" role="tooltip" aria-hidden="true"></div>
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
                <h1>Gas<em>o</em>line</h1>
            </div>
        </div>
        <div class="header-controls">
            <div class="lang-picker">
                <button class="lang-btn" data-lang="en">EN</button>
                <button class="lang-btn" data-lang="de">DE</button>
            </div>
            <button class="theme-toggle" id="theme-toggle" aria-label="Toggle theme">
                <svg id="theme-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="5"/><line x1="12" y1="1" x2="12" y2="3"/><line x1="12" y1="21" x2="12" y2="23"/><line x1="4.22" y1="4.22" x2="5.64" y2="5.64"/><line x1="18.36" y1="18.36" x2="19.78" y2="19.78"/><line x1="1" y1="12" x2="3" y2="12"/><line x1="21" y1="12" x2="23" y2="12"/><line x1="4.22" y1="19.78" x2="5.64" y2="18.36"/><line x1="18.36" y1="5.64" x2="19.78" y2="4.22"/></svg>
            </button>
        </div>
    </header>

    <!-- Main layout -->
    <div class="layout">

        <!-- Sidebar / filters -->
        <aside class="sidebar">
            <div class="sidebar-head">
                <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" style="color:var(--muted)"><line x1="4" y1="6" x2="20" y2="6"/><line x1="8" y1="12" x2="16" y2="12"/><line x1="11" y1="18" x2="13" y2="18"/></svg>
                <h2 data-i18n="filters">Filters</h2>
            </div>

            <form method="get">
                <div class="field">
                    <label for="f-city" data-i18n="city">City</label>
                    <select name="city" id="f-city" onchange="this.form.submit()">
                        <option value="" data-i18n="allCities">— all cities —</option>
                        <?php foreach ($cities as $city): ?>
                            <?php
                            $cityName   = (string) $city['city_name'];
                            $cityRadius = $city['search_radius_km'] !== null
                                ? ' (' . number_format((float) $city['search_radius_km'], 0) . ' km)'
                                : '';
                            ?>
                            <option value="<?= h($cityName) ?>" <?= $selectedCity === $cityName ? 'selected' : '' ?>>
                                <?= h($cityName . $cityRadius) ?>
                            </option>
                        <?php endforeach; ?>
                    </select>
                </div>

                <div class="field">
                    <label for="f-from" data-i18n="from">From</label>
                    <input type="date" name="from" id="f-from" value="<?= h($fromDate) ?>" onchange="this.form.submit()">
                </div>

                <div class="field">
                    <label for="f-to" data-i18n="to">To</label>
                    <input type="date" name="to" id="f-to" value="<?= h($toDate) ?>" onchange="this.form.submit()">
                </div>

                <?php
                $fuelI18nKeys = ['all' => 'fuelAll', 'diesel' => 'fuelDiesel', 'e5' => 'fuelE5', 'e10' => 'fuelE10'];
                $fuelLabels   = ['all' => 'All', 'diesel' => 'Diesel', 'e5' => 'E5', 'e10' => 'E10'];
                ?>
                <div class="field">
                    <label for="f-fuel" data-i18n="fuelType">Fuel type</label>
                    <select name="fuel" id="f-fuel" onchange="this.form.submit()">
                        <?php foreach ($validFuels as $fuel): ?>
                            <option value="<?= h($fuel) ?>" data-i18n="<?= h($fuelI18nKeys[$fuel]) ?>" <?= $selectedFuel === $fuel ? 'selected' : '' ?>>
                                <?= h($fuelLabels[$fuel]) ?>
                            </option>
                        <?php endforeach; ?>
                    </select>
                </div>

                <div class="field">
                    <label><span data-i18n="stations">Stations</span> <span style="color:var(--border-hi)" data-i18n="stationsHint">(hold Ctrl to multi-select)</span></label>
                    <select name="station_ids[]" multiple onchange="this.form.submit()">
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
                <a class="btn-reset" href="<?= h(strtok($_SERVER['REQUEST_URI'] ?? '/web/index.php', '?') ?: '/web/index.php') ?>" data-i18n="reset">Reset</a>
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
                    <div class="stat-label" data-i18n="snapshots">Snapshots</div>
                    <div class="stat-value"><?= h((string) $summary['points']) ?></div>
                </div>
                <div class="stat">
                    <div class="stat-label" data-i18n="stationsCount">Stations</div>
                    <div class="stat-value"><?= h((string) $summary['stations']) ?></div>
                </div>
                <div class="stat">
                    <div class="stat-label" data-i18n="firstRecorded">First recorded</div>
                    <div class="stat-value" style="font-size:1rem" <?= $summary['first_recorded_at'] ? 'data-recorded-at="' . h((string) $summary['first_recorded_at']) . '"' : '' ?>><?= h($summary['first_recorded_at'] ? substr((string) $summary['first_recorded_at'], 0, 10) : '—') ?></div>
                </div>
                <div class="stat">
                    <div class="stat-label" data-i18n="lastRecorded">Last recorded</div>
                    <div class="stat-value" style="font-size:1rem" <?= $summary['last_recorded_at'] ? 'data-recorded-at="' . h((string) $summary['last_recorded_at']) . '"' : '' ?>><?= h($summary['last_recorded_at'] ? substr((string) $summary['last_recorded_at'], 0, 10) : '—') ?></div>
                </div>
            </div>

            <!-- Cheapest now -->
            <div class="cheapest-card" id="cheapest-card"></div>

            <!-- Chart -->
            <div class="chart-card">
                <div class="chart-header">
                    <span class="chart-title" data-i18n="priceTimeline">Price timeline</span>
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
                    <div class="chart-empty" data-i18n="noSnapshots">No snapshots match the current filters.</div>
                <?php endif; ?>
            </div>

            <!-- Table -->
            <div class="table-card">
                <div class="table-card-header">
                    <span class="table-card-title" data-i18n="rawSnapshots">Raw snapshots</span>
                </div>
                <div class="table-wrap">
                    <table>
                        <thead>
                        <tr>
                            <th data-i18n="recordedAt">Recorded at</th>
                            <th data-i18n="station">Station</th>
                            <th data-i18n="brand">Brand</th>
                            <th data-i18n="street">Street</th>
                            <th data-i18n="place">Place</th>
                            <th data-i18n="open">Open</th>
                            <th>E5</th>
                            <th>E10</th>
                            <th>Diesel</th>
                        </tr>
                        </thead>
                        <tbody>
                        <?php foreach (array_reverse($rows) as $row): ?>
                            <?php
                            $streetFull = trim(implode(' ', array_filter([
                                (string) $row['street'],
                                (string) $row['house_number'],
                            ])));
                            ?>
                            <tr>
                                <td class="td-muted" data-recorded-at="<?= h((string) $row['recorded_at']) ?>"><?= h((string) $row['recorded_at']) ?></td>
                                <td><?= h((string) $row['station_name']) ?></td>
                                <td class="td-muted"><?= h((string) $row['brand']) ?></td>
                                <td class="td-muted"><?= h($streetFull) ?></td>
                                <td class="td-muted"><?= h((string) $row['place']) ?></td>
                                <td class="<?= !empty($row['is_open']) ? 'open-yes' : 'open-no' ?>" data-i18n="<?= !empty($row['is_open']) ? 'openYes' : 'openNo' ?>"><?= !empty($row['is_open']) ? 'open' : 'closed' ?></td>
                                <td class="price-e5"><?= h(formatPrice($row['e5'])) ?></td>
                                <td class="price-e10"><?= h(formatPrice($row['e10'])) ?></td>
                                <td class="price-diesel"><?= h(formatPrice($row['diesel'])) ?></td>
                            </tr>
                        <?php endforeach; ?>
                        <?php if ($rows === []): ?>
                            <tr><td colspan="9" style="text-align:center;color:var(--muted);padding:2rem;font-family:var(--mono);font-size:.82rem" data-i18n="noData">No data</td></tr>
                        <?php endif; ?>
                        </tbody>
                    </table>
                </div>
            </div>

        </div><!-- /.content -->
    </div><!-- /.layout -->
</main>

<script>
/* ── Locale-aware date/time helpers ────────────────────────────── */
// These reference `currentLang` which is set up below; safe to call after init.
function _tz() { return currentLang === 'de' ? 'Europe/Berlin' : 'UTC'; }
function _loc() { return currentLang === 'de' ? 'de-DE' : 'en-GB'; }

function formatDateTime(isoString) {
    const d = new Date(isoString);
    return d.toLocaleString(_loc(), {
        timeZone: _tz(),
        day: '2-digit', month: '2-digit', year: '2-digit',
        hour: '2-digit', minute: '2-digit',
        hour12: false,
    });
}

function formatTickDate(isoString) {
    const d = new Date(isoString);
    return d.toLocaleDateString(_loc(), {
        timeZone: _tz(),
        day: '2-digit', month: '2-digit',
    });
}

function formatTickTime(isoString) {
    const d = new Date(isoString);
    return d.toLocaleTimeString(_loc(), {
        timeZone: _tz(),
        hour: '2-digit', minute: '2-digit',
        hour12: false,
    });
}

/* ── Station colour helpers ────────────────────────────────────── */
// DJB2-style hash → hue 0-359, stable per station name
function nameToHue(name) {
    let h = 5381;
    for (let i = 0; i < name.length; i++) {
        h = ((h << 5) + h) ^ name.charCodeAt(i);
        h = h >>> 0;
    }
    return h % 360;
}

// Three tints of the station hue, one per fuel type
const FUEL_TINTS = {
    e5:     { s: 82, l: 70 },   // bright
    e10:    { s: 68, l: 55 },   // mid
    diesel: { s: 52, l: 42 },   // deep
};

function stationFuelColor(stationName, fuel) {
    const hue = nameToHue(stationName);
    const { s, l } = FUEL_TINTS[fuel];
    return `hsl(${hue},${s}%,${l}%)`;
}

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

/* ── Tooltip helpers ───────────────────────────────────────────── */
const tooltip = document.getElementById('price-tooltip');

function positionTooltip(e) {
    const x = e.clientX ?? 0;
    const y = e.clientY ?? 0;
    tooltip.style.left = (x + 14) + 'px';
    tooltip.style.top  = (y - 14) + 'px';
    // Clamp to viewport after paint
    requestAnimationFrame(() => {
        const r = tooltip.getBoundingClientRect();
        if (r.right  > window.innerWidth  - 8) tooltip.style.left = (x - r.width  - 14) + 'px';
        if (r.bottom > window.innerHeight - 8) tooltip.style.top  = (y - r.height - 14) + 'px';
    });
}

function showTooltip(e, row, fuel, cfg) {
    const color = stationFuelColor(row.station_name, fuel);
    tooltip.innerHTML =
        `<div class="tt-price" style="color:${color}">${cfg.label} &nbsp;${row[fuel].toFixed(3)} €</div>` +
        `<div class="tt-meta">${row.station_name}</div>` +
        `<div class="tt-meta">${formatDateTime(row.recorded_at)}</div>`;
    tooltip.style.display = 'block';
    positionTooltip(e);
}

let _activeDot = null;
function hideTooltip() {
    tooltip.style.display = 'none';
    if (_activeDot) { _activeDot.setAttribute('opacity', 0); _activeDot = null; }
}

document.addEventListener('touchend', hideTooltip);

// Declared early so renderChart() (called below) can access it via _tz()/_loc().
// Properly initialised later once `translations` is available.
let currentLang = 'en';

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

        const margin = { top: 24, right: 24, bottom: 60, left: 68 };
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

        // no fill — line-only chart

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

        const light = document.documentElement.getAttribute('data-theme') === 'light';
        const chartBg    = light ? '#ffffff' : '#13151a';
        const gridStroke = light ? 'rgba(0,0,0,0.06)' : 'rgba(255,255,255,0.05)';
        const tickStroke = light ? 'rgba(0,0,0,0.05)' : 'rgba(255,255,255,0.04)';
        const axisStroke = light ? 'rgba(0,0,0,0.15)' : 'rgba(255,255,255,0.12)';
        const dotFill    = chartBg;

        // Background
        mk('rect', { x: 0, y: 0, width: W, height: H, fill: chartBg });

        // Grid lines
        for (let i = 0; i <= 5; i++) {
            const val = minY + ((maxY - minY) / 5) * i;
            const yp = py(val);
            mk('line', { x1: margin.left, y1: yp, x2: W - margin.right, y2: yp,
                stroke: gridStroke, 'stroke-width': 1 });
            mk('text', { x: margin.left - 10, y: yp + 4, 'text-anchor': 'end',
                'font-size': 11, 'font-family': "'DM Mono', monospace", fill: '#6b7280' },
            ).textContent = val.toFixed(3);
        }

        // X ticks — two-line: date + time
        const tickCount = Math.min(7, visibleRows.length);
        const tickColor = light ? 'rgba(0,0,0,0.4)' : 'rgba(255,255,255,0.38)';
        for (let i = 0; i < tickCount; i++) {
            const idx = tickCount === 1 ? 0 : Math.round((visibleRows.length - 1) * (i / (tickCount - 1)));
            const row = visibleRows[idx];
            const xp = px(Date.parse(row.recorded_at));
            mk('line', { x1: xp, y1: margin.top, x2: xp, y2: H - margin.bottom,
                stroke: tickStroke, 'stroke-width': 1 });
            const txt = mk('text', { x: xp, y: H - margin.bottom + 14, 'text-anchor': 'middle',
                'font-size': 10, 'font-family': "'DM Mono', monospace", fill: tickColor });
            const tDate = document.createElementNS(ns, 'tspan');
            tDate.setAttribute('x', xp);
            tDate.setAttribute('dy', '0');
            tDate.textContent = formatTickDate(row.recorded_at);
            txt.appendChild(tDate);
            const tTime = document.createElementNS(ns, 'tspan');
            tTime.setAttribute('x', xp);
            tTime.setAttribute('dy', '14');
            tTime.textContent = formatTickTime(row.recorded_at);
            txt.appendChild(tTime);
        }

        // Axes
        mk('line', { x1: margin.left, y1: H - margin.bottom, x2: W - margin.right, y2: H - margin.bottom,
            stroke: axisStroke, 'stroke-width': 1 });
        mk('line', { x1: margin.left, y1: margin.top, x2: margin.left, y2: H - margin.bottom,
            stroke: axisStroke, 'stroke-width': 1 });

        const stations = [...new Set(visibleRows.map((r) => r.station_id))];

        // Line only — per-station colour, per-fuel tint
        for (const fuel of activeFuels) {
            for (const stationId of stations) {
                const series = visibleRows.filter((r) => r.station_id === stationId && r[fuel] !== null);
                if (series.length < 2) continue;

                const color = stationFuelColor(series[0].station_name, fuel);
                const pts = series.map((r) => [px(Date.parse(r.recorded_at)), py(r[fuel])]);
                const linePath = pts.map(([x, y], j) => `${j === 0 ? 'M' : 'L'}${x.toFixed(2)},${y.toFixed(2)}`).join(' ');

                mk('path', { d: linePath, fill: 'none', stroke: color,
                    'stroke-width': 2, 'stroke-linejoin': 'round', 'stroke-linecap': 'round', opacity: 0.9 });
            }
        }

        // Dots on top — per-station colour, per-fuel tint
        for (const fuel of activeFuels) {
            const cfg = fuelConfig[fuel];
            for (const stationId of stations) {
                const series = visibleRows.filter((r) => r.station_id === stationId && r[fuel] !== null);
                for (const row of series) {
                    const xp = px(Date.parse(row.recorded_at));
                    const yp = py(row[fuel]);
                    const color = stationFuelColor(row.station_name, fuel);
                    const g = mk('g', { style: 'cursor:pointer' });
                    // Hit area (invisible, generous for touch)
                    mk('circle', { cx: xp, cy: yp, r: 12, fill: 'transparent' }, g);
                    // Visible dot — hidden until hover/tap
                    const dot = mk('circle', { cx: xp, cy: yp, r: 4.5, fill: dotFill, stroke: color, 'stroke-width': 1.5, opacity: 0 }, g);
                    const _row = row, _cfg = cfg, _fuel = fuel;
                    g.addEventListener('mouseenter', (e) => {
                        if (_activeDot) _activeDot.setAttribute('opacity', 0);
                        _activeDot = dot;
                        dot.setAttribute('opacity', 1);
                        showTooltip(e, _row, _fuel, _cfg);
                    });
                    g.addEventListener('mousemove',  positionTooltip);
                    g.addEventListener('mouseleave', hideTooltip);
                    g.addEventListener('touchstart', (e) => {
                        e.preventDefault();
                        if (_activeDot) _activeDot.setAttribute('opacity', 0);
                        _activeDot = dot;
                        dot.setAttribute('opacity', 1);
                        showTooltip(e.touches[0], _row, _fuel, _cfg);
                    }, { passive: false });
                    chartEl.appendChild(g);
                }
            }
        }

        // Legend — one entry per station; dots show each active fuel tint
        for (const stationId of stations) {
            const sample = visibleRows.find((r) => r.station_id === stationId);
            const item = document.createElement('div');
            item.className = 'legend-item';
            const swatches = [...activeFuels].map((fuel) => {
                const color = stationFuelColor(sample.station_name, fuel);
                const label = fuelConfig[fuel].label;
                return `<span class="legend-dot" title="${label}" style="background:${color}"></span>`;
            }).join('');
            item.innerHTML = `${swatches}${sample.station_name}`;
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

/* ── i18n ──────────────────────────────────────────────────────── */
const translations = {
    en: {
        title: 'Price History',
        filters: 'Filters',
        city: 'City',
        allCities: '— all cities —',
        from: 'From',
        to: 'To',
        fuelType: 'Fuel type',
        fuelAll: 'All',
        fuelDiesel: 'Diesel',
        fuelE5: 'E5',
        fuelE10: 'E10',
        stations: 'Stations',
        stationsHint: '(hold Ctrl to multi-select)',
        reset: 'Reset',
        snapshots: 'Snapshots',
        stationsCount: 'Stations',
        firstRecorded: 'First recorded',
        lastRecorded: 'Last recorded',
        priceTimeline: 'Price timeline',
        rawSnapshots: 'Raw snapshots',
        recordedAt: 'Recorded at',
        station: 'Station',
        brand: 'Brand',
        street: 'Street',
        place: 'Place',
        cityCol: 'City',
        open: 'Open',
        dist: 'Dist',
        openYes: 'open',
        openNo: 'closed',
        noData: 'No data',
        noSnapshots: 'No snapshots match the current filters.',
        cheapestNow: 'Cheapest — last snapshot',
        cheapestNoData: 'No price data available.',
    },
    de: {
        title: 'Preisverlauf',
        filters: 'Filter',
        city: 'Stadt',
        allCities: '— alle Städte —',
        from: 'Von',
        to: 'Bis',
        fuelType: 'Kraftstoffart',
        fuelAll: 'Alle',
        fuelDiesel: 'Diesel',
        fuelE5: 'E5',
        fuelE10: 'E10',
        stations: 'Tankstellen',
        stationsHint: '(Strg für Mehrfachauswahl)',
        reset: 'Zurücksetzen',
        snapshots: 'Einträge',
        stationsCount: 'Tankstellen',
        firstRecorded: 'Erste Aufzeichnung',
        lastRecorded: 'Letzte Aufzeichnung',
        priceTimeline: 'Preisverlauf',
        rawSnapshots: 'Alle Einträge',
        recordedAt: 'Aufgezeichnet um',
        station: 'Tankstelle',
        brand: 'Marke',
        street: 'Straße',
        place: 'Ort',
        cityCol: 'Stadt',
        open: 'Geöffnet',
        dist: 'Entf.',
        openYes: 'offen',
        openNo: 'geschlossen',
        noData: 'Keine Daten',
        noSnapshots: 'Keine Einträge für die aktuellen Filter.',
        cheapestNow: 'Günstigster Preis — letzter Snapshot',
        cheapestNoData: 'Keine Preisdaten vorhanden.',
    },
};

currentLang = (() => {
    const stored = localStorage.getItem('lang');
    if (stored && translations[stored]) return stored;
    const browser = (navigator.language || 'en').slice(0, 2).toLowerCase();
    return translations[browser] ? browser : 'en';
})();

/* ── Cheapest-price box ────────────────────────────────────────── */
const cheapestCard = document.getElementById('cheapest-card');

function renderCheapest() {
    if (!cheapestCard) return;
    const t = translations[currentLang];

    // Most recent snapshot per station
    const latestByStation = new Map();
    for (const row of chartData) {
        const ts = Date.parse(row.recorded_at);
        const prev = latestByStation.get(row.station_id);
        if (!prev || ts > prev.ts) latestByStation.set(row.station_id, { ts, row });
    }
    const latestRows = [...latestByStation.values()].map((v) => v.row);

    const fuels = selectedFuel === 'all' ? ['e5', 'e10', 'diesel'] : [selectedFuel];
    const fuelColors = { e5: 'var(--e5)', e10: 'var(--e10)', diesel: 'var(--diesel)' };

    // Cheapest per fuel across all stations' latest snapshot
    const cheapest = [];
    for (const fuel of fuels) {
        let best = null;
        for (const row of latestRows) {
            if (row[fuel] !== null && (best === null || row[fuel] < best.price)) {
                best = { price: row[fuel], station: row.station_name, street: row.street, place: row.place, recorded_at: row.recorded_at };
            }
        }
        if (best) cheapest.push({ fuel, ...best });
    }


    const colClass = cheapest.length === 1 ? 'single' : cheapest.length === 2 ? 'two-col' : '';

    cheapestCard.innerHTML =
        `<div class="cheapest-header">` +
            `<svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" style="color:var(--amber);flex-shrink:0"><circle cx="12" cy="12" r="10"/><polyline points="8 12 12 8 16 12"/><line x1="12" y1="16" x2="12" y2="8"/></svg>` +
            `<span class="cheapest-title">${t.cheapestNow}</span>` +
        `</div>` +
        (cheapest.length === 0
            ? `<div class="cheapest-empty">${t.cheapestNoData}</div>`
            : `<div class="cheapest-grid${colClass ? ' ' + colClass : ''}">` +
                cheapest.map(({ fuel, price, station, street, place, recorded_at }) => {
                    const addressParts = [street, place].filter(Boolean);
                    const address = addressParts.length ? addressParts.join(', ') : '';
                    return `<div class="cheapest-cell">` +
                        `<div class="cheapest-fuel-label" style="color:${fuelColors[fuel]}">${fuelConfig[fuel].label}</div>` +
                        `<div class="cheapest-price" style="color:${fuelColors[fuel]}">${price.toFixed(3)} <span style="font-size:1rem;opacity:0.7">€</span></div>` +
                        `<div class="cheapest-station">${station}</div>` +
                        (address ? `<div class="cheapest-station" style="opacity:0.6">${address}</div>` : '') +
                        `<div class="cheapest-time">${formatDateTime(recorded_at)}</div>` +
                    `</div>`;
                }).join('') +
              `</div>`
        );
}

function applyLang(lang) {
    currentLang = lang;
    localStorage.setItem('lang', lang);
    const t = translations[lang];
    document.querySelectorAll('[data-i18n]').forEach((el) => {
        const key = el.dataset.i18n;
        if (t[key] !== undefined) el.textContent = t[key];
    });
    document.querySelectorAll('.lang-btn').forEach((btn) => {
        btn.classList.toggle('active', btn.dataset.lang === lang);
    });
    // Re-format all date/time cells
    document.querySelectorAll('[data-recorded-at]').forEach((el) => {
        el.textContent = formatDateTime(el.dataset.recordedAt);
    });
    renderCheapest();
    if (chartEl) renderChart();
}

document.querySelectorAll('.lang-btn').forEach((btn) => {
    btn.addEventListener('click', () => applyLang(btn.dataset.lang));
});

applyLang(currentLang);

/* ── Theme toggle ──────────────────────────────────────────────── */
const moonIcon = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 12.79A9 9 0 1 1 11.21 3a7 7 0 0 0 9.79 9.79z"/></svg>';
const sunIcon  = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="5"/><line x1="12" y1="1" x2="12" y2="3"/><line x1="12" y1="21" x2="12" y2="23"/><line x1="4.22" y1="4.22" x2="5.64" y2="5.64"/><line x1="18.36" y1="18.36" x2="19.78" y2="19.78"/><line x1="1" y1="12" x2="3" y2="12"/><line x1="21" y1="12" x2="23" y2="12"/><line x1="4.22" y1="19.78" x2="5.64" y2="18.36"/><line x1="18.36" y1="5.64" x2="19.78" y2="4.22"/></svg>';

const themeToggle = document.getElementById('theme-toggle');

function applyTheme(theme) {
    document.documentElement.setAttribute('data-theme', theme);
    localStorage.setItem('theme', theme);
    themeToggle.innerHTML = theme === 'light' ? moonIcon : sunIcon;
    if (chartEl) renderChart();
}

themeToggle.addEventListener('click', () => {
    const current = document.documentElement.getAttribute('data-theme') || 'dark';
    applyTheme(current === 'dark' ? 'light' : 'dark');
});

// Sync icon to current theme (set by head script)
applyTheme(document.documentElement.getAttribute('data-theme') || 'dark');
</script>
</body>
</html>
