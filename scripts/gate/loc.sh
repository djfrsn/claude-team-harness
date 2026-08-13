#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"
BUDGET_FILE="$REPO_ROOT/scripts/gate/loc-budget.txt"

count_lines() {
  local file
  {
    while IFS= read -r -d '' file; do
      [ ! -f "$file" ] || cat "$file"
    done < <(git ls-files -z -- ':!:go.sum' ':!:*.png' ':!:*.jpg' ':!:*.gif')
    while IFS= read -r -d '' file; do
      [ ! -f "$file" ] || cat "$file"
    done < <(git ls-files -z --others --exclude-standard -- \
      ':!:go.sum' ':!:*.png' ':!:*.jpg' ':!:*.gif')
  } | wc -l | tr -d ' '
}

case "${1:-}" in
  --check)
    count="$(count_lines)"
    ceiling="$(tr -d '[:space:]' < "$BUDGET_FILE")"
    if [ "$count" -gt "$ceiling" ]; then
      printf 'loc: %s counted lines exceed the ceiling of %s\n' "$count" "$ceiling" >&2
      exit 1
    fi
    printf 'loc: %s counted lines, ceiling %s\n' "$count" "$ceiling"
    ;;
  "") count_lines ;;
  *) printf 'loc: use --check or no argument\n' >&2; exit 2 ;;
esac
