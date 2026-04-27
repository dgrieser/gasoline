#!/usr/bin/env bash

set -u
set -o pipefail

PLACEHOLDERS=(
  address
  best_future_date
  best_future_end_time
  best_future_price
  best_future_start_time
  best_future_weekday
  brand
  confidence
  current_price
  date
  distance
  distance_km
  end_time
  expected_drop
  expected_lower
  first_seen_at
  fuel
  history_percentile
  house_number
  lat
  lng
  place
  post_code
  predicted_current_price
  predicted_price
  price
  recommendation
  recorded_at
  sample_count
  start_time
  station_id
  station_name
  street
  verdict
  weekday
)

CHECK_ROW_TEMPLATE='Buy {{fuel}} at {{station_name}} ({{distance}} km): {{current_price}} EUR, confidence {{confidence}}, verdict {{verdict}}'
SUGGEST_ROW_TEMPLATE='{{date}} {{start_time}}-{{end_time}} {{fuel}} at {{station_name}} ({{distance}} km): predicted {{predicted_price}} EUR, confidence {{confidence}}'

CITY=""
RADIUS_KM=""
FUEL=""
PREDICT_DAYS=""
HISTORY_DAYS=""
CHECK_MINUTES=""
SUGGEST_TIME=""
CHECK_COMMAND=""
SUGGEST_COMMAND=""
GASOLINE_BIN="${GASOLINE_BIN:-}"
SUGGEST_MINUTES=0
LAST_CHECK_EPOCH=0
LAST_SUGGEST_DATE=""
declare -A CHECK_ALERT_PRICES=()
FILTERED_ROWS=()

usage() {
  cat <<'EOF'
Usage:
  gasoline-watch.sh --city CITY --radius-km KM --fuel diesel|e5|e10 \
    --predict-days DAYS --history-days DAYS --check-minutes MINUTES \
    --suggest-time HH:MM --check-command COMMAND --suggest-command COMMAND

Commands are notification templates. Use {{message}} for the formatted
multiline message, for example:

  --check-command 'notify --message {{message}}'

If {{message}} is omitted, the script treats everything from the first row
placeholder onward as the per-result message format, for example:

  --check-command 'notify --message {{price}} {{fuel}} {{station_name}} {{distance}}'
EOF
}

log() {
  printf '[gasoline-watch] %s\n' "$*" >&2
}

die() {
  log "$*"
  exit 2
}

need_value() {
  local name=$1
  local value=${2-}
  if [[ -z "$value" || "$value" == --* ]]; then
    die "$name requires a value"
  fi
}

parse_args() {
  while (($# > 0)); do
    case "$1" in
      --city)
        need_value "$1" "${2-}"
        CITY=$2
        shift 2
        ;;
      --radius-km)
        need_value "$1" "${2-}"
        RADIUS_KM=$2
        shift 2
        ;;
      --fuel)
        need_value "$1" "${2-}"
        FUEL=$2
        shift 2
        ;;
      --predict-days)
        need_value "$1" "${2-}"
        PREDICT_DAYS=$2
        shift 2
        ;;
      --history-days)
        need_value "$1" "${2-}"
        HISTORY_DAYS=$2
        shift 2
        ;;
      --check-minutes)
        need_value "$1" "${2-}"
        CHECK_MINUTES=$2
        shift 2
        ;;
      --suggest-time)
        need_value "$1" "${2-}"
        SUGGEST_TIME=$2
        shift 2
        ;;
      --check-command)
        need_value "$1" "${2-}"
        CHECK_COMMAND=$2
        shift 2
        ;;
      --suggest-command)
        need_value "$1" "${2-}"
        SUGGEST_COMMAND=$2
        shift 2
        ;;
      -h|--help)
        usage
        exit 0
        ;;
      *)
        die "unknown argument: $1"
        ;;
    esac
  done
}

is_positive_int() {
  [[ "$1" =~ ^[1-9][0-9]*$ ]]
}

is_positive_number() {
  [[ "$1" =~ ^[0-9]+([.][0-9]+)?$ ]] && [[ ! "$1" =~ ^0+([.]0+)?$ ]]
}

validate_args() {
  [[ -n "$CITY" ]] || die "missing --city"
  [[ -n "$RADIUS_KM" ]] || die "missing --radius-km"
  [[ -n "$FUEL" ]] || die "missing --fuel"
  [[ -n "$PREDICT_DAYS" ]] || die "missing --predict-days"
  [[ -n "$HISTORY_DAYS" ]] || die "missing --history-days"
  [[ -n "$CHECK_MINUTES" ]] || die "missing --check-minutes"
  [[ -n "$SUGGEST_TIME" ]] || die "missing --suggest-time"
  [[ -n "$CHECK_COMMAND" ]] || die "missing --check-command"
  [[ -n "$SUGGEST_COMMAND" ]] || die "missing --suggest-command"

  is_positive_number "$RADIUS_KM" || die "--radius-km must be a positive number"
  [[ "$FUEL" =~ ^(diesel|e5|e10)$ ]] || die "--fuel must be one of: diesel, e5, e10"
  is_positive_int "$PREDICT_DAYS" || die "--predict-days must be a positive integer"
  is_positive_int "$HISTORY_DAYS" || die "--history-days must be a positive integer"
  is_positive_int "$CHECK_MINUTES" || die "--check-minutes must be a positive integer"
  [[ "$SUGGEST_TIME" =~ ^([01][0-9]|2[0-3]):[0-5][0-9]$ ]] || die "--suggest-time must be HH:MM"

  local hour minute
  hour=${SUGGEST_TIME%:*}
  minute=${SUGGEST_TIME#*:}
  SUGGEST_MINUTES=$((10#$hour * 60 + 10#$minute))
}

require_tools() {
  command -v jq >/dev/null 2>&1 || die "jq is required"
  if [[ -z "$GASOLINE_BIN" ]]; then
    if [[ -x ./gasoline ]]; then
      GASOLINE_BIN=./gasoline
    else
      GASOLINE_BIN=gasoline
    fi
  fi
  command -v "$GASOLINE_BIN" >/dev/null 2>&1 || die "gasoline command not found: $GASOLINE_BIN"
}

shell_quote() {
  printf '%q' "$1"
}

contains_row_placeholder() {
  local template=$1
  local key
  for key in "${PLACEHOLDERS[@]}"; do
    if [[ "$template" == *"{{${key}}}"* ]]; then
      return 0
    fi
  done
  return 1
}

jq_value() {
  local row=$1
  local expr=$2
  local value

  value=$(printf '%s' "$row" | jq -r "$expr | if . == null then \"\" else tostring end" 2>/dev/null) || value=""
  printf '%s' "$value"
}

number_value() {
  local row=$1
  local expr=$2
  local decimals=$3
  local value

  value=$(jq_value "$row" "$expr")
  if [[ -z "$value" ]]; then
    return 0
  fi
  if [[ "$value" =~ ^-?[0-9]+([.][0-9]+)?$ ]]; then
    LC_ALL=C printf "%.${decimals}f" "$value"
  else
    printf '%s' "$value"
  fi
}

row_value() {
  local kind=$1
  local row=$2
  local key=$3

  case "$key" in
    address) jq_value "$row" '.station.address // .address // ""' ;;
    best_future_date) jq_value "$row" '.best_future_date // ""' ;;
    best_future_end_time) jq_value "$row" '.best_future_end_time // ""' ;;
    best_future_price) number_value "$row" '.best_future_price // ""' 3 ;;
    best_future_start_time) jq_value "$row" '.best_future_start_time // ""' ;;
    best_future_weekday) jq_value "$row" '.best_future_weekday // ""' ;;
    brand) jq_value "$row" '.station.brand // .brand // ""' ;;
    confidence) jq_value "$row" '.confidence // ""' ;;
    current_price) number_value "$row" '.current_price // ""' 3 ;;
    date)
      if [[ "$kind" == check ]]; then
        jq_value "$row" '.best_future_date // .recorded_at // ""'
      else
        jq_value "$row" '.date // ""'
      fi
      ;;
    distance|distance_km) number_value "$row" '.distance_km // .station.distance_km // ""' 1 ;;
    end_time)
      if [[ "$kind" == check ]]; then
        jq_value "$row" '.best_future_end_time // ""'
      else
        jq_value "$row" '.end_time // ""'
      fi
      ;;
    expected_drop) number_value "$row" '.expected_drop // ""' 3 ;;
    expected_lower) jq_value "$row" '.expected_lower // ""' ;;
    first_seen_at) jq_value "$row" '.station.first_seen_at // ""' ;;
    fuel) jq_value "$row" '.fuel // ""' ;;
    history_percentile) number_value "$row" '.history_percentile // ""' 1 ;;
    house_number) jq_value "$row" '.station.house_number // ""' ;;
    lat) number_value "$row" '.station.lat // ""' 6 ;;
    lng) number_value "$row" '.station.lng // ""' 6 ;;
    place) jq_value "$row" '.station.place // ""' ;;
    post_code) jq_value "$row" '.station.post_code // ""' ;;
    predicted_current_price) number_value "$row" '.predicted_current_price // ""' 3 ;;
    predicted_price) number_value "$row" '.predicted_price // .predicted_current_price // ""' 3 ;;
    price)
      if [[ "$kind" == check ]]; then
        number_value "$row" '.current_price // ""' 3
      else
        number_value "$row" '.predicted_price // ""' 3
      fi
      ;;
    recommendation) jq_value "$row" '.recommendation // ""' ;;
    recorded_at) jq_value "$row" '.recorded_at // ""' ;;
    sample_count) jq_value "$row" '.sample_count // ""' ;;
    start_time)
      if [[ "$kind" == check ]]; then
        jq_value "$row" '.best_future_start_time // ""'
      else
        jq_value "$row" '.start_time // ""'
      fi
      ;;
    station_id) jq_value "$row" '.station_id // .station.id // ""' ;;
    station_name) jq_value "$row" '.station_name // .station.name // ""' ;;
    street) jq_value "$row" '.station.street // ""' ;;
    verdict) jq_value "$row" '.verdict // ""' ;;
    weekday)
      if [[ "$kind" == check ]]; then
        jq_value "$row" '.best_future_weekday // ""'
      else
        jq_value "$row" '.weekday // ""'
      fi
      ;;
    *) printf '' ;;
  esac
}

render_row_template() {
  local template=$1
  local kind=$2
  local row=$3
  local rendered=$template
  local key value

  for key in "${PLACEHOLDERS[@]}"; do
    value=$(row_value "$kind" "$row" "$key")
    rendered=${rendered//\{\{$key\}\}/$value}
  done
  printf '%s' "$rendered"
}

build_message() {
  local kind=$1
  local row_template=$2
  shift 2

  local message=""
  local row line
  for row in "$@"; do
    line=$(render_row_template "$row_template" "$kind" "$row")
    if [[ -z "$message" ]]; then
      message=$line
    else
      message+=$'\n'"$line"
    fi
  done
  printf '%s' "$message"
}

build_value_lines() {
  local kind=$1
  local key=$2
  shift 2

  local lines=""
  local row value
  for row in "$@"; do
    value=$(row_value "$kind" "$row" "$key")
    if [[ -z "$lines" ]]; then
      lines=$value
    else
      lines+=$'\n'"$value"
    fi
  done
  printf '%s' "$lines"
}

build_notification_command() {
  local template=$1
  local kind=$2
  local message=$3
  shift 3

  local command=$template
  local quoted key values prefix row_template

  if [[ "$command" == *"{{message}}"* ]]; then
    quoted=$(shell_quote "$message")
    command=${command//\{\{message\}\}/$quoted}
    for key in "${PLACEHOLDERS[@]}"; do
      if [[ "$command" == *"{{${key}}}"* ]]; then
        values=$(build_value_lines "$kind" "$key" "$@")
        quoted=$(shell_quote "$values")
        command=${command//\{\{$key\}\}/$quoted}
      fi
    done
    printf '%s' "$command"
    return 0
  fi

  if contains_row_placeholder "$command"; then
    prefix=${command%%\{\{*}
    row_template=${command#"$prefix"}
    message=$(build_message "$kind" "$row_template" "$@")
    printf '%s%s' "$prefix" "$(shell_quote "$message")"
    return 0
  fi

  printf '%s %s' "$command" "$(shell_quote "$message")"
}

run_notification() {
  local kind=$1
  local template=$2
  local message=$3
  shift 3

  local command
  command=$(build_notification_command "$template" "$kind" "$message" "$@")
  if ! bash -c "$command"; then
    log "$kind notification command failed"
  fi
}

send_matching_rows() {
  local kind=$1
  local command_template=$2
  local json=$3
  local jq_filter=$4
  local row_template=$5
  local rows=()

  mapfile -t rows < <(printf '%s' "$json" | jq -c "$jq_filter")
  if ((${#rows[@]} == 0)); then
    return 0
  fi

  local message
  message=$(build_message "$kind" "$row_template" "${rows[@]}")
  run_notification "$kind" "$command_template" "$message" "${rows[@]}"
}

check_alert_key() {
  local row=$1
  local station_id fuel

  station_id=$(row_value check "$row" station_id)
  fuel=$(row_value check "$row" fuel)
  if [[ -z "$station_id" ]]; then
    station_id=$(row_value check "$row" station_name)
  fi
  printf '%s|%s' "$station_id" "$fuel"
}

filter_changed_check_rows() {
  local rows=("$@")
  local row key price

  FILTERED_ROWS=()
  for row in "${rows[@]}"; do
    key=$(check_alert_key "$row")
    price=$(row_value check "$row" current_price)
    if [[ -z "$key" || -z "$price" ]]; then
      continue
    fi
    if [[ "${CHECK_ALERT_PRICES[$key]+set}" == set && "${CHECK_ALERT_PRICES[$key]}" == "$price" ]]; then
      continue
    fi
    CHECK_ALERT_PRICES[$key]=$price
    FILTERED_ROWS+=("$row")
  done
}

send_changed_check_rows() {
  local json=$1
  local rows=()

  mapfile -t rows < <(printf '%s' "$json" | jq -c 'if type == "array" then .[] else empty end | select(.recommendation == "buy" and (.confidence == "medium" or .confidence == "high"))')
  if ((${#rows[@]} == 0)); then
    return 0
  fi

  filter_changed_check_rows "${rows[@]}"
  if ((${#FILTERED_ROWS[@]} == 0)); then
    return 0
  fi

  local message
  message=$(build_message check "$CHECK_ROW_TEMPLATE" "${FILTERED_ROWS[@]}")
  run_notification check "$CHECK_COMMAND" "$message" "${FILTERED_ROWS[@]}"
}

run_check_once() {
  local output
  if ! output=$("$GASOLINE_BIN" check \
      --city "$CITY" \
      --range-km "$RADIUS_KM" \
      --fuel "$FUEL" \
      --history-days "$HISTORY_DAYS" \
      --predict-days "$PREDICT_DAYS" \
      --output json 2>&1); then
    log "check failed: $output"
    return 0
  fi

  if ! printf '%s' "$output" | jq -e 'type == "array"' >/dev/null 2>&1; then
    log "check returned invalid JSON: $output"
    return 0
  fi

  send_changed_check_rows "$output"
}

run_suggest_once() {
  local output
  if ! output=$("$GASOLINE_BIN" suggest \
      --city "$CITY" \
      --range-km "$RADIUS_KM" \
      --fuel "$FUEL" \
      --history-days "$HISTORY_DAYS" \
      --predict-days "$PREDICT_DAYS" \
      --output json 2>&1); then
    log "suggest failed: $output"
    return 0
  fi

  if ! printf '%s' "$output" | jq -e 'type == "array"' >/dev/null 2>&1; then
    log "suggest returned invalid JSON: $output"
    return 0
  fi

  send_matching_rows \
    suggest \
    "$SUGGEST_COMMAND" \
    "$output" \
    'if type == "array" then .[] else empty end | select(.confidence == "medium" or .confidence == "high")' \
    "$SUGGEST_ROW_TEMPLATE"
}

maybe_run_suggest() {
  local now_date now_hour now_min now_minutes

  now_date=$(date +%F)
  now_hour=$(date +%H)
  now_min=$(date +%M)
  now_minutes=$((10#$now_hour * 60 + 10#$now_min))

  if ((now_minutes >= SUGGEST_MINUTES)) && [[ "$LAST_SUGGEST_DATE" != "$now_date" ]]; then
    run_suggest_once
    LAST_SUGGEST_DATE=$now_date
  fi
}

compute_sleep() {
  local max_sleep=$1
  local now_epoch=$2
  local sleep=$max_sleep

  local next_check_in
  if ((LAST_CHECK_EPOCH == 0)); then
    next_check_in=$((CHECK_MINUTES * 60))
  else
    next_check_in=$((CHECK_MINUTES * 60 - (now_epoch - LAST_CHECK_EPOCH)))
  fi
  if ((next_check_in > 0 && next_check_in < sleep)); then
    sleep=$next_check_in
  fi

  local now_date now_hour now_min now_sec now_minutes suggest_in
  now_date=$(date +%F)
  now_hour=$(date +%H)
  now_min=$(date +%M)
  now_sec=$(date +%S)
  now_minutes=$((10#$now_hour * 60 + 10#$now_min))

  if [[ "$LAST_SUGGEST_DATE" != "$now_date" ]] && ((now_minutes < SUGGEST_MINUTES)); then
    suggest_in=$(( (SUGGEST_MINUTES - now_minutes) * 60 - 10#$now_sec ))
    if ((suggest_in > 0 && suggest_in < sleep)); then
      sleep=$suggest_in
    fi
  fi

  if ((sleep < 1)); then
    sleep=1
  fi

  printf '%d' "$sleep"
}

main_loop() {
  local max_sleep=${GASOLINE_WATCH_SLEEP_SECONDS:-600}
  is_positive_int "$max_sleep" || die "GASOLINE_WATCH_SLEEP_SECONDS must be a positive integer"

  while true; do
    local now_epoch
    now_epoch=$(date +%s)
    if ((LAST_CHECK_EPOCH == 0 || now_epoch - LAST_CHECK_EPOCH >= CHECK_MINUTES * 60)); then
      run_check_once
      LAST_CHECK_EPOCH=$(date +%s)
    fi

    maybe_run_suggest

    now_epoch=$(date +%s)
    local sleep_seconds
    sleep_seconds=$(compute_sleep "$max_sleep" "$now_epoch")
    sleep "$sleep_seconds"
  done
}

main() {
  parse_args "$@"
  validate_args
  require_tools
  main_loop
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
