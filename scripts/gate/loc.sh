#!/usr/bin/env bash
# loc.sh manages the source line ceiling in 100-line bands.
#
# Usage:
#   scripts/gate/loc.sh
#   scripts/gate/loc.sh --check [--base-ref <git-ref>]
#   scripts/gate/loc.sh --raise
#   scripts/gate/loc.sh --tighten
set -euo pipefail

STEP=100
REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"
BUDGET_PATH="scripts/gate/loc-budget.txt"
BUDGET_FILE="$REPO_ROOT/$BUDGET_PATH"

count_lines() {
  local file
  {
    while IFS= read -r -d '' file; do
      [ ! -f "$file" ] || cat "$file"
    done < <(git ls-files -z -- ':!:go.sum' ':!:**/pnpm-lock.yaml' \
      ':!:*.png' ':!:*.jpg' ':!:*.gif')
    while IFS= read -r -d '' file; do
      [ ! -f "$file" ] || cat "$file"
    done < <(git ls-files -z --others --exclude-standard -- \
      ':!:go.sum' ':!:**/pnpm-lock.yaml' ':!:*.png' ':!:*.jpg' ':!:*.gif')
  } | wc -l | tr -d ' '
}

ceiling_for() {
  printf '%s\n' "$((($1 / STEP + 1) * STEP))"
}

validate_ceiling() {
  local value="$1" source="$2"
  case "$value" in
    ''|*[!0-9]*|0)
      printf 'loc: %s must contain one positive integer\n' "$source" >&2
      return 1
      ;;
  esac
  if [ "$((value % STEP))" -ne 0 ]; then
    printf 'loc: %s ceiling %s must be a multiple of %s\n' \
      "$source" "$value" "$STEP" >&2
    return 1
  fi
}

read_ceiling() {
  local value
  if [ ! -f "$BUDGET_FILE" ]; then
    printf 'loc: ceiling file is missing; run "bash scripts/gate/loc.sh --tighten"\n' >&2
    return 1
  fi
  value="$(tr -d '[:space:]' < "$BUDGET_FILE")"
  validate_ceiling "$value" "$BUDGET_PATH" || return 1
  printf '%s\n' "$value"
}

read_base_ceiling() {
  local ref="$1" value
  if ! value="$(git show "$ref:$BUDGET_PATH" 2>/dev/null | tr -d '[:space:]')"; then
    printf 'loc: cannot read %s from base ref %s\n' "$BUDGET_PATH" "$ref" >&2
    return 1
  fi
  validate_ceiling "$value" "$ref:$BUDGET_PATH" || return 1
  printf '%s\n' "$value"
}

write_ceiling() {
  printf '%s\n' "$1" > "$BUDGET_FILE"
}

MODE="count"
BASE_REF=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --check) MODE="check"; shift ;;
    --raise) MODE="raise"; shift ;;
    --tighten) MODE="tighten"; shift ;;
    --base-ref) BASE_REF="${2:?loc: --base-ref needs a git ref}"; shift 2 ;;
    -h|--help) sed -n '3,8p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) printf 'loc: unknown argument %s\n' "$1" >&2; exit 2 ;;
  esac
done

if [ -n "$BASE_REF" ] && [ "$MODE" != "check" ]; then
  printf 'loc: --base-ref requires --check\n' >&2
  exit 2
fi

COUNT="$(count_lines)"
WANT="$(ceiling_for "$COUNT")"

case "$MODE" in
  count)
    printf '%s\n' "$COUNT"
    ;;
  raise)
    CEILING="$(read_ceiling)"
    if [ "$COUNT" -le "$CEILING" ]; then
      printf 'loc: %s counted lines fit the ceiling of %s; no change\n' \
        "$COUNT" "$CEILING"
      exit 0
    fi
    write_ceiling "$WANT"
    printf 'loc: ceiling raised %s -> %s for %s counted lines; commit %s with this work\n' \
      "$CEILING" "$WANT" "$COUNT" "$BUDGET_PATH"
    ;;
  tighten)
    CEILING="$(read_ceiling 2>/dev/null || true)"
    write_ceiling "$WANT"
    printf 'loc: ceiling set %s -> %s for %s counted lines\n' \
      "${CEILING:-none}" "$WANT" "$COUNT"
    ;;
  check)
    CEILING="$(read_ceiling)"
    if [ "$COUNT" -gt "$CEILING" ]; then
      printf 'loc: %s counted lines exceed the ceiling of %s (+%s); cut lines or run "bash scripts/gate/loc.sh --raise" and commit the budget change\n' \
        "$COUNT" "$CEILING" "$((COUNT - CEILING))" >&2
      exit 1
    fi
    if [ -n "$BASE_REF" ]; then
      BASE_CEILING="$(read_base_ceiling "$BASE_REF")"
      if [ "$CEILING" -gt "$BASE_CEILING" ]; then
        if [ "$COUNT" -le "$BASE_CEILING" ]; then
          printf 'loc: ceiling increase %s -> %s is premature; %s counted lines still fit the base ceiling\n' \
            "$BASE_CEILING" "$CEILING" "$COUNT" >&2
          exit 1
        fi
        if [ "$CEILING" -ne "$WANT" ]; then
          printf 'loc: ceiling increase %s -> %s is too large; %s counted lines need %s\n' \
            "$BASE_CEILING" "$CEILING" "$COUNT" "$WANT" >&2
          exit 1
        fi
      fi
    fi
    if [ "$((CEILING - COUNT))" -ge "$((STEP * 2))" ]; then
      printf 'loc: %s counted lines, ceiling %s (%s to spare); tighten with "bash scripts/gate/loc.sh --tighten"\n' \
        "$COUNT" "$CEILING" "$((CEILING - COUNT))"
    else
      printf 'loc: %s counted lines, ceiling %s (%s to spare)\n' \
        "$COUNT" "$CEILING" "$((CEILING - COUNT))"
    fi
    ;;
esac
