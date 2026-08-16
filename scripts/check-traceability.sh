#!/bin/sh
# Prove SPEC §8.2 / §10.6 traceability structurally: the E2E test
# functions and the rows of docs/traceability.md are the same set.
#
#   sh scripts/check-traceability.sh [traceability.md] [e2e-dir]
#
# §8.2 requires the mapping to hold in BOTH directions, so both are
# checked:
#
#   (a) every `^func Test...` in <e2e-dir>/*_test.go is named by a row of
#       the map — a test nobody documented is a use case nobody promised;
#   (b) every test ID in the map names a function that exists — a row
#       pointing at a deleted or renamed test is a matrix hole that reads
#       as coverage.
#
# Plus the two ways that pair can rot quietly:
#
#   (c) no ID appears in the map twice (two rows sharing a test means one
#       of them is uncovered);
#   (d) the infrastructure exemptions below still name real functions,
#       and none of them is listed as a B.5 row.
#
# The check is STRUCTURAL. It cannot tell whether a test exercises what
# its row claims; that is the reviewer's job. What it does guarantee is
# that renaming, adding or deleting a test without touching the map is
# impossible to merge.
#
# Every offender is printed before the script exits 1 — a check that
# stopped at the first one would need as many runs as there are mistakes.
set -eu

# comm(1) compares against the collating order of the sort that produced
# its inputs; pinning both to C is the only way that pairing is stable
# across a developer's locale and the runner's.
LC_ALL=C
export LC_ALL

repo_root=$(unset CDPATH; cd -- "$(dirname -- "$0")/.." && pwd)

# Both inputs are overridable so the check can be run against a mutated
# copy of either side: a checker nobody has ever seen fail is not known
# to work.
MAP=${1:-$repo_root/docs/traceability.md}
E2E_DIR=${2:-$repo_root/test/e2e}

# Test functions that are NOT B.5 rows. They test the suite, not the
# image: TestMain is the entry point (preflight, sweep, teardown) and
# TestHarnessSmoke is the harness's test of itself. Keep this list and
# the "Infrastructure tests" section of the map in sync — check (d)
# fails if an entry here stops naming a real function.
EXEMPT="TestMain TestHarnessSmoke"

if [ ! -f "$MAP" ]; then
	echo "error: traceability map not found: $MAP" >&2
	exit 1
fi
if [ ! -d "$E2E_DIR" ]; then
	echo "error: e2e directory not found: $E2E_DIR" >&2
	exit 1
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
trap 'rm -rf "$tmp"; exit 130' INT
trap 'rm -rf "$tmp"; exit 143' TERM

# --- side 1: the test functions -------------------------------------
#
# `^func Test` and nothing else. A helper named testFoo, a method with a
# receiver, or an indented (hence commented-out or nested) function are
# all correctly ignored.
set -- "$E2E_DIR"/*_test.go
if [ ! -f "$1" ]; then
	echo "error: no *_test.go under $E2E_DIR — this check would pass vacuously" >&2
	exit 1
fi
grep -h '^func Test' "$@" |
	sed -e 's/^func \(Test[A-Za-z0-9_]*\).*$/\1/' |
	sort -u >"$tmp/funcs" || true

if [ ! -s "$tmp/funcs" ]; then
	echo "error: no test function found in $E2E_DIR — this check would pass vacuously" >&2
	exit 1
fi

# --- side 2: the IDs in the map -------------------------------------
#
# Only markdown table rows are read, and only backticked Test* tokens
# inside them, so the prose around the table — which necessarily names
# TestHarnessSmoke and friends — can never be mistaken for a mapping.
grep '^[[:space:]]*|' "$MAP" |
	grep -o "\`Test[A-Za-z0-9_]*\`" |
	tr -d "\`" |
	sort >"$tmp/ids.all" || true
sort -u "$tmp/ids.all" >"$tmp/ids"

for name in $EXEMPT; do
	echo "$name"
done | sort -u >"$tmp/exempt"
comm -23 "$tmp/funcs" "$tmp/exempt" >"$tmp/mapped"

failures=0

# report <headline> <file-of-offenders>
report() {
	count=$(wc -l <"$2" | tr -d ' ')
	if [ "$count" -gt 0 ]; then
		echo "error: $1" >&2
		sed -e 's/^/  - /' "$2" >&2
		failures=$((failures + count))
	fi
}

# (a) a test with no row
comm -23 "$tmp/mapped" "$tmp/ids" >"$tmp/unmapped"
report "test function(s) missing from $MAP (add a B.5 row, or exempt as infrastructure)" \
	"$tmp/unmapped"

# (b) a row with no test
comm -13 "$tmp/funcs" "$tmp/ids" >"$tmp/dangling"
report "traceability row(s) naming a test that does not exist in $E2E_DIR" \
	"$tmp/dangling"

# (c) one ID claimed by two rows
uniq -d "$tmp/ids.all" >"$tmp/dupes"
report "test ID(s) listed by more than one row of $MAP" "$tmp/dupes"

# (d) the exemptions themselves
comm -23 "$tmp/exempt" "$tmp/funcs" >"$tmp/stale"
report "exempted name(s) that no longer name a function (drop them from EXEMPT)" \
	"$tmp/stale"
comm -12 "$tmp/exempt" "$tmp/ids" >"$tmp/infra_rows"
report "infrastructure test(s) listed as a B.5 row in $MAP (they cover the suite, not the image)" \
	"$tmp/infra_rows"

if [ "$failures" -ne 0 ]; then
	echo "traceability: $failures problem(s); see above" >&2
	exit 1
fi

rows=$(wc -l <"$tmp/ids" | tr -d ' ')
if [ "$rows" -eq 0 ]; then
	# Reachable only if every test is exempt: the map lists nothing and
	# agrees with a suite that documents nothing.
	echo "error: $MAP names no test at all — the gate would pass vacuously" >&2
	exit 1
fi

printf 'traceability ok: %s row(s) <-> %s test(s), %s infrastructure test(s) exempt\n' \
	"$rows" "$(wc -l <"$tmp/mapped" | tr -d ' ')" "$(wc -l <"$tmp/exempt" | tr -d ' ')"
