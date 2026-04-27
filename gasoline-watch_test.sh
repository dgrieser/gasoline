#!/usr/bin/env bash

set -euo pipefail

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
  CHECK_COMMAND="$FAKE_NOTIFY --message {{message}}"
  SUGGEST_COMMAND="$FAKE_NOTIFY --message {{message}}"
  CHECK_ALERT_PRICES=()
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
  assert_contains "$output" 'ARG2=1.700 diesel Station 1 1.2' "row-template command"
  assert_contains "$output" '1.680 diesel Station 2 2.3' "row-template command"
  assert_not_contains "$output" 'ARG3=' "row-template command"
}

test_check_sends_only_changed_station_prices() {
  configure_defaults
  write_check_json

  run_check_once
  : >"$NOTIFY_OUT"

  run_check_once
  [[ ! -s "$NOTIFY_OUT" ]] || fail "expected no repeated check notification for unchanged prices"

  local changed_json
  changed_json=$TEST_DIR/check.changed.json
  jq 'map(if .station_id == "station-1" then .current_price = 1.71 else . end)' "$CHECK_JSON_FILE" >"$changed_json"
  mv "$changed_json" "$CHECK_JSON_FILE"

  run_check_once

  local output begin_count
  output=$(<"$NOTIFY_OUT")
  begin_count=$(grep -c '^BEGIN$' "$NOTIFY_OUT")

  [[ "$begin_count" == 1 ]] || fail "expected one changed-price check notification, got $begin_count"
  assert_contains "$output" 'Buy diesel at Station 1 (1.2 km): 1.710 EUR, confidence medium, verdict low' "changed-price notification"
  assert_not_contains "$output" 'Station 2' "changed-price notification"
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

write_fakes
test_check_filters_and_batches_results
test_check_command_row_template_without_message_placeholder
test_check_sends_only_changed_station_prices
test_suggest_filters_and_batches_results
test_invalid_json_does_not_notify

printf 'gasoline-watch_test: ok\n'
