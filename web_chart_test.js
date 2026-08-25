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
// The locale helper and the price formatters read currentLang, so they are
// built per language. Lifting them keeps the decimal separator, the raised
// tenth-of-a-cent digit and the em-dash fallback the page's own.
const makeLocale = new Function('currentLang', [
    lift('function _loc() {'),
    lift('function decimalSeparator() {'),
    lift('function separatorHtml() {'),
    lift('function priceParts(v) {'),
    lift('function fmtPriceText(v, fallback) {'),
    lift('function fmtPriceHtml(v, fallback) {'),
    'return { _loc, fmtPriceText, fmtPriceHtml };',
].join('\n'));

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
    'fmtPriceText', 'formatDateTime', 'h', '_loc',
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
    const tooltip = makeElement('div');
    documentStub.documentElement.setAttribute('data-theme', theme);
    const hiddenTrends = new Set(hidden);
    const activeFuels = new Set(fuels);
    const locale = makeLocale(lang);

    let renderChart;
    const reRender = () => renderChart();
    renderChart = compiled(
        chartEl, legendEl, documentStub, activeFuels, hiddenTrends, null,
        fuelConfig, translations, lang, reRender, 0,
        () => {}, tooltip, () => rows, () => {},
        () => '#888888', (el, v) => { el.textContent = String(v); }, () => '01/08', () => '00:00',
        () => {}, () => {}, () => {}, locale.fmtPriceHtml,
        locale.fmtPriceText, (iso) => iso, h, locale._loc,
    );
    renderChart();
    // The crosshair fills the tooltip from the overlay's pointermove. The
    // overlay is the last rect drawn, and the handler maps clientX through the
    // element's box, which the stub reports as 960 wide starting at 0.
    const overlay = [...chartEl.children].reverse().find((el) => el.tagName === 'rect');
    const hover = (fraction) => {
        for (const fn of overlay.listeners.pointermove || []) {
            fn({ pointerType: 'mouse', clientX: fraction * 960, clientY: 100 });
        }
        return tooltip.innerHTML;
    };
    return { chartEl, legendEl, tooltip, hiddenTrends, hover, redraw: renderChart };
}

// The foreground the trend is drawn in, per theme, as renderChart picks it.
const TREND_INK = { dark: '#e8eaed', light: '#1c1c1e' };

const strokeOf = (el) => ({
    stroke: el.getAttribute('stroke'),
    dash: el.getAttribute('stroke-dasharray'),
    width: el.getAttribute('stroke-width'),
    opacity: el.getAttribute('opacity'),
    x1: Number(el.getAttribute('x1')), y1: Number(el.getAttribute('y1')),
    x2: Number(el.getAttribute('x2')), y2: Number(el.getAttribute('y2')),
});

/**
 * One entry per trendline: the line itself, drawn in the theme's foreground,
 * and the glow passes underneath it in the fuel's colour, outermost first.
 * Grouped by the geometry they share, so the number of glow passes is the
 * drawing's business and not this harness's.
 */
function trends(chartEl, theme = 'dark') {
    const palette = new Set(Object.values(fuelConfig).map((f) => f.color));
    const groups = new Map();
    for (const el of chartEl.children) {
        if (el.tagName !== 'line') continue;
        const stroke = el.getAttribute('stroke');
        if (stroke !== TREND_INK[theme] && !palette.has(stroke)) continue;   // not a trend
        const s = strokeOf(el);
        const key = [s.x1, s.y1, s.x2, s.y2, s.dash].join('|');
        if (!groups.has(key)) groups.set(key, { line: null, glow: [] });
        (stroke === TREND_INK[theme] ? (groups.get(key).line = s) : groups.get(key).glow.push(s));
    }
    return [...groups.values()];
}

/** Just the lines, which carry the geometry and the dash. */
function trendLines(chartEl, theme = 'dark') {
    return trends(chartEl, theme).map((t) => t.line).filter(Boolean);
}

/** Just the glow, flattened — its fuel colour is what identifies the trend. */
function trendHalos(chartEl, theme = 'dark') {
    return trends(chartEl, theme).flatMap((t) => t.glow);
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
    const halos = trendHalos(chartEl);
    check('every active fuel gets a trendline', lines.length, 3);

    // Station hues are spread around the whole wheel, so a trend cannot be
    // told apart by colour alone: it is drawn in the theme's foreground, which
    // no station line takes, over a halo in its fuel's colour.
    check('the line itself is the theme foreground, not a fuel colour',
        lines.map((l) => l.stroke), [TREND_INK.dark, TREND_INK.dark, TREND_INK.dark]);
    check('and the light theme switches it', trendLines(
        render(rows, { fuels: ['diesel'], theme: 'light' }).chartEl, 'light').length, 1);
    check('each trend glows in its own fuel colour',
        trends(chartEl).map((t) => [...new Set(t.glow.map((g) => g.stroke))]),
        ['e5', 'e10', 'diesel'].map((f) => [fuelConfig[f].color]));
    // Each pass out from the line is wider and fainter than the one inside it,
    // which is what makes a glow rather than a fat coloured line.
    check('the glow widens and fades outward', trends(chartEl).map((t) => {
        const passes = [...t.glow, t.line];   // outermost first, line last
        return passes.every((p, i) => i === 0
            || (Number(p.width) < Number(passes[i - 1].width)
                && Number(p.opacity) > Number(passes[i - 1].opacity)));
    }), [true, true, true]);
    check('the glow is never fully opaque',
        halos.every((g) => Number(g.opacity) < 1), true);
    check('every trend is a line with glow behind it',
        trends(chartEl).map((t) => [t.line !== null, t.glow.length > 0]),
        [[true, true], [true, true], [true, true]]);

    // Read the dash off the page's own palette: what matters is that each fuel
    // gets the dash the palette gives it, not what that happens to be today.
    check('each fuel is dashed the way its palette entry says',
        lines.map((l) => l.dash), ['e5', 'e10', 'diesel'].map((f) => fuelConfig[f].dash));
    check('a trend is dashed, never solid', lines.every((l) => /\d/.test(l.dash || '')), true);
    check('the dash patterns tell the fuels apart',
        new Set(lines.map((l) => l.dash)).size, 3);
    check('the legend lists one entry per fuel',
        trendLegend(legendEl).map((e) => e.text),
        ['Trend E5 +1.19 ct/day', 'Trend E10 +1.19 ct/day', 'Trend Diesel +1.19 ct/day']);

    // Only the fuels on the toggles are trended.
    const dieselOnly = render(rows, { fuels: ['diesel'] });
    check('a toggled-off fuel gets no trend',
        [...new Set(trendHalos(dieselOnly.chartEl).map((l) => l.stroke))], [fuelConfig.diesel.color]);
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
        [...new Set(trendHalos(hidden.chartEl).map((l) => l.stroke))], [fuelConfig.e10.color]);
    check('but keeps its legend entry, marked off',
        trendLegend(hidden.legendEl).map((e) => [e.text, e.off]),
        [['Trend E10 +1.19 ct/day', false], ['Trend Diesel +1.19 ct/day', true]]);

    // Clicking the entry is what toggles it, and the chart redraws.
    const live = render(rows, { fuels: ['e10', 'diesel'] });
    check('both trends are drawn to begin with', trendLines(live.chartEl).length, 2);
    live.legendEl.children.find((el) => el.innerHTML.includes('Diesel')).click();
    check('clicking a legend entry hides that fuel', [...live.hiddenTrends], ['diesel']);
    check('and the redraw drops its line',
        [...new Set(trendHalos(live.chartEl).map((l) => l.stroke))], [fuelConfig.e10.color]);
    live.legendEl.children.find((el) => el.innerHTML.includes('Diesel')).click();
    check('clicking again brings it back', trendLines(live.chartEl).length, 2);
}

/* ── What the crosshair and the hint report ─────────────────────── */

// Hovering the chart should say where the trend stands at that moment, next to
// the station prices, so a price can be read as above or below it.
{
    const rows = [
        ...series('a', 'Alpha', (h) => 1.70 + (0.024 / 24) * h, { everyHours: 2 }),
        ...series('b', 'Beta', (h) => 1.80 + (0.024 / 24) * h, { everyHours: 2 }),
    ].sort((a, b) => a._ts - b._ts);
    const { hover, legendEl } = render(rows);
    const rowsOf = (html) => html.split('tt-row').length - 1;
    // Rows split off their opening tag, so the class list survives on each and
    // the trend's row is found by it rather than by where it sits. Its name and
    // price are read as fields: the price markup raises the third decimal into
    // its own span, so flattening a row wholesale would break the number up.
    const flat = (frag) => frag.replace(/<[^>]*>/g, '').replace(/\s+/g, ' ').trim();
    const rowsIn = (html) => html.split('<div class="tt-row').slice(1).map((r) => ({
        trend: r.startsWith(' tt-trend'),
        name: flat((/class="tt-name">([\s\S]*?)<\/span>/.exec(r) || ['', ''])[1]),
        value: flat(r.slice(r.indexOf('class="tt-val"')).replace(/^[^>]*>/, '')),
        swatch: r.includes('legend-line') ? 'line' : r.includes('legend-dot') ? 'dot' : 'none',
    }));
    const trendRow = (html) => {
        const row = rowsIn(html).find((r) => r.trend);
        return row ? `${row.name} ${row.value}` : null;
    };

    const left = hover(0.08);
    check('the crosshair lists both stations and the trend', rowsOf(left), 3);
    // The pair averages 1.75 at the window's left edge and climbs 2.4 ct/day.
    check('the trend row is labelled and priced', trendRow(left), 'Trend 1.749 €');
    check('and it tracks the hovered moment', trendRow(hover(0.5)), 'Trend 1.761 €');
    check('to the right it has climbed again', trendRow(hover(0.95)), 'Trend 1.773 €');
    // The trend leads, so a long station list spilling into further columns
    // cannot push the benchmark out of sight.
    check('the trend leads the list', rowsIn(left).map((r) => r.trend), [true, false, false]);
    check('the stations under it stay sorted by price',
        rowsIn(left).filter((r) => !r.trend).map((r) => r.value), ['1.700 €', '1.800 €']);
    // It carries the same swatch the chart and the legend use.
    check('the trend has a line swatch and the stations dots',
        rowsIn(left).map((r) => r.swatch), ['line', 'dot', 'dot']);

    // The legend entry's hint gives the same reading at the right edge, for
    // anyone who has not hovered the chart.
    check('the hint names the latest reading',
        legendEl.children.find((el) => el.innerHTML.includes('legend-line')).title,
        `${translations.en.trendHint}\n${translations.en.trendLatest}: 1.773 €`);
    check('and it is localised too',
        render(rows, { lang: 'de' }).legendEl.children
            .find((el) => el.innerHTML.includes('legend-line')).title,
        `${translations.de.trendHint}\n${translations.de.trendLatest}: 1,773 €`);
}

// A trend switched off in the legend is not reported by the crosshair either.
{
    const rows = series('a', 'Alpha', (h) => 1.70 + (0.024 / 24) * h, { everyHours: 2 });
    check('a hidden trend is left out of the crosshair',
        render(rows, { hidden: ['diesel'] }).hover(0.5).includes('legend-line'), false);
    check('while a shown one is in it',
        render(rows).hover(0.5).includes('legend-line'), true);
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
        [...new Set(trendHalos(mixed.chartEl).map((l) => l.stroke))], [fuelConfig.diesel.color]);
    check('the fuel that has prices is unaffected', reportedRate(mixed.legendEl), 1.19);
}

/* ── The price cards' shared rows ───────────────────────────────── */
// stationBlock and stationRankRow build every station reference on the page, so
// they are what "the cards look like one thing" means in practice. What is
// tested is the distance: that it is its own column rather than a tail on the
// station's name, which is what used to let a long name ellipsize it away.

const ROW_DEPS = [
    'translations', 'currentLang', 'fuelConfig', 'FUEL_CSS_COLORS', 'ICON_STATION_INFO',
    'ICON_PIN', 'stationDot', 'h', 'fmtDistanceKm', 'fmtDistanceKmHtml',
    'fmtPriceHtml', 'fmtPriceText', 'formatDateTime',
    'nearbyCard', 'selectedFuel', 'locationLabel', 'locationRadiusKm',
    'nearbyRows', 'nearbyTotal', 'nearbyExpanded', 'NEARBY_PREVIEW_ROWS',
    'predictionStationMeta',
];

const rowSource = [
    lift('\nfunction stationBlock(stationId, stationName, fuel, addressHtml, distKm, trailingHtml) {'),
    lift('\nfunction stationRankRow(stationId, stationName, distKm, fuel, price, trailingHtml, titleText) {'),
    lift('\nfunction nearbyClosedTag(row) {'),
    lift('\nfunction renderNearby() {'),
].join('\n');

// The distance formatters read currentLang the same way the price ones do.
const makeDistance = new Function('currentLang', [
    lift('function _loc() {'),
    lift('function decimalSeparator() {'),
    lift('function separatorHtml() {'),
    lift('function fmtDecimal(v, digits) {'),
    lift('function fmtDecimalHtml(v, digits) {'),
    lift('function fmtDistanceKm(v) {'),
    lift('function fmtDistanceKmHtml(v) {'),
    'return { fmtDistanceKm, fmtDistanceKmHtml };',
].join('\n'));

/** Compile the row builders and the card against one set of stand-ins. */
function compileRows({
    lang = 'en', card = null, fuel = 'all', label = 'Berlin', radiusKm = 5,
    rows = [], total = null, expanded = false, meta = {},
} = {}) {
    const locale = makeLocale(lang);
    const distance = makeDistance(lang);
    return new Function(...ROW_DEPS,
        `${rowSource}\nreturn { stationBlock, stationRankRow, renderNearby };`)(
        translations, lang, fuelConfig,
        { e5: 'var(--e5)', e10: 'var(--e10)', diesel: 'var(--diesel)' }, '<info/>',
        '<pin/>', (name) => `<dot ${name}>`, h, distance.fmtDistanceKm, distance.fmtDistanceKmHtml,
        locale.fmtPriceHtml, locale.fmtPriceText, (iso) => iso,
        card, fuel, label, radiusKm,
        rows, total === null ? rows.length : total, expanded, 8,
        meta,
    );
}

/** The text of one rendered row, markup stripped. */
function rowText(html) {
    return html.replace(/<[^>]*>/g, ' ').replace(/\s+/g, ' ').trim();
}

{
    const { stationBlock, stationRankRow } = compileRows();

    // The name that broke the old rows: long enough that the distance behind it
    // fell off the end.
    const long = 'Markant Tankautomat Hüllhorst';
    const rank = stationRankRow('s1', long, 4.3, 'diesel', 1.709, '', '');
    check('the distance is its own element, not a tail on the name',
        rank.includes(`<span class="row-dist">4<span class="price-sep">.</span>3 km</span>`), true);
    check('so the name is not carrying it any more', rank.includes(`${long} (`), false);
    // The name is what gives way when the row runs out of width.
    check('the name sits in the column that ellipsizes',
        rank.includes(`<span class="rank-station"><dot ${long}>${long}</span>`), true);
    check('the row still leads with the price',
        rank.indexOf('rank-price') < rank.indexOf('rank-station'), true);
    check('and the distance comes last', rank.lastIndexOf('row-dist') > rank.indexOf('rank-station'), true);
    check('a reader without a location gets a row with no distance at all',
        stationRankRow('s1', long, null, 'diesel', 1.709, '', '').includes('row-dist'), false);
    // Spoken order matches the visual one.
    check('the spoken label names the price, the station and the distance',
        /1\.70.*Markant Tankautomat Hüllhorst — 4\.3 km/.test(rank), true);

    // The big cell says it the same way: address ellipsizes, distance holds the
    // right edge, and it keeps its own colour outside the address line's dimming.
    const block = stationBlock('s1', long, 'diesel', 'Hauptstraße 77, Lübbecke', 2.0);
    check('the big cell puts the distance beside the address, not inside it',
        block.includes('<span class="sd-meta-line">'), true);
    check('the address is the part that ellipsizes',
        block.includes('<span class="cheapest-station sd-addr-line">Hauptstraße 77, Lübbecke</span>'), true);
    check('and the distance follows it in its own column',
        block.includes('<span class="row-dist">2<span class="price-sep">.</span>0 km</span>'), true);
    check('a station with no address still shows how far away it is',
        stationBlock('s1', long, 'diesel', '', 2.0).includes('row-dist'), true);
    check('one with neither gets no meta line',
        stationBlock('s1', long, 'diesel', '', null).includes('sd-meta-line'), false);
}

/* ── The surroundings card ──────────────────────────────────────── */
// It is built out of the same cells as the cards above it — fuel label, big
// price, station block, ranked rows — so what is tested is that it really uses
// them, that distance and not price is what orders it, and that a radius
// holding more than it shows says so instead of reading as a complete list.

/** One payload row: station id, distance, and a price per fuel. */
function nearbyRow(id, distKm, prices = {}, open = true) {
    return {
        s: id,
        dist: distKm,
        t: '2026-08-01T09:00:00Z',
        o: open,
        e5: prices.e5 ?? null,
        e10: prices.e10 ?? null,
        diesel: prices.diesel ?? null,
    };
}

/**
 * Render the card and hand back its markup plus the pieces the tests read out
 * of it. `label` empty stands for "no location picked yet".
 */
function renderNearbyCard(rows, options = {}) {
    const card = makeElement('div');
    compileRows({ ...options, card, rows }).renderNearby();
    const html = card.innerHTML;
    return {
        html,
        // Station ids in the order the rows were emitted.
        listed: [...html.matchAll(/data-station-id="([^"]+)"/g)].map((m) => m[1]),
        text: rowText(html),
    };
}

const nearbyMeta = {
    a: { name: 'Aral Mitte', street: 'Hauptstraße 1', place: 'Berlin' },
    b: { name: 'Shell Nord', street: 'Nordstraße 12', place: 'Berlin' },
    c: { name: 'Esso West', street: 'Weststraße 7', place: 'Berlin' },
};

{
    const rows = [
        nearbyRow('a', 0.42, { e5: 1.789, e10: 1.729, diesel: 1.659 }),
        nearbyRow('b', 3.1, { e5: 1.819, diesel: 1.689 }),
        // c sells all three; b has no e10, so that column is one station shorter.
        nearbyRow('c', 4.4, { e5: 1.799, e10: 1.739, diesel: 1.679 }),
    ];
    const out = renderNearbyCard(rows, { meta: nearbyMeta });

    // Three fuels in scope, so three cells, like every other card.
    check('one cell per fuel in scope', (out.html.match(/cheapest-cell/g) || []).length, 3);
    check('each names its fuel', (out.html.match(/cheapest-fuel-label/g) || []).length, 3);
    check('and leads with a big price', (out.html.match(/cheapest-price/g) || []).length, 3);
    check('the rest follow as ranked rows', (out.html.match(/class="rank-list"/g) || []).length, 3);

    // Distance orders it, not price: b is dearer than c and still comes first.
    check('the nearest station leads each column, however it is priced',
        out.listed.slice(0, 3), ['a', 'b', 'c']);
    check('the big price is the nearest station one',
        out.html.includes('1<span class="price-sep">.</span>78<span class="price-milli">9</span>'), true);
    check('the header covers the radius', out.html.includes('>5 km<'), true);
    // The reader's own address is theirs; the card is not the place to publish it.
    check('and not where the reader lives', out.html.includes('Berlin ·'), false);
    check('a station missing a fuel is left out of that column',
        (out.html.match(/data-station-id="b"/g) || []).length, 2);
}

{
    // The fuel filter narrows to one column, like every other card.
    const out = renderNearbyCard([nearbyRow('a', 0.4, { e5: 1.789, diesel: 1.659 })],
        { fuel: 'diesel', meta: nearbyMeta });
    check('one fuel selected renders one cell', (out.html.match(/cheapest-cell/g) || []).length, 1);
    check('and it is the selected one', out.text.includes('Diesel'), true);
    check('showing that fuel price', out.text.includes('1 . 65 9'), true);
}

{
    // A closed station still lists — its price is the one it will reopen with —
    // but it says so, in the big cell and in the ranked rows alike.
    const closedFirst = renderNearbyCard([
        nearbyRow('a', 0.4, { diesel: 1.659 }, false),
        nearbyRow('b', 1.4, { diesel: 1.669 }, false),
    ], { meta: nearbyMeta });
    check('a closed station is marked wherever it appears',
        (closedFirst.html.match(/nearby-closed/g) || []).length, 2);
    const open = renderNearbyCard([nearbyRow('a', 0.4, { diesel: 1.659 }, true)], { meta: nearbyMeta });
    check('an open one is not', open.html.includes('nearby-closed'), false);
}

{
    // More rows than the preview holds: eight are shown and the rest wait
    // behind a button that names how many they are.
    const rows = Array.from({ length: 11 }, (_, i) => nearbyRow(`s${i}`, i * 0.5, { diesel: 1.6 + i / 1000 }));
    const collapsed = renderNearbyCard(rows, { fuel: 'diesel' });
    check('the preview stops at eight stations', collapsed.listed.length, 8);
    check('and offers the remainder by count', collapsed.text.includes('Show more (3)'), true);

    const expanded = renderNearbyCard(rows, { fuel: 'diesel', expanded: true });
    check('expanded shows every station', expanded.listed.length, 11);
    check('and drops the button', expanded.html.includes('nearby-more'), false);
}

{
    // The server caps how many stations it prices. Saying so is what keeps a
    // capped list from reading as "that is all there is in range".
    const rows = Array.from({ length: 9 }, (_, i) => nearbyRow(`s${i}`, i, { diesel: 1.6 }));
    const capped = renderNearbyCard(rows, { fuel: 'diesel', total: 23, expanded: true });
    check('a capped list says how much of the radius it covers',
        capped.text.includes('The 9 nearest of 23 stations in range.'), true);
    const whole = renderNearbyCard(rows, { fuel: 'diesel', total: 9, expanded: true });
    check('an uncapped one does not', whole.html.includes('nearby-foot'), false);
}

{
    // Without a location there is no "around here" to answer, and the card says
    // where to set one rather than looking broken.
    const none = renderNearbyCard([], { label: '' });
    check('no location asks for one',
        none.text.endsWith(translations.en.nearbyNoLocation), true);
    check('and shows no rows', none.listed, []);
    check('nor a radius it is not measuring from', none.html.includes('cheapest-scope'), false);

    const empty = renderNearbyCard([], { label: 'Nowhere' });
    check('a location with nothing in range says that instead',
        empty.text.includes(translations.en.nearbyNoData), true);
}

{
    // German is not a copy of the English card: the title is the one the page
    // ships, and the numbers carry the German separators.
    const out = renderNearbyCard([nearbyRow('a', 0.42, { diesel: 1.659 })],
        { fuel: 'diesel', lang: 'de', meta: nearbyMeta });
    check('the German card is titled Umgebung', out.text.includes('Umgebung'), true);
    check('and writes the distance with a comma',
        out.html.includes('0<span class="price-sep">,</span>4 km'), true);
}

/* ── The recommendations card ───────────────────────────────────── */
// The card exists to answer "when should I fill up": the day and the hours are
// the part a reader scans for, so they carry the accent colour the distances
// use, while the calendar date behind the day name stays muted. What is tested
// is that split — in both languages, since the weekday sits in a different
// place in each locale's date.

const PRED_DEPS = [
    'translations', 'currentLang', 'fuelConfig', 'FUEL_CSS_COLORS', 'ICON_STATION_INFO',
    'ICON_CLOCK', 'stationDot', 'h', 'fmtDistanceKm', 'fmtDistanceKmHtml',
    'fmtPriceHtml', 'fmtPriceText', '_loc', '_tz', 'formatDateTime', 'formatTimeOnly',
    'predictionsCard', 'selectedFuel', 'predictionData', 'predictionAsOf',
    'predictionStationMeta',
];

const predictionSource = [
    lift('\nfunction stationBlock(stationId, stationName, fuel, addressHtml, distKm, trailingHtml) {'),
    lift('\nfunction stationRankRow(stationId, stationName, distKm, fuel, price, trailingHtml, titleText) {'),
    lift('\nfunction renderPredictions() {'),
].join('\n');

// The clock helpers read currentLang the same way the price ones do, so the
// card is dated in the locale and timezone the page really shows.
const makeClock = new Function('currentLang', [
    lift('function _tz() {'),
    lift('function _loc() {'),
    lift('function formatDateTime(isoString) {'),
    lift('function formatTimeOnly(isoString) {'),
    'return { _tz, _loc, formatDateTime, formatTimeOnly };',
].join('\n'));

/** One prediction window: station, fuel, price and the hours it covers. */
function predWindow(id, fuel, start, end, price) {
    return { s: id, fuel, start, end, price };
}

/** Render the card and hand back its markup. */
function renderPredictionsCard(windows, { lang = 'en', fuel = 'all', meta = {}, asOf = {} } = {}) {
    const card = makeElement('div');
    const locale = makeLocale(lang);
    const distance = makeDistance(lang);
    const clock = makeClock(lang);
    new Function(...PRED_DEPS, `${predictionSource}\nreturn renderPredictions;`)(
        translations, lang, fuelConfig,
        { e5: 'var(--e5)', e10: 'var(--e10)', diesel: 'var(--diesel)' }, '<info/>',
        '<clock/>', (name) => `<dot ${name}>`, h, distance.fmtDistanceKm, distance.fmtDistanceKmHtml,
        locale.fmtPriceHtml, locale.fmtPriceText, clock._loc, clock._tz,
        clock.formatDateTime, clock.formatTimeOnly,
        card, fuel, windows, asOf, meta,
    )();
    return card.innerHTML;
}

{
    // Midday windows, so the day is the same one in UTC and in Berlin.
    const windows = [
        predWindow('a', 'diesel', '2026-08-25T12:00:00Z', '2026-08-25T13:00:00Z', 1.659),
        predWindow('b', 'diesel', '2026-08-25T15:00:00Z', '2026-08-25T16:00:00Z', 1.669),
    ];
    const html = renderPredictionsCard(windows, { fuel: 'diesel', meta: nearbyMeta });

    check('the day name carries the accent the distances use',
        html.includes('<span class="pred-accent">Tuesday</span>'), true);
    check('and the date beside it does not', /pred-accent">Tuesday<\/span>, 25\/08\/26/.test(html), true);
    check('the leading window’s hours are accented too',
        html.includes('<span class="pred-window">12:00–13:00</span>'), true);
    check('as are the hours on the ranked windows',
        html.includes('<span class="pred-time">15:00–16:00</span>'), true);

    // The day names its block from under the price, and every window's hours
    // hold the right edge of the line they sit on.
    check('the price leads the day block',
        html.indexOf('cheapest-price') < html.indexOf('pred-day-label'), true);
    check('the day is named on the line the hours are on',
        html.indexOf('pred-day-label') < html.indexOf('pred-window'), true);
    check('and the hours after it, on the same line',
        /<div class="pred-day">.*?<span class="pred-window">12:00–13:00<\/span><\/div>/.test(html), true);
    check('each day is one block, dividers and all',
        (html.match(/class="pred-day-block"/g) || []).length, 1);
}

{
    // The card answers when to fill up, not how far away it is, and on a phone
    // its cell is one column wide: a distance in it costs the hours their room
    // for nothing. So it carries none, even with a location picked — the other
    // three cards are where distances belong.
    const html = renderPredictionsCard([
        predWindow('a', 'diesel', '2026-08-25T12:00:00Z', '2026-08-25T13:00:00Z', 1.659),
        predWindow('b', 'diesel', '2026-08-25T15:00:00Z', '2026-08-25T16:00:00Z', 1.669),
    ], { fuel: 'diesel', meta: nearbyMeta });

    check('no distance anywhere in the card', html.includes('row-dist'), false);
    check('the address still has the line to itself',
        html.includes('<span class="cheapest-station sd-addr-line">Hauptstraße 1, Berlin</span>'), true);
    check('and the hours are the row’s right edge',
        /<span class="pred-time">15:00–16:00<\/span><\/button>/.test(html), true);
}

{
    // German puts the weekday in the same leading position but writes both the
    // day and the date differently, so the split is asserted there too.
    const html = renderPredictionsCard(
        [predWindow('a', 'diesel', '2026-08-25T12:00:00Z', '2026-08-25T13:00:00Z', 1.659)],
        { fuel: 'diesel', lang: 'de', meta: nearbyMeta });
    check('the German card accents the German day name',
        /pred-accent">Dienstag<\/span>, 25\.08\.26/.test(html), true);
    check('and the German hours with it',
        html.includes('<span class="pred-window">14:00–15:00</span>'), true);
}


if (failures > 0) {
    console.log(`web_chart_test: ${failures} failed`);
    process.exit(1);
}
console.log('web_chart_test: ok');
