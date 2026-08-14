#!/usr/bin/env bash
set -euo pipefail

SOURCE_ROOT="$(git rev-parse --show-toplevel)"
SOURCE_SCRIPT="$SOURCE_ROOT/scripts/gate/loc.sh"
FIXTURE="$(mktemp -d)"
trap 'rm -rf "$FIXTURE"' EXIT

# Git hooks export their repository and index settings. Remove those settings
# before the fixture creates its own repository.
while IFS= read -r name; do
  unset "$name"
done < <(git rev-parse --local-env-vars)

fail() {
  printf 'loc-test: %s\n' "$1" >&2
  exit 1
}

assert_fails() {
  if "$@" > "$FIXTURE/command.out" 2> "$FIXTURE/command.err"; then
    fail "command succeeded: $*"
  fi
}

budget() {
  tr -d '[:space:]' < "$FIXTURE/repo/scripts/gate/loc-budget.txt"
}

cd "$FIXTURE"
mkdir -p repo/scripts/gate
cp "$SOURCE_SCRIPT" repo/scripts/gate/loc.sh
cd repo
git init -q
git config user.email loc-test@example.invalid
git config user.name loc-test
printf 'seed\n' > source.txt
printf '10000\n' > scripts/gate/loc-budget.txt
git add source.txt scripts/gate/loc.sh scripts/gate/loc-budget.txt
git commit -qm 'test fixture'

COUNT="$(bash scripts/gate/loc.sh)"
CEILING="$(((COUNT / 100 + 1) * 100))"
printf '%s\n' "$CEILING" > scripts/gate/loc-budget.txt
git add scripts/gate/loc-budget.txt
git commit -qm 'set base ceiling'
BASE_REF="$(git rev-parse HEAD)"
bash scripts/gate/loc.sh --check --base-ref "$BASE_REF" > /dev/null

BEFORE="$(bash scripts/gate/loc.sh)"
printf 'one\ntwo\nthree\n' > untracked.txt
AFTER="$(bash scripts/gate/loc.sh)"
[ "$AFTER" -eq "$((BEFORE + 3))" ] || fail 'untracked lines were not counted'
rm -f untracked.txt

BEFORE="$(bash scripts/gate/loc.sh)"
mkdir -p generated
printf 'lock\ncontent\n' > generated/pnpm-lock.yaml
AFTER="$(bash scripts/gate/loc.sh)"
[ "$AFTER" -eq "$BEFORE" ] || fail 'generated pnpm lock lines were counted'
rm -f generated/pnpm-lock.yaml
rmdir generated

printf '%s\n' "$((CEILING + 100))" > scripts/gate/loc-budget.txt
assert_fails bash scripts/gate/loc.sh --check --base-ref "$BASE_REF"
printf '%s\n' "$CEILING" > scripts/gate/loc-budget.txt

CURRENT="$(bash scripts/gate/loc.sh)"
NEEDED="$((CEILING - CURRENT + 1))"
awk -v lines="$NEEDED" 'BEGIN { for (i = 0; i < lines; i++) print "growth" }' > growth.txt
assert_fails bash scripts/gate/loc.sh --check
bash scripts/gate/loc.sh --raise > /dev/null
GROWN="$(bash scripts/gate/loc.sh)"
WANT="$(((GROWN / 100 + 1) * 100))"
[ "$(budget)" = "$WANT" ] || fail 'raise did not use the smallest band'
bash scripts/gate/loc.sh --check --base-ref "$BASE_REF" > /dev/null

printf '%s\n' "$((WANT + 100))" > scripts/gate/loc-budget.txt
assert_fails bash scripts/gate/loc.sh --check --base-ref "$BASE_REF"
printf '%s\n' "$((WANT + 200))" > scripts/gate/loc-budget.txt
bash scripts/gate/loc.sh --tighten > /dev/null
[ "$(budget)" = "$WANT" ] || fail 'tighten did not restore the smallest band'

printf 'invalid\n' > scripts/gate/loc-budget.txt
assert_fails bash scripts/gate/loc.sh --check
printf 'loc-test: pass\n'
