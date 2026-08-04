-- Prediction-accuracy verification pack (read-only).
-- Companion to prediction-accuracy-analysis.md; query numbers match the
-- findings/recommendations referenced there.
--
-- Conventions:
--   * All timestamps are stored as RFC3339 UTC strings ("2026-08-03T19:00:00Z"),
--     so hours are extracted with SUBSTRING(ts, 12, 2) and dates with
--     SUBSTRING(ts, 1, 10).
--   * "local_hour" below assumes CEST (UTC+2). In winter change the +2 to +1,
--     or replace with CONVERT_TZ if the server has timezone tables loaded.
--   * Errors are stored in euro; multiply by 100 for cents.
--   * Adjust the fuel filter as needed ('diesel' | 'e5' | 'e10').

-- ---------------------------------------------------------------------------
-- #1  Headline stats: all evaluated rows vs deduplicated to the latest run
--     per (station, target window). The second view is what a user acting on
--     a fresh suggestion experiences; the first over-weights long leads.
-- ---------------------------------------------------------------------------
SELECT 'all rows' AS view_name,
       COUNT(*) AS n,
       ROUND(AVG(ABS(error)) * 100, 2)  AS mae_ct,
       ROUND(AVG(error) * 100, 2)       AS bias_ct,
       ROUND(SQRT(AVG(error * error)) * 100, 2) AS rmse_ct,
       ROUND(100 * AVG(ABS(error) <= 0.01), 1) AS within_1ct_pct,
       ROUND(100 * AVG(ABS(error) <= 0.02), 1) AS within_2ct_pct
FROM price_predictions
WHERE fuel = 'diesel' AND error IS NOT NULL
UNION ALL
SELECT 'latest run per window',
       COUNT(*),
       ROUND(AVG(ABS(pp.error)) * 100, 2),
       ROUND(AVG(pp.error) * 100, 2),
       ROUND(SQRT(AVG(pp.error * pp.error)) * 100, 2),
       ROUND(100 * AVG(ABS(pp.error) <= 0.01), 1),
       ROUND(100 * AVG(ABS(pp.error) <= 0.02), 1)
FROM price_predictions pp
JOIN (
    SELECT station_id, target_start, MAX(run_id) AS run_id
    FROM price_predictions
    WHERE fuel = 'diesel' AND error IS NOT NULL
    GROUP BY station_id, target_start
) latest ON latest.station_id = pp.station_id
        AND latest.target_start = pp.target_start
        AND latest.run_id = pp.run_id
WHERE pp.fuel = 'diesel' AND pp.error IS NOT NULL;

-- ---------------------------------------------------------------------------
-- #2  Bias/MAE by local target hour x lead bucket.
--     Expectation from the UI stats: strong positive bias at local hours
--     12-13 across ALL lead buckets (systematic shape miss, not just
--     unknowable future) and bias growing with lead uniformly (trend drift).
-- ---------------------------------------------------------------------------
SELECT (CAST(SUBSTRING(target_start, 12, 2) AS UNSIGNED) + 2) % 24 AS local_hour,
       CASE
           WHEN lead_minutes <= 60  THEN 'a 0-1h'
           WHEN lead_minutes <= 360 THEN 'b 1-6h'
           WHEN lead_minutes <= 1440 THEN 'c 6-24h'
           ELSE 'd >24h'
       END AS lead_bucket,
       COUNT(*) AS n,
       ROUND(AVG(ABS(error)) * 100, 2) AS mae_ct,
       ROUND(AVG(error) * 100, 2)      AS bias_ct
FROM price_predictions
WHERE fuel = 'diesel' AND error IS NOT NULL
GROUP BY local_hour, lead_bucket
ORDER BY local_hour, lead_bucket;

-- ---------------------------------------------------------------------------
-- #3  Shape-shrinkage test (finding A-1 / recommendation 3.2).
--     For each (station, local hour) cell: bias vs the hour's offset from the
--     station's own mean actual price. If bias grows with the offset
--     (positive at peak hours, negative at valley hours), the blend is
--     damping the intraday shape ("recent" term / median smoothing).
-- ---------------------------------------------------------------------------
WITH sh AS (
    SELECT station_id,
           (CAST(SUBSTRING(target_start, 12, 2) AS UNSIGNED) + 2) % 24 AS local_hour,
           AVG(error) AS bias,
           AVG(actual_price) AS hour_avg,
           COUNT(*) AS n
    FROM price_predictions
    WHERE fuel = 'diesel' AND error IS NOT NULL AND actual_price IS NOT NULL
    GROUP BY station_id, local_hour
),
s AS (
    SELECT station_id, SUM(hour_avg * n) / SUM(n) AS station_avg
    FROM sh GROUP BY station_id
)
SELECT CAST(ROUND((sh.hour_avg - s.station_avg) * 100) AS SIGNED) AS hour_offset_ct,
       COUNT(*) AS cells,
       ROUND(AVG(sh.bias) * 100, 2) AS avg_bias_ct
FROM sh JOIN s ON s.station_id = sh.station_id
GROUP BY hour_offset_ct
ORDER BY hour_offset_ct;

-- ---------------------------------------------------------------------------
-- #4  Day-over-day baseline drift (finding A-2 / recommendation 3.3).
--     Uses the persisted `baseline` column. A consistently positive avg delta
--     (~+0.5 ct/day in the screenshot window) is the level the flat-baseline
--     assumption gives away per day of lead.
-- ---------------------------------------------------------------------------
WITH station_day AS (
    SELECT pp.station_id,
           SUBSTRING(r.run_at, 1, 10) AS run_day,
           AVG(pp.baseline) AS baseline
    FROM price_predictions pp
    JOIN prediction_runs r ON r.id = pp.run_id
    WHERE pp.fuel = 'diesel' AND pp.baseline IS NOT NULL
    GROUP BY pp.station_id, run_day
),
deltas AS (
    SELECT station_id, run_day,
           baseline - LAG(baseline) OVER (PARTITION BY station_id ORDER BY run_day) AS delta
    FROM station_day
)
SELECT run_day,
       COUNT(*) AS stations,
       ROUND(AVG(delta) * 100, 2) AS avg_day_over_day_ct
FROM deltas
WHERE delta IS NOT NULL
GROUP BY run_day
ORDER BY run_day;

-- ---------------------------------------------------------------------------
-- #5  Persistence baseline vs model at each lead (recommendation 3.4).
--     price_check_decisions stores the observed live price at run time for
--     every station, so "just assume the price stays where it is" can be
--     scored against the model on identical targets. If persistence wins at
--     short leads, blend the live price in.
-- ---------------------------------------------------------------------------
SELECT CASE
           WHEN pp.lead_minutes <= 60   THEN 'a 0-1h'
           WHEN pp.lead_minutes <= 180  THEN 'b 1-3h'
           WHEN pp.lead_minutes <= 360  THEN 'c 3-6h'
           WHEN pp.lead_minutes <= 720  THEN 'd 6-12h'
           WHEN pp.lead_minutes <= 1440 THEN 'e 12-24h'
           ELSE 'f >24h'
       END AS lead_bucket,
       COUNT(*) AS n,
       ROUND(AVG(ABS(pp.error)) * 100, 2) AS model_mae_ct,
       ROUND(AVG(ABS(pp.actual_price - d.observed_price)) * 100, 2) AS persistence_mae_ct
FROM price_predictions pp
JOIN price_check_decisions d
  ON d.run_id = pp.run_id AND d.station_id = pp.station_id AND d.fuel = pp.fuel
WHERE pp.fuel = 'diesel'
  AND pp.error IS NOT NULL
  AND pp.actual_price IS NOT NULL
GROUP BY lead_bucket
ORDER BY lead_bucket;

-- ---------------------------------------------------------------------------
-- #6  Winner's curse on suggested windows (recommendation 3.7).
--     If bias for is_suggestion = 1 is clearly more positive than for the
--     rest, the displayed "predicted price" of suggestions is optimistic
--     even though ranking may be fine.
-- ---------------------------------------------------------------------------
SELECT is_suggestion,
       COUNT(*) AS n,
       ROUND(AVG(ABS(error)) * 100, 2) AS mae_ct,
       ROUND(AVG(error) * 100, 2)      AS bias_ct
FROM price_predictions
WHERE fuel = 'diesel' AND error IS NOT NULL
GROUP BY is_suggestion;

-- ---------------------------------------------------------------------------
-- #7  Confidence calibration by lead bucket (finding C / recommendation 3.5).
--     Checks whether "low beats medium" persists within the same lead bucket
--     (i.e. is a real inversion, not a lead-mix artifact), and confirms the
--     "high" tier is empty.
-- ---------------------------------------------------------------------------
SELECT confidence,
       CASE
           WHEN lead_minutes <= 360  THEN 'a 0-6h'
           WHEN lead_minutes <= 1440 THEN 'b 6-24h'
           ELSE 'c >24h'
       END AS lead_bucket,
       COUNT(*) AS n,
       ROUND(AVG(ABS(error)) * 100, 2) AS mae_ct,
       ROUND(AVG(error) * 100, 2)      AS bias_ct
FROM price_predictions
WHERE fuel = 'diesel' AND error IS NOT NULL
GROUP BY confidence, lead_bucket
ORDER BY confidence, lead_bucket;

-- ---------------------------------------------------------------------------
-- #8  Error tail: the 50 worst evaluated predictions with context.
--     Look for: target windows containing the noon jump (midpoint artifact),
--     station outages, or one station dominating the tail.
-- ---------------------------------------------------------------------------
SELECT pp.station_id,
       s.name AS station_name,
       pp.target_start,
       pp.lead_minutes,
       pp.confidence,
       ROUND(pp.predicted_price, 3) AS predicted,
       ROUND(pp.actual_price, 3)    AS actual,
       ROUND(pp.error * 100, 1)     AS error_ct
FROM price_predictions pp
JOIN stations s ON s.id = pp.station_id
WHERE pp.fuel = 'diesel' AND pp.error IS NOT NULL
ORDER BY ABS(pp.error) DESC
LIMIT 50;

-- ---------------------------------------------------------------------------
-- #9  Jump-anchor stability across runs. If the inferred anchor hour
--     flip-flops (e.g. 12 <-> 0), pricing-day baselines get inconsistent and
--     offsets smear — worth knowing before tuning anything else.
-- ---------------------------------------------------------------------------
SELECT jump_anchor_hour,
       COUNT(*) AS runs,
       MIN(run_at) AS first_run,
       MAX(run_at) AS last_run
FROM prediction_runs
GROUP BY jump_anchor_hour
ORDER BY runs DESC;

-- ---------------------------------------------------------------------------
-- #10 Decision quality: signed regret by recommendation. "buy" rows should
--     sit near the day floor (small positive regret); large-regret "buy"s and
--     tiny-regret "wait"s are the actionable failure cases.
-- ---------------------------------------------------------------------------
SELECT recommendation,
       COUNT(*) AS n,
       ROUND(AVG(regret) * 100, 2) AS avg_regret_ct,
       ROUND(100 * AVG(regret <= 0.02), 1) AS within_2ct_of_floor_pct
FROM price_check_decisions
WHERE fuel = 'diesel' AND regret IS NOT NULL
GROUP BY recommendation
ORDER BY recommendation;
