#!/bin/sh
# Capability bisection: establish the MINIMAL capability set the image
# needs, by measurement (SPEC §5.2, adaptation-profile B.2).
#
#   sh test/capbisect/bisect.sh [-o <report>] [-k]
#
# B.2 carries a capability set. Until this script runs, that set is a
# hypothesis: a list someone wrote down because it looked sufficient and
# because nothing failed. "Nothing failed" is evidence for sufficiency and
# no evidence at all for minimality — a set with one capability too many
# passes every test in the suite, forever, silently. The only way to learn
# that a capability is REQUIRED is to take it away and watch the image
# break.
#
# So that is what this does, against the real image, over five phases:
#
#   1. baseline      the set under test must pass (else the measurement
#                    has no reference point and the script aborts)
#   2. drop-one      for each capability C: run with (set - C).
#                    fail => C is REQUIRED.  pass => C is DROPPABLE.
#                    Every failure is re-run once before it is recorded,
#                    so a flake cannot quietly become a "required".
#   3. verify-min    run with only the required ones. This is the step
#                    that turns a list of individually-necessary
#                    capabilities into a jointly-sufficient set: dropping
#                    two capabilities at once can break what dropping
#                    either alone did not.
#   4. add-one       candidate capabilities outside the set under test
#                    (DAC_READ_SEARCH per B.2). A candidate cannot be
#                    required if phase 3 passed without it — that is a
#                    measurement, recorded as such, not a guess.
#   5. cross-check   one verdict the suite CANNOT reach on its own:
#                    privileged ports. Docker sets
#                    net.ipv4.ip_unprivileged_port_start=0 inside every
#                    container it creates, so ports 53/88/389/445 are not
#                    privileged there and NET_BIND_SERVICE measures as
#                    droppable — a property of the test runtime, not of
#                    the image. SPEC B.3 supports host networking, where
#                    the container inherits the HOST's value (1024 on any
#                    ordinary Linux system). This phase binds 389 in the
#                    image with the sysctl forced back to 1024, with and
#                    without the capability, and the answer decides
#                    whether NET_BIND_SERVICE is retained on top of the
#                    measured minimal set.
#
# It then COMPARES its own recommendation to what the image actually
# ships (harness.DefaultCaps) and EXITS NON-ZERO if they differ. That is
# what makes a scheduled or dispatched run worth having: without the
# comparison a green run means "the bisection completed", with it a green
# run means "B.2 is still true".
#
# The lever is the harness's E2E_CAPS override (unset => DefaultCaps,
# set-but-empty => no capabilities, comma list => that list). The set to
# bisect is read out of the harness itself with `go doc`, so this script
# and the suite can never disagree about what is being tested.
#
# The subset is the smoke triple, not the whole suite: provision (writes
# security.* xattrs, binds privileged ports, chowns), kinit (proves the
# served protocols actually work rather than that a container merely came
# up) and restart (the signal and ordered-shutdown path). A full-suite
# bisection would cost hours per capability and probe the same syscalls.
#
# Runtime: 9 runs or more — baseline, one per capability, a confirmation
# re-run of every failure, verify-minimal — plus two sub-second port
# probes. A passing run is a few minutes; a FAILING run can be much
# slower, because a capability-starved DC is sometimes only discovered by
# a health wait that has to expire. Budget an hour and a half.
#
# The report is written INCREMENTALLY: the header lands before the first
# run and every verdict is appended the moment it is measured. A run that
# is cancelled, times out or dies halfway therefore still leaves a report
# holding every verdict it reached — which is the whole reason CI uploads
# the file even when the job failed.
#
# Output: test/capbisect/results-<arch>-<date>.txt (this run) and the
# stable copy test/capbisect/results-<arch>.txt (committed record).
#
# Environment:
#   E2E_IMAGE          image under test (default samba-ad-dc:dev)
#   CAPBISECT_TIMEOUT  per-run `go test -timeout` (default 40m)
#   CAPBISECT_SET      comma list to bisect INSTEAD of harness.DefaultCaps.
#                      The way to re-measure a capability that a previous
#                      bisection removed: a run that starts from the
#                      shipped set can only ever re-confirm the shipped
#                      set, never rediscover what is no longer in it.
set -eu

# --- knobs ------------------------------------------------------------
#
# E2E_IMAGE is passed through to the harness untouched: the bisection must
# run against the same image the suite runs against, whichever that is.
: "${E2E_IMAGE:=samba-ad-dc:dev}"
export E2E_IMAGE
# Per-run `go test -timeout`. Generous on purpose: this value must never
# be what ends a run. Each individual test carries its own deadline
# (harness.HealthTimeout and friends), and those are what should fire when
# a missing capability breaks the image — a test deadline names the thing
# that did not happen, a binary timeout panics the process and skips every
# cleanup.
: "${CAPBISECT_TIMEOUT:=40m}"
# The B.5 smoke triple. TestProvision is anchored so it cannot also select
# TestProvisionOverStateRefused, which is a negative test with a different
# purpose and a much shorter runtime.
SUBSET='TestProvision$|TestKerberosKinit|TestIdempotentRestart'
# Capabilities NOT in the set under test that B.2 names as candidates.
CANDIDATES='DAC_READ_SEARCH'

REPORT=''
KEEP_GOING=0
while [ $# -gt 0 ]; do
    case $1 in
        -o) REPORT=${2:?-o needs a path}; shift 2 ;;
        -k) KEEP_GOING=1; shift ;;   # continue after an aborting failure
        -h|--help) sed -n '2,/^set -eu/p' "$0"; exit 0 ;;
        *) echo "usage: $0 [-o <report>] [-k]" >&2; exit 2 ;;
    esac
done

root=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
outdir=$root/test/capbisect

# Report names carry the architecture because the answer may legitimately
# differ between them: a capability check lives in the kernel, and the two
# runner architectures are two different kernels.
case $(uname -m) in
    arm64|aarch64) arch=arm64 ;;
    x86_64|amd64)  arch=amd64 ;;
    *)             arch=$(uname -m) ;;
esac
date_utc=$(date -u +%Y-%m-%d)
[ -n "$REPORT" ] || REPORT=$outdir/results-$arch-$date_utc.txt
STABLE=$outdir/results-$arch.txt

log() { printf '%s\n' "$*" >&2; }
die() { printf '\n!! %s\n' "$*" >&2; exit 1; }
# emit appends to the report. Everything the report says goes through
# here, so the file on disk is always complete up to the last thing
# measured — never a buffer that only exists if the script reaches its
# own end.
emit() { printf '%s\n' "$*" >>"$REPORT"; }

# --- preflight --------------------------------------------------------
command -v docker >/dev/null 2>&1 || die "the docker CLI is not on PATH"
command -v go >/dev/null 2>&1 || die "the go toolchain is not on PATH"
docker version --format '{{.Server.Version}}' >/dev/null 2>&1 ||
    die "the docker daemon is not reachable"
docker image inspect "$E2E_IMAGE" >/dev/null 2>&1 ||
    die "image $E2E_IMAGE not found; build it first: docker build -t $E2E_IMAGE ."

# The image identity goes in the report: a capability verdict is a
# statement about ONE build, and a report that does not say which build is
# a report nobody can re-check.
image_id=$(docker image inspect --format '{{.Id}}' "$E2E_IMAGE")
docker_version=$(docker version --format '{{.Server.Version}}')
# Read once, up front, so it can go in the header: this number is the
# reason the NET_BIND_SERVICE verdict says what it says, and a header is
# where a reader looks before believing a table.
port_start=$(docker run --rm --cap-drop ALL --entrypoint cat "$E2E_IMAGE" \
    /proc/sys/net/ipv4/ip_unprivileged_port_start 2>/dev/null || echo unknown)

# read_caps_var <GoVarName> -> one capability per line.
#
# The set comes from the harness, not from a copy kept here: the two
# drifting apart would make every verdict below a statement about a set
# nothing actually runs.
read_caps_var() {
    _doc=$(cd "$root/test/e2e" && go doc ./harness "$1") || return 1
    printf '%s\n' "$_doc" | awk -v v="$1" '
        $0 ~ "^var " v " " { inblock = 1; next }
        inblock && /^}/    { exit }
        inblock            { gsub(/[",[:space:]]/, ""); if ($0 != "") print }
    '
}

shipped_caps=$(read_caps_var DefaultCaps) ||
    die "could not read harness.DefaultCaps (go doc failed)"
[ -n "$shipped_caps" ] ||
    die "harness.DefaultCaps parsed as empty; refusing to measure against nothing"

if [ -n "${CAPBISECT_SET:-}" ]; then
    bisect_caps=$(printf '%s' "$CAPBISECT_SET" | tr ',' '\n' |
        sed 's/^[[:space:]]*//; s/[[:space:]]*$//' | grep -v '^$' || true)
    set_source="CAPBISECT_SET override (NOT harness.DefaultCaps)"
    [ -n "$bisect_caps" ] || die "CAPBISECT_SET is set but parsed as empty"
else
    bisect_caps=$shipped_caps
    set_source="harness.DefaultCaps"
fi
log "set under test ($set_source): $(echo "$bisect_caps" | tr '\n' ' ')"

# --- helpers ----------------------------------------------------------

# join_caps <newline-list> -> comma list, which is what E2E_CAPS speaks.
join_caps() {
    printf '%s\n' "$1" | tr '\n' ',' | sed 's/,*$//'
}

# without <newline-list> <cap> -> the list minus that one entry.
without() {
    printf '%s\n' "$1" | grep -v -x -e "$2" || true
}

# Every labeled object left behind belongs to a run that is over. A
# capability-starved run in particular dies in ways that skip cleanups, and
# the suite works with FIXED container names — a leftover `dc1` fails the
# next run on the name and would be scored as "capability required".
# Selection is by the harness's ownership label, so nothing this harness
# did not create can ever be selected.
sweep() {
    for kind in container volume network; do
        case $kind in
            container) ids=$(docker ps -aq --filter label=e2e.harness=1) ;;
            volume)    ids=$(docker volume ls -q --filter label=e2e.harness=1) ;;
            network)   ids=$(docker network ls -q --filter label=e2e.harness=1) ;;
        esac
        printf '%s\n' "$ids" | while read -r id; do
            [ -n "$id" ] || continue
            case $kind in
                container) docker rm -f "$id" >/dev/null 2>&1 || true ;;
                volume)    docker volume rm -f "$id" >/dev/null 2>&1 || true ;;
                network)   docker network rm "$id" >/dev/null 2>&1 || true ;;
            esac
            log "  swept $kind $id"
        done
    done
}

# port_bind_probe [docker args...] -- succeeds when a process in the image
# can bind a privileged port with net.ipv4.ip_unprivileged_port_start
# forced back to the kernel default. Nothing here involves the suite: it
# is one bind() against one port, which is precisely the syscall the
# capability governs.
port_bind_probe() {
    docker run --rm --cap-drop ALL "$@" \
        --sysctl net.ipv4.ip_unprivileged_port_start=1024 \
        --entrypoint python3 "$E2E_IMAGE" \
        -c 'import socket; socket.socket().bind(("0.0.0.0", 389))' \
        >/dev/null 2>&1
}

# run_subset <label> <caps-comma-list-or-empty>
#
# Sets RUN_VERDICT (pass|fail|infra), RUN_SECONDS and RUN_EVIDENCE. A run
# that fails for a reason that is not the image — no daemon, a build
# error, a stale container — is NOT data, and scoring it as "capability
# required" would quietly fabricate a finding. Such a run is retried once
# and, if it fails the same way again, aborts the bisection.
RUN_VERDICT=''
RUN_SECONDS=0
RUN_EVIDENCE=''
run_subset() {
    _label=$1
    _caps=$2
    _attempt=1
    while :; do
        sweep
        _logfile=$outdir/.run-$$.log
        log ""
        log "== $_label"
        log "   E2E_CAPS='$_caps'"
        _start=$(date +%s)
        _code=0
        # E2E_CAPS is exported SET (possibly empty) on purpose: the harness
        # distinguishes unset (use DefaultCaps) from set-and-empty (drop
        # everything), and the empty case is a legitimate point of this
        # measurement.
        ( cd "$root/test/e2e" &&
          E2E_CAPS="$_caps" go test ./... -count=1 -v \
              -run "$SUBSET" -timeout "$CAPBISECT_TIMEOUT" ) \
            >"$_logfile" 2>&1 || _code=$?
        _end=$(date +%s)
        RUN_SECONDS=$((_end - _start))

        if [ "$_code" -eq 0 ]; then
            RUN_VERDICT=pass
            RUN_EVIDENCE=''
            log "   -> PASS in ${RUN_SECONDS}s"
            rm -f "$_logfile"
            return 0
        fi
        # A real test verdict looks like a test verdict. Anything else --
        # a compile error, an unreachable daemon, a missing image -- exits
        # non-zero without ever running a test.
        if grep -q '^--- FAIL\|^ *--- FAIL\|^panic: test timed out' "$_logfile"; then
            RUN_VERDICT=fail
            # Up to three lines of the actual denial, verbatim. A verdict
            # of "required" with no mechanism behind it is an assertion;
            # the mechanism is what makes it reviewable, and it is what
            # tells a future reader whether a Samba upgrade could change
            # the answer.
            #
            # Lines naming the specific operation are preferred over the
            # generic ones, because several capabilities fail through the
            # SAME generic line ("{Access Denied}") and a report whose
            # rows are byte-identical distinguishes nothing. The
            # distinguishing line is usually further down the log than
            # the generic one, so ranking, not first-match, is what
            # picks it.
            RUN_EVIDENCE=$(awk '
                {
                    line = $0
                    sub(/^[[:space:]]+/, "", line)
                    sub(/[[:space:]]+$/, "", line)
                    if (line == "") next
                    if (line ~ /setntacl|set_nt_acl|security\.NTACL|xattr|sys_setgroups|failed to set uid|token stack underflow/) {
                        if (!(line in seen) && np < 3) { seen[line] = 1; pref[np++] = line }
                        next
                    }
                    if (line ~ /INTERNAL ERROR: |ERROR\(runtime\)|Permission denied|Operation not permitted|Access Denied/) {
                        if (!(line in seen) && ng < 3) { seen[line] = 1; gen[ng++] = line }
                    }
                }
                END {
                    n = 0
                    for (i = 0; i < np && n < 3; i++) { print substr(pref[i], 1, 96); n++ }
                    for (i = 0; i < ng && n < 3; i++) { print substr(gen[i], 1, 96); n++ }
                }' "$_logfile")
            _kept=$outdir/.fail-$(printf '%s' "$_label" | tr -cs 'A-Za-z0-9' '-').log
            log "   -> FAIL in ${RUN_SECONDS}s (test verdict; kept: $_kept)"
            [ -z "$RUN_EVIDENCE" ] || log "$RUN_EVIDENCE"
            mv "$_logfile" "$_kept"
            return 0
        fi
        log "   -> INFRASTRUCTURE failure (exit $_code, no test verdict):"
        tail -n 20 "$_logfile" >&2
        if [ "$_attempt" -ge 2 ]; then
            [ "$KEEP_GOING" -eq 1 ] || die "$_label: infrastructure failure twice; aborting.
This is NOT a capability verdict and must not be recorded as one. Fix the
environment (docker daemon, image, leftover containers) and re-run."
            RUN_VERDICT=infra
            return 0
        fi
        log "   retrying once"
        _attempt=$((_attempt + 1))
    done
}

# emit_row <name> <verdict> <seconds> [evidence-block]
emit_row() {
    printf '  %-18s %-11s %5ss\n' "$1" "$2" "$3" >>"$REPORT"
    if [ -n "${4:-}" ]; then
        printf '%s\n' "$4" | sed 's/^/                                   | /' >>"$REPORT"
    fi
}

# --- report header ----------------------------------------------------
started_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
total_start=$(date +%s)

: >"$REPORT"
emit "capability bisection report"
emit "==========================="
emit ""
emit "date (UTC)     : $started_at"
emit "architecture   : $arch ($(uname -s) $(uname -m))"
emit "image          : $E2E_IMAGE"
emit "image digest   : $image_id"
emit "docker server  : $docker_version"
emit "subset         : go test -run '$SUBSET'"
emit "per-run timeout: $CAPBISECT_TIMEOUT"
emit "ip_unpriv_port_start (inside a container of this image): $port_start"
emit ""
emit "method: baseline with the set under test, then one run per capability"
emit "with that capability removed (every failure re-run once to confirm),"
emit "then one run with only the capabilities that proved required. Every"
emit "run is the same subset against the same image; the only variable is"
emit "E2E_CAPS. cap_drop ALL is the floor in all of them."
emit ""
emit "set under test, from $set_source:"
printf '%s\n' "$bisect_caps" | sed 's/^/  /' >>"$REPORT"
emit ""
emit "measurements"
emit "------------"
emit "(appended as each run finishes, so an interrupted run still leaves"
emit "every verdict it reached; evidence lines are quoted from the run log)"
emit ""

# --- phase 1: baseline ------------------------------------------------
run_subset "baseline (full set under test)" "$(join_caps "$bisect_caps")"
baseline_seconds=$RUN_SECONDS
emit_row "baseline" "$(printf '%s' "$RUN_VERDICT" | tr '[:lower:]' '[:upper:]')" "$baseline_seconds"
[ "$RUN_VERDICT" = pass ] || die "BASELINE FAILED with the full set under test.
The bisection has no reference point: every later verdict would be
'required' for the wrong reason. Fix the suite or the image first."

# --- phase 2: drop-one ------------------------------------------------
required=''
droppable=''
flaky=''
for cap in $bisect_caps; do
    caps_minus=$(join_caps "$(without "$bisect_caps" "$cap")")
    run_subset "drop $cap" "$caps_minus"
    cap_seconds=$RUN_SECONDS
    cap_verdict=$RUN_VERDICT
    cap_evidence=$RUN_EVIDENCE
    cap_note=''
    # A single red run is not a capability verdict; it is one observation
    # of a suite that talks to a container runtime over a network. The
    # confirmation run costs one repeat per required capability and buys
    # the difference between "this broke" and "this breaks".
    if [ "$cap_verdict" = fail ]; then
        run_subset "drop $cap (confirmation)" "$caps_minus"
        cap_seconds=$((cap_seconds + RUN_SECONDS))
        if [ "$RUN_VERDICT" = fail ]; then
            [ -n "$cap_evidence" ] || cap_evidence=$RUN_EVIDENCE
        else
            # Kept REQUIRED: the conservative direction. But a capability
            # whose removal fails only sometimes is a fact about this
            # suite that must not disappear into a clean-looking table.
            flaky=$(printf '%s\n%s' "$flaky" "$cap")
            cap_note="FLAKE SUSPECT: the confirmation run PASSED. Verdict kept REQUIRED (conservative); investigate before trusting it."
        fi
    fi
    case $cap_verdict in
        fail)
            required=$(printf '%s\n%s' "$required" "$cap")
            if [ -n "$cap_note" ]; then
                cap_evidence=$(printf '%s\n%s' "$cap_note" "$cap_evidence")
            fi
            emit_row "$cap" REQUIRED "$cap_seconds" "$cap_evidence"
            ;;
        pass)
            droppable=$(printf '%s\n%s' "$droppable" "$cap")
            emit_row "$cap" DROPPABLE "$cap_seconds" "the subset passes without it"
            ;;
        *)
            emit_row "$cap" UNKNOWN "$cap_seconds" "infrastructure failure; NOT measured"
            ;;
    esac
done
required=$(printf '%s\n' "$required" | grep -v '^$' || true)
droppable=$(printf '%s\n' "$droppable" | grep -v '^$' || true)
flaky=$(printf '%s\n' "$flaky" | grep -v '^$' || true)

# --- phase 3: verify the required set is jointly sufficient ------------
minimal=$(join_caps "$required")
if [ -z "$droppable" ]; then
    # Nothing was droppable, so "the required ones only" IS the set under
    # test: the baseline already ran exactly this and re-running it would
    # measure nothing new.
    verify=pass
    verify_seconds=$baseline_seconds
    verify_note="identical to the baseline set (no capability was droppable); baseline run reused"
else
    run_subset "verify minimal ($minimal)" "$minimal"
    verify=$RUN_VERDICT
    verify_seconds=$RUN_SECONDS
    verify_note="measured directly"
fi
emit_row "verify-minimal" "$(printf '%s' "$verify" | tr '[:lower:]' '[:upper:]')" \
    "$verify_seconds" "$verify_note"
if [ "$verify" != pass ]; then
    log "!! VERIFY-MINIMAL FAILED: the individually-required capabilities are
not jointly sufficient. Some interaction needs a capability that looked
droppable on its own. The minimal set is NOT established; treat the set
under test as the answer and investigate."
fi

# --- phase 4: candidates outside the set under test -------------------
for cand in $CANDIDATES; do
    if [ "$verify" = pass ]; then
        emit_row "$cand" "NOT NEEDED" 0 \
            "candidate named by B.2; absent from the verified minimal set, which passes"
    else
        emit_row "$cand" UNKNOWN 0 "not probed: the minimal set is unverified"
    fi
done

# --- phase 5: privileged-port cross-check ------------------------------
#
# Runs unconditionally, including when NET_BIND_SERVICE measured as
# required: the number this reads is the reason the drop-one verdict for
# that capability says what it says, and a report that omits it invites
# the next reader to draw the wrong conclusion from a green run.
if port_bind_probe; then
    probe_without=bound
else
    probe_without=denied
fi
if port_bind_probe --cap-add NET_BIND_SERVICE; then
    probe_with=bound
else
    probe_with=denied
fi
# Retain the capability exactly when the probe shows it is what makes the
# difference at the kernel's own default. Anything else and the retention
# would be a habit rather than a finding.
if [ "$probe_without" = denied ] && [ "$probe_with" = bound ]; then
    retain_nbs=yes
else
    retain_nbs=no
fi
log ""
log "== privileged-port cross-check"
log "   ip_unprivileged_port_start in a container of this image: $port_start"
log "   bind :389 at floor 1024, no NET_BIND_SERVICE   -> $probe_without"
log "   bind :389 at floor 1024, with NET_BIND_SERVICE -> $probe_with"

# The set the image should actually ship with: what the suite proved
# necessary, plus any capability phase 5 proved necessary outside the
# suite's reach. Emitted in the set-under-test's order so it can be
# compared to harness.DefaultCaps by eye.
recommended=''
for cap in $bisect_caps; do
    keep=no
    if printf '%s\n' "$required" | grep -q -x -e "$cap"; then
        keep=yes
    elif [ "$cap" = NET_BIND_SERVICE ] && [ "$retain_nbs" = yes ]; then
        keep=yes
    fi
    if [ "$keep" = yes ]; then
        recommended=$(printf '%s\n%s' "$recommended" "$cap")
    fi
done
recommended=$(printf '%s\n' "$recommended" | grep -v '^$' || true)

sweep
total_seconds=$(($(date +%s) - total_start))

# --- conclusions ------------------------------------------------------
emit ""
emit "MEASURED MINIMAL SET (what the suite alone proves)"
emit "-------------------------------------------------"
if [ "$verify" = pass ]; then
    printf '%s\n' "$required" | sed 's/^/  /' >>"$REPORT"
    emit ""
    emit "  as E2E_CAPS: $minimal"
else
    emit "  NOT ESTABLISHED (verify-minimal did not pass)"
fi
emit ""
emit "PRIVILEGED-PORT CROSS-CHECK"
emit "---------------------------"
emit "  Docker sets net.ipv4.ip_unprivileged_port_start=0 in the network"
emit "  namespace it creates for a container, which makes 53/88/389/445"
emit "  unprivileged there. Under the E2E suite (a user-defined bridge)"
emit "  NET_BIND_SERVICE therefore measures droppable no matter what the"
emit "  image needs. SPEC B.3 also supports HOST networking, where the"
emit "  container inherits the host's value — 1024 on any ordinary Linux"
emit "  system. The probe below forces the floor back to 1024 and binds"
emit "  :389 in this image, with the capability and without it:"
emit ""
emit "    without NET_BIND_SERVICE : $probe_without"
emit "    with    NET_BIND_SERVICE : $probe_with"
emit ""
if [ "$retain_nbs" = yes ]; then
    emit "  => NET_BIND_SERVICE is REQUIRED wherever the port floor is at the"
    emit "     kernel default. Its 'droppable' verdict above is a property of"
    emit "     the test runtime, not of the image, and it is RETAINED in the"
    emit "     shipped profile."
else
    emit "  => the probe did not show NET_BIND_SERVICE making the difference;"
    emit "     it is not retained on this evidence."
fi
emit ""
emit "RECOMMENDED PROFILE SET (what harness.DefaultCaps and B.2 should carry)"
emit "----------------------------------------------------------------------"
if [ "$verify" = pass ]; then
    printf '%s\n' "$recommended" | sed 's/^/  /' >>"$REPORT"
    emit ""
    emit "  = the measured minimal set, plus any capability the cross-check"
    emit "    proved necessary outside the suite's reach."
else
    emit "  NOT ESTABLISHED (verify-minimal did not pass)"
fi
emit ""
if [ -n "$flaky" ]; then
    emit "FLAKE SUSPECTS"
    emit "--------------"
    printf '%s\n' "$flaky" | sed 's/^/  /' >>"$REPORT"
    emit "  (removal failed once and passed on the confirmation run; the"
    emit "   verdict was kept REQUIRED, which is the conservative direction,"
    emit "   but these are NOT clean measurements)"
    emit ""
fi
emit "recorded, not measured"
emit "----------------------"
emit "  CAP_KILL      NOT APPLICABLE  chronyd runs as root in this image"
emit "                                (B.6), so no privilege-dropping"
emit "                                child has to be signalled across a"
emit "                                uid boundary. CAP_KILL is the named"
emit "                                alternative IF chronyd is ever made"
emit "                                to drop privileges; while it does"
emit "                                not, there is nothing to measure."
emit ""

# --- the gate ---------------------------------------------------------
#
# Everything above is a measurement. This is the part that can fail a
# CI job: does what was just measured still agree with what the image
# ships? Without this, a dispatched run could only ever report "the
# bisection completed" — with it, a green run reports "B.2 is still true"
# and a red one names the drift.
recommended_sorted=$(printf '%s\n' "$recommended" | sort)
shipped_sorted=$(printf '%s\n' "$shipped_caps" | sort)
emit "SHIPPED-SET COMPARISON"
emit "----------------------"
emit "  harness.DefaultCaps : $(join_caps "$shipped_caps")"
emit "  recommended by run  : $(join_caps "$recommended")"
gate=0
if [ "$verify" != pass ]; then
    emit "  => INCONCLUSIVE: verify-minimal did not pass, so this run"
    emit "     recommends nothing and cannot confirm the shipped set."
    gate=1
elif [ "$recommended_sorted" = "$shipped_sorted" ]; then
    emit "  => AGREE. The shipped capability set is confirmed by measurement"
    emit "     on this architecture, against this image."
else
    emit "  => DIVERGED. What the image ships is no longer what measurement"
    emit "     says it needs: adaptation-profile B.2 and harness.DefaultCaps"
    emit "     are stale and must be updated to the recommended set above."
    gate=1
fi
emit ""
emit "total runtime: ${total_seconds}s"

# The stable copy is the committed record of the latest run; the dated
# file is this run. When -o already names the stable path the two are the
# same file, and `cp` onto itself is an error -- which would fail the
# script AFTER a completely successful bisection.
if [ "$REPORT" != "$STABLE" ]; then
    cp "$REPORT" "$STABLE"
fi

cat "$REPORT"
log ""
log "report: $REPORT"
log "stable: $STABLE"
if [ "$gate" -ne 0 ]; then
    die "the shipped capability set does not match this measurement (see
SHIPPED-SET COMPARISON in $REPORT). This is the finding, not a crash:
update harness.DefaultCaps and adaptation-profile B.2, or explain in B.2
why a capability is retained despite measuring droppable."
fi
