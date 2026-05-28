#!/usr/bin/env bash

set -u
set -o pipefail

PLACEHOLDERS=(
  address
  best_future_date
  best_future_end_time
  best_future_price
  best_future_price_formatted
  best_future_start_time
  best_future_weekday
  best_future_weekday_formatted
  best_future_weekday_short
  best_future_weekday_short_formatted
  brand
  confidence
  current_price
  current_price_formatted
  date
  distance
  distance_km
  end_time
  expected_drop
  expected_lower
  first_seen_at
  fuel
  fuel_formatted
  history_percentile
  house_number
  lat
  lng
  place
  post_code
  predicted_current_price
  predicted_current_price_formatted
  predicted_price
  predicted_price_formatted
  price
  price_formatted
  recommendation
  recorded_at
  sample_count
  start_time
  station_id
  station_name
  street
  verdict
  weekday
  weekday_formatted
  weekday_short
  weekday_short_formatted
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
RESET_TIME=""
CHECK_COMMAND=""
SUGGEST_COMMAND=""
GASOLINE_BIN="${GASOLINE_BIN:-}"
VERBOSE=0
SUGGEST_MINUTES=0
RESET_MINUTES=0
LAST_CHECK_EPOCH=0
LAST_SUGGEST_DATE=""
LAST_RESET_DATE=""
CHECK_LOWEST_PRICE=""
FILTERED_ROWS=()

usage() {
  cat <<'EOF'
Usage:
  gasoline-watch.sh --city CITY --radius-km KM --fuel diesel|e5|e10 \
    --predict-days DAYS --history-days DAYS --check-minutes MINUTES \
    --suggest-time HH:MM [--reset-time HH:MM] \
    --check-command COMMAND --suggest-command COMMAND \
    [--verbose]

--reset-time defaults to 00:00. The watcher only emits a check
notification when a buy recommendation is strictly cheaper than the
lowest price reported since the last reset.

Commands are notification templates. Use {{message}} for the formatted
multiline message, for example:

  --check-command 'notify --message {{message}}'

If {{message}} is omitted, the script treats everything from the first row
placeholder onward as the per-result message format, for example:

  --check-command 'notify --message {{price}} {{fuel}} {{station_name}} {{distance}}'

Rows are sorted ascending by price, so the first row is the cheapest. Use
{{cheapest_<field>}} (e.g. {{cheapest_price}}, {{cheapest_station_name}})
to interpolate a single scalar from the cheapest row, and {{count}} for the
number of rows in the notification. These are substituted once and are
useful for non-repeating titles, for example:

  --check-command 'pushover gasoline "Cheapest {{cheapest_price}} EUR ({{count}} stations)" {{message}}'

Like every other placeholder, scalar values are shell-quoted on substitution,
so keep placeholders at argument boundaries rather than wrapping them in
extra quotes when the value may contain whitespace.
EOF
}

log() {
  printf '[gasoline-watch] %s\n' "$*" >&2
}

verbose_log() {
  if ((VERBOSE)); then
    log "$*"
  fi
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
      --reset-time)
        need_value "$1" "${2-}"
        RESET_TIME=$2
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
      --verbose)
        VERBOSE=1
        shift
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

  if [[ -z "$RESET_TIME" ]]; then
    RESET_TIME="00:00"
  fi
  [[ "$RESET_TIME" =~ ^([01][0-9]|2[0-3]):[0-5][0-9]$ ]] || die "--reset-time must be HH:MM"

  local hour minute
  hour=${SUGGEST_TIME%:*}
  minute=${SUGGEST_TIME#*:}
  SUGGEST_MINUTES=$((10#$hour * 60 + 10#$minute))
  hour=${RESET_TIME%:*}
  minute=${RESET_TIME#*:}
  RESET_MINUTES=$((10#$hour * 60 + 10#$minute))
}

require_tools() {
  command -v jq >/dev/null 2>&1 || die "jq is required"
  command -v awk >/dev/null 2>&1 || die "awk is required"
  if [[ -z "$GASOLINE_BIN" ]]; then
    if [[ -x ./gasoline ]]; then
      GASOLINE_BIN=./gasoline
    else
      GASOLINE_BIN=gasoline
    fi
  fi
  command -v "$GASOLINE_BIN" >/dev/null 2>&1 || die "gasoline command not found: $GASOLINE_BIN"
}

log_config() {
  verbose_log "city=$(shell_quote "$CITY")"
  verbose_log "radius_km=$(shell_quote "$RADIUS_KM")"
  verbose_log "fuel=$(shell_quote "$FUEL")"
  verbose_log "predict_days=$(shell_quote "$PREDICT_DAYS")"
  verbose_log "history_days=$(shell_quote "$HISTORY_DAYS")"
  verbose_log "check_minutes=$(shell_quote "$CHECK_MINUTES")"
  verbose_log "suggest_time=$(shell_quote "$SUGGEST_TIME")"
  verbose_log "reset_time=$(shell_quote "$RESET_TIME")"
  verbose_log "check_command=$(shell_quote "$CHECK_COMMAND")"
  verbose_log "suggest_command=$(shell_quote "$SUGGEST_COMMAND")"
  verbose_log "gasoline_bin=$(shell_quote "$GASOLINE_BIN")"
}

shell_quote() {
  local s=${1-}
  if [[ -z "$s" ]]; then
    printf "''"
    return 0
  fi
  # Pass through unquoted when every byte is shell-inert. Avoids bash's
  # `printf '%q'` escaping locale-printable bytes like the German decimal
  # comma (1,88 -> 1\,88) — the backslash survived inside user templates that
  # wrap a placeholder in double quotes, leaking as a literal "1\,88".
  if [[ "$s" != *[!A-Za-z0-9._,/:+=@%-]* ]]; then
    printf '%s' "$s"
    return 0
  fi
  # Otherwise single-quote, splicing in any embedded single quotes the
  # POSIX-portable way ('  ->  '\''  ).
  local escaped=${s//\'/\'\\\'\'}
  printf "'%s'" "$escaped"
}

contains_row_placeholder() {
  local template=$1
  local key
  for key in "${PLACEHOLDERS[@]}"; do
    if [[ "$template" == *"{{${key}}}"* ]]; then
      return 0
    fi
    if [[ "$template" == *"{{${key}_onchange}}"* ]]; then
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

# Resolved once from the active LC_ALL/LC_NUMERIC/LANG via `locale -k` and
# cached in LOCALE_DECIMAL_SEP — spawning `locale` per row was noticeable on
# large result sets, and parsing with bash's built-in =~ avoids forking sed.
# Tests can reset the cache by setting LOCALE_DECIMAL_SEP="" before invoking.
# Falls back to "." when locale lookup fails.
LOCALE_DECIMAL_SEP=""
_compute_locale_decimal_separator() {
  local output sep="."
  output=$(locale -k decimal_point 2>/dev/null) || output=""
  local pattern='decimal_point="([^"]*)"'
  if [[ "$output" =~ $pattern ]] && [[ -n "${BASH_REMATCH[1]}" ]]; then
    sep=${BASH_REMATCH[1]}
  fi
  LOCALE_DECIMAL_SEP=$sep
}

locale_decimal_separator() {
  if [[ -z "$LOCALE_DECIMAL_SEP" ]]; then
    _compute_locale_decimal_separator
  fi
  printf '%s' "$LOCALE_DECIMAL_SEP"
}

# Resolve at script load so per-row callers (truncate_number_value runs inside
# row_value's command substitution) inherit the cached value instead of each
# subshell re-running `locale`.
_compute_locale_decimal_separator

# Locale weekday names, resolved once at script load. Same rationale as the
# decimal separator cache: per-row `date` forks were noticeable. Seven known
# reference dates (one per English weekday) get translated through the active
# LC_TIME locale; fall back to the English name if `date -d` fails.
declare -A LOCALE_WEEKDAY_LONG=()
declare -A LOCALE_WEEKDAY_SHORT=()
_compute_locale_weekdays() {
  local -a en=(Sunday Monday Tuesday Wednesday Thursday Friday Saturday)
  local -a ref=(2024-01-07 2024-01-01 2024-01-02 2024-01-03 2024-01-04 2024-01-05 2024-01-06)
  local i long short
  for ((i = 0; i < 7; i++)); do
    long=$(date -d "${ref[$i]}" +%A 2>/dev/null) || long=${en[$i]}
    short=$(date -d "${ref[$i]}" +%a 2>/dev/null) || short=${en[$i]:0:3}
    LOCALE_WEEKDAY_LONG[${en[$i]}]=$long
    LOCALE_WEEKDAY_SHORT[${en[$i]}]=$short
  done
}
_compute_locale_weekdays

_weekday_source() {
  local kind=$1 row=$2 field=$3
  if [[ "$field" == best_future ]]; then
    jq_value "$row" '.best_future_weekday // ""'
  elif [[ "$kind" == check ]]; then
    jq_value "$row" '.best_future_weekday // ""'
  else
    jq_value "$row" '.weekday // ""'
  fi
}

format_weekday() {
  local mode=$1 wd=$2
  if [[ -z "$wd" ]]; then
    return 0
  fi
  case "$mode" in
    short)           printf '%s' "${wd:0:2}" ;;
    long_formatted)  printf '%s' "${LOCALE_WEEKDAY_LONG[$wd]:-$wd}" ;;
    short_formatted) printf '%s' "${LOCALE_WEEKDAY_SHORT[$wd]:-${wd:0:3}}" ;;
  esac
}

# String-based to avoid FP pitfalls (e.g. 1.7 * 100 = 169.999... truncating to 1.69).
truncate_number_value() {
  local row=$1
  local expr=$2
  local decimals=$3
  local value

  value=$(jq_value "$row" "$expr")
  if [[ -z "$value" ]]; then
    return 0
  fi
  if [[ "$value" =~ ^(-?[0-9]+)([.]([0-9]+))?$ ]]; then
    local int_part=${BASH_REMATCH[1]}
    local frac_part=${BASH_REMATCH[3]:-}
    if ((${#frac_part} > decimals)); then
      frac_part=${frac_part:0:decimals}
    else
      while ((${#frac_part} < decimals)); do
        frac_part+="0"
      done
    fi
    if ((decimals > 0)); then
      printf '%s%s%s' "$int_part" "$(locale_decimal_separator)" "$frac_part"
    else
      printf '%s' "$int_part"
    fi
  else
    printf '%s' "$value"
  fi
}

capitalize_first() {
  local s=$1
  if [[ -z "$s" ]]; then
    return 0
  fi
  printf '%s' "${s^}"
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
    best_future_price_formatted) truncate_number_value "$row" '.best_future_price // ""' 2 ;;
    best_future_start_time) jq_value "$row" '.best_future_start_time // ""' ;;
    best_future_weekday) jq_value "$row" '.best_future_weekday // ""' ;;
    best_future_weekday_formatted) format_weekday long_formatted "$(_weekday_source "$kind" "$row" best_future)" ;;
    best_future_weekday_short) format_weekday short "$(_weekday_source "$kind" "$row" best_future)" ;;
    best_future_weekday_short_formatted) format_weekday short_formatted "$(_weekday_source "$kind" "$row" best_future)" ;;
    brand) jq_value "$row" '.station.brand // .brand // ""' ;;
    confidence) jq_value "$row" '.confidence // ""' ;;
    current_price) number_value "$row" '.current_price // ""' 3 ;;
    current_price_formatted) truncate_number_value "$row" '.current_price // ""' 2 ;;
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
    expected_drop) number_value "$row" '.expected_drop // ""' 2 ;;
    expected_lower) jq_value "$row" '.expected_lower // ""' ;;
    first_seen_at) jq_value "$row" '.station.first_seen_at // ""' ;;
    fuel) jq_value "$row" '.fuel // ""' ;;
    fuel_formatted) capitalize_first "$(jq_value "$row" '.fuel // ""')" ;;
    history_percentile) number_value "$row" '.history_percentile // ""' 1 ;;
    house_number) jq_value "$row" '.station.house_number // ""' ;;
    lat) number_value "$row" '.station.lat // ""' 6 ;;
    lng) number_value "$row" '.station.lng // ""' 6 ;;
    place) jq_value "$row" '.station.place // ""' ;;
    post_code) jq_value "$row" '.station.post_code // ""' ;;
    predicted_current_price) number_value "$row" '.predicted_current_price // ""' 3 ;;
    predicted_current_price_formatted) truncate_number_value "$row" '.predicted_current_price // ""' 2 ;;
    predicted_price) number_value "$row" '.predicted_price // .predicted_current_price // ""' 3 ;;
    predicted_price_formatted) truncate_number_value "$row" '.predicted_price // .predicted_current_price // ""' 2 ;;
    price)
      if [[ "$kind" == check ]]; then
        number_value "$row" '.current_price // ""' 3
      else
        number_value "$row" '.predicted_price // ""' 3
      fi
      ;;
    price_formatted)
      if [[ "$kind" == check ]]; then
        truncate_number_value "$row" '.current_price // ""' 2
      else
        truncate_number_value "$row" '.predicted_price // ""' 2
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
    weekday_formatted) format_weekday long_formatted "$(_weekday_source "$kind" "$row" auto)" ;;
    weekday_short) format_weekday short "$(_weekday_source "$kind" "$row" auto)" ;;
    weekday_short_formatted) format_weekday short_formatted "$(_weekday_source "$kind" "$row" auto)" ;;
    *) printf '' ;;
  esac
}

render_row_template() {
  local template=$1
  local kind=$2
  local row=$3
  local prev_row=${4-}
  local rendered=$template
  local key value prev_value

  for key in "${PLACEHOLDERS[@]}"; do
    if [[ "$rendered" != *"{{${key}_onchange}}"* ]]; then
      continue
    fi
    value=$(row_value "$kind" "$row" "$key")
    if [[ -n "$prev_row" ]]; then
      prev_value=$(row_value "$kind" "$prev_row" "$key")
      if [[ "$value" == "$prev_value" ]]; then
        value=""
      fi
    fi
    rendered=${rendered//\{\{${key}_onchange\}\}/$value}
  done

  for key in "${PLACEHOLDERS[@]}"; do
    if [[ "$rendered" != *"{{${key}}}"* ]]; then
      continue
    fi
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
  local row line prev_row=""
  for row in "$@"; do
    line=$(render_row_template "$row_template" "$kind" "$row" "$prev_row")
    if [[ -z "$message" ]]; then
      message=$line
    else
      message+=$'\n'"$line"
    fi
    prev_row=$row
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

expand_scalar_placeholders() {
  local template=$1
  local kind=$2
  local scalar_row=$3
  shift 3

  local result=$template
  local first_row="" key value quoted

  if [[ -n "$scalar_row" ]]; then
    first_row=$scalar_row
  elif (($# > 0)); then
    first_row=$1
  fi

  if [[ "$result" == *"{{count}}"* ]]; then
    result=${result//\{\{count\}\}/$#}
  fi

  if [[ "$result" == *"{{cheapest_"* ]]; then
    for key in "${PLACEHOLDERS[@]}"; do
      if [[ "$result" == *"{{cheapest_${key}}}"* ]]; then
        if [[ -n "$first_row" ]]; then
          value=$(row_value "$kind" "$first_row" "$key")
        else
          value=""
        fi
        quoted=$(shell_quote "$value")
        result=${result//\{\{cheapest_${key}\}\}/$quoted}
      fi
    done
  fi

  printf '%s' "$result"
}

build_notification_command() {
  local template=$1
  local kind=$2
  local message=$3
  local scalar_row=$4
  shift 4

  local command
  command=$(expand_scalar_placeholders "$template" "$kind" "$scalar_row" "$@")
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
  local scalar_row=$4
  shift 4

  local command
  command=$(build_notification_command "$template" "$kind" "$message" "$scalar_row" "$@")
  verbose_log "running $kind notification: $command"
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
  local scalar_row=${6-}
  local rows=()

  mapfile -t rows < <(printf '%s' "$json" | jq -c "$jq_filter")
  if ((${#rows[@]} == 0)); then
    verbose_log "no matching $kind rows"
    return 0
  fi

  verbose_log "sending ${#rows[@]} matching $kind row(s)"
  local message
  message=$(build_message "$kind" "$row_template" "${rows[@]}")
  run_notification "$kind" "$command_template" "$message" "$scalar_row" "${rows[@]}"
}

price_less_than() {
  LC_ALL=C awk -v a="$1" -v b="$2" 'BEGIN { exit !(a+0 < b+0) }'
}

filter_cheaper_check_rows() {
  local rows=("$@")
  local row price batch_min=""

  FILTERED_ROWS=()
  for row in "${rows[@]}"; do
    price=$(row_value check "$row" current_price)
    if [[ -z "$price" ]]; then
      continue
    fi
    if [[ -n "$CHECK_LOWEST_PRICE" ]] && ! price_less_than "$price" "$CHECK_LOWEST_PRICE"; then
      continue
    fi
    FILTERED_ROWS+=("$row")
    if [[ -z "$batch_min" ]] || price_less_than "$price" "$batch_min"; then
      batch_min=$price
    fi
  done

  if [[ -n "$batch_min" ]]; then
    CHECK_LOWEST_PRICE=$batch_min
  fi
}

send_changed_check_rows() {
  local json=$1
  local rows=()

  mapfile -t rows < <(printf '%s' "$json" | jq -c 'if type == "array" then map(select(.recommendation == "buy" and (.confidence == "medium" or .confidence == "high"))) | sort_by(.current_price) | .[] else empty end')
  if ((${#rows[@]} == 0)); then
    verbose_log "no matching check rows"
    return 0
  fi

  filter_cheaper_check_rows "${rows[@]}"
  if ((${#FILTERED_ROWS[@]} == 0)); then
    verbose_log "no cheaper check prices (baseline=$(shell_quote "$CHECK_LOWEST_PRICE"))"
    return 0
  fi

  verbose_log "sending ${#FILTERED_ROWS[@]} cheaper check row(s), baseline now $(shell_quote "$CHECK_LOWEST_PRICE")"
  local message
  message=$(build_message check "$CHECK_ROW_TEMPLATE" "${FILTERED_ROWS[@]}")
  run_notification check "$CHECK_COMMAND" "$message" "" "${FILTERED_ROWS[@]}"
}

run_check_once() {
  local output
  verbose_log "running check: $(shell_quote "$GASOLINE_BIN") check --city $(shell_quote "$CITY") --range-km $(shell_quote "$RADIUS_KM") --fuel $(shell_quote "$FUEL") --history-days $(shell_quote "$HISTORY_DAYS") --predict-days $(shell_quote "$PREDICT_DAYS") --output json"
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

  verbose_log "check returned $(printf '%s' "$output" | jq 'length') row(s)"
  send_changed_check_rows "$output"
}

run_suggest_once() {
  local output
  verbose_log "running suggest: $(shell_quote "$GASOLINE_BIN") suggest --city $(shell_quote "$CITY") --range-km $(shell_quote "$RADIUS_KM") --fuel $(shell_quote "$FUEL") --history-days $(shell_quote "$HISTORY_DAYS") --predict-days $(shell_quote "$PREDICT_DAYS") --output json"
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

  verbose_log "suggest returned $(printf '%s' "$output" | jq 'length') row(s)"
  local cheapest_row
  cheapest_row=$(printf '%s' "$output" | jq -c 'if type == "array" then map(select(.confidence == "medium" or .confidence == "high")) | min_by(.predicted_price // .predicted_current_price) // empty else empty end')
  send_matching_rows \
    suggest \
    "$SUGGEST_COMMAND" \
    "$output" \
    'if type == "array" then map(select(.confidence == "medium" or .confidence == "high")) | sort_by(.date, .start_time, (.station_name // .station.name)) | .[] else empty end' \
    "$SUGGEST_ROW_TEMPLATE" \
    "$cheapest_row"
}

maybe_run_suggest() {
  local now_date now_hour now_min now_minutes

  now_date=$(date +%F)
  now_hour=$(date +%H)
  now_min=$(date +%M)
  now_minutes=$((10#$now_hour * 60 + 10#$now_min))

  if ((now_minutes >= SUGGEST_MINUTES)) && [[ "$LAST_SUGGEST_DATE" != "$now_date" ]]; then
    verbose_log "suggest due for $now_date at $SUGGEST_TIME"
    run_suggest_once
    LAST_SUGGEST_DATE=$now_date
  else
    verbose_log "suggest not due; now_minutes=$now_minutes suggest_minutes=$SUGGEST_MINUTES last_suggest_date=$(shell_quote "$LAST_SUGGEST_DATE")"
  fi
}

maybe_reset_baseline() {
  local now_date now_hour now_min now_minutes

  read -r now_date now_hour now_min < <(date "+%F %H %M")
  now_minutes=$((10#$now_hour * 60 + 10#$now_min))

  if ((now_minutes >= RESET_MINUTES)) && [[ "$LAST_RESET_DATE" != "$now_date" ]]; then
    verbose_log "resetting baseline for $now_date at $RESET_TIME (was $(shell_quote "$CHECK_LOWEST_PRICE"))"
    CHECK_LOWEST_PRICE=""
    LAST_RESET_DATE=$now_date
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

  local reset_in
  if [[ "$LAST_RESET_DATE" != "$now_date" ]] && ((now_minutes < RESET_MINUTES)); then
    reset_in=$(( (RESET_MINUTES - now_minutes) * 60 - 10#$now_sec ))
    if ((reset_in > 0 && reset_in < sleep)); then
      sleep=$reset_in
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
    maybe_reset_baseline
    if ((LAST_CHECK_EPOCH == 0 || now_epoch - LAST_CHECK_EPOCH >= CHECK_MINUTES * 60)); then
      run_check_once
      LAST_CHECK_EPOCH=$(date +%s)
    fi

    maybe_run_suggest

    now_epoch=$(date +%s)
    local sleep_seconds
    sleep_seconds=$(compute_sleep "$max_sleep" "$now_epoch")
    verbose_log "sleeping ${sleep_seconds}s"
    sleep "$sleep_seconds"
  done
}

main() {
  parse_args "$@"
  validate_args
  require_tools
  log_config
  main_loop
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
