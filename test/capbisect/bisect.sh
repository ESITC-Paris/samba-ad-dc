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
# So that is what this does, against the real image, over four phases:
#
#   1. baseline      the default set must pass (else the measurement has
#                    no reference point and the script aborts)
#   2. drop-one      for each capability C: run with (default - C).
#                    fail => C is REQUIRED.  pass => C is DROPPABLE.
#   3. verify-min    run with only the required ones. This is the step
#                    that turns a list of individually-necessary
#                    capabilities into a jointly-sufficient set: dropping
#                    two capabilities at once can break what dropping
#                    either alone did not.
#   4. add-one       candidate capabilities outside the default set
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
# The lever is the harness's E2E_CAPS override (unset => DefaultCaps,
# set-but-empty => no capabilities, comma list => that list). The default
# set is read out of the harness itself with `go doc`, so this script and
# the suite can never disagree about what is being tested.
#
# The subset is the smoke triple, not the whole suite: provision (writes
# security.* xattrs, binds privileged ports, chowns), kinit (proves the
# served protocols actually work rather than that a container merely came
# up) and restart (the signal and ordered-shutdown path). A full-suite
# bisection would cost hours per capability and probe the same syscalls.
#
# Runtime: about 7 runs. A passing run is a few minutes; a FAILING run is
# slower, because a capability-starved DC is discovered by a health wait
# that has to expire. Budget an hour.
#
# Output: a report file (table capability -> verdict, with the subset, the
# image digest and per-run timings) plus the stable, committed copy
# test/capbisect/results-<arch>.txt.
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
# Capabilities NOT in the default set that B.2 names as candidates.
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
[ -n "$REPORT" ] || REPORT=$outdir/capbisect-report-$arch-$date_utc.txt
STABLE=$outdir/results-$arch.txt

log() { printf '%s\n' "$*" >&2; }
die() { printf '\n!! %s\n' "$*" >&2; exit 1; }

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

# The default set comes from the harness, not from a copy kept here: the
# two drifting apart would make every verdict below a statement about a
# set nothing actually runs.
default_caps=$(cd "$root/test/e2e" && go doc ./harness DefaultCaps) ||
    die "could not read harness.DefaultCaps (go doc failed)"
default_caps=$(printf '%s\n' "$default_caps" | awk '
    /^var DefaultCaps/ { inblock = 1; next }
    inblock && /^}/    { exit }
    inblock            { gsub(/[",[:space:]]/, ""); if ($0 != "") print }
')
[ -n "$default_caps" ] || die "harness.DefaultCaps parsed as empty; refusing to bisect nothing"
log "default set: $(echo "$default_caps" | tr '\n' ' ')"

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
# Sets RUN_VERDICT (pass|fail) and RUN_SECONDS. A run that fails for a
# reason that is not the image — no daemon, a build error, a stale
# container — is NOT data, and scoring it as "capability required" would
# quietly fabricate a finding. Such a run is retried once and, if it fails
# the same way again, aborts the bisection.
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
            # The first denial in the log, verbatim (trimmed). A verdict of
            # "required" with no mechanism behind it is an assertion; the
            # mechanism is what makes it reviewable, and it is what tells a
            # future reader whether a Samba upgrade could change the answer.
            RUN_EVIDENCE=$(awk '
                /INTERNAL ERROR: |ERROR\(runtime\)|Permission denied|Operation not permitted|Access Denied/ {
                    sub(/^[[:space:]]+/, ""); print substr($0, 1, 96); exit
                }' "$_logfile")
            _kept=$outdir/.fail-$(printf '%s' "$_label" | tr -cs 'A-Za-z0-9' '-').log
            log "   -> FAIL in ${RUN_SECONDS}s (test verdict; kept: $_kept)"
            [ -z "$RUN_EVIDENCE" ] || log "      $RUN_EVIDENCE"
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

# --- report -----------------------------------------------------------
rows=''            # "CAP<TAB>VERDICT<TAB>NOTE" lines, accumulated
row() { rows=$(printf '%s\n%s\t%s\t%s' "$rows" "$1" "$2" "$3"); }

started_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
total_start=$(date +%s)

# --- phase 1: baseline ------------------------------------------------
run_subset "baseline (full default set)" "$(join_caps "$default_caps")"
baseline_seconds=$RUN_SECONDS
[ "$RUN_VERDICT" = pass ] || die "BASELINE FAILED with the full default set.
The bisection has no reference point: every later verdict would be
'required' for the wrong reason. Fix the suite or the image first."

# --- phase 2: drop-one ------------------------------------------------
required=''
droppable=''
timings=$(printf 'baseline\t%ss' "$baseline_seconds")
for cap in $default_caps; do
    run_subset "drop $cap" "$(join_caps "$(without "$default_caps" "$cap")")"
    timings=$(printf '%s\ndrop %s\t%ss' "$timings" "$cap" "$RUN_SECONDS")
    case $RUN_VERDICT in
        fail)
            required=$(printf '%s\n%s' "$required" "$cap")
            row "$cap" REQUIRED "${RUN_EVIDENCE:-suite fails without it}"
            ;;
        pass)
            droppable=$(printf '%s\n%s' "$droppable" "$cap")
            row "$cap" DROPPABLE "suite passes without it"
            ;;
        *)
            row "$cap" UNKNOWN "infrastructure failure; not measured"
            ;;
    esac
done
required=$(printf '%s\n' "$required" | grep -v '^$' || true)
droppable=$(printf '%s\n' "$droppable" | grep -v '^$' || true)

# --- phase 3: verify the required set is jointly sufficient ------------
minimal=$(join_caps "$required")
if [ -z "$droppable" ]; then
    # Nothing was droppable, so "the required ones only" IS the default
    # set: the baseline already ran exactly this and re-running it would
    # measure nothing new.
    verify=pass
    verify_seconds=$baseline_seconds
    verify_note="identical to the baseline set (no capability was droppable); baseline run reused"
else
    run_subset "verify minimal ($minimal)" "$minimal"
    verify=$RUN_VERDICT
    verify_seconds=$RUN_SECONDS
    verify_note="measured directly"
    timings=$(printf '%s\nverify-minimal\t%ss' "$timings" "$verify_seconds")
    [ "$verify" = pass ] || log "!! VERIFY-MINIMAL FAILED: the individually-required
capabilities are not jointly sufficient. Some interaction needs a
capability that looked droppable on its own. The minimal set below is NOT
established; treat the default set as the answer and investigate."
fi

# --- phase 4: candidates outside the default set ----------------------
for cand in $CANDIDATES; do
    if [ "$verify" = pass ]; then
        row "$cand" "NOT NEEDED" "absent from the verified minimal set, which passes"
    else
        row "$cand" UNKNOWN "not probed: the minimal set is unverified"
    fi
done

# --- phase 5: privileged-port cross-check ------------------------------
#
# Runs unconditionally, including when NET_BIND_SERVICE measured as
# required: the number this reads is the reason the drop-one verdict for
# that capability says what it says, and a report that omits it invites
# the next reader to draw the wrong conclusion from a green run.
port_start=$(docker run --rm --cap-drop ALL --entrypoint cat "$E2E_IMAGE" \
    /proc/sys/net/ipv4/ip_unprivileged_port_start 2>/dev/null || echo unknown)
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
log "   bind :389 at floor 1024, no NET_BIND_SERVICE  -> $probe_without"
log "   bind :389 at floor 1024, with NET_BIND_SERVICE -> $probe_with"

# The set the image should actually ship with: what the suite proved
# necessary, plus any capability phase 5 proved necessary outside the
# suite's reach. Emitted in the default set's order so it can be compared
# to harness.DefaultCaps by eye.
recommended=''
for cap in $default_caps; do
    _keep=no
    if printf '%s\n' "$required" | grep -q -x -e "$cap"; then
        _keep=yes
    elif [ "$cap" = NET_BIND_SERVICE ] && [ "$retain_nbs" = yes ]; then
        _keep=yes
    fi
    if [ "$_keep" = yes ]; then
        recommended=$(printf '%s\n%s' "$recommended" "$cap")
    fi
done
recommended=$(printf '%s\n' "$recommended" | grep -v '^$' || true)

sweep
total_seconds=$(($(date +%s) - total_start))

{
    echo "capability bisection report"
    echo "==========================="
    echo
    echo "date (UTC)     : $started_at"
    echo "architecture   : $arch ($(uname -s) $(uname -m))"
    echo "image          : $E2E_IMAGE"
    echo "image digest   : $image_id"
    echo "docker server  : $docker_version"
    echo "subset         : go test -run '$SUBSET'"
    echo "per-run timeout: $CAPBISECT_TIMEOUT"
    echo "ip_unpriv_port_start (inside a container of this image): $port_start"
    echo
    echo "method: baseline with the harness default set, then one run per"
    echo "capability with that capability removed, then one run with only"
    echo "the capabilities the drop-one phase proved required. Every run is"
    echo "the same subset against the same image; the only variable is"
    echo "E2E_CAPS. cap_drop ALL is the floor in all of them."
    echo
    echo "default set under test (harness.DefaultCaps):"
    printf '%s\n' "$default_caps" | sed 's/^/  /'
    echo
    echo "verdicts"
    echo "--------"
    printf '%s\n' "$rows" | grep -v '^$' |
        awk -F'\t' '{ printf "  %-18s %-11s %s\n", $1, $2, $3 }'
    echo
    echo "verify-minimal : $verify ($verify_note)"
    echo
    echo "MEASURED MINIMAL SET"
    echo "--------------------"
    if [ "$verify" = pass ]; then
        printf '%s\n' "$required" | sed 's/^/  /'
        echo
        echo "  as E2E_CAPS: $minimal"
    else
        echo "  NOT ESTABLISHED (verify-minimal did not pass)"
    fi
    echo
    echo "PRIVILEGED-PORT CROSS-CHECK"
    echo "---------------------------"
    echo "  Docker sets net.ipv4.ip_unprivileged_port_start=0 in the network"
    echo "  namespace it creates for a container, which makes 53/88/389/445"
    echo "  unprivileged there. Under the E2E suite (a user-defined bridge)"
    echo "  NET_BIND_SERVICE therefore measures droppable no matter what the"
    echo "  image needs. SPEC B.3 also supports HOST networking, where the"
    echo "  container inherits the host's value — 1024 on any ordinary Linux"
    echo "  system. The probe below forces the floor back to 1024 and binds"
    echo "  :389 in this image, with the capability and without it:"
    echo
    echo "    without NET_BIND_SERVICE : $probe_without"
    echo "    with    NET_BIND_SERVICE : $probe_with"
    echo
    if [ "$retain_nbs" = yes ]; then
        echo "  => NET_BIND_SERVICE is REQUIRED wherever the port floor is at the"
        echo "     kernel default. Its 'droppable' verdict above is a property of"
        echo "     the test runtime, not of the image, and it is RETAINED in the"
        echo "     shipped profile."
    else
        echo "  => the probe did not show NET_BIND_SERVICE making the difference;"
        echo "     it is not retained on this evidence."
    fi
    echo
    echo "RECOMMENDED PROFILE SET (what harness.DefaultCaps and B.2 carry)"
    echo "----------------------------------------------------------------"
    if [ "$verify" = pass ]; then
        printf '%s\n' "$recommended" | sed 's/^/  /'
        echo
        echo "  = the measured minimal set, plus any capability the cross-check"
        echo "    proved necessary outside the suite's reach."
    else
        echo "  NOT ESTABLISHED (verify-minimal did not pass)"
    fi
    echo
    echo "recorded, not measured"
    echo "----------------------"
    echo "  CAP_KILL      NOT APPLICABLE  chronyd runs as root in this image"
    echo "                                (B.6), so no privilege-dropping"
    echo "                                child has to be signalled across a"
    echo "                                uid boundary. CAP_KILL is the named"
    echo "                                alternative IF chronyd is ever made"
    echo "                                to drop privileges; while it does"
    echo "                                not, there is nothing to measure."
    echo
    echo "timings"
    echo "-------"
    printf '%s\n' "$timings" | awk -F'\t' '{ printf "  %-22s %s\n", $1, $2 }'
    printf '  %-22s %ss\n' total "$total_seconds"
} | tee "$REPORT"

cp "$REPORT" "$STABLE"
log ""
log "report: $REPORT"
log "stable: $STABLE"
