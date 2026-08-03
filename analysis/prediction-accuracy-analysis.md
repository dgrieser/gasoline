# Prediction accuracy analysis — Diesel, 14d window (as of 2026-08-03)

This document analyzes the gap between persisted price predictions and the
actual prices recorded for their target windows, identifies where the current
model (`scoreForecast` + baseline/offset decomposition + per-station bias
learning) systematically misses, and proposes data-driven optimizations —
including additional self-learning loops beyond the existing per-station bias
correction.

Source data: the admin **Prediction accuracy** page (fuel = Diesel, all
cities, target range 14d, all confidences), i.e. SQL aggregates over
`price_predictions` rows with a non-NULL `error`. The companion file
`prediction-accuracy-queries.sql` contains queries to verify every hypothesis
below directly against the live MySQL database.

## 1. Headline numbers

| Metric | Value |
| --- | --- |
| Evaluated predictions | 381,772 (28 stations) |
| MAE | 4.16 ct |
| RMSE | 6.16 ct |
| Bias (actual − predicted) | **+0.75 ct** (under-prediction) |
| Within ±1 ct | 23.5 % |
| Within ±2 ct | 37.5 % |
| Largest error | 36.90 ct |

By confidence:

| Confidence | Count | MAE | Bias |
| --- | --- | --- | --- |
| medium | 370,971 | 4.19 ct | +0.77 ct |
| low | 10,801 | **3.23 ct** | +0.28 ct |
| high | **0** | — | — |

By lead time:

| Lead | Count | MAE | Bias |
| --- | --- | --- | --- |
| 0–1h | 6,824 | 2.61 ct | +0.06 ct |
| 1–3h | 13,612 | 2.95 ct | +0.10 ct |
| 3–6h | 20,363 | 3.24 ct | +0.13 ct |
| 6–12h | 40,017 | 3.43 ct | +0.40 ct |
| 12–24h | 76,261 | 3.81 ct | +0.35 ct |
| >24h (remainder) | ~224,700 (59 %) | ~4.6 ct (implied) | ~+1.0 ct (implied) |

By target hour (UTC; +2h = local CEST). Abridged to the interesting rows:

| Hour (UTC) | Local | MAE | Bias |
| --- | --- | --- | --- |
| 03–05 | 05–07 | 3.11–3.18 ct | +0.08…+0.34 ct |
| 07 | 09 | 3.60 ct | **−0.23 ct** |
| 08 | 10 | 3.74 ct | **−0.31 ct** |
| 09 | 11 | 3.81 ct | **−0.41 ct** |
| **10** | **12** | **7.98 ct** | **+5.87 ct** |
| **11** | **13** | **6.18 ct** | **+3.83 ct** |
| 12 | 14 | 4.87 ct | +1.58 ct |
| 13 | 15 | 4.61 ct | +0.37 ct |
| 16–21 | 18–23 | 3.6–4.0 ct | +0.30…+0.58 ct |

## 2. What the data says

### Finding A — the model systematically misses the noon jump (biggest defect)

Target hours 10–11 UTC (12:00–14:00 local) show MAE of 7.98/6.18 ct with bias
+5.87/+3.83 ct, versus ~3.5 ct MAE and near-zero bias elsewhere. The
predicted-vs-actual timeline confirms it visually: the orange (predicted)
spikes consistently top out 5–10 ct below the blue (actual) daily noon peaks.

These two hours are ~10 % of all evaluated rows but contribute roughly
**0.35 ct of the overall 4.16 ct MAE** in excess error alone.

Two mechanisms in `scoreForecast` explain the full hourly bias profile:

1. **Shape shrinkage toward the day median.** For offset-mode stations the
   `recent` bucket's weighted median is ≈ 0 by construction (offsets over the
   pricing day center on the baseline). The blends
   `0.60*weekdayHour + 0.30*hour + 0.10*recent` and `0.75*hour + 0.25*recent`
   therefore multiply every predicted intraday offset by ~0.9 (or ~0.75 on the
   low-confidence path). At flat hours that costs nothing; at the noon spike
   (offset +20…+40 ct) it shaves 2–4 ct off the peak — under-prediction. At
   the deepest valley (09:00–11:00 local, right before the jump) it lifts the
   prediction above the actual — and indeed those are exactly the only hours
   with *negative* bias (−0.23…−0.41 ct). A single scalar can't fix this;
   the sign of the error flips with the sign of the hour offset.

2. **Flat baseline in a trending market.** `BaselineForecast` is held flat
   across the prediction window ("future baseline shifts are unknowable"). The
   14-day window shows a steady upward trend (~2.10 € → ~2.19 €, roughly
   +0.6 ct/day). Every prediction that crosses a pricing-day boundary
   inherits a stale level, so bias grows with lead: +0.06 ct at 0–1h → +0.40 ct
   at 6–12h → ~+1 ct beyond 24h. The average day-over-day move *is* estimable
   from the recorded baselines; it is unknowable only in its daily surprise,
   not in its recent drift.

The two effects add up at the noon spike (both push predicted below actual)
and partially cancel in the morning valley — which is exactly the observed
profile. Queries #3 and #4 in the SQL pack verify each mechanism separately.

### Finding B — the existing bias learning works, but is too coarse

Short-lead bias is nearly zero (+0.06…+0.13 ct at 0–6h) — the per-station
recency-weighted-median correction (`loadPredictionBias`) is doing its job for
the *level*. But it is one scalar per station, learned only from ≤6h leads and
capped at ±3 ct:

- It cannot express "this station is fine at 20:00 but 6 ct low at 12:00" —
  the dominant residual is hour-shaped, not station-shaped.
- The cap (±3 ct) is below the hour-10 residual (+5.87 ct), so even a
  per-hour version of today's mechanism could not fully correct the spike.

### Finding C — confidence labels are mis-calibrated

- **"high" never occurs** (medium + low = 100 % of 381,772 rows). The gate
  `len(sameWeekday) >= 8 && distinctSampleDays >= 5` needs 5 distinct
  occurrences of the same weekday inside the 30-day history window — only
  achievable for at most 2 of 7 weekdays, and holidays/eviction erode even
  those. The tier is effectively dead in production.
- **"low" beats "medium"** on both MAE (3.23 vs 4.19 ct) and bias (+0.28 vs
  +0.77 ct). Sample-count-based confidence is not predictive of realized
  accuracy; if anything it is inverted. (Worth verifying per lead bucket —
  query #7 — since low-confidence rows may cluster at short leads.)

### Finding D — headline metrics over-weight long leads (measurement, not model)

Each hourly `--persist` run re-predicts every future hour up to
`predict_days`, so one target window accumulates ~40+ evaluated rows at leads
from 0 to ~72h (the raw-rows screenshot shows the same station/window from
runs 2h apart). 59 % of all evaluated rows have leads >24h. The overall MAE
of 4.16 ct is therefore mostly a statement about 1–3-day-ahead forecasts;
deduplicated to the *latest* prediction per window (what a user acting on a
fresh suggestion actually experiences) the effective error is much closer to
the 2.6–3.8 ct short-lead range. Query #1 computes both views.

### Finding E — short leads leave value on the table

0–1h MAE of 2.61 ct is high for a quantity that is piecewise-constant and
directly observed: at 0–1h lead, the current price is known from the same
snapshot sweep and German stations change price only a handful of times per
day. A last-price persistence baseline would likely beat 2.61 ct at 0–1h
(query #5 measures it exactly using `price_check_decisions.observed_price`).
The model currently ignores the live price entirely when forecasting.

### Finding F — suggested windows suffer winner's curse (hypothesis)

Suggestions pick the minimum predicted window across ~24×28 candidates.
Selecting the minimum of noisy estimates systematically selects negative
error, so the *displayed* predicted price for chosen windows is optimistic
even when the model is unbiased overall. Query #6 measures bias conditioned
on `is_suggestion = 1`. (The 36.90 ct worst error is also worth eyeballing —
query #8 — likely a midpoint-evaluation artifact around a jump or an outage.)

## 3. Recommended optimizations (ranked by expected impact ÷ effort)

### 3.1 Learn a per-hour bias profile — second self-learning loop (~-0.3…-0.4 ct MAE, kills the noon miss)

Extend the existing closed-loop mechanism with an hour-of-day dimension:

- Correction keyed by `(station, local target hour)` — or market-wide
  `(local hour)` as a fallback tier when a station-hour cell is thin.
- Learned exactly like today's bias: recency-weighted median of
  `error` from evaluated predictions, minimum sample gate, but:
  - include longer leads (the hour-shape error is systematic, visible at all
    leads — restrict to, say, ≤24h rather than ≤6h to keep trend
    contamination bounded);
  - larger cap for the hour dimension (±8 ct) since the residual to correct
    is +5.87 ct at hour 12 local.
- Apply additively in `scoreForecast` after the station-level bias.

This is the same "predict → evaluate → correct" loop already in the codebase,
so it converges rather than compounds, and it automatically adapts if the
market's jump hour moves (the anchor inference already handles that).

### 3.2 Stop shrinking the intraday shape (removes the root cause of A-1)

For offset-mode stations, drop (or re-weight to ~0) the `recent` term — the
level it used to anchor is now carried by `BaselineForecast`, so its only
remaining effect is damping the sawtooth amplitude by 10–25 %. Cheapest
possible change; directly raises predicted peaks and deepens predicted
valleys. Verify against query #3 (bias should stop correlating with hour
offset magnitude) before/after.

### 3.3 Damped baseline drift term (fixes lead-growing bias; ~-0.3…-0.5 ct at >24h leads)

The `baseline` column is already persisted per prediction, and daily
baselines exist in the model. Estimate market drift as the median day-over-day
baseline delta across stations over the last ~7 pricing days, then predict
`baseline + drift × days_ahead × damping` (damping ~0.5–0.7, cap ±2 ct/day).
In the observed window drift was ~+0.6 ct/day — this alone explains most of
the +0.75 ct global bias. A damped estimate reverts safely to ~0 in flat
markets and flips sign in falling ones. Query #4 computes the drift series so
the magnitude can be confirmed before implementing.

### 3.4 Blend in the live price at short leads (~halve 0–1h error)

`predicted = α(lead) × last_observed + (1 − α(lead)) × model`, with α → ~0.8
at 0–1h and → 0 by ~6h. α can itself be *learned from the decision log*:
`price_check_decisions` stores the observed price and the model's prediction
for the same instant, so the optimal mixing weight per lead bucket is a
one-line regression over existing data (query #5 gives the ingredients).
This matters most for `check`/notify ("buy now vs. in 2 hours").

### 3.5 Re-calibrate confidence from measured errors (self-learning, honest UI)

Replace sample-count confidence with empirical calibration: per
`(station, lead bucket)` rolling MAE/quantiles from evaluated predictions,
mapped to labels (e.g. p80 |error| ≤ 2 ct → high, ≤ 4 ct → medium, else low).
This is again the evaluate-and-feed-back pattern, fixes both the dead "high"
tier and the low<medium inversion, and makes the notification gating
("medium/high only") meaningful. Keep sample-count as cold-start fallback.

### 3.6 Measurement fixes (cheap, sharpen every future iteration)

- **Dedupe headline stats** to the latest run per target window (or add a
  lead-bucket filter to the accuracy page default) so the number the UI leads
  with reflects what users act on. Keep the full set for learning.
- **Evaluate against the time-weighted average price** over the target window
  instead of the midpoint price, or exclude windows that contain a detected
  jump crossing from *bias learning* — midpoint sampling at the steepest hour
  of the day both inflates MAE and injects noise into the corrections.
- **Winsorize learning inputs** (e.g. clamp |error| at ~15 ct) so outages and
  artifacts (the 36.9 ct tail) cannot steer corrections; the median is already
  robust, but the per-hour loop above will sometimes run on thin cells.

### 3.7 Correct the suggestion winner's curse (display honesty)

If query #6 confirms optimistic bias on `is_suggestion = 1` rows, add the
measured per-rank correction to the *displayed* predicted price (ranking is
unaffected — the argmin is; only the shown number lies). Alternatively rank by
predicted price + κ·(empirical station-hour error spread) to prefer
confidently-cheap windows over uncertainly-cheap ones.

### 3.8 Larger ideas the data would support (later)

- **Cross-station lead–lag**: with 28 stations, noon raises propagate through
  local competition within minutes–hours; a station's post-jump level is
  partly observable from neighbors that already jumped. Would need a richer
  model than additive corrections.
- **Cross-fuel signal**: e5/e10/diesel move together; a shared market factor
  would stabilize thin per-station estimates.
- **Persist the blend components** (weekday/hour/recent medians) per
  prediction row, then periodically re-fit the 0.60/0.30/0.10 weights per
  lead bucket by ridge regression on evaluated rows — turning the hardcoded
  blend into a slowly self-tuning one.

## 4. Expected combined effect

| Change | Where it acts | Rough MAE effect |
| --- | --- | --- |
| 3.1 per-hour bias loop | hours 10–13 UTC, all leads | −0.3…−0.4 ct overall |
| 3.2 un-shrink shape | peaks/valleys | −0.1…−0.2 ct overall |
| 3.3 drift term | leads >12h | −0.3…−0.5 ct on long leads |
| 3.4 live-price blend | leads <3h | −1.0…−1.5 ct on short leads |
| 3.5 confidence calibration | UI/notifications | quality, not MAE |
| 3.6 measurement fixes | metrics & learning | honesty + cleaner training |

Overall MAE ~4.16 → plausibly ~3.2–3.4 ct, with the headline (deduped,
short-lead) experience improving proportionally more, and bias ≈ 0 across
hours and leads.

## 5. Verification

Run `analysis/prediction-accuracy-queries.sql` against the live MySQL
database (each query is numbered to match the findings above). The queries are
read-only and each is written to complete on the ~400k-row 30-day retention
volume using existing indexes.

> Note: sections 1–4 were produced without live DB access — the sandbox this
> ran in only permits HTTPS egress through a TLS-terminating proxy, which
> cannot carry the MySQL wire protocol (server-greets-first, non-TLS-first).
> The numbers there come from the accuracy UI over the full filtered dataset.
> Section 6 below records the results of running the SQL pack against the
> live database.

## 6. Verification results (queries run 2026-08-03, raw output in results-2026-08-03.txt)

| Hypothesis | Verdict | Evidence |
| --- | --- | --- |
| A-1 shape shrinkage | **Confirmed, strongly** | Query #3: bias is monotone in the hour offset — cells at −3…−7 ct offset show −0.2…−0.35 ct bias, cells at +14…+23 ct offset show +4.7…+20 ct bias. The model captures only part of every peak, proportionally to its height. |
| A noon miss is systematic, not "unknowable future" | **Confirmed** | Query #2: local hour 12 bias is +5.89 / +5.78 / +5.30 / +6.19 ct at 0-1h / 1-6h / 6-24h / >24h leads — flat across leads, i.e. a shape defect the model repeats even one hour ahead. |
| A-2 baseline drift | **Weakened** | Query #4: day-over-day deltas oscillate (−3.6…+3.9 ct/day), mean only ≈ +0.3 ct/day. A damped drift term removes the small systematic long-lead bias (~+0.5 ct) but will not move MAE much. |
| B bias learner works at station level | Confirmed | Query #2: night/morning 0-1h bias ≈ 0, MAE 0.6–1.0 ct. |
| C confidence inversion | **Confirmed (≤24h)** | Query #7: low beats medium at 0-6h (2.20 vs 3.14 ct MAE) and 6-24h (3.14 vs 3.69); only at >24h does medium win narrowly (4.60 vs 4.87). "high" is empty everywhere. |
| D headline stats over-weight long leads | **Confirmed** | Query #1: deduplicated to the latest run per window, MAE drops 4.15 → **2.81 ct**, bias +0.74 → +0.19 ct, within ±2 ct 37.6 % → **59.4 %**. The UI's headline overstates the error a user acting on fresh output experiences. |
| E live-price persistence helps short leads | **Refuted** | Query #5: the model beats naive persistence at every lead, including 0-1h (2.54 vs 3.06 ct). Short-lead error concentrates at jump hours, where persistence is even worse. Recommendation 3.4 is withdrawn in its naive form. |
| F winner's curse on suggestions | **Confirmed** | Query #6: `is_suggestion = 1` rows have bias +2.79 ct vs +0.66 ct for the rest — displayed suggestion prices are ~2–3 ct optimistic. |
| Jump-anchor stability | OK | Query #9: anchor = 12 for 1,223 runs; one brief flip to 0 (15 runs, Jul 23–24). |

New findings from the live data:

1. **The noon spike is weekday-dependent (bimodal).** The worst
   over-predictions (−32…−37 ct: spike predicted, none came) are all Saturday
   Jul 25 / Sunday Jul 26 targets; the worst under-predictions (+31…+34 ct)
   are weekday targets. The per-hour correction should therefore distinguish
   at least weekday vs weekend (the weekday bucket exists in the model but
   carries only 0.60 weight and is thin at 30-day history).
2. **Hour 14 local flips sign with lead** (bias −2.46 ct at 0-1h, +1.8 ct at
   >24h): the spike decays faster than the model's hour profile on the day
   itself. Corrections must be keyed by (local hour × lead bucket), not hour
   alone — the table in query #2 is exactly the correction grid.
3. **Duplicate station identities.** "Kaiser-Tankstelle (Isenstedt)" appears
   as three distinct station ids with identical prices; they triple-count in
   every statistic and split the bias learner's samples three ways. Worth
   deduplicating by coordinates or merging at ingest.
4. **Evening over-prediction at 1-6h leads** (hours 15–19 local: −0.5…−1.6 ct)
   fits the `estimateCurrentBaseline` path inflating the current-day level
   when the post-noon buckets exceed the historical hour offsets — another
   place the (hour × lead) correction grid self-heals.
5. **Decision quality baseline** (query #10): "buy" recommendations averaged
   2.96 ct above the day floor (44 % within 2 ct). A useful KPI to track as
   the model improves.

> **Status:** every item in the priority list below has been implemented on
> this branch — see the "learned corrections" section of the README and the
> `merge-stations` command. The re-measurement baseline is this document's
> numbers; after ~14 days of `suggest --persist` runs on the new model, the
> accuracy page (and query pack) will show whether the corrections converged
> as predicted.

### Updated priority list

1. **Per-(local hour × lead bucket) bias correction loop** — the correction
   grid is query #2 verbatim; recency-weighted median per cell, weekday/
   weekend split at the jump hours, cap ±8 ct. Directly removes the +5.9 ct
   noon residual, the hour-14 sign flip, and the evening 1-6h over-prediction.
2. **Remove the `recent`-term shape shrinkage** for offset-mode stations
   (query #3 is the before/after metric: the bias-vs-offset slope should go
   to ~0).
3. **Dedupe the accuracy page headline** to latest-run-per-window (real
   user-facing MAE is 2.81 ct, not 4.15 ct) and correct the displayed
   suggestion price by the measured +2.8 ct winner's-curse bias.
4. **Confidence recalibration** from rolling empirical error quantiles.
5. **Damped drift term** — small, removes the residual +0.5 ct long-lead bias.
6. **Station identity dedup** (data hygiene; also concentrates learning
   samples).
7. ~~Live-price blend at short leads~~ — withdrawn; the model already beats
   persistence at every lead (query #5).
