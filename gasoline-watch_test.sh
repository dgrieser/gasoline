#!/usr/bin/env bash

set -euo pipefail

# Pin numeric locale so the `_formatted` price assertions stay deterministic
# regardless of the developer's LANG. Per-test cases may override this to
# exercise locale-specific behavior.
export LC_ALL=C
export LANG=C
export LC_NUMERIC=C

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
source "$SCRIPT_DIR/gasoline-watch.sh"

TEST_DIR=$(mktemp -d)
trap 'rm -rf "$TEST_DIR"' EXIT

CHECK_JSON_FILE="$TEST_DIR/check.json"
SUGGEST_JSON_FILE="$TEST_DIR/suggest.json"
GASOLINE_ARGS_FILE="$TEST_DIR/gasoline.args"
NOTIFY_OUT="$TEST_DIR/notify.out"
export CHECK_JSON_FILE SUGGEST_JSON_FILE GASOLINE_ARGS_FILE NOTIFY_OUT

fail() {
  printf 'gasoline-watch_test: %s\n' "$*" >&2
  exit 1
}

assert_contains() {
  local haystack=$1
  local needle=$2
  local label=$3

  if [[ "$haystack" != *"$needle"* ]]; then
    fail "$label: expected to contain [$needle], got [$haystack]"
  fi
}

assert_not_contains() {
  local haystack=$1
  local needle=$2
  local label=$3

  if [[ "$haystack" == *"$needle"* ]]; then
    fail "$label: expected not to contain [$needle], got [$haystack]"
  fi
}

write_fakes() {
  local fake_gasoline=$TEST_DIR/gasoline
  local fake_notify=$TEST_DIR/notify

  cat >"$fake_gasoline" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$GASOLINE_ARGS_FILE"
case "$1" in
  check)
    cat "$CHECK_JSON_FILE"
    ;;
  suggest)
    cat "$SUGGEST_JSON_FILE"
    ;;
  *)
    exit 42
    ;;
esac
EOF

  cat >"$fake_notify" <<'EOF'
#!/usr/bin/env bash
{
  printf 'BEGIN\n'
  index=0
  for arg in "$@"; do
    index=$((index + 1))
    printf 'ARG%d=%s\n' "$index" "$arg"
  done
  printf 'END\n'
} >>"$NOTIFY_OUT"
EOF

  chmod +x "$fake_gasoline" "$fake_notify"
  GASOLINE_BIN=$fake_gasoline
  FAKE_NOTIFY=$fake_notify
}

write_check_json() {
  cat >"$CHECK_JSON_FILE" <<'EOF'
[
  {
    "station_id": "station-1",
    "station_name": "Station 1",
    "distance_km": 1.2,
    "fuel": "diesel",
    "current_price": 1.7,
    "predicted_current_price": 1.8,
    "verdict": "low",
    "recommendation": "buy",
    "confidence": "medium",
    "sample_count": 6,
    "station": {"address": "Main Street 1", "brand": "TEST"}
  },
  {
    "station_id": "station-low",
    "station_name": "Station Low",
    "distance_km": 1.4,
    "fuel": "diesel",
    "current_price": 1.65,
    "verdict": "low",
    "recommendation": "buy",
    "confidence": "low",
    "station": {"address": "Low Street 1"}
  },
  {
    "station_id": "station-hold",
    "station_name": "Station Hold",
    "distance_km": 0.8,
    "fuel": "diesel",
    "current_price": 1.9,
    "verdict": "typical",
    "recommendation": "hold",
    "confidence": "high",
    "station": {"address": "Hold Street 1"}
  },
  {
    "station_id": "station-2",
    "station_name": "Station 2",
    "distance_km": 2.3,
    "fuel": "diesel",
    "current_price": 1.68,
    "predicted_current_price": 1.79,
    "verdict": "low",
    "recommendation": "buy",
    "confidence": "high",
    "station": {"address": "Second Street 2", "brand": "TEST"}
  }
]
EOF
}

write_suggest_json() {
  cat >"$SUGGEST_JSON_FILE" <<'EOF'
[
  {
    "date": "2026-04-27",
    "weekday": "Monday",
    "start_time": "18:00",
    "end_time": "19:00",
    "station_id": "station-1",
    "station_name": "Station 1",
    "distance_km": 1.2,
    "fuel": "diesel",
    "predicted_price": 1.66,
    "confidence": "high",
    "station": {"address": "Main Street 1", "brand": "TEST"}
  },
  {
    "date": "2026-04-27",
    "weekday": "Monday",
    "start_time": "20:00",
    "end_time": "21:00",
    "station_id": "station-low",
    "station_name": "Station Low",
    "distance_km": 1.4,
    "fuel": "diesel",
    "predicted_price": 1.64,
    "confidence": "low",
    "station": {"address": "Low Street 1"}
  },
  {
    "date": "2026-04-28",
    "weekday": "Tuesday",
    "start_time": "07:00",
    "end_time": "08:00",
    "station_id": "station-2",
    "station_name": "Station 2",
    "distance_km": 2.3,
    "fuel": "diesel",
    "predicted_price": 1.63,
    "confidence": "medium",
    "station": {"address": "Second Street 2", "brand": "TEST"}
  }
]
EOF
}

configure_defaults() {
  CITY=Berlin
  RADIUS_KM=10
  FUEL=diesel
  PREDICT_DAYS=3
  HISTORY_DAYS=21
  CHECK_MINUTES=5
  SUGGEST_TIME=07:30
  RESET_TIME=00:00
  RESET_MINUTES=0
  CHECK_COMMAND="$FAKE_NOTIFY --message {{message}}"
  SUGGEST_COMMAND="$FAKE_NOTIFY --message {{message}}"
  VERBOSE=0
  CHECK_LOWEST_PRICE=""
  LAST_RESET_DATE=$(date +%F)
  : >"$GASOLINE_ARGS_FILE"
  : >"$NOTIFY_OUT"
}

test_check_filters_and_batches_results() {
  configure_defaults
  write_check_json

  run_check_once

  local output args begin_count
  output=$(<"$NOTIFY_OUT")
  args=$(<"$GASOLINE_ARGS_FILE")
  begin_count=$(grep -c '^BEGIN$' "$NOTIFY_OUT")

  [[ "$begin_count" == 1 ]] || fail "expected one check notification, got $begin_count"
  assert_contains "$args" 'check --city Berlin --range-km 10 --fuel diesel --history-days 21 --predict-days 3 --output json' "check args"
  assert_contains "$output" 'ARG1=--message' "check notification"
  assert_contains "$output" 'Buy diesel at Station 1 (1.2 km): 1.700 EUR, confidence medium, verdict low' "check notification"
  assert_contains "$output" 'Buy diesel at Station 2 (2.3 km): 1.680 EUR, confidence high, verdict low' "check notification"
  assert_not_contains "$output" 'Station Low' "check notification"
  assert_not_contains "$output" 'Station Hold' "check notification"
}

test_check_command_row_template_without_message_placeholder() {
  configure_defaults
  write_check_json
  CHECK_COMMAND="$FAKE_NOTIFY --message {{price}} {{fuel}} {{station_name}} {{distance}}"

  run_check_once

  local output
  output=$(<"$NOTIFY_OUT")

  assert_contains "$output" 'ARG1=--message' "row-template command"
  assert_contains "$output" 'ARG2=1.680 diesel Station 2 2.3' "row-template command"
  assert_contains "$output" '1.700 diesel Station 1 1.2' "row-template command"
  assert_not_contains "$output" 'ARG3=' "row-template command"
}

test_check_cheapest_and_count_placeholders() {
  configure_defaults
  write_check_json
  CHECK_COMMAND="$FAKE_NOTIFY --title Cheapest_{{cheapest_price}}_at_{{cheapest_station_name}}_count_{{count}} --message {{message}}"

  run_check_once

  local output
  output=$(<"$NOTIFY_OUT")

  assert_contains "$output" 'ARG1=--title' "cheapest placeholders"
  assert_contains "$output" 'ARG2=Cheapest_1.680_at_Station 2_count_2' "cheapest placeholders"
  assert_contains "$output" 'ARG3=--message' "cheapest placeholders"
  assert_contains "$output" 'Buy diesel at Station 2 (2.3 km): 1.680 EUR' "cheapest placeholders"
  assert_contains "$output" 'Buy diesel at Station 1 (1.2 km): 1.700 EUR' "cheapest placeholders"
}

test_suggest_cheapest_placeholder() {
  configure_defaults
  write_suggest_json
  SUGGEST_COMMAND="$FAKE_NOTIFY --title Cheap_{{cheapest_price}}_{{cheapest_station_name}}_n{{count}} --message {{message}}"

  run_suggest_once

  local output
  output=$(<"$NOTIFY_OUT")

  assert_contains "$output" 'ARG2=Cheap_1.630_Station 2_n2' "suggest cheapest"
}

test_check_sends_only_cheaper_prices() {
  configure_defaults
  write_check_json

  run_check_once
  [[ "$CHECK_LOWEST_PRICE" == "1.680" ]] || fail "expected baseline 1.680 after initial check, got $CHECK_LOWEST_PRICE"
  : >"$NOTIFY_OUT"

  run_check_once
  [[ ! -s "$NOTIFY_OUT" ]] || fail "expected no repeated check notification for unchanged prices"
  [[ "$CHECK_LOWEST_PRICE" == "1.680" ]] || fail "expected baseline unchanged, got $CHECK_LOWEST_PRICE"

  local mutated_json
  mutated_json=$TEST_DIR/check.mutated.json
  jq 'map(
        if .station_id == "station-1" then .current_price = 1.71
        elif .station_id == "station-2" then .current_price = 1.66
        else . end
      )' "$CHECK_JSON_FILE" >"$mutated_json"
  mv "$mutated_json" "$CHECK_JSON_FILE"

  run_check_once

  local output begin_count
  output=$(<"$NOTIFY_OUT")
  begin_count=$(grep -c '^BEGIN$' "$NOTIFY_OUT")

  [[ "$begin_count" == 1 ]] || fail "expected one cheaper-price check notification, got $begin_count"
  assert_contains "$output" 'Buy diesel at Station 2 (2.3 km): 1.660 EUR, confidence high, verdict low' "cheaper-price notification"
  assert_not_contains "$output" 'Station 1' "cheaper-price notification should drop more-expensive station"
  [[ "$CHECK_LOWEST_PRICE" == "1.660" ]] || fail "expected baseline 1.660 after cheaper batch, got $CHECK_LOWEST_PRICE"

  : >"$NOTIFY_OUT"
  jq 'map(
        if .station_id == "station-1" then .current_price = 1.65
        else . end
      )' "$CHECK_JSON_FILE" >"$mutated_json"
  mv "$mutated_json" "$CHECK_JSON_FILE"

  run_check_once

  output=$(<"$NOTIFY_OUT")
  begin_count=$(grep -c '^BEGIN$' "$NOTIFY_OUT")

  [[ "$begin_count" == 1 ]] || fail "expected one notification on new low, got $begin_count"
  assert_contains "$output" 'Buy diesel at Station 1 (1.2 km): 1.650 EUR, confidence medium, verdict low' "new-low notification"
  assert_not_contains "$output" 'Station 2' "new-low notification should drop the previous-baseline station"
  [[ "$CHECK_LOWEST_PRICE" == "1.650" ]] || fail "expected baseline 1.650, got $CHECK_LOWEST_PRICE"
}

test_check_reset_releases_baseline() {
  configure_defaults
  write_check_json

  run_check_once
  [[ "$CHECK_LOWEST_PRICE" == "1.680" ]] || fail "expected baseline 1.680, got $CHECK_LOWEST_PRICE"
  : >"$NOTIFY_OUT"

  run_check_once
  [[ ! -s "$NOTIFY_OUT" ]] || fail "expected no notification before reset"

  CHECK_LOWEST_PRICE=""
  LAST_RESET_DATE=""
  RESET_MINUTES=0
  maybe_reset_baseline
  [[ -z "$CHECK_LOWEST_PRICE" ]] || fail "expected baseline cleared after reset, got $CHECK_LOWEST_PRICE"

  run_check_once

  local output begin_count
  output=$(<"$NOTIFY_OUT")
  begin_count=$(grep -c '^BEGIN$' "$NOTIFY_OUT")

  [[ "$begin_count" == 1 ]] || fail "expected one notification after reset, got $begin_count"
  assert_contains "$output" 'Buy diesel at Station 1 (1.2 km): 1.700 EUR' "post-reset notification"
  assert_contains "$output" 'Buy diesel at Station 2 (2.3 km): 1.680 EUR' "post-reset notification"
  [[ "$CHECK_LOWEST_PRICE" == "1.680" ]] || fail "expected baseline 1.680 after reset re-seed, got $CHECK_LOWEST_PRICE"
}

test_suggest_filters_and_batches_results() {
  configure_defaults
  write_suggest_json

  run_suggest_once

  local output args begin_count
  output=$(<"$NOTIFY_OUT")
  args=$(<"$GASOLINE_ARGS_FILE")
  begin_count=$(grep -c '^BEGIN$' "$NOTIFY_OUT")

  [[ "$begin_count" == 1 ]] || fail "expected one suggest notification, got $begin_count"
  assert_contains "$args" 'suggest --city Berlin --range-km 10 --fuel diesel --history-days 21 --predict-days 3 --output json' "suggest args"
  assert_contains "$output" '2026-04-27 18:00-19:00 diesel at Station 1 (1.2 km): predicted 1.660 EUR, confidence high' "suggest notification"
  assert_contains "$output" '2026-04-28 07:00-08:00 diesel at Station 2 (2.3 km): predicted 1.630 EUR, confidence medium' "suggest notification"
  assert_not_contains "$output" 'Station Low' "suggest notification"
}

test_invalid_json_does_not_notify() {
  configure_defaults
  printf 'not json\n' >"$CHECK_JSON_FILE"

  run_check_once 2>"$TEST_DIR/invalid-json.err"

  [[ ! -s "$NOTIFY_OUT" ]] || fail "expected no notification for invalid JSON"
  assert_contains "$(<"$TEST_DIR/invalid-json.err")" 'check returned invalid JSON' "invalid JSON log"
}

test_verbose_logs_parameters_and_actions() {
  configure_defaults
  write_check_json
  VERBOSE=1

  log_config 2>"$TEST_DIR/verbose.err"
  run_check_once 2>>"$TEST_DIR/verbose.err"

  local output
  output=$(<"$TEST_DIR/verbose.err")

  assert_contains "$output" '[gasoline-watch] city=Berlin' "verbose config"
  assert_contains "$output" '[gasoline-watch] radius_km=10' "verbose config"
  assert_contains "$output" '[gasoline-watch] fuel=diesel' "verbose config"
  assert_contains "$output" '[gasoline-watch] predict_days=3' "verbose config"
  assert_contains "$output" '[gasoline-watch] history_days=21' "verbose config"
  assert_contains "$output" '[gasoline-watch] check_minutes=5' "verbose config"
  assert_contains "$output" '[gasoline-watch] suggest_time=07:30' "verbose config"
  assert_contains "$output" '[gasoline-watch] reset_time=00:00' "verbose config"
  assert_contains "$output" '[gasoline-watch] gasoline_bin=' "verbose config"
  assert_contains "$output" 'running check:' "verbose check"
  assert_contains "$output" 'check --city Berlin --range-km 10 --fuel diesel --history-days 21 --predict-days 3 --output json' "verbose check"
  assert_contains "$output" 'check returned 4 row(s)' "verbose check"
  assert_contains "$output" 'sending 2 cheaper check row(s)' "verbose check"
  assert_contains "$output" 'running check notification:' "verbose notification"
}

test_check_formatted_placeholders() {
  configure_defaults
  write_check_json

  local mutated_json
  mutated_json=$TEST_DIR/check.formatted.json
  jq 'map(
        if .station_id == "station-2" then .current_price = 1.685
        else . end
      )' "$CHECK_JSON_FILE" >"$mutated_json"
  mv "$mutated_json" "$CHECK_JSON_FILE"

  CHECK_COMMAND="$FAKE_NOTIFY --title cheap_{{cheapest_price_formatted}}_{{cheapest_fuel_formatted}} --message {{current_price_formatted}}|{{price_formatted}}|{{predicted_current_price_formatted}}|{{fuel_formatted}}|{{station_name}}"

  run_check_once

  local output
  output=$(<"$NOTIFY_OUT")

  assert_contains "$output" 'ARG2=cheap_1.68_Diesel' "cheapest formatted placeholders"
  assert_contains "$output" '1.68|1.68|1.79|Diesel|Station 2' "truncated formatted row (1.685 -> 1.68)"
  assert_contains "$output" '1.70|1.70|1.80|Diesel|Station 1' "padded formatted row (1.7 -> 1.70)"
}

test_suggest_formatted_placeholders() {
  configure_defaults
  write_suggest_json

  local mutated_json
  mutated_json=$TEST_DIR/suggest.formatted.json
  jq 'map(
        if .station_id == "station-2" then .predicted_price = 1.629
        else . end
      )' "$SUGGEST_JSON_FILE" >"$mutated_json"
  mv "$mutated_json" "$SUGGEST_JSON_FILE"

  SUGGEST_COMMAND="$FAKE_NOTIFY --title cheap_{{cheapest_price_formatted}}_{{cheapest_fuel_formatted}} --message {{predicted_price_formatted}}|{{price_formatted}}|{{fuel_formatted}}|{{station_name}}"

  run_suggest_once

  local output
  output=$(<"$NOTIFY_OUT")

  assert_contains "$output" 'ARG2=cheap_1.62_Diesel' "suggest cheapest formatted placeholders"
  assert_contains "$output" '1.62|1.62|Diesel|Station 2' "suggest truncated (1.629 -> 1.62)"
  assert_contains "$output" '1.66|1.66|Diesel|Station 1' "suggest padded (1.66 -> 1.66)"
}

test_formatted_uses_locale_decimal_separator() {
  configure_defaults
  write_check_json

  local mutated_json
  mutated_json=$TEST_DIR/check.locale.json
  jq 'map(
        if .station_id == "station-2" then .current_price = 1.685
        else . end
      )' "$CHECK_JSON_FILE" >"$mutated_json"
  mv "$mutated_json" "$CHECK_JSON_FILE"

  # Stub `locale -k decimal_point` so the test does not depend on a German
  # locale actually being installed on the host. The stub is shadowed via PATH.
  local stub_dir=$TEST_DIR/locale_stub
  mkdir -p "$stub_dir"
  cat >"$stub_dir/locale" <<'EOF'
#!/usr/bin/env bash
if [[ "$1" == "-k" && "$2" == "decimal_point" ]]; then
  printf 'decimal_point=","\n'
  exit 0
fi
exec /usr/bin/env -i PATH="/usr/bin:/bin" locale "$@"
EOF
  chmod +x "$stub_dir/locale"

  CHECK_COMMAND="$FAKE_NOTIFY --message {{current_price_formatted}}|{{predicted_current_price_formatted}}|{{station_name}}"

  local saved_path=$PATH saved_sep=$LOCALE_DECIMAL_SEP
  LOCALE_DECIMAL_SEP=""
  PATH="$stub_dir:$PATH"
  run_check_once
  PATH=$saved_path
  LOCALE_DECIMAL_SEP=$saved_sep

  local output
  output=$(<"$NOTIFY_OUT")

  assert_contains "$output" '1,68|1,79|Station 2' "locale decimal sep (1.685 -> 1,68)"
  assert_contains "$output" '1,70|1,80|Station 1' "locale decimal sep (1.7 -> 1,70)"
}

# Repros the reported bug: a comma-formatted scalar placeholder sitting
# inside a user-supplied "..." title was rendering as 1\,88 because
# `printf '%q'` backslash-escapes the comma and the backslash survives
# inside double quotes.
test_locale_scalar_inside_quoted_title() {
  configure_defaults
  write_check_json

  local stub_dir=$TEST_DIR/locale_stub_scalar
  mkdir -p "$stub_dir"
  cat >"$stub_dir/locale" <<'EOF'
#!/usr/bin/env bash
if [[ "$1" == "-k" && "$2" == "decimal_point" ]]; then
  printf 'decimal_point=","\n'
  exit 0
fi
exec /usr/bin/env -i PATH="/usr/bin:/bin" locale "$@"
EOF
  chmod +x "$stub_dir/locale"

  CHECK_COMMAND="$FAKE_NOTIFY \"Tanken für {{cheapest_price_formatted}}\" {{current_price_formatted}} EUR {{fuel_formatted}}"

  local saved_path=$PATH saved_sep=$LOCALE_DECIMAL_SEP
  LOCALE_DECIMAL_SEP=""
  PATH="$stub_dir:$PATH"
  run_check_once
  PATH=$saved_path
  LOCALE_DECIMAL_SEP=$saved_sep

  local output
  output=$(<"$NOTIFY_OUT")

  assert_contains "$output" 'ARG1=Tanken für 1,68' "scalar in quoted title keeps literal comma"
  assert_not_contains "$output" 'Tanken für 1\,68' "no backslash escape leaking into title"
  assert_contains "$output" '1,68 EUR Diesel' "row placeholder keeps literal comma (row 1)"
  assert_contains "$output" '1,70 EUR Diesel' "row placeholder keeps literal comma (row 2)"
}

test_compute_sleep() {
  configure_defaults
  CHECK_MINUTES=10
  SUGGEST_MINUTES=725  # 12:05
  LAST_CHECK_EPOCH=0
  LAST_SUGGEST_DATE=""

  local now_epoch now_date
  now_epoch=$(date +%s)
  now_date=$(date +%F)

  # Default max sleep when nothing is imminent
  LAST_SUGGEST_DATE=$now_date  # suggest already done today
  local s
  s=$(compute_sleep 600 "$now_epoch")
  [[ "$s" -le 600 ]] || fail "compute_sleep: expected <= 600, got $s"
  [[ "$s" -ge 1 ]] || fail "compute_sleep: expected >= 1, got $s"

  # Check interval shorter than max sleep
  LAST_CHECK_EPOCH=$((now_epoch - 1))  # checked 1 second ago
  s=$(compute_sleep 600 "$now_epoch")
  local expected_max=$(( CHECK_MINUTES * 60 - 1 ))
  [[ "$s" -le "$expected_max" ]] || fail "compute_sleep: check cap: expected <= $expected_max, got $s"

  # Suggest time coming up soon: fake SUGGEST_MINUTES = now+2 minutes
  LAST_CHECK_EPOCH=0
  LAST_SUGGEST_DATE=""
  local now_hour now_min now_minutes
  now_hour=$(date +%H)
  now_min=$(date +%M)
  now_minutes=$((10#$now_hour * 60 + 10#$now_min))
  SUGGEST_MINUTES=$((now_minutes + 2))
  s=$(compute_sleep 600 "$now_epoch")
  [[ "$s" -le 120 ]] || fail "compute_sleep: suggest cap: expected <= 120, got $s"
  [[ "$s" -ge 1 ]] || fail "compute_sleep: suggest cap: expected >= 1, got $s"
}

write_fakes
test_compute_sleep
test_check_filters_and_batches_results
test_check_command_row_template_without_message_placeholder
test_check_cheapest_and_count_placeholders
test_suggest_cheapest_placeholder
test_check_sends_only_cheaper_prices
test_check_reset_releases_baseline
test_suggest_filters_and_batches_results
test_invalid_json_does_not_notify
test_verbose_logs_parameters_and_actions
test_check_formatted_placeholders
test_suggest_formatted_placeholders
test_formatted_uses_locale_decimal_separator
test_locale_scalar_inside_quoted_title

printf 'gasoline-watch_test: ok\n'
