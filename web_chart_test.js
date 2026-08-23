/**
 * Exercises the dashboard chart's per-fuel trendlines: the fit itself, the
 * dashed style each fuel is drawn with, the legend entries, and the legend's
 * hide/show toggle.
 *
 * web/index.php is a monolith whose client script only runs in a browser, so
 * renderChart is lifted out of it by name — the same trick web_picker_test.php
 * uses on the PHP side — and evaluated against the DOM stub below. That is
 * worth the machinery: the trend is a claim about where prices are heading,
 * and a fit that silently drifts would be believed.
 *
 * The lifted function's free variables are passed in explicitly by DEPS, so a
 * new dependency inside renderChart shows up here as a "not defined" failure
 * rather than as a silently untested branch. Stubs stand in only for what sits
 * outside the chart's drawing: range filtering, empty-state visibility, the
 * per-station colours, and tick formatting.
 *
 * Run directly (`node web_chart_test.js`) or via `make test`.
 */
'use strict';

const fs = require('fs');
const path = require('path');

const SVG_NS = 'http://www.w3.org/2000/svg';
const HOUR = 3_600_000;
const T0 = Date.parse('2026-08-01T00:00:00Z');

/* ── Reporting (mirrors web_picker_test.php) ─────────────────────── */

let failures = 0;
/** Within one step of the two decimals the legend rounds to. */
function checkClose(name, got, want) {
    if (typeof got === 'number' && Math.abs(got - want) <= 0.02) {
        console.log(`  ok   ${name}`);
        return;
    }
    console.log(`  FAIL ${name}\n       got  ${JSON.stringify(got)}\n       want ~${JSON.stringify(want)}`);
    failures++;
}

function check(name, got, want) {
    if (JSON.stringify(got) === JSON.stringify(want)) {
        console.log(`  ok   ${name}`);
        return;
    }
    console.log(`  FAIL ${name}\n       got  ${JSON.stringify(got)}\n       want ${JSON.stringify(want)}`);
    failures++;
}

/* ── Lifting the chart's own code out of the viewer ─────────────── */

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

// Four-space indentation picks the dashboard's renderChart: the prediction and
// statistics pages declare their own, one level deeper.
const renderChartSource = lift('\n    function renderChart() {');
// The fuel palette and the translation map are plain literals, so the tests
// assert against the dash patterns, colours and wording the page really ships
// rather than against a copy that could drift from it.
const fuelConfig = new Function(`${lift('const fuelConfig = {')}\nreturn fuelConfig;`)();
const translations = new Function(`${lift('const translations = {')}\nreturn translations;`)();
const h = new Function(`${lift('function h(str) {')}\nreturn h;`)();
const makeLoc = new Function('currentLang', `${lift('function _loc() {')}\nreturn _loc;`);

/* ── DOM stub: enough of an element for an SVG chart and a legend ── */

function makeElement(tagName) {
    return {
        tagName,
        attrs: {},
        children: [],
        listeners: {},
        style: {},
        className: '',
        title: '',
        hidden: false,
        textContent: '',
        _html: '',
        setAttribute(k, v) { this.attrs[k] = String(v); },
        getAttribute(k) { return k in this.attrs ? this.attrs[k] : null; },
        hasAttribute(k) { return k in this.attrs; },
        removeAttribute(k) { delete this.attrs[k]; },
        toggleAttribute(k, on) { if (on) this.attrs[k] = ''; else delete this.attrs[k]; },
        appendChild(child) { this.children.push(child); return child; },
        addEventListener(type, fn) { (this.listeners[type] = this.listeners[type] || []).push(fn); },
        // The chart is drawn in CSS pixels off the container's measured width.
        getBoundingClientRect() { return { width: 960, height: 380, left: 0, top: 0 }; },
        // Legend items are built as markup; the tests read it back as text.
        set innerHTML(v) { this._html = String(v); if (v === '') this.children = []; },
        get innerHTML() { return this._html; },
        click() { for (const fn of this.listeners.click || []) fn(); },
    };
}

const documentStub = {
    documentElement: makeElement('html'),
    createElement: (tag) => makeElement(tag),
    createElementNS: (ns, tag) => makeElement(ns === SVG_NS ? tag : tag),
    getElementById: () => null,
};

/* ── Fixtures ───────────────────────────────────────────────────── */

/**
 * One station's snapshot series. `everyHours` is how often it reprices, which
 * is what a row-weighted fit would let outvote a slower station.
 */
function series(stationId, stationName, priceAt, { everyHours = 1, hours = 24, fuels = ['diesel'] } = {}) {
    const rows = [];
    for (let h = 0; h <= hours; h += everyHours) {
        const row = {
            station_id: stationId,
            station_name: stationName,
            recorded_at: new Date(T0 + h * HOUR).toISOString(),
            _ts: T0 + h * HOUR,
            e5: null,
            e10: null,
            diesel: null,
        };
        for (const fuel of fuels) row[fuel] = priceAt(h, fuel);
        rows.push(row);
    }
    return rows;
}

/* ── Render harness ─────────────────────────────────────────────── */

// Every free variable renderChart reads. Anything new inside it has to be
// added here, which is the point: the list is the chart's dependency surface.
const DEPS = [
    'chartEl', 'legendEl', 'document', 'activeFuels', 'hiddenTrends', 'stationFilter',
    'fuelConfig', 'translations', 'currentLang', 'renderChart', 'lastChartWidth',
    'hideCrosshair', 'tooltip', 'getRangeFilteredData', 'setChartVisibility',
    'stationFuelColor', 'fillSvgPrice', 'formatTickDate', 'formatTickTime',
    'positionTooltip', 'hideTooltip', 'attachLongPressCrosshair', 'fmtPriceHtml',
    'h', '_loc',
];
const compiled = new Function(...DEPS, `${renderChartSource}\nreturn renderChart;`);

/**
 * Draw the given rows and hand back the chart, the legend, and the knobs the
 * tests turn. `fuels` are the active fuel toggles; `hidden` the fuels whose
 * trendline the legend has switched off.
 */
function render(rows, { fuels = ['diesel'], hidden = [], lang = 'en', theme = 'dark' } = {}) {
    const chartEl = makeElement('svg');
    const legendEl = makeElement('div');
    documentStub.documentElement.setAttribute('data-theme', theme);
    const hiddenTrends = new Set(hidden);
    const activeFuels = new Set(fuels);

    let renderChart;
    const reRender = () => renderChart();
    renderChart = compiled(
        chartEl, legendEl, documentStub, activeFuels, hiddenTrends, null,
        fuelConfig, translations, lang, reRender, 0,
        () => {}, makeElement('div'), () => rows, () => {},
        () => '#888888', (el, v) => { el.textContent = String(v); }, () => '01/08', () => '00:00',
        () => {}, () => {}, () => {}, (v) => String(v),
        h, makeLoc(lang),
    );
    renderChart();
    return { chartEl, legendEl, hiddenTrends, redraw: renderChart };
}

/** Only the trendlines: dashed strokes drawn from the fuel palette. */
function trendLines(chartEl) {
    const palette = new Set(Object.values(fuelConfig).map((f) => f.color));
    return chartEl.children
        .filter((el) => el.tagName === 'line' && palette.has(el.getAttribute('stroke')))
        .map((el) => ({
            stroke: el.getAttribute('stroke'),
            dash: el.getAttribute('stroke-dasharray'),
            width: el.getAttribute('stroke-width'),
            x1: Number(el.getAttribute('x1')), y1: Number(el.getAttribute('y1')),
            x2: Number(el.getAttribute('x2')), y2: Number(el.getAttribute('y2')),
        }));
}

/** Legend entries carrying a line swatch, i.e. the trend ones, as plain text. */
function trendLegend(legendEl) {
    return legendEl.children
        .filter((el) => el.innerHTML.includes('legend-line'))
        .map((el) => ({
            text: el.innerHTML.replace(/<[^>]*>/g, ' ').replace(/\s+/g, ' ').trim(),
            off: String(el.className).includes('off'),
        }));
}

/** The slope the legend reports, in ct/day, parsed back out of its label. */
function reportedRate(legendEl, index = 0) {
    const entry = trendLegend(legendEl)[index];
    if (!entry) return null;
    const m = entry.text.match(/([+−])(\d+[.,]\d+)/);
    return m === null ? null : Number((m[1] === '−' ? '-' : '') + m[2].replace(',', '.'));
}

/* ── The fit ────────────────────────────────────────────────────── */

// A ramp of exactly 1.2 ct/day comes back as 1.2 ct/day.
{
    const rows = series('a', 'Alpha', (h) => 1.5 + (0.012 / 24) * h);
    const { legendEl } = render(rows);
    check('an even ramp is reported at its own slope', reportedRate(legendEl), 1.2);
}

// A station that reprices hourly must not outvote one that reprices every
// eight hours. Here one station holds a flat price all week while the other
// climbs: how often the flat one is re-listed cannot change the answer, since
// it stands at the same price over the same window either way. A fit that
// counted rows would hand it eight times the say at the hourly cadence.
{
    const rising = series('rising', 'Rising', (h) => 1.60 + (0.024 / 24) * h, { everyHours: 8 });
    const flatAt = (everyHours) => series('flat', 'Flat', () => 1.70, { everyHours });
    const pooled = (everyHours) =>
        reportedRate(render([...flatAt(everyHours), ...rising].sort((a, b) => a._ts - b._ts)).legendEl);
    check('a churning station does not outvote a slow one', pooled(1), pooled(8));
    // Equal time on screen, equal say: the pair lands halfway between a flat
    // trend and the climber's own.
    const climbAlone = reportedRate(render(rising).legendEl);
    checkClose('the two stations get an equal say', pooled(1) * 2, climbAlone);
}

// Snapshots record price changes, but the writer also refreshes the newest row
// in place, and a station can be re-listed at a price it already had. Sampling
// the very same staircase more finely must not move the trend: the prices on
// screen did not change, so neither may the line through them. The path here
// is deliberately bent — a flat stretch then a climb — because any straight
// ramp would survive a reweighting by accident.
{
    const priceAt = (h) => (h < 12 ? 1.60 : 1.60 + 0.01 * (h - 12));
    const coarse = series('a', 'Alpha', priceAt, { everyHours: 4 });
    // Same staircase, four times the rows over the flat stretch.
    const dense = series('a', 'Alpha', priceAt, { everyHours: 1 })
        .filter((r) => {
            const hrs = (r._ts - T0) / HOUR;
            return hrs < 12 || hrs % 4 === 0;
        });
    const coarseRate = reportedRate(render(coarse).legendEl);
    check('resampling the same prices does not move the trend',
        reportedRate(render(dense).legendEl), coarseRate);
    // 1.60 stands over [0,16), 1.64 over [16,20), 1.68 over [20,24), so over
    // the window ∫y = 38.88, ∫ty = 470.72, ∫t² = 4608: slope 4.16/1152 €/h,
    // which is 8.67 ct/day. Well under the 12 ct/day the endpoints alone
    // suggest — most of the window was flat, and the line says so.
    check('and it is the least-squares line through that staircase', coarseRate, 8.67);
}

// The last price a station posted stands to the window's right edge — that is
// what the chart shows and what the crosshair reports. A station that has not
// repriced in days must therefore count for those days, not drop out after its
// final row: writing that same price out again at the edge cannot change the
// staircase, so it cannot change the trend either.
{
    const pin = series('other', 'Other', () => 1.60, { everyHours: 4 });   // runs to the edge
    const row = (h, diesel) => ({
        station_id: 'quiet', station_name: 'Quiet', recorded_at: new Date(T0 + h * HOUR).toISOString(),
        _ts: T0 + h * HOUR, e5: null, e10: null, diesel,
    });
    const stops = [...pin, row(0, 1.90), row(2, 1.60)].sort((a, b) => a._ts - b._ts);
    const spelledOut = [...stops, row(24, 1.60)].sort((a, b) => a._ts - b._ts);
    const rate = reportedRate(render(stops).legendEl);
    check('a station that stopped reporting still holds its last price',
        rate, reportedRate(render(spelledOut).legendEl));
    // Its 1.90 stood for two of the twenty-four hours and its 1.60 for the
    // other twenty-two. Stop the clock at its final row instead and only the
    // 1.90 counts, which reads -10.63 — half again as steep as the prices on
    // screen went.
    check('and holding it is what keeps the fit honest', rate, -6.87);
}

// A flat market trends flat, however unevenly it was sampled.
{
    const rows = [
        { station_id: 'a', station_name: 'Alpha', _ts: T0, recorded_at: '', e5: null, e10: null, diesel: 1.7 },
        { station_id: 'a', station_name: 'Alpha', _ts: T0 + HOUR, recorded_at: '', e5: null, e10: null, diesel: 1.7 },
        { station_id: 'a', station_name: 'Alpha', _ts: T0 + 19 * HOUR, recorded_at: '', e5: null, e10: null, diesel: 1.7 },
        { station_id: 'a', station_name: 'Alpha', _ts: T0 + 24 * HOUR, recorded_at: '', e5: null, e10: null, diesel: 1.7 },
    ];
    check('a flat market trends flat', reportedRate(render(rows).legendEl), 0);
}

// A falling market reads as falling, with the minus sign the legend uses.
{
    const rows = series('a', 'Alpha', (h) => 2.0 - (0.008 / 24) * h, { everyHours: 3, hours: 48 });
    const { legendEl } = render(rows);
    check('a falling market is reported negative', reportedRate(legendEl), -0.8);
    check('a falling trend is signed with a real minus',
        trendLegend(legendEl)[0].text.includes('−'), true);
}

// Two stations at different levels share one slope; the offset must not tilt it.
{
    const ramp = (h) => (0.012 / 24) * h;
    const rows = [
        ...series('lo', 'Lo', (h) => 1.4 + ramp(h), { everyHours: 2 }),
        ...series('hi', 'Hi', (h) => 1.8 + ramp(h), { everyHours: 2 }),
    ].sort((a, b) => a._ts - b._ts);
    const alone = reportedRate(render(series('lo', 'Lo', (h) => 1.4 + ramp(h), { everyHours: 2 })).legendEl);
    check('a price-level offset does not tilt the slope', reportedRate(render(rows).legendEl), alone);
    check('and a 1.20 ct/day ramp reads as such', alone, 1.19);
}

/* ── One trend per fuel, each in its own dashed style ────────────── */

{
    const rows = series('a', 'Alpha', (h, fuel) => {
        const base = { diesel: 1.70, e10: 1.76, e5: 1.82 }[fuel];
        return base + (0.012 / 24) * h;
    }, { everyHours: 2, fuels: ['e5', 'e10', 'diesel'] });

    const { chartEl, legendEl } = render(rows, { fuels: ['e5', 'e10', 'diesel'] });
    const lines = trendLines(chartEl);
    check('every active fuel gets a trendline', lines.length, 3);
    // Read the expected style off the page's own palette: what matters is that
    // each fuel is drawn in the colour and dash the palette gives it, not what
    // those happen to be today.
    check('each fuel is drawn in its own colour and dash',
        lines.map((l) => [l.stroke, l.dash]),
        ['e5', 'e10', 'diesel'].map((f) => [fuelConfig[f].color, fuelConfig[f].dash]));
    check('a trend is dashed, never solid', lines.every((l) => /\d/.test(l.dash || '')), true);
    check('the dash patterns tell the fuels apart',
        new Set(lines.map((l) => l.dash)).size, 3);
    check('a trend is drawn heavier than a station line', lines.map((l) => l.width),
        ['2.5', '2.5', '2.5']);
    check('the legend lists one entry per fuel',
        trendLegend(legendEl).map((e) => e.text),
        ['Trend E5 +1.19 ct/day', 'Trend E10 +1.19 ct/day', 'Trend Diesel +1.19 ct/day']);

    // Only the fuels on the toggles are trended.
    const dieselOnly = render(rows, { fuels: ['diesel'] });
    check('a toggled-off fuel gets no trend',
        trendLines(dieselOnly.chartEl).map((l) => l.stroke), ['#60a5fa']);
    check('and no legend entry either',
        trendLegend(dieselOnly.legendEl).map((e) => e.text), ['Trend Diesel +1.19 ct/day']);

    // The legend is localised, decimal comma included.
    check('the legend follows the language',
        trendLegend(render(rows, { lang: 'de' }).legendEl).map((e) => e.text),
        ['Trend Diesel +1,19 ct/Tag']);
}

// A trend spans the prices it was fitted from, not the whole chart. A fuel
// that only started being reported halfway through the window must not have a
// line drawn across the half where it has no data to claim anything about.
{
    const rows = series('a', 'Alpha', (h, fuel) => {
        if (fuel === 'e5' && h < 12) return null;
        return (fuel === 'diesel' ? 1.70 : 1.82) + (0.012 / 24) * h;
    }, { everyHours: 2, fuels: ['e5', 'diesel'] });
    const [e5Line, dieselLine] = trendLines(render(rows, { fuels: ['e5', 'diesel'] }).chartEl);
    // margin.left is 68 and the plot 868 wide at the stub's 960px, so a fuel
    // that starts at the window's midpoint starts at 68 + 434 = 502.
    check('a trend starts where its own fuel does',
        [Math.round(dieselLine.x1), Math.round(e5Line.x1)], [68, 502]);
    check('and both run to the right edge',
        Math.round(e5Line.x2), Math.round(dieselLine.x2));
}

/* ── Hiding a trend from the legend ─────────────────────────────── */

{
    const rows = series('a', 'Alpha', (h, fuel) => (fuel === 'diesel' ? 1.7 : 1.8) + (0.012 / 24) * h,
        { everyHours: 2, fuels: ['e10', 'diesel'] });

    const hidden = render(rows, { fuels: ['e10', 'diesel'], hidden: ['diesel'] });
    check('a hidden fuel draws no trendline',
        trendLines(hidden.chartEl).map((l) => l.stroke), ['#34d399']);
    check('but keeps its legend entry, marked off',
        trendLegend(hidden.legendEl).map((e) => [e.text, e.off]),
        [['Trend E10 +1.19 ct/day', false], ['Trend Diesel +1.19 ct/day', true]]);

    // Clicking the entry is what toggles it, and the chart redraws.
    const live = render(rows, { fuels: ['e10', 'diesel'] });
    check('both trends are drawn to begin with', trendLines(live.chartEl).length, 2);
    live.legendEl.children.find((el) => el.innerHTML.includes('Diesel')).click();
    check('clicking a legend entry hides that fuel', [...live.hiddenTrends], ['diesel']);
    check('and the redraw drops its line',
        trendLines(live.chartEl).map((l) => l.stroke), ['#34d399']);
    live.legendEl.children.find((el) => el.innerHTML.includes('Diesel')).click();
    check('clicking again brings it back', trendLines(live.chartEl).length, 2);
}

/* ── Staying inside the plot ────────────────────────────────────── */

// A steep fit through lopsided data leaves the padded y-range: a low plateau
// over two thirds of the window and a high one over the last third puts the
// fitted price at the left edge well under the cheapest price ever recorded.
// The line has to be clipped to the plot band rather than painted over the
// axis labels below it.
{
    const rows = [
        ...series('a', 'Alpha', () => 1.60, { everyHours: 2, hours: 14 }),
        ...series('a', 'Alpha', () => 2.40, { everyHours: 2, hours: 24 }).slice(8),
    ].sort((a, b) => a._ts - b._ts);
    const [line] = trendLines(render(rows).chartEl);
    // margin.top 24, H 380, margin.bottom 60 at the stub's 960px width.
    const bot = 380 - 60;
    check('the fit really does leave the plot', line.y1 >= bot - 0.01, true);
    check('a trend leaving the plot is clipped to it',
        [line.y1 <= bot + 0.01, line.y2 >= 24 - 0.01 && line.y2 <= bot + 0.01], [true, true]);
    check('and still slopes the way the market moved', line.y2 < line.y1, true);
}

/* ── Degenerate data draws nothing rather than a NaN line ────────── */

{
    const one = series('a', 'Alpha', () => 1.7, { hours: 0 });
    check('a single snapshot yields no trend', trendLines(render(one).chartEl).length, 0);
    check('and no legend entry', trendLegend(render(one).legendEl).length, 0);

    // Every row at the same instant: no span to trend over.
    const instant = [
        { station_id: 'a', station_name: 'Alpha', _ts: T0, recorded_at: '', e5: null, e10: null, diesel: 1.7 },
        { station_id: 'b', station_name: 'Beta', _ts: T0, recorded_at: '', e5: null, e10: null, diesel: 1.8 },
    ];
    check('rows sharing one instant yield no trend', trendLines(render(instant).chartEl).length, 0);

    // A fuel the rows never carry is not trended, and does not disturb the
    // fuel that is there.
    const rows = series('a', 'Alpha', (h) => 1.7 + (0.012 / 24) * h, { everyHours: 2 });
    const mixed = render(rows, { fuels: ['e5', 'diesel'] });
    check('a fuel with no prices is skipped',
        trendLines(mixed.chartEl).map((l) => l.stroke), ['#60a5fa']);
    check('the fuel that has prices is unaffected', reportedRate(mixed.legendEl), 1.19);
}

if (failures > 0) {
    console.log(`web_chart_test: ${failures} failed`);
    process.exit(1);
}
console.log('web_chart_test: ok');
