#!/usr/bin/env bash
# Claude Code hook for fast edit checks and the full stop gate.
set -u

export PATH="/opt/homebrew/bin:/usr/local/bin:${PATH:-}"
fail_open() {
	local status=$?
	if [ "$status" -ne 0 ] && [ "$status" -ne 2 ]; then
		exit 0
	fi
}
trap fail_open EXIT
command -v jq >/dev/null 2>&1 || exit 0

EVENT="${1:-}"
INPUT="$(cat)" || exit 0
jqr() { printf '%s' "$INPUT" | jq -r "$1 // empty" 2>/dev/null; }
deny() { printf '%s\n' "$1" >&2; exit 2; }

repo_root() {
  local cwd
  cwd="$(jqr '.cwd')"
  [ -n "$cwd" ] || return 1
  git -C "$cwd" rev-parse --show-toplevel 2>/dev/null
}

post_edit() {
  local root file output package
  root="$(repo_root)" || exit 0
  file="$(jqr '.tool_input.file_path')"
  [ -f "$file" ] || exit 0
  case "$file" in
    *.go)
      output="$(gofmt -l "$file" 2>&1)"
      [ -z "$output" ] || deny "gofmt: $file is not formatted"
      package="./$(dirname "${file#"$root"/}")"
      output="$(cd "$root" && go vet "$package" 2>&1)" || deny "go vet $package failed:\n$output"
      ;;
    "$root"/.github/workflows/*.yml|"$root"/.github/workflows/*.yaml)
      command -v actionlint >/dev/null 2>&1 || exit 0
      output="$(cd "$root" && actionlint 2>&1)" || deny "actionlint failed:\n$output"
      ;;
  esac
}

stop() {
  [ "$(jqr '.stop_hook_active')" != "true" ] || exit 0
  local root output
  root="$(repo_root)" || exit 0
  [ -f "$root/scripts/gate/check.sh" ] || exit 0
  output="$(cd "$root" && bash scripts/gate/check.sh 2>&1)" || \
    deny "quality gate failed:\n$output"
}

case "$EVENT" in
  post-edit) post_edit ;;
  stop) stop ;;
  *) exit 0 ;;
esac
