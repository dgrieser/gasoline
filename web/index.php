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

    return $pdo;
}

// ── Auth: session / CSRF / flash helpers ─────────────────────────────────────

function gasolineStartSession(): void
{
    $isHttps = (($_SERVER['HTTPS'] ?? '') !== '' && strtolower((string) $_SERVER['HTTPS']) !== 'off')
        || strtolower((string) ($_SERVER['HTTP_X_FORWARDED_PROTO'] ?? '')) === 'https';
    ini_set('session.use_strict_mode', '1');
    session_set_cookie_params([
        'httponly' => true,
        'samesite' => 'Lax',
        'secure' => $isHttps,
        'path' => '/',
    ]);
    session_name('gasoline_session');
    session_start();
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

function gasolineSchemaReady(PDO $pdo, string $driver): bool
{
    try {
        if ($driver === 'mysql') {
            $stmt = $pdo->query(
                "SELECT COUNT(*) AS n FROM information_schema.tables
                 WHERE table_schema = DATABASE() AND table_name IN ('users', 'settings', 'update_targets')"
            );
        } else {
            $stmt = $pdo->query(
                "SELECT COUNT(*) AS n FROM sqlite_master
                 WHERE type = 'table' AND name IN ('users', 'settings', 'update_targets')"
            );
        }
        $row = $stmt->fetch();
        return (int) ($row['n'] ?? 0) === 3;
    } catch (Throwable $e) {
        error_log('gasoline schema check error: ' . $e->getMessage());
        return false;
    }
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

function currentUser(PDO $pdo): ?array
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
        // Deleted, demoted to pending, or otherwise stale: sign out.
        $_SESSION = [];
        session_destroy();
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

/** Validates an already comma-separated windows string (admin text input). */
function validWindowListString(string $value): bool
{
    foreach (explode(',', $value) as $part) {
        $part = trim($part);
        if ($part === '') {
            return false;
        }
        $pair = explode('-', $part);
        if (count($pair) !== 2 || !validHHMM(trim($pair[0])) || !validHHMM(trim($pair[1]))) {
            return false;
        }
    }
    return true;
}

/** Validates an already comma-separated HH:MM list string (admin text input). */
function validTimeListString(string $value): bool
{
    foreach (explode(',', $value) as $part) {
        if (!validHHMM(trim($part))) {
            return false;
        }
    }
    return true;
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
    $user = currentUser($pdo);

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
                $_SESSION = [];
                session_destroy();
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
            if ($method !== 'pushover' || $days === null || $windows === null || $times === null) {
                setFlash('error', 'invalidNotifySettings');
                redirectTo('?page=account');
            }
            $stmt = $pdo->prepare(
                'UPDATE users SET notify_method = :method, pushover_app_name = :app,
                    pushover_user_key = :user_key, pushover_token = :token,
                    notify_days = :days, notify_windows = :windows,
                    notify_suggest_times = :times, notify_check_enabled = :check_enabled
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
            $stmt->bindValue(':id', (int) $user['id'], PDO::PARAM_INT);
            $stmt->execute();
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
            $stmt = $pdo->prepare('DELETE FROM users WHERE id = :id');
            $stmt->bindValue(':id', (int) $user['id'], PDO::PARAM_INT);
            $stmt->execute();
            $_SESSION = [];
            session_destroy();
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
            $fields = [
                'fuel' => static fn (string $v): bool => in_array($v, ['diesel', 'e5', 'e10'], true),
                'range_km' => static fn (string $v): bool => is_numeric($v) && (float) $v > 0 && (float) $v <= 100,
                'history_days' => static fn (string $v): bool => ctype_digit($v) && (int) $v > 0 && (int) $v <= 365,
                'predict_days' => static fn (string $v): bool => ctype_digit($v) && (int) $v > 0 && (int) $v <= 14,
                'limit_per_day' => static fn (string $v): bool => ctype_digit($v) && (int) $v >= 0 && (int) $v <= 50,
                'check_limit' => static fn (string $v): bool => ctype_digit($v) && (int) $v >= 0 && (int) $v <= 50,
                'suggest_times' => static fn (string $v): bool => validTimeListString($v),
                'check_reset_time' => static fn (string $v): bool => validHHMM($v),
                'notify_days' => null, // submitted as checkbox array below
                'notify_windows' => static fn (string $v): bool => validWindowListString($v),
                'check_template' => static fn (string $v): bool => $v !== '',
                'suggest_template' => static fn (string $v): bool => $v !== '',
                // Title templates may be empty: notifications then fall back
                // to each user's configured notification title.
                'check_title_template' => static fn (string $v): bool => true,
                'suggest_title_template' => static fn (string $v): bool => true,
            ];
            $kv = [];
            foreach ($fields as $name => $validate) {
                if ($name === 'notify_days') {
                    if (isset($_POST['notify_days'])) {
                        $days = normalizeDayList((array) $_POST['notify_days']);
                        if ($days === null) {
                            setFlash('error', 'invalidSettings');
                            redirectTo('?page=admin_settings');
                        }
                        $kv[$name] = $days;
                    }
                    continue;
                }
                if (!isset($_POST[$name])) {
                    continue;
                }
                $value = trim((string) $_POST[$name]);
                if ($validate !== null && !$validate($value)) {
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
                <div class="field">
                    <label class="check-toggle"><input type="checkbox" name="notify_check_enabled" <?= (int) $user['notify_check_enabled'] === 1 ? 'checked' : '' ?>><span data-i18n="notifyCheckEnabled">Send buy-now alerts when prices drop</span></label>
                </div>
                <button type="submit" class="btn-primary" data-i18n="save">Save</button>
            </form>
        </div>

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

function renderAdminSettingsPage(PDO $pdo, string $driver, array $user): never
{
    $settings = settingsAll($pdo);
    $targets = $pdo->query('SELECT id, city, radius_km FROM update_targets ORDER BY id ASC')->fetchAll();
    $get = static fn (string $name, string $fallback = ''): string => trim((string) ($settings[$name] ?? $fallback));
    $fuel = $get('fuel', 'diesel');
    renderPageStart('Settings', $user, 'admin_settings');
    ?>
    <div class="settings-layout wide">
        <?php renderFlash(); ?>

        <div class="settings-card">
            <h2 data-i18n="updateTargets">Automatic updates</h2>
            <p class="auth-note" data-i18n="updateTargetsHint">These cities are updated automatically by `gasoline update` (and used by suggest/check/notify) when the CLI is invoked without --city/--radius flags.</p>
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
            <h2 data-i18n="suggestionSettings">Suggestions &amp; checks</h2>
            <form method="post" action="">
                <?= csrfField() ?>
                <input type="hidden" name="action" value="save_settings">
                <div class="field-grid">
                    <div class="field">
                        <label for="st-fuel" data-i18n="settingFuel">Fuel</label>
                        <select id="st-fuel" name="fuel">
                            <option value="diesel" <?= $fuel === 'diesel' ? 'selected' : '' ?>>Diesel</option>
                            <option value="e5" <?= $fuel === 'e5' ? 'selected' : '' ?>>E5</option>
                            <option value="e10" <?= $fuel === 'e10' ? 'selected' : '' ?>>E10</option>
                        </select>
                    </div>
                    <div class="field">
                        <label for="st-range" data-i18n="settingRangeKm">Range (km)</label>
                        <input type="number" id="st-range" name="range_km" min="1" max="100" step="0.5" value="<?= h($get('range_km', '5')) ?>">
                    </div>
                    <div class="field">
                        <label for="st-history" data-i18n="settingHistoryDays">History days</label>
                        <input type="number" id="st-history" name="history_days" min="1" max="365" value="<?= h($get('history_days', '21')) ?>">
                    </div>
                    <div class="field">
                        <label for="st-predict" data-i18n="settingPredictDays">Prediction days</label>
                        <input type="number" id="st-predict" name="predict_days" min="1" max="14" value="<?= h($get('predict_days', '3')) ?>">
                    </div>
                    <div class="field">
                        <label for="st-limit-day" data-i18n="settingLimitPerDay">Suggestions per day</label>
                        <input type="number" id="st-limit-day" name="limit_per_day" min="0" max="50" value="<?= h($get('limit_per_day', '3')) ?>">
                    </div>
                    <div class="field">
                        <label for="st-check-limit" data-i18n="settingCheckLimit">Check row limit</label>
                        <input type="number" id="st-check-limit" name="check_limit" min="0" max="50" value="<?= h($get('check_limit', '5')) ?>">
                    </div>
                    <div class="field">
                        <label for="st-suggest-times" data-i18n="settingSuggestTimes">Default suggestion times</label>
                        <input type="text" id="st-suggest-times" name="suggest_times" pattern="\s*([01][0-9]|2[0-3]):[0-5][0-9](\s*,\s*([01][0-9]|2[0-3]):[0-5][0-9])*\s*" placeholder="08:00,13:00" value="<?= h($get('suggest_times', '08:00,13:00')) ?>">
                    </div>
                    <div class="field">
                        <label for="st-reset" data-i18n="settingCheckResetTime">Check baseline reset</label>
                        <input type="text" id="st-reset" name="check_reset_time" value="<?= h($get('check_reset_time', '00:00')) ?>" maxlength="5" pattern="([01][0-9]|2[0-3]):[0-5][0-9]" placeholder="HH:MM" title="HH:MM">
                    </div>
                </div>
                <div class="field">
                    <label for="st-windows" data-i18n="settingNotifyWindows">Default notification windows</label>
                    <input type="text" id="st-windows" name="notify_windows" placeholder="07:00-21:00" value="<?= h($get('notify_windows', '07:00-21:00')) ?>">
                </div>
                <div class="field">
                    <label data-i18n="settingNotifyDays">Default notification days</label>
                    <div class="day-toggles">
                        <?php $defaultDays = array_flip(array_filter(array_map('trim', explode(',', $get('notify_days', 'mon,tue,wed,thu,fri,sat,sun'))))); ?>
                        <?php foreach (GASOLINE_WEEKDAYS as $day) { ?>
                        <label class="day-toggle"><input type="checkbox" name="notify_days[]" value="<?= h($day) ?>" <?= isset($defaultDays[$day]) ? 'checked' : '' ?>><span data-i18n="day_<?= h($day) ?>"><?= h(ucfirst($day)) ?></span></label>
                        <?php } ?>
                    </div>
                </div>
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
$isJSONRequest = in_array($requestedAction, ['city_search', 'data'], true);

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

handlePost($authPdo, $dbDriver);

$currentUser = currentUser($authPdo);

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
    case 'admin_settings':
        if ((int) $currentUser['is_admin'] !== 1) {
            redirectTo('');
        }
        renderAdminSettingsPage($authPdo, $dbDriver, $currentUser);
        // no break
}

// Fall through: the dashboard (default page) below.

// ── Filter persistence ────────────────────────────────────────────────────────
// The last submitted dashboard filters are kept in a cookie so revisiting the
// bare URL restores them. Requests with explicit filter params refresh the
// cookie; requests without any are populated from it before validation below.
$filterCookieName = 'gasoline_filters';
$filterParamKeys = ['city', 'radius_km', 'range', 'from', 'to', 'fuel', 'station_ids'];
$cookieSecure = (($_SERVER['HTTPS'] ?? '') !== '' && strtolower((string) $_SERVER['HTTPS']) !== 'off')
    || strtolower((string) ($_SERVER['HTTP_X_FORWARDED_PROTO'] ?? '')) === 'https';

if (isset($_GET['reset'])) {
    setcookie($filterCookieName, '', ['expires' => time() - 3600, 'path' => '/', 'secure' => $cookieSecure, 'httponly' => true, 'samesite' => 'Lax']);
    redirectTo('');
}

$hasFilterParams = false;
foreach ($filterParamKeys as $filterKey) {
    if (array_key_exists($filterKey, $_GET)) {
        $hasFilterParams = true;
        break;
    }
}

if ($hasFilterParams) {
    // The async ?action=data fetch copies the page URL, so only page loads
    // need to refresh the cookie.
    if (!isset($_GET['action'])) {
        $filtersToSave = [];
        foreach ($filterParamKeys as $filterKey) {
            if (array_key_exists($filterKey, $_GET)) {
                $filtersToSave[$filterKey] = $_GET[$filterKey];
            }
        }
        setcookie($filterCookieName, json_encode($filtersToSave), ['expires' => time() + 60 * 60 * 24 * 365, 'path' => '/', 'secure' => $cookieSecure, 'httponly' => true, 'samesite' => 'Lax']);
    }
} else {
    $savedFilters = json_decode((string) ($_COOKIE[$filterCookieName] ?? ''), true);
    if (is_array($savedFilters)) {
        foreach ($filterParamKeys as $filterKey) {
            if (!array_key_exists($filterKey, $savedFilters)) {
                continue;
            }
            $savedValue = $savedFilters[$filterKey];
            if ($filterKey === 'station_ids') {
                if (is_array($savedValue)) {
                    $_GET[$filterKey] = array_values(array_filter($savedValue, 'is_string'));
                }
            } elseif (is_string($savedValue)) {
                $_GET[$filterKey] = $savedValue;
            }
        }
    }
}

$filtersCollapsed = (($_COOKIE['gasoline_filters_collapsed'] ?? '') === '1');

$errors = [];
$stations = [];

$selectedStationIds = array_values(array_filter(
    array_map(
        static function ($value): string {
            return trim((string) $value);
        },
        (array) ($_GET['station_ids'] ?? [])
    ),
    static function (string $value): bool {
        return $value !== '';
    }
));

$selectedRange = trim((string) ($_GET['range'] ?? ''));
$validRanges = ['7d', '14d', '30d'];
if (!in_array($selectedRange, $validRanges, true)) {
    $selectedRange = '';
}

$fromDate = trim((string) ($_GET['from'] ?? ''));
$toDate = trim((string) ($_GET['to'] ?? ''));

// Default to the last 7 days when the visitor hasn't chosen a range or explicit dates.
if ($selectedRange === '' && $fromDate === '' && $toDate === '') {
    $selectedRange = '7d';
}

if ($selectedRange !== '') {
    $rangeDays = ['7d' => 7, '14d' => 14, '30d' => 30];
    $fromDate = (new DateTimeImmutable('now', new DateTimeZone('UTC')))
        ->modify('-' . $rangeDays[$selectedRange] . ' days')
        ->format('Y-m-d');
    $toDate = '';
}

$selectedFuel = trim((string) ($_GET['fuel'] ?? 'all'));
$selectedCity = trim((string) ($_GET['city'] ?? ''));
$selectedRadiusKmRaw = trim((string) ($_GET['radius_km'] ?? ''));
$validFuels = ['all', 'diesel', 'e5', 'e10'];
$validRadiusOptions = [5, 10, 20];
if (!in_array($selectedFuel, $validFuels, true)) {
    $selectedFuel = 'all';
}
$selectedRadiusKm = in_array((int) $selectedRadiusKmRaw, $validRadiusOptions, true)
    ? (int) $selectedRadiusKmRaw
    : ($selectedCity !== '' ? 5 : $validRadiusOptions[0]);

if ($dbDriver === 'sqlite' && !file_exists($dbPath)) {
    $errors[] = [
        'key' => 'dbNotFound',
        'params' => ['path' => $dbPath],
        'message' => sprintf('SQLite database not found at %s', $dbPath),
    ];
}

// ── AJAX: city prefix search ──────────────────────────────────────────────────
if (isset($_GET['action']) && $_GET['action'] === 'city_search') {
    header('Content-Type: application/json; charset=utf-8');
    $q = trim((string) ($_GET['q'] ?? ''));
    if (strlen($q) < 3 || ($dbDriver === 'sqlite' && !file_exists($dbPath))) {
        echo '[]';
        exit;
    }
    try {
        $searchPdo = gasolineConnect($dbDriver, $dbPath);
        $searchStmt = $searchPdo->prepare(
            "SELECT normalized_name AS city_key, display_name
             FROM cities
             WHERE LOWER(normalized_name) LIKE :prefix
             ORDER BY normalized_name ASC
             LIMIT 20"
        );
        $searchStmt->bindValue(':prefix', strtolower($q) . '%');
        $searchStmt->execute();
        echo json_encode($searchStmt->fetchAll(), JSON_UNESCAPED_UNICODE | JSON_THROW_ON_ERROR);
    } catch (Throwable $e) {
        echo '[]';
    }
    exit;
}

$selectedCityRow = null;

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
 * Resolve the selected city to its cities-table row, or null when no city is
 * selected or the key is unknown. Callers distinguish the two by also checking
 * whether $selectedCity was non-empty.
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
 * Load the stations in scope for the filter sidebar and data endpoint.
 * With a city: the stations inside the radius (bbox pre-filter + haversine),
 * sorted by distance. Without a city: every station, sorted by name.
 *
 * @return array{0: array<int, array<string, mixed>>, 1: array<string, float>}
 *   [$stations, $distancesById]
 */
function loadScopeStations(PDO $pdo, ?array $cityRow, int $radiusKm): array
{
    if ($cityRow === null) {
        $stations = $pdo->query(
            <<<'SQL'
            SELECT
                s.id,
                COALESCE(s.name_override, s.name) AS name,
                COALESCE(NULLIF(TRIM(s.brand), ''), '') AS brand,
                TRIM(COALESCE(s.street, '')) AS street,
                TRIM(COALESCE(s.house_number, '')) AS house_number,
                TRIM(COALESCE(s.place, '')) AS place,
                s.last_seen_at
            FROM stations s
            ORDER BY COALESCE(s.name_override, s.name) ASC, s.id ASC
            SQL
        )->fetchAll();

        return [$stations, []];
    }

    $bbox = boundingBox((float) $cityRow['lat'], (float) $cityRow['lng'], $radiusKm);
    $stmt = $pdo->prepare(
        <<<'SQL'
        SELECT
            s.id,
            COALESCE(s.name_override, s.name) AS name,
            COALESCE(NULLIF(TRIM(s.brand), ''), '') AS brand,
            TRIM(COALESCE(s.street, '')) AS street,
            TRIM(COALESCE(s.house_number, '')) AS house_number,
            TRIM(COALESCE(s.place, '')) AS place,
            s.last_seen_at,
            s.lat,
            s.lng
        FROM stations s
        WHERE s.lat BETWEEN :min_lat AND :max_lat
          AND s.lng BETWEEN :min_lng AND :max_lng
        SQL
    );
    foreach ($bbox as $key => $value) {
        $stmt->bindValue(':' . $key, $value);
    }
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
 * Assemble the price-snapshot query for the active filters.
 * Station metadata is intentionally NOT joined in — the client joins rows to the
 * separately-sent station map — so the row payload stays small.
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

    $sql = <<<'SQL'
        SELECT
            ps.station_id,
            ps.recorded_at,
            ps.is_open,
            ps.e5,
            ps.e10,
            ps.diesel
        FROM price_snapshots ps
        SQL;

    if ($where !== []) {
        $sql .= "\nWHERE " . implode("\n  AND ", $where);
    }

    $sql .= "\nORDER BY ps.recorded_at ASC, ps.station_id ASC";

    return [$sql, $params, $shouldRun, $errors];
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
        'errors' => $errors,
    ];

    if ($errors !== []) {
        echo json_encode($out, $jsonFlags);
        exit;
    }

    try {
        $pdo = gasolineConnect($dbDriver, $dbPath);
        $cityRow = resolveCity($pdo, $selectedCity);
        if ($selectedCity !== '' && $cityRow === null) {
            $out['errors'][] = ['key' => 'cityNotFound', 'params' => [], 'message' => 'Selected city not found.'];
            echo json_encode($out, $jsonFlags);
            exit;
        }

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
                'dist' => isset($distances[$id]) ? round($distances[$id], 3) : null,
            ];
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

        // Shell only resolves the filter scope (city + station list) so the
        // sidebar renders immediately. The heavy snapshot payload is fetched
        // asynchronously via ?action=data.
        $selectedCityRow = resolveCity($pdo, $selectedCity);
        if ($selectedCity !== '' && $selectedCityRow === null) {
            $errors[] = [
                'key' => 'cityNotFound',
                'params' => [],
                'message' => 'Selected city not found.',
            ];
        } else {
            // Distances are only needed client-side (sent via ?action=data), so
            // the shell just needs the in-scope station list for the dropdown.
            [$stations] = loadScopeStations($pdo, $selectedCityRow, $selectedRadiusKm);
        }
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

function stationLabel(array $station): string
{
    $name = trim($station['name']);

    $place = trim($station['place'] ?? '');

    $dist = '';
    $selectedDistKm = $station['selected_dist_km'] ?? null;
    if ($selectedDistKm !== null) {
        $dist = number_format((float) $selectedDistKm, 1) . ' km';
    }

    $suffix = implode(' ', array_filter([$place, $dist !== '' ? "({$dist})" : '']));

    return $suffix !== '' ? "{$name}, {$suffix}" : $name;
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
            gap: 1rem;
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

        .brand-icon img { width: 54px; height: 54px; object-fit: contain; display: block; }

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
            background: linear-gradient(180deg, var(--ink) 55%, var(--muted) 145%);
            -webkit-background-clip: text;
            background-clip: text;
            -webkit-text-fill-color: transparent;
            color: var(--ink);
            position: relative;
            isolation: isolate;
        }

        /* Outline drawn by a duplicate text layer BEHIND the wordmark: only
           the stroke's outer rim shows, so it can't collide with the
           gradient fill the way text-stroke + background-clip:text does. */
        h1::after {
            content: attr(data-text);
            content: attr(data-text) / "";
            position: absolute;
            inset: 0;
            z-index: -1;
            background: none;
            -webkit-text-fill-color: transparent;
            -webkit-text-stroke: 0.08em var(--ink);
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

        /* ── City autocomplete ─────────────────────────────────────── */
        .city-ac { position: relative; }

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
            .layout { grid-template-columns: 1fr; }
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
            .header { flex-direction: column; align-items: flex-start; }
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
        .table-form { display: inline-block; margin: 0 0.15rem 0 0; }
        .actions-cell { white-space: nowrap; }
        .table-scroll { overflow-x: auto; }
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
        }
        html[data-theme="light"] .menu-panel { box-shadow: 0 14px 36px rgba(0, 0, 0, 0.15); }
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
            <h1 data-text="gasoline">gas<em><span>o</span></em>line</h1>
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
                <a class="menu-item<?= $activePage === 'admin_settings' ? ' active' : '' ?>" href="?page=admin_settings" data-i18n="menuSettings">Settings</a>
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
        loading: 'Loading…',
        showMore: 'Show more',
        showingRows: 'Showing {shown} of {total}',
        loadError: 'Could not load data. Please retry.',
        retry: 'Retry',
        cityNotFound: 'Selected city not found.',
        invalidFromDate: 'Invalid from date.',
        invalidToDate: 'Invalid to date.',
        noSnapshots: 'No snapshots match the current filters.',
        cheapestNow: 'Cheapest — current',
        cheapestNoData: 'No price data available.',
        cheapestPrefix: 'Cheapest',
        cheapestRangeNoData: 'No price data available.',
        highestPrefix: 'Highest',
        highestNoData: 'No price data available.',
        rangeAll: 'All',
        range30d: '30d',
        range14d: '14d',
        range7d: '7d',
        rangeToday: 'Today',
        toggleTheme: 'Toggle theme',
        chartAriaLabel: 'Fuel price history chart',
        brandAriaLabel: 'Gasoline — Dashboard',
        openMenu: 'Open menu',
        menuDashboard: 'Dashboard',
        menuAccount: 'My Account',
        menuAdminSection: 'Admin',
        menuUsers: 'Users',
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
        notifyCheckEnabled: 'Send buy-now alerts when prices drop',
        notifySaved: 'Notification settings saved.',
        invalidNotifySettings: 'Invalid notification settings. Check days, time windows, and times.',
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
        updateTargetsHint: 'These cities are updated automatically by gasoline update (and used by suggest/check/notify) when the CLI is invoked without --city/--radius flags.',
        targetCity: 'City',
        targetRadius: 'Radius (km)',
        addTarget: 'Add',
        removeTarget: 'Remove',
        noTargets: 'No update targets configured yet.',
        targetAdded: 'Update target added.',
        targetRemoved: 'Update target removed.',
        invalidTarget: 'Invalid city or radius (1-25 km).',
        targetExists: 'This city is already an update target.',
        suggestionSettings: 'Suggestions & checks',
        settingFuel: 'Fuel',
        settingRangeKm: 'Range (km)',
        settingHistoryDays: 'History days',
        settingPredictDays: 'Prediction days',
        settingLimitPerDay: 'Suggestions per day',
        settingCheckLimit: 'Check row limit',
        settingSuggestTimes: 'Default suggestion times',
        settingCheckResetTime: 'Check baseline reset',
        settingNotifyWindows: 'Default notification windows',
        settingNotifyDays: 'Default notification days',
        templateCheck: 'Buy-alert notification template',
        templateSuggest: 'Suggestion notification template',
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
    },
    de: {
        title: 'Preisverlauf',
        filters: 'Filter',
        city: 'Stadt',
        enterCity: 'Stadt eingeben...',
        allCities: '— alle Städte —',
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
        loading: 'Wird geladen…',
        showMore: 'Mehr anzeigen',
        showingRows: 'Zeige {shown} von {total}',
        loadError: 'Daten konnten nicht geladen werden. Bitte erneut versuchen.',
        retry: 'Erneut versuchen',
        cityNotFound: 'Ausgewählte Stadt nicht gefunden.',
        invalidFromDate: 'Ungültiges Von-Datum.',
        invalidToDate: 'Ungültiges Bis-Datum.',
        noSnapshots: 'Keine Einträge für die aktuellen Filter.',
        cheapestNow: 'Günstigster Preis — aktuell',
        cheapestNoData: 'Keine Preisdaten vorhanden.',
        cheapestPrefix: 'Günstigster Preis',
        cheapestRangeNoData: 'Keine Preisdaten vorhanden.',
        highestPrefix: 'Höchster Preis',
        highestNoData: 'Keine Preisdaten vorhanden.',
        rangeAll: 'Alle',
        range30d: '30d',
        range14d: '14d',
        range7d: '7d',
        rangeToday: 'Heute',
        toggleTheme: 'Design wechseln',
        chartAriaLabel: 'Kraftstoffpreis-Verlaufsdiagramm',
        brandAriaLabel: 'Gasoline — Dashboard',
        openMenu: 'Menü öffnen',
        menuDashboard: 'Dashboard',
        menuAccount: 'Mein Konto',
        menuAdminSection: 'Admin',
        menuUsers: 'Benutzer',
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
        notifyCheckEnabled: 'Kaufalarme bei Preistiefs senden',
        notifySaved: 'Benachrichtigungseinstellungen gespeichert.',
        invalidNotifySettings: 'Ungültige Benachrichtigungseinstellungen. Bitte Tage, Zeitfenster und Zeiten prüfen.',
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
        updateTargetsHint: 'Diese Städte werden von gasoline update automatisch aktualisiert (und von suggest/check/notify genutzt), wenn die CLI ohne --city/--radius aufgerufen wird.',
        targetCity: 'Stadt',
        targetRadius: 'Radius (km)',
        addTarget: 'Hinzufügen',
        removeTarget: 'Entfernen',
        noTargets: 'Noch keine Update-Ziele konfiguriert.',
        targetAdded: 'Update-Ziel hinzugefügt.',
        targetRemoved: 'Update-Ziel entfernt.',
        invalidTarget: 'Ungültige Stadt oder ungültiger Radius (1-25 km).',
        targetExists: 'Diese Stadt ist bereits ein Update-Ziel.',
        suggestionSettings: 'Vorschläge & Preisprüfungen',
        settingFuel: 'Kraftstoff',
        settingRangeKm: 'Umkreis (km)',
        settingHistoryDays: 'Historie (Tage)',
        settingPredictDays: 'Vorhersage (Tage)',
        settingLimitPerDay: 'Vorschläge pro Tag',
        settingCheckLimit: 'Zeilenlimit der Preisprüfung',
        settingSuggestTimes: 'Standard-Vorschlagszeiten',
        settingCheckResetTime: 'Preis-Baseline zurücksetzen um',
        settingNotifyWindows: 'Standard-Zeitfenster',
        settingNotifyDays: 'Standard-Wochentage',
        templateCheck: 'Vorlage für Kaufalarme',
        templateSuggest: 'Vorlage für Vorschläge',
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

            <form method="get">
                <div class="field">
                    <label for="f-city" data-i18n="city">City</label>
                    <div class="city-ac" id="city-ac">
                        <input
                            type="text"
                            id="f-city"
                            class="city-ac-input"
                            data-i18n-placeholder="enterCity"
                            placeholder="Enter city..."
                            autocomplete="off"
                            spellcheck="false"
                            value="<?= h($selectedCityRow ? (string) $selectedCityRow['display_name'] : '') ?>"
                            aria-autocomplete="list"
                            aria-controls="city-ac-list"
                            aria-expanded="false"
                        >
                        <input type="hidden" name="city" id="f-city-value" value="<?= h($selectedCity) ?>">
                        <ul class="city-ac-list" id="city-ac-list" role="listbox" hidden></ul>
                    </div>
                </div>

                <div class="field">
                    <label for="f-radius" data-i18n="radius">Radius</label>
                    <select
                        name="radius_km"
                        id="f-radius"
                        onchange="this.form.submit()"
                        <?= $selectedCity === '' ? 'disabled' : '' ?>
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
                <a class="btn-reset" href="<?= h((strtok($_SERVER['REQUEST_URI'] ?? '/web/index.php', '?') ?: '/web/index.php') . '?reset=1') ?>" data-i18n="reset">Reset</a>
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

            <!-- Cheapest now -->
            <div class="cheapest-card" id="cheapest-card"><div class="cheapest-empty" role="status"><span class="spinner" aria-hidden="true"></span><span class="sr-only" data-i18n="loading">Loading…</span></div></div>

            <!-- Cheapest in selected range -->
            <div class="cheapest-card" id="cheapest-range-card"><div class="cheapest-empty" role="status"><span class="spinner" aria-hidden="true"></span><span class="sr-only" data-i18n="loading">Loading…</span></div></div>

            <!-- Highest in selected range -->
            <div class="cheapest-card" id="highest-card"><div class="cheapest-empty" role="status"><span class="spinner" aria-hidden="true"></span><span class="sr-only" data-i18n="loading">Loading…</span></div></div>

            <!-- Chart -->
            <div class="chart-card">
                <div class="chart-header">
                    <span class="chart-title" data-i18n="priceTimeline">Price timeline</span>
                    <div class="range-toggles">
                        <button type="button" class="range-toggle active" data-range="all"   data-i18n="rangeAll">All</button>
                        <button type="button" class="range-toggle"        data-range="30d"   data-i18n="range30d">30d</button>
                        <button type="button" class="range-toggle"        data-range="14d"   data-i18n="range14d">14d</button>
                        <button type="button" class="range-toggle"        data-range="7d"    data-i18n="range7d">7d</button>
                        <button type="button" class="range-toggle"        data-range="today" data-i18n="rangeToday">Today</button>
                    </div>
                    <div class="fuel-toggles">
                        <button type="button" class="fuel-toggle active" data-fuel="e5">E5</button>
                        <button type="button" class="fuel-toggle active" data-fuel="e10">E10</button>
                        <button type="button" class="fuel-toggle active" data-fuel="diesel" data-i18n="fuelDiesel">Diesel</button>
                    </div>
                </div>
                <div class="chart-body" id="chart-body">
                    <div class="chart-loading" id="chart-loading" role="status"><span class="spinner" aria-hidden="true"></span><span class="sr-only" data-i18n="loading">Loading…</span></div>
                    <svg id="chart" viewBox="0 0 960 380" preserveAspectRatio="none" aria-label="Fuel price history chart" data-i18n-aria-label="chartAriaLabel" hidden></svg>
                </div>
                <div class="chart-legend" id="legend" hidden></div>
                <div class="chart-empty" id="chart-empty" data-i18n="noSnapshots" role="status" hidden>No snapshots match the current filters.</div>
                <div class="chart-retry" id="chart-retry" hidden>
                    <button type="button" class="btn-reset" id="retry-btn" data-i18n="retry">Retry</button>
                </div>
            </div>

            <!-- Stats -->
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
                            <th data-i18n="fuelDiesel">Diesel</th>
                        </tr>
                        </thead>
                        <tbody id="snapshot-tbody">
                            <tr><td colspan="9" class="table-loading" role="status" aria-busy="true"><span class="spinner" aria-hidden="true"></span><span class="sr-only" data-i18n="loading">Loading…</span></td></tr>
                        </tbody>
                    </table>
                </div>
                <div class="table-more" id="table-more" hidden>
                    <button type="button" class="btn-reset" id="table-more-btn"></button>
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
let dataLoaded = false;

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
const hasDataScope = <?= json_encode($selectedCity !== '' || $selectedStationIds !== [] || $errors !== [], JSON_THROW_ON_ERROR) ?>;

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
        `<div class="tt-meta">${h(row.station_name)}</div>` +
        `<div class="tt-meta">${h(formatDateTime(row.recorded_at))}</div>`;
    tooltip.style.display = 'block';
    positionTooltip(e);
}

let _activeDot = null;
function hideTooltip() {
    tooltip.style.display = 'none';
    if (_activeDot) { _activeDot.setAttribute('opacity', 0); _activeDot = null; }
}

document.addEventListener('touchend', hideTooltip);

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
        const days = chartRange === '30d' ? 30 : chartRange === '14d' ? 14 : 7;
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

    toggles.forEach((toggle) => {
        const fuel = toggle.dataset.fuel;
        toggle.classList.toggle('active', activeFuels.has(fuel));
        toggle.disabled = selectedFuel !== 'all' && fuel !== selectedFuel;
        if (toggle.disabled) toggle.classList.remove('active');
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

    function renderChart() {
        chartEl.innerHTML = '';
        legendEl.innerHTML = '';

        const rangeData = getRangeFilteredData();
        if (rangeData.length === 0) { setChartVisibility(true); return; }

        const visibleRows = rangeData.filter((row) => [...activeFuels].some((f) => row[f] !== null));
        if (visibleRows.length === 0) { setChartVisibility(true); return; }

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
            const xp = px(row._ts);
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

        // Dots on top — per-station colour, per-fuel tint
        for (const fuel of activeFuels) {
            const cfg = fuelConfig[fuel];
            for (const stationRows of byStation.values()) {
                const series = stationRows.filter((r) => r[fuel] !== null);
                for (const row of series) {
                    if (row._synthetic) continue;
                    const xp = px(row._ts);
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
        for (const stationRows of byStation.values()) {
            const sample = stationRows[0];
            const item = document.createElement('div');
            item.className = 'legend-item';
            const swatches = [...activeFuels].map((fuel) => {
                const color = stationFuelColor(sample.station_name, fuel);
                const label = fuelConfig[fuel].label;
                return `<span class="legend-dot" title="${label}" style="background:${color}"></span>`;
            }).join('');
            item.innerHTML = `${swatches}${h(sample.station_name)}`;
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

    // Initial render is triggered by loadData() once the async payload arrives.
}

/* ── i18n ── (translations + applyLang live in the shared script) ── */

/* ── Price cards (cheapest / highest) ──────────────────────────── */
const ICON_DOWN = `<svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" style="color:var(--amber);flex-shrink:0"><circle cx="12" cy="12" r="10"/><polyline points="8 12 12 16 16 12"/><line x1="12" y1="8" x2="12" y2="16"/></svg>`;
const ICON_UP   = `<svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" style="color:var(--amber);flex-shrink:0"><circle cx="12" cy="12" r="10"/><polyline points="8 12 12 8 16 12"/><line x1="12" y1="16" x2="12" y2="8"/></svg>`;

const cheapestCard      = document.getElementById('cheapest-card');
const cheapestRangeCard = document.getElementById('cheapest-range-card');
const highestCard       = document.getElementById('highest-card');

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

    el.innerHTML =
        `<div class="cheapest-header">${icon}<span class="cheapest-title">${title}</span></div>` +
        (results.length === 0
            ? `<div class="cheapest-empty">${emptyMsg}</div>`
            : `<div class="cheapest-grid${colClass ? ' ' + colClass : ''}">` +
                results.map(({ fuel, price, station_id, station, street, place, recorded_at }) => {
                    const addressParts = [street, place].filter(Boolean);
                    const selectedDistKm = stationDistancesById[station_id] ?? null;
                    if (selectedDistKm !== null) {
                        addressParts.push(`${selectedDistKm.toFixed(1)} km`);
                    }
                    const address = addressParts.length ? addressParts.join(', ') : '';
                    return `<div class="cheapest-cell">` +
                        `<div class="cheapest-fuel-label" style="color:${fuelColors[fuel]}">${fuelConfig[fuel].label}</div>` +
                        `<div class="cheapest-price" style="color:${fuelColors[fuel]}">${price.toFixed(3)} <span style="font-size:1rem;opacity:0.7">€</span></div>` +
                        `<div class="cheapest-station"><span class="legend-dot" style="background:${stationFuelColor(station, fuel)};display:inline-block;flex-shrink:0;margin-right:0.4rem"></span>${h(station)}</div>` +
                        (address ? `<div class="cheapest-station" style="opacity:0.6">${h(address)}</div>` : '') +
                        `<div class="cheapest-time">${h(formatDateTime(recorded_at))}</div>` +
                    `</div>`;
                }).join('') +
              `</div>`
        );
}

function rangeTitle(prefix) {
    const t = translations[currentLang];
    const rangeKey = 'range' + chartRange.charAt(0).toUpperCase() + chartRange.slice(1);
    return `${prefix} — ${t[rangeKey]}`;
}

function latestRows() {
    const byStation = new Map();
    for (const row of chartData) {
        const prev = byStation.get(row.station_id);
        if (!prev || row._ts > prev._ts) byStation.set(row.station_id, row);
    }
    return [...byStation.values()];
}

function renderCheapest() {
    const t = translations[currentLang];
    renderPriceCard(cheapestCard, latestRows(), t.cheapestNow, (a, b) => a < b, ICON_DOWN, t.cheapestNoData);
}

function renderCheapestRange() {
    const t = translations[currentLang];
    renderPriceCard(cheapestRangeCard, getRangeFilteredData(), rangeTitle(t.cheapestPrefix), (a, b) => a < b, ICON_DOWN, t.cheapestRangeNoData);
}

function renderHighest() {
    const t = translations[currentLang];
    renderPriceCard(highestCard, getRangeFilteredData(), rangeTitle(t.highestPrefix), (a, b) => a > b, ICON_UP, t.highestNoData);
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

/* ── City autocomplete ─────────────────────────────────────────── */
(function () {
    const wrap   = document.getElementById('city-ac');
    const input  = document.getElementById('f-city');
    const hidden = document.getElementById('f-city-value');
    const list   = document.getElementById('city-ac-list');
    const form   = input?.closest('form');
    const radius = document.getElementById('f-radius');

    if (!wrap || !input || !hidden || !list || !form) return;

    let controller = null;
    let activeIdx  = -1;

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

    function selectCity(cityKey, displayName) {
        input.value  = displayName;
        hidden.value = cityKey;
        hideList();
        if (radius) radius.disabled = (cityKey === '');
        form.submit();
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

    let debounceTimer = null;

    input.addEventListener('input', () => {
        const q = input.value.trim();
        hidden.value = '';
        if (radius) radius.disabled = true;
        clearTimeout(debounceTimer);
        if (q.length < 3) { hideList(); return; }

        debounceTimer = setTimeout(async () => {
            const results = await fetchMatches(q);
            if (results === null) return;

            list.innerHTML = '';
            if (results.length === 0) {
                const empty = document.createElement('li');
                empty.className = 'city-ac-empty';
                empty.textContent = '— no matches —';
                list.appendChild(empty);
            } else {
                results.forEach(({ city_key, display_name }) => {
                    const li = document.createElement('li');
                    li.className    = 'city-ac-item';
                    li.role         = 'option';
                    li.setAttribute('aria-selected', 'false');
                    li.textContent  = display_name || city_key;
                    li.addEventListener('mousedown', (e) => {
                        e.preventDefault();
                        selectCity(city_key, display_name || city_key);
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

    // Submit with empty city when user clears the field and blurs
    input.addEventListener('change', () => {
        if (input.value.trim() === '' && hidden.value === '') form.submit();
    });

    document.addEventListener('click', (e) => {
        if (!wrap.contains(e.target)) hideList();
    });
})();

/* ── Async snapshot data ───────────────────────────────────────── */
// The shell paints immediately; the (potentially huge) snapshot payload is
// fetched here and rendered client-side, so first paint is never blocked on it.
const tbodyEl       = document.getElementById('snapshot-tbody');
const tableMoreWrap = document.getElementById('table-more');
const tableMoreBtn  = document.getElementById('table-more-btn');
const TABLE_PAGE    = 1000; // rows rendered per chunk — nobody scrolls 100k <tr>

let tableRendered = 0;

function fmtPrice(v) { return (v === null || v === undefined) ? '-' : Number(v).toFixed(3); }

function tableRowHtml(row) {
    const t = translations[currentLang];
    const dist = stationDistancesById[row.station_id];
    const distSuffix = (dist !== undefined && dist !== null) ? ` (${dist.toFixed(1)} km)` : '';
    const openClass = row.is_open ? 'open-yes' : 'open-no';
    const openKey   = row.is_open ? 'openYes' : 'openNo';
    return '<tr>' +
        `<td class="td-muted" data-recorded-at="${h(row.recorded_at)}">${h(formatDateTime(row.recorded_at))}</td>` +
        `<td>${h(row.station_name + distSuffix)}</td>` +
        `<td class="td-muted">${h(row.brand)}</td>` +
        `<td class="td-muted">${h(row.street)}</td>` +
        `<td class="td-muted">${h(row.place)}</td>` +
        `<td class="${openClass}" data-i18n="${openKey}">${h(t[openKey])}</td>` +
        `<td class="price-e5">${fmtPrice(row.e5)}</td>` +
        `<td class="price-e10">${fmtPrice(row.e10)}</td>` +
        `<td class="price-diesel">${fmtPrice(row.diesel)}</td>` +
    '</tr>';
}

function updateShowMore() {
    if (!tableMoreWrap || !tableMoreBtn) return;
    const remaining = chartData.length - tableRendered;
    if (remaining <= 0) { tableMoreWrap.hidden = true; return; }
    const t = translations[currentLang];
    tableMoreBtn.textContent = `${t.showMore} (${remaining})`;
    tableMoreWrap.hidden = false;
}

function renderMoreRows() {
    if (!tbodyEl) return;
    // Newest-first without copying the whole dataset: take a page-sized window
    // from the end of chartData (sorted oldest→newest) and reverse only that.
    const upper = chartData.length - tableRendered;
    const lower = Math.max(0, upper - TABLE_PAGE);
    const slice = chartData.slice(lower, upper).reverse();
    tbodyEl.insertAdjacentHTML('beforeend', slice.map(tableRowHtml).join(''));
    tableRendered += slice.length;
    updateShowMore();
}

function renderTable() {
    if (!tbodyEl) return;
    tableRendered = 0;
    tbodyEl.innerHTML = '';
    if (chartData.length === 0) {
        const t = translations[currentLang];
        tbodyEl.innerHTML = `<tr><td colspan="9" style="text-align:center;color:var(--muted);padding:2rem;font-family:var(--mono);font-size:.82rem">${h(t.noData)}</td></tr>`;
        updateShowMore();
        return;
    }
    renderMoreRows();
}

if (tableMoreBtn) tableMoreBtn.addEventListener('click', renderMoreRows);

function setStat(id, value) {
    const el = document.getElementById(id);
    if (!el) return;
    el.textContent = String(value);
    el.classList.remove('skeleton');
    el.removeAttribute('aria-busy');
}

function applyData(payload) {
    if (retryWrap) retryWrap.hidden = true;
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
    _stationHues = computeStationHues();
    dataLoaded = true;

    const sum = payload.summary || {};
    setStat('stat-points',   sum.points   ?? 0);
    setStat('stat-stations', sum.stations ?? 0);
    setStat('stat-first', sum.first_recorded_at ? String(sum.first_recorded_at).slice(0, 10) : '—');
    setStat('stat-last',  sum.last_recorded_at  ? String(sum.last_recorded_at).slice(0, 10)  : '—');

    renderCheapest();
    renderCheapestRange();
    renderHighest();
    renderTable();
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
    if (tableMoreWrap) tableMoreWrap.hidden = true;
    if (tbodyEl) {
        tbodyEl.innerHTML = `<tr><td colspan="9" role="alert" style="text-align:center;color:var(--red);padding:2rem;font-family:var(--mono);font-size:.82rem" data-i18n="${key}">${h(msg)}</td></tr>`;
    }
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
    if (tbodyEl) tbodyEl.innerHTML = '<tr><td colspan="9" class="table-loading" aria-busy="true"><span class="spinner" aria-hidden="true"></span></td></tr>';
}

async function loadData() {
    const url = new URL(location.href);
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
        renderCheapestRange();
        renderHighest();
        if (chartEl) renderChart();
        updateShowMore(); // re-translate the "Show more (N)" button label
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
