/**
 * Exercises the Settings page's request-pacing estimate: the line that tells an
 * admin what the delay and burst they are typing add up to, and whether the
 * resulting sweep still fits inside the schedule.
 *
 * It is worth testing because it is the only thing standing between three
 * plausible-looking numbers and a collector that silently loses every second
 * run. The arithmetic mirrors Go's tankerPacing.pace; the constants it works
 * from are pinned against Go by TestWebPacingConstantsMatchGo.
 *
 * The estimate's own code is lifted out of web/index.php by name — the same
 * trick web_chart_test.js and web_stats_test.js use — so what runs here is what
 * ships.
 *
 * Run directly (`node web_pacing_test.js`) or via `make test`.
 */
'use strict';

const fs = require('fs');
const path = require('path');

let failures = 0;
function check(name, got, want) {
    if (JSON.stringify(got) === JSON.stringify(want)) {
        console.log(`  ok   ${name}`);
        return;
    }
    console.log(`  FAIL ${name}\n       got  ${JSON.stringify(got)}\n       want ${JSON.stringify(want)}`);
    failures++;
}

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

/** The page's own constants, read from the PHP that interpolates them. */
function phpConst(name) {
    const m = viewer.match(new RegExp(`const ${name} = (\\d+);`));
    if (m === null) {
        console.error(`web/index.php no longer declares ${name}`);
        process.exit(2);
    }
    return Number(m[1]);
}

const REQUESTS = phpConst('GASOLINE_MAX_TARGET_TILES');
const RADIUS = phpConst('GASOLINE_MAX_TARGET_RADIUS_KM');
const BUDGET = phpConst('GASOLINE_SWEEP_BUDGET_SECONDS');

/**
 * The estimate, wired to a fake form. render() reads its inputs from the DOM
 * and writes one line of text, so the stubs are three value holders and an
 * element that remembers what it was told.
 */
function build(lang) {
    const out = {
        textContent: '',
        classes: new Set(),
        classList: {
            toggle(name, on) { if (on) out.classes.add(name); else out.classes.delete(name); },
            remove(name) { out.classes.delete(name); },
        },
    };
    const delayEl = { value: '50' };
    const burstEl = { value: '1' };
    const body = new Function('translations', 'currentLang', 'out', 'delayEl', 'burstEl',
        'REQUESTS', 'RADIUS', 'BUDGET', 'MAX_SPARE', [
            lift('        function pace(n, delay, burst) {'),
            lift('        function clock(seconds) {'),
            lift('        function renderPacingEstimate() {'),
            'return { pace, clock, render: renderPacingEstimate };',
        ].join('\n'));
    const fns = body(translations, lang, out, delayEl, burstEl, REQUESTS, RADIUS, BUDGET, 99);
    return { ...fns, out, delayEl, burstEl };
}

/** The estimate for one pace, in the given language. */
function estimate(delay, burst, lang = 'en') {
    const page = build(lang);
    page.delayEl.value = String(delay);
    page.burstEl.value = String(burst);
    page.render();
    return { text: page.out.textContent, over: page.out.classes.has('over-budget') };
}

console.log('web_pacing_test: pace');

const { pace, clock } = build('en');

// Mirrors Go's tankerPacing.pace: the last of n requests waits
// ((n-1)/burst)·delay, with integer division.
check('one request never waits', pace(1, 50, 1), 0);
check('the sixth request of a one-per-window pace goes out at 4:10', pace(6, 50, 1), 250);
check('a burst of three lets three go out together', pace(3, 50, 3), 0);
check('and holds the fourth for one window', pace(4, 50, 3), 50);
check('a zero delay never waits', pace(6, 0, 1), 0);
check('nor does a burst of zero, which would let nothing out', pace(6, 50, 0), 0);

console.log('web_pacing_test: clock');
check('a whole minute', clock(300), '5:00');
check('seconds are zero-padded', clock(250), '4:10');
check('under a minute', clock(9), '0:09');
check('zero', clock(0), '0:00');

console.log('web_pacing_test: the estimate');

// The shipped default: six requests 50 s apart end at 4:10 of the 4:50 a sweep
// has, which is not another whole window — so nothing is left for a retry.
const shipped = estimate(50, 1);
check('the shipped pace is not over budget', shipped.over, false);
check('the shipped pace reports its wall clock', shipped.text.includes('4:10'), true);
check('and the budget it fits inside', shipped.text.includes('4:50'), true);
check('and that no retried request fits', shipped.text.endsWith('Room for retried requests: 0.'), true);

// 45 s leaves exactly one: 7 requests reach 4:30.
check('45 s leaves room for one retried request',
    estimate(45, 1).text.endsWith('Room for retried requests: 1.'), true);
// 60 s does not even fit the clean sweep.
const tooSlow = estimate(60, 1);
check('60 s runs past the budget', tooSlow.over, true);
check('and says so rather than reporting spare room', tooSlow.text.includes('5:00'), true);
check('a pace that fits again clears the warning', estimate(50, 1).over, false);

// A zero delay is no pace at all, not an infinitely generous one: reporting
// "room for 99" there would be a measurement of the loop's own stop.
const unpaced = estimate(0, 1);
check('a zero delay reads as pacing being off', unpaced.text, translations.en.pacingEstimateUnpaced
    .replace('{radius}', String(RADIUS)).replace('{requests}', String(REQUESTS)));
check('and is not flagged as over budget', unpaced.over, false);

console.log('web_pacing_test: translations');
check('the estimate is German in German', estimate(50, 1, 'de').text.startsWith('Ein Ziel'), true);
check('as is the over-budget warning', estimate(60, 1, 'de').text.startsWith('Ein Ziel'), true);
for (const key of ['requestPacing', 'requestPacingHint', 'pacingDelay', 'pacingBurst',
    'pacingRetries', 'pacingEstimate', 'pacingEstimateOver', 'pacingEstimateUnpaced']) {
    check(`${key} exists in both languages`,
        [typeof translations.en[key], typeof translations.de[key]], ['string', 'string']);
}
// Every placeholder the code substitutes has to be in both templates, or one
// language quietly ships a sentence with a hole in it.
for (const key of ['pacingEstimate', 'pacingEstimateOver']) {
    for (const placeholder of ['{radius}', '{requests}', '{clean}', '{budget}']) {
        check(`${key} keeps ${placeholder} in both languages`,
            [translations.en[key].includes(placeholder), translations.de[key].includes(placeholder)],
            [true, true]);
    }
}
check('only the fitting estimate reports spare room',
    [translations.en.pacingEstimate.includes('{spare}'), translations.de.pacingEstimate.includes('{spare}')],
    [true, true]);

if (failures > 0) {
    console.log(`web_pacing_test: ${failures} failed`);
    process.exit(1);
}
console.log('web_pacing_test: ok');
