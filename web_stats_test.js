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
    checkTrue('the failure reason is shown under the attempt that hit it',
        html.includes('tankerkönig request failed: 503 Service Unavailable'));
    // The reason goes on a row of its own beneath the attempt rather than into
    // its status cell: it is a sentence, and the columns around it are numbers.
    check('and on a row of its own', (html.match(/cs-tile-why/g) || []).length, 1);
    // It spans the rest of the row and no further: a colspan wider than the
    // header adds a phantom column to the whole table.
    checkTrue('spanning exactly the columns it has left',
        html.includes('<td class="cs-tile-why" colspan="6">'));

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
    // And the reason row widens with the header rather than staying behind it.
    const twoCityFail = build('en', okCache([
        req(0, 0, 1, 'failed'),
        req(1, 0, 1, 'ok', { city: 'Uchte' }),
    ])).tilesPanelHtml(7);
    checkTrue('a reason row follows the city column in',
        twoCityFail.includes('<td class="cs-tile-why" colspan="7">'));
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
    // The controls the phone does not get are hidden by a class, and .field
    // sets display too — later in the document, so an equal specificity there
    // would leave them on screen.
    checkTrue('and the desktop-only controls stay hidden',
        beats('.cs-layout .cs-wide-only', '.field', '.field {'));
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
