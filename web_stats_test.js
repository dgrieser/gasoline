/**
 * Exercises the Statistics page's per-request drill-down: the toggle that says
 * how many requests a run made and how many it had to retry, and the panel
 * behind it that lists them one by one.
 *
 * web/index.php is a monolith whose client script only runs in a browser, so
 * the renderers are lifted out of it by name — the same trick web_chart_test.js
 * and web_picker_test.php use. That is worth the machinery here because the
 * panel is the only place a retried tile is visible at all: the run's own
 * duration and status look identical whether a sweep answered on the first
 * request or on the second.
 *
 * The lifted functions' free variables are passed in explicitly, so a new
 * dependency inside them shows up here as a "not defined" failure rather than
 * as a silently untested branch. The page's own translations are lifted too, so
 * a label asserted here is the label it ships.
 *
 * Run directly (`node web_stats_test.js`) or via `make test`.
 */
'use strict';

const fs = require('fs');
const path = require('path');

/* ── Reporting (mirrors web_picker_test.php) ─────────────────────── */

let failures = 0;
function check(name, got, want) {
    if (JSON.stringify(got) === JSON.stringify(want)) {
        console.log(`  ok   ${name}`);
        return;
    }
    console.log(`  FAIL ${name}\n       got  ${JSON.stringify(got)}\n       want ${JSON.stringify(want)}`);
    failures++;
}

function checkTrue(name, got) {
    check(name, got === true, true);
}

/* ── Lifting the drill-down's own code out of the viewer ────────── */

const viewer = fs.readFileSync(path.join(__dirname, 'web', 'index.php'), 'utf8');

/** One declaration from the viewer's client script, delimited by brace depth. */
function lift(anchor) {
    const start = viewer.indexOf(anchor);
    if (start === -1) {
        console.error(`web/index.php no longer declares ${anchor.trim()}`);
        process.exit(2);
    }
    let depth = 0;
    for (let i = viewer.indexOf('{', start); i < viewer.length; i++) {
        if (viewer[i] === '{') depth++;
        else if (viewer[i] === '}' && --depth === 0) {
            const src = viewer.slice(start, i + 1);
            if (src.includes('<?')) {
                console.error(`${anchor.trim()} now interpolates PHP and cannot be lifted`);
                process.exit(2);
            }
            return src;
        }
    }
    console.error(`cannot delimit ${anchor.trim()}`);
    process.exit(2);
}

const translations = new Function(`${lift('const translations = {')}\nreturn translations;`)();

// The renderers, built against one language and one request cache. The
// formatters are the page's own so a column asserted here is formatted the way
// the reader sees it; only the locale-dependent clock is pinned, because a test
// asserting a machine's timezone would assert nothing about this code.
function build(lang, cache) {
    return new Function('T', 'tileCache', 'tilesOpen', 'esc', 'fmtNumber', 'fmtDuration', 'fmtTimeOfDay', [
        lift('        function tileStatusLabel(s) {'),
        lift('        function tileStatusClass(s) {'),
        lift('        function tileToggleHtml(r) {'),
        lift('        function flattenReason(msg) {'),
        lift('        function tilesPanelHtml(runID) {'),
        lift('        function metricsHtml(metrics) {'),
        'return { tileToggleHtml, tilesPanelHtml, tileStatusClass, tileStatusLabel, metricsHtml };',
    ].join('\n'))(
        () => translations[lang],
        cache,
        new Set(),
        (v) => String(v ?? '').replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;'),
        (v) => (v === null || v === undefined ? '—' : String(v)),
        (ms) => (ms === null || ms === undefined ? '—' : `${ms} ms`),
        (iso) => String(iso).slice(11, 19),
    );
}

/* ── Fixtures ───────────────────────────────────────────────────── */

/** One recorded request. */
function req(seq, tile, attempt, status, extra = {}) {
    return {
        seq,
        city: 'Berlin',
        tile_index: tile,
        attempt,
        sent_at: `2026-08-21T09:0${seq}:00Z`,
        waited_ms: seq === 0 ? 0 : 35000,
        duration_ms: 100 + seq,
        status,
        error: status === 'ok' ? null : 'tankerkönig request failed: 503 Service Unavailable',
        ...extra,
    };
}

/** A sweep of five tiles whose second tile needed a retry: six requests. */
const retriedSweep = [
    req(0, 0, 1, 'ok'),
    req(1, 1, 1, 'retried'),
    req(2, 1, 2, 'ok'),
    req(3, 2, 1, 'ok'),
    req(4, 3, 1, 'ok'),
    req(5, 4, 1, 'ok'),
];

const cacheOf = (entry) => new Map([[7, entry]]);
const okCache = (tiles, extra = {}) =>
    cacheOf({ state: 'ok', tiles, truncated: false, supported: true, ...extra });

/**
 * How many request rows the panel drew. Counted by the duration cell, which
 * only a request row has: the header and the reason row a failed attempt adds
 * beneath itself are both <tr> too.
 */
function rowCount(html) {
    return (html.match(/cs-tile-dur/g) || []).length;
}

/* ── The toggle ─────────────────────────────────────────────────── */

console.log('web_stats_test: tileToggleHtml');
{
    const { tileToggleHtml } = build('en', new Map());

    // A run that made no tiled requests has nothing to drill into, and must not
    // grow a toggle that opens an empty panel.
    check('a run with no request counters gets no toggle',
        tileToggleHtml({ id: 7, metrics: {} }), '');
    check('nor does a run with no metrics at all',
        tileToggleHtml({ id: 7, metrics: null }), '');

    const clean = tileToggleHtml({ id: 7, metrics: { tile_requests: 8, tile_retries: 0 } });
    checkTrue('a run that made requests says how many', clean.includes('8 requests'));
    checkTrue('and does not mention retries it did not make', !clean.includes('retried'));
    checkTrue('the toggle addresses the run it belongs to', clean.includes('data-cs-tiles="7"'));
    checkTrue('and starts collapsed', clean.includes('aria-expanded="false"'));

    // The count of retries is on the toggle rather than behind it: "did this
    // sweep have to retry" is the question the table is scanned for, and
    // expanding every green run to answer it defeats the point.
    const retried = tileToggleHtml({ id: 7, metrics: { tile_requests: 9, tile_retries: 2 } });
    checkTrue('a run that retried says so without being expanded',
        retried.includes('9 requests') && retried.includes('2 retried'));

    const de = build('de', new Map()).tileToggleHtml({ id: 7, metrics: { tile_requests: 9, tile_retries: 2 } });
    checkTrue('and says it in the reader\'s language',
        de.includes('9 Anfragen') && de.includes('2 wiederholt'));
}

/* ── The panel ──────────────────────────────────────────────────── */

console.log('web_stats_test: tilesPanelHtml');
{
    const html = build('en', okCache(retriedSweep)).tilesPanelHtml(7);

    check('every request the sweep made gets a row', rowCount(html), 6);
    // The retried attempt and the attempt that replaced it are both listed,
    // which is what makes the cost of a retry visible: two requests, two
    // pacing windows, one tile.
    checkTrue('the retried attempt is listed as retried', html.includes('>retried<'));
    checkTrue('and the attempt that replaced it as answered', html.includes('>answered<'));
    checkTrue('a retry is flagged amber rather than red',
        html.includes('<td class="cs-partial">2</td>'));
    checkTrue('the failure reason is shown',
        html.includes('tankerkönig request failed: 503 Service Unavailable'));
    // It is listed under the table rather than as a row in it, keyed by the
    // request numbers it belongs to. As a row spanning every column it was a
    // value in no column, and it made the table as wide as its own sentence.
    check('as one entry under the table', (html.match(/data-cs-why/g) || []).length, 1);
    checkTrue('naming the request it belongs to', html.includes('<span class="cs-why-seq">2</span>'));
    checkTrue('and no longer as a row in the table', !html.includes('cs-tile-why'));
    // Outside the scroller, so the panel's width bounds it rather than the
    // table's — which is what lets it be ellipsised to something visible.
    checkTrue('the table scrolls and the reasons do not',
        html.indexOf('cs-tiles-scroll') < html.indexOf('cs-why-list'));

    // The two halves of a request's cost are rendered apart: the API's own
    // latency, and the pacing wait that is the schedule we chose.
    checkTrue('the request duration is rendered as the request\'s own cost',
        html.includes('<td class="cs-tile-dur">100 ms</td>'));
    checkTrue('and the pacing wait apart from it, signed as added time',
        html.includes('<td class="cs-tile-wait">+35000 ms</td>'));
    checkTrue('a request that waited for nothing says so rather than showing a zero',
        html.includes('<td class="cs-tile-wait">—</td>'));

    // Tile 0 is the load-bearing one — if it fails the whole city fails — so it
    // is named, not numbered.
    checkTrue('the centre tile is named', html.includes('>centre<'));
    checkTrue('the seconds of a request are shown', html.includes('09:00:00'));

    // One city needs no city column; several do, or a slow request cannot be
    // attributed to the target that made it.
    checkTrue('a single-city sweep does not spend a column on the city',
        !html.includes('>City<'));
    const twoCities = build('en', okCache([
        req(0, 0, 1, 'ok'),
        req(1, 0, 1, 'ok', { city: 'Uchte' }),
    ])).tilesPanelHtml(7);
    checkTrue('a sweep of several cities names them', twoCities.includes('>City<')
        && twoCities.includes('>Uchte<'));

}

console.log('web_stats_test: tilesPanelHtml edge cases');
{
    const t = translations.en;

    checkTrue('a run in flight shows a spinner rather than an empty panel',
        build('en', cacheOf({ state: 'loading' })).tilesPanelHtml(7).includes('spinner'));
    checkTrue('a run whose requests could not be read says so',
        build('en', cacheOf({ state: 'error' })).tilesPanelHtml(7).includes(t.loadError));
    checkTrue('an expand that has not been answered yet shows a spinner, not an error',
        build('en', new Map()).tilesPanelHtml(7).includes('spinner'));

    // A database written by an older binary has no request log. That is an
    // empty drill-down with an explanation, not a broken page.
    checkTrue('a database without a request log explains itself',
        build('en', okCache([], { supported: false })).tilesPanelHtml(7).includes(t.statsTileUnsupported));
    checkTrue('a run that recorded nothing says that instead',
        build('en', okCache([])).tilesPanelHtml(7).includes(t.statsTileNone));
    checkTrue('a list that was cut says it was cut',
        build('en', okCache(retriedSweep, { truncated: true })).tilesPanelHtml(7).includes(t.statsTileTruncated));

    // The reason the panel escapes: a failure reason comes from the API, and
    // it is the one field on the page that arbitrary upstream text reaches.
    const nasty = build('en', okCache([
        { ...req(0, 0, 1, 'failed'), error: '<script>alert(1)</script>' },
    ])).tilesPanelHtml(7);
    checkTrue('an upstream failure reason cannot inject markup',
        !nasty.includes('<script>') && nasty.includes('&lt;script&gt;'));
}

console.log('web_stats_test: status colours');
{
    const { tileStatusClass } = build('en', new Map());
    check('an answered request reads as a success', tileStatusClass('ok'), 'cs-ok');
    // Amber, not red: the tile may well have succeeded on the next attempt.
    check('a retried one reads as degraded', tileStatusClass('retried'), 'cs-partial');
    check('a failed one reads as a failure', tileStatusClass('failed'), 'cs-error');
    check('and an unknown status is not quietly called a success',
        tileStatusClass('something-new'), 'cs-error');
}

/* ── The counter list beside the toggle ─────────────────────────── */

console.log('web_stats_test: metricsHtml');
{
    const { metricsHtml } = build('en', new Map());

    check('a run with no counters contributes nothing', metricsHtml({}), '');
    check('nor does a missing set', metricsHtml(null), '');

    const html = metricsHtml({ tile_retries: 2, cities: 1, tile_requests: 10 });
    // Each pair is its own box. A single string has no spaces inside a pair to
    // break at, so on a phone it wraps mid-token — which is what put a column
    // one character wide next to the toggle.
    check('each counter is its own box', (html.match(/class="cs-metric"/g) || []).length, 3);
    checkTrue('a pair is not split across boxes', html.includes('>tile_requests=10<'));
    checkTrue('and the counters read in a stable order',
        html.indexOf('cities=1') < html.indexOf('tile_requests=10')
        && html.indexOf('tile_requests=10') < html.indexOf('tile_retries=2'));
    // Nothing separates the boxes in the markup: the spacing is the box's own
    // margin, so a wrapped line does not start with a stray gap.
    checkTrue('the boxes carry their own spacing', !html.includes('</span>  <span'));
}

/* ── The card layout the panel lives inside ─────────────────────── */

console.log('web_stats_test: card layout');
{
    /** The max-width of the media query a rule is declared inside, or null. */
    function breakpointOf(needle) {
        const at = viewer.indexOf(needle);
        if (at === -1) return `missing rule: ${needle}`;
        const opened = [...viewer.slice(0, at).matchAll(/@media \(max-width: (\d+)px\)/g)].pop();
        return opened ? Number(opened[1]) : null;
    }

    // Below this width the page turns every table row into a card — including,
    // because the rule reaches every descendant cell, the request table inside
    // the drill-down. The overrides that put that table back have to start at
    // exactly the same width: higher and they fire while the outer table is
    // still a table, lower and the panel is carded with no columns left.
    const card = breakpointOf('.stack-table thead { display: none; }');
    check('the card layout still has a breakpoint to match', typeof card, 'number');
    check('the drill-down is put back to a table exactly where carding starts',
        breakpointOf('.stack-table.cs-inline .cs-tiles-panel table'), card);
    check('and the toggle is given its own line there too',
        breakpointOf('.stack-table.cs-inline td.cs-wide > .cs-tiles-toggle'), card);
    // The toggle cannot shrink, so the detail cell has to be allowed to wrap:
    // sharing one flex line with it is what left the counters a character wide.
    check('the detail cell may wrap at that width',
        breakpointOf('.stack-table.cs-inline td.cs-wide { flex-wrap: wrap; }'), card);
}

/* ── The panel's rules have to outrank the card rules ───────────── */

console.log('web_stats_test: cascade');
{
    /** CSS specificity as one comparable number. */
    function specificity(selector) {
        const ids = (selector.match(/#[\w-]+/g) || []).length;
        const classes = (selector.match(/\.[\w-]+/g) || []).length;
        const elements = (selector.replace(/[.#][\w-]+/g, ' ').match(/\b[a-z][\w-]*\b/g) || []).length;
        return ids * 10000 + classes * 100 + elements;
    }

    check('the specificity arithmetic is right', [
        specificity('.stack-table td'),
        specificity('.stack-table.cs-inline tr'),
        specificity('.stack-table.cs-inline .cs-tiles-panel tr'),
    ], [101, 201, 301]);

    /**
     * Whether a rule beats another one it shares a declaration with. Equal
     * specificity is decided by document order, which is the trap: the panel's
     * rules are declared before the card rules they have to survive, so a tie
     * silently loses.
     */
    function beats(panelSelector, cardSelector, cardNeedle) {
        const panelAt = viewer.indexOf(panelSelector + ' ');
        const cardAt = viewer.indexOf(cardNeedle || cardSelector + ' ');
        if (panelAt === -1) return `panel rule is gone: ${panelSelector}`;
        if (cardAt === -1) return `card rule is gone: ${cardSelector}`;
        const bySpec = specificity(panelSelector) - specificity(cardSelector);
        return bySpec !== 0 ? bySpec > 0 : panelAt > cardAt;
    }

    // The row is the one that matters most. A flex container blockifies its
    // children, so losing the row alone takes the cells with it: the columns
    // stop lining up and every header sits in a box of its own.
    checkTrue('the panel keeps its rows table rows',
        beats('.stack-table.cs-inline .cs-tiles-panel tr', '.stack-table.cs-inline tr'));
    checkTrue('and its cells table cells',
        beats('.stack-table.cs-inline .cs-tiles-panel td', '.stack-table td'));
    // Winning the display is not enough: the card layout also drops a cell's
    // horizontal padding, so the rule that wins has to restate the gutter or a
    // two-digit row number touches the column beside it.
    const cellRuleAt = viewer.indexOf('.stack-table.cs-inline .cs-tiles-panel th,');
    const cellRule = cellRuleAt === -1 ? '' : viewer.slice(cellRuleAt, viewer.indexOf('}', cellRuleAt));
    checkTrue('and restates the column gutter the card layout drops',
        /padding:[^;]*\d/.test(cellRule));

    // The work table stays a real table at the same width, and has the same
    // fight to win — plus a harder one, because the card rules that size a
    // cell name the cell's own class and so tie at three classes rather than
    // two. That is what the .cs-layout prefix on its rules is buying.
    checkTrue('the work table keeps its rows table rows',
        beats('.cs-layout .stack-table.cs-inline.cs-flat tr', '.stack-table.cs-inline tr'));
    checkTrue('and its cells their padding',
        beats('.cs-layout .stack-table.cs-inline.cs-flat td', '.stack-table.cs-inline td.cs-mini'));
}

/* ── The reasons under the table ────────────────────────────────── */

console.log('web_stats_test: reasons');
{
    // A sweep that meets a failing API meets the same failure on every tile.
    // Five copies of one sentence is five lines of the same thing; one line
    // naming five requests is the same information, read once.
    const many = build('en', okCache([
        req(0, 0, 1, 'ok'),
        req(1, 1, 1, 'retried'),
        req(2, 1, 2, 'ok'),
        req(3, 2, 1, 'retried'),
        req(4, 2, 2, 'ok'),
    ])).tilesPanelHtml(7);
    check('identical reasons are listed once', (many.match(/data-cs-why/g) || []).length, 1);
    checkTrue('naming every request that hit it', many.includes('<span class="cs-why-seq">2, 4</span>'));

    // Two different failures stay two entries, each with its own requests.
    const mixed = build('en', okCache([
        req(0, 0, 1, 'retried', { error: 'first failure' }),
        req(1, 0, 2, 'retried', { error: 'second failure' }),
        req(2, 0, 3, 'retried', { error: 'first failure' }),
    ])).tilesPanelHtml(7);
    check('different reasons stay apart', (mixed.match(/data-cs-why/g) || []).length, 2);
    checkTrue('each keeping its own requests',
        mixed.includes('<span class="cs-why-seq">1, 3</span>')
        && mixed.includes('<span class="cs-why-seq">2</span>'));

    // An API reason can be a whole HTML error page. One line is one line
    // whatever it holds, so newlines and runs of spaces are flattened.
    const messy = build('en', okCache([
        req(0, 0, 1, 'failed', { error: '  503\n\tService   Unavailable\n\nEnable friendly errors.  ' }),
    ])).tilesPanelHtml(7);
    checkTrue('a multi-line reason is flattened to one line',
        messy.includes('>503 Service Unavailable Enable friendly errors.<'));

    // The ellipsis is the browser's, against the width the reader can see. A
    // character budget would truncate a message that fitted a desktop fine.
    checkTrue('nothing truncates the text before the browser does',
        !viewer.includes('REASON_MAX'));
    checkTrue('and a reason starts shut', many.includes('aria-expanded="false"'));
}

/* ── Filtering the per-command table by status ──────────────────── */

console.log('web_stats_test: csFilterCommandRows');
{
    const csFilterCommandRows = new Function([
        lift('        function csFilterCommandRows(rows, status) {'),
        'return csFilterCommandRows;',
    ].join('\n'))();

    // A row here is a command and its tallies, not a run, so a status keeps
    // the commands that have any run of it — "which of these ever failed".
    const rows = [
        { command: 'update', runs: 826, ok: 800, partial: 20, error: 6 },
        { command: 'suggest', runs: 138, ok: 138, partial: 0, error: 0 },
        { command: 'check', runs: 138, ok: 137, partial: 0, error: 1 },
        { command: 'notify', runs: 690, ok: 690, partial: 0, error: 0 },
    ];
    const names = (out) => out.map((r) => r.command);

    check('no status narrows nothing', names(csFilterCommandRows(rows, 'all')),
        ['update', 'suggest', 'check', 'notify']);
    check('nor does a missing one', names(csFilterCommandRows(rows, '')),
        ['update', 'suggest', 'check', 'notify']);
    check('failed keeps the commands that have failures',
        names(csFilterCommandRows(rows, 'error')), ['update', 'check']);
    check('partial keeps the ones that have degraded runs',
        names(csFilterCommandRows(rows, 'partial')), ['update']);
    // Every command here has succeeded at least once, which is the ordinary
    // case and has to read as "all four" rather than as an empty table.
    check('ok keeps everything that has ever succeeded',
        names(csFilterCommandRows(rows, 'ok')), ['update', 'suggest', 'check', 'notify']);
    check('a command with none of the status drops out',
        names(csFilterCommandRows([{ command: 'notify', ok: 690, partial: 0, error: 0 }], 'error')), []);
}

/* ── Sorting from the column header ─────────────────────────────── */

console.log('web_stats_test: setSort');
{
    // The work table has no sort control any more: its header is there at
    // every width, so a column sorts by being tapped and taps after that
    // reverse it. That cycle is the whole interface, so it is worth asserting
    // rather than assuming.
    const setSort = new Function('syncSortControls', 'data', [
        lift('        function setSort(spec, key) {'),
        'return setSort;',
    ].join('\n'))(() => {}, null);

    const spec = {
        cols: [{ key: 'name', dir: 'asc' }, { key: 'total', dir: 'desc' }],
        sort: { key: 'name', dir: 'asc' },
        render: () => { throw new Error('rendered with no data loaded'); },
    };

    setSort(spec, 'total');
    // A column arrives sorted the way it is worth reading first — counts
    // largest, names smallest — rather than inheriting the last column's.
    check('a new column takes its own first direction', spec.sort, { key: 'total', dir: 'desc' });
    setSort(spec, 'total');
    check('tapping it again reverses it', spec.sort, { key: 'total', dir: 'asc' });
    setSort(spec, 'total');
    check('and again', spec.sort, { key: 'total', dir: 'desc' });
    setSort(spec, 'name');
    check('moving to another column does not carry the direction over',
        spec.sort, { key: 'name', dir: 'asc' });
    setSort(spec, 'nope');
    check('a column the table does not have changes nothing',
        spec.sort, { key: 'name', dir: 'asc' });
}

console.log('web_stats_test: controls');
{
    // The select existed because the card layout hides the header. The work
    // table keeps its header now, so the select would be a second way to do
    // one job; the runs table still cards its rows and still needs one.
    checkTrue('the work table has no sort select left', !viewer.includes('cs-metric-sort'));
    checkTrue('nor a direction button', !viewer.includes('cs-metric-dir'));
    checkTrue('the runs table still has both', viewer.includes('cs-run-sort') && viewer.includes('cs-run-dir'));
    // Every table's controls sit behind a disclosure that starts shut, so a
    // card opens as its heading and its table and nothing else.
    const disclosures = viewer.match(/<details class="cs-collapse">/g) || [];
    check('each of the three tables has a disclosure', disclosures.length, 3);
    // All three sections are headed by the same one word. Two of them used to
    // name sorting instead, which made cards of the same kind read as
    // different kinds of thing.
    check('every section shares one heading',
        (viewer.match(/data-i18n="statsFiltersLabel"/g) || []).length, 3);
    checkTrue('and nothing names sorting in a heading any more',
        !viewer.includes('statsControls') && !viewer.includes('statsSorting'));
    check('each table has a reset',
        (viewer.match(/class="btn-small cs-reset"/g) || []).length, 3);
    checkTrue('none of them starts open', !/<details class="cs-collapse"[^>]*\bopen\b/.test(viewer));
    for (const id of ['cs-cmd-sort', 'cs-metric-command', 'cs-run-status']) {
        const at = viewer.indexOf('id="' + id + '"');
        const open = viewer.lastIndexOf('<details class="cs-collapse">', at);
        const close = viewer.lastIndexOf('</details>', at);
        checkTrue(`${id} is inside one`, at !== -1 && open !== -1 && open > close);
    }

    // The resets say what they do through a translated title and aria-label
    // rather than their face: as text the label is the widest control in the
    // row and the only one that cannot shorten.
    for (const id of ['cs-cmd-reset', 'cs-metric-reset', 'cs-run-reset']) {
        const at = viewer.indexOf('id="' + id + '"');
        const tag = viewer.slice(viewer.lastIndexOf('<button', at), viewer.indexOf('</button>', at));
        checkTrue(`${id} is a symbol`, />\s*↺\s*$/.test(tag));
        checkTrue(`${id} keeps its name for assistive tech`,
            tag.includes('data-i18n-aria-label="statsClearFilters"')
            && tag.includes('data-i18n-title="statsClearFilters"'));
        // A lone button in a grid of labelled fields sits a label's height
        // below every control beside it, so it is built like one.
        checkTrue(`${id} is built like the fields it sits among`,
            viewer.slice(viewer.lastIndexOf('<div class="field cs-reset-cell">', at), at).includes('<label aria-hidden="true">'));
    }
}

/* ── Both languages carry every label the panel asks for ────────── */

console.log('web_stats_test: translations');
{
    const keys = Object.keys(translations.en).filter((k) => k.startsWith('statsTile'));
    checkTrue('the panel has labels to ask for', keys.length > 0);
    const missing = keys.filter((k) => translations.de[k] === undefined);
    check('every drill-down label exists in German too', missing, []);
}

if (failures > 0) {
    console.log(`web_stats_test: ${failures} failed`);
    process.exit(1);
}
console.log('web_stats_test: ok');
