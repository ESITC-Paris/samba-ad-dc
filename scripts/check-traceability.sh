#!/bin/sh
# Prove SPEC §8.2 / §10.6 traceability structurally: the E2E test
# functions, the rows of docs/traceability.md and the test IDs the three
# guides cite are the same set.
#
#   sh scripts/check-traceability.sh [traceability.md] [e2e-dir] [guides-root]
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
# Plus the three ways that pair can rot quietly:
#
#   (c) no ID appears in the map twice (two rows sharing a test means one
#       of them is uncovered);
#   (d) the infrastructure exemptions below still name real functions,
#       and none of them is listed as a B.5 row;
#   (e) there are exactly as many table ROWS as there are distinct test
#       IDs. (a) and (b) compare two SETS of names and are blind to a row
#       that names no test at all, or to two rows merged into one naming
#       two — both of which keep the name sets identical while breaking
#       the "one row <-> one test" mapping §8.2 requires.
#
# And the §10.6 side — the guides:
#
#   (f) every mapped ID is CITED by at least one of the three guides
#       (README.md, docs/deployment-guide.md, docs/update-guide.md), and
#       every test name the guides cite is a mapped ID or an exempt
#       infrastructure test. §10.6 wants every guide section to name the
#       test that covers it; a documented behaviour nothing cites is a
#       promise with no test attached, and a citation naming a test that
#       is not in the matrix is a reader sent to something that may not
#       exist at all.
#
#       A CITATION is a backticked WHOLE test ID — the token
#       `Test<name>` — anywhere in a guide file: in a "*Covered by:*"
#       line, in a section-to-test table, or in running prose. The three
#       guides word it differently and both forms count; what is checked
#       is the ID, not the sentence around it. The backticks are load
#       bearing: they are what makes `TestProvision` a different token
#       from `TestProvisionOverStateRefused` instead of a prefix of it.
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

# All three inputs are overridable so the check can be run against a
# mutated copy of any side: a checker nobody has ever seen fail is not
# known to work. GUIDES_ROOT is a directory holding the three guides at
# their usual paths, so a mutation test copies the files it wants to
# break and points the check at the copy.
MAP=${1:-$repo_root/docs/traceability.md}
E2E_DIR=${2:-$repo_root/test/e2e}
GUIDES_ROOT=${3:-$repo_root}

# The §10.6 guides, relative to GUIDES_ROOT. Adding a guide means adding
# it here — a guide absent from this list is one whose citations nothing
# checks, in either direction.
GUIDES="README.md docs/deployment-guide.md docs/update-guide.md"

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
# A missing guide is fatal rather than skipped: check (f) run over two of
# the three guides would report every ID cited only by the third as
# uncited, and — worse the other way round — a renamed guide would quietly
# stop being checked at all.
for g in $GUIDES; do
	if [ ! -f "$GUIDES_ROOT/$g" ]; then
		echo "error: guide not found: $GUIDES_ROOT/$g" >&2
		exit 1
	fi
done

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

# --- side 2b: how many ROWS the map has ------------------------------
#
# Counted from the table itself rather than from the IDs found in it, so
# that the two numbers come from independent sources and check (e) below
# can compare them. A separator line (|---|---|) is what marks the line
# before it as a header, so every separator cancels the header it
# follows: what is left is data rows, for any number of tables. The map
# is expected to hold exactly ONE table — the same assumption the ID
# extraction above already makes.
rows=$(awk '
	/^[[:space:]]*\|/ {
		if ($0 ~ /^[[:space:]]*\|[-:|[:space:]]*$/) { n--; next }
		n++
	}
	END { print n + 0 }
' "$MAP")

# --- side 3: the IDs the guides cite ---------------------------------
#
# The SAME extraction as the map's, deliberately: a backticked whole ID,
# and nothing else. Anything looser would let `TestProvision` match
# inside `TestProvisionOverStateRefused` and report a test as cited
# because a longer-named one is.
#
# Unlike the map, the whole file is read rather than its table rows: a
# guide cites a test wherever it documents the behaviour, which is mostly
# prose. Nothing in these three files names a Go function for any other
# reason, so there is no prose to protect from the extraction here.
set --
for g in $GUIDES; do
	set -- "$@" "$GUIDES_ROOT/$g"
done
guides=$#
grep -ho "\`Test[A-Za-z0-9_]*\`" "$@" |
	tr -d "\`" |
	sort -u >"$tmp/cited" || true

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

# (e) one row, one test — counted, not inferred from the name sets
ids=$(wc -l <"$tmp/ids" | tr -d ' ')
if [ "$rows" -ne "$ids" ]; then
	echo "error: $MAP has $rows matrix table row(s) but names $ids distinct test ID(s);" >&2
	echo "  §8.2 requires one row per test and one test per row. A row naming no test," >&2
	echo "  or one row naming two, is the usual cause." >&2
	failures=$((failures + 1))
fi

# (f) the guides, both ways
#
# A mapped ID nobody cites: §10.6 is unmet for that use case — the guides
# document the behaviour (or fail to), and a reader has no way to reach
# the test that proves it.
comm -23 "$tmp/ids" "$tmp/cited" >"$tmp/uncited"
report "test ID(s) in $MAP that no guide cites (add a \`TestX\` citation to the section that documents it)" \
	"$tmp/uncited"

# A citation naming something that is not a mapped ID and not exempt: a
# typo, a renamed test, or a test that was deleted and left behind a
# reference the reader cannot check.
sort -u "$tmp/ids" "$tmp/exempt" >"$tmp/known"
comm -13 "$tmp/known" "$tmp/cited" >"$tmp/invented"
report "test name(s) cited by a guide that no row of $MAP names (fix the citation, or add the row)" \
	"$tmp/invented"

if [ "$failures" -ne 0 ]; then
	echo "traceability: $failures problem(s); see above" >&2
	exit 1
fi

if [ "$rows" -eq 0 ]; then
	# Reachable only if every test is exempt: the map lists nothing and
	# agrees with a suite that documents nothing.
	echo "error: $MAP names no test at all — the gate would pass vacuously" >&2
	exit 1
fi

# The counts come from different places on purpose — table rows on one
# side, test functions found in the tree on another, backticked citations
# scraped out of the guides on a third — so printing them is printing the
# agreement, not the same number three times.
printf 'traceability ok: %s matrix row(s) <-> %s test function(s) <-> %s distinct ID(s) cited across %s guide(s), %s infrastructure test(s) exempt\n' \
	"$rows" "$(wc -l <"$tmp/mapped" | tr -d ' ')" \
	"$(wc -l <"$tmp/cited" | tr -d ' ')" "$guides" \
	"$(wc -l <"$tmp/exempt" | tr -d ' ')"
