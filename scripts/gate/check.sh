#!/usr/bin/env bash
# One quality-gate entry point for local hooks and CI.
# The check functions run through run_check's function-name argument.
# shellcheck disable=SC2329
set -uo pipefail

ONLY=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --only) ONLY="${2:?gate: --only needs a check name}"; shift 2 ;;
    --force) shift ;;
    *) printf 'gate: unknown argument %s\n' "$1" >&2; exit 2 ;;
  esac
done

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT" || exit 1
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
FAILED=0
RAN=0

need() {
  command -v "$1" >/dev/null 2>&1 && return 0
  printf 'gate: %s is missing. Install it with: %s\n' "$1" "$2" >&2
  return 1
}

run_check() {
  local name="$1" function_name="$2"
  [ -z "$ONLY" ] || [ "$ONLY" = "$name" ] || return 0
  RAN=1
  {
    local start end rc=0
    start="$(date +%s)"
    "$function_name" || rc=$?
    end="$(date +%s)"
    if [ "$rc" -eq 0 ]; then
      printf 'gate: %s ok (%ss)\n' "$name" "$((end - start))" >&2
    else
      printf 'gate: %s FAILED (%ss)\n' "$name" "$((end - start))" >&2
    fi
    printf '%s' "$rc" > "$WORK/$name.rc"
  } > "$WORK/$name.out" 2> "$WORK/$name.err" &
}

check_shell() {
	local file
	local -a files=()
	while IFS= read -r file; do
		files+=("$file")
	done < <(git ls-files '*.sh')
	[ "${#files[@]}" -eq 0 ] || {
		need shellcheck 'brew install shellcheck' && shellcheck "${files[@]}"
	}
	bash scripts/gate/loc_test.sh
}

check_actions() {
  local files
  files="$(git ls-files '.github/workflows/*.yml' '.github/workflows/*.yaml')"
  [ -z "$files" ] || { need actionlint 'brew install actionlint' && actionlint; }
}

check_typescript() {
  local package="$REPO_ROOT/typescript"
  [ -f "$package/package.json" ] || return 0
  need node 'nvm install 24 && nvm use 24' || return 1
  need pnpm 'corepack prepare pnpm@11.19.0 --activate' || return 1

  local node_major pnpm_version rc=0
  node_major="$(node -p 'process.versions.node.split(".")[0]')"
  if [ "$node_major" != 24 ]; then
    printf 'gate: Node is %s; this gate pins 24. Repair it with: nvm install 24 && nvm use 24\n' \
      "$(node --version)" >&2
    rc=1
  fi
  pnpm_version="$(cd "$package" && pnpm --version 2>/dev/null)"
  if [ "$pnpm_version" != 11.19.0 ]; then
    printf 'gate: pnpm is %s; this gate pins 11.19.0. Repair it with: corepack prepare pnpm@11.19.0 --activate\n' \
      "${pnpm_version:-missing}" >&2
    rc=1
  fi
  [ "$rc" -eq 0 ] || return "$rc"
  if [ ! -d "$package/node_modules" ]; then
    printf 'gate: TypeScript dependencies are missing. Install them with: (cd typescript && pnpm install --frozen-lockfile)\n' >&2
    return 1
  fi
  (cd "$package" && pnpm run check)
}

check_go_lint() {
  need go 'brew install go' || return 1
  need golangci-lint \
    'go install github.com/golangci/golangci-lint/cmd/golangci-lint@v1.64.8' || return 1
  local rc=0 formatted file lines
  formatted="$(gofmt -l . 2>&1)"
  [ -z "$formatted" ] || { printf 'gofmt: unformatted files:\n%s\n' "$formatted"; rc=1; }
  go vet ./... || rc=1
  golangci-lint run --allow-serial-runners || rc=1
  while IFS= read -r file; do
    lines="$(wc -l < "$file" | tr -d ' ')"
    if [ "$lines" -gt 500 ]; then
      printf 'filelen: %s is %s lines (max 500)\n' "$file" "$lines"
      rc=1
    fi
  done < <(git ls-files '*.go')
  return "$rc"
}

check_go_audit() {
  need govulncheck \
    'go install golang.org/x/vuln/cmd/govulncheck@v1.1.4' || return 1
  govulncheck ./...
}

check_go_test() {
  need go 'brew install go' || return 1
  local profile percentage rc=0 floor=50
  profile="$(mktemp)"
  if [ "${CI:-}" = "true" ]; then
    go test -race -shuffle=on -count=1 -coverprofile="$profile" -coverpkg=./... ./... || rc=1
  else
    go test -coverprofile="$profile" -coverpkg=./... ./... || rc=1
  fi
  percentage="$(go tool cover -func="$profile" 2>/dev/null | awk '/^total:/ { sub(/%/, "", $NF); print $NF }')"
  rm -f "$profile"
  if [ -z "$percentage" ]; then
    printf 'coverage: the test run produced no profile\n'
    rc=1
  elif awk -v value="$percentage" -v minimum="$floor" 'BEGIN { exit !(value < minimum) }'; then
    printf 'coverage: %s%% is below the %s%% floor\n' "$percentage" "$floor"
    rc=1
  else
    printf 'coverage: %s%% (floor %s%%)\n' "$percentage" "$floor"
  fi
  return "$rc"
}

check_loc_budget() {
  if [ -n "${GITHUB_BASE_REF:-}" ]; then
    bash "$REPO_ROOT/scripts/gate/loc.sh" --check --base-ref "origin/$GITHUB_BASE_REF"
  else
    bash "$REPO_ROOT/scripts/gate/loc.sh" --check
  fi
}

run_check shell check_shell
run_check actions check_actions
run_check typescript check_typescript
run_check go-lint check_go_lint
run_check go-audit check_go_audit
run_check go-test check_go_test
run_check loc-budget check_loc_budget
wait

for name in shell actions typescript go-lint go-audit go-test loc-budget; do
  [ -f "$WORK/$name.rc" ] || continue
  cat "$WORK/$name.out"
  cat "$WORK/$name.err" >&2
  [ "$(cat "$WORK/$name.rc")" = "0" ] || FAILED=1
done

if [ "$RAN" -eq 0 ]; then
  printf 'gate: unknown check %s (shell|actions|typescript|go-lint|go-audit|go-test|loc-budget)\n' "$ONLY" >&2
  exit 2
fi
exit "$FAILED"
