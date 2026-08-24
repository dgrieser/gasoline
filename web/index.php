<?php

declare(strict_types=1);

$envDBPath = trim((string) getenv('GASOLINE_DB_PATH'));
$defaultDBPath = realpath(__DIR__ . '/../gasoline.db') ?: (__DIR__ . '/../gasoline.db');
$dbPath = $envDBPath !== '' ? $envDBPath : $defaultDBPath;
$dbDriver = strtolower(trim((string) getenv('GASOLINE_DB_DRIVER')));
if (!in_array($dbDriver, ['sqlite', 'mysql'], true)) {
    $dbDriver = 'sqlite';
}

function gasolineConnect(string $driver, string $sqlitePath): PDO
{
    if ($driver === 'mysql') {
        $host = trim((string) getenv('GASOLINE_MYSQL_HOST')) ?: '127.0.0.1';
        $port = trim((string) getenv('GASOLINE_MYSQL_PORT')) ?: '3306';
        $database = trim((string) getenv('GASOLINE_MYSQL_DATABASE'));
        $user = trim((string) getenv('GASOLINE_MYSQL_USER'));
        $password = trim((string) getenv('GASOLINE_MYSQL_PASSWORD'));
        if ($database === '' || $user === '') {
            throw new RuntimeException('GASOLINE_MYSQL_DATABASE and GASOLINE_MYSQL_USER must be set when GASOLINE_DB_DRIVER=mysql');
        }
        $dsn = sprintf('mysql:host=%s;port=%s;dbname=%s;charset=utf8mb4', $host, $port, $database);
        $options = [];
        $tls = strtolower(trim((string) getenv('GASOLINE_MYSQL_TLS')));
        switch ($tls) {
            case '':
            case 'false':
                break;
            case 'skip-verify':
            case 'preferred':
                // Encrypt the connection but do not validate the server certificate.
                $options[PDO::MYSQL_ATTR_SSL_VERIFY_SERVER_CERT] = false;
                break;
            case 'true':
                $options[PDO::MYSQL_ATTR_SSL_VERIFY_SERVER_CERT] = true;
                $ca = trim((string) getenv('GASOLINE_MYSQL_SSL_CA'));
                if ($ca !== '') {
                    $options[PDO::MYSQL_ATTR_SSL_CA] = $ca;
                }
                break;
            default:
                throw new RuntimeException(sprintf('invalid GASOLINE_MYSQL_TLS %s (expected true, false, skip-verify, or preferred)', $tls));
        }
        $pdo = new PDO($dsn, $user, $password, $options);
    } else {
        $pdo = new PDO('sqlite:' . $sqlitePath);
    }
    $pdo->setAttribute(PDO::ATTR_ERRMODE, PDO::ERRMODE_EXCEPTION);
    $pdo->setAttribute(PDO::ATTR_DEFAULT_FETCH_MODE, PDO::FETCH_ASSOC);
    if ($driver !== 'mysql') {
        // SQLite enforces foreign keys (incl. the ON DELETE CASCADE cleanup of
        // user_notify_cities) only when enabled per connection.
        $pdo->exec('PRAGMA foreign_keys = ON');
    }

    return $pdo;
}

// ── Auth: session / CSRF / flash helpers ─────────────────────────────────────

function gasolineRequestIsHTTPS(): bool
{
    return (($_SERVER['HTTPS'] ?? '') !== '' && strtolower((string) $_SERVER['HTTPS']) !== 'off')
        || strtolower((string) ($_SERVER['HTTP_X_FORWARDED_PROTO'] ?? '')) === 'https';
}

/**
 * How long a signed-in browser stays signed in without retyping the password.
 * GASOLINE_SESSION_DAYS overrides the default; values outside 1..365 days are
 * clamped rather than rejected, so a typo cannot lock everyone out.
 */
function gasolineSessionDays(): int
{
    $raw = trim((string) getenv('GASOLINE_SESSION_DAYS'));
    if ($raw === '' || !ctype_digit($raw)) {
        return 30;
    }
    return max(1, min(365, (int) $raw));
}

function gasolineSessionTTL(): int
{
    return gasolineSessionDays() * 86400;
}

// Shared cookie attributes for both cookies the viewer sets, so the session and
// the persistent-login cookie can never disagree about scope or protection.
function gasolineCookieOptions(int $expires): array
{
    return [
        'expires' => $expires,
        'path' => '/',
        'httponly' => true,
        'samesite' => 'Lax',
        'secure' => gasolineRequestIsHTTPS(),
    ];
}

/**
 * Point PHP's session storage at a directory of our own.
 *
 * Session files in the shared default directory are garbage-collected by every
 * PHP process on the host, and most of them still run the 24-minute default
 * gc_maxlifetime — so raising ours only holds in a directory nothing else
 * sweeps. When the directory cannot be created or written (open_basedir, a
 * read-only temp), the default path stays in place: a short session beats no
 * session at all, and the persistent-login cookie carries the login anyway.
 */
function gasolineSessionSavePath(): void
{
    $dir = trim((string) getenv('GASOLINE_SESSION_PATH'));
    if ($dir === '') {
        $dir = rtrim(sys_get_temp_dir(), '/\\') . '/gasoline-sessions';
    }
    try {
        if (!is_dir($dir)) {
            @mkdir($dir, 0700, true);
        }
        if (!is_dir($dir) || !is_writable($dir)) {
            return;
        }
        @session_save_path($dir);
    } catch (Throwable $e) {
        error_log('gasoline session path error: ' . $e->getMessage());
    }
}

/**
 * Re-send the session cookie so an in-use login keeps sliding forward.
 *
 * PHP sends the cookie only when it creates or regenerates a session id, so
 * without this the cookie would expire a fixed window after sign-in no matter
 * how often the viewer is used. Once a day is enough and keeps a Set-Cookie
 * header off almost every response.
 */
function gasolineRefreshSessionCookie(): void
{
    if (headers_sent() || session_status() !== PHP_SESSION_ACTIVE) {
        return;
    }
    $now = time();
    $last = (int) ($_SESSION['cookie_refreshed_at'] ?? 0);
    if ($last > 0 && $now - $last < 86400) {
        return;
    }
    $_SESSION['cookie_refreshed_at'] = $now;
    if (!isset($_COOKIE[session_name()])) {
        // Brand-new or regenerated id: session_start() has already sent the
        // cookie with the full lifetime, so a second header would only repeat it.
        return;
    }
    setcookie(session_name(), session_id(), gasolineCookieOptions($now + gasolineSessionTTL()));
}

function gasolineStartSession(): void
{
    $ttl = gasolineSessionTTL();
    ini_set('session.use_strict_mode', '1');
    // Without this the login dies after PHP's default 24 idle minutes, which is
    // what made the viewer ask for the password over and over.
    ini_set('session.gc_maxlifetime', (string) $ttl);
    gasolineSessionSavePath();
    session_set_cookie_params([
        'lifetime' => $ttl,
        'httponly' => true,
        'samesite' => 'Lax',
        'secure' => gasolineRequestIsHTTPS(),
        'path' => '/',
    ]);
    session_name('gasoline_session');
    session_start();
    gasolineRefreshSessionCookie();
}

// Drop the session and its cookie. Used wherever the viewer signs someone out;
// the persistent-login cookie is cleared separately by the caller, because
// signing out of one browser must not always forget every other one.
function gasolineDestroySession(): void
{
    $_SESSION = [];
    if (session_status() === PHP_SESSION_ACTIVE) {
        session_destroy();
    }
    if (!headers_sent()) {
        setcookie(session_name(), '', gasolineCookieOptions(time() - 86400));
    }
    // Keep $_COOKIE honest: a session started again in the same request (the
    // account-deletion flow does that to carry a flash message) then knows it
    // is issuing a fresh cookie rather than refreshing the one just dropped.
    unset($_COOKIE[session_name()]);
}

function csrfToken(): string
{
    if (!isset($_SESSION['csrf']) || !is_string($_SESSION['csrf']) || $_SESSION['csrf'] === '') {
        $_SESSION['csrf'] = bin2hex(random_bytes(32));
    }
    return $_SESSION['csrf'];
}

function csrfField(): string
{
    return '<input type="hidden" name="csrf" value="' . h(csrfToken()) . '">';
}

function csrfValid(): bool
{
    $sent = (string) ($_POST['csrf'] ?? '');
    return $sent !== '' && hash_equals(csrfToken(), $sent);
}

function setFlash(string $type, string $key): void
{
    $_SESSION['flash'] = ['type' => $type, 'key' => $key];
}

function takeFlash(): ?array
{
    $flash = $_SESSION['flash'] ?? null;
    unset($_SESSION['flash']);
    return is_array($flash) ? $flash : null;
}

// English fallback texts for flash messages; the client i18n re-translates
// them via the data-i18n key (both en and de exist in the translations map).
function flashText(string $key): string
{
    $texts = [
        'csrfError' => 'The form has expired. Please try again.',
        'invalidCredentials' => 'Invalid email address or password.',
        'awaitingApproval' => 'Your account is awaiting approval by an administrator.',
        'registerPendingSent' => 'Account created. You will receive an email once an administrator approves it.',
        'accountCreated' => 'Account created. You can log in now.',
        'invalidEmail' => 'Please enter a valid email address.',
        'emailTaken' => 'An account with this email address already exists.',
        'passwordTooShort' => 'The password must be at least 10 characters long.',
        'passwordMismatch' => 'The passwords do not match.',
        'wrongPassword' => 'The current password is incorrect.',
        'passwordChanged' => 'Password changed.',
        'notifySaved' => 'Notification settings saved.',
        'invalidNotifySettings' => 'Invalid notification settings. Check days, time windows, and times.',
    'invalidNotifyLocation' => 'Pick a city from the suggestions and a radius between 1 and 100 km.',
        'lastAdminGuard' => 'You are the last administrator and cannot delete this account.',
        'accountDeleted' => 'Your account has been deleted.',
        'confirmRequired' => 'Please confirm the deletion.',
        'userApproved' => 'User approved.',
        'userApprovedEmailFailed' => 'User approved, but the notification email could not be sent.',
        'userDeleted' => 'User deleted.',
        'userPromoted' => 'User is now an administrator.',
        'userDemoted' => 'User is no longer an administrator.',
        'cannotActOnSelf' => 'You cannot perform this action on your own account.',
        'settingsSaved' => 'Settings saved.',
        'invalidSettings' => 'Invalid settings. Please check the highlighted values.',
        'targetAdded' => 'Update target added.',
        'targetRemoved' => 'Update target removed.',
        'invalidTarget' => 'Invalid city or radius (1-25 km).',
        'targetExists' => 'This city is already an update target.',
        'stationRenamed' => 'Station renamed.',
        'renameCleared' => 'Rename removed. The original name is used again.',
        'invalidRename' => 'Select a station and enter a non-empty new name.',
        'notFound' => 'The requested item was not found.',
        'loggedOut' => 'You have been signed out.',
    ];
    return $texts[$key] ?? $key;
}

function redirectTo(string $query): never
{
    $base = strtok((string) ($_SERVER['REQUEST_URI'] ?? ''), '?') ?: '';
    header('Location: ' . $base . $query);
    exit;
}

// ── Auth: schema guard ────────────────────────────────────────────────────────

/**
 * Report whether one table exists, using the same driver-aware catalog lookup
 * as gasolineSchemaReady. Used for tables added after a deployment's database
 * was created, so the page degrades instead of erroring.
 */
function gasolineTableExists(PDO $pdo, string $driver, string $table): bool
{
    try {
        if ($driver === 'mysql') {
            $stmt = $pdo->prepare(
                'SELECT COUNT(*) AS n FROM information_schema.tables
                 WHERE table_schema = DATABASE() AND table_name = :t'
            );
        } else {
            $stmt = $pdo->prepare(
                "SELECT COUNT(*) AS n FROM sqlite_master WHERE type = 'table' AND name = :t"
            );
        }
        $stmt->bindValue(':t', $table);
        $stmt->execute();
        $row = $stmt->fetch();
        return (int) ($row['n'] ?? 0) > 0;
    } catch (Throwable $e) {
        error_log('gasoline table check error: ' . $e->getMessage());
        return false;
    }
}

function gasolineIndexExists(PDO $pdo, string $driver, string $table, string $index): bool
{
    try {
        if ($driver === 'mysql') {
            $stmt = $pdo->prepare(
                'SELECT COUNT(*) AS n FROM information_schema.statistics
                 WHERE table_schema = DATABASE() AND table_name = :t AND index_name = :i'
            );
        } else {
            $stmt = $pdo->prepare(
                "SELECT COUNT(*) AS n FROM sqlite_master WHERE type = 'index' AND tbl_name = :t AND name = :i"
            );
        }
        $stmt->bindValue(':t', $table);
        $stmt->bindValue(':i', $index);
        $stmt->execute();
        $row = $stmt->fetch();
        return (int) ($row['n'] ?? 0) > 0;
    } catch (Throwable $e) {
        error_log('gasoline index check error: ' . $e->getMessage());
        return false;
    }
}

/**
 * The lead-time buckets the accuracy page groups by, in order.
 *
 * They are indexes rather than labels in SQL and labels only when they reach the
 * client: a GROUP BY key is evaluated once per row of the slice, and returning
 * the six strings instead of six small integers measured 674 ms more on a
 * 1.39M-row slice for exactly the same 336 groups.
 *
 * @return array<int, string>
 */
function gasolineLeadBucketLabels(): array
{
    return ['0-1h', '1-3h', '3-6h', '6-12h', '12-24h', '24h+'];
}

/**
 * SQL bucketing lead_minutes into the index of its label above.
 *
 * The 3-6h boundary is deliberately 360 minutes, the cutoff below which
 * predictions.go lets predictions train the bias correction, so the table shows
 * both sides of that line.
 */
function gasolineLeadBucketSql(): string
{
    return 'CASE'
        . ' WHEN pp.lead_minutes < 60 THEN 0'
        . ' WHEN pp.lead_minutes < 180 THEN 1'
        . ' WHEN pp.lead_minutes < 360 THEN 2'
        . ' WHEN pp.lead_minutes < 720 THEN 3'
        . ' WHEN pp.lead_minutes < 1440 THEN 4'
        . ' ELSE 5 END';
}

/**
 * Sum one breakdown pass down each of its three dimensions.
 *
 * The accuracy page's confidence, lead-time and hour tables are three groupings
 * of the same rows. They used to be three queries, each walking the whole
 * filtered slice again — on a production database 1.39M index entries read three
 * times, 4.5-6 s, to produce 3, 6 and 24 rows. One pass grouped by all three
 * keys returns at most 3 x 6 x 24 = 432 rows, and each table is summed out of
 * those here.
 *
 * The query has to return SUM and COUNT rather than AVG for this to be possible:
 * the mean of means is not the mean unless every group is the same size. Adding
 * the sums and dividing once per table is exact, not an approximation.
 *
 * @param array<int, array<string, mixed>> $rows one row per (target_start, confidence, bucket)
 * @return array{by_confidence: array<int, array<string, mixed>>, by_lead: array<int, array<string, mixed>>, by_hour: array<int, array<string, mixed>>, series: array<int, array<string, mixed>>}
 */
function gasolineBreakdownTables(array $rows): array
{
    $byConfidence = [];
    $byLead = [];
    $byHour = [];
    $byTarget = [];

    // lead_floor is carried through as the JS sort key for the lead table, which
    // orders buckets by their lower bound rather than by the label — otherwise
    // "12-24h" sorts before "3-6h".
    $add = static function (array &$into, string $key, int $n, float $absError, float $sumError, ?int $floor): void {
        if (!isset($into[$key])) {
            $into[$key] = ['count' => 0, 'abs' => 0.0, 'sum' => 0.0, 'floor' => $floor];
        }
        $into[$key]['count'] += $n;
        $into[$key]['abs'] += $absError;
        $into[$key]['sum'] += $sumError;
        if ($floor !== null && ($into[$key]['floor'] === null || $floor < $into[$key]['floor'])) {
            $into[$key]['floor'] = $floor;
        }
    };

    foreach ($rows as $row) {
        $n = (int) $row['n'];
        $absError = (float) $row['abs_error'];
        $sumError = (float) $row['sum_error'];
        $target = (string) $row['t'];
        $add($byConfidence, (string) $row['confidence'], $n, $absError, $sumError, null);
        $add($byLead, (string) $row['bucket'], $n, $absError, $sumError, (int) $row['lead_floor']);
        // Target windows are hourly and target_start is fixed-width RFC3339, so
        // characters 12-13 are the UTC hour.
        $add($byHour, substr($target, 11, 2), $n, $absError, $sumError, null);

        // The chart wants mean predicted against mean actual per window, which
        // is the same sum-then-divide as the tables above on different columns.
        if (!isset($byTarget[$target])) {
            $byTarget[$target] = ['count' => 0, 'predicted' => 0.0, 'actual' => 0.0];
        }
        $byTarget[$target]['count'] += $n;
        $byTarget[$target]['predicted'] += (float) $row['sum_predicted'];
        $byTarget[$target]['actual'] += (float) $row['sum_actual'];
    }

    // A group with no rows cannot occur — GROUP BY only returns keys that
    // matched — but the guard keeps a division by zero impossible if that ever
    // stops being true.
    $mean = static function (float $total, int $count): float {
        return $count > 0 ? $total / $count : 0.0;
    };

    $out = ['by_confidence' => [], 'by_lead' => [], 'by_hour' => [], 'series' => []];
    foreach ($byConfidence as $confidence => $agg) {
        $out['by_confidence'][] = [
            'confidence' => (string) $confidence,
            'count' => $agg['count'],
            'mae' => $mean($agg['abs'], $agg['count']),
            'bias' => $mean($agg['sum'], $agg['count']),
        ];
    }
    $labels = gasolineLeadBucketLabels();
    ksort($byLead);
    foreach ($byLead as $bucket => $agg) {
        $out['by_lead'][] = [
            // The client shows the label; the index was only ever a cheap
            // group key. An index outside the list cannot occur unless the SQL
            // and the labels drift, and then showing it is better than hiding it.
            'bucket' => $labels[(int) $bucket] ?? ('bucket ' . (string) $bucket),
            'count' => $agg['count'],
            'mae' => $mean($agg['abs'], $agg['count']),
            'bias' => $mean($agg['sum'], $agg['count']),
            'lead_floor' => (int) ($agg['floor'] ?? 0),
        ];
    }
    ksort($byHour);
    foreach ($byHour as $hour => $agg) {
        $out['by_hour'][] = [
            'hour' => (int) $hour,
            'count' => $agg['count'],
            'mae' => $mean($agg['abs'], $agg['count']),
            'bias' => $mean($agg['sum'], $agg['count']),
        ];
    }
    // Oldest window first: the chart plots left to right in time, and the query
    // no longer orders its rows because this sort is cheaper than one over the
    // whole slice.
    ksort($byTarget);
    foreach ($byTarget as $target => $agg) {
        $out['series'][] = [
            't' => (string) $target,
            'p' => round($mean($agg['predicted'], $agg['count']), 4),
            'a' => round($mean($agg['actual'], $agg['count']), 4),
            'n' => $agg['count'],
        ];
    }
    return $out;
}

// gasolineAccuracyIndexHint returns the index hint the accuracy page's aggregate
// queries carry, or '' when it must not be used.
//
// Hinting is a last resort and it is here on measurement, not principle. Those
// queries are all covered by idx_price_predictions_accuracy, but both engines
// prefer the narrower idx_price_predictions_due, which leads with fuel too and
// is cheaper per row — and then they fetch a quarter of a million rows from the
// table. On the live MySQL, forcing the covering index measured 66-73% faster
// per query with byte-identical results; refreshing the statistics the
// optimizer reasons from (ANALYZE TABLE, and a histogram on target_start) did
// not change its choice.
//
// The hint is omitted when the index is absent, so a database that has not run
// `gasoline migrate` still renders the page instead of erroring on an unknown
// key. Re-check the decision with `gasoline doctor --try-index
// idx_price_predictions_accuracy`, which times each query both ways: if the
// forced plan stops winning, drop the hint rather than keeping it on faith.
function gasolineAccuracyIndexHint(PDO $pdo, string $driver): string
{
    if (!gasolineIndexExists($pdo, $driver, 'price_predictions', 'idx_price_predictions_accuracy')) {
        return '';
    }
    if ($driver === 'mysql') {
        return 'FORCE INDEX (idx_price_predictions_accuracy)';
    }
    return 'INDEXED BY idx_price_predictions_accuracy';
}

function gasolineSchemaReady(PDO $pdo, string $driver): bool
{
    try {
        if ($driver === 'mysql') {
            $stmt = $pdo->query(
                "SELECT COUNT(*) AS n FROM information_schema.tables
                 WHERE table_schema = DATABASE()
                   AND table_name IN ('users', 'user_filters', 'settings', 'update_targets')"
            );
        } else {
            $stmt = $pdo->query(
                "SELECT COUNT(*) AS n FROM sqlite_master
                 WHERE type = 'table'
                   AND name IN ('users', 'user_filters', 'settings', 'update_targets')"
            );
        }
        $row = $stmt->fetch();
        return (int) ($row['n'] ?? 0) === 4;
    } catch (Throwable $e) {
        error_log('gasoline schema check error: ' . $e->getMessage());
        return false;
    }
}

// gasolineCommandStatsSeries turns the statistics page's bucket query into the
// chart's series, filling in the buckets the GROUP BY never produced.
//
// Two things depend on this. The chart lays buckets out in equal-width slots,
// so a period with no runs at all has to be present as a zero — an absent
// bucket takes up no width, closing the gap and hiding the very outage the page
// exists to reveal. And a bucket whose runs are all unfinished has no average
// duration: that stays null rather than becoming zero, so the duration line
// breaks across it instead of plunging to "instant".
//
// Everything is UTC — started_at is RFC3339 Z and $now carries the UTC zone —
// so there are no DST-length days to step over.
function gasolineCommandStatsSeries(array $rows, DateTimeImmutable $now, int $hours, bool $daily): array
{
    $found = [];
    foreach ($rows as $row) {
        $found[(string) $row['bucket']] = [
            't' => (string) $row['bucket'],
            'ok' => (int) $row['ok'],
            'partial' => (int) $row['partial'],
            'error' => (int) $row['error_count'],
            'running' => (int) $row['running'],
            'avg_ms' => $row['avg_ms'] !== null ? (float) $row['avg_ms'] : null,
        ];
    }

    // The key format has to match the SUBSTR widths the query groups by:
    // characters 1-13 are 'YYYY-MM-DDTHH', 1-10 are 'YYYY-MM-DD'.
    $format = $daily ? 'Y-m-d' : 'Y-m-d\TH';
    $step = $daily ? '+1 day' : '+1 hour';
    $floor = static fn (DateTimeImmutable $t): DateTimeImmutable => $daily
        ? $t->setTime(0, 0)
        : $t->setTime((int) $t->format('G'), 0);

    $cursor = $floor($now->modify('-' . $hours . ' hours'));
    $last = $floor($now);

    $series = [];
    // At most 25 hourly or 31 daily buckets; the guard only keeps a malformed
    // window from spinning.
    for ($guard = 0; $cursor <= $last && $guard < 800; $guard++) {
        $key = $cursor->format($format);
        $series[] = $found[$key] ?? [
            't' => $key,
            'ok' => 0,
            'partial' => 0,
            'error' => 0,
            'running' => 0,
            'avg_ms' => null,
        ];
        $cursor = $cursor->modify($step);
    }

    return $series;
}

// ── Auth: user helpers ────────────────────────────────────────────────────────

function normalizeEmail(string $email): string
{
    return strtolower(trim($email));
}

function findUserByEmail(PDO $pdo, string $email): ?array
{
    $stmt = $pdo->prepare('SELECT * FROM users WHERE email = :email');
    $stmt->bindValue(':email', normalizeEmail($email));
    $stmt->execute();
    $user = $stmt->fetch();
    return $user === false ? null : $user;
}

function findUserByID(PDO $pdo, int $id): ?array
{
    $stmt = $pdo->prepare('SELECT * FROM users WHERE id = :id');
    $stmt->bindValue(':id', $id, PDO::PARAM_INT);
    $stmt->execute();
    $user = $stmt->fetch();
    return $user === false ? null : $user;
}

function currentUser(PDO $pdo, string $driver): ?array
{
    static $cached = false;
    static $user = null;
    if ($cached) {
        return $user;
    }
    $cached = true;
    $userId = $_SESSION['user_id'] ?? null;
    if (!is_int($userId) && !ctype_digit((string) $userId)) {
        return $user = null;
    }
    $row = findUserByID($pdo, (int) $userId);
    if ($row === null || $row['status'] !== 'approved') {
        // Deleted, demoted to pending, or otherwise stale: sign out, and drop
        // the persistent login too so the next request cannot restore it.
        rememberForget($pdo, $driver);
        gasolineDestroySession();
        return $user = null;
    }
    return $user = $row;
}

function countApprovedAdmins(PDO $pdo): int
{
    $stmt = $pdo->query("SELECT COUNT(*) AS n FROM users WHERE is_admin = 1 AND status = 'approved'");
    $row = $stmt->fetch();
    return (int) ($row['n'] ?? 0);
}

function nowUTC(): string
{
    return gmdate('Y-m-d\TH:i:s\Z');
}

function adminEmailFromEnv(): string
{
    return normalizeEmail((string) getenv('GASOLINE_ADMIN_EMAIL'));
}

// ── Auth: persistent login ───────────────────────────────────────────────────
//
// PHP's session storage is not a place to keep a login: the files are garbage-
// collected, wiped by shared hosts, and lost whenever the session path or the
// PHP worker changes — which is what made the viewer demand the password again
// and again. The durable half of the login therefore lives in the database.
//
// The cookie carries "selector:validator". The selector is the lookup key; only
// a SHA-256 of the validator is stored, so read access to `user_sessions`
// cannot be replayed as a cookie, and the row is found by an indexed equality
// match while the secret is still compared in constant time.
//
// The validator is not rotated on use. Rotation would buy a little theft
// detection but races with the parallel requests a page load fires (the
// dashboard fetches JSON alongside the HTML), and losing that race would sign
// the user out — exactly the bug this code exists to fix.

const REMEMBER_COOKIE = 'gasoline_remember';

// Sliding window: an in-use login is extended at most once an hour, so a busy
// browser costs one small UPDATE per hour rather than one per request.
const REMEMBER_TOUCH_INTERVAL = 3600;

function utcAt(int $timestamp): string
{
    return gmdate('Y-m-d\TH:i:s\Z', $timestamp);
}

// Deployments whose database predates this table keep working (with plain
// sessions) until `gasoline migrate` runs, the same way the rest of the viewer
// degrades instead of erroring on an older schema.
function rememberReady(PDO $pdo, string $driver): bool
{
    static $ready = null;
    if ($ready === null) {
        $ready = gasolineTableExists($pdo, $driver, 'user_sessions');
    }
    return $ready;
}

function rememberSetCookie(string $value, int $expires): void
{
    if (headers_sent()) {
        return;
    }
    setcookie(REMEMBER_COOKIE, $value, gasolineCookieOptions($expires));
    $_COOKIE[REMEMBER_COOKIE] = $value;
}

function rememberClearCookie(): void
{
    if (!headers_sent()) {
        setcookie(REMEMBER_COOKIE, '', gasolineCookieOptions(time() - 86400));
    }
    unset($_COOKIE[REMEMBER_COOKIE]);
}

/** Split the cookie into [selector, validator], or null when it is malformed. */
function rememberParseCookie(): ?array
{
    $raw = (string) ($_COOKIE[REMEMBER_COOKIE] ?? '');
    if ($raw === '' || substr_count($raw, ':') !== 1) {
        return null;
    }
    [$selector, $validator] = explode(':', $raw, 2);
    if (!ctype_xdigit($selector) || !ctype_xdigit($validator) || $selector === '' || $validator === '') {
        return null;
    }
    return [$selector, $validator];
}

function rememberDeleteSelector(PDO $pdo, string $selector): void
{
    $stmt = $pdo->prepare('DELETE FROM user_sessions WHERE selector = :selector');
    $stmt->bindValue(':selector', $selector);
    $stmt->execute();
}

// Expired rows are swept on sign-in, which is rare enough to keep the delete
// off the hot path and often enough that the table cannot grow without bound.
function rememberPurgeExpired(PDO $pdo): void
{
    $stmt = $pdo->prepare('DELETE FROM user_sessions WHERE expires_at < :now');
    $stmt->bindValue(':now', nowUTC());
    $stmt->execute();
}

/** Start a persistent login for this browser. */
function rememberIssue(PDO $pdo, string $driver, int $userId): void
{
    if (!rememberReady($pdo, $driver)) {
        return;
    }
    try {
        rememberPurgeExpired($pdo);
        $selector = bin2hex(random_bytes(16));
        $validator = bin2hex(random_bytes(32));
        $now = time();
        $expires = $now + gasolineSessionTTL();
        $stmt = $pdo->prepare(
            'INSERT INTO user_sessions (user_id, selector, validator_hash, created_at, last_used_at, expires_at)
             VALUES (:user_id, :selector, :hash, :created_at, :last_used_at, :expires_at)'
        );
        $stmt->bindValue(':user_id', $userId, PDO::PARAM_INT);
        $stmt->bindValue(':selector', $selector);
        $stmt->bindValue(':hash', hash('sha256', $validator));
        $stmt->bindValue(':created_at', utcAt($now));
        $stmt->bindValue(':last_used_at', utcAt($now));
        $stmt->bindValue(':expires_at', utcAt($expires));
        $stmt->execute();
        rememberSetCookie($selector . ':' . $validator, $expires);
    } catch (Throwable $e) {
        error_log('gasoline persistent login error: ' . $e->getMessage());
        rememberClearCookie();
    }
}

/** Forget this browser only (sign-out). */
function rememberForget(PDO $pdo, string $driver): void
{
    $parsed = rememberParseCookie();
    rememberClearCookie();
    if ($parsed === null || !rememberReady($pdo, $driver)) {
        return;
    }
    try {
        rememberDeleteSelector($pdo, $parsed[0]);
    } catch (Throwable $e) {
        error_log('gasoline persistent login error: ' . $e->getMessage());
    }
}

/** Forget every browser of one user (password change, account deletion). */
function rememberForgetUser(PDO $pdo, string $driver, int $userId): void
{
    if (!rememberReady($pdo, $driver)) {
        return;
    }
    try {
        $stmt = $pdo->prepare('DELETE FROM user_sessions WHERE user_id = :user_id');
        $stmt->bindValue(':user_id', $userId, PDO::PARAM_INT);
        $stmt->execute();
    } catch (Throwable $e) {
        error_log('gasoline persistent login error: ' . $e->getMessage());
    }
}

/**
 * Restore the login from the cookie when the PHP session is gone, and slide the
 * window forward while it is in use. Called once per request, right after the
 * session starts and before anything reads $_SESSION['user_id'].
 */
function rememberSync(PDO $pdo, string $driver): void
{
    $parsed = rememberParseCookie();
    if ($parsed === null) {
        if (isset($_COOKIE[REMEMBER_COOKIE])) {
            rememberClearCookie();
        }
        return;
    }
    if (!rememberReady($pdo, $driver)) {
        return;
    }
    [$selector, $validator] = $parsed;
    try {
        $stmt = $pdo->prepare('SELECT * FROM user_sessions WHERE selector = :selector');
        $stmt->bindValue(':selector', $selector);
        $stmt->execute();
        $row = $stmt->fetch();
        $now = time();
        if ($row === false || (string) $row['expires_at'] < utcAt($now)) {
            if ($row !== false) {
                rememberDeleteSelector($pdo, $selector);
            }
            rememberClearCookie();
            return;
        }
        if (!hash_equals((string) $row['validator_hash'], hash('sha256', $validator))) {
            // Right selector, wrong secret: the cookie is forged or stale, and
            // the token it points at is no longer trustworthy either.
            rememberDeleteSelector($pdo, $selector);
            rememberClearCookie();
            return;
        }
        $user = findUserByID($pdo, (int) $row['user_id']);
        if ($user === null || $user['status'] !== 'approved') {
            rememberDeleteSelector($pdo, $selector);
            rememberClearCookie();
            return;
        }
        $sessionUser = $_SESSION['user_id'] ?? null;
        if ((int) $sessionUser !== (int) $user['id']) {
            // The session was lost (garbage-collected, or the browser dropped
            // its cookie); rebuild it under a fresh id.
            session_regenerate_id(true);
            $_SESSION['user_id'] = (int) $user['id'];
        }
        if ((string) $row['last_used_at'] < utcAt($now - REMEMBER_TOUCH_INTERVAL)) {
            $expires = $now + gasolineSessionTTL();
            $stmt = $pdo->prepare(
                'UPDATE user_sessions SET last_used_at = :last_used_at, expires_at = :expires_at
                 WHERE selector = :selector'
            );
            $stmt->bindValue(':last_used_at', utcAt($now));
            $stmt->bindValue(':expires_at', utcAt($expires));
            $stmt->bindValue(':selector', $selector);
            $stmt->execute();
            rememberSetCookie($selector . ':' . $validator, $expires);
        }
    } catch (Throwable $e) {
        error_log('gasoline persistent login error: ' . $e->getMessage());
    }
}

// ── Email: minimal dependency-free SMTP client ───────────────────────────────

function smtpReadReply($socket): string
{
    $reply = '';
    while (($line = fgets($socket, 2048)) !== false) {
        $reply .= $line;
        if (preg_match('/^\d{3} /', $line)) {
            break;
        }
    }
    return $reply;
}

function smtpCommand($socket, string $command, array $okCodes): void
{
    if ($command !== '') {
        fwrite($socket, $command . "\r\n");
    }
    $reply = smtpReadReply($socket);
    $code = (int) substr($reply, 0, 3);
    if (!in_array($code, $okCodes, true)) {
        throw new RuntimeException(sprintf('SMTP command failed (%s): %s', $command === '' ? 'greeting' : strtok($command, ' '), trim($reply)));
    }
}

/**
 * Sends a plain-text email via the SMTP relay configured in the environment
 * (GASOLINE_SMTP_HOST/PORT/USER/PASSWORD/FROM/TLS). Returns false — after
 * logging — when SMTP is unconfigured or anything fails; callers proceed
 * regardless so registration/approval never block on email delivery.
 */
function smtpSend(string $to, string $subject, string $body): bool
{
    $host = trim((string) getenv('GASOLINE_SMTP_HOST'));
    if ($host === '') {
        error_log('gasoline smtp: GASOLINE_SMTP_HOST not set, skipping email to ' . $to);
        return false;
    }
    $port = (int) (trim((string) getenv('GASOLINE_SMTP_PORT')) ?: '587');
    $user = trim((string) getenv('GASOLINE_SMTP_USER'));
    $password = (string) getenv('GASOLINE_SMTP_PASSWORD');
    $from = trim((string) getenv('GASOLINE_SMTP_FROM')) ?: ('gasoline@' . gethostname());
    $tls = strtolower(trim((string) getenv('GASOLINE_SMTP_TLS')));
    if ($tls === '') {
        $tls = $port === 465 ? 'implicit' : 'starttls';
    }

    $socket = null;
    try {
        $address = ($tls === 'implicit' ? 'ssl://' : 'tcp://') . $host . ':' . $port;
        $socket = stream_socket_client($address, $errno, $errstr, 10);
        if ($socket === false) {
            throw new RuntimeException(sprintf('connect to %s failed: %s', $address, $errstr));
        }
        stream_set_timeout($socket, 10);

        smtpCommand($socket, '', [220]);
        smtpCommand($socket, 'EHLO ' . (gethostname() ?: 'localhost'), [250]);
        if ($tls === 'starttls') {
            smtpCommand($socket, 'STARTTLS', [220]);
            if (!stream_socket_enable_crypto($socket, true, STREAM_CRYPTO_METHOD_TLS_CLIENT)) {
                throw new RuntimeException('STARTTLS negotiation failed');
            }
            smtpCommand($socket, 'EHLO ' . (gethostname() ?: 'localhost'), [250]);
        }
        if ($user !== '') {
            fwrite($socket, "AUTH LOGIN\r\n");
            $reply = smtpReadReply($socket);
            $code = (int) substr($reply, 0, 3);
            if ($code === 334) {
                smtpCommand($socket, base64_encode($user), [334]);
                smtpCommand($socket, base64_encode($password), [235]);
            } else {
                // Fall back to AUTH PLAIN when LOGIN is not offered.
                smtpCommand($socket, 'AUTH PLAIN ' . base64_encode("\0" . $user . "\0" . $password), [235]);
            }
        }
        smtpCommand($socket, 'MAIL FROM:<' . $from . '>', [250]);
        smtpCommand($socket, 'RCPT TO:<' . $to . '>', [250, 251]);
        smtpCommand($socket, 'DATA', [354]);

        $headers = [
            'From: ' . $from,
            'To: ' . $to,
            'Subject: ' . mb_encode_mimeheader($subject, 'UTF-8'),
            'Date: ' . gmdate('r'),
            'Message-ID: <' . bin2hex(random_bytes(16)) . '@gasoline>',
            'MIME-Version: 1.0',
            'Content-Type: text/plain; charset=utf-8',
            'Content-Transfer-Encoding: 8bit',
        ];
        // Dot-stuff lines starting with a period (RFC 5321 §4.5.2).
        $stuffed = preg_replace('/^\./m', '..', str_replace(["\r\n", "\r"], "\n", $body));
        $data = implode("\r\n", $headers) . "\r\n\r\n" . str_replace("\n", "\r\n", (string) $stuffed);
        smtpCommand($socket, $data . "\r\n.", [250]);
        smtpCommand($socket, 'QUIT', [221]);
        return true;
    } catch (Throwable $e) {
        error_log('gasoline smtp: sending to ' . $to . ' failed: ' . $e->getMessage());
        return false;
    } finally {
        if (is_resource($socket)) {
            fclose($socket);
        }
    }
}

function gasolineBaseURL(): string
{
    $base = trim((string) getenv('GASOLINE_BASE_URL'));
    if ($base !== '') {
        return rtrim($base, '/');
    }
    $isHttps = (($_SERVER['HTTPS'] ?? '') !== '' && strtolower((string) $_SERVER['HTTPS']) !== 'off')
        || strtolower((string) ($_SERVER['HTTP_X_FORWARDED_PROTO'] ?? '')) === 'https';
    $host = (string) ($_SERVER['HTTP_HOST'] ?? 'localhost');
    $path = strtok((string) ($_SERVER['REQUEST_URI'] ?? '/'), '?') ?: '/';
    return ($isHttps ? 'https' : 'http') . '://' . $host . rtrim(dirname($path . 'x'), '/');
}

function sendPendingEmail(string $to): bool
{
    $body = "Hello,\n\n"
        . "your gasoline account (" . $to . ") has been created and is waiting for\n"
        . "approval by an administrator. You will receive another email as soon as\n"
        . "your account has been approved.\n\n"
        . "This is an automated message.\n";
    return smtpSend($to, 'gasoline: your account is awaiting approval', $body);
}

function sendApprovedEmail(string $to): bool
{
    $body = "Hello,\n\n"
        . "your gasoline account (" . $to . ") has been approved. You can log in now:\n\n"
        . gasolineBaseURL() . "/?page=login\n\n"
        . "This is an automated message.\n";
    return smtpSend($to, 'gasoline: your account has been approved', $body);
}

// ── Admin settings storage ────────────────────────────────────────────────────

function settingsAll(PDO $pdo): array
{
    $settings = [];
    foreach ($pdo->query('SELECT name, value FROM settings') as $row) {
        $settings[$row['name']] = $row['value'];
    }
    return $settings;
}

function settingsSave(PDO $pdo, string $driver, array $kv): void
{
    if ($driver === 'mysql') {
        $sql = 'INSERT INTO settings (name, value, updated_at) VALUES (:name, :value, :now)
                ON DUPLICATE KEY UPDATE value = VALUES(value), updated_at = VALUES(updated_at)';
    } else {
        $sql = 'INSERT INTO settings (name, value, updated_at) VALUES (:name, :value, :now)
                ON CONFLICT(name) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at';
    }
    $stmt = $pdo->prepare($sql);
    foreach ($kv as $name => $value) {
        $stmt->bindValue(':name', (string) $name);
        $stmt->bindValue(':value', (string) $value);
        $stmt->bindValue(':now', nowUTC());
        $stmt->execute();
    }
}

// ── Validation helpers for notification schedules ────────────────────────────

function validHHMM(string $value): bool
{
    return preg_match('/^([01][0-9]|2[0-3]):[0-5][0-9]$/', $value) === 1;
}

const GASOLINE_WEEKDAYS = ['mon', 'tue', 'wed', 'thu', 'fri', 'sat', 'sun'];

/**
 * Renders a notification radius for the form: the stored value unchanged, minus a
 * trailing ".0" so a whole number does not read as a decimal. Rounding here would
 * silently resize a migrated fractional area the next time the form is saved.
 * Always a period decimal separator, which is what a number input requires.
 */
function formatRadiusKm(float $radiusKm): string
{
    $text = rtrim(rtrim(number_format($radiusKm, 3, '.', ''), '0'), '.');
    return $text === '' ? '0' : $text;
}

/**
 * Largest notification radius a user may pick, in km. Generous compared with an
 * update target's collection radius, because a user can sit between two targets
 * and legitimately care about stations from both — and it matches the ceiling the
 * old admin-wide range_km allowed, so any radius the migration carried over from
 * it can still be saved from this form.
 */
const GASOLINE_MAX_NOTIFY_RADIUS_KM = 100;

/** The suggest/check fuel types, in canonical display order. */
const GASOLINE_FUELS = ['diesel', 'e5', 'e10'];

/**
 * How long a station stays in scope after its last price update, mirroring Go's
 * stationFreshness (settings.go). A station leaves scope when it stops being
 * fed, which is what happens when an update target is removed or its radius
 * shrinks, and the dashboard shows only stations still in it: otherwise a
 * station nobody collects any more renders its last known price as though that
 * were today's, and its card sits next to stations that are actually current.
 */
const GASOLINE_STATION_FRESHNESS_HOURS = 48;

/**
 * How many of the nearest stations the surroundings card reads a current price
 * for. A radius that admits more than this many stations has long stopped being
 * a neighbourhood, and the cap is what bounds that query's cost.
 */
const NEARBY_STATION_LIMIT = 40;

/** The RFC3339 instant a station's newest snapshot must reach to be in scope. */
function stationFreshnessCutoff(): string
{
    // Resolved once per request. Several queries in one page load apply this
    // bound — the station scope and the surroundings prices among them — and a
    // station whose newest snapshot sits within a second of the boundary must
    // not be in scope for one of them and out of scope for the next.
    static $cutoff = null;
    if ($cutoff === null) {
        $cutoff = gmdate('Y-m-d\TH:i:s\Z', time() - GASOLINE_STATION_FRESHNESS_HOURS * 3600);
    }

    return $cutoff;
}



/** Normalizes a submitted weekday list to canonical order; null when invalid/empty. */
function normalizeDayList(array $days): ?string
{
    $selected = [];
    foreach ($days as $day) {
        $day = strtolower(trim((string) $day));
        if (!in_array($day, GASOLINE_WEEKDAYS, true)) {
            return null;
        }
        $selected[$day] = true;
    }
    if ($selected === []) {
        return null;
    }
    $ordered = array_values(array_filter(GASOLINE_WEEKDAYS, static fn (string $d): bool => isset($selected[$d])));
    return implode(',', $ordered);
}

/** Pairs from[]/to[] time inputs into a "HH:MM-HH:MM,..." list; null when invalid/empty. */
function normalizeWindowList(array $from, array $to): ?string
{
    $windows = [];
    $count = count($from);
    if ($count !== count($to)) {
        return null;
    }
    for ($i = 0; $i < $count; $i++) {
        $f = trim((string) $from[$i]);
        $t = trim((string) $to[$i]);
        if ($f === '' && $t === '') {
            continue;
        }
        if (!validHHMM($f) || !validHHMM($t)) {
            return null;
        }
        $windows[] = $f . '-' . $t;
    }
    if ($windows === []) {
        return null;
    }
    $windows = array_values(array_unique($windows));
    sort($windows);
    return implode(',', $windows);
}

/** Normalizes a list of HH:MM inputs into a sorted, deduplicated CSV; null when invalid/empty. */
function normalizeTimeList(array $times): ?string
{
    $normalized = [];
    foreach ($times as $time) {
        $time = trim((string) $time);
        if ($time === '') {
            continue;
        }
        if (!validHHMM($time)) {
            return null;
        }
        $normalized[$time] = true;
    }
    if ($normalized === []) {
        return null;
    }
    $list = array_keys($normalized);
    sort($list);
    return implode(',', $list);
}







/* ── Dashboard filters ────────────────────────────────────────────────────────
 *
 * The sidebar's filters belong to the account, not to the URL: one row per user
 * in `user_filters`, written by the save_filters post and read back on every
 * load — including by ?action=data, which no longer copies the page's query
 * string because there is nothing in it to copy.
 *
 * They used to be query parameters kept alive by a cookie. That made a filtered
 * dashboard a link, but it also meant one account saw different filters in
 * every browser it signed in from, and the reader's own location — the thing
 * the surroundings card is about — lived in a cookie that any cache clear threw
 * away.
 *
 * The location is stored as the label the reader sees plus the coordinates it
 * resolved to. Deliberately not as a key into `cities`: that table is the CLI's
 * cache of the places it collects, one row per city, and a reader's street
 * address is neither a city nor something the CLI should have to carry.
 */

/** Radii the sidebar offers. doctor accepts exactly these (doctor_dashboard.go). */
const DASHBOARD_RADIUS_OPTIONS = [5, 10, 20];

/** Quick ranges, and how many days back each one reaches. */
const DASHBOARD_RANGE_DAYS = ['7d' => 7, '14d' => 14, '30d' => 30];

/** Fuel filter values, "all" included. */
const DASHBOARD_FUELS = ['all', 'diesel', 'e5', 'e10'];

/**
 * How many hand-picked stations one filter row keeps. The picker is for
 * comparing a handful, and the cap is what stops a row from growing without
 * bound if something ever posts the whole list.
 */
const DASHBOARD_STATION_LIMIT = 200;

/** What a user who has never touched the sidebar sees. */
function defaultDashboardFilters(): array
{
    return [
        'location_label' => '',
        'location_lat' => 0.0,
        'location_lng' => 0.0,
        'radius_km' => DASHBOARD_RADIUS_OPTIONS[0],
        'range' => '7d',
        'from' => '',
        'to' => '',
        'fuel' => 'all',
        'station_ids' => [],
    ];
}

/** True for a "YYYY-MM-DD" that is also a real calendar date. */
function validISODate(string $value): bool
{
    $parsed = DateTimeImmutable::createFromFormat('!Y-m-d', $value, new DateTimeZone('UTC'));

    return $parsed !== false && $parsed->format('Y-m-d') === $value;
}

/**
 * Clamp a raw filter set to what the page can render.
 *
 * The same function guards both directions: what the sidebar posts (which is
 * whatever a form can be made to send) and what comes back out of the database
 * (which an older release, a hand-edited row or a dropped station could have
 * left inconsistent). Anything it cannot make sense of falls back to the
 * default rather than propagating — a bad radius must not empty the dashboard.
 *
 * @param array<string, mixed> $raw
 * @return array<string, mixed>
 */
function normalizeDashboardFilters(array $raw): array
{
    $filters = defaultDashboardFilters();

    // The location is all-or-nothing: a label without usable coordinates
    // cannot centre a radius, and coordinates without a label would leave the
    // sidebar unable to say where the reader is.
    $label = trim((string) ($raw['location_label'] ?? ''));
    $lat = $raw['location_lat'] ?? '';
    $lng = $raw['location_lng'] ?? '';
    if ($label !== '' && is_numeric($lat) && is_numeric($lng)
        && (float) $lat >= -90.0 && (float) $lat <= 90.0
        && (float) $lng >= -180.0 && (float) $lng <= 180.0
    ) {
        // user_filters.location_label is VARCHAR(255).
        $filters['location_label'] = mb_substr($label, 0, 200, 'UTF-8');
        $filters['location_lat'] = (float) $lat;
        $filters['location_lng'] = (float) $lng;
    }

    $radius = (int) ($raw['radius_km'] ?? 0);
    if (in_array($radius, DASHBOARD_RADIUS_OPTIONS, true)) {
        $filters['radius_km'] = $radius;
    }

    $fuel = trim((string) ($raw['fuel'] ?? ''));
    if (in_array($fuel, DASHBOARD_FUELS, true)) {
        $filters['fuel'] = $fuel;
    }

    // A quick range and explicit dates are alternatives: picking a range is
    // what clears the date boxes, so a row carrying both would render two
    // different answers in one sidebar.
    $range = trim((string) ($raw['range'] ?? ''));
    $from = trim((string) ($raw['from'] ?? ''));
    $to = trim((string) ($raw['to'] ?? ''));
    if (isset(DASHBOARD_RANGE_DAYS[$range])) {
        $filters['range'] = $range;
        return $filters;
    }
    $filters['from'] = validISODate($from) ? $from : '';
    $filters['to'] = validISODate($to) ? $to : '';
    // Neither a range nor a usable date: back to the default window rather
    // than to the whole history, which is what the page did before too.
    $filters['range'] = ($filters['from'] === '' && $filters['to'] === '') ? '7d' : '';

    return $filters;
}

/**
 * The hand-picked station ids, cleaned up.
 *
 * Split out of normalizeDashboardFilters because it is the one field that
 * arrives as a list, and the ordering matters: the picker renders in scope
 * order, so what is stored is a set, deduplicated, capped and otherwise left
 * as submitted.
 *
 * @param mixed $raw
 * @return array<int, string>
 */
function normalizeDashboardStationIds($raw): array
{
    if (!is_array($raw)) {
        return [];
    }
    $ids = [];
    foreach ($raw as $value) {
        if (!is_scalar($value)) {
            continue;
        }
        $id = trim((string) $value);
        if ($id === '' || isset($ids[$id])) {
            continue;
        }
        $ids[$id] = true;
        if (count($ids) >= DASHBOARD_STATION_LIMIT) {
            break;
        }
    }

    return array_keys($ids);
}

/**
 * Read one user's stored filters, falling back to the defaults for an account
 * that has never saved any — or to a row a later release cannot read.
 */
function loadDashboardFilters(PDO $pdo, int $userId): array
{
    $stmt = $pdo->prepare(
        <<<'SQL'
        SELECT location_label, location_lat, location_lng, radius_km,
               range_key, from_date, to_date, fuel, station_ids
        FROM user_filters
        WHERE user_id = :user_id
        SQL
    );
    $stmt->bindValue(':user_id', $userId, PDO::PARAM_INT);
    $stmt->execute();
    $row = $stmt->fetch();
    if ($row === false) {
        return defaultDashboardFilters();
    }

    $stationIds = json_decode((string) $row['station_ids'], true);
    $filters = normalizeDashboardFilters([
        'location_label' => $row['location_label'],
        'location_lat' => $row['location_lat'],
        'location_lng' => $row['location_lng'],
        'radius_km' => $row['radius_km'],
        'range' => $row['range_key'],
        'from' => $row['from_date'],
        'to' => $row['to_date'],
        'fuel' => $row['fuel'],
    ]);
    $filters['station_ids'] = normalizeDashboardStationIds($stationIds);

    return $filters;
}

/**
 * Write one user's filters, normalizing on the way in. Upsert rather than
 * insert-or-update, mirroring how the CLI writes its own single-row tables
 * (citiesUpsertSQL in db.go).
 *
 * @param array<string, mixed> $raw as submitted by the sidebar
 */
function saveDashboardFilters(PDO $pdo, string $driver, int $userId, array $raw): array
{
    $filters = normalizeDashboardFilters($raw);
    $filters['station_ids'] = normalizeDashboardStationIds($raw['station_ids'] ?? []);

    $sql = $driver === 'mysql'
        ? <<<'SQL'
            INSERT INTO user_filters (user_id, location_label, location_lat, location_lng,
                radius_km, range_key, from_date, to_date, fuel, station_ids, updated_at)
            VALUES (:user_id, :label, :lat, :lng, :radius, :range_key, :from_date, :to_date,
                :fuel, :station_ids, :updated_at)
            ON DUPLICATE KEY UPDATE
                location_label = VALUES(location_label),
                location_lat = VALUES(location_lat),
                location_lng = VALUES(location_lng),
                radius_km = VALUES(radius_km),
                range_key = VALUES(range_key),
                from_date = VALUES(from_date),
                to_date = VALUES(to_date),
                fuel = VALUES(fuel),
                station_ids = VALUES(station_ids),
                updated_at = VALUES(updated_at)
            SQL
        : <<<'SQL'
            INSERT INTO user_filters (user_id, location_label, location_lat, location_lng,
                radius_km, range_key, from_date, to_date, fuel, station_ids, updated_at)
            VALUES (:user_id, :label, :lat, :lng, :radius, :range_key, :from_date, :to_date,
                :fuel, :station_ids, :updated_at)
            ON CONFLICT(user_id) DO UPDATE SET
                location_label = excluded.location_label,
                location_lat = excluded.location_lat,
                location_lng = excluded.location_lng,
                radius_km = excluded.radius_km,
                range_key = excluded.range_key,
                from_date = excluded.from_date,
                to_date = excluded.to_date,
                fuel = excluded.fuel,
                station_ids = excluded.station_ids,
                updated_at = excluded.updated_at
            SQL;

    $stmt = $pdo->prepare($sql);
    $stmt->bindValue(':user_id', $userId, PDO::PARAM_INT);
    $stmt->bindValue(':label', $filters['location_label']);
    $stmt->bindValue(':lat', $filters['location_lat']);
    $stmt->bindValue(':lng', $filters['location_lng']);
    $stmt->bindValue(':radius', $filters['radius_km'], PDO::PARAM_INT);
    $stmt->bindValue(':range_key', $filters['range']);
    $stmt->bindValue(':from_date', $filters['from']);
    $stmt->bindValue(':to_date', $filters['to']);
    $stmt->bindValue(':fuel', $filters['fuel']);
    $stmt->bindValue(':station_ids', json_encode($filters['station_ids'], JSON_UNESCAPED_UNICODE));
    $stmt->bindValue(':updated_at', nowUTC());
    $stmt->execute();

    return $filters;
}

/** Drop one user's filters, which is what the sidebar's Reset does. */
function clearDashboardFilters(PDO $pdo, int $userId): void
{
    $stmt = $pdo->prepare('DELETE FROM user_filters WHERE user_id = :user_id');
    $stmt->bindValue(':user_id', $userId, PDO::PARAM_INT);
    $stmt->execute();
}

// ── POST router ───────────────────────────────────────────────────────────────

function handlePost(PDO $pdo, string $driver): void
{
    if (($_SERVER['REQUEST_METHOD'] ?? 'GET') !== 'POST') {
        return;
    }
    $action = (string) ($_POST['action'] ?? '');
    if (!csrfValid()) {
        setFlash('error', 'csrfError');
        redirectTo(in_array($action, ['login', 'register'], true) ? '?page=' . $action : '');
    }
    $user = currentUser($pdo, $driver);

    switch ($action) {
        case 'login':
            if ($user !== null) {
                redirectTo('');
            }
            $email = normalizeEmail((string) ($_POST['email'] ?? ''));
            $password = (string) ($_POST['password'] ?? '');
            $row = $email !== '' ? findUserByEmail($pdo, $email) : null;
            // Constant-shape verification: always run password_verify so
            // unknown emails take the same time as wrong passwords.
            $hash = $row['password_hash'] ?? '$2y$10$mUvx7uH2ZDLLLSAybMwuVOxuDgKzc4Cul5xEQmk9RIYBYEnp3eLJa';
            $ok = password_verify($password, $hash);
            if ($row === null || !$ok) {
                usleep(300000);
                setFlash('error', 'invalidCredentials');
                redirectTo('?page=login');
            }
            if ($row['status'] !== 'approved') {
                setFlash('info', 'awaitingApproval');
                redirectTo('?page=login');
            }
            session_regenerate_id(true);
            $_SESSION['user_id'] = (int) $row['id'];
            rememberIssue($pdo, $driver, (int) $row['id']);
            if (password_needs_rehash($hash, PASSWORD_DEFAULT)) {
                $stmt = $pdo->prepare('UPDATE users SET password_hash = :hash WHERE id = :id');
                $stmt->bindValue(':hash', password_hash($password, PASSWORD_DEFAULT));
                $stmt->bindValue(':id', (int) $row['id'], PDO::PARAM_INT);
                $stmt->execute();
            }
            redirectTo('');
            // no break (redirectTo exits)

        case 'register':
            if ($user !== null) {
                redirectTo('');
            }
            $email = normalizeEmail((string) ($_POST['email'] ?? ''));
            $password = (string) ($_POST['password'] ?? '');
            $repeat = (string) ($_POST['password_repeat'] ?? '');
            if (filter_var($email, FILTER_VALIDATE_EMAIL) === false) {
                setFlash('error', 'invalidEmail');
                redirectTo('?page=register&email=' . urlencode($email));
            }
            if (strlen($password) < 10) {
                setFlash('error', 'passwordTooShort');
                redirectTo('?page=register&email=' . urlencode($email));
            }
            if ($password !== $repeat) {
                setFlash('error', 'passwordMismatch');
                redirectTo('?page=register&email=' . urlencode($email));
            }
            if (findUserByEmail($pdo, $email) !== null) {
                setFlash('error', 'emailTaken');
                redirectTo('?page=register');
            }
            $isInitialAdmin = $email !== '' && $email === adminEmailFromEnv();
            try {
                $stmt = $pdo->prepare(
                    'INSERT INTO users (email, password_hash, is_admin, status, created_at, approved_at)
                     VALUES (:email, :hash, :is_admin, :status, :created_at, :approved_at)'
                );
                $stmt->bindValue(':email', $email);
                $stmt->bindValue(':hash', password_hash($password, PASSWORD_DEFAULT));
                $stmt->bindValue(':is_admin', $isInitialAdmin ? 1 : 0, PDO::PARAM_INT);
                $stmt->bindValue(':status', $isInitialAdmin ? 'approved' : 'pending');
                $stmt->bindValue(':created_at', nowUTC());
                $stmt->bindValue(':approved_at', $isInitialAdmin ? nowUTC() : null);
                $stmt->execute();
            } catch (PDOException $e) {
                // Unique-constraint race: someone registered the email in between.
                error_log('gasoline register error: ' . $e->getMessage());
                setFlash('error', 'emailTaken');
                redirectTo('?page=register');
            }
            if ($isInitialAdmin) {
                setFlash('success', 'accountCreated');
            } else {
                sendPendingEmail($email);
                setFlash('success', 'registerPendingSent');
            }
            redirectTo('?page=login');
            // no break

        case 'logout':
            if ($user !== null) {
                rememberForget($pdo, $driver);
                gasolineDestroySession();
            }
            redirectTo('?page=login');
            // no break
    }

    // Everything below requires a signed-in, approved user.
    if ($user === null) {
        redirectTo('?page=login');
    }

    switch ($action) {
        case 'change_password':
            $current = (string) ($_POST['current_password'] ?? '');
            $new = (string) ($_POST['new_password'] ?? '');
            $repeat = (string) ($_POST['new_password_repeat'] ?? '');
            if (!password_verify($current, $user['password_hash'])) {
                setFlash('error', 'wrongPassword');
                redirectTo('?page=account');
            }
            if (strlen($new) < 10) {
                setFlash('error', 'passwordTooShort');
                redirectTo('?page=account');
            }
            if ($new !== $repeat) {
                setFlash('error', 'passwordMismatch');
                redirectTo('?page=account');
            }
            $stmt = $pdo->prepare('UPDATE users SET password_hash = :hash WHERE id = :id');
            $stmt->bindValue(':hash', password_hash($new, PASSWORD_DEFAULT));
            $stmt->bindValue(':id', (int) $user['id'], PDO::PARAM_INT);
            $stmt->execute();
            session_regenerate_id(true);
            // A new password invalidates every persistent login, including this
            // browser's — which then gets a fresh one, so changing the password
            // does not sign you out of the tab you changed it in.
            rememberForgetUser($pdo, $driver, (int) $user['id']);
            rememberIssue($pdo, $driver, (int) $user['id']);
            setFlash('success', 'passwordChanged');
            redirectTo('?page=account');
            // no break

        case 'save_notify':
            $method = (string) ($_POST['notify_method'] ?? 'pushover');
            $days = normalizeDayList((array) ($_POST['notify_days'] ?? []));
            $windows = normalizeWindowList(
                (array) ($_POST['notify_windows_from'] ?? []),
                (array) ($_POST['notify_windows_to'] ?? [])
            );
            $times = normalizeTimeList((array) ($_POST['notify_suggest_times'] ?? []));
            $fuel = strtolower(trim((string) ($_POST['notify_fuel'] ?? '')));
            if ($method !== 'pushover' || $days === null || $windows === null || $times === null
                || !in_array($fuel, GASOLINE_FUELS, true)) {
                setFlash('error', 'invalidNotifySettings');
                redirectTo('?page=account');
            }
            // The notification area: a city and a radius around it, resolved to
            // coordinates here so nothing downstream has to consult the geocode
            // cache. A location is only required once a notification kind is
            // switched on; leaving both off pauses everything.
            $wantsNotifications = isset($_POST['notify_suggest_enabled']) || isset($_POST['notify_check_enabled']);
            $cityKey = trim((string) ($_POST['notify_city'] ?? ''));
            $radiusRaw = trim((string) ($_POST['notify_radius_km'] ?? ''));
            $cityRow = resolveCity($pdo, $cityKey);
            $radius = 0.0;
            if ($cityKey !== '' || $radiusRaw !== '' || $wantsNotifications) {
                // Numeric rather than integer: the legacy range_km this migrates
                // from stepped in halves, so 7.5 km subscriptions exist and must
                // survive an unrelated save.
                if ($cityRow === null || !is_numeric($radiusRaw)
                    || (float) $radiusRaw < 1 || (float) $radiusRaw > GASOLINE_MAX_NOTIFY_RADIUS_KM) {
                    setFlash('error', 'invalidNotifyLocation');
                    redirectTo('?page=account');
                }
                $radius = (float) $radiusRaw;
            }
            $pdo->beginTransaction();
            $stmt = $pdo->prepare(
                'UPDATE users SET notify_method = :method, pushover_app_name = :app,
                    pushover_user_key = :user_key, pushover_token = :token,
                    notify_days = :days, notify_windows = :windows,
                    notify_suggest_times = :times, notify_check_enabled = :check_enabled,
                    notify_suggest_enabled = :suggest_enabled,
                    notify_fuel = :fuel,
                    notify_city = :city, notify_lat = :lat, notify_lng = :lng,
                    notify_radius_km = :radius
                 WHERE id = :id'
            );
            $stmt->bindValue(':method', 'pushover');
            $stmt->bindValue(':app', trim((string) ($_POST['pushover_app_name'] ?? '')) ?: 'gasoline');
            $stmt->bindValue(':user_key', trim((string) ($_POST['pushover_user_key'] ?? '')));
            $stmt->bindValue(':token', trim((string) ($_POST['pushover_token'] ?? '')));
            $stmt->bindValue(':days', $days);
            $stmt->bindValue(':windows', $windows);
            $stmt->bindValue(':times', $times);
            $stmt->bindValue(':check_enabled', isset($_POST['notify_check_enabled']) ? 1 : 0, PDO::PARAM_INT);
            $stmt->bindValue(':suggest_enabled', isset($_POST['notify_suggest_enabled']) ? 1 : 0, PDO::PARAM_INT);
            $stmt->bindValue(':fuel', $fuel);
            $stmt->bindValue(':city', $cityRow === null ? '' : (string) $cityRow['city_key']);
            $stmt->bindValue(':lat', $cityRow === null ? 0.0 : (float) $cityRow['lat']);
            $stmt->bindValue(':lng', $cityRow === null ? 0.0 : (float) $cityRow['lng']);
            $stmt->bindValue(':radius', $radius);
            $stmt->bindValue(':id', (int) $user['id'], PDO::PARAM_INT);
            $stmt->execute();
            $pdo->commit();
            setFlash('success', 'notifySaved');
            redirectTo('?page=account');
            // no break

        case 'delete_account':
            $password = (string) ($_POST['current_password'] ?? '');
            if (!isset($_POST['confirm'])) {
                setFlash('error', 'confirmRequired');
                redirectTo('?page=account');
            }
            if (!password_verify($password, $user['password_hash'])) {
                setFlash('error', 'wrongPassword');
                redirectTo('?page=account');
            }
            if ((int) $user['is_admin'] === 1 && countApprovedAdmins($pdo) <= 1) {
                setFlash('error', 'lastAdminGuard');
                redirectTo('?page=account');
            }
            rememberForgetUser($pdo, $driver, (int) $user['id']);
            $stmt = $pdo->prepare('DELETE FROM users WHERE id = :id');
            $stmt->bindValue(':id', (int) $user['id'], PDO::PARAM_INT);
            $stmt->execute();
            rememberClearCookie();
            gasolineDestroySession();
            gasolineStartSession();
            setFlash('success', 'accountDeleted');
            redirectTo('?page=login');
            // no break
    }

    // Everything below requires an administrator.
    if ((int) $user['is_admin'] !== 1) {
        redirectTo('');
    }

    switch ($action) {
        case 'approve_user':
            $target = findUserByID($pdo, (int) ($_POST['user_id'] ?? 0));
            if ($target === null || $target['status'] !== 'pending') {
                setFlash('error', 'notFound');
                redirectTo('?page=admin_users');
            }
            $stmt = $pdo->prepare("UPDATE users SET status = 'approved', approved_at = :now WHERE id = :id");
            $stmt->bindValue(':now', nowUTC());
            $stmt->bindValue(':id', (int) $target['id'], PDO::PARAM_INT);
            $stmt->execute();
            setFlash('success', sendApprovedEmail($target['email']) ? 'userApproved' : 'userApprovedEmailFailed');
            redirectTo('?page=admin_users');
            // no break

        case 'delete_user':
            $targetId = (int) ($_POST['user_id'] ?? 0);
            if ($targetId === (int) $user['id']) {
                setFlash('error', 'cannotActOnSelf');
                redirectTo('?page=admin_users');
            }
            rememberForgetUser($pdo, $driver, $targetId);
            $stmt = $pdo->prepare('DELETE FROM users WHERE id = :id');
            $stmt->bindValue(':id', $targetId, PDO::PARAM_INT);
            $stmt->execute();
            setFlash($stmt->rowCount() > 0 ? 'success' : 'error', $stmt->rowCount() > 0 ? 'userDeleted' : 'notFound');
            redirectTo('?page=admin_users');
            // no break

        case 'set_admin':
            $targetId = (int) ($_POST['user_id'] ?? 0);
            $makeAdmin = (string) ($_POST['admin'] ?? '') === '1';
            if ($targetId === (int) $user['id']) {
                // An admin can never demote themselves, so at least one
                // admin always remains.
                setFlash('error', 'cannotActOnSelf');
                redirectTo('?page=admin_users');
            }
            $target = findUserByID($pdo, $targetId);
            if ($target === null) {
                setFlash('error', 'notFound');
                redirectTo('?page=admin_users');
            }
            $stmt = $pdo->prepare('UPDATE users SET is_admin = :admin WHERE id = :id');
            $stmt->bindValue(':admin', $makeAdmin ? 1 : 0, PDO::PARAM_INT);
            $stmt->bindValue(':id', $targetId, PDO::PARAM_INT);
            $stmt->execute();
            setFlash('success', $makeAdmin ? 'userPromoted' : 'userDemoted');
            redirectTo('?page=admin_users');
            // no break

        case 'save_settings':
            // Templates are the whole of the stored configuration. Title
            // templates may be empty: notifications then fall back to each
            // user's configured notification title.
            $fields = [
                'check_template' => static fn (string $v): bool => $v !== '',
                'suggest_template' => static fn (string $v): bool => $v !== '',
                'check_title_template' => static fn (string $v): bool => true,
                'suggest_title_template' => static fn (string $v): bool => true,
            ];
            $kv = [];
            foreach ($fields as $name => $validate) {
                if (!isset($_POST[$name])) {
                    continue;
                }
                $value = trim((string) $_POST[$name]);
                if (!$validate($value)) {
                    setFlash('error', 'invalidSettings');
                    redirectTo('?page=admin_settings');
                }
                $kv[$name] = $value;
            }
            settingsSave($pdo, $driver, $kv);
            setFlash('success', 'settingsSaved');
            redirectTo('?page=admin_settings');
            // no break

        case 'add_target':
            $city = trim((string) ($_POST['city'] ?? ''));
            $radius = (string) ($_POST['radius_km'] ?? '');
            if ($city === '' || !ctype_digit($radius) || (int) $radius < 1 || (int) $radius > 25) {
                setFlash('error', 'invalidTarget');
                redirectTo('?page=admin_settings');
            }
            try {
                $stmt = $pdo->prepare('INSERT INTO update_targets (city, radius_km, created_at) VALUES (:city, :radius, :now)');
                $stmt->bindValue(':city', $city);
                $stmt->bindValue(':radius', (int) $radius, PDO::PARAM_INT);
                $stmt->bindValue(':now', nowUTC());
                $stmt->execute();
            } catch (PDOException $e) {
                setFlash('error', 'targetExists');
                redirectTo('?page=admin_settings');
            }
            setFlash('success', 'targetAdded');
            redirectTo('?page=admin_settings');
            // no break

        case 'delete_target':
            $stmt = $pdo->prepare('DELETE FROM update_targets WHERE id = :id');
            $stmt->bindValue(':id', (int) ($_POST['target_id'] ?? 0), PDO::PARAM_INT);
            $stmt->execute();
            setFlash($stmt->rowCount() > 0 ? 'success' : 'error', $stmt->rowCount() > 0 ? 'targetRemoved' : 'notFound');
            redirectTo('?page=admin_settings');
            // no break

        case 'save_filters':
            // The sidebar posts its whole state on every change, so this is
            // both "apply" and "remember": there is nowhere else the filters
            // live now.
            if ($user === null) {
                redirectTo('?page=login');
            }
            saveDashboardFilters($pdo, $driver, (int) $user['id'], [
                'location_label' => $_POST['location_label'] ?? '',
                'location_lat' => $_POST['location_lat'] ?? '',
                'location_lng' => $_POST['location_lng'] ?? '',
                'radius_km' => $_POST['radius_km'] ?? '',
                'range' => $_POST['range'] ?? '',
                'from' => $_POST['from'] ?? '',
                'to' => $_POST['to'] ?? '',
                'fuel' => $_POST['fuel'] ?? '',
                'station_ids' => (array) ($_POST['station_ids'] ?? []),
            ]);
            redirectTo('');
            // no break

        case 'reset_filters':
            if ($user === null) {
                redirectTo('?page=login');
            }
            clearDashboardFilters($pdo, (int) $user['id']);
            redirectTo('');
            // no break

        case 'rename_station':
            $stationId = trim((string) ($_POST['station_id'] ?? ''));
            $newName = trim((string) ($_POST['new_name'] ?? ''));
            if ($stationId === '' || $newName === '' || mb_strlen($newName, 'UTF-8') > 200) {
                setFlash('error', 'invalidRename');
                redirectTo('?page=admin_stations');
            }
            if (!stationExists($pdo, $stationId)) {
                setFlash('error', 'notFound');
                redirectTo('?page=admin_stations');
            }
            // Same override the CLI's `gasoline rename` sets; `update` keeps
            // the canonical name in sync but never touches the override.
            $stmt = $pdo->prepare('UPDATE stations SET name_override = :name WHERE id = :id');
            $stmt->bindValue(':name', $newName);
            $stmt->bindValue(':id', $stationId);
            $stmt->execute();
            setFlash('success', 'stationRenamed');
            redirectTo('?page=admin_stations');
            // no break

        case 'clear_station_rename':
            $stationId = trim((string) ($_POST['station_id'] ?? ''));
            if ($stationId === '' || !stationExists($pdo, $stationId)) {
                setFlash('error', 'notFound');
                redirectTo('?page=admin_stations');
            }
            $stmt = $pdo->prepare('UPDATE stations SET name_override = NULL WHERE id = :id');
            $stmt->bindValue(':id', $stationId);
            $stmt->execute();
            setFlash('success', 'renameCleared');
            redirectTo('?page=admin_stations');
            // no break
    }

    // Unknown action: back to the dashboard.
    redirectTo('');
}

// ── Page renderers (auth, account, admin) ────────────────────────────────────
// renderDocumentHead / renderHeader / renderCommonScript are defined further
// down (top-level functions are hoisted), so these can call them freely.

function renderFlash(): void
{
    $flash = takeFlash();
    if ($flash === null) {
        return;
    }
    $class = $flash['type'] === 'error' ? 'error-box' : 'success-box';
    echo '<div class="' . h($class) . '" data-i18n="' . h($flash['key']) . '">' . h(flashText($flash['key'])) . "</div>\n";
}

function renderPageStart(string $titleSuffix, ?array $user, string $activePage): void
{
    renderDocumentHead($titleSuffix);
    echo "<body>\n<main class=\"page\">\n";
    renderHeader($user, $activePage);
}

function renderPageEnd(): never
{
    echo "</main>\n";
    renderCommonScript();
    echo "</body>\n</html>\n";
    exit;
}

function renderSchemaGuardPage(string $reasonKey): never
{
    renderDocumentHead('Setup');
    ?>
<body>
<main class="page">
    <div class="auth-wrap">
        <div class="auth-card">
            <h2 data-i18n="schemaOutdatedTitle">Database not ready</h2>
            <?php if ($reasonKey === 'dbNotFound') { ?>
            <p class="auth-note" data-i18n="schemaDbNotFound">The database was not found.</p>
            <?php } ?>
            <p class="auth-note" data-i18n="schemaOutdatedBody">The database schema is missing the required tables. Run the following command on the server, then reload this page:</p>
            <pre class="auth-code">gasoline migrate</pre>
        </div>
    </div>
</main>
<?php
    renderCommonScript();
    echo "</body>\n</html>\n";
    exit;
}

function renderLoginPage(): never
{
    renderPageStart('Sign in', null, 'login');
    $email = trim((string) ($_GET['email'] ?? ''));
    ?>
    <div class="auth-wrap">
        <div class="auth-card">
            <h2 data-i18n="loginTitle">Sign in</h2>
            <?php renderFlash(); ?>
            <form method="post" action="">
                <?= csrfField() ?>
                <input type="hidden" name="action" value="login">
                <div class="field">
                    <label for="login-email" data-i18n="email">Email address</label>
                    <input type="email" id="login-email" name="email" required autofocus autocomplete="username" value="<?= h($email) ?>">
                </div>
                <div class="field">
                    <label for="login-password" data-i18n="password">Password</label>
                    <input type="password" id="login-password" name="password" required autocomplete="current-password">
                </div>
                <button type="submit" class="btn-primary" data-i18n="signIn">Sign in</button>
            </form>
            <p class="auth-note"><span data-i18n="noAccountYet">No account yet?</span> <a href="?page=register" data-i18n="createAccount">Create an account</a></p>
        </div>
    </div>
    <?php
    renderPageEnd();
}

function renderRegisterPage(): never
{
    renderPageStart('Register', null, 'register');
    $email = trim((string) ($_GET['email'] ?? ''));
    ?>
    <div class="auth-wrap">
        <div class="auth-card">
            <h2 data-i18n="registerTitle">Create an account</h2>
            <?php renderFlash(); ?>
            <p class="auth-note" data-i18n="registerHint">Your email address is your username. After registration an administrator has to approve your account before you can sign in.</p>
            <form method="post" action="">
                <?= csrfField() ?>
                <input type="hidden" name="action" value="register">
                <div class="field">
                    <label for="reg-email" data-i18n="email">Email address</label>
                    <input type="email" id="reg-email" name="email" required autofocus autocomplete="username" value="<?= h($email) ?>">
                </div>
                <div class="field">
                    <label for="reg-password" data-i18n="password">Password</label>
                    <input type="password" id="reg-password" name="password" required minlength="10" autocomplete="new-password">
                </div>
                <div class="field">
                    <label for="reg-password2" data-i18n="passwordRepeat">Repeat password</label>
                    <input type="password" id="reg-password2" name="password_repeat" required minlength="10" autocomplete="new-password">
                </div>
                <button type="submit" class="btn-primary" data-i18n="createAccount">Create an account</button>
            </form>
            <p class="auth-note"><span data-i18n="haveAccount">Already have an account?</span> <a href="?page=login" data-i18n="signIn">Sign in</a></p>
        </div>
    </div>
    <?php
    renderPageEnd();
}

function renderScheduleEditor(string $days, string $windows, string $times): void
{
    $selectedDays = array_flip(array_filter(array_map('trim', explode(',', $days))));
    $windowRows = array_values(array_filter(array_map('trim', explode(',', $windows))));
    $timeRows = array_values(array_filter(array_map('trim', explode(',', $times))));
    ?>
                <div class="field">
                    <label data-i18n="notifyDays">Days of the week</label>
                    <div class="day-toggles">
                        <?php foreach (GASOLINE_WEEKDAYS as $day) { ?>
                        <label class="day-toggle"><input type="checkbox" name="notify_days[]" value="<?= h($day) ?>" <?= isset($selectedDays[$day]) ? 'checked' : '' ?>><span data-i18n="day_<?= h($day) ?>"><?= h(ucfirst($day)) ?></span></label>
                        <?php } ?>
                    </div>
                </div>
                <div class="field">
                    <label data-i18n="notifyWindows">Time windows</label>
                    <div class="row-list" id="window-list">
                        <?php foreach ($windowRows as $window) {
                            $pair = explode('-', $window);
                            $from = h(trim($pair[0] ?? ''));
                            $to = h(trim($pair[1] ?? ''));
                        ?>
                        <div class="row-item">
                            <input type="text" class="time-input" name="notify_windows_from[]" value="<?= $from ?>" required maxlength="5" pattern="([01][0-9]|2[0-3]):[0-5][0-9]" placeholder="HH:MM" title="HH:MM">
                            <span>–</span>
                            <input type="text" class="time-input" name="notify_windows_to[]" value="<?= $to ?>" required maxlength="5" pattern="([01][0-9]|2[0-3]):[0-5][0-9]" placeholder="HH:MM" title="HH:MM">
                            <button type="button" class="btn-row-remove" data-i18n-aria-label="removeRow" aria-label="Remove">×</button>
                        </div>
                        <?php } ?>
                    </div>
                    <button type="button" class="btn-row-add" data-add-row="window" data-i18n="addWindow">Add window</button>
                </div>
                <div class="field">
                    <label data-i18n="notifySuggestTimes">Daily suggestion times</label>
                    <div class="row-list" id="suggest-time-list">
                        <?php foreach ($timeRows as $time) { ?>
                        <div class="row-item">
                            <input type="text" class="time-input" name="notify_suggest_times[]" value="<?= h($time) ?>" required maxlength="5" pattern="([01][0-9]|2[0-3]):[0-5][0-9]" placeholder="HH:MM" title="HH:MM">
                            <button type="button" class="btn-row-remove" data-i18n-aria-label="removeRow" aria-label="Remove">×</button>
                        </div>
                        <?php } ?>
                    </div>
                    <button type="button" class="btn-row-add" data-add-row="suggest-time" data-i18n="addTime">Add time</button>
                </div>
    <?php
}

function renderAccountPage(PDO $pdo, array $user): never
{
    renderPageStart('My Account', $user, 'account');
    ?>
    <div class="settings-layout">
        <?php renderFlash(); ?>
        <div class="settings-card">
            <h2 data-i18n="changePassword">Change password</h2>
            <form method="post" action="">
                <?= csrfField() ?>
                <input type="hidden" name="action" value="change_password">
                <div class="field">
                    <label for="cp-current" data-i18n="currentPassword">Current password</label>
                    <input type="password" id="cp-current" name="current_password" required autocomplete="current-password">
                </div>
                <div class="field">
                    <label for="cp-new" data-i18n="newPassword">New password</label>
                    <input type="password" id="cp-new" name="new_password" required minlength="10" autocomplete="new-password">
                </div>
                <div class="field">
                    <label for="cp-new2" data-i18n="passwordRepeat">Repeat password</label>
                    <input type="password" id="cp-new2" name="new_password_repeat" required minlength="10" autocomplete="new-password">
                </div>
                <button type="submit" class="btn-primary" data-i18n="save">Save</button>
            </form>
        </div>

        <div class="settings-card">
            <h2 data-i18n="notifySettings">Notifications</h2>
            <form method="post" action="">
                <?= csrfField() ?>
                <input type="hidden" name="action" value="save_notify">
                <div class="field">
                    <label for="nf-method" data-i18n="notifyMethod">Delivery method</label>
                    <select id="nf-method" name="notify_method">
                        <option value="pushover" selected>Pushover</option>
                    </select>
                </div>
                <div class="field">
                    <label for="nf-app" data-i18n="pushoverAppName">Notification title</label>
                    <input type="text" id="nf-app" name="pushover_app_name" value="<?= h($user['pushover_app_name']) ?>">
                    <p class="field-hint" data-i18n="pushoverAppNameHint">Shown as the title of your notifications unless an administrator has configured a title template.</p>
                </div>
                <div class="field">
                    <label for="nf-user" data-i18n="pushoverUserKey">Pushover user key</label>
                    <input type="text" id="nf-user" name="pushover_user_key" value="<?= h($user['pushover_user_key']) ?>" autocomplete="off">
                </div>
                <div class="field">
                    <label for="nf-token" data-i18n="pushoverToken">Pushover API token</label>
                    <input type="text" id="nf-token" name="pushover_token" value="<?= h($user['pushover_token']) ?>" autocomplete="off">
                </div>
                <?php renderScheduleEditor((string) $user['notify_days'], (string) $user['notify_windows'], (string) $user['notify_suggest_times']); ?>
                <?php
                $currentFuel = strtolower(trim((string) ($user['notify_fuel'] ?? '')));
                if (!in_array($currentFuel, GASOLINE_FUELS, true)) {
                    $currentFuel = GASOLINE_FUELS[0];
                }
                $fuelLabels = ['diesel' => 'Diesel', 'e5' => 'E5', 'e10' => 'E10'];
                $fuelI18n = ['diesel' => 'fuelDiesel', 'e5' => 'fuelE5', 'e10' => 'fuelE10'];
                ?>
                <div class="field">
                    <label for="nf-fuel" data-i18n="notifyFuel">Fuel to be notified about</label>
                    <select id="nf-fuel" name="notify_fuel">
                        <?php foreach (GASOLINE_FUELS as $f) { ?>
                        <option value="<?= h($f) ?>" data-i18n="<?= h($fuelI18n[$f]) ?>" <?= $currentFuel === $f ? 'selected' : '' ?>><?= h($fuelLabels[$f]) ?></option>
                        <?php } ?>
                    </select>
                    <p class="field-hint" data-i18n="notifyFuelHint">You are notified about this fuel only. All three are tracked, so any choice is served.</p>
                </div>
                <?php
                // The subscription area. Coordinates are stored on the account,
                // so the city is only a way of picking them: the notification
                // itself is "every station within N km of here".
                $notifyCityKey = trim((string) ($user['notify_city'] ?? ''));
                $notifyCityRow = resolveCity($pdo, $notifyCityKey);
                $notifyRadius = (float) ($user['notify_radius_km'] ?? 0);
                ?>
                <div class="field">
                    <label for="nf-city" data-i18n="notifyLocation">Notify me around</label>
                    <div class="city-ac" id="nf-city-ac">
                        <input
                            type="text"
                            id="nf-city"
                            class="city-ac-input"
                            data-i18n-placeholder="enterCity"
                            placeholder="Enter city..."
                            autocomplete="off"
                            spellcheck="false"
                            value="<?= h($notifyCityRow ? (string) $notifyCityRow['display_name'] : '') ?>"
                            aria-autocomplete="list"
                            aria-controls="nf-city-list"
                            aria-expanded="false"
                        >
                        <input type="hidden" name="notify_city" id="nf-city-value" value="<?= h($notifyCityKey) ?>">
                        <ul class="city-ac-list" id="nf-city-list" role="listbox" hidden></ul>
                    </div>
                </div>
                <div class="field">
                    <label for="nf-radius" data-i18n="notifyRadius">Radius (km)</label>
                    <input type="number" id="nf-radius" name="notify_radius_km" min="1" max="<?= GASOLINE_MAX_NOTIFY_RADIUS_KM ?>" step="any"
                        value="<?= $notifyRadius > 0 ? h(formatRadiusKm($notifyRadius)) : '' ?>">
                    <p class="field-hint" data-i18n="notifyLocationHint">Notifications cover every tracked station within this distance of the city you pick, and distances are measured from there. Stations only appear once your administrator's update targets actually collect them.</p>
                </div>
                <div class="field">
                    <label class="check-toggle"><input type="checkbox" name="notify_suggest_enabled" <?= (int) $user['notify_suggest_enabled'] === 1 ? 'checked' : '' ?>><span data-i18n="notifySuggestEnabled">Send daily price suggestions</span></label>
                    <label class="check-toggle"><input type="checkbox" name="notify_check_enabled" <?= (int) $user['notify_check_enabled'] === 1 ? 'checked' : '' ?>><span data-i18n="notifyCheckEnabled">Send buy-now alerts when prices drop</span></label>
                    <p class="field-hint" data-i18n="notifyKindsHint">Choose which notifications you receive. Suggestions forecast good times to fill up; buy-now alerts fire when a current price drops. Leave both off to pause all notifications.</p>
                </div>
                <button type="submit" class="btn-primary" data-i18n="save">Save</button>
            </form>
        </div>

        <script>
        /* City autocomplete for the notification area. Unlike the dashboard's
           filter this must not submit on selection: the radius is picked
           afterwards and the form is saved explicitly. */
        (function () {
            const wrap   = document.getElementById('nf-city-ac');
            const input  = document.getElementById('nf-city');
            const hidden = document.getElementById('nf-city-value');
            const list   = document.getElementById('nf-city-list');
            if (!wrap || !input || !hidden || !list) return;

            let controller = null;
            let debounceTimer = null;

            const hide = () => { list.hidden = true; input.setAttribute('aria-expanded', 'false'); };
            const show = () => { list.hidden = false; input.setAttribute('aria-expanded', 'true'); };

            async function fetchMatches(q) {
                if (controller) controller.abort();
                controller = new AbortController();
                try {
                    const url = new URL(location.href);
                    url.search = '';
                    url.searchParams.set('action', 'city_search');
                    url.searchParams.set('q', q);
                    const res = await fetch(url.toString(), { signal: controller.signal });
                    return await res.json();
                } catch { return null; }
            }

            input.addEventListener('input', () => {
                const q = input.value.trim();
                // Clearing the key makes a stale selection impossible to save.
                hidden.value = '';
                clearTimeout(debounceTimer);
                if (q.length < 3) { hide(); return; }
                debounceTimer = setTimeout(async () => {
                    const results = await fetchMatches(q);
                    if (results === null) return;
                    list.innerHTML = '';
                    // A notification covers a city, so the postal-code hits the
                    // dashboard's own picker offers are dropped here: they carry
                    // no city key to subscribe to.
                    const cities = results.filter((match) => match.city_key);
                    cities.forEach(({ city_key, label }) => {
                        const li = document.createElement('li');
                        li.className = 'city-ac-item';
                        li.role = 'option';
                        li.setAttribute('aria-selected', 'false');
                        li.textContent = label || city_key;
                        li.addEventListener('mousedown', (e) => {
                            e.preventDefault();
                            input.value = label || city_key;
                            hidden.value = city_key;
                            hide();
                        });
                        list.appendChild(li);
                    });
                    if (cities.length) show(); else hide();
                }, 200);
            });

            input.addEventListener('blur', () => setTimeout(hide, 150));
            document.addEventListener('click', (e) => { if (!wrap.contains(e.target)) hide(); });
        })();
        </script>

        <div class="settings-card danger">
            <h2 data-i18n="dangerZone">Danger zone</h2>
            <form method="post" action="" data-confirm="deleteAccountConfirm">
                <?= csrfField() ?>
                <input type="hidden" name="action" value="delete_account">
                <div class="field">
                    <label for="da-password" data-i18n="currentPassword">Current password</label>
                    <input type="password" id="da-password" name="current_password" required autocomplete="current-password">
                </div>
                <div class="field">
                    <label class="check-toggle"><input type="checkbox" name="confirm" required><span data-i18n="deleteAccountConfirmLabel">I understand that my account and settings will be permanently deleted.</span></label>
                </div>
                <button type="submit" class="btn-danger" data-i18n="deleteAccount">Delete account</button>
            </form>
        </div>
    </div>
    <?php
    renderPageEnd();
}

function renderAdminUsersPage(PDO $pdo, array $user): never
{
    $users = $pdo->query('SELECT id, email, is_admin, status, created_at, approved_at FROM users ORDER BY created_at ASC, id ASC')->fetchAll();
    renderPageStart('Users', $user, 'admin_users');
    ?>
    <div class="settings-layout wide">
        <?php renderFlash(); ?>
        <div class="settings-card">
            <h2 data-i18n="adminUsersTitle">Users</h2>
            <div class="table-scroll">
            <table class="stack-table">
                <thead>
                    <tr>
                        <th data-i18n="colEmail">Email</th>
                        <th data-i18n="colStatus">Status</th>
                        <th data-i18n="colAdmin">Admin</th>
                        <th data-i18n="colCreated">Registered</th>
                        <th data-i18n="colApproved">Approved</th>
                        <th data-i18n="colActions">Actions</th>
                    </tr>
                </thead>
                <tbody>
                    <?php foreach ($users as $row) { $isSelf = (int) $row['id'] === (int) $user['id']; ?>
                    <tr>
                        <td class="stack-primary"><?= h($row['email']) ?><?= $isSelf ? ' <span class="badge">you</span>' : '' ?></td>
                        <td data-label="Status" data-i18n-label="colStatus"><span class="badge <?= $row['status'] === 'approved' ? 'ok' : 'warn' ?>" data-i18n="status<?= h(ucfirst($row['status'])) ?>"><?= h($row['status']) ?></span></td>
                        <td <?= (int) $row['is_admin'] === 1 ? 'data-label="Admin" data-i18n-label="colAdmin"' : '' ?>><?= (int) $row['is_admin'] === 1 ? '<span class="badge ok" data-i18n="adminYes">admin</span>' : '' ?></td>
                        <td data-label="Registered" data-i18n-label="colCreated" data-recorded-at="<?= h((string) $row['created_at']) ?>"><?= h((string) $row['created_at']) ?></td>
                        <td data-label="Approved" data-i18n-label="colApproved" <?= $row['approved_at'] !== null ? 'data-recorded-at="' . h((string) $row['approved_at']) . '"' : '' ?>><?= h((string) ($row['approved_at'] ?? '—')) ?></td>
                        <td class="actions-cell">
                            <?php if ($row['status'] === 'pending') { ?>
                            <form method="post" action="" class="table-form"><?= csrfField() ?><input type="hidden" name="action" value="approve_user"><input type="hidden" name="user_id" value="<?= (int) $row['id'] ?>"><button type="submit" class="btn-small" data-i18n="actionApprove">Approve</button></form>
                            <?php } ?>
                            <?php if (!$isSelf) { ?>
                            <form method="post" action="" class="table-form"><?= csrfField() ?><input type="hidden" name="action" value="set_admin"><input type="hidden" name="user_id" value="<?= (int) $row['id'] ?>"><input type="hidden" name="admin" value="<?= (int) $row['is_admin'] === 1 ? '0' : '1' ?>"><button type="submit" class="btn-small" data-i18n="<?= (int) $row['is_admin'] === 1 ? 'actionDemote' : 'actionPromote' ?>"><?= (int) $row['is_admin'] === 1 ? 'Demote' : 'Promote' ?></button></form>
                            <form method="post" action="" class="table-form" data-confirm="confirmDeleteUser"><?= csrfField() ?><input type="hidden" name="action" value="delete_user"><input type="hidden" name="user_id" value="<?= (int) $row['id'] ?>"><button type="submit" class="btn-small danger" data-i18n="actionDelete">Delete</button></form>
                            <?php } ?>
                        </td>
                    </tr>
                    <?php } ?>
                </tbody>
            </table>
            </div>
        </div>
    </div>
    <?php
    renderPageEnd();
}

function renderAdminStationsPage(PDO $pdo, array $user): never
{
    $renamed = $pdo->query(
        <<<'SQL'
        SELECT id, name, name_override, street, house_number, post_code, place
        FROM stations
        WHERE name_override IS NOT NULL
        ORDER BY name_override ASC, id ASC
        SQL
    )->fetchAll();
    renderPageStart('Stations', $user, 'admin_stations');
    ?>
    <div class="settings-layout wide">
        <?php renderFlash(); ?>

        <div class="settings-card">
            <h2 data-i18n="renameStation">Rename a station</h2>
            <p class="auth-note" data-i18n="renameStationHint">The new name replaces the Tankerkönig name everywhere — dashboard, CLI output, and notifications. The original name is kept and can be restored at any time.</p>
            <form method="post" action="" id="rename-form">
                <?= csrfField() ?>
                <input type="hidden" name="action" value="rename_station">
                <div class="field">
                    <label for="st-search" data-i18n="station">Station</label>
                    <div class="city-ac" id="station-ac">
                        <input
                            type="text"
                            id="st-search"
                            class="city-ac-input"
                            data-i18n-placeholder="stationSearchPlaceholder"
                            placeholder="Search by name or address..."
                            autocomplete="off"
                            spellcheck="false"
                            aria-autocomplete="list"
                            aria-controls="station-ac-list"
                            aria-expanded="false"
                        >
                        <input type="hidden" name="station_id" id="st-station-id" value="">
                        <ul class="city-ac-list" id="station-ac-list" role="listbox" hidden></ul>
                    </div>
                </div>
                <div class="field">
                    <label for="st-new-name" data-i18n="newStationName">New name</label>
                    <input type="text" id="st-new-name" name="new_name" required maxlength="200" autocomplete="off">
                </div>
                <button type="submit" class="btn-primary" id="st-apply" data-i18n="applyRename" disabled>Apply</button>
            </form>
        </div>

        <div class="settings-card">
            <h2 data-i18n="renamedStations">Renamed stations</h2>
            <div class="table-scroll">
            <table class="stack-table">
                <thead>
                    <tr><th data-i18n="station">Station</th><th data-i18n="colNewName">New name</th></tr>
                </thead>
                <tbody>
                    <?php foreach ($renamed as $row) { ?>
                    <tr>
                        <td class="stack-primary"><?= h((string) $row['name']) ?><span class="station-sub"><?= h(stationAddress($row)) ?></span></td>
                        <td class="rename-cell" data-label="New name" data-i18n-label="colNewName">
                            <div class="rename-controls">
                                <form method="post" action="" class="rename-form"><?= csrfField() ?><input type="hidden" name="action" value="rename_station"><input type="hidden" name="station_id" value="<?= h((string) $row['id']) ?>"><input type="text" name="new_name" value="<?= h((string) $row['name_override']) ?>" required maxlength="200"><button type="submit" class="btn-icon" aria-label="Save" title="Save" data-i18n-aria-label="save" data-i18n-title="save"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M19 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h11l5 5v11a2 2 0 0 1-2 2z"/><polyline points="17 21 17 13 7 13 7 21"/><polyline points="7 3 7 8 15 8"/></svg></button></form>
                                <form method="post" action="" class="rename-remove" data-confirm="confirmRemoveRename"><?= csrfField() ?><input type="hidden" name="action" value="clear_station_rename"><input type="hidden" name="station_id" value="<?= h((string) $row['id']) ?>"><button type="submit" class="btn-icon danger" aria-label="Remove" title="Remove" data-i18n-aria-label="removeRename" data-i18n-title="removeRename"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="3 6 5 6 21 6"/><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"/><line x1="10" y1="11" x2="10" y2="17"/><line x1="14" y1="11" x2="14" y2="17"/></svg></button></form>
                            </div>
                        </td>
                    </tr>
                    <?php } ?>
                    <?php if ($renamed === []) { ?>
                    <tr><td colspan="2" data-i18n="noRenames">No stations have been renamed yet.</td></tr>
                    <?php } ?>
                </tbody>
            </table>
            </div>
        </div>
    </div>
    <script>
    /* ── Station autocomplete (rename form) ────────────────────────── */
    (function () {
        const wrap    = document.getElementById('station-ac');
        const input   = document.getElementById('st-search');
        const hidden  = document.getElementById('st-station-id');
        const list    = document.getElementById('station-ac-list');
        const newName = document.getElementById('st-new-name');
        const apply   = document.getElementById('st-apply');
        const form    = document.getElementById('rename-form');

        if (!wrap || !input || !hidden || !list || !newName || !apply || !form) return;

        let controller = null;
        let activeIdx  = -1;
        let debounceTimer = null;

        function showList() {
            list.hidden = false;
            input.setAttribute('aria-expanded', 'true');
        }

        function hideList() {
            list.hidden = true;
            input.setAttribute('aria-expanded', 'false');
            activeIdx = -1;
        }

        function setActive(idx) {
            const items = list.querySelectorAll('.city-ac-item');
            items.forEach((el, i) => el.setAttribute('aria-selected', String(i === idx)));
            activeIdx = idx;
        }

        function selectStation(station) {
            input.value  = station.address ? station.name + ' — ' + station.address : station.name;
            hidden.value = station.id;
            newName.value = station.name;
            apply.disabled = false;
            hideList();
            newName.focus();
            newName.select();
        }

        async function fetchMatches(q) {
            if (controller) controller.abort();
            controller = new AbortController();
            try {
                const url = new URL(location.href);
                url.search = '';
                url.searchParams.set('action', 'station_search');
                url.searchParams.set('q', q);
                const res = await fetch(url.toString(), { signal: controller.signal });
                return await res.json();
            } catch {
                return null;
            }
        }

        input.addEventListener('input', () => {
            hidden.value = '';
            apply.disabled = true;
            clearTimeout(debounceTimer);
            const q = input.value.trim();
            if (q.length < 2) { hideList(); return; }

            debounceTimer = setTimeout(async () => {
                const results = await fetchMatches(q);
                if (!Array.isArray(results)) return;

                list.innerHTML = '';
                if (results.length === 0) {
                    const empty = document.createElement('li');
                    empty.className = 'city-ac-empty';
                    empty.textContent = '— no matches —';
                    list.appendChild(empty);
                } else {
                    results.forEach((station) => {
                        const li = document.createElement('li');
                        li.className = 'city-ac-item station-ac-item';
                        li.role      = 'option';
                        li.setAttribute('aria-selected', 'false');
                        const name = document.createElement('span');
                        name.className   = 'ac-name';
                        name.textContent = station.name;
                        li.appendChild(name);
                        const subText = [station.brand, station.address].filter(Boolean).join(' · ');
                        if (subText !== '') {
                            const sub = document.createElement('span');
                            sub.className   = 'ac-sub';
                            sub.textContent = subText;
                            li.appendChild(sub);
                        }
                        li.addEventListener('mousedown', (e) => {
                            e.preventDefault();
                            selectStation(station);
                        });
                        list.appendChild(li);
                    });
                }
                showList();
                activeIdx = -1;
            }, 200);
        });

        input.addEventListener('keydown', (e) => {
            const items = [...list.querySelectorAll('.city-ac-item')];
            if (e.key === 'ArrowDown') {
                e.preventDefault();
                setActive(Math.min(activeIdx + 1, items.length - 1));
            } else if (e.key === 'ArrowUp') {
                e.preventDefault();
                setActive(Math.max(activeIdx - 1, 0));
            } else if (e.key === 'Enter' && !list.hidden && activeIdx >= 0 && items[activeIdx]) {
                e.preventDefault();
                items[activeIdx].dispatchEvent(new MouseEvent('mousedown'));
            } else if (e.key === 'Escape') {
                hideList();
            }
        });

        input.addEventListener('blur', () => setTimeout(hideList, 150));

        document.addEventListener('click', (e) => {
            if (!wrap.contains(e.target)) hideList();
        });

        form.addEventListener('submit', (e) => {
            if (hidden.value === '') {
                e.preventDefault();
                input.focus();
            }
        });
    })();
    </script>
    <?php
    renderPageEnd();
}

function renderAdminPredictionsPage(PDO $pdo, string $driver, array $user): never
{

    // Default the fuel picker to whichever fuel has the most evaluated predictions,
    // so the page lands on data instead of an empty set.
    $defaultFuel = 'diesel';
    try {
        $fuelRow = $pdo->query(
            "SELECT fuel FROM price_predictions WHERE actual_price IS NOT NULL GROUP BY fuel ORDER BY COUNT(*) DESC"
        )->fetch();
        if ($fuelRow !== false && in_array((string) $fuelRow['fuel'], ['diesel', 'e5', 'e10'], true)) {
            $defaultFuel = (string) $fuelRow['fuel'];
        }
    } catch (Throwable $e) {
        // Ignore — keep the diesel default.
    }

    $fuelLabels = ['diesel' => 'Diesel', 'e5' => 'E5', 'e10' => 'E10'];
    $fuelI18n = ['diesel' => 'fuelDiesel', 'e5' => 'fuelE5', 'e10' => 'fuelE10'];

    renderPageStart('Prediction accuracy', $user, 'admin_predictions');
    ?>
    <style>
        .pred-layout { max-width: 1180px; }
        .pred-filters { display: grid; grid-template-columns: repeat(auto-fit, minmax(150px, 1fr)); gap: 0 1.1rem; }
        .pred-filters .field { margin-bottom: 0.6rem; }
        /* Long-press summons the crosshair on touch, so keep the browser's
           own long-press reactions (selection, iOS callout) off the chart. */
        #pred-chart { width: 100%; display: block; height: auto; -webkit-user-select: none; user-select: none; -webkit-touch-callout: none; }
        .pred-note { font-family: var(--mono); font-size: 0.76rem; color: var(--muted); margin-bottom: 0.85rem; }
        .pred-err-good { color: var(--e10); }
        .pred-err-bad  { color: var(--red); }
        .pred-sugg { color: var(--amber); margin-left: 0.25rem; }
        .pred-legend-line { width: 16px; height: 3px; border-radius: 2px; display: inline-block; }
        .pred-legend-band { width: 16px; height: 10px; border-radius: 2px; display: inline-block; background: rgba(245,166,35,0.25); border: 1px solid rgba(245,166,35,0.45); }
        .pred-legend-diag { width: 16px; height: 0; border-top: 2px dashed var(--muted); display: inline-block; }
        .stat-value.pred-small { font-size: 1.15rem; }
    </style>
    <div id="price-tooltip" role="tooltip" aria-hidden="true"></div>
    <div class="settings-layout wide pred-layout">
        <?php renderFlash(); ?>

        <div class="settings-card">
            <h2 data-i18n="predAccuracyTitle">Prediction accuracy</h2>
            <p class="auth-note" data-i18n="predAccuracyHint">Compares each past prediction with the actual price recorded for that target window. Only evaluated predictions — whose target hour has passed and had a recorded price — are included.</p>
            <div class="pred-filters">
                <div class="field">
                    <label for="pred-fuel" data-i18n="fuelType">Fuel type</label>
                    <select id="pred-fuel">
                        <?php foreach ($fuelLabels as $fuelValue => $fuelLabel) { ?>
                        <option value="<?= h($fuelValue) ?>" data-i18n="<?= h($fuelI18n[$fuelValue]) ?>" <?= $defaultFuel === $fuelValue ? 'selected' : '' ?>><?= h($fuelLabel) ?></option>
                        <?php } ?>
                    </select>
                </div>
                <div class="field">
                    <label for="pred-range" data-i18n="predRange">Target range</label>
                    <select id="pred-range">
                        <option value="7d" data-i18n="range7d">7d</option>
                        <option value="14d" selected data-i18n="range14d">14d</option>
                        <option value="30d" data-i18n="range30d">30d</option>
                    </select>
                </div>
                <div class="field">
                    <label for="pred-conf" data-i18n="predConfidence">Confidence</label>
                    <select id="pred-conf">
                        <option value="all" data-i18n="predConfAll">All</option>
                        <option value="medium_high" data-i18n="predConfMediumHigh">Medium + High</option>
                    </select>
                </div>
            </div>
        </div>

        <div class="stats" aria-live="polite">
            <div class="stat"><div class="stat-label" data-i18n="predStatCount">Evaluated</div><div class="stat-value skeleton" id="ps-count" aria-busy="true">&nbsp;</div></div>
            <div class="stat"><div class="stat-label" data-i18n="predStatStations">Stations</div><div class="stat-value skeleton" id="ps-stations" aria-busy="true">&nbsp;</div></div>
            <div class="stat"><div class="stat-label" data-i18n="predStatMae">MAE</div><div class="stat-value skeleton pred-small" id="ps-mae" aria-busy="true">&nbsp;</div></div>
            <div class="stat"><div class="stat-label" data-i18n="predStatBias">Bias (act − pred)</div><div class="stat-value skeleton pred-small" id="ps-bias" aria-busy="true">&nbsp;</div></div>
            <div class="stat"><div class="stat-label" data-i18n="predStatRmse">RMSE</div><div class="stat-value skeleton pred-small" id="ps-rmse" aria-busy="true">&nbsp;</div></div>
            <div class="stat"><div class="stat-label" data-i18n="predStatWithin1">Within ±1 ct</div><div class="stat-value skeleton pred-small" id="ps-within1" aria-busy="true">&nbsp;</div></div>
            <div class="stat"><div class="stat-label" data-i18n="predStatWithin2">Within ±2 ct</div><div class="stat-value skeleton pred-small" id="ps-within2" aria-busy="true">&nbsp;</div></div>
            <div class="stat"><div class="stat-label" data-i18n="predStatWorst">Worst error</div><div class="stat-value skeleton pred-small" id="ps-worst" aria-busy="true">&nbsp;</div></div>
        </div>

        <p class="auth-note" data-i18n="predLatestHint">The tiles above count every stored prediction, so each target window appears once per hourly run and long leads dominate. These tiles keep only the latest prediction per station and window — the accuracy of acting on fresh output.</p>
        <div class="stats" aria-live="polite">
            <div class="stat"><div class="stat-label" data-i18n="predStatLatestCount">Latest run: evaluated</div><div class="stat-value skeleton" id="ps-l-count" aria-busy="true">&nbsp;</div></div>
            <div class="stat"><div class="stat-label" data-i18n="predStatLatestMae">Latest run: MAE</div><div class="stat-value skeleton pred-small" id="ps-l-mae" aria-busy="true">&nbsp;</div></div>
            <div class="stat"><div class="stat-label" data-i18n="predStatLatestBias">Latest run: bias</div><div class="stat-value skeleton pred-small" id="ps-l-bias" aria-busy="true">&nbsp;</div></div>
            <div class="stat"><div class="stat-label" data-i18n="predStatLatestWithin2">Latest run: within ±2 ct</div><div class="stat-value skeleton pred-small" id="ps-l-within2" aria-busy="true">&nbsp;</div></div>
        </div>

        <div class="chart-card">
            <div class="chart-header">
                <span class="chart-title" data-i18n="predChartTitle">Predicted vs. actual</span>
                <div class="range-toggles" id="pred-view-toggles">
                    <button type="button" class="range-toggle active" data-view="timeline" data-i18n="predViewTimeline">Timeline</button>
                    <button type="button" class="range-toggle" data-view="scatter" data-i18n="predViewScatter">Scatter</button>
                </div>
            </div>
            <div class="chart-body">
                <div class="chart-loading" id="pred-chart-loading" role="status"><span class="spinner" aria-hidden="true"></span><span class="sr-only" data-i18n="loading">Loading…</span></div>
                <svg id="pred-chart" viewBox="0 0 960 380" role="img" hidden></svg>
            </div>
            <div class="chart-legend" id="pred-legend" hidden></div>
            <div class="chart-empty" id="pred-chart-empty" data-i18n="predNoData" role="status" hidden>No evaluated predictions match the current filters.</div>
        </div>

        <div class="settings-card">
            <h2 data-i18n="predByConfidence">Accuracy by confidence</h2>
            <div class="table-scroll">
                <table class="stack-table">
                    <thead><tr><th data-i18n="predColConf">Confidence</th><th data-i18n="predColCount">Count</th><th data-i18n="predStatMae">MAE</th><th data-i18n="predStatBias">Bias</th></tr></thead>
                    <tbody id="pred-conf-tbody"><tr><td colspan="4" class="table-loading" aria-busy="true"><span class="spinner" aria-hidden="true"></span></td></tr></tbody>
                </table>
            </div>
        </div>

        <div class="settings-card">
            <h2 data-i18n="predByLead">Accuracy by lead time</h2>
            <p class="auth-note" data-i18n="predLeadHint">How far ahead the prediction was made. Errors beyond six hours are dominated by price moves nobody could have known about, which is why only shorter leads train the bias correction.</p>
            <div class="table-scroll">
                <table class="stack-table">
                    <thead><tr><th data-i18n="predColBucket">Lead time</th><th data-i18n="predColCount">Count</th><th data-i18n="predStatMae">MAE</th><th data-i18n="predStatBias">Bias</th></tr></thead>
                    <tbody id="pred-lead-tbody"><tr><td colspan="4" class="table-loading" aria-busy="true"><span class="spinner" aria-hidden="true"></span></td></tr></tbody>
                </table>
            </div>
        </div>

        <div class="settings-card">
            <h2 data-i18n="predByHour">Accuracy by hour (UTC)</h2>
            <p class="auth-note" data-i18n="predHourHint">Hours are the target hour in UTC, not your local time. A consistent bias in particular hours means the model misses that part of the daily price curve.</p>
            <div class="table-scroll">
                <table class="stack-table">
                    <thead><tr><th data-i18n="predColHour">Hour (UTC)</th><th data-i18n="predColCount">Count</th><th data-i18n="predStatMae">MAE</th><th data-i18n="predStatBias">Bias</th></tr></thead>
                    <tbody id="pred-hour-tbody"><tr><td colspan="4" class="table-loading" aria-busy="true"><span class="spinner" aria-hidden="true"></span></td></tr></tbody>
                </table>
            </div>
        </div>

        <div class="settings-card" id="pred-dec-card" hidden>
            <h2 data-i18n="predDecTitle">Alert outcomes</h2>
            <p class="auth-note" data-i18n="predDecHint">What the check path decided, scored against the cheapest price that pricing day actually offered. Regret is how much more than the day's low the price was at the moment of the decision, so a good "buy" has a regret near zero. These are the model's decisions recorded on the suggestion timer, not a log of delivered notifications: per-user schedules, city selections and the repeat-suppression baseline are not reflected here.</p>
            <div class="table-scroll">
                <table class="stack-table">
                    <thead><tr><th data-i18n="predColRecommendation">Recommendation</th><th data-i18n="predColCount">Count</th><th data-i18n="predColRegret">Mean regret</th><th data-i18n="predColHit1">Within 1 ct</th><th data-i18n="predColHit2">Within 2 ct</th></tr></thead>
                    <tbody id="pred-dec-tbody"><tr><td colspan="5" class="table-loading" aria-busy="true"><span class="spinner" aria-hidden="true"></span></td></tr></tbody>
                </table>
            </div>
        </div>

        <div class="settings-card">
            <h2 data-i18n="predRawTitle">Raw data</h2>
            <div class="pred-note" id="pred-truncated" data-i18n="predTruncated" hidden>Showing the most recent 1,000 rows; the statistics above cover the full filtered set.</div>
            <div class="table-scroll">
                <table class="stack-table">
                    <thead>
                        <tr>
                            <th data-i18n="predColTarget">Target window</th>
                            <th data-i18n="predColStation">Station</th>
                            <th data-i18n="predColRunAt">Predicted at</th>
                            <th data-i18n="predColLead">Lead time</th>
                            <th data-i18n="predColConf">Confidence</th>
                            <th data-i18n="predColPredicted">Predicted</th>
                            <th data-i18n="predColActual">Actual</th>
                            <th data-i18n="predColError">Error</th>
                        </tr>
                    </thead>
                    <tbody id="pred-tbody"><tr><td colspan="8" class="table-loading" aria-busy="true"><span class="spinner" aria-hidden="true"></span></td></tr></tbody>
                </table>
            </div>
            <div class="table-more" id="pred-more" hidden>
                <button type="button" class="btn-reset" id="pred-more-btn"></button>
            </div>
        </div>
    </div>

    <script>
    (function () {
        const cfg = {
            fuel:  document.getElementById('pred-fuel'),
            range: document.getElementById('pred-range'),
            conf:  document.getElementById('pred-conf'),
        };
        const loadingEl  = document.getElementById('pred-chart-loading');
        const chartEl    = document.getElementById('pred-chart');
        const legendEl   = document.getElementById('pred-legend');
        const emptyEl    = document.getElementById('pred-chart-empty');
        const tbody      = document.getElementById('pred-tbody');
        const confTbody  = document.getElementById('pred-conf-tbody');
        const leadTbody  = document.getElementById('pred-lead-tbody');
        const hourTbody  = document.getElementById('pred-hour-tbody');
        const decTbody   = document.getElementById('pred-dec-tbody');
        const decCard    = document.getElementById('pred-dec-card');
        const moreWrap   = document.getElementById('pred-more');
        const moreBtn    = document.getElementById('pred-more-btn');
        const truncEl    = document.getElementById('pred-truncated');
        const viewTogl   = document.getElementById('pred-view-toggles');
        const statIds    = ['ps-count','ps-stations','ps-mae','ps-bias','ps-rmse','ps-within1','ps-within2','ps-worst','ps-l-count','ps-l-mae','ps-l-bias','ps-l-within2'];

        const NS = 'http://www.w3.org/2000/svg';
        const C_PRED = '#f5a623', C_ACTUAL = '#60a5fa', C_BAND = 'rgba(245,166,35,0.18)';

        let data = null;
        let view = 'timeline';
        let rowsRendered = 0;
        const PAGE = 100;

        const T   = () => translations[currentLang];
        const loc = () => currentLang === 'de' ? 'de-DE' : 'en-GB';
        const tz  = () => currentLang === 'de' ? 'Europe/Berlin' : 'UTC';

        function esc(s) { return String(s).replace(/[&<>"']/g, (c) => ({ '&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;' }[c])); }
        // Numbers on this page stay plain text. The stat tiles and the tables
        // lay a cell's children out as their own columns for narrow screens, so
        // a raised digit or a sized-down separator — both of which need a span —
        // breaks out of the number and lands in a column of its own. The
        // separator still follows the UI language, since that is text.
        function fmtEur(v) { return fmtPriceText(v); }
        function fmtCt(v) { const s = (v === null || v === undefined) ? null : fmtDecimal(v * 100, 2); return s === null ? '—' : s + ' ct'; }
        function fmtSignedCt(v) { if (v === null || v === undefined) return '—'; const c = v * 100; const s = fmtDecimal(c, 2); return s === null ? '—' : (c >= 0 ? '+' : '') + s + ' ct'; }
        function fmtPct(v) { const s = fmtDecimal(v, 1); return s === null ? '—' : s + '%'; }
        function fmtInt(v) { return Number(v || 0).toLocaleString(loc()); }
        function fmtDateTime(iso) { if (!iso) return '—'; return new Date(iso).toLocaleString(loc(), { timeZone: tz(), year: 'numeric', month: 'short', day: '2-digit', hour: '2-digit', minute: '2-digit' }); }
        function fmtDate(iso) { return new Date(iso).toLocaleDateString(loc(), { timeZone: tz(), month: 'short', day: '2-digit' }); }
        function fmtTime(iso) { return new Date(iso).toLocaleTimeString(loc(), { timeZone: tz(), hour: '2-digit', minute: '2-digit' }); }
        function fmtWindow(s, e) { return fmtTime(s) + '–' + fmtTime(e); }
        function fmtLead(min) { min = Number(min || 0); if (min < 60) return min + 'm'; const h = Math.floor(min / 60), m = min % 60; return m === 0 ? h + 'h' : h + 'h ' + m + 'm'; }
        function confLabel(c) { return T()['predConf_' + c] || c; }

        function setStat(id, val) { const el = document.getElementById(id); if (!el) return; el.textContent = val; el.classList.remove('skeleton'); el.removeAttribute('aria-busy'); }

        function buildUrl() {
            const u = new URL(location.origin + location.pathname);
            u.searchParams.set('action', 'prediction_accuracy');
            u.searchParams.set('fuel', cfg.fuel.value);
            u.searchParams.set('range', cfg.range.value);
            u.searchParams.set('confidence', cfg.conf.value);
            return u.toString();
        }

        function resetLoading() {
            if (loadingEl) loadingEl.hidden = false;
            if (chartEl) chartEl.setAttribute('hidden', '');
            if (legendEl) legendEl.hidden = true;
            if (emptyEl) { emptyEl.hidden = true; emptyEl.dataset.i18n = 'predNoData'; }
            statIds.forEach((id) => { const el = document.getElementById(id); if (el) { el.textContent = ' '; el.classList.add('skeleton'); el.setAttribute('aria-busy', 'true'); } });
            if (tbody) tbody.innerHTML = '<tr><td colspan="8" class="table-loading" aria-busy="true"><span class="spinner" aria-hidden="true"></span></td></tr>';
            if (confTbody) confTbody.innerHTML = '<tr><td colspan="4" class="table-loading" aria-busy="true"><span class="spinner" aria-hidden="true"></span></td></tr>';
            if (leadTbody) leadTbody.innerHTML = '<tr><td colspan="4" class="table-loading" aria-busy="true"><span class="spinner" aria-hidden="true"></span></td></tr>';
            if (hourTbody) hourTbody.innerHTML = '<tr><td colspan="4" class="table-loading" aria-busy="true"><span class="spinner" aria-hidden="true"></span></td></tr>';
            if (decTbody) decTbody.innerHTML = '<tr><td colspan="5" class="table-loading" aria-busy="true"><span class="spinner" aria-hidden="true"></span></td></tr>';
            if (moreWrap) moreWrap.hidden = true;
            if (truncEl) truncEl.hidden = true;
        }

        function showError(err) {
            if (loadingEl) loadingEl.hidden = true;
            const t = T();
            const key = (err && err.key && t[err.key]) ? err.key : 'loadError';
            const msg = (err && err.key && t[err.key]) ? t[err.key] : t.loadError;
            if (emptyEl) { emptyEl.hidden = false; emptyEl.dataset.i18n = key; emptyEl.textContent = msg; }
            if (chartEl) chartEl.setAttribute('hidden', '');
            if (legendEl) legendEl.hidden = true;
            statIds.forEach((id) => setStat(id, '—'));
            if (tbody) tbody.innerHTML = '<tr><td colspan="8" role="alert" style="text-align:center;color:var(--red);padding:2rem;font-family:var(--mono);font-size:.82rem" data-i18n="' + key + '">' + esc(msg) + '</td></tr>';
            if (confTbody) confTbody.innerHTML = '';
            if (leadTbody) leadTbody.innerHTML = '';
            if (hourTbody) hourTbody.innerHTML = '';
            if (decTbody) decTbody.innerHTML = '';
            if (moreWrap) moreWrap.hidden = true;
        }

        async function load() {
            resetLoading();
            try {
                const res = await fetch(buildUrl(), { headers: { Accept: 'application/json' } });
                if (res.status === 401) { location.href = '?page=login'; return; }
                if (res.status === 403) { location.href = '?'; return; }
                if (!res.ok) throw new Error('HTTP ' + res.status);
                const payload = await res.json();
                if (payload.errors && payload.errors.length) { showError(payload.errors[0]); return; }
                data = payload;
                render();
            } catch (e) {
                showError();
            }
        }

        function render() {
            if (!data) return;
            renderStats();
            renderConf();
            renderLead();
            renderHour();
            renderDecisions();
            renderChart();
            renderTable();
        }

        function renderStats() {
            if (loadingEl) loadingEl.hidden = true;
            const s = data.summary;
            if (!s) { statIds.forEach((id) => setStat(id, '—')); return; }
            setStat('ps-count', fmtInt(s.count));
            setStat('ps-stations', fmtInt(s.stations));
            setStat('ps-mae', fmtCt(s.mae));
            setStat('ps-bias', fmtSignedCt(s.bias));
            setStat('ps-rmse', fmtCt(s.rmse));
            setStat('ps-within1', fmtPct(s.within1_pct));
            setStat('ps-within2', fmtPct(s.within2_pct));
            setStat('ps-worst', fmtCt(Math.max(Math.abs(s.min_error || 0), Math.abs(s.max_error || 0))));
            const l = data.summary_latest;
            if (!l) {
                ['ps-l-count','ps-l-mae','ps-l-bias','ps-l-within2'].forEach((id) => setStat(id, '—'));
                return;
            }
            setStat('ps-l-count', fmtInt(l.count));
            setStat('ps-l-mae', fmtCt(l.mae));
            setStat('ps-l-bias', fmtSignedCt(l.bias));
            setStat('ps-l-within2', fmtPct(l.within2_pct));
        }

        function renderConf() {
            if (!confTbody) return;
            const rank = { high: 0, medium: 1, low: 2 };
            const rows = ((data && data.by_confidence) || []).slice().sort((a, b) => (rank[a.confidence] ?? 9) - (rank[b.confidence] ?? 9));
            const t = T();
            if (rows.length === 0) { confTbody.innerHTML = '<tr><td colspan="4" style="text-align:center;color:var(--muted);padding:1rem;font-family:var(--mono);font-size:.8rem">' + esc(t.predNoData) + '</td></tr>'; return; }
            confTbody.innerHTML = rows.map((r) =>
                '<tr>'
                + '<td data-label="' + esc(t.predColConf) + '">' + esc(confLabel(r.confidence)) + '</td>'
                + '<td data-label="' + esc(t.predColCount) + '">' + fmtInt(r.count) + '</td>'
                + '<td data-label="' + esc(t.predStatMae) + '">' + esc(fmtCt(r.mae)) + '</td>'
                + '<td data-label="' + esc(t.predStatBias) + '">' + esc(fmtSignedCt(r.bias)) + '</td>'
                + '</tr>'
            ).join('');
        }

        // Empty-state cell shared by the breakdown tables.
        function emptyRow(span) {
            return '<tr><td colspan="' + span + '" style="text-align:center;color:var(--muted);padding:1rem;font-family:var(--mono);font-size:.8rem">'
                + esc(T().predNoData) + '</td></tr>';
        }

        function renderLead() {
            if (!leadTbody) return;
            // Ordered by the bucket's lower bound: the labels would sort
            // lexicographically, putting "12-24h" before "1-3h".
            const rows = ((data && data.by_lead) || []).slice().sort((a, b) => (a.lead_floor || 0) - (b.lead_floor || 0));
            const t = T();
            if (rows.length === 0) { leadTbody.innerHTML = emptyRow(4); return; }
            leadTbody.innerHTML = rows.map((r) =>
                '<tr>'
                + '<td data-label="' + esc(t.predColBucket) + '">' + esc(r.bucket) + '</td>'
                + '<td data-label="' + esc(t.predColCount) + '">' + fmtInt(r.count) + '</td>'
                + '<td data-label="' + esc(t.predStatMae) + '">' + esc(fmtCt(r.mae)) + '</td>'
                + '<td data-label="' + esc(t.predStatBias) + '">' + esc(fmtSignedCt(r.bias)) + '</td>'
                + '</tr>'
            ).join('');
        }

        function renderHour() {
            if (!hourTbody) return;
            const rows = ((data && data.by_hour) || []).slice().sort((a, b) => a.hour - b.hour);
            const t = T();
            if (rows.length === 0) { hourTbody.innerHTML = emptyRow(4); return; }
            hourTbody.innerHTML = rows.map((r) =>
                '<tr>'
                + '<td data-label="' + esc(t.predColHour) + '">' + esc(String(r.hour).padStart(2, '0') + ':00') + '</td>'
                + '<td data-label="' + esc(t.predColCount) + '">' + fmtInt(r.count) + '</td>'
                + '<td data-label="' + esc(t.predStatMae) + '">' + esc(fmtCt(r.mae)) + '</td>'
                + '<td data-label="' + esc(t.predStatBias) + '">' + esc(fmtSignedCt(r.bias)) + '</td>'
                + '</tr>'
            ).join('');
        }

        function recLabel(r) { return T()['predRec_' + r] || r; }

        function renderDecisions() {
            if (!decTbody || !decCard) return;
            // null means the table does not exist in this database yet.
            const rows = (data && data.decisions) || null;
            if (rows === null) { decCard.hidden = true; return; }
            decCard.hidden = false;
            const rank = { buy: 0, hold: 1, wait: 2 };
            const sorted = rows.slice().sort((a, b) => (rank[a.recommendation] ?? 9) - (rank[b.recommendation] ?? 9));
            const t = T();
            if (sorted.length === 0) { decTbody.innerHTML = emptyRow(5); return; }
            decTbody.innerHTML = sorted.map((r) =>
                '<tr>'
                + '<td data-label="' + esc(t.predColRecommendation) + '">' + esc(recLabel(r.recommendation)) + '</td>'
                + '<td data-label="' + esc(t.predColCount) + '">' + fmtInt(r.count) + '</td>'
                + '<td data-label="' + esc(t.predColRegret) + '">' + esc(fmtCt(r.avg_regret)) + '</td>'
                + '<td data-label="' + esc(t.predColHit1) + '">' + esc(fmtPct(r.within1_pct)) + '</td>'
                + '<td data-label="' + esc(t.predColHit2) + '">' + esc(fmtPct(r.within2_pct)) + '</td>'
                + '</tr>'
            ).join('');
        }

        function rowHtml(r) {
            const meta = (data.stations || {})[r.s] || {};
            const t = T();
            const good = Math.abs(r.err * 100) <= 1.0;
            const sugg = r.sugg ? ' <span class="pred-sugg" title="' + esc(t.predSuggestion) + '">★</span>' : '';
            return '<tr>'
                + '<td class="stack-primary" data-label="' + esc(t.predColTarget) + '">' + esc(fmtWindow(r.start, r.end)) + '<span class="station-sub">' + esc(fmtDate(r.start)) + '</span></td>'
                + '<td data-label="' + esc(t.predColStation) + '">' + esc(meta.name || r.s) + sugg + '<span class="station-sub">' + esc(meta.address || '') + '</span></td>'
                + '<td class="td-muted" data-label="' + esc(t.predColRunAt) + '">' + esc(fmtDateTime(r.run_at)) + '</td>'
                + '<td class="td-muted" data-label="' + esc(t.predColLead) + '">' + esc(fmtLead(r.lead)) + '</td>'
                + '<td data-label="' + esc(t.predColConf) + '">' + esc(confLabel(r.conf)) + '</td>'
                + '<td data-label="' + esc(t.predColPredicted) + '">' + esc(fmtEur(r.p)) + '</td>'
                + '<td data-label="' + esc(t.predColActual) + '">' + esc(fmtEur(r.a)) + '</td>'
                + '<td class="' + (good ? 'pred-err-good' : 'pred-err-bad') + '" data-label="' + esc(t.predColError) + '">' + esc(fmtSignedCt(r.err)) + '</td>'
                + '</tr>';
        }

        function renderMore() {
            const rows = data.rows || [];
            const slice = rows.slice(rowsRendered, rowsRendered + PAGE);
            tbody.insertAdjacentHTML('beforeend', slice.map(rowHtml).join(''));
            rowsRendered += slice.length;
            const remaining = rows.length - rowsRendered;
            if (remaining <= 0) { moreWrap.hidden = true; }
            else { moreWrap.hidden = false; moreBtn.textContent = T().showMore + ' (' + remaining + ')'; }
        }

        function renderTable() {
            rowsRendered = 0;
            tbody.innerHTML = '';
            const rows = data.rows || [];
            if (truncEl) truncEl.hidden = !data.truncated;
            if (rows.length === 0) {
                tbody.innerHTML = '<tr><td colspan="8" style="text-align:center;color:var(--muted);padding:2rem;font-family:var(--mono);font-size:.82rem">' + esc(T().predNoData) + '</td></tr>';
                moreWrap.hidden = true;
                return;
            }
            renderMore();
        }

        const mk = (tag, attrs, parent) => {
            const el = document.createElementNS(NS, tag);
            for (const k in attrs) el.setAttribute(k, String(attrs[k]));
            (parent || chartEl).appendChild(el);
            return el;
        };

        /* ── Crosshair tooltip (mirrors the dashboard chart) ────── */
        const tooltip = document.getElementById('price-tooltip');
        // Re-assigned by drawTimeline so hideTooltip also drops the crosshair.
        let hideCrosshair = () => {};

        // Placed clear of the plot (positionChartTooltip, shared script) so
        // the curves stay visible while the crosshair is up.
        function positionTooltip(clientX, clientY) {
            positionChartTooltip(chartEl, clientX, clientY);
        }

        function hideTooltip() {
            if (tooltip) tooltip.style.display = 'none';
            hideCrosshair();
        }

        // Lifting the finger on the chart keeps the crosshair readable; touching
        // anywhere else dismisses it.
        document.addEventListener('touchend', (e) => {
            if (e.target instanceof Element && e.target.closest('#pred-chart')) return;
            hideTooltip();
        });

        // `valueHtml` is inserted as-is; ttRow escapes plain-text values for it.
        const ttRowHtml = (color, name, valueHtml, valueClass) =>
            '<div class="tt-row">'
            + (color ? '<span class="legend-dot" style="background:' + color + '"></span>' : '')
            + '<span class="tt-name">' + esc(name) + '</span>'
            + '<span class="tt-val' + (valueClass ? ' ' + valueClass : '') + '"'
            + (color ? ' style="color:' + color + '"' : '') + '>' + valueHtml + '</span>'
            + '</div>';

        const ttRow = (color, name, value, valueClass) =>
            ttRowHtml(color, name, esc(value), valueClass);

        // Predicted / actual / error / sample-count block shared by both views.
        function ttBody(q, iso) {
            const t = T();
            const err = q.a - q.p;
            const good = Math.abs(err * 100) <= 1.0;
            return '<div class="tt-meta">' + esc(fmtDateTime(iso)) + '</div>'
                + ttRow(C_PRED, t.predLegendPredicted, fmtEur(q.p) + ' €')
                + ttRow(C_ACTUAL, t.predLegendActual, fmtEur(q.a) + ' €')
                + ttRow(null, t.predColError, fmtSignedCt(err), good ? 'pred-err-good' : 'pred-err-bad')
                + ttRow(null, t.predColCount, fmtInt(q.n));
        }

        // Transparent hit area over the plot area; hands pointer and touch
        // positions to the view's own hover handler.
        function attachHover(c, onPoint) {
            const overlay = mk('rect', {
                x: c.m.left, y: c.m.top, width: c.iW, height: c.iH,
                fill: 'transparent', style: 'cursor:crosshair',
            });
            // Hover via pointer events gated on a real mouse: a tap also fires
            // compatibility mousemove/mouseleave, which would leak the
            // crosshair past the long-press gate below.
            overlay.addEventListener('pointermove', (e) => {
                if (e.pointerType === 'mouse') onPoint(e.clientX, e.clientY);
            });
            overlay.addEventListener('pointerleave', (e) => {
                if (e.pointerType === 'mouse') hideTooltip();
            });
            // Touch: long-press only (attachLongPressCrosshair, shared script),
            // so swiping across the chart scrolls the page; the tooltip
            // auto-hides a few seconds after the finger lifts.
            attachLongPressCrosshair(overlay, onPoint, hideTooltip);
        }

        // Pointer position in viewBox units (the viewBox is 1:1 with CSS pixels).
        function svgPoint(clientX, clientY, c) {
            const rect = chartEl.getBoundingClientRect();
            const scale = rect.width ? c.W / rect.width : 1;
            return { x: (clientX - rect.left) * scale, y: (clientY - rect.top) * scale };
        }

        function renderChart() {
            // The old crosshair line is about to be discarded with the SVG
            // contents, so drop the tooltip that belonged to it first.
            hideTooltip();
            hideCrosshair = () => {};
            chartEl.innerHTML = '';
            legendEl.innerHTML = '';
            const series = (data && data.series) || [];
            if (series.length === 0) {
                chartEl.setAttribute('hidden', '');
                legendEl.hidden = true;
                if (emptyEl) emptyEl.hidden = false;
                return;
            }
            if (emptyEl) emptyEl.hidden = true;
            chartEl.removeAttribute('hidden');

            const light = document.documentElement.getAttribute('data-theme') === 'light';
            const bg    = light ? '#ffffff' : '#13151a';
            const grid  = light ? 'rgba(0,0,0,0.06)' : 'rgba(255,255,255,0.05)';
            const axis  = light ? 'rgba(0,0,0,0.15)' : 'rgba(255,255,255,0.12)';
            const tick  = light ? 'rgba(0,0,0,0.45)' : 'rgba(255,255,255,0.4)';
            const label = '#6b7280';

            const W = Math.max(280, Math.round(chartEl.getBoundingClientRect().width) || 960);
            const compact = W < 560;
            const H = compact ? 300 : 380;
            const m = compact ? { top: 18, right: 14, bottom: 50, left: 54 } : { top: 24, right: 24, bottom: 62, left: 68 };
            const iW = W - m.left - m.right;
            const iH = H - m.top - m.bottom;
            chartEl.setAttribute('viewBox', '0 0 ' + W + ' ' + H);
            mk('rect', { x: 0, y: 0, width: W, height: H, fill: bg });

            const font = "'DM Mono', monospace";
            if (view === 'scatter') drawScatter(series, { W, H, m, iW, iH, grid, axis, tick, label, font, light });
            else drawTimeline(series, { W, H, m, iW, iH, grid, axis, tick, label, font, light });
            drawLegend();
        }

        function drawTimeline(series, c) {
            const pts = series.map((d) => ({ x: Date.parse(d.t), p: d.p, a: d.a, n: d.n }));
            let minX = Infinity, maxX = -Infinity, minY = Infinity, maxY = -Infinity;
            for (const q of pts) {
                if (q.x < minX) minX = q.x;
                if (q.x > maxX) maxX = q.x;
                if (q.p < minY) minY = q.p; if (q.p > maxY) maxY = q.p;
                if (q.a < minY) minY = q.a; if (q.a > maxY) maxY = q.a;
            }
            if (minX === maxX) maxX += 3600000;
            const padY = Math.max((maxY - minY) * 0.15, 0.02);
            minY -= padY; maxY += padY;
            const px = (v) => c.m.left + ((v - minX) / (maxX - minX)) * c.iW;
            const py = (v) => c.m.top + c.iH - ((v - minY) / (maxY - minY)) * c.iH;

            for (let i = 0; i <= 5; i++) {
                const val = minY + ((maxY - minY) / 5) * i;
                const yp = py(val);
                mk('line', { x1: c.m.left, y1: yp, x2: c.W - c.m.right, y2: yp, stroke: c.grid, 'stroke-width': 1 });
                mk('text', { x: c.m.left - 8, y: yp + 4, 'text-anchor': 'end', 'font-size': 11, 'font-family': c.font, fill: c.label }).textContent = fmtDecimal(val, 3);
            }
            const tickCount = Math.min(c.W < 560 ? 4 : 7, pts.length);
            for (let i = 0; i < tickCount; i++) {
                const idx = tickCount === 1 ? 0 : Math.round((pts.length - 1) * (i / (tickCount - 1)));
                const xp = px(pts[idx].x);
                const lx = Math.min(Math.max(xp, 22), c.W - 22);
                const txt = mk('text', { x: lx, y: c.H - c.m.bottom + 14, 'text-anchor': 'middle', 'font-size': 10, 'font-family': c.font, fill: c.tick });
                const l1 = document.createElementNS(NS, 'tspan'); l1.setAttribute('x', lx); l1.setAttribute('dy', '0'); l1.textContent = fmtDate(series[idx].t); txt.appendChild(l1);
                const l2 = document.createElementNS(NS, 'tspan'); l2.setAttribute('x', lx); l2.setAttribute('dy', '14'); l2.textContent = fmtTime(series[idx].t); txt.appendChild(l2);
            }
            mk('line', { x1: c.m.left, y1: c.H - c.m.bottom, x2: c.W - c.m.right, y2: c.H - c.m.bottom, stroke: c.axis, 'stroke-width': 1 });
            mk('line', { x1: c.m.left, y1: c.m.top, x2: c.m.left, y2: c.H - c.m.bottom, stroke: c.axis, 'stroke-width': 1 });

            // Error band: the area between the predicted and actual lines.
            const band = [];
            for (let i = 0; i < pts.length; i++) band.push(px(pts[i].x).toFixed(1) + ',' + py(pts[i].p).toFixed(1));
            for (let i = pts.length - 1; i >= 0; i--) band.push(px(pts[i].x).toFixed(1) + ',' + py(pts[i].a).toFixed(1));
            mk('polygon', { points: band.join(' '), fill: C_BAND, stroke: 'none' });

            mk('polyline', { points: pts.map((q) => px(q.x).toFixed(1) + ',' + py(q.p).toFixed(1)).join(' '), fill: 'none', stroke: C_PRED, 'stroke-width': 2, 'stroke-linejoin': 'round', 'stroke-linecap': 'round' });
            mk('polyline', { points: pts.map((q) => px(q.x).toFixed(1) + ',' + py(q.a).toFixed(1)).join(' '), fill: 'none', stroke: C_ACTUAL, 'stroke-width': 2, 'stroke-linejoin': 'round', 'stroke-linecap': 'round' });

            if (!tooltip) return;

            // Crosshair: a thin vertical line follows the pointer/finger and the
            // tooltip lists the values of the target hour it snapped to.
            const crossLine = mk('line', {
                x1: 0, x2: 0, y1: c.m.top, y2: c.H - c.m.bottom,
                stroke: c.light ? 'rgba(0,0,0,0.45)' : 'rgba(255,255,255,0.5)',
                'stroke-width': 1, 'stroke-dasharray': '4 3',
                opacity: 0, 'pointer-events': 'none',
            });
            const markPred = mk('circle', { cx: 0, cy: 0, r: 3.5, fill: C_PRED, opacity: 0, 'pointer-events': 'none' });
            const markAct  = mk('circle', { cx: 0, cy: 0, r: 3.5, fill: C_ACTUAL, opacity: 0, 'pointer-events': 'none' });

            const showCrosshair = (clientX, clientY) => {
                let sx = svgPoint(clientX, clientY, c).x;
                sx = Math.max(c.m.left, Math.min(c.W - c.m.right, sx));
                const at = minX + ((sx - c.m.left) / c.iW) * (maxX - minX);

                // Snap to the nearest target hour so the line sits on data.
                let lo = 0, hi = pts.length - 1;
                while (lo < hi) {
                    const mid = (lo + hi) >> 1;
                    if (pts[mid].x < at) lo = mid + 1; else hi = mid;
                }
                let idx = lo;
                if (idx > 0 && at - pts[idx - 1].x < pts[idx].x - at) idx -= 1;

                const q = pts[idx];
                const xp = px(q.x);
                crossLine.setAttribute('x1', xp);
                crossLine.setAttribute('x2', xp);
                crossLine.setAttribute('opacity', 1);
                markPred.setAttribute('cx', xp); markPred.setAttribute('cy', py(q.p)); markPred.setAttribute('opacity', 1);
                markAct.setAttribute('cx', xp);  markAct.setAttribute('cy', py(q.a));  markAct.setAttribute('opacity', 1);

                tooltip.innerHTML = ttBody(q, series[idx].t);
                tooltip.style.display = 'block';
                positionTooltip(clientX, clientY);
            };

            hideCrosshair = () => {
                crossLine.setAttribute('opacity', 0);
                markPred.setAttribute('opacity', 0);
                markAct.setAttribute('opacity', 0);
            };

            attachHover(c, showCrosshair);
        }

        function drawScatter(series, c) {
            const pts = series.map((d) => ({ t: d.t, p: d.p, a: d.a, n: d.n }));
            let lo = Infinity, hi = -Infinity;
            for (const q of pts) { if (q.p < lo) lo = q.p; if (q.p > hi) hi = q.p; if (q.a < lo) lo = q.a; if (q.a > hi) hi = q.a; }
            const pad = Math.max((hi - lo) * 0.08, 0.02);
            lo -= pad; hi += pad;
            if (lo === hi) hi += 0.01;
            const px = (v) => c.m.left + ((v - lo) / (hi - lo)) * c.iW;
            const py = (v) => c.m.top + c.iH - ((v - lo) / (hi - lo)) * c.iH;

            for (let i = 0; i <= 5; i++) {
                const val = lo + ((hi - lo) / 5) * i;
                const yp = py(val), xp = px(val);
                mk('line', { x1: c.m.left, y1: yp, x2: c.W - c.m.right, y2: yp, stroke: c.grid, 'stroke-width': 1 });
                mk('line', { x1: xp, y1: c.m.top, x2: xp, y2: c.H - c.m.bottom, stroke: c.grid, 'stroke-width': 1 });
                mk('text', { x: c.m.left - 8, y: yp + 4, 'text-anchor': 'end', 'font-size': 11, 'font-family': c.font, fill: c.label }).textContent = fmtDecimal(val, 2);
                mk('text', { x: xp, y: c.H - c.m.bottom + 14, 'text-anchor': 'middle', 'font-size': 10, 'font-family': c.font, fill: c.tick }).textContent = fmtDecimal(val, 2);
            }
            mk('line', { x1: c.m.left, y1: c.H - c.m.bottom, x2: c.W - c.m.right, y2: c.H - c.m.bottom, stroke: c.axis, 'stroke-width': 1 });
            mk('line', { x1: c.m.left, y1: c.m.top, x2: c.m.left, y2: c.H - c.m.bottom, stroke: c.axis, 'stroke-width': 1 });
            // Perfect-accuracy diagonal (y = x).
            mk('line', { x1: px(lo), y1: py(lo), x2: px(hi), y2: py(hi), stroke: c.axis, 'stroke-width': 1.5, 'stroke-dasharray': '5 4' });
            for (const q of pts) mk('circle', { cx: px(q.p).toFixed(1), cy: py(q.a).toFixed(1), r: 3, fill: C_PRED, 'fill-opacity': 0.55, stroke: C_ACTUAL, 'stroke-width': 0.5, 'stroke-opacity': 0.4 });
            mk('text', { x: c.m.left + c.iW / 2, y: c.H - 6, 'text-anchor': 'middle', 'font-size': 11, 'font-family': c.font, fill: c.label }).textContent = T().predAxisPredicted;
            const yc = c.m.top + c.iH / 2;
            mk('text', { x: 14, y: yc, 'text-anchor': 'middle', 'font-size': 11, 'font-family': c.font, fill: c.label, transform: 'rotate(-90 14 ' + yc + ')' }).textContent = T().predAxisActual;

            if (!tooltip) return;

            // Per-point hover: ring the nearest target hour and drop dashed
            // guides to both axes so the pair can be read off the scales.
            const screen = pts.map((q) => ({ x: px(q.p), y: py(q.a) }));
            const guide = () => mk('line', {
                x1: 0, y1: 0, x2: 0, y2: 0,
                stroke: c.light ? 'rgba(0,0,0,0.45)' : 'rgba(255,255,255,0.5)',
                'stroke-width': 1, 'stroke-dasharray': '4 3',
                opacity: 0, 'pointer-events': 'none',
            });
            const guideX = guide();
            const guideY = guide();
            const ring = mk('circle', {
                cx: 0, cy: 0, r: 6, fill: 'none', stroke: C_PRED,
                'stroke-width': 1.5, opacity: 0, 'pointer-events': 'none',
            });

            // Points are sparse in places, so a reading is only shown when the
            // pointer is actually near one — roughly a fingertip's reach.
            const HIT_R2 = 30 * 30;

            const showPoint = (clientX, clientY) => {
                const at = svgPoint(clientX, clientY, c);
                let best = -1, bestDist = Infinity;
                for (let i = 0; i < screen.length; i++) {
                    const dx = screen[i].x - at.x, dy = screen[i].y - at.y;
                    const dist = dx * dx + dy * dy;
                    if (dist < bestDist) { bestDist = dist; best = i; }
                }
                if (best < 0 || bestDist > HIT_R2) { hideTooltip(); return; }

                const s = screen[best];
                guideX.setAttribute('x1', c.m.left); guideX.setAttribute('y1', s.y);
                guideX.setAttribute('x2', s.x);      guideX.setAttribute('y2', s.y);
                guideY.setAttribute('x1', s.x);      guideY.setAttribute('y1', s.y);
                guideY.setAttribute('x2', s.x);      guideY.setAttribute('y2', c.H - c.m.bottom);
                ring.setAttribute('cx', s.x);        ring.setAttribute('cy', s.y);
                guideX.setAttribute('opacity', 1);
                guideY.setAttribute('opacity', 1);
                ring.setAttribute('opacity', 1);

                tooltip.innerHTML = ttBody(pts[best], pts[best].t);
                tooltip.style.display = 'block';
                positionTooltip(clientX, clientY);
            };

            hideCrosshair = () => {
                guideX.setAttribute('opacity', 0);
                guideY.setAttribute('opacity', 0);
                ring.setAttribute('opacity', 0);
            };

            attachHover(c, showPoint);
        }

        function drawLegend() {
            legendEl.hidden = false;
            legendEl.innerHTML = '';
            const t = T();
            const add = (swatch, text) => { const it = document.createElement('div'); it.className = 'legend-item'; it.innerHTML = swatch + '<span>' + esc(text) + '</span>'; legendEl.appendChild(it); };
            if (view === 'scatter') {
                add('<span class="legend-dot" style="background:' + C_PRED + '"></span>', t.predLegendPoint);
                add('<span class="pred-legend-diag"></span>', t.predLegendDiagonal);
            } else {
                add('<span class="pred-legend-line" style="background:' + C_PRED + '"></span>', t.predLegendPredicted);
                add('<span class="pred-legend-line" style="background:' + C_ACTUAL + '"></span>', t.predLegendActual);
                add('<span class="pred-legend-band"></span>', t.predLegendBand);
            }
        }

        [cfg.fuel, cfg.range, cfg.conf].forEach((el) => { if (el) el.addEventListener('change', load); });
        if (moreBtn) moreBtn.addEventListener('click', renderMore);
        if (viewTogl) viewTogl.querySelectorAll('[data-view]').forEach((btn) => btn.addEventListener('click', () => {
            view = btn.dataset.view;
            viewTogl.querySelectorAll('[data-view]').forEach((b) => b.classList.toggle('active', b === btn));
            if (data) renderChart();
        }));

        window.onLangChange = () => { if (data) { renderStats(); renderConf(); renderLead(); renderHour(); renderDecisions(); renderChart(); renderTable(); } };
        window.onThemeChange = () => { if (data) renderChart(); };

        if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', load);
        else load();
    })();
    </script>
    <?php
    renderPageEnd();
}

// renderAdminStatsPage shows what the scheduled commands actually did: one row
// per recorded run of update/suggest/check/notify, with the counters each of
// them already computes. Like the accuracy page it renders an empty shell and
// fills it from ?action=command_stats, so the page paints before the
// aggregates come back.
function renderAdminStatsPage(PDO $pdo, string $driver, array $user): never
{
    $hasTable = gasolineTableExists($pdo, $driver, 'command_runs');

    $commandLabels = [
        'all'     => 'All commands',
        'update'  => 'update',
        'suggest' => 'suggest',
        'check'   => 'check',
        'notify'  => 'notify',
    ];

    renderPageStart('Statistics', $user, 'admin_stats');
    ?>
    <style>
        .cs-layout { max-width: 1180px; }
        .cs-filters { display: grid; grid-template-columns: repeat(auto-fit, minmax(150px, 1fr)); gap: 0 1.1rem; }
        .cs-filters .field { margin-bottom: 0.6rem; }
        #cs-chart { width: 100%; display: block; height: auto; -webkit-user-select: none; user-select: none; -webkit-touch-callout: none; }
        .cs-ok      { color: var(--e10); }
        .cs-partial { color: var(--amber); }
        .cs-error   { color: var(--red); }
        .cs-running { color: var(--muted); }
        /* The metric list inside a run row: name=value pairs, wrapping. */
        .cs-metrics { font-family: var(--mono); font-size: 0.72rem; color: var(--muted); }
        .cs-err { font-family: var(--mono); font-size: 0.72rem; color: var(--red); word-break: break-word; }
        .cs-legend-swatch { width: 16px; height: 10px; border-radius: 2px; display: inline-block; }
        .cs-legend-line { width: 16px; height: 3px; border-radius: 2px; display: inline-block; }
        .stat-value.cs-small { font-size: 1.15rem; }
    </style>
    <div id="price-tooltip" role="tooltip" aria-hidden="true"></div>
    <div class="settings-layout wide cs-layout">
        <?php renderFlash(); ?>

        <div class="settings-card">
            <h2 data-i18n="statsTitle">Command statistics</h2>
            <p class="auth-note" data-i18n="statsHint">Every run of the scheduled commands, with what it did and how long it took. A run is recorded once its database is open, so a failure before that — a bad flag, an unreachable server — leaves no row. `notify --dry-run` is not recorded: it delivers nothing.</p>
            <?php if (!$hasTable) { ?>
            <p class="auth-note" data-i18n="statsNoTable">No runs have been recorded yet. Run `gasoline migrate` on the server to create the tables, then wait for the next scheduled command.</p>
            <?php } else { ?>
            <div class="cs-filters">
                <div class="field">
                    <label for="cs-command" data-i18n="statsCommand">Command</label>
                    <select id="cs-command">
                        <?php foreach ($commandLabels as $value => $label) { ?>
                        <option value="<?= h($value) ?>"<?= $value === 'all' ? ' data-i18n="statsAllCommands"' : '' ?>><?= h($label) ?></option>
                        <?php } ?>
                    </select>
                </div>
                <div class="field">
                    <label for="cs-range" data-i18n="statsRange">Range</label>
                    <select id="cs-range">
                        <option value="24h" data-i18n="range24h">24h</option>
                        <option value="7d" selected data-i18n="range7d">7d</option>
                        <option value="30d" data-i18n="range30d">30d</option>
                    </select>
                </div>
            </div>
            <?php } ?>
        </div>

        <?php if ($hasTable) { ?>
        <div class="stats" aria-live="polite">
            <div class="stat"><div class="stat-label" data-i18n="statsTileRuns">Runs</div><div class="stat-value skeleton" id="cs-runs" aria-busy="true">&nbsp;</div></div>
            <div class="stat"><div class="stat-label" data-i18n="statsTileSuccess">Success rate</div><div class="stat-value skeleton cs-small" id="cs-success" aria-busy="true">&nbsp;</div></div>
            <div class="stat"><div class="stat-label" data-i18n="statsTilePartial">Partial</div><div class="stat-value skeleton" id="cs-partial" aria-busy="true">&nbsp;</div></div>
            <div class="stat"><div class="stat-label" data-i18n="statsTileFailed">Failed</div><div class="stat-value skeleton" id="cs-failed" aria-busy="true">&nbsp;</div></div>
            <div class="stat"><div class="stat-label" data-i18n="statsTileInterrupted">Interrupted</div><div class="stat-value skeleton" id="cs-interrupted" aria-busy="true">&nbsp;</div></div>
            <div class="stat"><div class="stat-label" data-i18n="statsTileMedian">Median duration</div><div class="stat-value skeleton cs-small" id="cs-p50" aria-busy="true">&nbsp;</div></div>
            <div class="stat"><div class="stat-label" data-i18n="statsTileP95">p95 duration</div><div class="stat-value skeleton cs-small" id="cs-p95" aria-busy="true">&nbsp;</div></div>
            <div class="stat"><div class="stat-label" data-i18n="statsTileLastRun">Last run</div><div class="stat-value skeleton cs-small" id="cs-last" aria-busy="true">&nbsp;</div></div>
        </div>
        <p class="auth-note" data-i18n="statsInterruptedHint">"Interrupted" counts runs that recorded a start and never a finish — killed, out of memory, or still going. Nothing clears them later, so a run that takes longer than six hours stays counted.</p>

        <div class="chart-card">
            <div class="chart-header">
                <span class="chart-title" data-i18n="statsChartTitle">Runs over time</span>
            </div>
            <div class="chart-body">
                <div class="chart-loading" id="cs-chart-loading" role="status"><span class="spinner" aria-hidden="true"></span><span class="sr-only" data-i18n="loading">Loading…</span></div>
                <svg id="cs-chart" viewBox="0 0 960 340" role="img" hidden></svg>
            </div>
            <div class="chart-legend" id="cs-legend" hidden></div>
            <div class="chart-empty" id="cs-chart-empty" data-i18n="statsNoData" role="status" hidden>No recorded runs match the current filters.</div>
        </div>

        <div class="settings-card">
            <h2 data-i18n="statsByCommand">By command</h2>
            <div class="table-scroll">
                <table class="stack-table">
                    <thead>
                        <tr>
                            <th data-i18n="statsColCommand">Command</th>
                            <th data-i18n="statsColRuns">Runs</th>
                            <th data-i18n="statsColOk">OK</th>
                            <th data-i18n="statsColPartial">Partial</th>
                            <th data-i18n="statsColError">Failed</th>
                            <th data-i18n="statsColAvg">Avg duration</th>
                            <th data-i18n="statsColMax">Max duration</th>
                            <th data-i18n="statsColLast">Last run</th>
                        </tr>
                    </thead>
                    <tbody id="cs-cmd-tbody"><tr><td colspan="8" class="table-loading" aria-busy="true"><span class="spinner" aria-hidden="true"></span></td></tr></tbody>
                </table>
            </div>
        </div>

        <div class="settings-card">
            <h2 data-i18n="statsWork">Work done</h2>
            <p class="auth-note" data-i18n="statsWorkHint">The counters the commands report, summed over the filtered runs. Per run averages only over the runs that reported the metric, so suggest’s persist counters are not diluted by runs without --persist.</p>
            <div class="table-scroll">
                <table class="stack-table">
                    <thead>
                        <tr>
                            <th data-i18n="statsColCommand">Command</th>
                            <th data-i18n="statsColMetric">Metric</th>
                            <th data-i18n="statsColTotal">Total</th>
                            <th data-i18n="statsColPerRun">Per run</th>
                        </tr>
                    </thead>
                    <tbody id="cs-metric-tbody"><tr><td colspan="4" class="table-loading" aria-busy="true"><span class="spinner" aria-hidden="true"></span></td></tr></tbody>
                </table>
            </div>
        </div>

        <div class="settings-card">
            <h2 data-i18n="statsRecent">Recent runs</h2>
            <div class="pred-note" id="cs-truncated" data-i18n="statsTruncated" hidden>Showing the most recent 200 runs; the statistics above cover the full filtered set.</div>
            <div class="table-scroll">
                <table class="stack-table">
                    <thead>
                        <tr>
                            <th data-i18n="statsColStarted">Started</th>
                            <th data-i18n="statsColCommand">Command</th>
                            <th data-i18n="statsColStatus">Status</th>
                            <th data-i18n="statsColDuration">Duration</th>
                            <th data-i18n="statsColHost">Host</th>
                            <th data-i18n="statsColDetail">Detail</th>
                        </tr>
                    </thead>
                    <tbody id="cs-run-tbody"><tr><td colspan="6" class="table-loading" aria-busy="true"><span class="spinner" aria-hidden="true"></span></td></tr></tbody>
                </table>
            </div>
            <div class="table-more" id="cs-more" hidden>
                <button type="button" class="btn-reset" id="cs-more-btn"></button>
            </div>
        </div>
        <?php } ?>
    </div>

    <?php if ($hasTable) { ?>
    <script>
    (function () {
        const cfg = {
            command: document.getElementById('cs-command'),
            range:   document.getElementById('cs-range'),
        };
        const loadingEl   = document.getElementById('cs-chart-loading');
        const chartEl     = document.getElementById('cs-chart');
        const legendEl    = document.getElementById('cs-legend');
        const emptyEl     = document.getElementById('cs-chart-empty');
        const cmdTbody    = document.getElementById('cs-cmd-tbody');
        const metricTbody = document.getElementById('cs-metric-tbody');
        const runTbody    = document.getElementById('cs-run-tbody');
        const moreWrap    = document.getElementById('cs-more');
        const moreBtn     = document.getElementById('cs-more-btn');
        const truncEl     = document.getElementById('cs-truncated');
        const statIds     = ['cs-runs','cs-success','cs-partial','cs-failed','cs-interrupted','cs-p50','cs-p95','cs-last'];

        const NS = 'http://www.w3.org/2000/svg';
        // Status colours reused from the theme tokens the rest of the UI uses:
        // green for ok, amber for degraded, red for failed, grey for unfinished.
        const C_OK = '#34d399', C_PARTIAL = '#f5a623', C_ERROR = '#ef4444', C_RUNNING = '#6b7280';
        const C_DURATION = '#60a5fa';

        let data = null;
        let rowsRendered = 0;
        const PAGE = 100;

        const T   = () => translations[currentLang];
        const loc = () => currentLang === 'de' ? 'de-DE' : 'en-GB';
        const tz  = () => currentLang === 'de' ? 'Europe/Berlin' : 'UTC';

        function esc(s) { return String(s).replace(/[&<>"']/g, (c) => ({ '&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;' }[c])); }
        function fmtInt(v) { return Number(v || 0).toLocaleString(loc()); }
        function fmtPct(v) { const s = fmtDecimal(v, 1); return s === null ? '—' : s + '%'; }
        // Durations span three orders of magnitude here — a notify run that
        // sends nothing is milliseconds, a suggest --persist sweep is minutes —
        // so the unit follows the value instead of being fixed.
        function fmtDuration(ms) {
            if (ms === null || ms === undefined) return '—';
            const v = Number(ms);
            if (v < 1000) return fmtInt(Math.round(v)) + ' ms';
            if (v < 60000) return (fmtDecimal(v / 1000, 1) ?? '—') + ' s';
            const mins = Math.floor(v / 60000);
            const secs = Math.round((v % 60000) / 1000);
            return mins + ' min ' + String(secs).padStart(2, '0') + ' s';
        }
        function fmtNumber(v) {
            // Counters are whole numbers and reach seven digits over a month —
            // grouped, or nobody can read them. Only a per-run average needs
            // decimals, and never more than the two the server rounded to.
            if (v === null || v === undefined) return '—';
            const n = Number(v);
            return n.toLocaleString(loc(), { maximumFractionDigits: Number.isInteger(n) ? 0 : 2 });
        }
        function fmtDateTime(iso) { if (!iso) return '—'; return new Date(iso).toLocaleString(loc(), { timeZone: tz(), year: 'numeric', month: 'short', day: '2-digit', hour: '2-digit', minute: '2-digit' }); }
        function fmtBucket(t, daily) {
            const d = new Date(daily ? t + 'T00:00:00Z' : t + ':00:00Z');
            return daily
                ? d.toLocaleDateString(loc(), { timeZone: tz(), month: 'short', day: '2-digit' })
                : d.toLocaleTimeString(loc(), { timeZone: tz(), hour: '2-digit', minute: '2-digit' });
        }
        // How long ago, so "is the timer still firing?" is answerable at a glance.
        function fmtAgo(iso) {
            if (!iso) return '—';
            const t = T();
            const mins = Math.max(0, Math.round((Date.now() - Date.parse(iso)) / 60000));
            if (mins < 1) return t.statsJustNow;
            if (mins < 60) return mins + ' ' + t.statsMinutesAgo;
            const hours = Math.floor(mins / 60);
            if (hours < 48) return hours + ' ' + t.statsHoursAgo;
            return Math.floor(hours / 24) + ' ' + t.statsDaysAgo;
        }
        function statusLabel(s) { return T()['statsStatus_' + s] || s; }
        function statusClass(s) { return 'cs-' + s; }

        function setStat(id, val) { const el = document.getElementById(id); if (!el) return; el.textContent = val; el.classList.remove('skeleton'); el.removeAttribute('aria-busy'); }

        function buildUrl() {
            const u = new URL(location.origin + location.pathname);
            u.searchParams.set('action', 'command_stats');
            u.searchParams.set('command', cfg.command.value);
            u.searchParams.set('range', cfg.range.value);
            return u.toString();
        }

        function loadingRow(span) {
            return '<tr><td colspan="' + span + '" class="table-loading" aria-busy="true"><span class="spinner" aria-hidden="true"></span></td></tr>';
        }
        function emptyRow(span) {
            return '<tr><td colspan="' + span + '" style="text-align:center;color:var(--muted);padding:1rem;font-family:var(--mono);font-size:.8rem">'
                + esc(T().statsNoData) + '</td></tr>';
        }

        function resetLoading() {
            if (loadingEl) loadingEl.hidden = false;
            if (chartEl) chartEl.setAttribute('hidden', '');
            if (legendEl) legendEl.hidden = true;
            if (emptyEl) { emptyEl.hidden = true; emptyEl.dataset.i18n = 'statsNoData'; }
            statIds.forEach((id) => { const el = document.getElementById(id); if (el) { el.textContent = ' '; el.classList.add('skeleton'); el.setAttribute('aria-busy', 'true'); } });
            if (cmdTbody) cmdTbody.innerHTML = loadingRow(8);
            if (metricTbody) metricTbody.innerHTML = loadingRow(4);
            if (runTbody) runTbody.innerHTML = loadingRow(6);
            if (moreWrap) moreWrap.hidden = true;
            if (truncEl) truncEl.hidden = true;
        }

        function showError(err) {
            if (loadingEl) loadingEl.hidden = true;
            const t = T();
            const key = (err && err.key && t[err.key]) ? err.key : 'loadError';
            const msg = (err && err.key && t[err.key]) ? t[err.key] : t.loadError;
            if (emptyEl) { emptyEl.hidden = false; emptyEl.dataset.i18n = key; emptyEl.textContent = msg; }
            if (chartEl) chartEl.setAttribute('hidden', '');
            if (legendEl) legendEl.hidden = true;
            statIds.forEach((id) => setStat(id, '—'));
            if (runTbody) runTbody.innerHTML = '<tr><td colspan="6" role="alert" style="text-align:center;color:var(--red);padding:2rem;font-family:var(--mono);font-size:.82rem" data-i18n="' + key + '">' + esc(msg) + '</td></tr>';
            if (cmdTbody) cmdTbody.innerHTML = '';
            if (metricTbody) metricTbody.innerHTML = '';
            if (moreWrap) moreWrap.hidden = true;
        }

        async function load() {
            resetLoading();
            try {
                const res = await fetch(buildUrl(), { headers: { Accept: 'application/json' } });
                if (res.status === 401) { location.href = '?page=login'; return; }
                if (res.status === 403) { location.href = '?'; return; }
                if (!res.ok) throw new Error('HTTP ' + res.status);
                const payload = await res.json();
                if (payload.errors && payload.errors.length) { showError(payload.errors[0]); return; }
                data = payload;
                render();
            } catch (e) {
                showError();
            }
        }

        function render() {
            if (!data) return;
            renderStats();
            renderByCommand();
            renderMetrics();
            renderChart();
            renderRuns();
        }

        function renderStats() {
            if (loadingEl) loadingEl.hidden = true;
            const s = data.summary;
            if (!s || !s.runs) { statIds.forEach((id) => setStat(id, '—')); setStat('cs-runs', '0'); return; }
            setStat('cs-runs', fmtInt(s.runs));
            setStat('cs-success', fmtPct(s.success_pct));
            setStat('cs-partial', fmtInt(s.partial));
            setStat('cs-failed', fmtInt(s.error));
            setStat('cs-interrupted', fmtInt(s.interrupted));
            setStat('cs-p50', fmtDuration(s.p50_ms));
            setStat('cs-p95', fmtDuration(s.p95_ms));
            setStat('cs-last', fmtAgo(s.last_started_at));
        }

        function renderByCommand() {
            if (!cmdTbody) return;
            const rows = (data && data.by_command) || [];
            const t = T();
            if (rows.length === 0) { cmdTbody.innerHTML = emptyRow(8); return; }
            cmdTbody.innerHTML = rows.map((r) =>
                '<tr>'
                + '<td class="stack-primary" data-label="' + esc(t.statsColCommand) + '" data-i18n-label="statsColCommand">' + esc(r.command) + '</td>'
                + '<td data-label="' + esc(t.statsColRuns) + '" data-i18n-label="statsColRuns">' + fmtInt(r.runs) + '</td>'
                + '<td class="cs-ok" data-label="' + esc(t.statsColOk) + '" data-i18n-label="statsColOk">' + fmtInt(r.ok) + '</td>'
                + '<td class="cs-partial" data-label="' + esc(t.statsColPartial) + '" data-i18n-label="statsColPartial">' + fmtInt(r.partial) + '</td>'
                + '<td class="cs-error" data-label="' + esc(t.statsColError) + '" data-i18n-label="statsColError">' + fmtInt(r.error) + '</td>'
                + '<td data-label="' + esc(t.statsColAvg) + '" data-i18n-label="statsColAvg">' + esc(fmtDuration(r.avg_ms)) + '</td>'
                + '<td data-label="' + esc(t.statsColMax) + '" data-i18n-label="statsColMax">' + esc(fmtDuration(r.max_ms)) + '</td>'
                + '<td class="td-muted" data-label="' + esc(t.statsColLast) + '" data-i18n-label="statsColLast">' + esc(fmtAgo(r.last_started_at)) + '</td>'
                + '</tr>'
            ).join('');
        }

        function renderMetrics() {
            if (!metricTbody) return;
            const rows = (data && data.metric_totals) || [];
            const t = T();
            if (rows.length === 0) { metricTbody.innerHTML = emptyRow(4); return; }
            metricTbody.innerHTML = rows.map((r) =>
                '<tr>'
                + '<td class="stack-primary" data-label="' + esc(t.statsColCommand) + '" data-i18n-label="statsColCommand">' + esc(r.command) + '</td>'
                + '<td data-label="' + esc(t.statsColMetric) + '" data-i18n-label="statsColMetric"><span class="cs-metrics">' + esc(r.name) + '</span></td>'
                + '<td data-label="' + esc(t.statsColTotal) + '" data-i18n-label="statsColTotal">' + esc(fmtNumber(r.total)) + '</td>'
                + '<td class="td-muted" data-label="' + esc(t.statsColPerRun) + '" data-i18n-label="statsColPerRun">' + esc(fmtNumber(r.per_run)) + '</td>'
                + '</tr>'
            ).join('');
        }

        function metricsHtml(metrics) {
            const names = Object.keys(metrics || {}).sort();
            if (names.length === 0) return '';
            return '<span class="cs-metrics">' + names.map((n) => esc(n) + '=' + esc(fmtNumber(metrics[n]))).join('  ') + '</span>';
        }

        function runRowHtml(r) {
            const t = T();
            const detail = r.error
                ? '<span class="cs-err">' + esc(r.error) + '</span>'
                : metricsHtml(r.metrics);
            return '<tr>'
                + '<td class="stack-primary" data-label="' + esc(t.statsColStarted) + '" data-i18n-label="statsColStarted">' + esc(fmtDateTime(r.started_at)) + '</td>'
                + '<td data-label="' + esc(t.statsColCommand) + '" data-i18n-label="statsColCommand">' + esc(r.command) + '</td>'
                + '<td class="' + statusClass(r.status) + '" data-label="' + esc(t.statsColStatus) + '" data-i18n-label="statsColStatus">' + esc(statusLabel(r.status)) + '</td>'
                + '<td data-label="' + esc(t.statsColDuration) + '" data-i18n-label="statsColDuration">' + esc(fmtDuration(r.duration_ms)) + '</td>'
                + '<td class="td-muted" data-label="' + esc(t.statsColHost) + '" data-i18n-label="statsColHost">' + esc(r.host || '—') + '</td>'
                + '<td data-label="' + esc(t.statsColDetail) + '" data-i18n-label="statsColDetail">' + detail + '</td>'
                + '</tr>';
        }

        function renderMore() {
            const rows = data.rows || [];
            const slice = rows.slice(rowsRendered, rowsRendered + PAGE);
            runTbody.insertAdjacentHTML('beforeend', slice.map(runRowHtml).join(''));
            rowsRendered += slice.length;
            const remaining = rows.length - rowsRendered;
            if (remaining <= 0) { moreWrap.hidden = true; }
            else { moreWrap.hidden = false; moreBtn.textContent = T().showMore + ' (' + remaining + ')'; }
        }

        function renderRuns() {
            rowsRendered = 0;
            runTbody.innerHTML = '';
            const rows = data.rows || [];
            if (truncEl) truncEl.hidden = !data.truncated;
            if (rows.length === 0) { runTbody.innerHTML = emptyRow(6); moreWrap.hidden = true; return; }
            renderMore();
        }

        /* ── Chart ─────────────────────────────────────────────── */

        const mk = (tag, attrs, parent) => {
            const el = document.createElementNS(NS, tag);
            for (const k in attrs) el.setAttribute(k, String(attrs[k]));
            (parent || chartEl).appendChild(el);
            return el;
        };

        const tooltip = document.getElementById('price-tooltip');
        let hideCrosshair = () => {};

        function hideTooltip() {
            if (tooltip) tooltip.style.display = 'none';
            hideCrosshair();
        }

        document.addEventListener('touchend', (e) => {
            if (e.target instanceof Element && e.target.closest('#cs-chart')) return;
            hideTooltip();
        });

        const ttRow = (color, name, value) =>
            '<div class="tt-row">'
            + (color ? '<span class="legend-dot" style="background:' + color + '"></span>' : '')
            + '<span class="tt-name">' + esc(name) + '</span>'
            + '<span class="tt-val"' + (color ? ' style="color:' + color + '"' : '') + '>' + esc(value) + '</span>'
            + '</div>';

        function renderChart() {
            hideTooltip();
            hideCrosshair = () => {};
            chartEl.innerHTML = '';
            legendEl.innerHTML = '';
            const series = (data && data.series) || [];
            if (series.length === 0) {
                chartEl.setAttribute('hidden', '');
                legendEl.hidden = true;
                if (emptyEl) emptyEl.hidden = false;
                return;
            }
            if (emptyEl) emptyEl.hidden = true;
            chartEl.removeAttribute('hidden');

            const light = document.documentElement.getAttribute('data-theme') === 'light';
            const bg    = light ? '#ffffff' : '#13151a';
            const grid  = light ? 'rgba(0,0,0,0.06)' : 'rgba(255,255,255,0.05)';
            const axis  = light ? 'rgba(0,0,0,0.15)' : 'rgba(255,255,255,0.12)';
            const tick  = light ? 'rgba(0,0,0,0.45)' : 'rgba(255,255,255,0.4)';
            const label = '#6b7280';
            const font  = "'DM Mono', monospace";

            const W = Math.max(280, Math.round(chartEl.getBoundingClientRect().width) || 960);
            const compact = W < 560;
            const H = compact ? 280 : 340;
            const m = compact ? { top: 18, right: 54, bottom: 46, left: 42 } : { top: 24, right: 62, bottom: 54, left: 56 };
            const iW = W - m.left - m.right;
            const iH = H - m.top - m.bottom;
            chartEl.setAttribute('viewBox', '0 0 ' + W + ' ' + H);
            mk('rect', { x: 0, y: 0, width: W, height: H, fill: bg });

            const daily = !!data.daily_buckets;
            const maxRuns = Math.max(1, ...series.map((d) => d.ok + d.partial + d.error + d.running));
            // A bucket with no finished run carries a null duration, not a
            // zero. Scale off the measured ones, and drop the whole duration
            // axis when nothing in the window finished — otherwise the
            // Math.max floor below invents an axis reading "0 ms … 1 ms".
            const durVals = series.map((d) => d.avg_ms).filter((v) => v !== null && v !== undefined);
            const hasDur  = durVals.length > 0;
            const maxDur  = Math.max(1, ...durVals);

            const py     = (v) => m.top + iH - (v / maxRuns) * iH;
            const pyDur  = (v) => m.top + iH - (v / maxDur) * iH;
            const slot   = iW / series.length;
            const barW   = Math.max(1, Math.min(28, slot * 0.7));

            // Left axis: run counts. Whole numbers only — half a run is not a
            // thing — so the gridline count follows the maximum when it is small.
            const steps = Math.min(5, maxRuns);
            for (let i = 0; i <= steps; i++) {
                const val = Math.round((maxRuns / steps) * i);
                const yp = py(val);
                mk('line', { x1: m.left, y1: yp, x2: W - m.right, y2: yp, stroke: grid, 'stroke-width': 1 });
                mk('text', { x: m.left - 8, y: yp + 4, 'text-anchor': 'end', 'font-size': 11, 'font-family': font, fill: label }).textContent = fmtInt(val);
            }
            // Right axis: the average duration line's own scale.
            if (hasDur) {
                for (let i = 0; i <= steps; i++) {
                    const val = (maxDur / steps) * i;
                    mk('text', { x: W - m.right + 8, y: pyDur(val) + 4, 'text-anchor': 'start', 'font-size': 10, 'font-family': font, fill: C_DURATION })
                        .textContent = fmtDuration(val);
                }
            }

            series.forEach((d, i) => {
                const x = m.left + slot * i + (slot - barW) / 2;
                let bottom = m.top + iH;
                // Stacked worst-last, so a red cap is the eye-catching part.
                [['ok', C_OK], ['partial', C_PARTIAL], ['running', C_RUNNING], ['error', C_ERROR]].forEach(([key, color]) => {
                    const n = d[key] || 0;
                    if (n <= 0) return;
                    const hgt = (n / maxRuns) * iH;
                    mk('rect', { x: x, y: bottom - hgt, width: barW, height: hgt, fill: color, rx: 1 });
                    bottom -= hgt;
                });
            });

            // Break the line where nothing finished rather than drawing it
            // through: a continuous line across a gap claims a duration that
            // was never measured, and drawing the gap at zero would read as an
            // instant run when it is really a hung or absent one.
            const durSegments = [];
            let durSegment = [];
            series.forEach((d, i) => {
                if (d.avg_ms === null || d.avg_ms === undefined) {
                    if (durSegment.length) durSegments.push(durSegment);
                    durSegment = [];
                    return;
                }
                durSegment.push({ x: m.left + slot * i + slot / 2, y: pyDur(d.avg_ms) });
            });
            if (durSegment.length) durSegments.push(durSegment);
            durSegments.forEach((seg) => {
                if (seg.length === 1) {
                    // A lone measured bucket between two gaps: a one-point
                    // polyline draws nothing, so mark the point instead.
                    mk('circle', { cx: seg[0].x, cy: seg[0].y, r: 2.5, fill: C_DURATION });
                    return;
                }
                mk('polyline', {
                    points: seg.map((p) => p.x.toFixed(1) + ',' + p.y.toFixed(1)).join(' '),
                    fill: 'none', stroke: C_DURATION, 'stroke-width': 2,
                    'stroke-linejoin': 'round', 'stroke-linecap': 'round',
                });
            });

            mk('line', { x1: m.left, y1: m.top + iH, x2: W - m.right, y2: m.top + iH, stroke: axis, 'stroke-width': 1 });
            mk('line', { x1: m.left, y1: m.top, x2: m.left, y2: m.top + iH, stroke: axis, 'stroke-width': 1 });

            const tickCount = Math.min(compact ? 4 : 8, series.length);
            for (let i = 0; i < tickCount; i++) {
                const idx = tickCount === 1 ? 0 : Math.round((series.length - 1) * (i / (tickCount - 1)));
                const xp = Math.min(Math.max(m.left + slot * idx + slot / 2, 22), W - 22);
                mk('text', { x: xp, y: H - m.bottom + 16, 'text-anchor': 'middle', 'font-size': 10, 'font-family': font, fill: tick })
                    .textContent = fmtBucket(series[idx].t, daily);
            }

            drawLegend(hasDur);
            if (!tooltip) return;

            const crossLine = mk('line', {
                x1: 0, x2: 0, y1: m.top, y2: m.top + iH,
                stroke: light ? 'rgba(0,0,0,0.45)' : 'rgba(255,255,255,0.5)',
                'stroke-width': 1, 'stroke-dasharray': '4 3',
                opacity: 0, 'pointer-events': 'none',
            });

            const overlay = mk('rect', {
                x: m.left, y: m.top, width: iW, height: iH,
                fill: 'transparent', style: 'cursor:crosshair',
            });

            const showCrosshair = (clientX, clientY) => {
                const rect = chartEl.getBoundingClientRect();
                const scale = rect.width ? W / rect.width : 1;
                const sx = Math.max(m.left, Math.min(W - m.right, (clientX - rect.left) * scale));
                const idx = Math.max(0, Math.min(series.length - 1, Math.floor((sx - m.left) / slot)));
                const d = series[idx];
                const xp = m.left + slot * idx + slot / 2;
                crossLine.setAttribute('x1', xp);
                crossLine.setAttribute('x2', xp);
                crossLine.setAttribute('opacity', 1);

                const t = T();
                let html = '<div class="tt-meta">' + esc(fmtBucket(d.t, daily)) + '</div>';
                if (d.ok)      html += ttRow(C_OK, t.statsStatus_ok, fmtInt(d.ok));
                if (d.partial) html += ttRow(C_PARTIAL, t.statsStatus_partial, fmtInt(d.partial));
                if (d.error)   html += ttRow(C_ERROR, t.statsStatus_error, fmtInt(d.error));
                if (d.running) html += ttRow(C_RUNNING, t.statsStatus_running, fmtInt(d.running));
                html += ttRow(C_DURATION, t.statsLegendDuration, fmtDuration(d.avg_ms));
                tooltip.innerHTML = html;
                tooltip.style.display = 'block';
                positionChartTooltip(chartEl, clientX, clientY);
            };

            hideCrosshair = () => { crossLine.setAttribute('opacity', 0); };

            overlay.addEventListener('pointermove', (e) => { if (e.pointerType === 'mouse') showCrosshair(e.clientX, e.clientY); });
            overlay.addEventListener('pointerleave', (e) => { if (e.pointerType === 'mouse') hideTooltip(); });
            attachLongPressCrosshair(overlay, showCrosshair, hideTooltip);
        }

        function drawLegend(hasDur) {
            legendEl.hidden = false;
            legendEl.innerHTML = '';
            const t = T();
            const add = (swatch, text) => { const it = document.createElement('div'); it.className = 'legend-item'; it.innerHTML = swatch + '<span>' + esc(text) + '</span>'; legendEl.appendChild(it); };
            const box = (color) => '<span class="cs-legend-swatch" style="background:' + color + '"></span>';
            add(box(C_OK), t.statsStatus_ok);
            add(box(C_PARTIAL), t.statsStatus_partial);
            add(box(C_ERROR), t.statsStatus_error);
            add(box(C_RUNNING), t.statsStatus_running);
            // Nothing finished in this window, so there is no line to explain.
            if (hasDur) {
                add('<span class="cs-legend-line" style="background:' + C_DURATION + '"></span>', t.statsLegendDuration);
            }
        }

        [cfg.command, cfg.range].forEach((el) => { if (el) el.addEventListener('change', load); });
        if (moreBtn) moreBtn.addEventListener('click', renderMore);

        window.onLangChange = () => { if (data) render(); };
        window.onThemeChange = () => { if (data) renderChart(); };

        if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', load);
        else load();
    })();
    </script>
    <?php } ?>
    <?php
    renderPageEnd();
}

function renderAdminSettingsPage(PDO $pdo, string $driver, array $user): never
{
    $settings = settingsAll($pdo);
    $targets = $pdo->query('SELECT id, city, radius_km FROM update_targets ORDER BY id ASC')->fetchAll();
    $get = static fn (string $name, string $fallback = ''): string => trim((string) ($settings[$name] ?? $fallback));
    renderPageStart('Settings', $user, 'admin_settings');
    ?>
    <div class="settings-layout wide">
        <?php renderFlash(); ?>

        <div class="settings-card">
            <h2 data-i18n="updateTargets">Automatic updates</h2>
            <p class="auth-note" data-i18n="updateTargetsHint">These cities are collected automatically by `gasoline update` when the CLI is invoked without --city/--radius flags. They decide which stations exist; each user picks the area they are notified about separately.</p>
            <div class="table-scroll">
            <table class="stack-table">
                <thead>
                    <tr><th data-i18n="targetCity">City</th><th data-i18n="targetRadius">Radius (km)</th><th data-i18n="colActions">Actions</th></tr>
                </thead>
                <tbody>
                    <?php foreach ($targets as $target) { ?>
                    <tr>
                        <td class="stack-primary"><?= h($target['city']) ?></td>
                        <td data-label="Radius (km)" data-i18n-label="targetRadius"><?= h((string) round((float) $target['radius_km'], 1)) ?></td>
                        <td class="actions-cell"><form method="post" action="" class="table-form"><?= csrfField() ?><input type="hidden" name="action" value="delete_target"><input type="hidden" name="target_id" value="<?= (int) $target['id'] ?>"><button type="submit" class="btn-small danger" data-i18n="removeTarget">Remove</button></form></td>
                    </tr>
                    <?php } ?>
                    <?php if ($targets === []) { ?>
                    <tr><td colspan="3" data-i18n="noTargets">No update targets configured yet.</td></tr>
                    <?php } ?>
                </tbody>
            </table>
            </div>
            <form method="post" action="" class="inline-form">
                <?= csrfField() ?>
                <input type="hidden" name="action" value="add_target">
                <input type="text" name="city" data-i18n-placeholder="targetCity" placeholder="City" required>
                <input type="number" name="radius_km" min="1" max="25" value="5" required>
                <button type="submit" class="btn-primary" data-i18n="addTarget">Add</button>
            </form>
        </div>

        <div class="settings-card">
            <h2 data-i18n="notificationTexts">Notification texts</h2>
            <p class="auth-note" data-i18n="notificationTextsHint">Suggestions and checks are computed for every fuel, covering every station the update targets above currently feed. These templates are the only part that is configured here; each user picks their own fuel, area and schedule in My Account.</p>
            <form method="post" action="">
                <?= csrfField() ?>
                <input type="hidden" name="action" value="save_settings">
                <div class="field">
                    <label for="st-check-title" data-i18n="templateCheckTitle">Buy-alert notification title</label>
                    <input type="text" id="st-check-title" name="check_title_template" data-i18n-placeholder="titleTemplatePlaceholder" placeholder="e.g. Fill up for {{cheapest_current_price_formatted}} EUR" value="<?= h($get('check_title_template')) ?>">
                </div>
                <div class="field">
                    <label for="st-check-tpl" data-i18n="templateCheck">Buy-alert notification template</label>
                    <textarea id="st-check-tpl" name="check_template" rows="3"><?= h($get('check_template')) ?></textarea>
                </div>
                <div class="field">
                    <label for="st-suggest-title" data-i18n="templateSuggestTitle">Suggestion notification title</label>
                    <input type="text" id="st-suggest-title" name="suggest_title_template" data-i18n-placeholder="titleTemplatePlaceholder" placeholder="e.g. Fill up for {{cheapest_current_price_formatted}} EUR" value="<?= h($get('suggest_title_template')) ?>">
                </div>
                <div class="field">
                    <label for="st-suggest-tpl" data-i18n="templateSuggest">Suggestion notification template</label>
                    <textarea id="st-suggest-tpl" name="suggest_template" rows="3"><?= h($get('suggest_template')) ?></textarea>
                </div>
                <p class="auth-note" data-i18n="templatePlaceholdersHint">Templates use {{placeholder}} syntax with the full gasoline-watch set, e.g. {{station_name}}, {{price}}, {{price_formatted}}, {{fuel}}, {{date}}, {{start_time}}, {{end_time}}, {{distance}}, {{confidence}}, {{count}}, {{cheapest_price}}, {{message}} and *_onchange variants.</p>
                <p class="auth-note" data-i18n="titleTemplatesHint">Title templates use the same placeholders; row placeholders resolve against the cheapest row. Leave a title empty to use each user's notification title instead.</p>
                <button type="submit" class="btn-primary" data-i18n="save">Save</button>
            </form>
        </div>
    </div>
    <?php
    renderPageEnd();
}

// ── Bootstrap: session, schema guard, POST routing, login gate ────────────────

header('X-Content-Type-Options: nosniff');
gasolineStartSession();

$requestedAction = (string) ($_GET['action'] ?? '');
$requestedPage = (string) ($_GET['page'] ?? '');
$isJSONRequest = in_array($requestedAction, ['city_search', 'station_search', 'geocode', 'data', 'prediction_accuracy', 'command_stats'], true);

$authPdo = null;
$schemaGuardReason = null;
if ($dbDriver === 'sqlite' && !file_exists($dbPath)) {
    // Do not connect: PDO would create an empty SQLite file as a side effect.
    $schemaGuardReason = 'dbNotFound';
} else {
    try {
        $authPdo = gasolineConnect($dbDriver, $dbPath);
    } catch (Throwable $e) {
        error_log('gasoline connect error: ' . $e->getMessage());
        $schemaGuardReason = 'dbConnectFailed';
    }
    if ($authPdo !== null && !gasolineSchemaReady($authPdo, $dbDriver)) {
        $schemaGuardReason = 'schemaOutdated';
    }
}

if ($schemaGuardReason !== null) {
    if ($isJSONRequest) {
        http_response_code(503);
        header('Content-Type: application/json; charset=utf-8');
        echo json_encode(['errors' => [['key' => 'schemaOutdatedBody', 'params' => [], 'message' => 'Database is not ready. Run `gasoline migrate` on the server.']]]);
        exit;
    }
    renderSchemaGuardPage($schemaGuardReason);
}

rememberSync($authPdo, $dbDriver);

handlePost($authPdo, $dbDriver);

$currentUser = currentUser($authPdo, $dbDriver);

if ($isJSONRequest && $currentUser === null) {
    http_response_code(401);
    header('Content-Type: application/json; charset=utf-8');
    echo json_encode(['errors' => [['key' => 'unauthorized', 'params' => [], 'message' => 'Login required.']]]);
    exit;
}

if ($currentUser === null) {
    if ($requestedPage === 'register') {
        renderRegisterPage();
    }
    if ($requestedPage !== 'login') {
        redirectTo('?page=login');
    }
    renderLoginPage();
}

switch ($requestedPage) {
    case 'login':
    case 'register':
        redirectTo('');
        // no break
    case 'account':
        renderAccountPage($authPdo, $currentUser);
        // no break
    case 'admin_users':
        if ((int) $currentUser['is_admin'] !== 1) {
            redirectTo('');
        }
        renderAdminUsersPage($authPdo, $currentUser);
        // no break
    case 'admin_stations':
        if ((int) $currentUser['is_admin'] !== 1) {
            redirectTo('');
        }
        renderAdminStationsPage($authPdo, $currentUser);
        // no break
    case 'admin_settings':
        if ((int) $currentUser['is_admin'] !== 1) {
            redirectTo('');
        }
        renderAdminSettingsPage($authPdo, $dbDriver, $currentUser);
        // no break
    case 'admin_predictions':
        if ((int) $currentUser['is_admin'] !== 1) {
            redirectTo('');
        }
        renderAdminPredictionsPage($authPdo, $dbDriver, $currentUser);
        // no break
    case 'admin_stats':
        if ((int) $currentUser['is_admin'] !== 1) {
            redirectTo('');
        }
        renderAdminStatsPage($authPdo, $dbDriver, $currentUser);
        // no break
}

// Fall through: the dashboard (default page) below.

// ── Filter state ──────────────────────────────────────────────────────────────
// Straight out of user_filters, for this account, on every load — page and
// ?action=data alike. Nothing in the query string selects data any more; the
// sidebar posts save_filters and comes back to the bare URL.
$dashboardFilters = loadDashboardFilters($authPdo, (int) $currentUser['id']);

// The filter panel's own collapsed state stays a cookie: it only applies below
// 900px, so it is a property of the phone in the reader's hand rather than of
// the account, and storing it per user would fold the desktop sidebar shut too.
$filtersCollapsed = (($_COOKIE['gasoline_filters_collapsed'] ?? '') === '1');

$errors = [];
$stations = [];

$selectedStationIds = $dashboardFilters['station_ids'];
$selectedRange = $dashboardFilters['range'];
$fromDate = $dashboardFilters['from'];
$toDate = $dashboardFilters['to'];
$selectedFuel = $dashboardFilters['fuel'];
$selectedRadiusKm = $dashboardFilters['radius_km'];

// The reader's location: the label they picked and the point it resolved to.
// Shaped like the cities row this used to be, because loadScopeStations and
// the snapshot query only ever wanted the coordinates out of it.
$selectedLocation = $dashboardFilters['location_label'];
$selectedCityRow = $selectedLocation === '' ? null : [
    'display_name' => $selectedLocation,
    'lat' => $dashboardFilters['location_lat'],
    'lng' => $dashboardFilters['location_lng'],
];

$rangeDays = DASHBOARD_RANGE_DAYS;
if ($selectedRange !== '') {
    $fromDate = (new DateTimeImmutable('now', new DateTimeZone('UTC')))
        ->modify('-' . $rangeDays[$selectedRange] . ' days')
        ->format('Y-m-d');
    $toDate = '';
}

// Days covered by the active date filter — decides which chart quick-range
// toggles are worth rendering (a 14d toggle is dead weight when the filter
// only loads 7 days; "All" already shows the whole payload). null means
// unbounded: no From date, so the full history loads.
$filterSpanDays = null;
if ($selectedRange !== '') {
    $filterSpanDays = $rangeDays[$selectedRange];
} elseif ($fromDate !== '') {
    $spanFrom = DateTimeImmutable::createFromFormat('Y-m-d', $fromDate, new DateTimeZone('UTC'));
    $spanTo = $toDate !== ''
        ? DateTimeImmutable::createFromFormat('Y-m-d', $toDate, new DateTimeZone('UTC'))
        : new DateTimeImmutable('now', new DateTimeZone('UTC'));
    if ($spanFrom !== false && $spanTo !== false && $spanTo >= $spanFrom) {
        $filterSpanDays = max(1, (int) ceil(($spanTo->getTimestamp() - $spanFrom->getTimestamp()) / 86400));
    }
}

$validFuels = DASHBOARD_FUELS;
$validRadiusOptions = DASHBOARD_RADIUS_OPTIONS;

if ($dbDriver === 'sqlite' && !file_exists($dbPath)) {
    $errors[] = [
        'key' => 'dbNotFound',
        'params' => ['path' => $dbPath],
        'message' => sprintf('SQLite database not found at %s', $dbPath),
    ];
}
// ── AJAX: location typeahead ──────────────────────────────────────────────────
// Answers from the database only, which is what makes it safe to run on a
// keystroke: Nominatim's usage policy rules out autocomplete traffic. Two
// sources, because a reader names where they are in two ways — the place, and
// the postal code of the place. A street address is neither, and is resolved by
// the dropdown's closing row instead (?action=geocode).
//
// Every hit carries its coordinates, so picking one costs no second lookup and
// nothing has to be looked up again when the filter is saved.
if (isset($_GET['action']) && $_GET['action'] === 'city_search') {
    header('Content-Type: application/json; charset=utf-8');
    $q = trim((string) ($_GET['q'] ?? ''));
    if (strlen($q) < 3 || ($dbDriver === 'sqlite' && !file_exists($dbPath))) {
        echo '[]';
        exit;
    }
    try {
        $searchPdo = gasolineConnect($dbDriver, $dbPath);
        // Prefix match as a half-open range over the stored folded name, which
        // is what lets idx_cities_search answer it. The alternatives both lose
        // the index: LOWER(normalized_name) puts the column inside a function,
        // and LIKE on a BINARY-collated column is not a range SQLite will seek.
        //
        // The upper bound appends the highest code point there is. Any character
        // that could follow the prefix sorts below it, so the range covers every
        // name starting with the prefix and nothing else — without having to
        // increment the prefix's last code point, which is fiddly in UTF-8.
        //
        // mb_strtolower mirrors citySearchKey in the CLI, which is what wrote
        // the column; plain strtolower would not, since it leaves Ü alone.
        $prefix = mb_strtolower($q, 'UTF-8');
        $searchStmt = $searchPdo->prepare(
            "SELECT normalized_name AS city_key, display_name, lat, lng
             FROM cities
             WHERE normalized_lower >= :prefix AND normalized_lower < :prefix_end
             ORDER BY normalized_lower ASC
             LIMIT 20"
        );
        $searchStmt->bindValue(':prefix', $prefix);
        $searchStmt->bindValue(':prefix_end', $prefix . "\u{10FFFF}");
        $searchStmt->execute();

        $matches = [];
        foreach ($searchStmt->fetchAll() as $row) {
            $matches[] = [
                // city_key only on rows that are a whole city: the account
                // page's notification area subscribes to one and needs its
                // key, and a postal code is not one.
                'city_key' => (string) $row['city_key'],
                'label' => (string) ($row['display_name'] !== '' ? $row['display_name'] : $row['city_key']),
                'lat' => (float) $row['lat'],
                'lng' => (float) $row['lng'],
            ];
        }
        foreach (postalCodeMatches($searchPdo, $q) as $match) {
            $matches[] = $match;
        }
        echo json_encode($matches, JSON_UNESCAPED_UNICODE | JSON_THROW_ON_ERROR);
    } catch (Throwable $e) {
        error_log('gasoline city_search error: ' . $e->getMessage());
        echo '[]';
    }
    exit;
}

// ── AJAX: station search (admin station rename) ───────────────────────────────
if (isset($_GET['action']) && $_GET['action'] === 'station_search') {
    header('Content-Type: application/json; charset=utf-8');
    if ((int) $currentUser['is_admin'] !== 1) {
        http_response_code(403);
        echo json_encode(['errors' => [['key' => 'notFound', 'params' => [], 'message' => 'Administrator access required.']]]);
        exit;
    }
    $q = trim((string) ($_GET['q'] ?? ''));
    $terms = preg_split('/\s+/', mb_strtolower($q, 'UTF-8'), 8, PREG_SPLIT_NO_EMPTY) ?: [];
    if (strlen($q) < 2 || $terms === []) {
        echo '[]';
        exit;
    }
    try {
        // Every whitespace-separated term must match the effective name, the
        // canonical name, the brand, or one of the address fields.
        $haystacks = [
            'COALESCE(s.name_override, s.name)',
            's.name',
            "COALESCE(s.brand, '')",
            "COALESCE(s.street, '')",
            "COALESCE(s.post_code, '')",
            "COALESCE(s.place, '')",
        ];
        $where = [];
        $params = [];
        foreach ($terms as $termIndex => $term) {
            $group = [];
            foreach ($haystacks as $haystackIndex => $haystack) {
                $placeholder = ':t' . $termIndex . '_' . $haystackIndex;
                $group[] = 'LOWER(' . $haystack . ') LIKE ' . $placeholder;
                $params[$placeholder] = '%' . $term . '%';
            }
            $where[] = '(' . implode(' OR ', $group) . ')';
        }
        $searchStmt = $authPdo->prepare(
            'SELECT s.id, s.name, s.name_override, s.brand, s.street, s.house_number, s.post_code, s.place
             FROM stations s
             WHERE ' . implode(' AND ', $where) . '
             ORDER BY COALESCE(s.name_override, s.name) ASC, s.id ASC
             LIMIT 20'
        );
        foreach ($params as $key => $value) {
            $searchStmt->bindValue($key, $value);
        }
        $searchStmt->execute();
        $results = [];
        foreach ($searchStmt->fetchAll() as $row) {
            $results[] = [
                'id' => (string) $row['id'],
                'name' => $row['name_override'] !== null ? (string) $row['name_override'] : (string) $row['name'],
                'brand' => trim((string) ($row['brand'] ?? '')),
                'address' => stationAddress($row),
            ];
        }
        echo json_encode($results, JSON_UNESCAPED_UNICODE | JSON_THROW_ON_ERROR);
    } catch (Throwable $e) {
        error_log('gasoline station_search error: ' . $e->getMessage());
        echo '[]';
    }
    exit;
}

// ── AJAX: resolve an address or a browser position ────────────────────────────
// The location filter's typeahead is served from the cities cache and never
// leaves the server; this is the one path that adds to that cache, and it runs
// only when the reader explicitly asks for an address or presses the locate
// button. See the geocoding block further down for why that separation matters.
if (isset($_GET['action']) && $_GET['action'] === 'geocode') {
    header('Content-Type: application/json; charset=utf-8');
    $geocodeFail = static function (string $key, string $message): void {
        echo json_encode(
            ['errors' => [['key' => $key, 'params' => [], 'message' => $message]]],
            JSON_UNESCAPED_UNICODE
        );
        exit;
    };

    // A GET that writes to the cache, so it carries the page's token: without
    // it another site could have a signed-in reader's browser spend the
    // server's Nominatim budget.
    if (!hash_equals(csrfToken(), (string) ($_GET['csrf'] ?? ''))) {
        http_response_code(403);
        $geocodeFail('geocodeFailed', 'Invalid request token.');
    }

    $query = trim((string) ($_GET['q'] ?? ''));
    $latRaw = trim((string) ($_GET['lat'] ?? ''));
    $lngRaw = trim((string) ($_GET['lng'] ?? ''));
    $fromPosition = $latRaw !== '' || $lngRaw !== '';

    try {
        if ($fromPosition) {
            if (!is_numeric($latRaw) || !is_numeric($lngRaw)) {
                $geocodeFail('geocodeFailed', 'Invalid position.');
            }
            $lat = (float) $latRaw;
            $lng = (float) $lngRaw;
            if ($lat < -90.0 || $lat > 90.0 || $lng < -180.0 || $lng > 180.0) {
                $geocodeFail('geocodeFailed', 'Invalid position.');
            }
            $place = geocodeEnabled() ? geocodeReverse($lat, $lng) : null;
            if ($place === null) {
                // No reverse lookup (disabled, or the service is unreachable):
                // the fix itself is still a perfectly good centre, so fall back
                // to labelling it by its coordinates rather than failing. The
                // radius filter only ever needs the coordinates.
                $place = ['label' => sprintf('%.4f, %.4f', $lat, $lng), 'lat' => $lat, 'lng' => $lng];
            }
        } else {
            if (!geocodeEnabled()) {
                $geocodeFail('geocodeDisabled', 'Address lookup is disabled on this server.');
            }
            if (mb_strlen($query, 'UTF-8') < 3) {
                $geocodeFail('geocodeFailed', 'Enter at least three characters.');
            }
            $place = geocodeSearch($query);
            if ($place === null) {
                $geocodeFail('geocodeNoMatch', 'No place found for that address.');
            }
        }

        // Handed straight back, not stored: where the reader is belongs on
        // their filter row, which the sidebar writes when it posts this.
        echo json_encode(
            ['label' => $place['label'], 'lat' => $place['lat'], 'lng' => $place['lng']],
            JSON_UNESCAPED_UNICODE | JSON_THROW_ON_ERROR
        );
    } catch (Throwable $e) {
        error_log('gasoline geocode error: ' . $e->getMessage());
        http_response_code(500);
        $geocodeFail('geocodeFailed', 'Could not resolve that location.');
    }
    exit;
}

// ── AJAX: prediction accuracy (predicted vs. actual) ──────────────────────────
// Admin-only. Surfaces the evaluated prediction grid — `actual_price`/`error` are
// filled in by the Go `suggest --persist` evaluation step (predictions.go), where
// error = actual_price − predicted_price. All heavy aggregation stays in SQL so the
// payload is bounded no matter how large the evaluated grid grows; only a capped
// sample of raw rows is shipped for the table.
if (isset($_GET['action']) && $_GET['action'] === 'prediction_accuracy') {
    header('Content-Type: application/json; charset=utf-8');
    $jsonFlags = JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE | JSON_THROW_ON_ERROR;
    if ((int) $currentUser['is_admin'] !== 1) {
        http_response_code(403);
        echo json_encode(['errors' => [['key' => 'notFound', 'params' => [], 'message' => 'Administrator access required.']]], $jsonFlags);
        exit;
    }

    $rawTableLimit = 1000;

    // Filters ------------------------------------------------------------------
    $paFuel = trim((string) ($_GET['fuel'] ?? 'diesel'));
    if (!in_array($paFuel, ['diesel', 'e5', 'e10'], true)) {
        $paFuel = 'diesel';
    }
    $paConfidence = trim((string) ($_GET['confidence'] ?? 'all')); // 'all' | 'medium_high'

    // Date range on the target hour. Default: last 14 days. target_start is an
    // RFC3339 UTC string, so lexicographic comparison against 'Y-m-dTHH:MM:SSZ' is
    // a correct chronological comparison.
    $paRange = trim((string) ($_GET['range'] ?? ''));
    $rangeDaysMap = ['7d' => 7, '14d' => 14, '30d' => 30];
    $fromRaw = trim((string) ($_GET['from'] ?? ''));
    $toRaw = trim((string) ($_GET['to'] ?? ''));
    $utc = new DateTimeZone('UTC');
    if (isset($rangeDaysMap[$paRange]) || ($fromRaw === '' && $toRaw === '')) {
        $days = $rangeDaysMap[$paRange] ?? 14;
        $fromTs = (new DateTimeImmutable('now', $utc))->modify('-' . $days . ' days')->format('Y-m-d\T00:00:00\Z');
        $toTs = (new DateTimeImmutable('now', $utc))->format('Y-m-d\T23:59:59\Z');
    } else {
        $fromTs = preg_match('/^\d{4}-\d{2}-\d{2}$/', $fromRaw) ? $fromRaw . 'T00:00:00Z' : '0000-01-01T00:00:00Z';
        $toTs = preg_match('/^\d{4}-\d{2}-\d{2}$/', $toRaw) ? $toRaw . 'T23:59:59Z' : '9999-12-31T23:59:59Z';
    }

    $out = [
        'summary' => null,
        'summary_latest' => null,
        'by_confidence' => [],
        'by_lead' => [],
        'by_hour' => [],
        // null (not []) while the decisions table does not exist yet, so the
        // UI can hide the card instead of showing an empty one.
        'decisions' => null,
        'series' => [],
        'rows' => [],
        'stations' => [],
        'filters' => ['fuel' => $paFuel, 'confidence' => $paConfidence, 'from' => $fromTs, 'to' => $toTs],
        'truncated' => false,
        'errors' => [],
    ];

    try {
        $pdo = $authPdo;
        $driver = $dbDriver;
        // Shared WHERE + params reused across the aggregate, breakdown, series and
        // raw-row queries so every panel reflects exactly the same filtered set.
        // Aggregate queries below read `$ppHinted` instead of naming the table
        // directly; see gasolineAccuracyIndexHint for why they carry a hint and
        // how to re-check that it still earns its place. The raw-row query
        // carries it too, since the table grew past where its old measurement
        // held. `series` is the one query left on the plain reference: forcing
        // the index there moved it by -13%, +4%, -1% and +5% over four runs,
        // which straddles zero, so there is nothing to buy and the optimizer is
        // left free.
        $ppHint = gasolineAccuracyIndexHint($pdo, $driver);
        $ppTable = 'price_predictions pp ';
        $ppHinted = $ppHint === '' ? $ppTable : $ppTable . $ppHint . ' ';
        $joinRuns = '';
        $where = 'pp.actual_price IS NOT NULL AND pp.fuel = :fuel AND pp.target_start >= :from AND pp.target_start <= :to';
        $params = [':fuel' => $paFuel, ':from' => $fromTs, ':to' => $toTs];
        if ($paConfidence === 'medium_high') {
            $where .= " AND pp.confidence IN ('medium', 'high')";
        }
        $bind = static function (PDOStatement $stmt) use ($params): void {
            foreach ($params as $k => $v) {
                $stmt->bindValue($k, $v);
            }
        };

        // 1) Overall accuracy stats — SQL aggregates, exact over the full set.
        $aggStmt = $pdo->prepare(
            'SELECT COUNT(*) AS n, COUNT(DISTINCT pp.station_id) AS stations, '
            . 'MIN(pp.target_start) AS first_t, MAX(pp.target_start) AS last_t, '
            . 'AVG(ABS(pp.error)) AS mae, AVG(pp.error) AS bias, AVG(pp.error * pp.error) AS mse, '
            . 'MIN(pp.error) AS min_err, MAX(pp.error) AS max_err, '
            . 'SUM(CASE WHEN ABS(pp.error) <= 0.01 THEN 1 ELSE 0 END) AS within1, '
            . 'SUM(CASE WHEN ABS(pp.error) <= 0.02 THEN 1 ELSE 0 END) AS within2 '
            . 'FROM ' . $ppHinted . $joinRuns
            . 'WHERE ' . $where
        );
        $bind($aggStmt);
        $aggStmt->execute();
        $agg = $aggStmt->fetch() ?: [];
        $count = (int) ($agg['n'] ?? 0);

        if ($count === 0) {
            echo json_encode($out, $jsonFlags);
            exit;
        }

        $mse = (float) ($agg['mse'] ?? 0);
        $out['summary'] = [
            'count' => $count,
            'stations' => (int) ($agg['stations'] ?? 0),
            'first' => $agg['first_t'] ?? null,
            'last' => $agg['last_t'] ?? null,
            'mae' => (float) ($agg['mae'] ?? 0),
            'bias' => (float) ($agg['bias'] ?? 0),
            'rmse' => sqrt(max(0.0, $mse)),
            'min_error' => (float) ($agg['min_err'] ?? 0),
            'max_error' => (float) ($agg['max_err'] ?? 0),
            'within1' => (int) ($agg['within1'] ?? 0),
            'within2' => (int) ($agg['within2'] ?? 0),
            'within1_pct' => (float) ($agg['within1'] ?? 0) / $count * 100,
            'within2_pct' => (float) ($agg['within2'] ?? 0) / $count * 100,
        ];

        // 1b) Same stats deduplicated to the latest run per (station, target
        //     window). Every hourly persist run re-predicts the same future
        //     hours, so the full set counts one window dozens of times across
        //     leads and is dominated by long-lead rows; the freshest
        //     prediction per window is what a user acting on current output
        //     experiences. Both views are shown side by side.
        //
        //     The outer query repeats `fuel` even though the join keys already
        //     pin it: a prediction row and its run always carry the same fuel
        //     (see persistPredictions), so run_id determines it and the
        //     predicate cannot change the result. It exists to make
        //     idx_price_predictions_accuracy usable — its leading column is
        //     fuel, so without a fuel predicate the outer lookup cannot use the
        //     index at all and MySQL falls back to matching station_id alone,
        //     then filtering ~180 rows per joined window by hand. Measured on
        //     the live database that was 18.9 s of a 26 s page; with the
        //     predicate all four of (fuel, target_start, station_id, run_id)
        //     are equalities and the lookup is a single index-only probe.
        //     Bind it under its own name so the query never depends on a
        //     driver's willingness to reuse a named placeholder.
        $latestJoin = 'JOIN ('
            . 'SELECT pp.station_id AS station_id, pp.target_start AS target_start, MAX(pp.run_id) AS run_id '
            . 'FROM ' . $ppHinted . $joinRuns
            . 'WHERE ' . $where
            // Grouped target_start first, station_id second. Same groups
            // either way — the join below matches on both — but this is the
            // index's own order within one fuel, and run_id is the column
            // immediately after them in it. Look for "Using index for group-by"
            // or the loss of "Using temporary" in EXPLAIN to see whether the
            // server takes advantage; SQLite streamed it both ways.
            . ' GROUP BY pp.target_start, pp.station_id'
            . ') latest ON latest.station_id = pp.station_id'
            . ' AND latest.target_start = pp.target_start'
            . ' AND latest.run_id = pp.run_id';
        $latestStmt = $pdo->prepare(
            'SELECT COUNT(*) AS n, '
            . 'AVG(ABS(pp.error)) AS mae, AVG(pp.error) AS bias, '
            . 'SUM(CASE WHEN ABS(pp.error) <= 0.02 THEN 1 ELSE 0 END) AS within2 '
            . 'FROM ' . $ppHinted . $latestJoin
            . ' WHERE pp.fuel = :outer_fuel'
        );
        $latestStmt->bindValue(':outer_fuel', $paFuel);
        $bind($latestStmt);
        $latestStmt->execute();
        $latestAgg = $latestStmt->fetch() ?: [];
        $latestCount = (int) ($latestAgg['n'] ?? 0);
        if ($latestCount > 0) {
            $out['summary_latest'] = [
                'count' => $latestCount,
                'mae' => (float) ($latestAgg['mae'] ?? 0),
                'bias' => (float) ($latestAgg['bias'] ?? 0),
                'within2_pct' => (float) ($latestAgg['within2'] ?? 0) / $latestCount * 100,
            ];
        }

        // 2) The three breakdown tables — by confidence, by lead-time bucket and
        //    by hour of day — and the predicted-vs-actual chart series all come
        //    out of one pass.
        //
        //    They are four groupings of exactly the same rows, and they used to
        //    be four queries, each walking the whole filtered slice again: on a
        //    production database that was 1.4M index entries read four times, to
        //    produce 3, 6, 24 and ~570 rows. Grouping by target_start,
        //    confidence and lead bucket at once reads the slice once; hour is a
        //    substring of target_start and the chart is the sum per
        //    target_start, so every table falls out of that one result. Measured
        //    on SQLite, folding the chart into the breakdown pass cost nothing
        //    at all — the merged query timed the same as the breakdowns alone.
        //
        //    The sums are what make that possible: an average cannot be
        //    re-aggregated (the mean of means is not the mean unless every group
        //    is the same size), so the query returns SUM and COUNT and the
        //    averages are divided out per table below. That is exact, not an
        //    approximation, up to the order floating-point addition happens in.
        //
        //    The lead bucket is grouped as an integer and labelled in PHP, not
        //    grouped by its label: the group key is evaluated for every row in
        //    the slice, and on a 1.39M-row slice the string form measured 674 ms
        //    more than the integer one for the same 336 groups.
        //
        //    The 3-6h lead boundary is deliberately 360 minutes, the cutoff
        //    below which predictions.go lets predictions train the bias
        //    correction, so the table shows both sides of that line. Hours are
        //    taken in PHP from characters 12-13 of target_start, a fixed-width
        //    RFC3339 UTC string (see the invariant above); doing it there rather
        //    than with SUBSTR in the group key avoids evaluating it per row, and
        //    sidesteps strftime()/HOUR(), neither of which exists in both SQLite
        //    and MySQL. The buckets are therefore UTC hours and the UI labels
        //    them as such rather than silently shifting them into the viewer's
        //    timezone.
        $leadBucket = gasolineLeadBucketSql();
        $breakdownStmt = $pdo->prepare(
            'SELECT pp.target_start AS t, pp.confidence AS confidence, '
            . $leadBucket . ' AS bucket, MIN(pp.lead_minutes) AS lead_floor, '
            . 'COUNT(*) AS n, SUM(ABS(pp.error)) AS abs_error, SUM(pp.error) AS sum_error, '
            . 'SUM(pp.predicted_price) AS sum_predicted, SUM(pp.actual_price) AS sum_actual '
            . 'FROM ' . $ppHinted . $joinRuns
            . 'WHERE ' . $where
            . ' GROUP BY pp.target_start, pp.confidence, ' . $leadBucket
        );
        $bind($breakdownStmt);
        $breakdownStmt->execute();

        $breakdowns = gasolineBreakdownTables($breakdownStmt->fetchAll());
        $out['by_confidence'] = $breakdowns['by_confidence'];
        $out['by_lead'] = $breakdowns['by_lead'];
        $out['by_hour'] = $breakdowns['by_hour'];
        $out['series'] = $breakdowns['series'];

        // 2d) Alert outcomes: when the check path said buy, how close to that
        //     pricing day's floor did the price turn out to be? Only settled
        //     rows count. The table is newer than the rest of the schema, so
        //     its absence must not break the page.
        if (gasolineTableExists($pdo, $driver, 'price_check_decisions')) {
            $decWhere = 'd.outcome_evaluated_at IS NOT NULL AND d.regret IS NOT NULL'
                . ' AND d.fuel = :fuel AND d.target_start >= :from AND d.target_start <= :to';
            $decParams = [':fuel' => $paFuel, ':from' => $fromTs, ':to' => $toTs];
            $decJoin = '';
            if ($paConfidence === 'medium_high') {
                $decWhere .= " AND d.confidence IN ('medium', 'high')";
            }
            $decStmt = $pdo->prepare(
                'SELECT d.recommendation AS recommendation, COUNT(*) AS n, '
                . 'AVG(d.regret) AS avg_regret, '
                . 'SUM(CASE WHEN d.regret <= 0.01 THEN 1 ELSE 0 END) AS within1, '
                . 'SUM(CASE WHEN d.regret <= 0.02 THEN 1 ELSE 0 END) AS within2 '
                . 'FROM price_check_decisions d ' . $decJoin
                . 'WHERE ' . $decWhere . ' GROUP BY d.recommendation'
            );
            foreach ($decParams as $k => $v) {
                $decStmt->bindValue($k, $v);
            }
            $decStmt->execute();
            $decisions = [];
            foreach ($decStmt->fetchAll() as $row) {
                $n = (int) $row['n'];
                $decisions[] = [
                    'recommendation' => (string) $row['recommendation'],
                    'count' => $n,
                    'avg_regret' => (float) $row['avg_regret'],
                    'within1_pct' => $n > 0 ? (float) $row['within1'] / $n * 100 : 0.0,
                    'within2_pct' => $n > 0 ? (float) $row['within2'] / $n * 100 : 0.0,
                ];
            }
            $out['decisions'] = $decisions;
        }

        // 4) Raw rows for the table — newest target first, capped. Station metadata
        //    is sent once keyed by id (mirrors the ?action=data slim-row shape).
        //
        //    The filter, ordering and cap are applied in a derived table before
        //    the metadata joins. Joined directly, the optimizer is free to drive
        //    the query from stations and probe price_predictions per station:
        //    measured on the live database it scanned all 360 stations, examined
        //    ~180 prediction rows each, then sorted the lot — 2.4 s to return
        //    1001 rows. Selecting the capped page first bounds both joins to
        //    those rows. Both joins are on primary keys of mandatory foreign
        //    keys, so neither can drop or duplicate a row, and the outer ORDER
        //    BY repeats the inner one because a derived table's order is not
        //    guaranteed to survive a join.
        //
        //    The inner select carries the index hint. It used not to: when this
        //    query was last measured the hint moved it by -2%. At 8M rows the
        //    optimizer instead drives it from idx_price_predictions_due, which
        //    leads with fuel and then has nothing for the target_start range or
        //    the sort, and it became the slowest query on the page — 8.3-8.8 s
        //    against 6.0-6.5 s forced, measured four times with the ranges never
        //    overlapping. `gasoline doctor --try-index
        //    idx_price_predictions_accuracy` is what re-checks this.
        $rowsStmt = $pdo->prepare(
            'SELECT page.station_id, page.fuel, pr.run_at, page.target_start, page.target_end, '
            . 'page.predicted_price, page.actual_price, page.error, page.confidence, page.lead_minutes, page.is_suggestion, '
            . 'COALESCE(s.name_override, s.name) AS name, s.brand, s.street, s.house_number, s.post_code, s.place '
            . 'FROM ('
            . 'SELECT pp.run_id, pp.station_id, pp.fuel, pp.target_start, pp.target_end, '
            . 'pp.predicted_price, pp.actual_price, pp.error, pp.confidence, pp.lead_minutes, pp.is_suggestion '
            . 'FROM ' . $ppHinted . $joinRuns
            . 'WHERE ' . $where
            // Both keys descending: that is this index read backwards, so the
            // LIMIT stops after 1001 entries instead of sorting the whole slice
            // to find them. With station_id ascending the server cannot walk one
            // direction for both keys and has to materialise and sort every row
            // in the range first — SQLite's plan says "USE TEMP B-TREE FOR LAST
            // TERM OF ORDER BY", and on 8M rows that query was 6.1 s of a 15.8 s
            // page while its own probe showed the joins costing nothing.
            //
            // The outer query re-sorts station_id ascending for display, so the
            // only difference is which stations fall inside the cap at its
            // oldest hour, and that boundary is arbitrary either way.
            . ' ORDER BY pp.target_start DESC, pp.station_id DESC LIMIT ' . ($rawTableLimit + 1)
            . ') page '
            . 'JOIN prediction_runs pr ON pr.id = page.run_id '
            . 'JOIN stations s ON s.id = page.station_id '
            . ' ORDER BY page.target_start DESC, page.station_id ASC'
        );
        $bind($rowsStmt);
        $rowsStmt->execute();
        $rawRows = $rowsStmt->fetchAll();
        if (count($rawRows) > $rawTableLimit) {
            $out['truncated'] = true;
            $rawRows = array_slice($rawRows, 0, $rawTableLimit);
        }
        $meta = [];
        foreach ($rawRows as $row) {
            $sid = (string) $row['station_id'];
            if (!isset($meta[$sid])) {
                $meta[$sid] = [
                    'name' => (string) $row['name'],
                    'brand' => trim((string) ($row['brand'] ?? '')),
                    'address' => stationAddress($row),
                ];
            }
            $out['rows'][] = [
                's' => $sid,
                'fuel' => (string) $row['fuel'],
                'run_at' => (string) $row['run_at'],
                'start' => (string) $row['target_start'],
                'end' => (string) $row['target_end'],
                'p' => (float) $row['predicted_price'],
                'a' => (float) $row['actual_price'],
                'err' => (float) $row['error'],
                'conf' => (string) $row['confidence'],
                'lead' => (int) $row['lead_minutes'],
                'sugg' => (int) $row['is_suggestion'] === 1,
            ];
        }
        $out['stations'] = $meta;

        echo json_encode($out, $jsonFlags);
    } catch (Throwable $e) {
        error_log('gasoline prediction_accuracy error: ' . $e->getMessage());
        http_response_code(500);
        echo json_encode(['errors' => [['key' => 'loadError', 'params' => [], 'message' => 'Could not load prediction data.']]], $jsonFlags);
    }
    exit;
}

// ── AJAX: command run statistics ──────────────────────────────────────────────
if (isset($_GET['action']) && $_GET['action'] === 'command_stats') {
    header('Content-Type: application/json; charset=utf-8');
    $jsonFlags = JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE | JSON_THROW_ON_ERROR;
    if ((int) $currentUser['is_admin'] !== 1) {
        http_response_code(403);
        echo json_encode(['errors' => [['key' => 'notFound', 'params' => [], 'message' => 'Administrator access required.']]], $jsonFlags);
        exit;
    }

    // The recent-runs table is a sample, not the statistics: the aggregates
    // below run over the whole filtered set regardless of this cap.
    $csRowLimit = 200;
    // A run still marked 'running' after this long never finished. Nothing
    // clears the row later — there is no daemon — so the reader decides, and
    // the window is generous enough to cover a slow `suggest --persist`.
    $csStaleHours = 6;

    $csCommand = trim((string) ($_GET['command'] ?? 'all'));
    if (!in_array($csCommand, ['all', 'update', 'suggest', 'check', 'notify'], true)) {
        $csCommand = 'all';
    }
    $csRange = trim((string) ($_GET['range'] ?? '7d'));
    $rangeHours = ['24h' => 24, '7d' => 24 * 7, '30d' => 24 * 30];
    if (!isset($rangeHours[$csRange])) {
        $csRange = '7d';
    }
    $hours = $rangeHours[$csRange];
    // Hourly buckets read as noise over a month, and daily ones hide a gap
    // within a day, so the bucket follows the range.
    $daily = $hours > 24;

    $utc = new DateTimeZone('UTC');
    $now = new DateTimeImmutable('now', $utc);
    // started_at is an RFC3339 UTC string, so lexicographic comparison against
    // another one is a correct chronological comparison.
    $fromTs = $now->modify('-' . $hours . ' hours')->format('Y-m-d\TH:i:s\Z');
    $staleTs = $now->modify('-' . $csStaleHours . ' hours')->format('Y-m-d\TH:i:s\Z');

    $out = [
        'summary' => null,
        'by_command' => [],
        'series' => [],
        'metric_totals' => [],
        'rows' => [],
        'daily_buckets' => $daily,
        'filters' => ['command' => $csCommand, 'range' => $csRange, 'from' => $fromTs],
        'truncated' => false,
        'errors' => [],
    ];

    try {
        $pdo = $authPdo;

        // One shared WHERE reused by every panel, so all of them describe the
        // same set of runs.
        $where = 'cr.started_at >= :from';
        $params = [':from' => $fromTs];
        if ($csCommand !== 'all') {
            $where .= ' AND cr.command = :command';
            $params[':command'] = $csCommand;
        }
        $bind = static function (PDOStatement $stmt) use ($params): void {
            foreach ($params as $k => $v) {
                $stmt->bindValue($k, $v);
            }
        };

        // 1) Overall counts. duration_ms is NULL for a run that never
        //    finished, so the averages below already exclude those.
        $aggStmt = $pdo->prepare(
            "SELECT COUNT(*) AS runs,
                    SUM(CASE WHEN cr.status = 'ok' THEN 1 ELSE 0 END) AS ok,
                    SUM(CASE WHEN cr.status = 'partial' THEN 1 ELSE 0 END) AS partial,
                    SUM(CASE WHEN cr.status = 'error' THEN 1 ELSE 0 END) AS error_count,
                    SUM(CASE WHEN cr.status = 'running' AND cr.started_at < :stale THEN 1 ELSE 0 END) AS interrupted,
                    MAX(cr.started_at) AS last_started_at
             FROM command_runs cr
             WHERE $where"
        );
        $bind($aggStmt);
        $aggStmt->bindValue(':stale', $staleTs);
        $aggStmt->execute();
        $agg = $aggStmt->fetch();

        $runs = (int) ($agg['runs'] ?? 0);
        if ($runs === 0) {
            $out['summary'] = ['runs' => 0];
            echo json_encode($out, $jsonFlags);
            exit;
        }

        // 2) Percentiles in PHP, not SQL: SQLite has no percentile function and
        //    the supported MySQL/MariaDB floor rules out window functions. Only
        //    the duration column is read, so this stays a narrow scan.
        $durStmt = $pdo->prepare(
            "SELECT cr.duration_ms FROM command_runs cr
             WHERE $where AND cr.duration_ms IS NOT NULL
             ORDER BY cr.duration_ms ASC"
        );
        $bind($durStmt);
        $durStmt->execute();
        $durations = array_map('intval', $durStmt->fetchAll(PDO::FETCH_COLUMN, 0));
        $percentile = static function (array $sorted, float $p): ?int {
            if (count($sorted) === 0) {
                return null;
            }
            $idx = (int) floor($p * (count($sorted) - 1));
            return $sorted[$idx];
        };

        $okish = (int) ($agg['ok'] ?? 0) + (int) ($agg['partial'] ?? 0);
        $out['summary'] = [
            'runs' => $runs,
            'ok' => (int) ($agg['ok'] ?? 0),
            'partial' => (int) ($agg['partial'] ?? 0),
            'error' => (int) ($agg['error_count'] ?? 0),
            'interrupted' => (int) ($agg['interrupted'] ?? 0),
            // A degraded run still did most of its work, so it counts as a
            // success here; the Failed tile is what separates them.
            'success_pct' => $runs > 0 ? round(($okish / $runs) * 100, 1) : null,
            'p50_ms' => $percentile($durations, 0.5),
            'p95_ms' => $percentile($durations, 0.95),
            'last_started_at' => $agg['last_started_at'] !== null ? (string) $agg['last_started_at'] : null,
        ];

        // 3) Per command.
        $cmdStmt = $pdo->prepare(
            "SELECT cr.command,
                    COUNT(*) AS runs,
                    SUM(CASE WHEN cr.status = 'ok' THEN 1 ELSE 0 END) AS ok,
                    SUM(CASE WHEN cr.status = 'partial' THEN 1 ELSE 0 END) AS partial,
                    SUM(CASE WHEN cr.status = 'error' THEN 1 ELSE 0 END) AS error_count,
                    AVG(cr.duration_ms) AS avg_ms,
                    MAX(cr.duration_ms) AS max_ms,
                    MAX(cr.started_at) AS last_started_at
             FROM command_runs cr
             WHERE $where
             GROUP BY cr.command
             ORDER BY cr.command ASC"
        );
        $bind($cmdStmt);
        $cmdStmt->execute();
        foreach ($cmdStmt->fetchAll() as $row) {
            $out['by_command'][] = [
                'command' => (string) $row['command'],
                'runs' => (int) $row['runs'],
                'ok' => (int) $row['ok'],
                'partial' => (int) $row['partial'],
                'error' => (int) $row['error_count'],
                'avg_ms' => $row['avg_ms'] !== null ? (float) $row['avg_ms'] : null,
                'max_ms' => $row['max_ms'] !== null ? (int) $row['max_ms'] : null,
                'last_started_at' => $row['last_started_at'] !== null ? (string) $row['last_started_at'] : null,
            ];
        }

        // 4) Time buckets for the chart. Slicing the RFC3339 string is the one
        //    bucketing both engines agree on without date parsing: characters
        //    1-13 are 'YYYY-MM-DDTHH', 1-10 are 'YYYY-MM-DD'.
        $bucketLen = $daily ? 10 : 13;
        $seriesStmt = $pdo->prepare(
            "SELECT SUBSTR(cr.started_at, 1, $bucketLen) AS bucket,
                    SUM(CASE WHEN cr.status = 'ok' THEN 1 ELSE 0 END) AS ok,
                    SUM(CASE WHEN cr.status = 'partial' THEN 1 ELSE 0 END) AS partial,
                    SUM(CASE WHEN cr.status = 'error' THEN 1 ELSE 0 END) AS error_count,
                    SUM(CASE WHEN cr.status = 'running' THEN 1 ELSE 0 END) AS running,
                    AVG(cr.duration_ms) AS avg_ms
             FROM command_runs cr
             WHERE $where
             GROUP BY SUBSTR(cr.started_at, 1, $bucketLen)
             ORDER BY bucket ASC"
        );
        $bind($seriesStmt);
        $seriesStmt->execute();
        $out['series'] = gasolineCommandStatsSeries($seriesStmt->fetchAll(), $now, $hours, $daily);

        // 5) Metric totals. The per-run average divides by the runs that
        //    actually reported the metric, not by every run of the command:
        //    `suggest` only records its persist counters with --persist, and
        //    averaging those over plain runs too would understate them.
        $metricStmt = $pdo->prepare(
            "SELECT cr.command, m.name,
                    SUM(m.value) AS total,
                    COUNT(*) AS samples
             FROM command_run_metrics m
             JOIN command_runs cr ON cr.id = m.run_id
             WHERE $where
             GROUP BY cr.command, m.name
             ORDER BY cr.command ASC, m.name ASC"
        );
        $bind($metricStmt);
        $metricStmt->execute();
        foreach ($metricStmt->fetchAll() as $row) {
            $samples = (int) $row['samples'];
            $total = (float) $row['total'];
            $out['metric_totals'][] = [
                'command' => (string) $row['command'],
                'name' => (string) $row['name'],
                'total' => $total,
                'per_run' => $samples > 0 ? round($total / $samples, 2) : null,
            ];
        }

        // 6) The recent runs themselves, newest first, with their metrics.
        $rowStmt = $pdo->prepare(
            "SELECT cr.id, cr.command, cr.started_at, cr.finished_at, cr.duration_ms,
                    cr.status, cr.error, cr.host, cr.version
             FROM command_runs cr
             WHERE $where
             ORDER BY cr.started_at DESC, cr.id DESC
             LIMIT " . ($csRowLimit + 1)
        );
        $bind($rowStmt);
        $rowStmt->execute();
        $runRows = $rowStmt->fetchAll();
        if (count($runRows) > $csRowLimit) {
            $out['truncated'] = true;
            $runRows = array_slice($runRows, 0, $csRowLimit);
        }

        $byID = [];
        foreach ($runRows as $row) {
            $id = (int) $row['id'];
            $byID[$id] = [
                'id' => $id,
                'command' => (string) $row['command'],
                'started_at' => (string) $row['started_at'],
                'finished_at' => $row['finished_at'] !== null ? (string) $row['finished_at'] : null,
                'duration_ms' => $row['duration_ms'] !== null ? (int) $row['duration_ms'] : null,
                'status' => (string) $row['status'],
                'error' => $row['error'] !== null && $row['error'] !== '' ? (string) $row['error'] : null,
                'host' => (string) $row['host'],
                'version' => (string) $row['version'],
                'metrics' => [],
            ];
        }
        if (count($byID) > 0) {
            // One query for every displayed run's metrics, pivoted in PHP:
            // a row per metric would make the table N+1 queries deep.
            $ids = array_keys($byID);
            $placeholders = implode(', ', array_fill(0, count($ids), '?'));
            $mStmt = $pdo->prepare(
                "SELECT run_id, name, value FROM command_run_metrics WHERE run_id IN ($placeholders)"
            );
            $mStmt->execute($ids);
            foreach ($mStmt->fetchAll() as $row) {
                $rid = (int) $row['run_id'];
                if (isset($byID[$rid])) {
                    $byID[$rid]['metrics'][(string) $row['name']] = (float) $row['value'];
                }
            }
        }
        $out['rows'] = array_values($byID);

        echo json_encode($out, $jsonFlags);
    } catch (Throwable $e) {
        error_log('gasoline command_stats error: ' . $e->getMessage());
        http_response_code(500);
        echo json_encode(['errors' => [['key' => 'loadError', 'params' => [], 'message' => 'Could not load command statistics.']]], $jsonFlags);
    }
    exit;
}

function boundingBox(float $lat, float $lng, int $radiusKm): array
{
    $latDelta = $radiusKm / 111.32;
    $lngDivisor = 111.32 * max(cos(deg2rad($lat)), 0.01);
    $lngDelta = $radiusKm / $lngDivisor;

    return [
        'min_lat' => $lat - $latDelta,
        'max_lat' => $lat + $latDelta,
        'min_lng' => $lng - $lngDelta,
        'max_lng' => $lng + $lngDelta,
    ];
}

function haversineKm(float $lat1, float $lng1, float $lat2, float $lng2): float
{
    $earthRadiusKm = 6371.0;
    $latDelta = deg2rad($lat2 - $lat1);
    $lngDelta = deg2rad($lng2 - $lng1);
    $a = sin($latDelta / 2) ** 2
        + cos(deg2rad($lat1)) * cos(deg2rad($lat2)) * sin($lngDelta / 2) ** 2;

    return $earthRadiusKm * 2 * asin(min(1.0, sqrt($a)));
}

/**
 * Resolve a city key to its cities-table row, or null when the key is empty or
 * unknown. Callers distinguish the two by also checking whether the key was
 * non-empty.
 *
 * This serves the account page's notification area, whose picker still selects
 * a whole city from the CLI's cache — that is the unit a subscription covers.
 * The dashboard's own location does not come through here: it is a label and a
 * point on the reader's filter row, which may be an address the cities table
 * has never heard of.
 */
function resolveCity(PDO $pdo, string $selectedCity): ?array
{
    if ($selectedCity === '') {
        return null;
    }
    $stmt = $pdo->prepare(
        <<<'SQL'
        SELECT normalized_name AS city_key, normalized_name AS city_name, display_name, lat, lng
        FROM cities
        WHERE normalized_name = :city_key
        LIMIT 1
        SQL
    );
    $stmt->bindValue(':city_key', $selectedCity);
    $stmt->execute();

    return $stmt->fetch() ?: null;
}

/**
 * Places matching a postal code, from the station addresses themselves.
 *
 * The cities table is names only — it is the CLI's cache of the places it
 * collects, and nothing in it is a postal code. But every station carries one,
 * so a reader who thinks of home as "10115" can be answered from the same
 * database the rest of the typeahead reads, without a geocoder.
 *
 * The prefix becomes a numeric range rather than a string comparison, because
 * post_code is an integer column and the two engines spell string casts
 * differently. German codes are five digits (the feed is German), so "101"
 * covers 10100 to 10199.
 *
 * The centre is the mean of the matching stations' coordinates: a postal code
 * is an area, and the middle of its filling stations is a better place to
 * measure a radius from than any one of them.
 *
 * @return array<int, array{label: string, lat: float, lng: float}>
 */
function postalCodeMatches(PDO $pdo, string $term): array
{
    $digits = trim($term);
    if ($digits === '' || !ctype_digit($digits) || strlen($digits) > 5) {
        return [];
    }
    $low = (int) str_pad($digits, 5, '0');
    $high = (int) str_pad($digits, 5, '9');

    $stmt = $pdo->prepare(
        <<<'SQL'
        SELECT s.post_code, TRIM(COALESCE(s.place, '')) AS place,
               AVG(s.lat) AS lat, AVG(s.lng) AS lng
        FROM stations s
        WHERE s.post_code BETWEEN :low AND :high
        GROUP BY s.post_code, TRIM(COALESCE(s.place, ''))
        ORDER BY s.post_code ASC
        LIMIT 10
        SQL
    );
    $stmt->bindValue(':low', $low, PDO::PARAM_INT);
    $stmt->bindValue(':high', $high, PDO::PARAM_INT);
    $stmt->execute();

    $matches = [];
    foreach ($stmt->fetchAll() as $row) {
        $code = trim((string) $row['post_code']);
        $place = trim((string) $row['place']);
        if ($code === '') {
            continue;
        }
        $matches[] = [
            'label' => trim($code . ' ' . $place),
            'lat' => (float) $row['lat'],
            'lng' => (float) $row['lng'],
        ];
    }

    return $matches;
}

/* ── Geocoding for the location filter ────────────────────────────────────────
 *
 * The typeahead answers from the database — cities the CLI has geocoded, postal
 * codes read off the station addresses — and nothing else, which is what keeps
 * a keystroke from becoming an HTTP request: Nominatim's usage policy rules out
 * autocomplete traffic outright.
 *
 * A street and house number is in neither source, so this is the path that asks
 * a geocoder, and only on an explicit act: picking the "search this address"
 * row of the dropdown, or pressing the locate button. One user action, one
 * request.
 *
 * What comes back is not written to the database here. It goes to the sidebar,
 * which posts it onto the reader's own filter row — a label and a point. The
 * `cities` table stays what the CLI made it: one row per place it collects,
 * with no reader's front door in it.
 */

/** User-Agent for the viewer's Nominatim calls; mirrors the CLI's --user-agent. */
function geocodeUserAgent(): string
{
    $agent = trim((string) getenv('GASOLINE_USER_AGENT'));

    return $agent !== '' ? $agent : 'gasoline-web/1.0 (viewer)';
}

/** False when the operator has switched the viewer's outbound lookups off. */
function geocodeEnabled(): bool
{
    $flag = strtolower(trim((string) getenv('GASOLINE_GEOCODE')));

    return !in_array($flag, ['0', 'false', 'off', 'no'], true);
}

/** Base URL of the Nominatim instance to ask, without a trailing slash. */
function geocodeBaseURL(): string
{
    $base = rtrim(trim((string) getenv('GASOLINE_NOMINATIM_URL')), '/');

    return $base !== '' ? $base : 'https://nominatim.openstreetmap.org';
}

/**
 * One Nominatim GET, decoded. Null on any failure — a viewer that cannot reach
 * the internet still has to render, so the caller degrades to "not found"
 * rather than to an error page.
 *
 * @param array<string, string> $query
 */
function geocodeRequest(string $path, array $query): mixed
{
    $url = geocodeBaseURL() . $path . '?' . http_build_query($query);
    $timeout = 6;
    $body = false;

    if (function_exists('curl_init')) {
        $curl = curl_init($url);
        curl_setopt_array($curl, [
            CURLOPT_RETURNTRANSFER => true,
            CURLOPT_USERAGENT => geocodeUserAgent(),
            CURLOPT_CONNECTTIMEOUT => $timeout,
            CURLOPT_TIMEOUT => $timeout,
            CURLOPT_FOLLOWLOCATION => false,
        ]);
        $response = curl_exec($curl);
        $status = (int) curl_getinfo($curl, CURLINFO_RESPONSE_CODE);
        curl_close($curl);
        if (is_string($response) && $status === 200) {
            $body = $response;
        }
    } else {
        // The http context's timeout covers the read, not opening the socket:
        // a host that cannot reach the geocoder at all would otherwise sit on
        // default_socket_timeout (60s by default) with the reader watching a
        // spinner. Narrow it for this one call and put it back afterwards.
        $context = stream_context_create(['http' => [
            'method' => 'GET',
            'header' => 'User-Agent: ' . geocodeUserAgent() . "\r\n",
            'timeout' => $timeout,
            'ignore_errors' => false,
        ]]);
        $previousSocketTimeout = ini_get('default_socket_timeout');
        ini_set('default_socket_timeout', (string) $timeout);
        try {
            $body = @file_get_contents($url, false, $context);
        } finally {
            if ($previousSocketTimeout !== false) {
                ini_set('default_socket_timeout', $previousSocketTimeout);
            }
        }
    }

    if (!is_string($body) || $body === '') {
        return null;
    }

    try {
        return json_decode($body, true, 32, JSON_THROW_ON_ERROR);
    } catch (Throwable $e) {
        return null;
    }
}

/**
 * Compact one Nominatim hit into the short label the cache stores.
 *
 * The CLI keeps display_name short (the place's own name), and the filter input
 * has to stay readable, so a full "5, Hauptstraße, Mitte, Berlin, 10115,
 * Deutschland" is folded back into "Hauptstraße 5, 10115 Berlin". Falls back to
 * the leading components of display_name when the structured parts are missing.
 *
 * The label is also the cache key, so it has to be derived deterministically:
 * the same address must fold to the same string on every lookup, or a second
 * search would store a duplicate row.
 *
 * @param array<string, mixed> $hit
 */
function geocodeLabel(array $hit): string
{
    $address = is_array($hit['address'] ?? null) ? $hit['address'] : [];
    $pick = static function (array $address, array $keys): string {
        foreach ($keys as $key) {
            $value = trim((string) ($address[$key] ?? ''));
            if ($value !== '') {
                return $value;
            }
        }
        return '';
    };

    $street = $pick($address, ['road', 'pedestrian', 'footway', 'residential', 'neighbourhood']);
    $houseNumber = $pick($address, ['house_number']);
    $place = $pick($address, ['city', 'town', 'village', 'municipality', 'suburb', 'county', 'state']);
    $postcode = $pick($address, ['postcode']);

    $line1 = trim($street . ($houseNumber !== '' ? ' ' . $houseNumber : ''));
    $line2 = trim($postcode . ($postcode !== '' && $place !== '' ? ' ' : '') . $place);
    $label = implode(', ', array_filter([$line1, $line2], static fn (string $part): bool => $part !== ''));

    if ($label === '') {
        // No structured address (a lake, a country, an instance that does not
        // return addressdetails): keep the first two components of the free
        // text, which is the place and what contains it.
        $parts = array_slice(array_map('trim', explode(',', (string) ($hit['display_name'] ?? ''))), 0, 2);
        $label = implode(', ', array_filter($parts, static fn (string $part): bool => $part !== ''));
    }
    if ($label === '') {
        $label = trim((string) ($hit['name'] ?? ''));
    }

    // cities.name is VARCHAR(255) and this label is the primary key.
    return mb_substr($label, 0, 200, 'UTF-8');
}

/**
 * Turn one raw Nominatim hit into the cache row shape, or null when it carries
 * no usable coordinates.
 *
 * @param array<string, mixed> $hit
 * @return array{label: string, lat: float, lng: float}|null
 */
function geocodeHit(array $hit): ?array
{
    $lat = $hit['lat'] ?? null;
    $lng = $hit['lon'] ?? null;
    if (!is_numeric($lat) || !is_numeric($lng)) {
        return null;
    }
    $label = geocodeLabel($hit);
    if ($label === '') {
        return null;
    }

    return ['label' => $label, 'lat' => (float) $lat, 'lng' => (float) $lng];
}

/**
 * Forward-geocode a free-form place or address.
 *
 * @return array{label: string, lat: float, lng: float}|null
 */
function geocodeSearch(string $query): ?array
{
    $results = geocodeRequest('/search', [
        'q' => $query,
        'format' => 'jsonv2',
        'limit' => '1',
        'addressdetails' => '1',
    ]);
    if (!is_array($results) || $results === [] || !is_array($results[0] ?? null)) {
        return null;
    }

    return geocodeHit($results[0]);
}

/**
 * Reverse-geocode a browser position. zoom=18 asks for building precision, so a
 * fix in a street returns that street rather than the whole district.
 *
 * @return array{label: string, lat: float, lng: float}|null
 */
function geocodeReverse(float $lat, float $lng): ?array
{
    $hit = geocodeRequest('/reverse', [
        'lat' => sprintf('%.6f', $lat),
        'lon' => sprintf('%.6f', $lng),
        'format' => 'jsonv2',
        'zoom' => '18',
        'addressdetails' => '1',
    ]);
    if (!is_array($hit) || isset($hit['error'])) {
        return null;
    }
    $resolved = geocodeHit($hit);
    if ($resolved === null) {
        return null;
    }

    // Keep the browser's own fix rather than the matched building's centroid:
    // the label describes where the reader is, the coordinates are where they
    // actually are, and the radius is measured from the latter.
    return ['label' => $resolved['label'], 'lat' => $lat, 'lng' => $lng];
}


/**
 * Load the stations in scope for the filter sidebar and data endpoint.
 * With a city: the stations inside the radius (bbox pre-filter + haversine),
 * sorted by distance. Without a city: every station, sorted by name.
 *
 * Both forms are restricted to stations still being fed
 * (GASOLINE_STATION_FRESHNESS_HOURS), so the dashboard covers the same station
 * universe as `gasoline suggest`. The station list, the snapshot rows and the
 * fill-up card all derive from this result, so this is the one place the rule
 * has to be applied.
 *
 * @return array{0: array<int, array<string, mixed>>, 1: array<string, float>}
 *   [$stations, $distancesById]
 */
function loadScopeStations(PDO $pdo, ?array $cityRow, int $radiusKm): array
{
    // EXISTS against idx_price_snapshots_station_recorded: one index seek per
    // candidate station, rather than an aggregate over the snapshot history.
    $fedRecently = <<<'SQL'
        EXISTS (
            SELECT 1
            FROM price_snapshots fresh
            WHERE fresh.station_id = s.id
              AND fresh.recorded_at >= :fresh_cutoff
        )
        SQL;

    if ($cityRow === null) {
        $stmt = $pdo->prepare(
            <<<SQL
            SELECT
                s.id,
                COALESCE(s.name_override, s.name) AS name,
                COALESCE(NULLIF(TRIM(s.brand), ''), '') AS brand,
                TRIM(COALESCE(s.street, '')) AS street,
                TRIM(COALESCE(s.house_number, '')) AS house_number,
                s.post_code,
                TRIM(COALESCE(s.place, '')) AS place,
                s.last_seen_at,
                s.lat,
                s.lng
            FROM stations s
            WHERE {$fedRecently}
            ORDER BY COALESCE(s.name_override, s.name) ASC, s.id ASC
            SQL
        );
        $stmt->bindValue(':fresh_cutoff', stationFreshnessCutoff());
        $stmt->execute();

        return [$stmt->fetchAll(), []];
    }

    $bbox = boundingBox((float) $cityRow['lat'], (float) $cityRow['lng'], $radiusKm);
    $stmt = $pdo->prepare(
        <<<SQL
        SELECT
            s.id,
            COALESCE(s.name_override, s.name) AS name,
            COALESCE(NULLIF(TRIM(s.brand), ''), '') AS brand,
            TRIM(COALESCE(s.street, '')) AS street,
            TRIM(COALESCE(s.house_number, '')) AS house_number,
            s.post_code,
            TRIM(COALESCE(s.place, '')) AS place,
            s.last_seen_at,
            s.lat,
            s.lng
        FROM stations s
        WHERE s.lat BETWEEN :min_lat AND :max_lat
          AND s.lng BETWEEN :min_lng AND :max_lng
          AND {$fedRecently}
        SQL
    );
    foreach ($bbox as $key => $value) {
        $stmt->bindValue(':' . $key, $value);
    }
    $stmt->bindValue(':fresh_cutoff', stationFreshnessCutoff());
    $stmt->execute();
    $candidateStations = $stmt->fetchAll();

    $stations = [];
    $distances = [];
    foreach ($candidateStations as $station) {
        $distKm = haversineKm(
            (float) $cityRow['lat'],
            (float) $cityRow['lng'],
            (float) $station['lat'],
            (float) $station['lng']
        );
        if ($distKm > $radiusKm) {
            continue;
        }
        $station['selected_dist_km'] = $distKm;
        $stations[] = $station;
        $distances[(string) $station['id']] = $distKm;
    }

    usort($stations, static function (array $left, array $right): int {
        $distCompare = ($left['selected_dist_km'] ?? INF) <=> ($right['selected_dist_km'] ?? INF);
        if ($distCompare !== 0) {
            return $distCompare;
        }
        $nameCompare = strcmp((string) $left['name'], (string) $right['name']);
        if ($nameCompare !== 0) {
            return $nameCompare;
        }
        return strcmp((string) $left['id'], (string) $right['id']);
    });

    return [$stations, $distances];
}

/**
 * The current price at each of the nearest stations, for the surroundings card.
 *
 * Deliberately not derived from the snapshot payload the chart is drawn from.
 * That one is cut by the date filter and by the station picker, and the card
 * answers a question neither of those applies to: what does fuel cost around
 * here, right now. So it reads the newest snapshot per station directly, for
 * every station the selected location and radius admit — the same scope list
 * the sidebar shows, which loadScopeStations has already sorted by distance.
 *
 * Bounded to the freshness window on both sides, which is what keeps it cheap.
 * The first shipped version correlated a MAX() against the outer row instead:
 * that reads every snapshot each station ever recorded and re-runs the
 * subquery for each one — 93,000 rows and 93,000 subquery executions for forty
 * stations on a database with 888,000 snapshots, which SQLite absorbed and
 * MySQL did not. Since loadScopeStations has already established that every
 * station here was fed inside the window, the newest snapshot is inside it too,
 * so the search never has to leave it: the grouped lookup reads one index range
 * per station and the join back is one seek per station.
 *
 * Prices are normalized to the raised-9 board style in the projection, exactly
 * like the snapshot query, so the card and the chart quote the same number.
 *
 * @param array<int, array<string, mixed>> $stations scope stations, nearest first
 * @param array<string, float> $distancesKm station id -> km from the location
 * @return array<int, array<string, mixed>> rows in distance order
 */
function loadNearbyPrices(PDO $pdo, array $stations, array $distancesKm, int $limit): array
{
    $stations = array_slice($stations, 0, max(0, $limit));
    if ($stations === []) {
        return [];
    }

    // Two parameter sets for one list of stations: the bound is repeated on the
    // outer query so that the work stays inside the freshness window whichever
    // side the engine decides to drive the join from, and a native prepared
    // statement cannot reuse one placeholder twice.
    $inner = [];
    $outer = [];
    $params = [];
    foreach ($stations as $index => $station) {
        $id = (string) $station['id'];
        $inner[] = ':nearby_inner_' . $index;
        $outer[] = ':nearby_outer_' . $index;
        $params[':nearby_inner_' . $index] = $id;
        $params[':nearby_outer_' . $index] = $id;
    }
    $params[':nearby_inner_cutoff'] = stationFreshnessCutoff();
    $params[':nearby_outer_cutoff'] = stationFreshnessCutoff();

    $e5 = raisedNinePriceSql('ps.e5');
    $e10 = raisedNinePriceSql('ps.e10');
    $diesel = raisedNinePriceSql('ps.diesel');
    $innerIn = implode(', ', $inner);
    $outerIn = implode(', ', $outer);
    $stmt = $pdo->prepare(
        <<<SQL
        SELECT
            ps.station_id,
            ps.recorded_at,
            ps.is_open,
            {$e5} AS e5,
            {$e10} AS e10,
            {$diesel} AS diesel
        FROM price_snapshots ps
        JOIN (
            SELECT station_id, MAX(recorded_at) AS newest_at
            FROM price_snapshots
            WHERE station_id IN ({$innerIn})
              AND recorded_at >= :nearby_inner_cutoff
            GROUP BY station_id
        ) newest
          ON newest.station_id = ps.station_id
         AND newest.newest_at = ps.recorded_at
        WHERE ps.station_id IN ({$outerIn})
          AND ps.recorded_at >= :nearby_outer_cutoff
        SQL
    );
    foreach ($params as $key => $value) {
        $stmt->bindValue($key, $value);
    }
    $stmt->execute();

    $latestById = [];
    foreach ($stmt->fetchAll() as $row) {
        $id = (string) $row['station_id'];
        // Two snapshots can carry the same recorded_at for one station (two
        // update targets covering it in the same sweep); either is that
        // station's current price, so the first one wins.
        if (isset($latestById[$id])) {
            continue;
        }
        $latestById[$id] = $row;
    }

    $rows = [];
    foreach ($stations as $station) {
        $id = (string) $station['id'];
        if (!isset($latestById[$id])) {
            // No snapshot inside the freshness window. loadScopeStations only
            // returns stations that have one, so rather than invent an empty
            // row, leave it out.
            continue;
        }
        $latest = $latestById[$id];
        $rows[] = [
            's' => $id,
            'dist' => isset($distancesKm[$id]) ? round($distancesKm[$id], 3) : null,
            't' => (string) $latest['recorded_at'],
            'o' => (int) $latest['is_open'] === 1,
            'e5' => $latest['e5'] === null ? null : (float) $latest['e5'],
            'e10' => $latest['e10'] === null ? null : (float) $latest['e10'],
            'diesel' => $latest['diesel'] === null ? null : (float) $latest['diesel'],
        ];
    }

    return $rows;
}

/* ── Raised-9 price normalization ──────────────────────────────────
   German pump boards end every price in a raised 9 (1,89⁹), but the feed
   records whatever milli digit a station happened to report (1.891,
   2.137, …). The dashboard shows board prices only, so every price is
   normalized to the board style — keep the cents, force the
   tenth-of-a-cent digit to 9 — before it reaches the client.

   Snapshot rows are normalized inside the SQL projection (the history
   query returns thousands of rows, so a per-row PHP pass would be pure
   overhead); predicted prices are normalized in PHP instead, AFTER the
   winner's-curse display correction is added, so that calculation keeps
   operating on the raw model price (see loadFilteredPredictions).

   Both flavours implement the same rule: round to milli precision to
   shake off float noise, drop the milli digit, add 9. NULL and
   non-positive prices pass through untouched.
   ──────────────────────────────────────────────────────────────── */

/**
 * SQL expression normalizing $column to the raised-9 board price.
 * ROUND, % and / behave identically here on SQLite and MySQL (unlike
 * FLOOR/TRUNCATE, which SQLite only has with the optional math extension,
 * or CAST AS INTEGER, which MySQL rounds instead of truncating).
 */
function raisedNinePriceSql(string $column): string
{
    $milli = "ROUND({$column} * 1000)";
    return "CASE WHEN {$column} > 0 THEN ({$milli} - {$milli} % 10 + 9) / 1000.0 ELSE {$column} END";
}

/**
 * PHP twin of raisedNinePriceSql() for prices assembled after the query.
 */
function raisedNinePrice(float $price): float
{
    if ($price <= 0.0) {
        return $price;
    }
    $milli = (int) round($price * 1000);
    return ($milli - $milli % 10 + 9) / 1000.0;
}

/**
 * Assemble the price-snapshot query for the active filters.
 * Station metadata is intentionally NOT joined in — the client joins rows to the
 * separately-sent station map — so the row payload stays small.
 * Prices are normalized to the raised-9 board style in the projection.
 *
 * @return array{0: string, 1: array<string, mixed>, 2: bool, 3: array<int, array<string, mixed>>}
 *   [$sql, $params, $shouldRun, $errors]
 */
function buildSnapshotQuery(
    ?array $cityRow,
    array $stations,
    array $selectedStationIds,
    string $fromDate,
    string $toDate
): array {
    $where = [];
    $params = [];
    $shouldRun = true;
    $errors = [];

    if ($fromDate !== '') {
        $from = DateTimeImmutable::createFromFormat('Y-m-d', $fromDate, new DateTimeZone('UTC'));
        if ($from === false) {
            $errors[] = ['key' => 'invalidFromDate', 'params' => [], 'message' => 'Invalid from date.'];
        } else {
            $where[] = 'ps.recorded_at >= :from_recorded_at';
            $params[':from_recorded_at'] = $from->setTime(0, 0, 0)->format(DateTimeInterface::RFC3339);
        }
    }

    if ($toDate !== '') {
        $to = DateTimeImmutable::createFromFormat('Y-m-d', $toDate, new DateTimeZone('UTC'));
        if ($to === false) {
            $errors[] = ['key' => 'invalidToDate', 'params' => [], 'message' => 'Invalid to date.'];
        } else {
            $where[] = 'ps.recorded_at <= :to_recorded_at';
            $params[':to_recorded_at'] = $to->setTime(23, 59, 59)->format(DateTimeInterface::RFC3339);
        }
    }

    if ($cityRow !== null) {
        $effectiveStationIds = array_column($stations, 'id');
        if ($selectedStationIds !== []) {
            $effectiveStationIds = array_values(array_intersect($effectiveStationIds, $selectedStationIds));
        }

        if ($effectiveStationIds === []) {
            $shouldRun = false;
        } else {
            $placeholders = [];
            foreach ($effectiveStationIds as $index => $stationId) {
                $placeholder = ':station_scope_id_' . $index;
                $placeholders[] = $placeholder;
                $params[$placeholder] = $stationId;
            }
            $where[] = 'ps.station_id IN (' . implode(', ', $placeholders) . ')';
        }
    }

    if ($cityRow === null && $selectedStationIds !== []) {
        $placeholders = [];
        foreach ($selectedStationIds as $index => $stationId) {
            $placeholder = ':station_id_' . $index;
            $placeholders[] = $placeholder;
            $params[$placeholder] = $stationId;
        }
        $where[] = 'ps.station_id IN (' . implode(', ', $placeholders) . ')';
    }

    // Without a city or an explicit station selection there is no scope to
    // display, so skip the snapshot query instead of loading every station's
    // full history for the default date range.
    if ($cityRow === null && $selectedStationIds === []) {
        $shouldRun = false;
    }

    $e5 = raisedNinePriceSql('ps.e5');
    $e10 = raisedNinePriceSql('ps.e10');
    $diesel = raisedNinePriceSql('ps.diesel');
    $sql = <<<SQL
        SELECT
            ps.station_id,
            ps.recorded_at,
            ps.is_open,
            {$e5} AS e5,
            {$e10} AS e10,
            {$diesel} AS diesel
        FROM price_snapshots ps
        SQL;

    if ($where !== []) {
        $sql .= "\nWHERE " . implode("\n  AND ", $where);
    }

    $sql .= "\nORDER BY ps.recorded_at ASC, ps.station_id ASC";

    return [$sql, $params, $shouldRun, $errors];
}

/**
 * Pick the upcoming fill-up windows for the in-scope stations.
 *
 * Since prediction runs went global, the stored is_suggestion flag marks the
 * per-run *globally* cheapest windows across every station being fed — for a
 * dashboard filtered to one area those flags almost never land in scope, so
 * they cannot drive this card. Instead the windows are picked here, from the
 * newest run's full forecast grid restricted to exactly the stations in
 * scope, mirroring the notifier's per-area picker (notify.go
 * collectSuggestions → generateSuggestions → the medium/high filter): per
 * fuel and local day the cheapest hour windows are selected — ordered by
 * price, then confidence, then distance, then start — up to the notifier's
 * per-day limit, a window less than two hours from an already selected
 * window at the same station is skipped as a duplicate, equal-priced picks
 * of one station merge into a single span, and only medium/high confidence
 * survives. Displayed prices carry the run's recorded suggestion display
 * correction (prediction_runs.suggestion_bias) on top of the raw grid price,
 * again like the notifier, and are then normalized to the raised-9 board
 * style — ordering stays on the raw price. The result is
 * what a subscriber to this area would be sent. It never triggers a suggest
 * run.
 *
 * Later runs supersede earlier ones for the same target hour, so only the
 * newest run per (station, fuel) is considered; a station covered by several
 * runs resolves independently via its own newest run.
 *
 * That newest run is resolved from the candidate rows themselves rather than by
 * a second query. Asking the database for it separately meant an aggregate over
 * every prediction the scope had ever accumulated — bounded by station and fuel
 * but not in time — which `gasoline doctor dashboard` measured at 158 seconds
 * against 612,665 rows on a production database, to produce 23 rows of which
 * only the newest run's were used. The candidates below are already the newest
 * run's rows plus its predecessors', so the maximum run_id among them is the
 * same answer for one pass instead of two.
 *
 * The one case where that differs: a station the newest run stored nothing
 * future for (too little history to forecast it, say) used to show nothing at
 * all, because its rows all belonged to a superseded run. It now shows the most
 * recent run that did cover it, which is the more useful of the two answers.
 * Runs are compared by run_id rather than by run_at, so two runs recorded in the
 * same second no longer collide.
 *
 * @param array<int, string> $stationIds effective in-scope station ids
 * @param array<string, float> $distancesKm station id -> km from the selected city
 * @return array{rows: array<int, array<string, mixed>>, as_of: array<string, string>}
 *   rows sorted by window start then station id; as_of maps fuel -> newest run_at
 */
function loadFilteredPredictions(PDO $pdo, array $stationIds, array $distancesKm, string $selectedFuel, string $nowUtc): array
{
    $empty = ['rows' => [], 'as_of' => []];
    if ($stationIds === []) {
        return $empty;
    }

    $fuels = $selectedFuel === 'all' ? ['e5', 'e10', 'diesel'] : [$selectedFuel];

    // Shared IN(...) placeholders for both queries.
    $scopeParams = [];
    $stationPlaceholders = [];
    foreach (array_values($stationIds) as $index => $stationId) {
        $placeholder = ':pred_station_' . $index;
        $stationPlaceholders[] = $placeholder;
        $scopeParams[$placeholder] = $stationId;
    }
    $fuelPlaceholders = [];
    foreach (array_values($fuels) as $index => $fuel) {
        $placeholder = ':pred_fuel_' . $index;
        $fuelPlaceholders[] = $placeholder;
        $scopeParams[$placeholder] = $fuel;
    }
    $stationIn = implode(', ', $stationPlaceholders);
    $fuelIn = implode(', ', $fuelPlaceholders);

    // Candidate windows: every future window the scope has, across whichever
    // runs produced them. Confidence is deliberately not filtered here — the
    // notifier picks the cheapest windows first and only then drops low
    // confidence, so a cheap low-confidence window consumes a slot without
    // being shown.
    $rowStmt = $pdo->prepare(
        'SELECT pp.station_id, pp.fuel, pp.run_id, pp.target_start, pp.target_end, '
        . 'pp.predicted_price, pp.confidence, pr.run_at, pr.suggestion_bias '
        . 'FROM price_predictions pp '
        . 'JOIN prediction_runs pr ON pr.id = pp.run_id '
        . 'WHERE pp.station_id IN (' . $stationIn . ') '
        . 'AND pp.fuel IN (' . $fuelIn . ') '
        . 'AND pp.target_start > :pred_now '
        . 'ORDER BY pp.target_start ASC, pp.station_id ASC'
    );
    foreach ($scopeParams as $key => $value) {
        $rowStmt->bindValue($key, $value);
    }
    $rowStmt->bindValue(':pred_now', $nowUtc);
    $rowStmt->execute();
    $candidateRows = $rowStmt->fetchAll();

    // First pass: the newest run per (station, fuel) among these candidates,
    // which is the run whose picks we display. run_id ordering is insert
    // ordering, so the largest is the newest.
    $latestRunByKey = [];   // "station|fuel" -> newest run_id
    $runAtById = [];        // run_id -> run_at, for the "as of" stamp
    foreach ($candidateRows as $row) {
        $runId = (int) $row['run_id'];
        $runAtById[$runId] = (string) $row['run_at'];
        $key = (string) $row['station_id'] . '|' . (string) $row['fuel'];
        if (!isset($latestRunByKey[$key]) || $runId > $latestRunByKey[$key]) {
            $latestRunByKey[$key] = $runId;
        }
    }
    if ($latestRunByKey === []) {
        return $empty;
    }

    $asOf = [];             // fuel -> newest run_at across the scope
    foreach ($latestRunByKey as $key => $runId) {
        $fuel = substr($key, strpos($key, '|') + 1);
        $runAt = $runAtById[$runId] ?? '';
        if ($runAt !== '' && (!isset($asOf[$fuel]) || strcmp($runAt, $asOf[$fuel]) > 0)) {
            $asOf[$fuel] = $runAt;
        }
    }

    // Bucket candidates per fuel and local day, like generateSuggestions does
    // with the server's local time (the Go side groups by opts.Location, which
    // is the server timezone there too).
    $localZone = new DateTimeZone(date_default_timezone_get());
    $byFuelDate = [];
    foreach ($candidateRows as $row) {
        $stationId = (string) $row['station_id'];
        $fuel = (string) $row['fuel'];
        // Keep only rows from that station/fuel's newest run — older runs are
        // history whose picks the newest run has already superseded.
        $key = $stationId . '|' . $fuel;
        if (!isset($latestRunByKey[$key]) || (int) $row['run_id'] !== $latestRunByKey[$key]) {
            continue;
        }
        $start = new DateTimeImmutable((string) $row['target_start']);
        $localDate = $start->setTimezone($localZone)->format('Y-m-d');
        // The displayed price carries the run's suggestion display correction
        // (the measured winner's curse of picking the cheapest window), so the
        // card quotes the same price a notification would, and is then
        // normalized to the raised-9 board style — the correction has to be
        // applied to the raw model price first, so it is not folded into the
        // SQL projection like the snapshot prices are. Ordering below stays
        // on the raw model price, exactly like the Go picker — the grid
        // keeps storing raw prices, so the learning never sees the correction.
        $rawPrice = (float) $row['predicted_price'];
        $byFuelDate[$fuel][$localDate][] = [
            's' => $stationId,
            'fuel' => $fuel,
            'start' => (string) $row['target_start'],
            'end' => (string) $row['target_end'],
            'price' => raisedNinePrice($rawPrice + (float) $row['suggestion_bias']),
            'conf' => (string) $row['confidence'],
            'raw' => $rawPrice,
            'ts' => $start->getTimestamp(),
        ];
    }

    // suggestLimitPerDay and the two-hour duplicate window, mirrored from the
    // Go picker (generateSuggestions / duplicatesNearbyStationWindow).
    $limitPerDay = 3;
    $dupWindowSeconds = 2 * 3600;
    $confidenceRank = static function (string $confidence): int {
        return $confidence === 'high' ? 3 : ($confidence === 'medium' ? 2 : 1);
    };

    $rows = [];
    foreach ($byFuelDate as $byDate) {
        foreach ($byDate as $candidates) {
            // suggestionCandidateLess: raw price, confidence, distance
            // (rounded to 0.1 km like the Go side reports it), start, then
            // station id for determinism.
            usort($candidates, static function (array $a, array $b) use ($confidenceRank, $distancesKm): int {
                return ($a['raw'] <=> $b['raw'])
                    ?: ($confidenceRank($b['conf']) <=> $confidenceRank($a['conf']))
                    ?: (round($distancesKm[$a['s']] ?? INF, 1) <=> round($distancesKm[$b['s']] ?? INF, 1))
                    ?: ($a['ts'] <=> $b['ts'])
                    ?: strcmp($a['s'], $b['s']);
            });

            $selected = [];
            foreach ($candidates as $candidate) {
                $duplicate = false;
                foreach ($selected as $existing) {
                    if ($existing['s'] === $candidate['s'] && abs($candidate['ts'] - $existing['ts']) < $dupWindowSeconds) {
                        $duplicate = true;
                        break;
                    }
                }
                if ($duplicate) {
                    continue;
                }
                $selected[] = $candidate;
                if (count($selected) === $limitPerDay) {
                    break;
                }
            }

            // The notifier's confidence filter, then mergeSuggestions: picks of
            // one station with the same cent-rounded display price and
            // confidence collapse into one span, matching the Go side's
            // compare of rounded display prices.
            $merged = [];
            foreach ($selected as $candidate) {
                if ($candidate['conf'] !== 'medium' && $candidate['conf'] !== 'high') {
                    continue;
                }
                $mergeKey = $candidate['s'] . '|' . number_format($candidate['price'], 2, '.', '') . '|' . $candidate['conf'];
                if (isset($merged[$mergeKey])) {
                    if (strcmp($candidate['end'], $merged[$mergeKey]['end']) > 0) {
                        $merged[$mergeKey]['end'] = $candidate['end'];
                    }
                    if (strcmp($candidate['start'], $merged[$mergeKey]['start']) < 0) {
                        $merged[$mergeKey]['start'] = $candidate['start'];
                    }
                    continue;
                }
                $merged[$mergeKey] = $candidate;
            }
            foreach ($merged as $pick) {
                unset($pick['ts'], $pick['raw']);
                $rows[] = $pick;
            }
        }
    }

    usort($rows, static function (array $a, array $b): int {
        return strcmp($a['start'], $b['start']) ?: strcmp($a['s'], $b['s']);
    });

    return ['rows' => $rows, 'as_of' => $asOf];
}

// ── AJAX: async snapshot data ─────────────────────────────────────────────────
// The page renders a fast shell; the heavy snapshot payload is fetched here and
// rendered client-side. Station metadata is sent once (keyed by id) and rows
// omit the repeated name/brand/street/place strings.
if (isset($_GET['action']) && $_GET['action'] === 'data') {
    header('Content-Type: application/json; charset=utf-8');
    $jsonFlags = JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE | JSON_THROW_ON_ERROR;
    $out = [
        'summary' => ['points' => 0, 'stations' => 0, 'first_recorded_at' => null, 'last_recorded_at' => null],
        'stations' => [],
        'rows' => [],
        'predictions' => [],
        'predictions_as_of' => [],
        'nearby' => [],
        'nearby_total' => 0,
        'errors' => $errors,
    ];

    if ($errors !== []) {
        echo json_encode($out, $jsonFlags);
        exit;
    }

    try {
        $pdo = gasolineConnect($dbDriver, $dbPath);
        $cityRow = $selectedCityRow;

        [$stations, $distances] = loadScopeStations($pdo, $cityRow, $selectedRadiusKm);

        $metaById = [];
        foreach ($stations as $station) {
            $id = (string) $station['id'];
            $metaById[$id] = [
                'name' => (string) $station['name'],
                'brand' => (string) $station['brand'],
                'street' => trim(implode(' ', array_filter([
                    (string) $station['street'],
                    (string) $station['house_number'],
                ]))),
                'place' => trim((string) ($station['place'] ?? '')),
                'zip' => ($station['post_code'] ?? null) !== null ? (string) $station['post_code'] : '',
                // Coordinates power the client-side Google Maps navigation link
                // in the station detail dialog (no persisted link needed).
                'lat' => isset($station['lat']) ? (float) $station['lat'] : null,
                'lng' => isset($station['lng']) ? (float) $station['lng'] : null,
                'dist' => isset($distances[$id]) ? round($distances[$id], 3) : null,
            ];
        }

        // Surroundings card: the stations around the selected location with
        // their current price, independent of the date range and the station
        // picker. Only with a location — without one there is no "around here"
        // and $stations is every station being fed.
        if ($cityRow !== null) {
            $out['nearby_total'] = count($stations);
            $out['nearby'] = loadNearbyPrices($pdo, $stations, $distances, NEARBY_STATION_LIMIT);
            // Its stations are not the chart's: the date range or the picker
            // can leave them out of the snapshot rows entirely, and without
            // their metadata the card would list bare station ids.
            foreach ($out['nearby'] as $nearbyRow) {
                $nearbyId = (string) $nearbyRow['s'];
                if (isset($metaById[$nearbyId])) {
                    $out['stations'][$nearbyId] = $metaById[$nearbyId];
                }
            }
        }

        [$sql, $params, $shouldRun, $queryErrors] = buildSnapshotQuery(
            $cityRow,
            $stations,
            $selectedStationIds,
            $fromDate,
            $toDate
        );
        foreach ($queryErrors as $queryError) {
            $out['errors'][] = $queryError;
        }

        if ($out['errors'] === [] && $shouldRun) {
            $statement = $pdo->prepare($sql);
            foreach ($params as $key => $value) {
                $statement->bindValue($key, $value);
            }
            $statement->execute();
            $rawRows = $statement->fetchAll();

            // For a city scope the meaningful tie-break among equal timestamps is
            // proximity to the city centre; mirror the previous server ordering.
            if ($cityRow !== null) {
                usort($rawRows, static function (array $left, array $right) use ($distances, $metaById): int {
                    $timeCompare = strcmp((string) $left['recorded_at'], (string) $right['recorded_at']);
                    if ($timeCompare !== 0) {
                        return $timeCompare;
                    }
                    $leftId = (string) $left['station_id'];
                    $rightId = (string) $right['station_id'];
                    $distCompare = (($distances[$leftId] ?? INF) <=> ($distances[$rightId] ?? INF));
                    if ($distCompare !== 0) {
                        return $distCompare;
                    }
                    // Preserve the previous name tie-break (metadata is already loaded).
                    $nameCompare = strcmp(
                        (string) ($metaById[$leftId]['name'] ?? ''),
                        (string) ($metaById[$rightId]['name'] ?? '')
                    );
                    if ($nameCompare !== 0) {
                        return $nameCompare;
                    }
                    return strcmp($leftId, $rightId);
                });
            }

            $usedStationIds = [];
            foreach ($rawRows as $row) {
                $id = (string) $row['station_id'];
                $usedStationIds[$id] = true;
                $out['rows'][] = [
                    's' => $id,
                    't' => (string) $row['recorded_at'],
                    'o' => (int) $row['is_open'],
                    'e5' => $row['e5'] !== null ? (float) $row['e5'] : null,
                    'e10' => $row['e10'] !== null ? (float) $row['e10'] : null,
                    'diesel' => $row['diesel'] !== null ? (float) $row['diesel'] : null,
                ];
            }

            foreach (array_keys($usedStationIds) as $id) {
                if (isset($metaById[$id])) {
                    $out['stations'][$id] = $metaById[$id];
                }
            }

            if ($out['rows'] !== []) {
                $out['summary']['points'] = count($out['rows']);
                $out['summary']['stations'] = count($usedStationIds);
                $out['summary']['first_recorded_at'] = $out['rows'][0]['t'];
                $out['summary']['last_recorded_at'] = $out['rows'][count($out['rows']) - 1]['t'];
            }
        }

        // Upcoming, notification-worthy predictions for the same in-scope
        // stations (mirrors the snapshot scoping in buildSnapshotQuery). Read
        // only — no suggest run is triggered.
        if ($out['errors'] === []) {
            $predictionStationIds = [];
            if ($cityRow !== null) {
                $predictionStationIds = array_column($stations, 'id');
                if ($selectedStationIds !== []) {
                    $predictionStationIds = array_values(array_intersect($predictionStationIds, $selectedStationIds));
                }
            } elseif ($selectedStationIds !== []) {
                $predictionStationIds = $selectedStationIds;
            }

            if ($predictionStationIds !== []) {
                $nowUtc = (new DateTimeImmutable('now', new DateTimeZone('UTC')))->format(DateTimeInterface::RFC3339);
                $predictions = loadFilteredPredictions($pdo, $predictionStationIds, $distances, $selectedFuel, $nowUtc);
                $out['predictions'] = $predictions['rows'];
                $out['predictions_as_of'] = $predictions['as_of'];
                // Predicted stations may have no snapshot in the current date
                // range, so ensure their metadata (name/address) is sent too.
                foreach ($predictions['rows'] as $prediction) {
                    $predictedId = (string) $prediction['s'];
                    if (isset($metaById[$predictedId]) && !isset($out['stations'][$predictedId])) {
                        $out['stations'][$predictedId] = $metaById[$predictedId];
                    }
                }
            }
        }
    } catch (Throwable $e) {
        // Never leak the raw message (DB host, DSN, paths, SQL) to the client.
        error_log('gasoline data endpoint error: ' . $e->getMessage());
        $out['errors'][] = ['key' => 'loadError', 'params' => [], 'message' => 'Could not load data.'];
    }

    echo json_encode($out, $jsonFlags);
    exit;
}

if ($errors === []) {
    try {
        $pdo = gasolineConnect($dbDriver, $dbPath);

        // The location came out of the filter row already resolved, so the
        // shell only has the station list left to load for the sidebar. The
        // heavy snapshot payload is fetched asynchronously via ?action=data.
        // Distances are only needed client-side (sent with that payload).
        [$stations] = loadScopeStations($pdo, $selectedCityRow, $selectedRadiusKm);
    } catch (Throwable $e) {
        // Never leak the raw message (DB host, DSN, paths, SQL) to the client.
        error_log('gasoline shell error: ' . $e->getMessage());
        $errors[] = [
            'key' => 'loadError',
            'params' => [],
            'message' => 'Could not load data.',
        ];
    }
}

function h(?string $value): string
{
    return htmlspecialchars((string) $value, ENT_QUOTES, 'UTF-8');
}

/**
 * Label for a station in the picker. The distance is optional because the
 * client re-renders it on a language switch (German writes 3,9 km, English
 * 3.9 km) and needs the label without it as the stable base.
 */
function stationLabel(array $station, bool $withDistance = true): string
{
    $name = trim($station['name']);

    $place = trim($station['place'] ?? '');

    $dist = '';
    $selectedDistKm = $station['selected_dist_km'] ?? null;
    if ($withDistance && $selectedDistKm !== null) {
        $dist = number_format((float) $selectedDistKm, 1) . ' km';
    }

    $suffix = implode(' ', array_filter([$place, $dist !== '' ? "({$dist})" : '']));

    return $suffix !== '' ? "{$name}, {$suffix}" : $name;
}

function stationExists(PDO $pdo, string $stationId): bool
{
    $stmt = $pdo->prepare('SELECT 1 FROM stations WHERE id = :id');
    $stmt->bindValue(':id', $stationId);
    $stmt->execute();
    return $stmt->fetch() !== false;
}

/** "Street 5, 12345 Place" from a stations row; empty parts are dropped. */
function stationAddress(array $station): string
{
    // Explicit callback: array_filter's default would also drop "0",
    // discarding e.g. a legitimate house number 0.
    $nonEmpty = static fn (string $value): bool => $value !== '';
    $street = trim(implode(' ', array_filter([
        trim((string) ($station['street'] ?? '')),
        trim((string) ($station['house_number'] ?? '')),
    ], $nonEmpty)));
    $town = trim(implode(' ', array_filter([
        trim((string) ($station['post_code'] ?? '')),
        trim((string) ($station['place'] ?? '')),
    ], $nonEmpty)));

    return implode(', ', array_filter([$street, $town], $nonEmpty));
}

// renderDocumentHead emits everything from <!doctype> through </head> —
// shared by the dashboard and all auth/account/admin pages.
function renderDocumentHead(string $titleSuffix): void
{
?>
<!doctype html>
<html lang="en">
<head>
    <meta charset="utf-8">
    <meta name="viewport" content="width=device-width, initial-scale=1">
    <title>Gasoline — <?= h($titleSuffix) ?></title>
    <link rel="icon" type="image/png" sizes="32x32" href="favicon-32.png">
    <link rel="icon" type="image/png" sizes="192x192" href="favicon-192.png">
    <link rel="apple-touch-icon" href="apple-touch-icon.png">
    <script>
        (function () {
            const t = localStorage.getItem('theme') ||
                (window.matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark');
            document.documentElement.setAttribute('data-theme', t);
        })();
    </script>
    <link rel="preconnect" href="https://fonts.googleapis.com">
    <link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
    <link href="https://fonts.googleapis.com/css2?family=Space+Grotesk:wght@400;500;700&family=Quicksand:wght@700&family=DM+Mono:wght@400;500&display=swap" rel="stylesheet">
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
            --sans:        'Space Grotesk', system-ui, sans-serif;
            --wm-shadow:   rgba(0,0,0,0.6);
            --wm-shadow-x: 1.3px;
            --wm-shadow-y: 1.6px;
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
            /* Clamp the single implicit track to the container so wide
               content (tables, template strings) scrolls inside its own
               overflow container instead of stretching the whole page. */
            grid-template-columns: minmax(0, 1fr);
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

        a.brand {
            display: flex;
            align-items: center;
            gap: 0.6rem;
            text-decoration: none;
            color: inherit;
            border-radius: 16px;
        }

        a.brand:focus-visible {
            outline: 2px solid var(--amber);
            outline-offset: 5px;
        }

        .brand-icon {
            width: 54px;
            height: 54px;
            display: grid;
            place-items: center;
            flex-shrink: 0;
            transition: transform 0.3s cubic-bezier(0.34, 1.56, 0.64, 1);
        }

        .brand-icon img {
            width: 54px;
            height: 54px;
            object-fit: contain;
            display: block;
            /* same sharp offset shadow as the wordmark, traced along the artwork */
            filter: drop-shadow(var(--wm-shadow-x) var(--wm-shadow-y) 0 var(--wm-shadow));
        }

        html[data-theme="light"] .logo-dark { display: none; }
        html:not([data-theme="light"]) .logo-light { display: none; }

        .brand:hover .brand-icon {
            transform: translateY(-2px) rotate(-4deg) scale(1.05);
        }

        @media (prefers-reduced-motion: reduce) {
            .brand-icon { transition: none; }
            .brand:hover .brand-icon { transform: none; }
        }

        h1 {
            font-family: 'Quicksand', var(--sans);
            font-size: clamp(1.6rem, 3vw, 2.4rem);
            font-weight: 700;
            letter-spacing: -0.015em;
            line-height: 1;
            color: var(--ink);
            /* sharp offset shadow; the gradient clip lives on the inner
               .wm span so the filter never touches clipped text directly */
            filter: drop-shadow(var(--wm-shadow-x) var(--wm-shadow-y) 0 var(--wm-shadow));
        }

        h1 .wm {
            background: linear-gradient(180deg, var(--ink) 55%, var(--muted) 145%);
            -webkit-background-clip: text;
            background-clip: text;
            -webkit-text-fill-color: transparent;
        }

        /* The hover drop-shadow lives on the em while the gradient clip lives
           on a nested span: combining filter with background-clip:text on the
           same element breaks rendering in WebKit/Blink. */
        h1 em {
            font-style: normal;
            transition: filter 0.3s ease;
        }

        h1 em span {
            background: linear-gradient(180deg, #ffd27a 0%, var(--amber) 55%, #dd8a06 100%);
            -webkit-background-clip: text;
            background-clip: text;
            -webkit-text-fill-color: transparent;
            color: var(--amber);
        }

        .brand:hover h1 em {
            filter: drop-shadow(0 0 10px rgba(245,166,35,0.55));
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

        /* Collapse affordance, only shown in the mobile layout */
        .sidebar-chevron {
            display: none;
            margin-left: auto;
            color: var(--muted);
            line-height: 0;
        }

        .sidebar-chevron svg { transition: transform 0.2s; }

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

        .field input:not([type="checkbox"]),
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

        .field input:not([type="checkbox"]):focus,
        .field select:focus {
            border-color: var(--amber);
            box-shadow: 0 0 0 3px var(--amber-dim);
        }

        /* ── Checkboxes ────────────────────────────────────────── */
        input[type="checkbox"] {
            appearance: none;
            width: 18px;
            height: 18px;
            flex-shrink: 0;
            margin: 0;
            border: 1px solid var(--border-hi);
            border-radius: 5px;
            background: var(--surface-hi);
            cursor: pointer;
            display: inline-grid;
            place-items: center;
            transition: background 0.15s, border-color 0.15s;
        }

        input[type="checkbox"]::after {
            content: "";
            width: 10px;
            height: 10px;
            background: #0d0e11; /* dark check on the amber fill in both themes */
            clip-path: polygon(14% 44%, 0 65%, 50% 100%, 100% 16%, 82% 2%, 43% 66%);
            transform: scale(0);
            transition: transform 0.12s;
        }

        input[type="checkbox"]:checked {
            background: var(--amber);
            border-color: var(--amber);
        }

        input[type="checkbox"]:checked::after { transform: scale(1); }

        input[type="checkbox"]:focus-visible {
            outline: none;
            border-color: var(--amber);
            box-shadow: 0 0 0 3px var(--amber-dim);
        }

        /* ── Station picker (filterable checkbox list) ─────────── */
        .station-picker {
            background: var(--surface-hi);
            border: 1px solid var(--border-hi);
            border-radius: 8px;
            overflow: hidden;
            transition: border-color 0.15s;
        }

        .station-picker:focus-within {
            border-color: var(--amber);
            box-shadow: 0 0 0 3px var(--amber-dim);
        }

        /* Overrides the generic .field input skin: the filter box is a row
           inside the picker frame, not a standalone control. */
        .field .station-filter-input {
            background: transparent;
            border: none;
            border-bottom: 1px solid var(--border-hi);
            border-radius: 0;
            padding: 0.55rem 0.8rem;
        }

        .field .station-filter-input:focus {
            border-color: var(--border-hi);
            box-shadow: none;
            outline: none;
        }

        .station-options {
            max-height: 13rem;
            overflow-y: auto;
            padding: 0.4rem;
        }

        .station-option {
            display: flex;
            align-items: center;
            gap: 0.55rem;
            padding: 0.35rem 0.5rem;
            border-radius: 5px;
            cursor: pointer;
            font-size: 0.85rem;
            color: var(--ink);
        }

        .station-option:hover { background: var(--amber-dim); }

        .station-option:has(input:checked) { color: var(--amber); }

        /* The filter hides non-matching rows; checked state lives on the
           (still-submitting) hidden checkboxes. Class instead of [hidden]
           because the row's display:flex would win over the UA rule. */
        .station-option.filtered-out { display: none; }

        .station-option-label { min-width: 0; overflow-wrap: anywhere; }

        /* ── Location filter (city / address typeahead + locate) ───── */
        .city-ac { position: relative; }

        /* Location field: the text box and the locate button share a row, so the
           dropdown underneath still spans the whole sidebar width. */
        .loc-row {
            display: flex;
            gap: 0.4rem;
            align-items: stretch;
        }

        .loc-row .city-ac-input {
            flex: 1;
            min-width: 0;
        }

        .loc-gps {
            flex: 0 0 auto;
            display: inline-flex;
            align-items: center;
            justify-content: center;
            width: 2.5rem;
            padding: 0;
            background: var(--surface-hi);
            border: 1px solid var(--border-hi);
            border-radius: 8px;
            color: var(--muted);
            cursor: pointer;
            transition: border-color 0.15s, color 0.15s;
        }

        .loc-gps:hover:not(:disabled) { border-color: var(--amber); color: var(--amber); }
        .loc-gps:disabled { cursor: progress; color: var(--amber); }
        .loc-gps:disabled svg { animation: spin 1.1s linear infinite; }

        /* The dropdown row that spends a Nominatim lookup. Set apart from the
           cached matches above it, because it is the one that leaves the host. */
        .city-ac-search {
            display: flex;
            align-items: center;
            gap: 0.45rem;
            color: var(--amber);
        }

        .city-ac-search svg { flex-shrink: 0; }
        .city-ac-item + .city-ac-search { border-top: 1px solid var(--border); margin-top: 0.2rem; padding-top: 0.55rem; }
        .city-ac-empty.is-error { color: var(--red); }

        .city-ac-input {
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

        .city-ac-input:focus {
            border-color: var(--amber);
            box-shadow: 0 0 0 3px var(--amber-dim);
        }

        .city-ac-list {
            position: absolute;
            top: calc(100% + 4px);
            left: 0;
            right: 0;
            background: var(--surface);
            border: 1px solid var(--border-hi);
            border-radius: 8px;
            list-style: none;
            padding: 0.3rem;
            margin: 0;
            z-index: 200;
            max-height: 14rem;
            overflow-y: auto;
            box-shadow: 0 8px 24px rgba(0, 0, 0, 0.35);
            scrollbar-width: thin;
        }

        .city-ac-list[hidden] { display: none; }

        .city-ac-item {
            padding: 0.48rem 0.6rem;
            border-radius: 5px;
            cursor: pointer;
            font-family: var(--mono);
            font-size: 0.82rem;
            color: var(--ink);
            white-space: nowrap;
            overflow: hidden;
            text-overflow: ellipsis;
            transition: background 0.1s, color 0.1s;
        }

        .city-ac-item:hover,
        .city-ac-item[aria-selected="true"] {
            background: var(--amber-dim);
            color: var(--amber);
        }

        .city-ac-empty {
            padding: 0.48rem 0.6rem;
            font-family: var(--mono);
            font-size: 0.82rem;
            color: var(--muted);
            text-align: center;
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

        .quick-ranges {
            display: flex;
            gap: 0.35rem;
        }

        .quick-range-btn {
            flex: 1;
            padding: 0.45rem 0.4rem;
            border-radius: 6px;
            border: 1px solid var(--border-hi);
            background: transparent;
            color: var(--muted);
            font-family: var(--mono);
            font-size: 0.75rem;
            cursor: pointer;
            transition: all 0.15s;
            letter-spacing: 0.04em;
            text-align: center;
        }

        .quick-range-btn.active {
            border-color: var(--amber);
            color: var(--amber);
            background: var(--amber-dim);
        }

        .quick-range-btn:hover:not(.active) { color: var(--ink); }

        /* ── Main content ──────────────────────────────────────── */
        .content {
            display: grid;
            gap: 1.25rem;
        }

        /* ── Stats row ─────────────────────────────────────────── */
        /* Card frame around the stats grid so a header can scope the
           numbers to the selected range. */
        .stats-card {
            background: var(--surface);
            border: 1px solid var(--border);
            border-radius: 14px;
            overflow: hidden;
        }

        .stats {
            display: grid;
            grid-template-columns: repeat(4, 1fr);
            gap: 1px;
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

        /* ── Prices ────────────────────────────────────────────── */
        /* German pump boards print the tenth-of-a-cent digit raised and
           smaller (2.09⁹). Purely a display treatment: same font, same
           value, scaled off the surrounding text so it works at every
           size from the big card price down to a table cell. Positioned
           rather than vertical-aligned so the raised glyph cannot grow
           the line box and shift the layout around it.

           DM Mono's digits are 0.72em tall, so flush tops would be
           0.72 × (1 − 0.62) / 0.62 = 0.441em of lift. The offset is biased
           past that: browsers round the resulting sub-pixel offset to whole
           device pixels in either direction, and a raised digit reading a
           pixel high passes for flush where one reading low looks dropped. */
        .price-milli {
            font-size: 0.62em;
            line-height: 1;
            position: relative;
            top: -0.48em;
        }

        /* The decimal separator comes down with it. At full size DM Mono's
           comma is a heavy mark between the digits; smaller, it separates
           without competing, and since the advance width shrinks with the
           glyph the whole number tightens up. Kept on the baseline. */
        .price-sep {
            font-size: 0.7em;
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

        /* Lighter scope note next to a card title ("in range"): lowercase
           and faded so it reads as a qualifier, not part of the title. */
        .cheapest-scope {
            font-size: 0.72rem;
            color: var(--muted);
            opacity: 0.6;
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

        /* Runners-up (ranks 2-5) inside the top-5 cheapest card */
        .rank-list {
            margin-top: 0.8rem;
            padding-top: 0.7rem;
            border-top: 1px solid var(--border);
            display: grid;
            gap: 0.45rem;
        }

        .rank-row {
            display: flex;
            align-items: baseline;
            gap: 0.5rem;
            font-family: var(--mono);
            font-size: 0.75rem;
            min-width: 0;
        }

        .rank-price {
            font-weight: 500;
            flex-shrink: 0;
        }

        .rank-station {
            flex: 1;
            min-width: 0;
            color: var(--ink);
            white-space: nowrap;
            overflow: hidden;
            text-overflow: ellipsis;
        }

        /* ── Predictions card ──────────────────────────────────── */
        /* Reuses the .cheapest-* / .rank-* structure; these add the per-day
           header, the window time column, and the "as of" run note. */
        .pred-day {
            margin-top: 1rem;
            padding-top: 0.7rem;
            border-top: 1px solid var(--border);
            font-family: var(--mono);
            font-size: 0.68rem;
            color: var(--muted);
            letter-spacing: 0.04em;
        }

        /* The first day sits flush under the fuel label (no divider). */
        .cheapest-fuel-label + .pred-day {
            margin-top: 0.5rem;
            padding-top: 0;
            border-top: none;
        }

        .pred-time {
            color: var(--muted);
            flex-shrink: 0;
            white-space: nowrap;
        }

        .pred-asof {
            margin-top: 0.8rem;
            font-family: var(--mono);
            font-size: 0.66rem;
            color: var(--muted);
        }

        /* ── Surroundings card ─────────────────────────────────── */
        /* A list, not the three-column fuel grid the other cards use: the
           question here is which pump is nearest, so distance leads the row and
           every fuel in scope sits on it. */
        .nearby-list {
            display: grid;
            gap: 1px;
            background: var(--border);
        }

        .nearby-btn {
            appearance: none;
            width: 100%;
            border: none;
            background: var(--surface);
            color: inherit;
            font: inherit;
            text-align: left;
            cursor: pointer;
            padding: 0.7rem 1.25rem;
            display: grid;
            grid-template-columns: 4.4rem minmax(0, 1fr) auto;
            align-items: center;
            gap: 0.15rem 0.9rem;
            transition: background 0.15s ease;
        }

        .nearby-btn:hover { background: var(--surface-hi); }

        .nearby-btn:focus-visible {
            outline: 2px solid var(--amber);
            outline-offset: -2px;
            background: var(--surface-hi);
        }

        /* Every cell is placed by hand: a row whose address or price list is
           empty must not let the rest of it reflow into the gap. */
        .nearby-dist {
            grid-column: 1;
            grid-row: 1 / span 2;
            font-family: var(--mono);
            font-size: 0.85rem;
            font-weight: 500;
            color: var(--amber);
            white-space: nowrap;
        }

        .nearby-name {
            grid-column: 2;
            grid-row: 1;
            display: flex;
            align-items: center;
            min-width: 0;
            font-family: var(--mono);
            font-size: 0.8rem;
            color: var(--ink);
        }

        .nearby-addr {
            grid-column: 2;
            grid-row: 2;
            font-family: var(--mono);
            font-size: 0.7rem;
            color: var(--muted);
            white-space: nowrap;
            overflow: hidden;
            text-overflow: ellipsis;
        }

        .nearby-prices {
            grid-column: 3;
            grid-row: 1 / span 2;
            display: flex;
            align-items: baseline;
            gap: 0.9rem;
            font-family: var(--mono);
            white-space: nowrap;
        }

        .nearby-price {
            display: inline-flex;
            align-items: baseline;
            gap: 0.3rem;
            font-size: 0.95rem;
            font-weight: 500;
        }

        .nearby-price-label {
            font-size: 0.6rem;
            letter-spacing: 0.1em;
            text-transform: uppercase;
            opacity: 0.75;
        }

        .nearby-price.empty { color: var(--muted); opacity: 0.5; }

        .nearby-closed {
            flex-shrink: 0;
            margin-left: 0.45rem;
            padding: 0.05rem 0.4rem;
            border: 1px solid var(--border-hi);
            border-radius: 999px;
            font-size: 0.6rem;
            text-transform: uppercase;
            letter-spacing: 0.08em;
            color: var(--muted);
        }

        .nearby-more {
            background: var(--surface);
            padding: 0.75rem 1.25rem;
            text-align: center;
        }

        .nearby-foot {
            background: var(--surface);
            padding: 0 1.25rem 0.9rem;
            font-family: var(--mono);
            font-size: 0.66rem;
            color: var(--muted);
        }

        /* ── Clickable station references inside the price cards ─── */
        /* Every station mentioned in the four cards is a button opening the
           station detail dialog; the resets keep the card layout untouched. */
        .station-btn,
        .station-rank-btn {
            appearance: none;
            background: transparent;
            border: none;
            color: inherit;
            text-align: left;
            cursor: pointer;
            border-radius: 8px;
            transition: background 0.15s ease, box-shadow 0.15s ease;
        }

        /* The block button carries no text of its own — every line inside it is
           a .cheapest-station span — so it just drops the UA button font. */
        .station-btn {
            display: block;
            font: inherit;
            width: calc(100% + 0.6rem);
            padding: 0.2rem 0.3rem;
            margin: -0.2rem -0.3rem;
        }

        /* No font reset here: the row keeps .rank-row's mono font, and a
           `font: inherit` would out-order that rule and enlarge the text.
           The padding gives the hover pill some body; the matching negative
           margin keeps the row's layout height exactly as it was. */
        .station-rank-btn {
            width: calc(100% + 0.6rem);
            padding: 0.15rem 0.3rem;
            margin: -0.15rem -0.3rem;
        }

        .station-btn:hover,
        .station-rank-btn:hover {
            background: var(--surface-hi);
        }

        .station-btn:focus-visible,
        .station-rank-btn:focus-visible {
            outline: 2px solid var(--amber);
            outline-offset: 1px;
            background: var(--surface-hi);
        }

        /* The name line needs flex so the trailing icon survives the ellipsis. */
        .sd-name-line {
            display: flex;
            align-items: center;
            min-width: 0;
        }

        .sd-name-text {
            min-width: 0;
            overflow: hidden;
            text-overflow: ellipsis;
            white-space: nowrap;
        }

        .sd-addr-line {
            display: block;
            opacity: 0.6;
        }

        .station-btn-icon {
            flex-shrink: 0;
            margin-left: 0.4rem;
            color: var(--muted);
            opacity: 0.55;
            transition: opacity 0.15s ease, color 0.15s ease;
        }

        .station-btn:hover .station-btn-icon,
        .station-btn:focus-visible .station-btn-icon {
            opacity: 1;
            color: var(--amber);
        }

        /* ── Station detail dialog ─────────────────────────────── */
        .station-dialog {
            width: min(470px, calc(100vw - 1.5rem));
            /* The global `* { margin: 0 }` reset drops the UA's auto margins,
               which is what centres a modal dialog — restore them here. */
            margin: auto;
            max-height: calc(100dvh - 2rem);
            padding: 0;
            border: 1px solid var(--border-hi);
            border-radius: 18px;
            background: var(--surface);
            color: var(--ink);
            box-shadow: 0 30px 70px rgba(0,0,0,0.55);
            overflow: hidden auto;
        }

        .station-dialog::backdrop {
            background: rgba(6,7,10,0.62);
            backdrop-filter: blur(3px);
        }

        @keyframes sd-in {
            from { opacity: 0; transform: translateY(12px) scale(0.98); }
            to   { opacity: 1; transform: none; }
        }

        .station-dialog[open] { animation: sd-in 0.18s ease both; }

        @media (prefers-reduced-motion: reduce) {
            .station-dialog[open] { animation: none; }
        }

        .sd-head {
            position: relative;
            padding: 1.15rem 3.1rem 1.1rem 1.25rem;
            border-bottom: 1px solid var(--border);
            background:
                radial-gradient(130% 150% at 0% 0%, var(--amber-dim) 0%, transparent 62%),
                var(--surface-hi);
        }

        .sd-kicker {
            font-family: var(--mono);
            font-size: 0.62rem;
            letter-spacing: 0.16em;
            text-transform: uppercase;
            color: var(--muted);
        }

        .sd-name {
            display: flex;
            /* flex-start keeps the colour dot on the first line when a long
               station name wraps. */
            align-items: flex-start;
            gap: 0.5rem;
            margin-top: 0.35rem;
            font-size: 1.1rem;
            font-weight: 700;
            line-height: 1.3;
            letter-spacing: -0.01em;
        }

        .sd-name .legend-dot { margin-top: 0.45rem; }

        .sd-tags {
            display: flex;
            flex-wrap: wrap;
            gap: 0.4rem;
            margin-top: 0.65rem;
        }

        .sd-tag {
            font-family: var(--mono);
            font-size: 0.68rem;
            padding: 0.22rem 0.55rem;
            border-radius: 999px;
            border: 1px solid var(--border-hi);
            background: var(--surface);
            color: var(--muted);
            white-space: nowrap;
        }

        .sd-tag.is-open   { color: var(--e10); border-color: rgba(52,211,153,0.35);  background: rgba(52,211,153,0.10); }
        .sd-tag.is-closed { color: var(--red); border-color: rgba(248,113,113,0.30); background: rgba(248,113,113,0.08); }

        .sd-tag .sd-tag-dot {
            display: inline-block;
            width: 6px;
            height: 6px;
            border-radius: 50%;
            background: currentColor;
            margin-right: 0.4rem;
            vertical-align: middle;
        }

        .sd-close {
            position: absolute;
            top: 0.85rem;
            right: 0.85rem;
            width: 30px;
            height: 30px;
            display: grid;
            place-items: center;
            border-radius: 9px;
            border: 1px solid var(--border-hi);
            background: var(--surface);
            color: var(--muted);
            cursor: pointer;
            transition: color 0.15s, border-color 0.15s;
        }

        .sd-close:hover { color: var(--ink); border-color: var(--amber); }
        .sd-close:focus-visible { outline: 2px solid var(--amber); outline-offset: 2px; }

        .sd-prices {
            display: grid;
            grid-template-columns: repeat(3, 1fr);
            gap: 1px;
            background: var(--border);
            border-bottom: 1px solid var(--border);
        }

        .sd-price {
            background: var(--surface);
            padding: 0.8rem 1rem;
        }

        .sd-price-label {
            font-family: var(--mono);
            font-size: 0.62rem;
            letter-spacing: 0.12em;
            text-transform: uppercase;
            margin-bottom: 0.3rem;
        }

        .sd-price-value {
            font-family: var(--mono);
            font-size: 1.1rem;
            font-weight: 500;
            line-height: 1;
            letter-spacing: -0.02em;
        }

        .sd-price.empty .sd-price-label,
        .sd-price.empty .sd-price-value { color: var(--muted); opacity: 0.6; }

        .sd-body {
            padding: 1.05rem 1.25rem 1.25rem;
            display: grid;
            gap: 0.75rem;
        }

        .sd-row {
            display: grid;
            grid-template-columns: 6.6rem minmax(0, 1fr);
            gap: 0.75rem;
            align-items: start;
        }

        .sd-key {
            font-family: var(--mono);
            font-size: 0.64rem;
            letter-spacing: 0.1em;
            text-transform: uppercase;
            color: var(--muted);
            padding-top: 0.12rem;
        }

        .sd-val {
            font-family: var(--mono);
            font-size: 0.8rem;
            color: var(--ink);
            overflow-wrap: anywhere;
        }

        .sd-note {
            font-family: var(--mono);
            font-size: 0.75rem;
            color: var(--muted);
        }

        /* The "none predicted" line stands in for a whole key/value row, so it
           spans the full width and keeps to a single line. */
        .sd-key-alone {
            padding-top: 0;
            line-height: 1.35;
        }

        .sd-windows { display: grid; gap: 0.35rem; }

        .sd-window {
            display: flex;
            align-items: baseline;
            gap: 0.6rem;
            font-family: var(--mono);
            font-size: 0.75rem;
        }

        .sd-window-fuel { color: var(--muted); flex-shrink: 0; width: 3.2rem; }
        .sd-window-time { color: var(--ink); flex: 1; min-width: 0; }
        .sd-window-price { font-weight: 500; flex-shrink: 0; }

        .sd-nav {
            display: flex;
            align-items: center;
            justify-content: center;
            gap: 0.5rem;
            margin-top: 0.2rem;
            padding: 0.75rem 1rem;
            border-radius: 11px;
            border: 1px solid rgba(0,0,0,0.18);
            background: linear-gradient(180deg, #ffc85f 0%, var(--amber) 100%);
            color: #2a1c02;
            font-size: 0.85rem;
            font-weight: 700;
            text-decoration: none;
            box-shadow: 0 8px 20px var(--amber-glow);
            transition: transform 0.15s ease, box-shadow 0.15s ease;
        }

        .sd-nav:hover { transform: translateY(-1px); box-shadow: 0 12px 26px var(--amber-glow); }
        .sd-nav:focus-visible { outline: 2px solid var(--amber); outline-offset: 3px; }

        @media (prefers-reduced-motion: reduce) {
            .sd-nav:hover { transform: none; }
        }

        @media (max-width: 560px) {
            .cheapest-grid,
            .cheapest-grid.two-col { grid-template-columns: 1fr; }
            .sd-price { padding: 0.7rem 0.6rem; }
            .sd-row { grid-template-columns: 1fr; gap: 0.15rem; }
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

        .range-toggles {
            display: flex;
            gap: 0.35rem;
        }

        .range-toggle {
            font-family: var(--mono);
            font-size: 0.72rem;
            padding: 0.3rem 0.6rem;
            border-radius: 6px;
            border: 1px solid var(--border-hi);
            background: transparent;
            color: var(--muted);
            cursor: pointer;
            transition: all 0.15s;
            letter-spacing: 0.04em;
        }

        .range-toggle.active {
            border-color: var(--amber);
            color: var(--amber);
            background: rgba(245,166,35,0.1);
        }

        .chart-body {
            padding: 1rem 1.25rem;
        }

        /* Height comes from the viewBox aspect ratio, which renderChart()
           keeps 1:1 with CSS pixels so axis text is never stretched. */
        #chart {
            width: 100%;
            display: block;
            height: auto;
            /* Long-press summons the crosshair on touch, so keep the browser's
               own long-press reactions (selection, iOS callout) off the chart. */
            -webkit-user-select: none;
            user-select: none;
            -webkit-touch-callout: none;
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
            cursor: pointer;
            user-select: none;
        }

        .legend-item.off {
            opacity: 0.35;
        }

        .legend-item.off .legend-dot {
            filter: grayscale(1);
        }

        .legend-dot {
            width: 8px;
            height: 8px;
            border-radius: 50%;
            flex-shrink: 0;
        }

        /* Trend entries carry a line swatch instead of a dot so the legend
           shows the exact dash pattern the trendline is drawn with. */
        .legend-line {
            flex-shrink: 0;
            overflow: visible;
        }

        .legend-item.off .legend-line {
            filter: grayscale(1);
        }

        .legend-trend-rate {
            opacity: 0.75;
        }

        .chart-empty {
            padding: 3rem 1.25rem;
            text-align: center;
            font-family: var(--mono);
            font-size: 0.85rem;
            color: var(--muted);
        }

        /* ── Loading states ────────────────────────────────────── */
        /* Author display rules (grid/flex/block) on elements like
           .chart-loading, #chart and .chart-legend override the UA's
           [hidden] { display: none }, so toggling el.hidden had no effect. */
        [hidden] { display: none !important; }

        @keyframes skeleton-pulse { 0%, 100% { opacity: 0.55; } 50% { opacity: 0.25; } }

        .skeleton {
            border-radius: 6px;
            background: var(--surface-hi);
            animation: skeleton-pulse 1.2s ease-in-out infinite;
            min-height: 1em;
            max-width: 5ch;
        }

        @keyframes spin { to { transform: rotate(360deg); } }

        .spinner {
            display: inline-block;
            width: 22px;
            height: 22px;
            border: 2px solid var(--border-hi);
            border-top-color: var(--amber);
            border-radius: 50%;
            animation: spin 0.7s linear infinite;
        }

        .chart-loading {
            display: grid;
            place-items: center;
            min-height: 380px;
        }

        .table-loading {
            text-align: center;
            padding: 2.5rem 1rem !important;
        }

        .table-more {
            padding: 0.85rem 1.25rem;
            border-top: 1px solid var(--border);
        }

        .chart-retry {
            padding: 0 1.25rem 1.25rem;
            text-align: center;
        }

        .chart-retry .btn-reset { display: inline-block; width: auto; padding: 0.55rem 1.4rem; }

        /* Visually hidden but exposed to assistive tech. */
        .sr-only {
            position: absolute;
            width: 1px; height: 1px;
            padding: 0; margin: -1px;
            overflow: hidden; clip: rect(0, 0, 0, 0);
            white-space: nowrap; border: 0;
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
            /* Single column: promote the content cards to layout children so
               the top-5 card can sit above the filters (order: -1) while the
               rest keeps its DOM order below them. */
            /* align-items: stretch overrides the desktop grid's `start`, which
               would let wide cards (the snapshot table) blow out the viewport. */
            .layout { display: flex; flex-direction: column; gap: 1.25rem; align-items: stretch; }
            .content { display: contents; }
            .content > .error-box { order: -2; }
            #cheapest-card { order: -1; }
            /* Keep the predictions card directly beneath the top-5 card (equal
               order preserves DOM sequence) and above the filters on mobile. */
            #predictions-card { order: -1; }
            /* Same for the surroundings card, which follows it in the DOM. */
            #nearby-card { order: -1; }
            .sidebar { position: static; }
            .sidebar-head { cursor: pointer; }
            .sidebar-chevron { display: inline-flex; }
            .sidebar:not(.collapsed) .sidebar-chevron svg { transform: rotate(180deg); }
            .sidebar.collapsed form,
            .sidebar.collapsed .sidebar-actions { display: none; }
            .stats { grid-template-columns: repeat(2, 1fr); }
        }

        @media (max-width: 560px) {
            .page { width: 100vw; padding: 1rem 0.75rem 3rem; }
            .stats { grid-template-columns: 1fr 1fr; }
            /* Keep the controls on the same row as the logo. */
            .header { align-items: center; gap: 0.75rem; padding-bottom: 1rem; }
            .brand-icon,
            .brand-icon img { width: 40px; height: 40px; }
            h1 { font-size: 1.4rem; }
            /* Too narrow for three columns: the prices drop to their own row
               under the address, still left-aligned with the station. */
            .nearby-btn { grid-template-columns: 3.6rem minmax(0, 1fr); }
            .nearby-prices { grid-column: 1 / -1; grid-row: 3; gap: 0.75rem; margin-top: 0.35rem; }
            /* Give the plot itself more width on phones. */
            .chart-body { padding: 0.75rem 0.5rem; }
            .chart-legend { padding: 0.75rem; gap: 0.5rem 0.85rem; }
            .chart-loading { min-height: 300px; }
        }

        /* ── Load animation ────────────────────────────────────── */
        @keyframes fade-up {
            from { opacity: 0; transform: translateY(12px); }
            /* End at `none` so the retained fill-mode value doesn't leave a
               stacking context behind (it would trap dropdowns' z-index). */
            to   { opacity: 1; transform: none; }
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
            --wm-shadow:  rgba(28,28,30,0.12);
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
        /* Parked in the free strip below (or above) the plot by
           positionChartTooltip, which also caps the height to that strip and
           picks the column layout a long station list needs to fit it. */
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
            max-width: min(320px, calc(100vw - 16px));
            overflow: hidden;
        }

        /* Long station lists: spread over columns (wide screens) and/or
           tightened (phones) so the whole list fits without covering the plot. */
        #price-tooltip.tt-cols {
            max-width: calc(100vw - 16px);
            column-gap: 1.1rem;
            column-rule: 1px solid var(--border);
        }

        #price-tooltip.tt-cols .tt-meta { column-span: all; }
        #price-tooltip .tt-row { break-inside: avoid; }

        #price-tooltip.tt-dense { line-height: 1.35; padding: 0.45rem 0.7rem; }
        #price-tooltip.tt-dense .tt-row { margin-top: 1px; }

        #price-tooltip .tt-meta {
            color: var(--muted);
            font-size: 0.72rem;
        }

        #price-tooltip .tt-row {
            display: flex;
            align-items: center;
            gap: 0.45rem;
            margin-top: 4px;
        }

        #price-tooltip .tt-name {
            flex: 1;
            min-width: 0;
            overflow: hidden;
            text-overflow: ellipsis;
            white-space: nowrap;
            font-size: 0.72rem;
            color: var(--ink);
        }

        #price-tooltip .tt-fuel {
            color: var(--muted);
            font-size: 0.62rem;
            text-transform: uppercase;
            letter-spacing: 0.08em;
            flex-shrink: 0;
        }

        #price-tooltip .tt-val {
            font-size: 0.78rem;
            font-weight: 500;
            flex-shrink: 0;
        }

        /* ── Auth, hamburger menu, account & admin pages ── */
        .header { position: relative; z-index: 300; }
        .menu-toggle svg { width: 18px; height: 18px; }
        .menu-panel {
            position: absolute;
            top: calc(100% + 10px);
            right: 0;
            min-width: 230px;
            max-width: calc(100vw - 2rem);
            background: var(--surface);
            border: 1px solid var(--border-hi);
            border-radius: 12px;
            padding: 8px;
            z-index: 300;
            box-shadow: 0 14px 36px rgba(0, 0, 0, 0.4);
            display: flex;
            flex-direction: column;
            gap: 2px;
        }
        .menu-user {
            font-family: var(--mono);
            font-size: 0.72rem;
            color: var(--muted);
            padding: 6px 10px 8px;
            border-bottom: 1px solid var(--border);
            margin-bottom: 4px;
            overflow: hidden;
            text-overflow: ellipsis;
            white-space: nowrap;
        }
        .menu-item {
            display: block;
            width: 100%;
            text-align: left;
            padding: 8px 10px;
            border-radius: 8px;
            border: none;
            background: none;
            color: var(--ink);
            text-decoration: none;
            font-family: var(--mono);
            font-size: 0.82rem;
            cursor: pointer;
        }
        .menu-item:hover { background: var(--amber-dim); color: var(--amber); }
        .menu-item.active { color: var(--amber); }
        .menu-sep {
            font-family: var(--mono);
            font-size: 0.68rem;
            letter-spacing: 0.1em;
            text-transform: uppercase;
            color: var(--muted);
            padding: 8px 10px 2px;
            border-top: 1px solid var(--border);
            margin-top: 4px;
        }
        .menu-logout { margin: 0; }
        .auth-wrap { display: flex; justify-content: center; padding: 3rem 0; }
        .auth-card {
            width: 100%;
            max-width: 430px;
            background: var(--surface);
            border: 1px solid var(--border);
            border-radius: 14px;
            padding: 1.6rem;
        }
        .auth-card h2, .settings-card h2 {
            font-family: var(--sans);
            font-size: 1.05rem;
            margin: 0 0 1rem;
        }
        .auth-card .field, .settings-card .field { margin-bottom: 0.9rem; }
        .auth-note {
            font-family: var(--mono);
            font-size: 0.75rem;
            color: var(--muted);
            line-height: 1.5;
            margin: 0.9rem 0 0.9rem;
            overflow-wrap: anywhere;
        }
        .auth-note a { color: var(--amber); }
        .auth-code {
            font-family: var(--mono);
            background: var(--bg);
            border: 1px solid var(--border-hi);
            border-radius: 8px;
            padding: 0.7rem 0.9rem;
            color: var(--amber);
            font-size: 0.85rem;
        }
        .settings-layout {
            width: 100%;
            max-width: 560px;
            margin: 0 auto;
            display: flex;
            flex-direction: column;
            gap: 1.2rem;
            padding-bottom: 3rem;
        }
        .settings-layout.wide { max-width: 900px; }
        .settings-card {
            background: var(--surface);
            border: 1px solid var(--border);
            border-radius: 14px;
            padding: 1.4rem 1.6rem;
        }
        .settings-card.danger { border-color: rgba(248, 113, 113, 0.35); }
        .settings-card textarea {
            width: 100%;
            padding: 0.6rem 0.8rem;
            border-radius: 8px;
            border: 1px solid var(--border-hi);
            background: var(--bg);
            color: var(--ink);
            font-family: var(--mono);
            font-size: 0.8rem;
            resize: vertical;
        }
        .success-box {
            background: rgba(52, 211, 153, 0.08);
            border: 1px solid rgba(52, 211, 153, 0.3);
            border-radius: 10px;
            padding: 0.85rem 1rem;
            font-family: var(--mono);
            font-size: 0.82rem;
            color: var(--e10);
            margin-bottom: 0.5rem;
        }
        .btn-danger {
            display: block;
            width: 100%;
            padding: 0.75rem 1rem;
            border-radius: 8px;
            border: 1px solid rgba(248, 113, 113, 0.5);
            background: rgba(248, 113, 113, 0.12);
            color: var(--red);
            font-family: var(--mono);
            font-size: 0.85rem;
            cursor: pointer;
            letter-spacing: 0.04em;
        }
        .btn-danger:hover { background: rgba(248, 113, 113, 0.22); }
        .btn-small {
            padding: 0.35rem 0.7rem;
            border-radius: 6px;
            border: 1px solid var(--border-hi);
            background: var(--surface-hi);
            color: var(--ink);
            font-family: var(--mono);
            font-size: 0.72rem;
            cursor: pointer;
        }
        .btn-small:hover { border-color: var(--amber); color: var(--amber); }
        .btn-small.danger:hover { border-color: var(--red); color: var(--red); }
        .btn-icon {
            display: inline-flex;
            align-items: center;
            justify-content: center;
            width: 30px;
            height: 30px;
            padding: 0;
            flex-shrink: 0;
            border: none;
            border-radius: 6px;
            background: none;
            color: var(--amber);
            cursor: pointer;
            transition: background 0.15s;
        }
        .btn-icon:hover { background: var(--amber-dim); }
        .btn-icon.danger { color: var(--red); }
        .btn-icon.danger:hover { background: rgba(248, 113, 113, 0.12); }
        .btn-icon svg { width: 16px; height: 16px; }
        .table-form { display: inline-block; margin: 0 0.15rem 0 0; }
        .actions-cell { white-space: nowrap; }
        .table-scroll { overflow-x: auto; }
        .btn-primary:disabled { opacity: 0.45; cursor: not-allowed; box-shadow: none; }
        /* Address line under a station name (admin stations table) */
        .station-sub {
            display: block;
            font-family: var(--mono);
            font-size: 0.7rem;
            color: var(--muted);
            margin-top: 0.15rem;
        }
        /* Inline edit controls inside the renamed-stations table: input,
           save, and remove share one compact line. */
        .rename-controls { display: flex; align-items: center; gap: 0.15rem; }
        .rename-form { display: flex; align-items: center; gap: 0.15rem; flex: 1; min-width: 0; }
        .rename-remove { display: flex; flex-shrink: 0; }
        .rename-form input[type="text"] {
            flex: 1;
            min-width: 8rem;
            padding: 0.45rem 0.6rem;
            border-radius: 8px;
            border: 1px solid var(--border-hi);
            background: var(--bg);
            color: var(--ink);
            font-family: var(--mono);
            font-size: 0.8rem;
        }
        /* Two-line station suggestions in the autocomplete dropdown */
        .station-ac-item .ac-name { display: block; overflow: hidden; text-overflow: ellipsis; }
        .station-ac-item .ac-sub {
            display: block;
            font-size: 0.7rem;
            color: var(--muted);
            overflow: hidden;
            text-overflow: ellipsis;
        }
        /* Stack admin tables into label/value cards on small screens instead
           of forcing a horizontal scroll. */
        @media (max-width: 640px) {
            .stack-table thead { display: none; }
            .stack-table, .stack-table tbody, .stack-table tr, .stack-table td { display: block; width: 100%; }
            .stack-table tr {
                border: 1px solid var(--border);
                border-radius: 10px;
                padding: 0.55rem 0.75rem;
                margin-bottom: 0.6rem;
            }
            .stack-table tr:last-child { margin-bottom: 0; }
            .stack-table td {
                display: flex;
                justify-content: space-between;
                align-items: center;
                gap: 1rem;
                padding: 0.3rem 0;
                border: none;
            }
            .stack-table td[data-label]::before {
                content: attr(data-label);
                font-size: 0.66rem;
                text-transform: uppercase;
                letter-spacing: 0.1em;
                color: var(--muted);
                flex-shrink: 0;
            }
            .stack-table td.stack-primary {
                justify-content: flex-start;
                flex-wrap: wrap;
                gap: 0.4rem;
                font-size: 0.9rem;
                padding-bottom: 0.45rem;
                border-bottom: 1px solid var(--border);
                margin-bottom: 0.2rem;
                /* `anywhere` (unlike break-word) also affects min-content
                   sizing, so a long unbroken email cannot widen the card. */
                overflow-wrap: anywhere;
            }
            .stack-table td.actions-cell {
                white-space: normal;
                justify-content: flex-start;
                flex-wrap: wrap;
                gap: 0.35rem;
                padding-top: 0.5rem;
            }
            .stack-table td:empty { display: none; }
            /* The inline rename controls get their own full-width line below
               the label; input, save, and remove stay on that one line. */
            .stack-table td.rename-cell { flex-wrap: wrap; }
            .stack-table td.rename-cell .rename-controls { flex: 1 1 100%; }
            .stack-table td.stack-primary .station-sub { flex-basis: 100%; margin-top: 0; }
        }
        .badge {
            display: inline-block;
            padding: 0.1rem 0.5rem;
            border-radius: 999px;
            border: 1px solid var(--border-hi);
            font-family: var(--mono);
            font-size: 0.68rem;
            color: var(--muted);
        }
        .badge.ok { border-color: rgba(52, 211, 153, 0.4); color: var(--e10); }
        .badge.warn { border-color: rgba(245, 166, 35, 0.5); color: var(--amber); }
        .day-toggles { display: flex; flex-wrap: wrap; gap: 0.4rem; }
        .day-toggle {
            display: inline-flex;
            align-items: center;
            gap: 0.4rem;
            font-family: var(--mono);
            font-size: 0.75rem;
            color: var(--ink);
            border: 1px solid var(--border-hi);
            border-radius: 8px;
            padding: 0.35rem 0.6rem;
            cursor: pointer;
            user-select: none;
            transition: border-color 0.15s, color 0.15s, background 0.15s;
        }
        .day-toggle:has(input:checked) {
            border-color: var(--amber);
            color: var(--amber);
            background: var(--amber-dim);
        }
        .check-toggle {
            display: inline-flex;
            align-items: center;
            gap: 0.5rem;
            font-family: var(--mono);
            font-size: 0.78rem;
            color: var(--ink);
            cursor: pointer;
            line-height: 1.45;
        }
        .field-hint {
            font-family: var(--mono);
            font-size: 0.7rem;
            color: var(--muted);
            line-height: 1.5;
            margin-top: 0.35rem;
        }
        .row-list { display: flex; flex-direction: column; gap: 0.4rem; margin-bottom: 0.5rem; }
        .row-item { display: flex; align-items: center; gap: 0.45rem; }
        .field .row-item input.time-input {
            width: 5.4rem;
            flex: none;
            text-align: center;
            padding: 0.45rem 0.4rem;
            border-radius: 8px;
            border: 1px solid var(--border-hi);
            background: var(--bg);
            color: var(--ink);
            font-family: var(--mono);
            font-size: 0.8rem;
        }
        .btn-row-remove {
            border: 1px solid var(--border-hi);
            background: none;
            color: var(--muted);
            border-radius: 6px;
            width: 26px;
            height: 26px;
            cursor: pointer;
            font-size: 0.9rem;
            line-height: 1;
        }
        .btn-row-remove:hover { color: var(--red); border-color: var(--red); }
        .btn-row-add {
            border: 1px dashed var(--border-hi);
            background: none;
            color: var(--muted);
            border-radius: 8px;
            padding: 0.35rem 0.7rem;
            font-family: var(--mono);
            font-size: 0.72rem;
            cursor: pointer;
        }
        .btn-row-add:hover { color: var(--amber); border-color: var(--amber); }
        .inline-form { display: flex; flex-wrap: wrap; gap: 0.5rem; margin-top: 0.8rem; }
        .inline-form input[type="text"] {
            flex: 1;
            padding: 0.5rem 0.7rem;
            border-radius: 8px;
            border: 1px solid var(--border-hi);
            background: var(--bg);
            color: var(--ink);
            font-family: var(--mono);
            font-size: 0.8rem;
        }
        .inline-form input[type="number"] {
            width: 90px;
            padding: 0.5rem 0.7rem;
            border-radius: 8px;
            border: 1px solid var(--border-hi);
            background: var(--bg);
            color: var(--ink);
            font-family: var(--mono);
            font-size: 0.8rem;
        }
        .inline-form .btn-primary { width: auto; display: inline-block; padding: 0.5rem 1rem; }
        .field-grid {
            display: grid;
            grid-template-columns: repeat(auto-fill, minmax(190px, 1fr));
            gap: 0.4rem 1rem;
            align-items: start;
        }
        html[data-theme="light"] .menu-panel { box-shadow: 0 14px 36px rgba(0, 0, 0, 0.15); }

        html[data-theme="light"] .station-dialog { box-shadow: 0 24px 60px rgba(0, 0, 0, 0.18); }
        html[data-theme="light"] .station-dialog::backdrop { background: rgba(28, 28, 30, 0.32); }
        html[data-theme="light"] .sd-nav { color: #241800; border-color: rgba(0, 0, 0, 0.12); }
        @media (max-width: 560px) {
            .settings-card { padding: 1.1rem 1rem; }
            .inline-form input[type="text"] { min-width: 0; flex: 1 1 10rem; }
            .field-grid { grid-template-columns: 1fr; gap: 0; }
        }
    </style>
</head>
<?php
}

// renderHeader emits the shared page header. Signed-in users additionally get
// the hamburger button and its slide-down menu.
function renderHeader(?array $user, string $activePage): void
{
?>
    <!-- Header -->
    <header class="header">
        <a class="brand" href="?" aria-label="Gasoline — Dashboard" data-i18n-aria-label="brandAriaLabel">
            <span class="brand-icon" aria-hidden="true">
                <img class="logo-light" src="logo-light.svg" alt="">
                <img class="logo-dark" src="logo-dark.svg" alt="">
            </span>
            <h1><span class="wm">gas<em><span>o</span></em>line</span></h1>
        </a>
        <div class="header-controls">
            <div class="lang-picker">
                <button class="lang-btn" data-lang="en">EN</button>
                <button class="lang-btn" data-lang="de">DE</button>
            </div>
            <button class="theme-toggle" id="theme-toggle" aria-label="Toggle theme" data-i18n-aria-label="toggleTheme">
                <svg id="theme-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="5"/><line x1="12" y1="1" x2="12" y2="3"/><line x1="12" y1="21" x2="12" y2="23"/><line x1="4.22" y1="4.22" x2="5.64" y2="5.64"/><line x1="18.36" y1="18.36" x2="19.78" y2="19.78"/><line x1="1" y1="12" x2="3" y2="12"/><line x1="21" y1="12" x2="23" y2="12"/><line x1="4.22" y1="19.78" x2="5.64" y2="18.36"/><line x1="18.36" y1="5.64" x2="19.78" y2="4.22"/></svg>
            </button>
            <?php if ($user !== null) { ?>
            <button class="theme-toggle menu-toggle" id="menu-toggle" aria-expanded="false" aria-controls="app-menu" aria-label="Open menu" data-i18n-aria-label="openMenu">
                <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><line x1="3" y1="6" x2="21" y2="6"/><line x1="3" y1="12" x2="21" y2="12"/><line x1="3" y1="18" x2="21" y2="18"/></svg>
            </button>
            <nav class="menu-panel" id="app-menu" hidden>
                <div class="menu-user"><?= h($user['email']) ?></div>
                <a class="menu-item<?= $activePage === 'dashboard' ? ' active' : '' ?>" href="?" data-i18n="menuDashboard">Dashboard</a>
                <a class="menu-item<?= $activePage === 'account' ? ' active' : '' ?>" href="?page=account" data-i18n="menuAccount">My Account</a>
                <?php if ((int) $user['is_admin'] === 1) { ?>
                <div class="menu-sep" data-i18n="menuAdminSection">Admin</div>
                <a class="menu-item<?= $activePage === 'admin_users' ? ' active' : '' ?>" href="?page=admin_users" data-i18n="menuUsers">Users</a>
                <a class="menu-item<?= $activePage === 'admin_stations' ? ' active' : '' ?>" href="?page=admin_stations" data-i18n="menuStations">Stations</a>
                <a class="menu-item<?= $activePage === 'admin_settings' ? ' active' : '' ?>" href="?page=admin_settings" data-i18n="menuSettings">Settings</a>
                <a class="menu-item<?= $activePage === 'admin_predictions' ? ' active' : '' ?>" href="?page=admin_predictions" data-i18n="menuPredictions">Prediction accuracy</a>
                <a class="menu-item<?= $activePage === 'admin_stats' ? ' active' : '' ?>" href="?page=admin_stats" data-i18n="menuStats">Statistics</a>
                <?php } ?>
                <div class="menu-sep"></div>
                <form method="post" action="" class="menu-logout"><?= csrfField() ?><input type="hidden" name="action" value="logout"><button type="submit" class="menu-item" data-i18n="menuLogout">Sign out</button></form>
            </nav>
            <?php } ?>
        </div>
    </header>
<?php
}


// renderCommonScript emits the JS shared by every page: i18n, theme toggle,
// hamburger menu, confirm dialogs, and the schedule-editor row controls.
function renderCommonScript(): void
{
?>
<script>
/* ── Shared: i18n, theme, menu ─────────────────────────────────── */
let currentLang = 'en';

const translations = {
    en: {
        title: 'Price History',
        filters: 'Filters',
        city: 'City',
        enterCity: 'Enter city...',
        allCities: '— all cities —',
        location: 'Location',
        enterLocation: 'City or address...',
        useMyLocation: 'Use my location',
        searchAddress: 'Search address "{query}"',
        locating: 'Searching…',
        geocodeFailed: 'Could not resolve that location.',
        geocodeNoMatch: 'No place found for that address.',
        geocodeDisabled: 'Address lookup is switched off on this server.',
        gpsUnsupported: 'This browser cannot report a position.',
        gpsDenied: 'Location access was denied.',
        gpsFailed: 'Could not determine your position.',
        radius: 'Radius',
        from: 'From',
        to: 'To',
        quickRange: 'Quick range',
        fuelType: 'Fuel type',
        fuelAll: 'All',
        fuelDiesel: 'Diesel',
        fuelE5: 'E5',
        fuelE10: 'E10',
        stations: 'Stations',
        filterStations: 'Filter stations...',
        reset: 'Reset',
        statsTitle: 'Selected range',
        snapshots: 'Snapshots',
        stationsCount: 'Stations',
        firstRecorded: 'First recorded',
        lastRecorded: 'Last recorded',
        priceTimeline: 'Price timeline',
        trend: 'Trend',
        trendPerDay: 'ct/day',
        trendHint: 'Linear trend across the shown stations. Click to hide.',
        trendLatest: 'Trend at the latest reading',
        station: 'Station',
        brand: 'Brand',
        openYes: 'open',
        openNo: 'closed',
        loading: 'Loading…',
        showMore: 'Show more',
        loadError: 'Could not load data. Please retry.',
        retry: 'Retry',
        cityNotFound: 'Selected city not found.',
        invalidFromDate: 'Invalid from date.',
        invalidToDate: 'Invalid to date.',
        noSnapshots: 'No snapshots match the current filters.',
        cheapestNow: 'Cheapest right now',
        cheapestNoData: 'No price data available.',
        cheapestPrefix: 'Lowest',
        cheapestRangeNoData: 'No price data available.',
        highestPrefix: 'Highest price',
        highestNoData: 'No price data available.',
        rangeScopeHint: 'in range',
        nearbyTitle: 'Nearby',
        nearbyNoLocation: 'Pick a location in the filters: a city, an address, or your current position.',
        nearbyNoData: 'No stations with current prices within this radius.',
        nearbyCapped: 'The {shown} nearest of {total} stations in range.',
        predictionsTitle: 'Recommended fill-ups',
        predictionsNoData: 'No upcoming predictions in the database for these stations.',
        predictionsAsOf: 'as of {time}',
        sdHint: 'Show station details',
        sdTitle: 'Station details',
        sdCurrentPrices: 'Current prices',
        sdAddress: 'Address',
        sdDistance: 'Distance',
        sdLastUpdate: 'Last update',
        sdNoPrices: 'No price snapshot for this station in the selected range.',
        sdUpcoming: 'Upcoming fill-up windows',
        sdNoUpcoming: 'No upcoming fill-up windows',
        sdNavigate: 'Navigate with Google Maps',
        sdClose: 'Close',
        rangeAll: 'All',
        range24h: '24h',
        range30d: '30d',
        range14d: '14d',
        range7d: '7d',
        range3d: '3d',
        rangeToday: 'Today',
        toggleTheme: 'Toggle theme',
        chartAriaLabel: 'Fuel price history chart',
        brandAriaLabel: 'Gasoline — Dashboard',
        openMenu: 'Open menu',
        menuDashboard: 'Dashboard',
        menuAccount: 'My Account',
        menuAdminSection: 'Admin',
        menuUsers: 'Users',
        menuStations: 'Stations',
        menuSettings: 'Settings',
        menuLogout: 'Sign out',
        loginTitle: 'Sign in',
        registerTitle: 'Create an account',
        registerHint: 'Your email address is your username. After registration an administrator has to approve your account before you can sign in.',
        email: 'Email address',
        password: 'Password',
        passwordRepeat: 'Repeat password',
        signIn: 'Sign in',
        createAccount: 'Create an account',
        noAccountYet: 'No account yet?',
        haveAccount: 'Already have an account?',
        unauthorized: 'Login required.',
        csrfError: 'The form has expired. Please try again.',
        invalidCredentials: 'Invalid email address or password.',
        awaitingApproval: 'Your account is awaiting approval by an administrator.',
        registerPendingSent: 'Account created. You will receive an email once an administrator approves it.',
        accountCreated: 'Account created. You can log in now.',
        invalidEmail: 'Please enter a valid email address.',
        emailTaken: 'An account with this email address already exists.',
        passwordTooShort: 'The password must be at least 10 characters long.',
        passwordMismatch: 'The passwords do not match.',
        wrongPassword: 'The current password is incorrect.',
        passwordChanged: 'Password changed.',
        changePassword: 'Change password',
        currentPassword: 'Current password',
        newPassword: 'New password',
        save: 'Save',
        notifySettings: 'Notifications',
        notifyMethod: 'Delivery method',
        pushoverAppName: 'Notification title',
        pushoverAppNameHint: 'Shown as the title of your notifications unless an administrator has configured a title template.',
        pushoverUserKey: 'Pushover user key',
        pushoverToken: 'Pushover API token',
        notifyDays: 'Days of the week',
        notifyWindows: 'Time windows',
        notifySuggestTimes: 'Daily suggestion times',
        notifySuggestEnabled: 'Send daily price suggestions',
        notifyCheckEnabled: 'Send buy-now alerts when prices drop',
        notifyKindsHint: 'Choose which notifications you receive. Suggestions forecast good times to fill up; buy-now alerts fire when a current price drops. Leave both off to pause all notifications.',
        notifyFuel: 'Fuel to be notified about',
        notifyFuelHint: 'You are notified about this fuel only. All three are tracked, so any choice is served.',
        notifyLocation: 'Notify me around',
        notifyRadius: 'Radius (km)',
        notifyLocationHint: "Notifications cover every tracked station within this distance of the city you pick, and distances are measured from there. Stations only appear once your administrator's update targets actually collect them.",
        notifySaved: 'Notification settings saved.',
        invalidNotifySettings: 'Invalid notification settings. Check days, time windows, and times.',
        invalidNotifyLocation: 'Pick a city from the suggestions and a radius between 1 and 100 km.',
        addWindow: 'Add window',
        addTime: 'Add time',
        removeRow: 'Remove',
        day_mon: 'Mon',
        day_tue: 'Tue',
        day_wed: 'Wed',
        day_thu: 'Thu',
        day_fri: 'Fri',
        day_sat: 'Sat',
        day_sun: 'Sun',
        dangerZone: 'Danger zone',
        deleteAccount: 'Delete account',
        deleteAccountConfirmLabel: 'I understand that my account and settings will be permanently deleted.',
        deleteAccountConfirm: 'Really delete your account? This cannot be undone.',
        confirmRequired: 'Please confirm the deletion.',
        lastAdminGuard: 'You are the last administrator and cannot delete this account.',
        accountDeleted: 'Your account has been deleted.',
        loggedOut: 'You have been signed out.',
        adminUsersTitle: 'Users',
        colEmail: 'Email',
        colStatus: 'Status',
        colAdmin: 'Admin',
        colCreated: 'Registered',
        colApproved: 'Approved',
        colActions: 'Actions',
        statusPending: 'pending',
        statusApproved: 'approved',
        adminYes: 'admin',
        actionApprove: 'Approve',
        actionDelete: 'Delete',
        actionPromote: 'Promote',
        actionDemote: 'Demote',
        confirmDeleteUser: 'Really delete this user?',
        userApproved: 'User approved.',
        userApprovedEmailFailed: 'User approved, but the notification email could not be sent.',
        userDeleted: 'User deleted.',
        userPromoted: 'User is now an administrator.',
        userDemoted: 'User is no longer an administrator.',
        cannotActOnSelf: 'You cannot perform this action on your own account.',
        notFound: 'The requested item was not found.',
        updateTargets: 'Automatic updates',
        updateTargetsHint: 'These cities are collected automatically by gasoline update when the CLI is invoked without --city/--radius flags. They decide which stations exist; each user picks the area they are notified about separately.',
        targetCity: 'City',
        targetRadius: 'Radius (km)',
        addTarget: 'Add',
        removeTarget: 'Remove',
        noTargets: 'No update targets configured yet.',
        targetAdded: 'Update target added.',
        targetRemoved: 'Update target removed.',
        invalidTarget: 'Invalid city or radius (1-25 km).',
        targetExists: 'This city is already an update target.',
        renameStation: 'Rename a station',
        renameStationHint: 'The new name replaces the Tankerkönig name everywhere — dashboard, CLI output, and notifications. The original name is kept and can be restored at any time.',
        stationSearchPlaceholder: 'Search by name or address...',
        newStationName: 'New name',
        applyRename: 'Apply',
        renamedStations: 'Renamed stations',
        colNewName: 'New name',
        noRenames: 'No stations have been renamed yet.',
        removeRename: 'Remove',
        confirmRemoveRename: 'Really remove this rename and restore the original name?',
        stationRenamed: 'Station renamed.',
        renameCleared: 'Rename removed. The original name is used again.',
        invalidRename: 'Select a station and enter a non-empty new name.',
        templateCheck: 'Buy-alert notification template',
        templateSuggest: 'Suggestion notification template',
        notificationTexts: 'Notification texts',
        notificationTextsHint: 'Suggestions and checks are computed for every fuel, covering every station the update targets above currently feed. These templates are the only part that is configured here; each user picks their own fuel, area and schedule in My Account.',
        templateCheckTitle: 'Buy-alert notification title',
        templateSuggestTitle: 'Suggestion notification title',
        titleTemplatePlaceholder: 'e.g. Fill up for {{cheapest_current_price_formatted}} EUR',
        templatePlaceholdersHint: 'Templates use {{placeholder}} syntax with the full gasoline-watch set, e.g. {{station_name}}, {{price}}, {{price_formatted}}, {{fuel}}, {{date}}, {{start_time}}, {{end_time}}, {{distance}}, {{confidence}}, {{count}}, {{cheapest_price}}, {{message}} and *_onchange variants. \\n inserts a line break.',
        titleTemplatesHint: 'Title templates use the same placeholders; row placeholders resolve against the cheapest row. Leave a title empty to use each user\'s notification title instead.',
        settingsSaved: 'Settings saved.',
        invalidSettings: 'Invalid settings. Please check the highlighted values.',
        schemaOutdatedTitle: 'Database not ready',
        schemaOutdatedBody: 'The database schema is missing the required tables. Run the following command on the server, then reload this page:',
        schemaDbNotFound: 'The database was not found.',
        menuPredictions: 'Prediction accuracy',
        menuStats: 'Statistics',
        statsTitle: 'Command statistics',
        statsHint: 'Every run of the scheduled commands, with what it did and how long it took. A run is recorded once its database is open, so a failure before that — a bad flag, an unreachable server — leaves no row. `notify --dry-run` is not recorded: it delivers nothing.',
        statsNoTable: 'No runs have been recorded yet. Run `gasoline migrate` on the server to create the tables, then wait for the next scheduled command.',
        statsCommand: 'Command',
        statsAllCommands: 'All commands',
        statsRange: 'Range',
        statsTileRuns: 'Runs',
        statsTileSuccess: 'Success rate',
        statsTilePartial: 'Partial',
        statsTileFailed: 'Failed',
        statsTileInterrupted: 'Interrupted',
        statsTileMedian: 'Median duration',
        statsTileP95: 'p95 duration',
        statsTileLastRun: 'Last run',
        statsInterruptedHint: '"Interrupted" counts runs that recorded a start and never a finish — killed, out of memory, or still going. Nothing clears them later, so a run that takes longer than six hours stays counted.',
        statsChartTitle: 'Runs over time',
        statsNoData: 'No recorded runs match the current filters.',
        statsByCommand: 'By command',
        statsWork: 'Work done',
        statsWorkHint: 'The counters the commands report, summed over the filtered runs. Per run averages only over the runs that reported the metric, so suggest\u2019s persist counters are not diluted by runs without --persist.',
        statsRecent: 'Recent runs',
        statsTruncated: 'Showing the most recent 200 runs; the statistics above cover the full filtered set.',
        statsColCommand: 'Command',
        statsColRuns: 'Runs',
        statsColOk: 'OK',
        statsColPartial: 'Partial',
        statsColError: 'Failed',
        statsColAvg: 'Avg duration',
        statsColMax: 'Max duration',
        statsColLast: 'Last run',
        statsColMetric: 'Metric',
        statsColTotal: 'Total',
        statsColPerRun: 'Per run',
        statsColStarted: 'Started',
        statsColStatus: 'Status',
        statsColDuration: 'Duration',
        statsColHost: 'Host',
        statsColDetail: 'Detail',
        statsStatus_ok: 'OK',
        statsStatus_partial: 'Partial',
        statsStatus_error: 'Failed',
        statsStatus_running: 'Unfinished',
        statsLegendDuration: 'Avg duration',
        statsJustNow: 'just now',
        statsMinutesAgo: 'min ago',
        statsHoursAgo: 'h ago',
        statsDaysAgo: 'd ago',
        predAccuracyTitle: 'Prediction accuracy',
        predAccuracyHint: 'Compares each past prediction with the actual price recorded for that target window. Only evaluated predictions — whose target hour has passed and had a recorded price — are included. Errors are shown in cents (ct); bias is the mean signed error (actual − predicted), so a positive bias means predictions ran low.',
        predRange: 'Target range',
        predConfidence: 'Confidence',
        predConfAll: 'All',
        predConfMediumHigh: 'Medium + High',
        predChartTitle: 'Predicted vs. actual',
        predViewTimeline: 'Timeline',
        predViewScatter: 'Scatter',
        predNoData: 'No evaluated predictions match the current filters.',
        predByConfidence: 'Accuracy by confidence',
        predByLead: 'Accuracy by lead time',
        predLeadHint: 'How far ahead the prediction was made. Errors beyond six hours are dominated by price moves nobody could have known about, which is why only shorter leads train the bias correction.',
        predColBucket: 'Lead time',
        predByHour: 'Accuracy by hour (UTC)',
        predHourHint: 'Hours are the target hour in UTC, not your local time. A consistent bias in particular hours means the model misses that part of the daily price curve.',
        predColHour: 'Hour (UTC)',
        predDecTitle: 'Alert outcomes',
        predDecHint: 'What the check path decided, scored against the cheapest price that pricing day actually offered. Regret is how much more than the day\'s low the price was at the moment of the decision, so a good "buy" has a regret near zero. These are the model\'s decisions recorded on the suggestion timer, not a log of delivered notifications: per-user schedules, city selections and the repeat-suppression baseline are not reflected here.',
        predColRecommendation: 'Recommendation',
        predColRegret: 'Mean regret',
        predColHit1: 'Within 1 ct',
        predColHit2: 'Within 2 ct',
        predRec_buy: 'Buy',
        predRec_hold: 'Hold',
        predRec_wait: 'Wait',
        predRawTitle: 'Raw data',
        predTruncated: 'Showing the most recent 1,000 rows; the statistics above cover the full filtered set.',
        predColTarget: 'Target window',
        predColStation: 'Station',
        predColRunAt: 'Predicted at',
        predColLead: 'Lead time',
        predColConf: 'Confidence',
        predColCount: 'Count',
        predColPredicted: 'Predicted',
        predColActual: 'Actual',
        predColError: 'Error',
        predStatCount: 'Evaluated',
        predStatStations: 'Stations',
        predStatMae: 'MAE',
        predStatBias: 'Bias (act − pred)',
        predStatRmse: 'RMSE',
        predStatWithin1: 'Within ±1 ct',
        predStatWithin2: 'Within ±2 ct',
        predStatWorst: 'Worst error',
        predLatestHint: 'The tiles above count every stored prediction, so each target window appears once per hourly run and long leads dominate. These tiles keep only the latest prediction per station and window — the accuracy of acting on fresh output.',
        predStatLatestCount: 'Latest run: evaluated',
        predStatLatestMae: 'Latest run: MAE',
        predStatLatestBias: 'Latest run: bias',
        predStatLatestWithin2: 'Latest run: within ±2 ct',
        predConf_low: 'Low',
        predConf_medium: 'Medium',
        predConf_high: 'High',
        predLegendPredicted: 'Predicted',
        predLegendActual: 'Actual',
        predLegendBand: 'Error',
        predLegendPoint: 'Target hour',
        predLegendDiagonal: 'Perfect accuracy',
        predAxisPredicted: 'Predicted (€)',
        predAxisActual: 'Actual (€)',
        predSuggestion: 'This prediction was surfaced as a suggestion',
    },
    de: {
        title: 'Preisverlauf',
        filters: 'Filter',
        city: 'Stadt',
        enterCity: 'Stadt eingeben...',
        allCities: '— alle Städte —',
        location: 'Standort',
        enterLocation: 'Stadt oder Adresse...',
        useMyLocation: 'Meinen Standort verwenden',
        searchAddress: 'Adresse „{query}“ suchen',
        locating: 'Suche…',
        geocodeFailed: 'Standort konnte nicht ermittelt werden.',
        geocodeNoMatch: 'Zu dieser Adresse wurde nichts gefunden.',
        geocodeDisabled: 'Die Adresssuche ist auf diesem Server deaktiviert.',
        gpsUnsupported: 'Dieser Browser kann keinen Standort melden.',
        gpsDenied: 'Zugriff auf den Standort wurde abgelehnt.',
        gpsFailed: 'Standort konnte nicht bestimmt werden.',
        radius: 'Radius',
        from: 'Von',
        to: 'Bis',
        quickRange: 'Zeitraum',
        fuelType: 'Kraftstoffart',
        fuelAll: 'Alle',
        fuelDiesel: 'Diesel',
        fuelE5: 'E5',
        fuelE10: 'E10',
        stations: 'Tankstellen',
        filterStations: 'Tankstellen filtern...',
        reset: 'Zurücksetzen',
        statsTitle: 'Gewählter Zeitraum',
        snapshots: 'Einträge',
        stationsCount: 'Tankstellen',
        firstRecorded: 'Erste Aufzeichnung',
        lastRecorded: 'Letzte Aufzeichnung',
        priceTimeline: 'Preisverlauf',
        trend: 'Trend',
        trendPerDay: 'ct/Tag',
        trendHint: 'Linearer Trend über die gezeigten Tankstellen. Klicken zum Ausblenden.',
        trendLatest: 'Trend zum letzten Stand',
        station: 'Tankstelle',
        brand: 'Marke',
        openYes: 'offen',
        openNo: 'geschlossen',
        loading: 'Wird geladen…',
        showMore: 'Mehr anzeigen',
        loadError: 'Daten konnten nicht geladen werden. Bitte erneut versuchen.',
        retry: 'Erneut versuchen',
        cityNotFound: 'Ausgewählte Stadt nicht gefunden.',
        invalidFromDate: 'Ungültiges Von-Datum.',
        invalidToDate: 'Ungültiges Bis-Datum.',
        noSnapshots: 'Keine Einträge für die aktuellen Filter.',
        cheapestNow: 'Jetzt am günstigsten',
        cheapestNoData: 'Keine Preisdaten vorhanden.',
        cheapestPrefix: 'Tiefstpreis',
        cheapestRangeNoData: 'Keine Preisdaten vorhanden.',
        highestPrefix: 'Höchstpreis',
        highestNoData: 'Keine Preisdaten vorhanden.',
        rangeScopeHint: 'im Zeitraum',
        nearbyTitle: 'Umgebung',
        nearbyNoLocation: 'Standort links auswählen: Stadt, Adresse oder aktuelle Position.',
        nearbyNoData: 'Keine Stationen mit aktuellen Preisen in diesem Umkreis.',
        nearbyCapped: 'Die {shown} nächsten von {total} Stationen im Umkreis.',
        predictionsTitle: 'Tankempfehlungen',
        predictionsNoData: 'Keine kommenden Vorhersagen für diese Tankstellen in der Datenbank.',
        predictionsAsOf: 'Stand {time}',
        sdHint: 'Details zur Tankstelle anzeigen',
        sdTitle: 'Tankstellen-Details',
        sdCurrentPrices: 'Aktuelle Preise',
        sdAddress: 'Adresse',
        sdDistance: 'Entfernung',
        sdLastUpdate: 'Letzte Aktualisierung',
        sdNoPrices: 'Kein Preis-Snapshot für diese Tankstelle im gewählten Zeitraum.',
        sdUpcoming: 'Kommende Tankfenster',
        sdNoUpcoming: 'Keine kommenden Tankfenster',
        sdNavigate: 'Mit Google Maps navigieren',
        sdClose: 'Schließen',
        rangeAll: 'Alle',
        range24h: '24h',
        range30d: '30d',
        range14d: '14d',
        range7d: '7d',
        range3d: '3d',
        rangeToday: 'Heute',
        toggleTheme: 'Design wechseln',
        chartAriaLabel: 'Kraftstoffpreis-Verlaufsdiagramm',
        brandAriaLabel: 'Gasoline — Dashboard',
        openMenu: 'Menü öffnen',
        menuDashboard: 'Dashboard',
        menuAccount: 'Mein Konto',
        menuAdminSection: 'Admin',
        menuUsers: 'Benutzer',
        menuStations: 'Tankstellen',
        menuSettings: 'Einstellungen',
        menuLogout: 'Abmelden',
        loginTitle: 'Anmelden',
        registerTitle: 'Konto erstellen',
        registerHint: 'Deine E-Mail-Adresse ist dein Benutzername. Nach der Registrierung muss ein Administrator dein Konto freischalten, bevor du dich anmelden kannst.',
        email: 'E-Mail-Adresse',
        password: 'Passwort',
        passwordRepeat: 'Passwort wiederholen',
        signIn: 'Anmelden',
        createAccount: 'Konto erstellen',
        noAccountYet: 'Noch kein Konto?',
        haveAccount: 'Schon ein Konto?',
        unauthorized: 'Anmeldung erforderlich.',
        csrfError: 'Das Formular ist abgelaufen. Bitte erneut versuchen.',
        invalidCredentials: 'Ungültige E-Mail-Adresse oder falsches Passwort.',
        awaitingApproval: 'Dein Konto wartet auf die Freischaltung durch einen Administrator.',
        registerPendingSent: 'Konto erstellt. Du erhältst eine E-Mail, sobald ein Administrator dein Konto freigeschaltet hat.',
        accountCreated: 'Konto erstellt. Du kannst dich jetzt anmelden.',
        invalidEmail: 'Bitte eine gültige E-Mail-Adresse eingeben.',
        emailTaken: 'Ein Konto mit dieser E-Mail-Adresse existiert bereits.',
        passwordTooShort: 'Das Passwort muss mindestens 10 Zeichen lang sein.',
        passwordMismatch: 'Die Passwörter stimmen nicht überein.',
        wrongPassword: 'Das aktuelle Passwort ist falsch.',
        passwordChanged: 'Passwort geändert.',
        changePassword: 'Passwort ändern',
        currentPassword: 'Aktuelles Passwort',
        newPassword: 'Neues Passwort',
        save: 'Speichern',
        notifySettings: 'Benachrichtigungen',
        notifyMethod: 'Versandweg',
        pushoverAppName: 'Titel der Benachrichtigung',
        pushoverAppNameHint: 'Wird als Titel deiner Benachrichtigungen angezeigt, sofern kein Administrator eine Titel-Vorlage konfiguriert hat.',
        pushoverUserKey: 'Pushover User-Key',
        pushoverToken: 'Pushover API-Token',
        notifyDays: 'Wochentage',
        notifyWindows: 'Zeitfenster',
        notifySuggestTimes: 'Tägliche Vorschlagszeiten',
        notifySuggestEnabled: 'Tägliche Preisvorschläge senden',
        notifyCheckEnabled: 'Kaufalarme bei Preistiefs senden',
        notifyKindsHint: 'Wähle, welche Benachrichtigungen du erhältst. Vorschläge sagen günstige Tankzeiten voraus; Kaufalarme werden ausgelöst, wenn ein aktueller Preis fällt. Lass beide aus, um alle Benachrichtigungen zu pausieren.',
        notifyFuel: 'Kraftstoff für Benachrichtigungen',
        notifyFuelHint: 'Sie werden nur über diesen Kraftstoff benachrichtigt. Alle drei werden erfasst, jede Auswahl wird also bedient.',
        notifyLocation: 'Benachrichtigen rund um',
        notifyRadius: 'Radius (km)',
        notifyLocationHint: 'Benachrichtigungen umfassen alle erfassten Tankstellen innerhalb dieser Entfernung zur gewählten Stadt, und Entfernungen werden von dort gemessen. Tankstellen erscheinen erst, wenn die Aktualisierungsziele Ihres Administrators sie tatsächlich erfassen.',
        notifySaved: 'Benachrichtigungseinstellungen gespeichert.',
        invalidNotifySettings: 'Ungültige Benachrichtigungseinstellungen. Bitte Tage, Zeitfenster und Zeiten prüfen.',
        invalidNotifyLocation: 'Bitte eine Stadt aus den Vorschlägen und einen Radius zwischen 1 und 100 km wählen.',
        addWindow: 'Zeitfenster hinzufügen',
        addTime: 'Zeit hinzufügen',
        removeRow: 'Entfernen',
        day_mon: 'Mo',
        day_tue: 'Di',
        day_wed: 'Mi',
        day_thu: 'Do',
        day_fri: 'Fr',
        day_sat: 'Sa',
        day_sun: 'So',
        dangerZone: 'Gefahrenzone',
        deleteAccount: 'Konto löschen',
        deleteAccountConfirmLabel: 'Ich verstehe, dass mein Konto und meine Einstellungen dauerhaft gelöscht werden.',
        deleteAccountConfirm: 'Konto wirklich löschen? Das kann nicht rückgängig gemacht werden.',
        confirmRequired: 'Bitte die Löschung bestätigen.',
        lastAdminGuard: 'Du bist der letzte Administrator und kannst dieses Konto nicht löschen.',
        accountDeleted: 'Dein Konto wurde gelöscht.',
        loggedOut: 'Du wurdest abgemeldet.',
        adminUsersTitle: 'Benutzer',
        colEmail: 'E-Mail',
        colStatus: 'Status',
        colAdmin: 'Admin',
        colCreated: 'Registriert',
        colApproved: 'Freigeschaltet',
        colActions: 'Aktionen',
        statusPending: 'wartend',
        statusApproved: 'freigeschaltet',
        adminYes: 'Admin',
        actionApprove: 'Freischalten',
        actionDelete: 'Löschen',
        actionPromote: 'Zum Admin machen',
        actionDemote: 'Adminrechte entziehen',
        confirmDeleteUser: 'Diesen Benutzer wirklich löschen?',
        userApproved: 'Benutzer freigeschaltet.',
        userApprovedEmailFailed: 'Benutzer freigeschaltet, aber die Benachrichtigungs-E-Mail konnte nicht gesendet werden.',
        userDeleted: 'Benutzer gelöscht.',
        userPromoted: 'Benutzer ist jetzt Administrator.',
        userDemoted: 'Benutzer ist kein Administrator mehr.',
        cannotActOnSelf: 'Diese Aktion ist auf dem eigenen Konto nicht möglich.',
        notFound: 'Der angeforderte Eintrag wurde nicht gefunden.',
        updateTargets: 'Automatische Updates',
        updateTargetsHint: 'Diese Städte werden von gasoline update automatisch erfasst, wenn die CLI ohne --city/--radius aufgerufen wird. Sie bestimmen, welche Tankstellen es gibt; das Gebiet für Benachrichtigungen wählt jeder Nutzer separat.',
        targetCity: 'Stadt',
        targetRadius: 'Radius (km)',
        addTarget: 'Hinzufügen',
        removeTarget: 'Entfernen',
        noTargets: 'Noch keine Update-Ziele konfiguriert.',
        targetAdded: 'Update-Ziel hinzugefügt.',
        targetRemoved: 'Update-Ziel entfernt.',
        invalidTarget: 'Ungültige Stadt oder ungültiger Radius (1-25 km).',
        targetExists: 'Diese Stadt ist bereits ein Update-Ziel.',
        renameStation: 'Tankstelle umbenennen',
        renameStationHint: 'Der neue Name ersetzt den Tankerkönig-Namen überall — Dashboard, CLI-Ausgabe und Benachrichtigungen. Der Originalname bleibt erhalten und kann jederzeit wiederhergestellt werden.',
        stationSearchPlaceholder: 'Nach Name oder Adresse suchen...',
        newStationName: 'Neuer Name',
        applyRename: 'Übernehmen',
        renamedStations: 'Umbenannte Tankstellen',
        colNewName: 'Neuer Name',
        noRenames: 'Noch keine Tankstellen umbenannt.',
        removeRename: 'Entfernen',
        confirmRemoveRename: 'Diese Umbenennung wirklich entfernen und den Originalnamen wiederherstellen?',
        stationRenamed: 'Tankstelle umbenannt.',
        renameCleared: 'Umbenennung entfernt. Der Originalname wird wieder verwendet.',
        invalidRename: 'Bitte eine Tankstelle auswählen und einen neuen Namen eingeben.',
        templateCheck: 'Vorlage für Kaufalarme',
        templateSuggest: 'Vorlage für Vorschläge',
        notificationTexts: 'Benachrichtigungstexte',
        notificationTextsHint: 'Vorschläge und Prüfungen werden für jeden Kraftstoff berechnet und umfassen alle Tankstellen, die von den Aktualisierungszielen oben derzeit erfasst werden. Nur diese Vorlagen werden hier konfiguriert; Kraftstoff, Gebiet und Zeitplan wählt jeder Nutzer im eigenen Konto.',
        templateCheckTitle: 'Titel für Kaufalarme',
        templateSuggestTitle: 'Titel für Vorschläge',
        titleTemplatePlaceholder: 'z. B. Tanken für {{cheapest_current_price_formatted}} EUR',
        templatePlaceholdersHint: 'Vorlagen nutzen die {{placeholder}}-Syntax mit dem vollen gasoline-watch-Satz, z. B. {{station_name}}, {{price}}, {{price_formatted}}, {{fuel}}, {{date}}, {{start_time}}, {{end_time}}, {{distance}}, {{confidence}}, {{count}}, {{cheapest_price}}, {{message}} und *_onchange-Varianten. \\n fügt einen Zeilenumbruch ein.',
        titleTemplatesHint: 'Titel-Vorlagen nutzen dieselben Platzhalter; Zeilen-Platzhalter beziehen sich auf die günstigste Zeile. Leer lassen, um den Benachrichtigungstitel des jeweiligen Benutzers zu verwenden.',
        settingsSaved: 'Einstellungen gespeichert.',
        invalidSettings: 'Ungültige Einstellungen. Bitte die markierten Werte prüfen.',
        schemaOutdatedTitle: 'Datenbank nicht bereit',
        schemaOutdatedBody: 'Im Datenbankschema fehlen die benötigten Tabellen. Führe folgenden Befehl auf dem Server aus und lade die Seite neu:',
        schemaDbNotFound: 'Die Datenbank wurde nicht gefunden.',
        menuPredictions: 'Vorhersagegenauigkeit',
        menuStats: 'Statistiken',
        statsTitle: 'Befehlsstatistiken',
        statsHint: 'Jeder Lauf der geplanten Befehle, mit dem, was er getan hat, und wie lange er gedauert hat. Ein Lauf wird erst aufgezeichnet, wenn seine Datenbank offen ist — ein Fehler davor, etwa ein falsches Flag oder ein nicht erreichbarer Server, hinterlässt keine Zeile. `notify --dry-run` wird nicht aufgezeichnet: dabei wird nichts zugestellt.',
        statsNoTable: 'Es wurden noch keine Läufe aufgezeichnet. Führe `gasoline migrate` auf dem Server aus, um die Tabellen anzulegen, und warte auf den nächsten geplanten Befehl.',
        statsCommand: 'Befehl',
        statsAllCommands: 'Alle Befehle',
        statsRange: 'Zeitraum',
        statsTileRuns: 'Läufe',
        statsTileSuccess: 'Erfolgsquote',
        statsTilePartial: 'Teilweise',
        statsTileFailed: 'Fehlgeschlagen',
        statsTileInterrupted: 'Abgebrochen',
        statsTileMedian: 'Median-Dauer',
        statsTileP95: 'p95-Dauer',
        statsTileLastRun: 'Letzter Lauf',
        statsInterruptedHint: '„Abgebrochen“ zählt Läufe, die einen Start, aber nie ein Ende aufgezeichnet haben — beendet, ohne Speicher oder noch laufend. Nichts räumt sie später auf, ein Lauf über sechs Stunden bleibt also gezählt.',
        statsChartTitle: 'Läufe im Zeitverlauf',
        statsNoData: 'Keine aufgezeichneten Läufe passen zu den aktuellen Filtern.',
        statsByCommand: 'Nach Befehl',
        statsWork: 'Geleistete Arbeit',
        statsWorkHint: 'Die Zähler, die die Befehle melden, summiert über die gefilterten Läufe. „Pro Lauf“ mittelt nur über die Läufe, die den Zähler gemeldet haben — die Persist-Zähler von suggest werden also nicht durch Läufe ohne --persist verwässert.',
        statsRecent: 'Letzte Läufe',
        statsTruncated: 'Es werden die letzten 200 Läufe angezeigt; die Statistiken oben umfassen die gesamte gefilterte Menge.',
        statsColCommand: 'Befehl',
        statsColRuns: 'Läufe',
        statsColOk: 'OK',
        statsColPartial: 'Teilweise',
        statsColError: 'Fehlgeschlagen',
        statsColAvg: 'Ø Dauer',
        statsColMax: 'Max. Dauer',
        statsColLast: 'Letzter Lauf',
        statsColMetric: 'Kennzahl',
        statsColTotal: 'Summe',
        statsColPerRun: 'Pro Lauf',
        statsColStarted: 'Gestartet',
        statsColStatus: 'Status',
        statsColDuration: 'Dauer',
        statsColHost: 'Host',
        statsColDetail: 'Detail',
        statsStatus_ok: 'OK',
        statsStatus_partial: 'Teilweise',
        statsStatus_error: 'Fehlgeschlagen',
        statsStatus_running: 'Unbeendet',
        statsLegendDuration: 'Ø Dauer',
        statsJustNow: 'gerade eben',
        statsMinutesAgo: 'Min. her',
        statsHoursAgo: 'Std. her',
        statsDaysAgo: 'Tage her',
        predAccuracyTitle: 'Vorhersagegenauigkeit',
        predAccuracyHint: 'Vergleicht jede vergangene Vorhersage mit dem tatsächlich aufgezeichneten Preis im jeweiligen Zielfenster. Nur ausgewertete Vorhersagen — deren Zielstunde vorbei ist und für die ein Preis vorlag — werden berücksichtigt. Fehler werden in Cent (ct) angezeigt; der Bias ist der mittlere vorzeichenbehaftete Fehler (Ist − Vorhersage), ein positiver Bias bedeutet also zu niedrige Vorhersagen.',
        predRange: 'Zeitraum (Ziel)',
        predConfidence: 'Konfidenz',
        predConfAll: 'Alle',
        predConfMediumHigh: 'Mittel + Hoch',
        predChartTitle: 'Vorhersage vs. Ist',
        predViewTimeline: 'Zeitverlauf',
        predViewScatter: 'Streudiagramm',
        predNoData: 'Keine ausgewerteten Vorhersagen für die aktuellen Filter.',
        predByConfidence: 'Genauigkeit nach Konfidenz',
        predByLead: 'Genauigkeit nach Vorlaufzeit',
        predLeadHint: 'Wie weit im Voraus die Vorhersage erstellt wurde. Fehler jenseits von sechs Stunden werden von Preisbewegungen bestimmt, die niemand vorhersehen konnte — deshalb fließen nur kürzere Vorlaufzeiten in die Bias-Korrektur ein.',
        predColBucket: 'Vorlaufzeit',
        predByHour: 'Genauigkeit nach Stunde (UTC)',
        predHourHint: 'Die Stunden beziehen sich auf die Zielstunde in UTC, nicht auf Ihre lokale Zeit. Ein durchgängiger Bias in bestimmten Stunden bedeutet, dass das Modell diesen Teil des Tagesverlaufs verfehlt.',
        predColHour: 'Stunde (UTC)',
        predDecTitle: 'Ergebnisse der Kaufempfehlungen',
        predDecHint: 'Was der Preis-Check entschieden hat, gemessen am günstigsten Preis, den der jeweilige Preistag tatsächlich geboten hat. Die Differenz zum Tagestief gibt an, wie viel mehr als das Tagestief der Preis im Moment der Entscheidung betrug — eine gute Kaufempfehlung liegt also nahe null. Erfasst sind die Entscheidungen des Modells zum Zeitpunkt des Vorschlags-Timers, nicht die tatsächlich versendeten Benachrichtigungen: persönliche Zeitfenster, Städteauswahl und die Wiederholungssperre sind hier nicht berücksichtigt.',
        predColRecommendation: 'Empfehlung',
        predColRegret: 'Mittlere Differenz zum Tagestief',
        predColHit1: 'Innerhalb 1 ct',
        predColHit2: 'Innerhalb 2 ct',
        predRec_buy: 'Kaufen',
        predRec_hold: 'Abwarten',
        predRec_wait: 'Warten',
        predRawTitle: 'Rohdaten',
        predTruncated: 'Es werden die neuesten 1.000 Zeilen angezeigt; die Statistiken oben umfassen den vollständigen gefilterten Datensatz.',
        predColTarget: 'Zielfenster',
        predColStation: 'Tankstelle',
        predColRunAt: 'Vorhergesagt am',
        predColLead: 'Vorlaufzeit',
        predColConf: 'Konfidenz',
        predColCount: 'Anzahl',
        predColPredicted: 'Vorhergesagt',
        predColActual: 'Tatsächlich',
        predColError: 'Fehler',
        predStatCount: 'Ausgewertet',
        predStatStations: 'Tankstellen',
        predStatMae: 'MAE',
        predStatBias: 'Bias (Ist − Vorh.)',
        predStatRmse: 'RMSE',
        predStatWithin1: 'Innerh. ±1 ct',
        predStatWithin2: 'Innerh. ±2 ct',
        predStatWorst: 'Größter Fehler',
        predLatestHint: 'Die Kacheln oben zählen jede gespeicherte Vorhersage — jedes Zielfenster erscheint also einmal pro stündlichem Lauf, und lange Vorlaufzeiten dominieren. Diese Kacheln behalten nur die neueste Vorhersage je Tankstelle und Fenster: die Genauigkeit, wenn man auf frische Ausgaben reagiert.',
        predStatLatestCount: 'Neuester Lauf: ausgewertet',
        predStatLatestMae: 'Neuester Lauf: MAE',
        predStatLatestBias: 'Neuester Lauf: Bias',
        predStatLatestWithin2: 'Neuester Lauf: innerh. ±2 ct',
        predConf_low: 'Niedrig',
        predConf_medium: 'Mittel',
        predConf_high: 'Hoch',
        predLegendPredicted: 'Vorhergesagt',
        predLegendActual: 'Tatsächlich',
        predLegendBand: 'Fehler',
        predLegendPoint: 'Zielstunde',
        predLegendDiagonal: 'Perfekte Genauigkeit',
        predAxisPredicted: 'Vorhergesagt (€)',
        predAxisActual: 'Tatsächlich (€)',
        predSuggestion: 'Diese Vorhersage wurde als Empfehlung angezeigt',
    },
};

currentLang = (() => {
    const stored = localStorage.getItem('lang');
    if (stored && translations[stored]) return stored;
    const browser = (navigator.language || 'en').slice(0, 2).toLowerCase();
    return translations[browser] ? browser : 'en';
})();

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

function formatTimeOnly(isoString) {
    return new Date(isoString).toLocaleTimeString(_loc(), {
        timeZone: _tz(),
        hour: '2-digit', minute: '2-digit',
        hour12: false,
    });
}

/* ── Number & price formatting ─────────────────────────────────────
   Display only — nothing here touches stored or submitted values.

   German pump boards show the tenth-of-a-cent digit raised and smaller
   (2.09⁹), so every three-decimal euro price in the UI is rendered that
   way, and the decimal separator is set a size smaller than the digits it
   parts. The separator itself follows the UI language: comma for de, dot
   for en. Form inputs keep the dot, since that is what the server parses.

   Every formatter comes in two flavours. The plain ones return text for
   title attributes, aria labels and <option> labels, which cannot carry
   markup and therefore keep a full-size separator; the *Html twins wrap
   the separator in a span for anything rendered as markup. SVG <text> gets
   a third treatment — tspans, since CSS classes cannot size a tspan's
   font relative to the label it sits in.

   The prediction-accuracy page opts out and formats its numbers as plain
   text throughout: its stat tiles and tables break a cell's children into
   separate columns on narrow screens, which pulls a raised digit or a
   sized-down separator out of the number it belongs to.
   ──────────────────────────────────────────────────────────────── */
const SVG_NS = 'http://www.w3.org/2000/svg';

// Kept in step with the .price-milli and .price-sep rules: the raised digit
// at 0.62em lifted 0.48 of its own em (0.62 × 0.48 ≈ 0.30 of the full size),
// and the separator at 0.7em.
const MILLI_SCALE = 0.62;
const MILLI_RISE  = 0.30;
const SEP_SCALE   = 0.7;

function decimalSeparator() { return currentLang === 'de' ? ',' : '.'; }

// The separator, sized down. Digits and separators only, so nothing here
// ever needs escaping.
function separatorHtml() { return '<span class="price-sep">' + decimalSeparator() + '</span>'; }

// Fixed-decimal number in the active locale's separator. Returns null for
// anything that is not a finite number, so callers can pick their own dash.
function fmtDecimal(v, digits) {
    if (v === null || v === undefined || v === '') return null;
    const n = Number(v);
    if (!Number.isFinite(n)) return null;
    return n.toFixed(digits).replace('.', decimalSeparator());
}

// Same number with the separator sized down.
function fmtDecimalHtml(v, digits) {
    const s = fmtDecimal(v, digits);
    return s === null ? null : s.replace(decimalSeparator(), separatorHtml());
}

function fmtDistanceKm(v) {
    const s = fmtDecimal(v, 1);
    return s === null ? null : s + ' km';
}

function fmtDistanceKmHtml(v) {
    const s = fmtDecimalHtml(v, 1);
    return s === null ? null : s + ' km';
}

// Splits a price into the part shown at normal size and the raised
// tenth-of-a-cent digit. null when there is no number to show.
function priceParts(v) {
    if (v === null || v === undefined || v === '') return null;
    const n = Number(v);
    if (!Number.isFinite(n)) return null;
    const s = n.toFixed(3);
    return { head: s.slice(0, -1).replace('.', decimalSeparator()), milli: s.slice(-1) };
}

// Plain text — title attributes, aria labels, anything not parsed as HTML.
function fmtPriceText(v, fallback) {
    const p = priceParts(v);
    if (p === null) return fallback === undefined ? '—' : fallback;
    return p.head + p.milli;
}

// HTML with the third decimal raised and the separator sized down. Digits
// and separators only, so the result never needs escaping; the fallback is
// escaped by the caller.
function fmtPriceHtml(v, fallback) {
    const p = priceParts(v);
    if (p === null) return fallback === undefined ? '—' : fallback;
    return p.head.replace(decimalSeparator(), separatorHtml())
        + '<span class="price-milli">' + p.milli + '</span>';
}

function svgTspan(textEl, text, attrs) {
    const el = document.createElementNS(SVG_NS, 'tspan');
    for (const name in (attrs || {})) el.setAttribute(name, attrs[name]);
    el.textContent = text;
    textEl.appendChild(el);
    return el;
}

// Digits at label size with the separator sized down, as tspans. Splits on
// the first separator only; the caller's string holds a single number.
function fillSvgDigits(textEl, s, fontSize) {
    const sep = decimalSeparator();
    const at = s.indexOf(sep);
    if (at < 0) { svgTspan(textEl, s); return; }
    svgTspan(textEl, s.slice(0, at));
    svgTspan(textEl, sep, { 'font-size': (fontSize * SEP_SCALE).toFixed(2) });
    svgTspan(textEl, s.slice(at + 1));
}

// Same treatment inside an SVG <text>, where CSS classes cannot size a tspan
// against its label: the tenth-of-a-cent digit shrunk and lifted off the
// baseline by the proportions the CSS rules use, and the separator shrunk.
// The raised digit goes last, so its dy needs no counterpart.
function fillSvgPrice(textEl, v, fontSize, fallback) {
    const p = priceParts(v);
    textEl.textContent = '';
    if (p === null) {
        textEl.textContent = fallback === undefined ? '—' : fallback;
        return textEl;
    }
    fillSvgDigits(textEl, p.head, fontSize);
    svgTspan(textEl, p.milli, {
        'font-size': (fontSize * MILLI_SCALE).toFixed(2),
        dy: (-fontSize * MILLI_RISE).toFixed(2),
    });
    return textEl;
}

/* ── Crosshair reading placement ─────────────────────────────────────
   The reading is parked in the free strip below the plot (above it when
   there is more room up there) instead of following the pointer across the
   lines, so the chart stays visible the whole time the crosshair is up.
   Its height is capped to that strip; a station list too tall for it is
   spread over columns (wide screens) and tightened (narrow ones) rather
   than allowed to grow back over the plot. Horizontally it tracks the
   pointer, clamped to the viewport. */
function positionChartTooltip(plotEl, clientX, clientY) {
    const tip = document.getElementById('price-tooltip');
    if (!tip || !plotEl) return;

    const GAP = 10;      // breathing room between plot and reading
    const EDGE = 8;      // keep clear of the viewport edges
    const MIN_FIT = 120; // strip too thin to be worth using (~4 rows)
    const COL_W = 240;   // target width per column when columns are used
    const MAX_COLS = 5;

    tip.classList.remove('tt-cols', 'tt-dense');
    tip.style.columnCount = '';
    tip.style.width = '';

    const plot = plotEl.getBoundingClientRect();
    const below = window.innerHeight - plot.bottom - GAP - EDGE;
    const above = plot.top - GAP - EDGE;
    const under = below >= above;
    const strip = under ? below : above;

    // Neither strip can hold a readable list (a very short window, or a plot
    // filling it): fall back to following the pointer, since there is nowhere
    // to put the reading that would keep the plot clear anyway.
    if (strip < MIN_FIT) {
        tip.style.maxHeight = (window.innerHeight - 2 * EDGE) + 'px';
        tip.style.left = (clientX + 14) + 'px';
        tip.style.top  = (clientY - 14) + 'px';
        const r = tip.getBoundingClientRect();
        if (r.right  > window.innerWidth  - EDGE) tip.style.left = Math.max(EDGE, window.innerWidth  - r.width  - EDGE) + 'px';
        if (r.bottom > window.innerHeight - EDGE) tip.style.top  = Math.max(EDGE, window.innerHeight - r.height - EDGE) + 'px';
        return;
    }

    const maxH = Math.floor(strip);
    const availW = Math.max(COL_W, window.innerWidth - 2 * EDGE);
    tip.style.maxHeight = maxH + 'px';

    // How many rows one column of the strip holds, and how many columns the
    // list therefore needs. Measured single-column: scrollHeight reports the
    // untruncated height while the overflow is clipped, but once the box is
    // multi-column the surplus spills into columns outside it and scrollHeight
    // stops growing, so it could no longer tell what fits.
    const measure = () => {
        const rows = tip.querySelectorAll('.tt-row');
        const n = rows.length;
        const rowH = n > 0 ? rows[0].getBoundingClientRect().height : 0;
        // Everything that is not a row: the box padding plus the timestamp
        // line, which spans all columns and so costs each of them its height.
        const chrome = tip.scrollHeight - n * rowH;
        const perCol = rowH > 0 ? Math.max(1, Math.floor((maxH - chrome) / rowH)) : n;
        return { need: Math.max(1, Math.ceil(n / perCol)) };
    };

    if (tip.scrollHeight > maxH) {
        const maxCols = Math.max(1, Math.min(MAX_COLS, Math.floor(availW / COL_W)));
        let need = measure().need;
        // More columns than the width allows: tighten the rows too, so a long
        // list keeps its tail instead of losing it to the clip.
        if (need > maxCols) {
            tip.classList.add('tt-dense');
            need = measure().need;
        }
        const cols = Math.min(maxCols, need);
        if (cols >= 2) {
            tip.classList.add('tt-cols');
            tip.style.columnCount = String(cols);
            tip.style.width = Math.min(availW, cols * COL_W) + 'px';
        }
    }

    const box = tip.getBoundingClientRect();
    const left = Math.min(Math.max(clientX - box.width / 2, EDGE),
                          Math.max(EDGE, window.innerWidth - box.width - EDGE));
    tip.style.left = Math.round(left) + 'px';
    tip.style.top  = Math.round(under ? plot.bottom + GAP
                                      : plot.top - GAP - box.height) + 'px';
}

/* ── Long-press crosshair for touch ─────────────────────────────────
   On touch devices the chart crosshair/tooltip appears only after a
   long-press on the plot, so a swipe across a chart scrolls the page
   instead of getting swallowed by the tooltip. Once the press activates,
   moving the finger drags the crosshair (and scrolling is suppressed for
   that gesture). A tap or a scroll-intent move cancels the press. After
   the finger lifts the tooltip stays readable, then hides on its own
   after IDLE_MS without further touch interaction. Mouse hover is
   untouched, so desktop behaves exactly as before. */
function attachLongPressCrosshair(el, show, hide) {
    const HOLD_MS = 450;
    const SLOP_PX = 10;   // finger drift allowed before it counts as a scroll
    const IDLE_MS = 3000; // lifted-finger grace before the tooltip auto-hides

    let timer = null;
    let idleTimer = null;
    let active = false;       // finger down and crosshair engaged
    let shownByTouch = false; // tooltip on screen because of a long-press
    let startX = 0, startY = 0, lastX = 0, lastY = 0;
    const cancelHold = () => { if (timer !== null) { clearTimeout(timer); timer = null; } };
    const cancelIdle = () => { if (idleTimer !== null) { clearTimeout(idleTimer); idleTimer = null; } };
    const hideNow = () => { cancelIdle(); shownByTouch = false; hide(); };
    // No countdown while the finger is down — holding still IS interacting.
    const scheduleIdleHide = () => { cancelIdle(); idleTimer = setTimeout(hideNow, IDLE_MS); };

    el.addEventListener('touchstart', (e) => {
        active = false;
        cancelHold();
        cancelIdle();
        if (e.touches.length !== 1) return; // pinch/second finger: not a press
        startX = lastX = e.touches[0].clientX;
        startY = lastY = e.touches[0].clientY;
        timer = setTimeout(() => {
            timer = null;
            active = true;
            shownByTouch = true;
            show(lastX, lastY);
        }, HOLD_MS);
    }, { passive: true });

    // Non-passive so an ACTIVE crosshair drag can suppress scrolling; before
    // activation the default is left alone and real movement cancels the hold.
    el.addEventListener('touchmove', (e) => {
        const touch = e.touches[0];
        if (!touch) return;
        lastX = touch.clientX;
        lastY = touch.clientY;
        if (active) {
            e.preventDefault();
            show(lastX, lastY);
        } else if (Math.abs(lastX - startX) > SLOP_PX || Math.abs(lastY - startY) > SLOP_PX) {
            cancelHold();
        }
    }, { passive: false });

    const endPress = () => {
        cancelHold();
        active = false;
        // Also re-arms after a later tap on the chart while the tooltip is
        // still up, so it can never get stuck on screen.
        if (shownByTouch) scheduleIdleHide();
    };
    el.addEventListener('touchend', endPress);
    el.addEventListener('touchcancel', endPress);

    // Android fires contextmenu on long-press; swallow it only while a touch
    // press is pending or active so desktop right-click stays intact.
    el.addEventListener('contextmenu', (e) => {
        if (active || timer !== null) e.preventDefault();
    });
}

function applyLang(lang) {
    currentLang = lang;
    localStorage.setItem('lang', lang);
    const t = translations[lang];
    document.querySelectorAll('[data-i18n]').forEach((el) => {
        const key = el.dataset.i18n;
        if (t[key] !== undefined) el.textContent = t[key];
    });
    document.querySelectorAll('[data-i18n-placeholder]').forEach((el) => {
        const key = el.dataset.i18nPlaceholder;
        if (t[key] !== undefined) el.setAttribute('placeholder', t[key]);
    });
    document.querySelectorAll('[data-i18n-aria-label]').forEach((el) => {
        const key = el.dataset.i18nAriaLabel;
        if (t[key] !== undefined) el.setAttribute('aria-label', t[key]);
    });
    document.querySelectorAll('[data-i18n-title]').forEach((el) => {
        const key = el.dataset.i18nTitle;
        if (t[key] !== undefined) el.setAttribute('title', t[key]);
    });
    // Stacked-table row labels (mobile card layout).
    document.querySelectorAll('[data-i18n-label]').forEach((el) => {
        const key = el.dataset.i18nLabel;
        if (t[key] !== undefined) el.setAttribute('data-label', t[key]);
    });
    document.querySelectorAll('.lang-btn').forEach((btn) => {
        btn.classList.toggle('active', btn.dataset.lang === lang);
    });
    // Re-format all date/time cells
    document.querySelectorAll('[data-recorded-at]').forEach((el) => {
        el.textContent = formatDateTime(el.dataset.recordedAt);
    });
    // Re-format distances rendered once server-side (station picker rows),
    // which the page-specific renderers do not rebuild.
    document.querySelectorAll('[data-dist-km]').forEach((el) => {
        const base = el.dataset.nameBase || '';
        const dist = fmtDistanceKmHtml(el.dataset.distKm);
        if (dist === null) return;
        // The station name goes in as text and the distance as markup, which
        // saves escaping a name this script has no escaper for.
        el.textContent = base === '' ? '' : base + ' ';
        el.insertAdjacentHTML('beforeend', base === '' ? dist : `(${dist})`);
    });
    // Page-specific re-rendering (e.g. the dashboard chart) hooks in here.
    if (typeof window.onLangChange === 'function') window.onLangChange();
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
    if (themeToggle) themeToggle.innerHTML = theme === 'light' ? moonIcon : sunIcon;
    if (typeof window.onThemeChange === 'function') window.onThemeChange();
}

if (themeToggle) {
    themeToggle.addEventListener('click', () => {
        const current = document.documentElement.getAttribute('data-theme') || 'dark';
        applyTheme(current === 'dark' ? 'light' : 'dark');
    });
}

// Sync icon to current theme (set by head script)
applyTheme(document.documentElement.getAttribute('data-theme') || 'dark');

/* ── Hamburger menu ────────────────────────────────────────────── */
const menuToggle = document.getElementById('menu-toggle');
const menuPanel = document.getElementById('app-menu');

if (menuToggle && menuPanel) {
    const closeMenu = () => {
        menuPanel.hidden = true;
        menuToggle.setAttribute('aria-expanded', 'false');
    };
    menuToggle.addEventListener('click', (e) => {
        e.stopPropagation();
        const open = menuPanel.hidden;
        menuPanel.hidden = !open;
        menuToggle.setAttribute('aria-expanded', open ? 'true' : 'false');
    });
    document.addEventListener('click', (e) => {
        if (!menuPanel.hidden && !menuPanel.contains(e.target)) closeMenu();
    });
    document.addEventListener('keydown', (e) => {
        if (e.key === 'Escape') closeMenu();
    });
}

/* ── Confirm dialogs for destructive forms ─────────────────────── */
document.querySelectorAll('form[data-confirm]').forEach((form) => {
    form.addEventListener('submit', (e) => {
        const key = form.dataset.confirm;
        const message = translations[currentLang][key] || key;
        if (!window.confirm(message)) e.preventDefault();
    });
});

/* ── Schedule editor row controls (account/admin pages) ────────── */
function scheduleRow(kind) {
    const row = document.createElement('div');
    row.className = 'row-item';
    const removeLabel = translations[currentLang].removeRow || 'Remove';
    const timeInput = (name) => '<input type="text" class="time-input" name="' + name + '" required ' +
        'maxlength="5" pattern="([01][0-9]|2[0-3]):[0-5][0-9]" placeholder="HH:MM" title="HH:MM">';
    if (kind === 'window') {
        row.innerHTML = timeInput('notify_windows_from[]') + ' <span>–</span> ' +
            timeInput('notify_windows_to[]') + ' ' +
            '<button type="button" class="btn-row-remove" aria-label="' + removeLabel + '">×</button>';
    } else {
        row.innerHTML = timeInput('notify_suggest_times[]') + ' ' +
            '<button type="button" class="btn-row-remove" aria-label="' + removeLabel + '">×</button>';
    }
    return row;
}

document.querySelectorAll('.btn-row-add').forEach((btn) => {
    btn.addEventListener('click', () => {
        const list = btn.dataset.addRow === 'window'
            ? document.getElementById('window-list')
            : document.getElementById('suggest-time-list');
        if (list) list.appendChild(scheduleRow(btn.dataset.addRow));
    });
});

document.addEventListener('click', (e) => {
    if (e.target.matches('.btn-row-remove')) {
        e.target.closest('.row-item')?.remove();
    }
});
</script>
<?php
}

// ── Dashboard page (default) ──────────────────────────────────────────────────
renderDocumentHead('Price History');
?>
<body>
<div id="price-tooltip" role="tooltip" aria-hidden="true"></div>
<!-- Station detail dialog; filled in by openStationDialog() on click/tap. -->
<dialog id="station-dialog" class="station-dialog" aria-labelledby="sd-name"></dialog>
<main class="page">

<?php renderHeader($currentUser, 'dashboard'); ?>

    <?php $dashboardFlash = takeFlash(); if ($dashboardFlash !== null) { ?>
    <div class="<?= $dashboardFlash['type'] === 'error' ? 'error-box' : 'success-box' ?>" data-i18n="<?= h($dashboardFlash['key']) ?>"><?= h(flashText($dashboardFlash['key'])) ?></div>
    <?php } ?>

    <!-- Main layout -->
    <div class="layout">

        <!-- Sidebar / filters -->
        <aside class="sidebar<?= $filtersCollapsed ? ' collapsed' : '' ?>" id="filters-sidebar">
            <div class="sidebar-head" id="filters-toggle" role="button" tabindex="0" aria-expanded="<?= $filtersCollapsed ? 'false' : 'true' ?>">
                <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" style="color:var(--muted)"><line x1="4" y1="6" x2="20" y2="6"/><line x1="8" y1="12" x2="16" y2="12"/><line x1="11" y1="18" x2="13" y2="18"/></svg>
                <h2 data-i18n="filters">Filters</h2>
                <span class="sidebar-chevron" aria-hidden="true"><svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="6 9 12 15 18 9"/></svg></span>
            </div>

            <?php // Saved against the account, not the URL: every change posts the
                  // whole sidebar and comes back to the bare dashboard. ?>
            <form method="post" action="" id="filters-form">
                <?= csrfField() ?>
                <input type="hidden" name="action" value="save_filters">
                <div class="field">
                    <label for="f-city" data-i18n="location">Location</label>
                    <div class="city-ac" id="city-ac">
                        <div class="loc-row">
                            <input
                                type="text"
                                id="f-city"
                                class="city-ac-input"
                                data-i18n-placeholder="enterLocation"
                                placeholder="City or address..."
                                autocomplete="off"
                                spellcheck="false"
                                value="<?= h($selectedCityRow ? (string) $selectedCityRow['display_name'] : '') ?>"
                                aria-autocomplete="list"
                                aria-controls="city-ac-list"
                                aria-expanded="false"
                            >
                            <?php // One tap, one position fix, one reverse lookup - see the geocode handler. ?>
                            <button type="button" class="loc-gps" id="f-locate" data-i18n-title="useMyLocation" data-i18n-aria-label="useMyLocation" title="Use my location" aria-label="Use my location">
                                <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="12" cy="12" r="7"/><circle cx="12" cy="12" r="2.5" fill="currentColor" stroke="none"/><line x1="12" y1="1.5" x2="12" y2="4.5"/><line x1="12" y1="19.5" x2="12" y2="22.5"/><line x1="1.5" y1="12" x2="4.5" y2="12"/><line x1="19.5" y1="12" x2="22.5" y2="12"/></svg>
                            </button>
                        </div>
                        <?php // The text box is deliberately unnamed. What gets posted is
                              // the last *resolved* place, so changing the fuel while a
                              // half-typed address sits in the box cannot move the reader
                              // somewhere they never picked. ?>
                        <input type="hidden" name="location_label" id="f-loc-label" value="<?= h($selectedLocation) ?>">
                        <input type="hidden" name="location_lat" id="f-loc-lat" value="<?= $selectedCityRow ? h(sprintf('%.6f', (float) $selectedCityRow['lat'])) : '' ?>">
                        <input type="hidden" name="location_lng" id="f-loc-lng" value="<?= $selectedCityRow ? h(sprintf('%.6f', (float) $selectedCityRow['lng'])) : '' ?>">
                        <ul class="city-ac-list" id="city-ac-list" role="listbox" hidden></ul>
                    </div>
                </div>

                <div class="field">
                    <label for="f-radius" data-i18n="radius">Radius</label>
                    <select
                        name="radius_km"
                        id="f-radius"
                        onchange="this.form.submit()"
                        <?= $selectedLocation === '' ? 'disabled' : '' ?>
                    >
                        <?php foreach ($validRadiusOptions as $radiusOption): ?>
                            <option value="<?= h((string) $radiusOption) ?>" <?= $selectedRadiusKm === $radiusOption ? 'selected' : '' ?>>
                                <?= h((string) $radiusOption . ' km') ?>
                            </option>
                        <?php endforeach; ?>
                    </select>
                </div>

                <div class="field">
                    <label data-i18n="quickRange">Quick range</label>
                    <div class="quick-ranges">
                        <button type="button" class="quick-range-btn" data-range="7d"  data-i18n="range7d">7d</button>
                        <button type="button" class="quick-range-btn" data-range="14d" data-i18n="range14d">14d</button>
                        <button type="button" class="quick-range-btn" data-range="30d" data-i18n="range30d">30d</button>
                    </div>
                    <input type="hidden" name="range" id="f-range" value="<?= h($selectedRange) ?>">
                </div>

                <div class="field">
                    <label for="f-from" data-i18n="from">From</label>
                    <input type="date" name="from" id="f-from" value="<?= $selectedRange === '' ? h($fromDate) : '' ?>" onchange="onDateChange(this)">
                </div>

                <div class="field">
                    <label for="f-to" data-i18n="to">To</label>
                    <input type="date" name="to" id="f-to" value="<?= $selectedRange === '' ? h($toDate) : '' ?>" onchange="onDateChange(this)">
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
                    <label for="f-station-filter" data-i18n="stations">Stations</label>
                    <div class="station-picker">
                        <input
                            type="text"
                            id="f-station-filter"
                            class="station-filter-input"
                            data-i18n-placeholder="filterStations"
                            placeholder="Filter stations..."
                            autocomplete="off"
                            spellcheck="false"
                        >
                        <div class="station-options" id="station-options">
                            <?php foreach ($stations as $station): ?>
                                <?php
                                $stationId = (string) $station['id'];
                                // The distance is re-formatted client-side per language,
                                // so hand over the raw value and the label without it.
                                $stationDistKm = $station['selected_dist_km'] ?? null;
                                $distAttrs = $stationDistKm === null ? '' : sprintf(
                                    ' data-name-base="%s" data-dist-km="%s"',
                                    h(stationLabel($station, false)),
                                    h(number_format((float) $stationDistKm, 1, '.', ''))
                                );
                                ?>
                                <label class="station-option">
                                    <input
                                        type="checkbox"
                                        name="station_ids[]"
                                        value="<?= h($stationId) ?>"
                                        onchange="this.form.submit()"
                                        <?= in_array($stationId, $selectedStationIds, true) ? 'checked' : '' ?>
                                    >
                                    <span class="station-option-label"<?= $distAttrs ?>><?= h(stationLabel($station)) ?></span>
                                </label>
                            <?php endforeach; ?>
                        </div>
                    </div>
                </div>
            </form>

            <div class="sidebar-actions">
                <form method="post" action="">
                    <?= csrfField() ?>
                    <input type="hidden" name="action" value="reset_filters">
                    <button type="submit" class="btn-reset" data-i18n="reset">Reset</button>
                </form>
            </div>
        </aside>

        <!-- Right column -->
        <div class="content">

            <?php foreach ($errors as $error): ?>
                <div
                    class="error-box"
                    <?= !empty($error['key']) ? 'data-error-key="' . h((string) $error['key']) . '"' : '' ?>
                    <?= !empty($error['params']['path']) ? 'data-error-path="' . h((string) $error['params']['path']) . '"' : '' ?>
                ><?= h((string) $error['message']) ?></div>
            <?php endforeach; ?>

            <!-- Top 5 cheapest now -->
            <div class="cheapest-card" id="cheapest-card"><div class="cheapest-empty" role="status"><span class="spinner" aria-hidden="true"></span><span class="sr-only" data-i18n="loading">Loading…</span></div></div>

            <!-- Upcoming predictions (only those a suggest notification would send) -->
            <div class="cheapest-card" id="predictions-card"><div class="cheapest-empty" role="status"><span class="spinner" aria-hidden="true"></span><span class="sr-only" data-i18n="loading">Loading…</span></div></div>

            <!-- The stations around the selected location, nearest first -->
            <div class="cheapest-card" id="nearby-card"><div class="cheapest-empty" role="status"><span class="spinner" aria-hidden="true"></span><span class="sr-only" data-i18n="loading">Loading…</span></div></div>

            <!-- Chart -->
            <div class="chart-card">
                <div class="chart-header">
                    <span class="chart-title" data-i18n="priceTimeline">Price timeline</span>
                    <div class="range-toggles">
                        <?php
                        // Only sub-ranges the filtered payload can actually
                        // narrow: a toggle spanning the whole filter (or more)
                        // duplicates "All", so it is not rendered. "All" and
                        // "Today" always make sense.
                        $chartRangeOptions = [
                            ['all',   'rangeAll',   'All',   null],
                            ['30d',   'range30d',   '30d',   30],
                            ['14d',   'range14d',   '14d',   14],
                            ['7d',    'range7d',    '7d',    7],
                            ['3d',    'range3d',    '3d',    3],
                            ['today', 'rangeToday', 'Today', null],
                        ];
                        foreach ($chartRangeOptions as [$rangeValue, $rangeI18n, $rangeLabel, $rangeSpan]):
                            if ($rangeSpan !== null && $filterSpanDays !== null && $rangeSpan >= $filterSpanDays) {
                                continue;
                            }
                        ?>
                            <button type="button" class="range-toggle<?= $rangeValue === 'all' ? ' active' : '' ?>" data-range="<?= h($rangeValue) ?>" data-i18n="<?= h($rangeI18n) ?>"><?= h($rangeLabel) ?></button>
                        <?php endforeach; ?>
                    </div>
                    <div class="fuel-toggles">
                        <?php // Only fuels in scope — a toggle for a filtered-out fuel is dead weight. ?>
                        <?php foreach (($selectedFuel === 'all' ? ['e5', 'e10', 'diesel'] : [$selectedFuel]) as $chartFuel): ?>
                            <button type="button" class="fuel-toggle active" data-fuel="<?= h($chartFuel) ?>"<?= $chartFuel === 'diesel' ? ' data-i18n="fuelDiesel"' : '' ?>><?= h($fuelLabels[$chartFuel]) ?></button>
                        <?php endforeach; ?>
                    </div>
                </div>
                <div class="chart-body" id="chart-body">
                    <div class="chart-loading" id="chart-loading" role="status"><span class="spinner" aria-hidden="true"></span><span class="sr-only" data-i18n="loading">Loading…</span></div>
                    <svg id="chart" viewBox="0 0 960 380" aria-label="Fuel price history chart" data-i18n-aria-label="chartAriaLabel" hidden></svg>
                </div>
                <div class="chart-legend" id="legend" hidden></div>
                <div class="chart-empty" id="chart-empty" data-i18n="noSnapshots" role="status" hidden>No snapshots match the current filters.</div>
                <div class="chart-retry" id="chart-retry" hidden>
                    <button type="button" class="btn-reset" id="retry-btn" data-i18n="retry">Retry</button>
                </div>
            </div>

            <!-- Cheapest in selected range -->
            <div class="cheapest-card" id="cheapest-range-card"><div class="cheapest-empty" role="status"><span class="spinner" aria-hidden="true"></span><span class="sr-only" data-i18n="loading">Loading…</span></div></div>

            <!-- Highest in selected range -->
            <div class="cheapest-card" id="highest-card"><div class="cheapest-empty" role="status"><span class="spinner" aria-hidden="true"></span><span class="sr-only" data-i18n="loading">Loading…</span></div></div>

            <!-- Stats: what the selected range (sidebar filters) loaded -->
            <div class="stats-card">
                <div class="cheapest-header">
                    <span class="cheapest-title" data-i18n="statsTitle">Selected range</span>
                </div>
                <div class="stats" aria-live="polite">
                    <div class="stat">
                        <div class="stat-label" data-i18n="snapshots">Snapshots</div>
                        <div class="stat-value skeleton" id="stat-points" aria-busy="true">&nbsp;</div>
                    </div>
                    <div class="stat">
                        <div class="stat-label" data-i18n="stationsCount">Stations</div>
                        <div class="stat-value skeleton" id="stat-stations" aria-busy="true">&nbsp;</div>
                    </div>
                    <div class="stat">
                        <div class="stat-label" data-i18n="firstRecorded">First recorded</div>
                        <div class="stat-value skeleton" id="stat-first" style="font-size:1rem" aria-busy="true">&nbsp;</div>
                    </div>
                    <div class="stat">
                        <div class="stat-label" data-i18n="lastRecorded">Last recorded</div>
                        <div class="stat-value skeleton" id="stat-last" style="font-size:1rem" aria-busy="true">&nbsp;</div>
                    </div>
                </div>
            </div>

        </div><!-- /.content -->
    </div><!-- /.layout -->
</main>

<script>
/* ── Mobile filter collapse ─────────────────────────────────────── */
(() => {
    const sidebar = document.getElementById('filters-sidebar');
    const toggle = document.getElementById('filters-toggle');
    if (!sidebar || !toggle) return;
    const mobileLayout = window.matchMedia('(max-width: 900px)');
    const toggleFilters = () => {
        if (!mobileLayout.matches) return;
        const collapsed = sidebar.classList.toggle('collapsed');
        toggle.setAttribute('aria-expanded', String(!collapsed));
        document.cookie = 'gasoline_filters_collapsed=' + (collapsed ? '1' : '0')
            + '; path=/; max-age=31536000; samesite=Lax';
    };
    toggle.addEventListener('click', toggleFilters);
    toggle.addEventListener('keydown', (e) => {
        if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); toggleFilters(); }
    });
})();

/* ── Locale helpers (_tz/_loc/formatDateTime) live in the shared script. ── */

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

function h(str) {
    return String(str)
        .replace(/&/g, '&amp;')
        .replace(/</g, '&lt;')
        .replace(/>/g, '&gt;')
        .replace(/"/g, '&quot;')
        .replace(/'/g, '&#39;');
}

// Populated asynchronously by loadData() from the ?action=data endpoint.
let chartData = [];
let stationDistancesById = {};
// Upcoming notification-worthy predictions from ?action=data, kept so a
// language switch can re-render the card without re-fetching. predictionStationMeta
// is the station id -> {name,...} map (payload.stations) used to resolve names.
let predictionData = [];
let predictionAsOf = {};
let predictionStationMeta = {};
let dataLoaded = false;
// Surroundings card: the nearest stations with their current price
// (payload.nearby), the count the radius actually holds, and whether the reader
// has expanded past the preview.
let nearbyRows = [];
let nearbyTotal = 0;
let nearbyExpanded = false;
// Current price per station id from that same block, which is what lets the
// detail dialog answer for a station the date filter left out of the chart.
let nearbyLatestById = new Map();
const NEARBY_PREVIEW_ROWS = 8;
// In-memory, non-persistent chart-only filter: null = all stations shown,
// otherwise a Set of visible station_ids (strings). Reset on fresh data.
let stationFilter = null;
// Fuels whose trendline the legend has toggled off. Chart-only and
// non-persistent like stationFilter, but not tied to the payload's station
// roster, so a refresh keeps the choice.
const hiddenTrends = new Set();

// Evenly-spread hues for all stations in this view using golden-angle spacing.
// Stations sorted alphabetically → deterministic within a place. Recomputed
// whenever new data arrives.
function computeStationHues() {
    const GOLDEN_ANGLE = 137.508;
    const names = [...new Set(chartData.map((r) => r.station_name))].sort();
    return Object.fromEntries(names.map((name, i) => [name, (i * GOLDEN_ANGLE) % 360]));
}

let _stationHues = computeStationHues();

function stationFuelColor(stationName, fuel) {
    const hue = _stationHues[stationName] ?? nameToHue(stationName);
    const { s, l } = FUEL_TINTS[fuel];
    return `hsl(${hue},${s}%,${l}%)`;
}

const selectedFuel = <?= json_encode($selectedFuel, JSON_THROW_ON_ERROR) ?>;
// Whether a city or station filter is set. Without one the server never runs
// the snapshot query (the payload is known-empty), so the client can skip the
// fetch and render the empty state immediately instead of showing spinners.
// Server-side errors still force a fetch so the usual error UI renders.
const hasDataScope = <?= json_encode($selectedLocation !== '' || $selectedStationIds !== [] || $errors !== [], JSON_THROW_ON_ERROR) ?>;
// ?action=geocode writes to the cities cache, so it is token-checked like a
// form post even though it is fetched with GET.
const geocodeCsrf = <?= json_encode(csrfToken(), JSON_THROW_ON_ERROR) ?>;
// The selected location, as the surroundings card names it in its header. Empty
// when no city, address or position has been picked.
const locationLabel = <?= json_encode($selectedLocation, JSON_THROW_ON_ERROR) ?>;
const locationRadiusKm = <?= json_encode($selectedRadiusKm, JSON_THROW_ON_ERROR) ?>;

const fuelConfig = {
    // `dash` is the trendline's stroke-dasharray: dashed sets a trend apart
    // from the solid per-station price lines, and one pattern per fuel keeps
    // three trends apart from each other even where their colours crowd.
    e5:     { label: 'E5',     color: '#f5a623', glow: 'rgba(245,166,35,0.18)', dash: '10 6' },
    e10:    { label: 'E10',    color: '#34d399', glow: 'rgba(52,211,153,0.15)', dash: '2 5' },
    diesel: { label: 'Diesel', color: '#60a5fa', glow: 'rgba(96,165,250,0.15)', dash: '12 5 2 5' },
};

const chartEl = document.getElementById('chart');
const legendEl = document.getElementById('legend');
const toggles = [...document.querySelectorAll('.fuel-toggle')];

/* ── Tooltip helpers ───────────────────────────────────────────── */
const tooltip = document.getElementById('price-tooltip');

// Placed clear of the plot (positionChartTooltip, shared script) so the
// price lines stay visible while the crosshair is up.
function positionTooltip(clientX, clientY) {
    positionChartTooltip(chartEl, clientX, clientY);
}

// Re-assigned by renderChart so hideTooltip can also drop the crosshair line.
let hideCrosshair = () => {};

function hideTooltip() {
    tooltip.style.display = 'none';
    hideCrosshair();
}

// Lifting the finger on the chart keeps the crosshair readable; touching
// anywhere else dismisses it.
document.addEventListener('touchend', (e) => {
    if (e.target instanceof Element && e.target.closest('#chart')) return;
    hideTooltip();
});

// currentLang is declared in the shared script (renderCommonScript).

let chartRange = 'all';

function getRangeFilteredData() {
    if (chartRange === 'all') return chartData;

    let cutoffTs;
    if (chartRange === 'today') {
        const startOfToday = new Date();
        startOfToday.setHours(0, 0, 0, 0);
        cutoffTs = startOfToday.getTime();
    } else {
        const days = chartRange === '30d' ? 30 : chartRange === '14d' ? 14 : chartRange === '7d' ? 7 : 3;
        cutoffTs = Date.now() - days * 24 * 60 * 60 * 1000;
    }

    // Single pass: split into before-cutoff (track last per station) and in-range rows.
    // chartData is sorted by recorded_at ASC, so iterating forward naturally keeps
    // the last assignment as the most recent pre-cutoff row for each station.
    const rangeRows = [];
    const lastBeforeByStation = new Map();
    const stationsInRange = new Set();
    for (const row of chartData) {
        if (row._ts < cutoffTs) {
            lastBeforeByStation.set(row.station_id, row);
        } else {
            rangeRows.push(row);
            stationsInRange.add(row.station_id);
        }
    }

    const nowTs = Date.now();
    const synthetic = [];
    for (const [stationId, lastRow] of lastBeforeByStation) {
        // Synthetic point at the start of the range (left edge of chart)
        synthetic.push({
            ...lastRow,
            _ts: cutoffTs,
            recorded_at: new Date(cutoffTs).toISOString(),
            _synthetic: true,
        });
        // If there are no real data points for this station within the range,
        // also add a synthetic point at "now" to draw a flat line across the range.
        if (!stationsInRange.has(stationId)) {
            synthetic.push({
                ...lastRow,
                _ts: nowTs,
                recorded_at: new Date(nowTs).toISOString(),
                _synthetic: true,
            });
        }
    }

    return [...synthetic, ...rangeRows].sort((a, b) => a._ts - b._ts);
}

if (!chartEl) {
    // No chart in DOM (empty state)
} else {
    const activeFuels = new Set(selectedFuel === 'all' ? ['e5', 'e10', 'diesel'] : [selectedFuel]);

    const rangeToggleEls = [...document.querySelectorAll('.range-toggle')];

    // Only in-scope fuels are rendered (the shell drops filtered-out ones),
    // so every toggle starts active.
    toggles.forEach((toggle) => {
        toggle.classList.toggle('active', activeFuels.has(toggle.dataset.fuel));
    });

    rangeToggleEls.forEach((btn) => {
        btn.addEventListener('click', () => {
            chartRange = btn.dataset.range;
            rangeToggleEls.forEach((b) => b.classList.toggle('active', b.dataset.range === chartRange));
            renderChart();
            renderCheapestRange();
            renderHighest();
        });
    });

    function setChartVisibility(isEmpty) {
        const emptyEl = document.getElementById('chart-empty');
        const loadingEl = document.getElementById('chart-loading');
        if (loadingEl) loadingEl.hidden = true;
        if (emptyEl) emptyEl.hidden = !isEmpty;
        // chartEl is an SVG element: the `hidden` IDL property only exists on
        // HTMLElement, so assigning chartEl.hidden would not touch the attribute.
        chartEl.toggleAttribute('hidden', isEmpty);
        legendEl.hidden = isEmpty;
    }

    // The chart is drawn in CSS pixels (see renderChart), so a container
    // resize — rotation, sidebar toggle, window resize — needs a re-render
    // at the new width. Observe the always-visible parent, debounced.
    let lastChartWidth = 0;
    let chartResizeTimer = null;
    if (typeof ResizeObserver !== 'undefined') {
        new ResizeObserver((entries) => {
            if (!dataLoaded || chartEl.hasAttribute('hidden')) return;
            const entry = entries[0];
            if (!entry) return;
            // contentRect is the parent's content box (padding excluded),
            // which the width:100% SVG fills exactly — no reflow needed.
            const w = Math.round(entry.contentRect.width);
            if (w === 0 || Math.abs(w - lastChartWidth) < 2) return;
            clearTimeout(chartResizeTimer);
            chartResizeTimer = setTimeout(renderChart, 100);
        }).observe(chartEl.parentElement);
    }

    function renderChart() {
        chartEl.innerHTML = '';
        legendEl.innerHTML = '';

        const rangeData = getRangeFilteredData();
        if (rangeData.length === 0) { setChartVisibility(true); return; }

        let visibleRows = rangeData.filter((row) => [...activeFuels].some((f) => row[f] !== null));
        if (visibleRows.length === 0) { setChartVisibility(true); return; }

        // Full station roster for the legend, captured BEFORE the isolate filter so
        // hidden stations stay listed and clickable to toggle back on.
        const legendStations = [];
        const seenLegend = new Set();
        for (const r of visibleRows) {
            const id = String(r.station_id);
            if (!seenLegend.has(id)) { seenLegend.add(id); legendStations.push(r); }
        }
        // Legend renderer — one entry per station (ALL stations, even hidden ones);
        // click a station (name or dot — one element) to isolate it. Clicking the
        // sole isolated station restores all; from a partial selection clicks toggle
        // individual stations. Chart-only, in-memory, non-persistent. Defined here
        // (before the filter) so it can also be drawn on the empty-filter path.
        const drawLegend = () => {
            const allIds = legendStations.map((s) => String(s.station_id));
            for (const sample of legendStations) {
                const id = String(sample.station_id);
                const off = stationFilter && !stationFilter.has(id);
                const item = document.createElement('div');
                item.className = 'legend-item' + (off ? ' off' : '');
                const swatches = [...activeFuels].map((fuel) => {
                    const color = stationFuelColor(sample.station_name, fuel);
                    const label = fuelConfig[fuel].label;
                    return `<span class="legend-dot" title="${label}" style="background:${color}"></span>`;
                }).join('');
                item.innerHTML = `${swatches}${h(sample.station_name)}`;
                item.addEventListener('click', () => {
                    if (stationFilter && stationFilter.size === 1 && stationFilter.has(id)) {
                        stationFilter = null;                          // sole station clicked again → show all
                    } else if (!stationFilter) {
                        stationFilter = new Set([id]);                 // all shown → isolate this one
                    } else {
                        stationFilter.has(id) ? stationFilter.delete(id) : stationFilter.add(id); // partial → plain toggle
                        if (stationFilter.size === 0 || stationFilter.size >= allIds.length) stationFilter = null;
                    }
                    renderChart();
                });
                legendEl.appendChild(item);
            }
        };

        // Apply the in-memory isolate filter unconditionally so an isolation with no
        // data in the current fuel/range shows the empty state rather than silently
        // reverting to all stations. The legend stays visible in that case so the
        // user can still toggle stations back on.
        if (stationFilter) {
            visibleRows = visibleRows.filter((r) => stationFilter.has(String(r.station_id)));
        }
        if (visibleRows.length === 0) {
            setChartVisibility(true);
            legendEl.hidden = false;
            drawLegend();
            return;
        }

        const ns = 'http://www.w3.org/2000/svg';

        const mk = (tag, attrs = {}, parent = chartEl) => {
            const el = document.createElementNS(ns, tag);
            for (const [k, v] of Object.entries(attrs)) el.setAttribute(k, String(v));
            parent.appendChild(el);
            return el;
        };

        // no fill — line-only chart

        // Single-pass min/max — never spread a per-point array into Math.min/max,
        // which overflows the call stack once there are ~100k points.
        let minX = Infinity, maxX = -Infinity;
        for (const r of visibleRows) {
            if (r._ts < minX) minX = r._ts;
            if (r._ts > maxX) maxX = r._ts;
        }

        let minY = Infinity, maxY = -Infinity, valCount = 0;
        for (const fuel of activeFuels) {
            for (const r of visibleRows) {
                const v = r[fuel];
                if (v !== null) {
                    valCount++;
                    if (v < minY) minY = v;
                    if (v > maxY) maxY = v;
                }
            }
        }

        if (valCount === 0) { setChartVisibility(true); return; }
        setChartVisibility(false);

        // Draw in a coordinate system that is 1:1 with on-screen CSS pixels,
        // so text keeps its true proportions at any container width. (The old
        // fixed 960-wide viewBox with preserveAspectRatio="none" squashed the
        // labels into tall, narrow glyphs on small screens.)
        const W = Math.max(280, Math.round(chartEl.getBoundingClientRect().width) || 960);
        const compact = W < 560;
        const H = compact ? 300 : 380;
        const margin = compact
            ? { top: 18, right: 10, bottom: 48, left: 46 }
            : { top: 24, right: 24, bottom: 60, left: 68 };
        const iW = W - margin.left - margin.right;
        const iH = H - margin.top - margin.bottom;
        chartEl.setAttribute('viewBox', `0 0 ${W} ${H}`);
        lastChartWidth = W;

        if (minX === maxX) maxX += 3_600_000;

        const padY = Math.max((maxY - minY) * 0.15, 0.02);
        minY -= padY; maxY += padY;

        const px = (v) => margin.left + ((v - minX) / (maxX - minX)) * iW;
        const py = (v) => margin.top + iH - ((v - minY) / (maxY - minY)) * iH;

        const light = document.documentElement.getAttribute('data-theme') === 'light';
        const chartBg    = light ? '#ffffff' : '#13151a';
        const gridStroke = light ? 'rgba(0,0,0,0.06)' : 'rgba(255,255,255,0.05)';
        const tickStroke = light ? 'rgba(0,0,0,0.05)' : 'rgba(255,255,255,0.04)';
        const axisStroke = light ? 'rgba(0,0,0,0.15)' : 'rgba(255,255,255,0.12)';

        // Background
        mk('rect', { x: 0, y: 0, width: W, height: H, fill: chartBg });

        // Grid lines
        for (let i = 0; i <= 5; i++) {
            const val = minY + ((maxY - minY) / 5) * i;
            const yp = py(val);
            mk('line', { x1: margin.left, y1: yp, x2: W - margin.right, y2: yp,
                stroke: gridStroke, 'stroke-width': 1 });
            fillSvgPrice(mk('text', { x: margin.left - (compact ? 6 : 10), y: yp + 4, 'text-anchor': 'end',
                'font-size': 11, 'font-family': "'DM Mono', monospace", fill: '#6b7280' },
            ), val, 11);
        }

        // X ticks — two-line: date + time; fewer on narrow screens so the
        // labels keep breathing room instead of overlapping.
        const tickCount = Math.min(compact ? 4 : 7, visibleRows.length);
        const tickColor = light ? 'rgba(0,0,0,0.4)' : 'rgba(255,255,255,0.38)';
        for (let i = 0; i < tickCount; i++) {
            const idx = tickCount === 1 ? 0 : Math.round((visibleRows.length - 1) * (i / (tickCount - 1)));
            const row = visibleRows[idx];
            const xp = px(row._ts);
            mk('line', { x1: xp, y1: margin.top, x2: xp, y2: H - margin.bottom,
                stroke: tickStroke, 'stroke-width': 1 });
            // Clamp so edge labels don't get clipped by the SVG bounds.
            const lx = Math.min(Math.max(xp, 22), W - 22);
            const txt = mk('text', { x: lx, y: H - margin.bottom + 14, 'text-anchor': 'middle',
                'font-size': 10, 'font-family': "'DM Mono', monospace", fill: tickColor });
            const tDate = document.createElementNS(ns, 'tspan');
            tDate.setAttribute('x', lx);
            tDate.setAttribute('dy', '0');
            tDate.textContent = formatTickDate(row.recorded_at);
            txt.appendChild(tDate);
            const tTime = document.createElementNS(ns, 'tspan');
            tTime.setAttribute('x', lx);
            tTime.setAttribute('dy', '14');
            tTime.textContent = formatTickTime(row.recorded_at);
            txt.appendChild(tTime);
        }

        // Axes
        mk('line', { x1: margin.left, y1: H - margin.bottom, x2: W - margin.right, y2: H - margin.bottom,
            stroke: axisStroke, 'stroke-width': 1 });
        mk('line', { x1: margin.left, y1: margin.top, x2: margin.left, y2: H - margin.bottom,
            stroke: axisStroke, 'stroke-width': 1 });

        // Group rows by station ONCE (visibleRows stays sorted by _ts), so the
        // line/dot/legend passes are O(N) instead of re-scanning the whole array
        // per station per fuel.
        const byStation = new Map();
        for (const r of visibleRows) {
            let arr = byStation.get(r.station_id);
            if (!arr) { arr = []; byStation.set(r.station_id, arr); }
            arr.push(r);
        }

        // Line only — per-station colour, per-fuel tint
        for (const fuel of activeFuels) {
            for (const stationRows of byStation.values()) {
                const series = stationRows.filter((r) => r[fuel] !== null);
                if (series.length < 2) continue;

                const color = stationFuelColor(series[0].station_name, fuel);
                const pts = series.map((r) => [px(r._ts), py(r[fuel])]);
                const linePath = pts.map(([x, y], j) => `${j === 0 ? 'M' : 'L'}${x.toFixed(2)},${y.toFixed(2)}`).join(' ');

                mk('path', { d: linePath, fill: 'none', stroke: color,
                    'stroke-width': 2, 'stroke-linejoin': 'round', 'stroke-linecap': 'round', opacity: 0.9 });
            }
        }

        // A trend has to be unmistakable among a dozen station lines, and it
        // cannot win that on hue: station colours are spread right around the
        // wheel, so any colour a trend picks is some station's too. It is drawn
        // in the page's own foreground instead — a value no station line takes
        // — over a soft halo in the fuel's colour. The halo, the dash pattern
        // and the legend swatch carry which fuel it belongs to.
        const trendInk = light ? '#1c1c1e' : '#e8eaed';
        const trendSwatch = (fuel, w = 20) =>
            `<svg class="legend-line" width="${w}" height="8" aria-hidden="true">`
            + [[fuelConfig[fuel].color, 7, 0.18], [fuelConfig[fuel].color, 4, 0.35],
                [trendInk, 2, 0.9]].map(([stroke, width, opacity]) =>
                `<line x1="1" y1="4" x2="${w - 1}" y2="4" stroke="${stroke}" stroke-width="${width}"`
                + ` stroke-opacity="${opacity}" stroke-dasharray="${fuelConfig[fuel].dash}"`
                + ` stroke-linecap="round"/>`).join('')
            + `</svg>`;

        // Clip a fitted segment to the plot band: a trend can leave the padded
        // y-range (a steep fit through lopsided data), and an unclipped line
        // would paint over the axis labels.
        const clipToPlot = (x1, y1, x2, y2) => {
            const top = margin.top, bot = H - margin.bottom;
            if ((y1 < top && y2 < top) || (y1 > bot && y2 > bot)) return null;
            const xAt = (y) => x1 + ((y - y1) / (y2 - y1)) * (x2 - x1);
            if (y1 < top)      { x1 = xAt(top); y1 = top; }
            else if (y1 > bot) { x1 = xAt(bot); y1 = bot; }
            if (y2 < top)      { x2 = xAt(top); y2 = top; }
            else if (y2 > bot) { x2 = xAt(bot); y2 = bot; }
            return { x1, y1, x2, y2 };
        };

        // Trendlines — one least-squares fit per fuel across every visible
        // station, drawn dashed in the fuel's own colour so a trend reads as a
        // summary rather than as one more station's price line. The fits follow
        // the isolate filter, so isolating a station trends that station alone.
        // trendLegend is collected while fitting, so the legend lists only the
        // fuels that actually produced a line; trendFits keeps each fit so the
        // crosshair can report where the trend stands at the hovered moment.
        const trendLegend = [];
        const trendFits = [];
        for (const fuel of activeFuels) {
            // Fit the step function the chart actually draws, not the rows
            // that encode it: a price holds until the station reprices, so
            // each sample carries the weight of its hold and sits at the
            // middle of it. The five sums below are then the exact integrals
            // of that step function over the window, which makes the fit
            // depend only on the prices and how long each one stood — never on
            // how many rows they arrive in. That last part matters here: row
            // density is partly an artefact. Snapshots are only written when a
            // price moves, the newest one is refreshed in place, and a ranged
            // view synthesizes carry-in rows at the left edge. Weighting rows
            // equally would let all of that move the line — the same staircase
            // read 12.5 ct/day cut one way and 8.7 cut another — and would let
            // a station that reprices hourly outvote one that reprices daily.
            // Time is hours from the window's left edge, not epoch millis: the
            // sums of squares stay small enough to hold their precision, and
            // the slope comes out per hour, which is what the legend reports.
            let sumW = 0, sumX = 0, sumY = 0, sumXX = 0, sumXY = 0;
            let firstTs = Infinity, lastTs = -Infinity;
            for (const stationRows of byStation.values()) {
                const series = stationRows.filter((r) => r[fuel] !== null);
                if (series.length === 0) continue;
                if (series[0]._ts < firstTs) firstTs = series[0]._ts;
                if (series[series.length - 1]._ts > lastTs) lastTs = series[series.length - 1]._ts;
                for (let i = 0; i < series.length; i++) {
                    // The last price of a series stands to the window's right
                    // edge, the same reading the crosshair gives it.
                    const until = i + 1 < series.length ? series[i + 1]._ts : maxX;
                    const w = (until - series[i]._ts) / 3_600_000;
                    if (w <= 0) continue;
                    const v = series[i][fuel];
                    const mid = ((series[i]._ts + until) / 2 - minX) / 3_600_000;
                    sumW += w;
                    sumX += w * mid;
                    sumY += w * v;
                    // The w^2/12 term is the hold's own spread about its
                    // midpoint. Without it this sum is not the integral of
                    // t^2, and splitting one hold into two would move the fit.
                    sumXX += w * (mid * mid + (w * w) / 12);
                    sumXY += w * mid * v;
                }
            }
            // A single observation holds for the window's whole width, which is
            // enough for the arithmetic but not enough to claim a direction
            // from — the chart does not even draw a price line for it. A trend
            // needs this fuel priced at two different times.
            if (sumW <= 0 || lastTs <= firstTs) continue;
            const denom = sumW * sumXX - sumX * sumX;
            if (denom <= 0) continue;                    // no spread in time to trend over
            const slope = (sumW * sumXY - sumX * sumY) / denom;   // € per hour
            const intercept = (sumY - slope * sumX) / sumW;
            const fitAt = (ts) => intercept + slope * ((ts - minX) / 3_600_000);

            trendLegend.push({ fuel, slope, fitAt });
            if (hiddenTrends.has(fuel)) continue;
            trendFits.push({ fuel, fitAt });

            const seg = clipToPlot(px(firstTs), py(fitAt(firstTs)), px(maxX), py(fitAt(maxX)));
            if (!seg) continue;
            const line = {
                x1: seg.x1.toFixed(2), y1: seg.y1.toFixed(2),
                x2: seg.x2.toFixed(2), y2: seg.y2.toFixed(2),
                'stroke-dasharray': fuelConfig[fuel].dash, 'stroke-linecap': 'round',
                'pointer-events': 'none',
            };
            // Two halo passes give the glow a falloff a single wide stroke
            // cannot, then the line goes over them.
            mk('line', { ...line, stroke: fuelConfig[fuel].color, 'stroke-width': 9, opacity: 0.18 });
            mk('line', { ...line, stroke: fuelConfig[fuel].color, 'stroke-width': 5, opacity: 0.35 });
            mk('line', { ...line, stroke: trendInk, 'stroke-width': 2, opacity: 0.9 });
        }

        // Trend legend — the same dash the chart drew, plus the fit's slope in
        // cents per day, which is the number the line is worth reading for.
        // Clicking one hides or shows that fuel's trendline.
        const drawTrendLegend = () => {
            const t = translations[currentLang];
            for (const { fuel, slope, fitAt } of trendLegend) {
                const off = hiddenTrends.has(fuel);
                const perDay = slope * 24 * 100;   // €/h → ct/day
                const rate = (perDay < 0 ? '\u2212' : '+')
                    + Math.abs(perDay).toLocaleString(_loc(), { minimumFractionDigits: 2, maximumFractionDigits: 2 });
                const item = document.createElement('div');
                item.className = 'legend-item' + (off ? ' off' : '');
                // Where the trend stands at the right edge — the reading the
                // rate is heading towards, for anyone not hovering the chart.
                item.title = `${t.trendHint}\n${t.trendLatest}: ${fmtPriceText(fitAt(maxX))} €`;
                item.innerHTML =
                    trendSwatch(fuel)
                    + `${h(t.trend)} ${h(fuelConfig[fuel].label)}`
                    + `<span class="legend-trend-rate">${h(rate)} ${h(t.trendPerDay)}</span>`;
                item.addEventListener('click', () => {
                    hiddenTrends.has(fuel) ? hiddenTrends.delete(fuel) : hiddenTrends.add(fuel);
                    renderChart();
                });
                legendEl.appendChild(item);
            }
        };

        // Crosshair: a thin vertical line follows the pointer/finger and the
        // tooltip lists every station's price in effect at that timestamp.
        const crossSeries = [];
        for (const stationRows of byStation.values()) {
            for (const fuel of activeFuels) {
                const rows = stationRows.filter((r) => r[fuel] !== null);
                if (rows.length) crossSeries.push({ name: rows[0].station_name, fuel, rows });
            }
        }

        // visibleRows is sorted by _ts ASC, so one linear pass dedups in order.
        const distinctTs = [];
        for (const r of visibleRows) {
            if (distinctTs.length === 0 || distinctTs[distinctTs.length - 1] !== r._ts) {
                distinctTs.push(r._ts);
            }
        }

        const crossLine = mk('line', {
            x1: 0, x2: 0, y1: margin.top, y2: H - margin.bottom,
            stroke: light ? 'rgba(0,0,0,0.45)' : 'rgba(255,255,255,0.5)',
            'stroke-width': 1, 'stroke-dasharray': '4 3',
            opacity: 0, 'pointer-events': 'none',
        });

        // Last row of a series recorded at or before ts (series is _ts-sorted).
        const lastAtOrBefore = (rows, ts) => {
            let lo = 0, hi = rows.length - 1, best = null;
            while (lo <= hi) {
                const mid = (lo + hi) >> 1;
                if (rows[mid]._ts <= ts) { best = rows[mid]; lo = mid + 1; } else { hi = mid - 1; }
            }
            return best;
        };

        const showCrosshair = (clientX, clientY) => {
            const rect = chartEl.getBoundingClientRect();
            let sx = ((clientX - rect.left) / rect.width) * W;
            sx = Math.max(margin.left, Math.min(W - margin.right, sx));
            const t = minX + ((sx - margin.left) / iW) * (maxX - minX);

            // Snap to the nearest recorded timestamp so the line sits on data.
            let lo = 0, hi = distinctTs.length - 1;
            while (lo < hi) {
                const mid = (lo + hi) >> 1;
                if (distinctTs[mid] < t) lo = mid + 1; else hi = mid;
            }
            const ts = (lo > 0 && t - distinctTs[lo - 1] < distinctTs[lo] - t) ? distinctTs[lo - 1] : distinctTs[lo];

            const xp = px(ts);
            crossLine.setAttribute('x1', xp);
            crossLine.setAttribute('x2', xp);
            crossLine.setAttribute('opacity', 1);

            const entries = [];
            for (const s of crossSeries) {
                const row = lastAtOrBefore(s.rows, ts);
                if (row) entries.push({ name: s.name, fuel: s.fuel, price: row[s.fuel] });
            }
            if (entries.length === 0) { hideTooltip(); return; }
            entries.sort((a, b) => a.price - b.price);

            const showFuel = activeFuels.size > 1;
            const labels = translations[currentLang];   // `t` is the timestamp here
            tooltip.innerHTML =
                `<div class="tt-meta">${h(formatDateTime(new Date(ts).toISOString()))}</div>` +
                // The trend leads: it is the benchmark the prices under it are
                // read against, and a long station list can spill into further
                // columns, so anywhere but the top could put it out of sight.
                trendFits.map(({ fuel, fitAt }) =>
                    `<div class="tt-row tt-trend">` +
                        trendSwatch(fuel, 14) +
                        `<span class="tt-name">${h(labels.trend)}</span>` +
                        (showFuel ? `<span class="tt-fuel">${fuelConfig[fuel].label}</span>` : '') +
                        `<span class="tt-val" style="color:${trendInk}">${fmtPriceHtml(fitAt(ts))} €</span>` +
                    `</div>`).join('') +
                entries.map((en) => {
                    const color = stationFuelColor(en.name, en.fuel);
                    return `<div class="tt-row">` +
                        `<span class="legend-dot" style="background:${color}"></span>` +
                        `<span class="tt-name">${h(en.name)}</span>` +
                        (showFuel ? `<span class="tt-fuel">${fuelConfig[en.fuel].label}</span>` : '') +
                        `<span class="tt-val" style="color:${color}">${fmtPriceHtml(en.price)} €</span>` +
                    `</div>`;
                }).join('');
            tooltip.style.display = 'block';
            positionTooltip(clientX, clientY);
        };

        hideCrosshair = () => crossLine.setAttribute('opacity', 0);

        const overlay = mk('rect', {
            x: margin.left, y: margin.top, width: iW, height: iH,
            fill: 'transparent', style: 'cursor:crosshair',
        });
        // Hover via pointer events gated on a real mouse: a tap also fires
        // compatibility mousemove/mouseleave, which would leak the crosshair
        // past the long-press gate below (compat mouse events never come back
        // as pointer events, so the gate is airtight).
        overlay.addEventListener('pointermove', (e) => {
            if (e.pointerType === 'mouse') showCrosshair(e.clientX, e.clientY);
        });
        overlay.addEventListener('pointerleave', (e) => {
            if (e.pointerType === 'mouse') hideTooltip();
        });
        // Touch: long-press only (attachLongPressCrosshair, shared script),
        // so swiping across the chart scrolls the page; the tooltip auto-hides
        // a few seconds after the finger lifts.
        attachLongPressCrosshair(overlay, showCrosshair, hideTooltip);

        drawTrendLegend();
        drawLegend();
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

    // Initial render is triggered by loadData() once the async payload arrives.
}

/* ── i18n ── (translations + applyLang live in the shared script) ── */

/* ── Price cards (cheapest / highest) ──────────────────────────── */
const ICON_DOWN = `<svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" style="color:var(--amber);flex-shrink:0"><circle cx="12" cy="12" r="10"/><polyline points="8 12 12 16 16 12"/><line x1="12" y1="8" x2="12" y2="16"/></svg>`;
const ICON_UP   = `<svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" style="color:var(--amber);flex-shrink:0"><circle cx="12" cy="12" r="10"/><polyline points="8 12 12 8 16 12"/><line x1="12" y1="16" x2="12" y2="8"/></svg>`;

const cheapestCard      = document.getElementById('cheapest-card');
const cheapestRangeCard = document.getElementById('cheapest-range-card');
const highestCard       = document.getElementById('highest-card');
const predictionsCard   = document.getElementById('predictions-card');

const FUEL_CSS_COLORS = { e5: 'var(--e5)', e10: 'var(--e10)', diesel: 'var(--diesel)' };

/* ── Clickable station references ──────────────────────────────── */
// Every station named in the four price cards opens the detail dialog, so the
// name lines and the ranked runner-up rows are rendered as buttons.
const ICON_STATION_INFO = `<svg class="station-btn-icon" width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="12" cy="12" r="10"/><line x1="12" y1="16" x2="12" y2="12"/><line x1="12" y1="8" x2="12.01" y2="8"/></svg>`;

function stationDot(stationName, fuel, extra) {
    return `<span class="legend-dot" style="background:${stationFuelColor(stationName, fuel)};display:inline-block;flex-shrink:0;margin-right:0.4rem${extra || ''}"></span>`;
}

// Big station block of a card cell: name line plus the optional address line,
// wrapped in one button that opens the station dialog. `addressHtml` arrives
// as markup, since the distance in it carries a sized-down separator.
function stationBlock(stationId, stationName, fuel, addressHtml) {
    const t = translations[currentLang];
    return `<button type="button" class="station-btn" data-station-id="${h(stationId)}"` +
        ` title="${h(t.sdHint)}" aria-label="${h(t.sdHint + ': ' + stationName)}">` +
        `<span class="cheapest-station sd-name-line">` +
            stationDot(stationName, fuel) +
            `<span class="sd-name-text">${h(stationName)}</span>` +
            ICON_STATION_INFO +
        `</span>` +
        (addressHtml ? `<span class="cheapest-station sd-addr-line">${addressHtml}</span>` : '') +
    `</button>`;
}

// Compact ranked row (runners-up / extra prediction windows), also clickable.
// `stationName` drives the colour dot and the label, `distKm` is appended to
// that label when the reader has picked a location, `price` is the raw number
// to render. The label is built twice over: as markup for the row, as text for
// the aria label, which cannot carry the sized-down separator.
function stationRankRow(stationId, stationName, distKm, fuel, price, trailingHtml, titleText) {
    const t = translations[currentLang];
    const distText = fmtDistanceKm(distKm);
    const distHtml = fmtDistanceKmHtml(distKm);
    const label = stationName + (distText === null ? '' : ` (${distText})`);
    const labelHtml = h(stationName) + (distHtml === null ? '' : ` (${distHtml})`);
    return `<button type="button" class="rank-row station-rank-btn" data-station-id="${h(stationId)}"` +
        ` title="${h(titleText ? titleText + ' — ' + t.sdHint : t.sdHint)}"` +
        ` aria-label="${h(t.sdHint + ': ' + fmtPriceText(price) + ' — ' + label)}">` +
        `<span class="rank-price" style="color:${FUEL_CSS_COLORS[fuel]}">${fmtPriceHtml(price)}</span>` +
        `<span class="rank-station">${stationDot(stationName, fuel)}${labelHtml}</span>` +
        (trailingHtml || '') +
    `</button>`;
}

function renderPriceCard(el, rows, title, better, icon, emptyMsg) {
    if (!el) return;
    const fuels      = selectedFuel === 'all' ? ['e5', 'e10', 'diesel'] : [selectedFuel];
    const fuelColors = { e5: 'var(--e5)', e10: 'var(--e10)', diesel: 'var(--diesel)' };

    const results = [];
    for (const fuel of fuels) {
        let best = null;
        for (const row of rows) {
            if (row[fuel] !== null && (best === null || better(row[fuel], best.price))) {
                best = {
                    price: row[fuel],
                    station_id: row.station_id,
                    station: row.station_name,
                    street: row.street,
                    place: row.place,
                    recorded_at: row.recorded_at,
                };
            }
        }
        if (best) results.push({ fuel, ...best });
    }

    const colClass = results.length === 1 ? 'single' : results.length === 2 ? 'two-col' : '';

    // Scope note instead of a range name in the title: the card always shows
    // the extreme of whatever range the chart toggles currently narrow to.
    const scopeHint = translations[currentLang].rangeScopeHint;

    el.innerHTML =
        `<div class="cheapest-header">${icon}<span class="cheapest-title">${title}</span><span class="cheapest-scope">${h(scopeHint)}</span></div>` +
        (results.length === 0
            ? `<div class="cheapest-empty">${emptyMsg}</div>`
            : `<div class="cheapest-grid${colClass ? ' ' + colClass : ''}">` +
                results.map(({ fuel, price, station_id, station, street, place, recorded_at }) => {
                    // Address line as markup: text parts escaped here, the
                    // distance already formatted with its smaller separator.
                    const addressParts = [street, place].filter(Boolean).map(h);
                    const selectedDistKm = stationDistancesById[station_id] ?? null;
                    if (selectedDistKm !== null) {
                        addressParts.push(fmtDistanceKmHtml(selectedDistKm));
                    }
                    const address = addressParts.length ? addressParts.join(', ') : '';
                    return `<div class="cheapest-cell">` +
                        `<div class="cheapest-fuel-label" style="color:${fuelColors[fuel]}">${fuelConfig[fuel].label}</div>` +
                        `<div class="cheapest-price" style="color:${fuelColors[fuel]}">${fmtPriceHtml(price)} <span style="font-size:1rem;opacity:0.7">€</span></div>` +
                        stationBlock(station_id, station, fuel, address) +
                        `<div class="cheapest-time">${h(formatDateTime(recorded_at))}</div>` +
                    `</div>`;
                }).join('') +
              `</div>`
        );
}

// Newest snapshot per station id, keyed by station id.
function latestRowById() {
    const byStation = new Map();
    for (const row of chartData) {
        const prev = byStation.get(row.station_id);
        if (!prev || row._ts > prev._ts) byStation.set(row.station_id, row);
    }
    return byStation;
}

function latestRows() {
    return [...latestRowById().values()];
}

// Top 5 cheapest stations right now, per fuel: the best price rendered like
// the other cards, followed by a compact ranked list of the runners-up.
function renderCheapest() {
    const t = translations[currentLang];
    if (!cheapestCard) return;
    const fuels      = selectedFuel === 'all' ? ['e5', 'e10', 'diesel'] : [selectedFuel];
    const fuelColors = { e5: 'var(--e5)', e10: 'var(--e10)', diesel: 'var(--diesel)' };
    const rows = latestRows();

    const results = [];
    for (const fuel of fuels) {
        const top = rows.filter((r) => r[fuel] !== null)
            .sort((a, b) => a[fuel] - b[fuel])
            .slice(0, 6);
        if (top.length) results.push({ fuel, top });
    }

    const colClass = results.length === 1 ? ' single' : results.length === 2 ? ' two-col' : '';

    cheapestCard.innerHTML =
        `<div class="cheapest-header">${ICON_DOWN}<span class="cheapest-title">${t.cheapestNow}</span></div>` +
        (results.length === 0
            ? `<div class="cheapest-empty">${t.cheapestNoData}</div>`
            : `<div class="cheapest-grid${colClass}">` +
                results.map(({ fuel, top }) => {
                    const best = top[0];
                    const addressParts = [best.street, best.place].filter(Boolean).map(h);
                    const bestDist = stationDistancesById[best.station_id] ?? null;
                    if (bestDist !== null) addressParts.push(fmtDistanceKmHtml(bestDist));
                    const runnersUp = top.slice(1).map((row) => stationRankRow(
                        row.station_id,
                        row.station_name,
                        stationDistancesById[row.station_id] ?? null,
                        fuel,
                        row[fuel],
                        '',
                        formatDateTime(row.recorded_at)
                    )).join('');
                    return `<div class="cheapest-cell">` +
                        `<div class="cheapest-fuel-label" style="color:${fuelColors[fuel]}">${fuelConfig[fuel].label}</div>` +
                        `<div class="cheapest-price" style="color:${fuelColors[fuel]}">${fmtPriceHtml(best[fuel])} <span style="font-size:1rem;opacity:0.7">€</span></div>` +
                        stationBlock(best.station_id, best.station_name, fuel, addressParts.join(', ')) +
                        `<div class="cheapest-time">${h(formatDateTime(best.recorded_at))}</div>` +
                        (runnersUp ? `<div class="rank-list">${runnersUp}</div>` : '') +
                    `</div>`;
                }).join('') +
              `</div>`
        );
}

function renderCheapestRange() {
    const t = translations[currentLang];
    renderPriceCard(cheapestRangeCard, getRangeFilteredData(), t.cheapestPrefix, (a, b) => a < b, ICON_DOWN, t.cheapestRangeNoData);
}

function renderHighest() {
    const t = translations[currentLang];
    renderPriceCard(highestCard, getRangeFilteredData(), t.highestPrefix, (a, b) => a > b, ICON_UP, t.highestNoData);
}

const ICON_CLOCK = `<svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" style="color:var(--amber);flex-shrink:0"><circle cx="12" cy="12" r="10"/><polyline points="12 6 12 12 16 14"/></svg>`;

// Upcoming predictions the notifier would send: windows picked server-side
// from the stored forecast grid for exactly the in-scope stations (medium/high
// confidence, cheapest-first per day like a notify subscriber to this area),
// grouped per fuel then per day. Within a day the cheapest window is shown
// large and the rest follow as a compact price-ranked list. Data comes from
// ?action=data (predictionData / predictionAsOf); the page never triggers a
// suggest run.
function renderPredictions() {
    const t = translations[currentLang];
    if (!predictionsCard) return;
    const fuels      = selectedFuel === 'all' ? ['e5', 'e10', 'diesel'] : [selectedFuel];
    const fuelColors = { e5: 'var(--e5)', e10: 'var(--e10)', diesel: 'var(--diesel)' };

    const nameById = (id) => (predictionStationMeta[id] && predictionStationMeta[id].name) || id;
    const distById = (id) => stationDistancesById[id] ?? null;
    // Address line for the top station, mirroring the top-5 card: street, place,
    // then distance. Distance moves out of the name and into this line. Markup,
    // so the distance keeps its sized-down separator.
    const addressHtmlById = (id) => {
        const meta = predictionStationMeta[id] || {};
        const parts = [meta.street, meta.place].filter(Boolean).map(h);
        const dist = distById(id);
        if (dist !== null) parts.push(fmtDistanceKmHtml(dist));
        return parts.join(', ');
    };
    // Day bucket key + header in the displayed timezone/locale so grouping
    // matches the visible date (recomputed on language change).
    const dayKey = (iso) => new Date(iso).toLocaleDateString(_loc(), {
        timeZone: _tz(), year: 'numeric', month: '2-digit', day: '2-digit',
    });
    const dayLabel = (iso) => new Date(iso).toLocaleDateString(_loc(), {
        timeZone: _tz(), weekday: 'long', day: '2-digit', month: '2-digit', year: '2-digit',
    });
    const windowLabel = (p) => `${formatTimeOnly(p.start)}–${formatTimeOnly(p.end)}`;

    const results = [];
    for (const fuel of fuels) {
        const windows = predictionData.filter((p) => p.fuel === fuel);
        if (windows.length) results.push({ fuel, windows });
    }

    const colClass = results.length === 1 ? ' single' : results.length === 2 ? ' two-col' : '';

    predictionsCard.innerHTML =
        `<div class="cheapest-header">${ICON_CLOCK}<span class="cheapest-title">${t.predictionsTitle}</span></div>` +
        (results.length === 0
            ? `<div class="cheapest-empty">${t.predictionsNoData}</div>`
            : `<div class="cheapest-grid${colClass}">` +
                results.map(({ fuel, windows }) => {
                    const asOf = predictionAsOf[fuel] || null;
                    // Group by day; predictionData is already sorted by start
                    // ascending, so first-seen day order is chronological.
                    const byDay = new Map();
                    for (const p of windows) {
                        const key = dayKey(p.start);
                        if (!byDay.has(key)) byDay.set(key, []);
                        byDay.get(key).push(p);
                    }

                    const days = [...byDay.values()].map((dayWindows) => {
                        dayWindows.sort((a, b) => (a.price - b.price) || a.start.localeCompare(b.start));
                        dayWindows = dayWindows.slice(0, 5); // cap: cheapest 5 per day
                        const best = dayWindows[0];
                        const bestName = nameById(best.s);
                        const bestAddr = addressHtmlById(best.s);
                        const runners = dayWindows.slice(1).map((p) => stationRankRow(
                            p.s,
                            nameById(p.s),
                            distById(p.s),
                            fuel,
                            p.price,
                            `<span class="pred-time">${h(windowLabel(p))}</span>`
                        )).join('');
                        return `<div class="pred-day">${h(dayLabel(best.start))}</div>` +
                            `<div class="cheapest-price" style="color:${fuelColors[fuel]}">${fmtPriceHtml(best.price)} <span style="font-size:1rem;opacity:0.7">€</span></div>` +
                            `<div class="cheapest-time">${h(windowLabel(best))}</div>` +
                            stationBlock(best.s, bestName, fuel, bestAddr) +
                            (runners ? `<div class="rank-list">${runners}</div>` : '');
                    }).join('');

                    return `<div class="cheapest-cell">` +
                        `<div class="cheapest-fuel-label" style="color:${fuelColors[fuel]}">${fuelConfig[fuel].label}</div>` +
                        days +
                        (asOf ? `<div class="pred-asof">${h(t.predictionsAsOf.replace('{time}', formatDateTime(asOf)))}</div>` : '') +
                    `</div>`;
                }).join('') +
              `</div>`
        );
}

/* ── Surroundings card ─────────────────────────────────────────── */
// The stations the selected location and radius admit, nearest first, each with
// the price it is showing right now. Its rows come from payload.nearby, which
// the server reads straight from the newest snapshot per station — so unlike
// the cards above it, this one is not narrowed by the date range or by the
// station picker. Pick a location in the filters (a city, an address, or the
// locate button) and this is what is around it.
const nearbyCard = document.getElementById('nearby-card');

const ICON_PIN = `<svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" style="color:var(--amber);flex-shrink:0"><path d="M20 10c0 6-8 12-8 12s-8-6-8-12a8 8 0 0 1 16 0z"/><circle cx="12" cy="10" r="3"/></svg>`;

function nearbyRowHtml(row, fuels) {
    const t = translations[currentLang];
    const meta = predictionStationMeta[row.s] || {};
    const name = meta.name || row.s;
    const distHtml = fmtDistanceKmHtml(row.dist);
    const distText = fmtDistanceKm(row.dist);
    const address = [meta.street, meta.place].filter(Boolean).map(h).join(', ');

    const prices = fuels.map((fuel) => {
        const value = row[fuel];
        const has = value !== null && value !== undefined;
        return `<span class="nearby-price${has ? '' : ' empty'}"${has ? ` style="color:${FUEL_CSS_COLORS[fuel]}"` : ''}>` +
            `<span class="nearby-price-label">${fuelConfig[fuel].label}</span>` +
            (has ? fmtPriceHtml(value) : '—') +
        `</span>`;
    }).join('');

    // Spoken label: distance, station, then each price — the same reading order
    // the row has visually, which the grid's column spans would otherwise lose.
    const spokenPrices = fuels
        .map((fuel) => `${fuelConfig[fuel].label} ${fmtPriceText(row[fuel], '—')}`)
        .join(', ');
    const spoken = [distText, name, spokenPrices].filter(Boolean).join(' — ');

    return `<button type="button" class="nearby-btn" data-station-id="${h(row.s)}"` +
        ` title="${h(t.sdHint)}" aria-label="${h(t.sdHint + ': ' + spoken)}">` +
        `<span class="nearby-dist">${distHtml === null ? '' : distHtml}</span>` +
        `<span class="nearby-name">` +
            stationDot(name, fuels[0]) +
            `<span class="sd-name-text">${h(name)}</span>` +
            (row.o ? '' : `<span class="nearby-closed">${h(t.openNo)}</span>`) +
            ICON_STATION_INFO +
        `</span>` +
        `<span class="nearby-addr">${address}</span>` +
        `<span class="nearby-prices">${prices}</span>` +
    `</button>`;
}

function renderNearby() {
    const t = translations[currentLang];
    if (!nearbyCard) return;
    const fuels = selectedFuel === 'all' ? ['e5', 'e10', 'diesel'] : [selectedFuel];

    const scope = locationLabel === ''
        ? ''
        : `<span class="cheapest-scope">${h(locationLabel + ' · ' + locationRadiusKm + ' km')}</span>`;
    const header = `<div class="cheapest-header">${ICON_PIN}<span class="cheapest-title">${h(t.nearbyTitle)}</span>${scope}</div>`;

    if (locationLabel === '') {
        nearbyCard.innerHTML = header + `<div class="cheapest-empty">${h(t.nearbyNoLocation)}</div>`;
        return;
    }
    if (nearbyRows.length === 0) {
        nearbyCard.innerHTML = header + `<div class="cheapest-empty">${h(t.nearbyNoData)}</div>`;
        return;
    }

    const visible = nearbyExpanded ? nearbyRows : nearbyRows.slice(0, NEARBY_PREVIEW_ROWS);
    const hidden = nearbyRows.length - visible.length;
    // Say when the radius holds more than the card asked the server for, so a
    // short list never reads as "that is all there is".
    const capped = nearbyTotal > nearbyRows.length
        ? `<div class="nearby-foot">${h(t.nearbyCapped
            .replace('{shown}', String(nearbyRows.length))
            .replace('{total}', String(nearbyTotal)))}</div>`
        : '';

    nearbyCard.innerHTML = header +
        `<div class="nearby-list">${visible.map((row) => nearbyRowHtml(row, fuels)).join('')}</div>` +
        (hidden > 0
            ? `<div class="nearby-more"><button type="button" class="btn-small" id="nearby-more">${h(t.showMore)} (${hidden})</button></div>`
            : '') +
        capped;
}

document.addEventListener('click', (e) => {
    if (e.target instanceof Element && e.target.closest('#nearby-more')) {
        nearbyExpanded = true;
        renderNearby();
    }
});

/* ── Station detail dialog ─────────────────────────────────────── */
// Clicking/tapping any station in the four price cards opens this dialog: the
// full record for that station (current prices, address, open state, upcoming
// windows) plus a Google Maps navigation link built here in the UI — the link
// is derived from the station's coordinates (or address) and never persisted.
const stationDialog = document.getElementById('station-dialog');
let openStationId = null;

const ICON_CLOSE = `<svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><line x1="18" y1="6" x2="6" y2="18"/><line x1="6" y1="6" x2="18" y2="18"/></svg>`;
const ICON_NAV   = `<svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><polygon points="3 11 22 2 13 21 11 13 3 11"/></svg>`;

// Google Maps directions link: coordinates when we have them (most precise),
// otherwise the postal address as a search string.
function googleMapsUrl(meta, name) {
    const base = 'https://www.google.com/maps/dir/?api=1&travelmode=driving&destination=';
    const lat = typeof meta.lat === 'number' ? meta.lat : null;
    const lng = typeof meta.lng === 'number' ? meta.lng : null;
    if (lat !== null && lng !== null) {
        return base + encodeURIComponent(lat.toFixed(6) + ',' + lng.toFixed(6));
    }
    const query = [name, meta.street, [meta.zip, meta.place].filter(Boolean).join(' ')]
        .filter(Boolean).join(', ');
    return base + encodeURIComponent(query);
}

function stationDialogHtml(stationId) {
    const t       = translations[currentLang];
    const meta    = predictionStationMeta[stationId] || {};
    const name    = meta.name || stationId;
    // The chart payload first; the surroundings card's current price is the
    // fallback for a station the date range or the station picker excluded.
    const latest  = latestRowById().get(stationId) || nearbyLatestById.get(stationId) || null;
    const dist    = stationDistancesById[stationId] ?? null;
    const addressLines = [meta.street, [meta.zip, meta.place].filter(Boolean).join(' ')].filter(Boolean);

    const prices = ['e5', 'e10', 'diesel'].map((fuel) => {
        const value = latest ? latest[fuel] : null;
        const has   = value !== null && value !== undefined;
        return `<div class="sd-price${has ? '' : ' empty'}">` +
            `<div class="sd-price-label"${has ? ` style="color:${FUEL_CSS_COLORS[fuel]}"` : ''}>${fuelConfig[fuel].label}</div>` +
            `<div class="sd-price-value"${has ? ` style="color:${FUEL_CSS_COLORS[fuel]}"` : ''}>` +
                (has ? `${fmtPriceHtml(value)} <span style="font-size:0.8rem;opacity:0.6">€</span>` : '—') +
            `</div>` +
        `</div>`;
    }).join('');

    const rows = [];
    if (addressLines.length) {
        rows.push([t.sdAddress, addressLines.map((line) => h(line)).join('<br>')]);
    }
    if (meta.brand) rows.push([t.brand, h(meta.brand)]);
    if (dist !== null) rows.push([t.sdDistance, fmtDistanceKmHtml(dist)]);
    if (latest) rows.push([t.sdLastUpdate, h(formatDateTime(latest.recorded_at))]);

    // Upcoming suggestion windows for this station (same source as the
    // predictions card), so the dialog is complete when opened from there.
    const windows = predictionData
        .filter((p) => p.s === stationId)
        .slice(0, 5)
        .map((p) => {
            const day = new Date(p.start).toLocaleDateString(_loc(), {
                timeZone: _tz(), weekday: 'short', day: '2-digit', month: '2-digit',
            });
            return `<div class="sd-window">` +
                `<span class="sd-window-fuel" style="color:${FUEL_CSS_COLORS[p.fuel]}">${fuelConfig[p.fuel].label}</span>` +
                `<span class="sd-window-time">${h(day + ' · ' + formatTimeOnly(p.start) + '–' + formatTimeOnly(p.end))}</span>` +
                `<span class="sd-window-price" style="color:${FUEL_CSS_COLORS[p.fuel]}">${fmtPriceHtml(p.price)}</span>` +
            `</div>`;
        }).join('');

    const openTag = latest
        ? `<span class="sd-tag ${latest.is_open ? 'is-open' : 'is-closed'}">` +
              `<span class="sd-tag-dot"></span>${h(latest.is_open ? t.openYes : t.openNo)}` +
          `</span>`
        : '';

    return `<div class="sd-head">` +
            `<button type="button" class="sd-close" data-sd-close aria-label="${h(t.sdClose)}" title="${h(t.sdClose)}">${ICON_CLOSE}</button>` +
            `<div class="sd-kicker">${h(t.sdTitle)}</div>` +
            `<h2 class="sd-name" id="sd-name">` +
                stationDot(name, 'e10') +
                `<span>${h(name)}</span>` +
            `</h2>` +
            `<div class="sd-tags">` +
                openTag +
                (dist !== null ? `<span class="sd-tag">${fmtDistanceKmHtml(dist)}</span>` : '') +
                (meta.brand ? `<span class="sd-tag">${h(meta.brand)}</span>` : '') +
            `</div>` +
        `</div>` +
        `<div class="sd-prices">${prices}</div>` +
        `<div class="sd-body">` +
            (latest ? '' : `<div class="sd-note">${h(t.sdNoPrices)}</div>`) +
            rows.map(([key, value]) =>
                `<div class="sd-row"><span class="sd-key">${h(key)}</span><span class="sd-val">${value}</span></div>`
            ).join('') +
            (windows
                ? `<div class="sd-row"><span class="sd-key">${h(t.sdUpcoming)}</span>` +
                  `<span class="sd-val"><span class="sd-windows">${windows}</span></span></div>`
                // Say so rather than dropping the section, so its absence reads
                // as "none predicted" instead of "sometimes there, sometimes not".
                : `<div class="sd-key sd-key-alone">${h(t.sdNoUpcoming)}</div>`) +
            `<a class="sd-nav" href="${h(googleMapsUrl(meta, name))}" target="_blank" rel="noopener noreferrer">` +
                `${ICON_NAV}<span>${h(t.sdNavigate)}</span>` +
            `</a>` +
        `</div>`;
}

function openStationDialog(stationId) {
    if (!stationDialog || !stationId) return;
    openStationId = stationId;
    stationDialog.innerHTML = stationDialogHtml(stationId);
    if (!stationDialog.open) stationDialog.showModal();
}

function closeStationDialog() {
    if (stationDialog && stationDialog.open) stationDialog.close();
}

if (stationDialog) {
    // Open: any station reference inside one of the four price cards.
    document.addEventListener('click', (e) => {
        const trigger = e.target instanceof Element
            ? e.target.closest('.cheapest-card [data-station-id]')
            : null;
        if (trigger) openStationDialog(trigger.dataset.stationId);
    });

    stationDialog.addEventListener('click', (e) => {
        // Close button, or a click on the backdrop area outside the panel.
        if (e.target instanceof Element && e.target.closest('[data-sd-close]')) {
            closeStationDialog();
            return;
        }
        if (e.target === stationDialog) closeStationDialog();
    });

    stationDialog.addEventListener('close', () => { openStationId = null; });
}

/* applyLang lives in the shared script (renderCommonScript). */

/* ── Theme toggle lives in the shared script (renderCommonScript). ── */

/* ── Quick date-range buttons ──────────────────────────────────── */
function onDateChange(el) {
    document.getElementById('f-range').value = '';
    el.form.submit();
}

(function () {
    const rangeInput = document.getElementById('f-range');
    const fromInput  = document.getElementById('f-from');
    const toInput    = document.getElementById('f-to');
    const form       = rangeInput?.closest('form');
    if (!rangeInput || !fromInput || !toInput || !form) return;

    function updateActiveStates() {
        const active = rangeInput.value;
        document.querySelectorAll('.quick-range-btn').forEach((btn) => {
            btn.classList.toggle('active', btn.dataset.range === active);
        });
    }

    document.querySelectorAll('.quick-range-btn').forEach((btn) => {
        btn.addEventListener('click', () => {
            rangeInput.value = btn.dataset.range;
            fromInput.value  = '';
            toInput.value    = '';
            updateActiveStates();
            form.submit();
        });
    });

    updateActiveStates();
})();

/* ── Station picker filter ─────────────────────────────────────── */
// Narrows the checkbox list to rows matching the typed text. Only display is
// touched: checked boxes stay checked (and keep submitting) while hidden, so
// filtering can never lose a selection.
(function () {
    const input = document.getElementById('f-station-filter');
    if (!input) return;
    const options = [...document.querySelectorAll('#station-options .station-option')];

    input.addEventListener('input', () => {
        const query = input.value.trim().toLowerCase();
        for (const option of options) {
            option.classList.toggle(
                'filtered-out',
                query !== '' && !option.textContent.toLowerCase().includes(query)
            );
        }
    });
})();

/* ── Location field: city, address, or the browser's own position ── */
// Two very different lookups behind one input. Typing searches the cached
// places table and nothing else: Nominatim's usage policy rules out
// autocomplete traffic, so a keystroke never leaves the host. The trailing
// "search address" row and the locate button are the explicit acts that do
// spend one lookup — and the server caches what they resolve, so the same
// address is an ordinary typeahead hit from then on (?action=geocode).
(function () {
    const wrap   = document.getElementById('city-ac');
    const input  = document.getElementById('f-city');
    const label  = document.getElementById('f-loc-label');
    const latIn  = document.getElementById('f-loc-lat');
    const lngIn  = document.getElementById('f-loc-lng');
    const list   = document.getElementById('city-ac-list');
    const form   = input?.closest('form');
    const radius = document.getElementById('f-radius');
    const locate = document.getElementById('f-locate');

    if (!wrap || !input || !label || !latIn || !lngIn || !list || !form) return;

    const ICON_SEARCH = `<svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="11" cy="11" r="7"/><line x1="16.5" y1="16.5" x2="21" y2="21"/></svg>`;

    let controller = null;
    let activeIdx  = -1;
    // One outbound lookup at a time; the locate button and the address row
    // share it, so neither can fire while the other is waiting.
    let busy = false;

    function showList() {
        list.hidden = false;
        input.setAttribute('aria-expanded', 'true');
    }

    function hideList() {
        list.hidden = true;
        input.setAttribute('aria-expanded', 'false');
        activeIdx = -1;
    }

    function setActive(idx) {
        const items = list.querySelectorAll('.city-ac-item');
        items.forEach((el, i) => el.setAttribute('aria-selected', String(i === idx)));
        activeIdx = idx;
    }

    // Apply a resolved place: the three hidden fields are the location, the
    // text box is only what the reader reads. Saving is what applying means
    // now — the post writes the filter row and the page comes back from it.
    function applyLocation(placeLabel, lat, lng) {
        input.value = placeLabel;
        label.value = placeLabel;
        latIn.value = placeLabel === '' ? '' : String(lat);
        lngIn.value = placeLabel === '' ? '' : String(lng);
        hideList();
        if (radius) radius.disabled = (placeLabel === '');
        form.submit();
    }

    // Replaces the dropdown with a single status line ("searching", "denied",
    // "nothing found"), so the field answers where the reader is looking.
    function showMessage(text, isError) {
        list.innerHTML = '';
        const li = document.createElement('li');
        li.className = 'city-ac-empty' + (isError ? ' is-error' : '');
        li.textContent = text;
        list.appendChild(li);
        showList();
    }

    // The one path that spends a geocoding request. The server resolves it,
    // writes it into the places cache and answers with the key the filter
    // carries, which is then selected exactly like a cached match.
    async function resolveLocation(params) {
        if (busy) return;
        busy = true;
        if (locate) locate.disabled = true;
        showMessage(translations[currentLang].locating, false);
        try {
            const url = new URL(location.href);
            url.search = '';
            url.searchParams.set('action', 'geocode');
            url.searchParams.set('csrf', geocodeCsrf);
            for (const [key, value] of Object.entries(params)) {
                url.searchParams.set(key, value);
            }
            const res = await fetch(url.toString(), { headers: { Accept: 'application/json' } });
            if (res.status === 401) { location.href = '?page=login'; return; }
            const payload = await res.json();
            const t = translations[currentLang];
            if (payload && payload.label) {
                applyLocation(payload.label, payload.lat, payload.lng);
                return;
            }
            const failure = payload && payload.errors && payload.errors[0];
            showMessage((failure && t[failure.key]) || t.geocodeFailed, true);
        } catch {
            showMessage(translations[currentLang].geocodeFailed, true);
        } finally {
            busy = false;
            if (locate) locate.disabled = false;
        }
    }

    function searchAddress() {
        const q = input.value.trim();
        if (q.length < 3) return;
        resolveLocation({ q });
    }

    async function fetchMatches(q) {
        if (controller) controller.abort();
        controller = new AbortController();
        try {
            const url = new URL(location.href);
            url.search = '';
            url.searchParams.set('action', 'city_search');
            url.searchParams.set('q', q);
            const res = await fetch(url.toString(), { signal: controller.signal });
            return await res.json();
        } catch {
            return null;
        }
    }

    // Closing row of the dropdown: what the cache could not answer, the
    // geocoder can. Always offered, because a cached city and a house number
    // in that same city are both plausible readings of what was typed.
    function addressRow(q) {
        const li = document.createElement('li');
        li.className = 'city-ac-item city-ac-search';
        li.role      = 'option';
        li.setAttribute('aria-selected', 'false');
        const label = document.createElement('span');
        label.textContent = translations[currentLang].searchAddress.replace('{query}', q);
        li.innerHTML = ICON_SEARCH;
        li.appendChild(label);
        li.addEventListener('mousedown', (e) => {
            e.preventDefault();
            searchAddress();
        });
        return li;
    }

    let debounceTimer = null;

    input.addEventListener('input', () => {
        // Typing alone changes nothing: the stored location stands until a
        // resolved place replaces it, so an unfinished edit cannot ride along
        // on the next radius or fuel change.
        const q = input.value.trim();
        clearTimeout(debounceTimer);
        if (q.length < 3) { hideList(); return; }

        debounceTimer = setTimeout(async () => {
            const results = await fetchMatches(q);
            if (results === null) return;
            // A slower answer to an older prefix must not overwrite the list.
            if (input.value.trim() !== q) return;

            list.innerHTML = '';
            results.forEach((match) => {
                const li = document.createElement('li');
                li.className    = 'city-ac-item';
                li.role         = 'option';
                li.setAttribute('aria-selected', 'false');
                li.textContent  = match.label;
                li.addEventListener('mousedown', (e) => {
                    e.preventDefault();
                    applyLocation(match.label, match.lat, match.lng);
                });
                list.appendChild(li);
            });
            list.appendChild(addressRow(q));
            showList();
            activeIdx = -1;
        }, 200);
    });

    input.addEventListener('keydown', (e) => {
        const items = [...list.querySelectorAll('.city-ac-item')];
        if (e.key === 'ArrowDown') {
            e.preventDefault();
            setActive(Math.min(activeIdx + 1, items.length - 1));
        } else if (e.key === 'ArrowUp') {
            e.preventDefault();
            setActive(Math.max(activeIdx - 1, 0));
        } else if (e.key === 'Enter' && !list.hidden && activeIdx >= 0 && items[activeIdx]) {
            e.preventDefault();
            items[activeIdx].dispatchEvent(new MouseEvent('mousedown'));
        } else if (e.key === 'Enter' && input.value.trim() !== label.value) {
            // Nothing highlighted, and the box no longer says what is stored:
            // read Enter as "resolve what I just typed" rather than as a
            // submit that would re-save the location already in place.
            e.preventDefault();
            searchAddress();
        } else if (e.key === 'Escape') {
            hideList();
        }
    });

    input.addEventListener('blur', () => setTimeout(hideList, 150));

    // Clearing the box and leaving it is how a reader says "no location".
    input.addEventListener('change', () => {
        if (input.value.trim() === '' && label.value !== '') applyLocation('', 0, 0);
    });

    if (locate) {
        locate.addEventListener('click', () => {
            const t = translations[currentLang];
            if (!navigator.geolocation) { showMessage(t.gpsUnsupported, true); return; }
            if (busy) return;
            busy = true;
            locate.disabled = true;
            showMessage(t.locating, false);
            // One fix, once. The resolved address becomes the filter's
            // location and rides along in the filter cookie, so later visits
            // reuse it instead of asking the browser again; maximumAge lets
            // the browser answer from a recent fix rather than powering the
            // receiver up for a second one.
            navigator.geolocation.getCurrentPosition(
                (position) => {
                    busy = false;
                    locate.disabled = false;
                    resolveLocation({
                        lat: position.coords.latitude.toFixed(6),
                        lng: position.coords.longitude.toFixed(6),
                    });
                },
                (error) => {
                    busy = false;
                    locate.disabled = false;
                    showMessage(error && error.code === 1 ? t.gpsDenied : t.gpsFailed, true);
                },
                { enableHighAccuracy: true, timeout: 12000, maximumAge: 600000 }
            );
        });
    }

    document.addEventListener('click', (e) => {
        if (!wrap.contains(e.target)) hideList();
    });
})();

/* ── Async snapshot data ───────────────────────────────────────── */
// The shell paints immediately; the (potentially huge) snapshot payload is
// fetched here and rendered client-side, so first paint is never blocked on it.
function setStat(id, value) {
    const el = document.getElementById(id);
    if (!el) return;
    el.textContent = String(value);
    el.classList.remove('skeleton');
    el.removeAttribute('aria-busy');
}

// Like setStat, but formats a raw timestamp locale-aware (date AND time, so
// the range boundaries are exact) and stashes the raw value in
// data-recorded-at, which applyLang() re-formats on a language change.
function setStatDate(id, isoValue) {
    const el = document.getElementById(id);
    if (!el) return;
    if (isoValue) el.dataset.recordedAt = isoValue; else el.removeAttribute('data-recorded-at');
    setStat(id, isoValue ? formatDateTime(isoValue) : '—');
}

function applyData(payload) {
    if (retryWrap) retryWrap.hidden = true;
    // Fresh data may not contain the station the dialog is showing.
    closeStationDialog();
    const meta = payload.stations || {};
    // Expand slim rows back into the shape the existing renderers expect by
    // joining each row to its station metadata (sent once, keyed by id).
    chartData = (payload.rows || []).map((r) => {
        const s = meta[r.s] || {};
        return {
            station_id:  r.s,
            station_name: s.name || r.s,
            brand:  s.brand  || '',
            street: s.street || '',
            place:  s.place  || '',
            recorded_at: r.t,
            is_open: !!r.o,
            e5:     r.e5 ?? null,
            e10:    r.e10 ?? null,
            diesel: r.diesel ?? null,
            _ts:    Date.parse(r.t),
        };
    });
    stationDistancesById = {};
    for (const [id, s] of Object.entries(meta)) {
        if (s.dist !== null && s.dist !== undefined) stationDistancesById[id] = s.dist;
    }
    predictionData = payload.predictions || [];
    predictionAsOf = payload.predictions_as_of || {};
    predictionStationMeta = meta;
    nearbyRows = payload.nearby || [];
    nearbyTotal = payload.nearby_total || nearbyRows.length;
    nearbyExpanded = false;
    // Same row, re-shaped to what the detail dialog reads, so a station the
    // date filter kept out of the chart still opens with a current price.
    nearbyLatestById = new Map(nearbyRows.map((row) => [row.s, {
        recorded_at: row.t,
        is_open: !!row.o,
        e5: row.e5 ?? null,
        e10: row.e10 ?? null,
        diesel: row.diesel ?? null,
    }]));
    _stationHues = computeStationHues();
    stationFilter = null;
    dataLoaded = true;

    const sum = payload.summary || {};
    setStat('stat-points',   sum.points   ?? 0);
    setStat('stat-stations', sum.stations ?? 0);
    setStatDate('stat-first', sum.first_recorded_at || '');
    setStatDate('stat-last',  sum.last_recorded_at  || '');

    renderCheapest();
    renderPredictions();
    renderNearby();
    renderCheapestRange();
    renderHighest();
    if (chartEl) renderChart();
}

// Deliberately leaves dataLoaded=false so a later language/theme change does
// NOT re-run the data renderers and wipe the error UI with empty placeholders.
function showDataError(err) {
    const t = translations[currentLang];
    const key = (err && err.key && t[err.key]) ? err.key : 'loadError';
    const msg = (err && err.key && t[err.key]) ? t[err.key]
        : (err && err.message) ? err.message
        : t.loadError;
    ['stat-points', 'stat-stations', 'stat-first', 'stat-last'].forEach((id) => setStat(id, '—'));
    const loadingEl = document.getElementById('chart-loading');
    if (loadingEl) loadingEl.hidden = true;
    const emptyEl = document.getElementById('chart-empty');
    if (emptyEl) {
        emptyEl.hidden = false;
        emptyEl.removeAttribute('aria-busy');
        // Retarget i18n so a later language switch keeps the error text.
        emptyEl.dataset.i18n = key;
        emptyEl.textContent = msg;
    }
    if (chartEl)  chartEl.toggleAttribute('hidden', true);
    if (legendEl) legendEl.hidden = true;
    if (retryWrap) retryWrap.hidden = false;
}

function resetLoadingUI() {
    if (retryWrap) retryWrap.hidden = true;
    const loadingEl = document.getElementById('chart-loading');
    if (loadingEl) loadingEl.hidden = false;
    const emptyEl = document.getElementById('chart-empty');
    if (emptyEl) { emptyEl.hidden = true; emptyEl.dataset.i18n = 'noSnapshots'; }
    ['stat-points', 'stat-stations', 'stat-first', 'stat-last'].forEach((id) => {
        const el = document.getElementById(id);
        if (el) { el.textContent = ' '; el.classList.add('skeleton'); el.setAttribute('aria-busy', 'true'); }
    });
}

async function loadData() {
    // No filters in the URL: the endpoint reads the same stored row the page
    // was rendered from, so there is nothing to copy across.
    const url = new URL(location.href);
    url.search = '';
    url.searchParams.set('action', 'data');
    try {
        const res = await fetch(url.toString(), { headers: { Accept: 'application/json' } });
        if (res.status === 401) { location.href = '?page=login'; return; }
        if (!res.ok) throw new Error('HTTP ' + res.status);
        const payload = await res.json();
        // Surface application-level errors (invalid date, city not found, …)
        // instead of silently rendering an empty result — matches the old
        // synchronous behaviour where errors showed in the error box.
        if (payload.errors && payload.errors.length) {
            showDataError(payload.errors[0]);
            return;
        }
        applyData(payload);
    } catch (e) {
        showDataError();
    }
}

const retryWrap = document.getElementById('chart-retry');
const retryBtn  = document.getElementById('retry-btn');
if (retryBtn) retryBtn.addEventListener('click', () => { resetLoadingUI(); loadData(); });

/* Shared-script hooks: the dashboard re-renders on language/theme change. */
window.onLangChange = () => {
    if (dataLoaded) {
        renderCheapest();
        renderPredictions();
        renderNearby();
        renderCheapestRange();
        renderHighest();
        if (chartEl) renderChart();
        // The dialog is rendered imperatively, so re-render it while it is open.
        if (openStationId) openStationDialog(openStationId);
    }
};
window.onThemeChange = () => {
    if (chartEl && dataLoaded) renderChart();
};
</script>
<?php renderCommonScript(); ?>
<script>
if (hasDataScope) {
    loadData();
} else {
    applyData({});
}
</script>
</body>
</html>
