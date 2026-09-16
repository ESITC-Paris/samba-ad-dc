#!/usr/bin/env python3
"""The release watcher: decide what, if anything, this catalog owes today.

SPEC §9bis in one process. Three verbs, deliberately separable so the
part that decides never touches the network and the part that touches the
network never decides:

  observe   probe every monitored source and print one observation
            document (JSON). Network, docker and `gh` live here.
  plan      turn an observation into a list of decisions. Pure: same
            inputs, same output, no I/O, fully table-tested.
  apply     carry the decisions out — edit the catalog, the Dockerfile
            mirror, the build state, the CHANGELOG and the README matrix,
            and print the tags to create and the branches to dispatch.

Usage:
  python3 scripts/watch.py observe [--output obs.json] [--dry-run]
  python3 scripts/watch.py plan --observe obs.json [--now ISO]
                                [--soak-hours 24] [--security]
                                [--output plan.json]
  python3 scripts/watch.py apply --decisions plan.json [--dry-run]
  python3 scripts/watch.py published --observe obs.json

Exit codes: 0 success, 2 refusal (a probe that cannot be trusted, a
branch that is not in the catalog, a malformed observation).

The decision order is fixed and is the whole contract (§9bis.8, necessity
criterion — a build is dispatched only when it is CERTAIN to produce an
image different from the last published one):

  1. a strictly greater upstream patch release, after its soak  -> version
  2. a moved base-image digest                                  -> revision
  3. a moved package-closure hash                               -> revision
  4. the catalog's current tag missing from the registry        -> publish
  5. otherwise                                                  -> none

Steps 2 and 3 are the two halves of "the image would differ even though
Samba did not move"; step 4 is the self-heal that makes the first
publication, and any lost dispatch, converge on the next hourly run
without a human.

`apply` prints `key=value` lines on STDOUT and progress on STDERR, so the
workflow can redirect stdout straight into $GITHUB_OUTPUT.
"""

import argparse
import datetime
import gzip
import io
import json
import os
import re
import subprocess
import sys
import time
import urllib.error
import urllib.request

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import catalog  # noqa: E402  (needs the sys.path line above)

UTC = datetime.timezone.utc

STABLE_URL = "https://download.samba.org/pub/samba/stable/"
RC_URL = "https://download.samba.org/pub/samba/rc/"

# GHCR lowercases the organisation; the GitHub API path does not care, but
# the image reference does, and having one spelling here keeps the two from
# drifting.
DEFAULT_ORG = "esitc-paris"
DEFAULT_IMAGE = "samba-ad-dc"

# A release tarball and nothing else. The patch number must be followed
# immediately by `.tar.gz`, which is what excludes `samba-4.24.0rc4.tar.gz`
# — release candidates live in their own directory but have leaked into the
# stable listing before, and a candidate published as a release would be an
# auto-published beta.
STABLE_TARBALL_RE = re.compile(r"samba-(\d+)\.(\d+)\.(\d+)\.tar\.gz")
RC_TARBALL_RE = re.compile(r"samba-(\d+)\.(\d+)\.(\d+)rc(\d+)\.tar\.gz")

DIGEST_RE = re.compile(r"^sha256:[0-9a-f]{64}$")
CLOSURE_HASH_RE = re.compile(r"^[0-9a-f]{16}$")

# The three base pins, and the state key each one is remembered under.
BASE_DIGEST_KEYS = (("runtime", "runtime_digest"),
                    ("builder", "builder_digest"),
                    ("gobuild", "gobuild_digest"))

# ARG defaults the Dockerfile mirrors from the catalog's DEFAULT branch.
# `scripts/check-pins-consistency.sh` asserts every one of these pairs, so
# a catalog edit that leaves the Dockerfile behind turns CI red on the very
# commit the watcher just pushed. PKG_INDEX_HASH is deliberately absent:
# its ARG default is the fallback for a bare `docker build .` and CI
# injects the catalog value, which that script states and asserts.
MIRRORED_ARGS = ("SAMBA_VERSION", "SAMBA_TARBALL_SHA256", "BUILDER_BASE",
                 "RUNTIME_BASE", "GOBUILD_BASE", "BASE_NAME", "BASE_DIGEST")


class Refusal(Exception):
    """A refusal with an exit code: the caller prints it and exits."""

    def __init__(self, message, code=2):
        super().__init__(message)
        self.code = code


class CommandFailed(Exception):
    """A subprocess that exited non-zero; the caller decides what it means."""

    def __init__(self, output, returncode):
        super().__init__(output)
        self.output = output
        self.returncode = returncode


class Response:
    """Just enough of an HTTP response for the two listings we read."""

    def __init__(self, status, body, headers=None):
        self.status = status
        self.body = body
        self._headers = dict(headers or {})

    def header(self, name):
        # Header names are case-insensitive, and the two sources disagree
        # about capitalisation between HTTP/1.1 and HTTP/2.
        for key, value in self._headers.items():
            if key.lower() == name.lower():
                return value
        return ""


# --------------------------------------------------------------------------
# Pure helpers (no I/O below this line until the probes)


def version_key(version):
    """Sort key with `sort -V` semantics over X.Y.Z.

    String order puts 4.24.9 above 4.24.10, which would make the
    monotonicity guard reject the very release it exists to let through.
    """
    parts = []
    for chunk in str(version).split("."):
        digits = re.match(r"\d+", chunk)
        parts.append(int(digits.group()) if digits else 0)
    return tuple(parts)


def series_of(version):
    """`4.24.7` -> `4.24`."""
    parts = str(version).split(".")
    return ".".join(parts[:2])


def parse_stable_listing(html):
    """Every X.Y.Z with a published release tarball, ascending, deduplicated.

    Read off the tarball names rather than the table structure: the server
    is an Apache autoindex whose markup is not a contract, while the file
    names are (they are what the build downloads).
    """
    found = {"%s.%s.%s" % match.groups()
             for match in STABLE_TARBALL_RE.finditer(html or "")}
    return sorted(found, key=version_key)


def parse_rc_listing(html):
    """Every X.Y.ZrcN with a published candidate tarball, ascending.

    Tarballs only. The directory keeps a folder per past candidate
    (`4.24.0rc/`), and reading series from those would report every series
    that ever had one.
    """
    found = {}
    for match in RC_TARBALL_RE.finditer(html or ""):
        major, minor, patch, candidate = match.groups()
        name = "%s.%s.%src%s" % (major, minor, patch, candidate)
        found[name] = (int(major), int(minor), int(patch), int(candidate))
    return sorted(found, key=lambda name: found[name])


def rc_series(html):
    """The series that have a published release candidate, ascending."""
    return sorted({series_of(name) for name in parse_rc_listing(html)},
                  key=version_key)


def latest_patch(versions, branch):
    """The highest X.Y.Z of `branch` in `versions`, or "" when it has none."""
    owned = [version for version in versions if series_of(version) == branch]
    return max(owned, key=version_key) if owned else ""


def parse_time(text):
    value = datetime.datetime.fromisoformat(str(text).replace("Z", "+00:00"))
    # A naive timestamp in the state file would raise on comparison. Treat
    # it as UTC: everything this tool writes is UTC, so the only way to get
    # one is a hand edit, and refusing it would fail the run over a
    # formatting detail.
    return value if value.tzinfo else value.replace(tzinfo=UTC)


def decide(branch, entry, state, obs, now, soak_hours, security=False):
    """The decision table for one branch. Pure.

    `entry` is the catalog entry, `state` what the last run remembered,
    `obs` this run's observation for the branch (plus `published_tags`).
    Returns a decision record; `apply()` is the only thing that acts on it.
    """
    notes = []
    current_version = str(entry["samba_version"])
    current_tag = "%s-r%s" % (current_version, entry["revision"])
    decision = {
        "branch": branch,
        "action": "none",
        "cause": "",
        "version": current_version,
        "tag": current_tag,
        "state": {},
        "pending": None,
        "clear_pending": False,
        "notes": notes,
    }

    observed = {name: obs.get(name) or ""
                for _, name in BASE_DIGEST_KEYS}
    observed["pkg_index_hash"] = obs.get("pkg_index_hash") or ""
    pending = state.get("pending") or {}
    latest = obs.get("latest_patch") or ""

    # 1/2 — upstream patch release, behind the §9bis.5 soak.
    if latest and version_key(latest) > version_key(current_version):
        required = 0 if security else float(soak_hours)
        # A pending record for a DIFFERENT version is not this version's
        # clock: 4.24.9 appearing while 4.24.8 soaked restarts the soak
        # rather than inheriting the older one's elapsed time.
        first_seen = pending.get("first_seen") \
            if pending.get("version") == latest else None
        if required > 0:
            if first_seen is None:
                decision["pending"] = {"version": latest,
                                       "first_seen": now.isoformat()}
                notes.append("%s seen for the first time; soak of %g h starts"
                             % (latest, required))
                return decision
            elapsed = (now - parse_time(first_seen)).total_seconds() / 3600.0
            remaining = required - elapsed
            if remaining > 0:
                notes.append("%s is soaking; %.1f h remaining"
                             % (latest, remaining))
                return decision
            notes.append("%s soaked for %.1f h" % (latest, elapsed))
        else:
            # §9bis.5: security fixes bypass the delay. No pending record is
            # written — there is nothing left to wait for, and one written
            # here would outlive the bump that immediately follows it.
            notes.append("security run: %s released without a soak" % latest)
        decision.update(action="version", cause="samba-release",
                        version=latest, tag="%s-r1" % latest,
                        clear_pending=True)
        decision["state"] = {key: value for key, value in observed.items()
                             if value}
        return decision

    if latest and version_key(latest) < version_key(current_version):
        # The monotonicity guard. A partial listing or a stale mirror must
        # never be able to auto-publish a downgrade as :latest — so it is
        # ignored, loudly, and the run carries on to the other triggers.
        notes.append("WARNING: upstream reports %s for branch %s, below the "
                     "pinned %s — ignoring it" % (latest, branch,
                                                  current_version))

    if pending and latest:
        # Control only reaches here when the listing offers nothing above
        # the pin, so no pending record can ever be confirmed by it: the
        # version was withdrawn, or the pin has since overtaken it. Left in
        # place it would be compared against forever. `latest` must be
        # non-empty to say that — an absent listing is no information, and
        # discarding a soak clock on no information restarts the soak.
        notes.append("forgetting the pending %s: upstream's newest for this "
                     "branch is %s, and the catalog pins %s"
                     % (pending.get("version"), latest, current_version))
        decision["clear_pending"] = True

    # 3 — a moved base-image digest (§9bis.1.c). An empty observed digest is
    # NOT a change: it is the absence of an answer, and writing it into the
    # state would make every later run bump again.
    moved = [name for _, name in BASE_DIGEST_KEYS
             if observed[name] and observed[name] != state.get(name)]
    if moved:
        notes.append("base digest moved: %s" % ", ".join(sorted(moved)))
        decision.update(action="revision", cause="base-digest")
        decision["state"] = {key: value for key, value in observed.items()
                             if value}
        return decision

    # 4 — a moved package closure (§9bis.1.c second half, §9bis.8.b).
    if observed["pkg_index_hash"] \
            and observed["pkg_index_hash"] != state.get("pkg_index_hash"):
        notes.append("package closure moved: %s -> %s"
                     % (state.get("pkg_index_hash") or "unset",
                        observed["pkg_index_hash"]))
        decision.update(action="revision", cause="pkg-update")
        decision["state"] = {key: value for key, value in observed.items()
                             if value}
        return decision

    # 5 — the self-heal. Everything agrees, but the tag the catalog names is
    # not on the registry: either nothing was ever published for this branch,
    # or a dispatch was lost. Nothing is edited; the tag is created if it is
    # missing and the release is dispatched against it.
    published = obs.get("published_tags") or []
    if current_tag not in published:
        notes.append("%s is not published" % current_tag)
        decision.update(action="publish", cause="first-publication")
        return decision

    notes.append("up to date at %s" % current_tag)
    return decision


def series_decisions(branches, stable_versions, rc_versions):
    """Lifecycle relay (§9bis.1.e, §9.4). Once per run, not per branch."""
    decisions = []
    if not branches:
        return decisions
    newest = max(branches, key=version_key)
    oldest = min(branches, key=version_key)

    stable_series = sorted({series_of(v) for v in stable_versions},
                           key=version_key)
    if stable_series and stable_series[-1] not in branches \
            and version_key(stable_series[-1]) > version_key(newest):
        series = stable_series[-1]
        decisions.append({
            "branch": "",
            "action": "new_series",
            "series": series,
            "title": "New upstream series %s — catalog decision required"
                     % series,
            "body": "Upstream published a stable release of series %s, which "
                    "is not in `versions.yaml`.\n\nThis is a human decision "
                    "(§3.6): adding the series means publishing it on every "
                    "release from then on, and the oldest branch leaves the "
                    "catalog when upstream ends it.\n\nAdd the branch with "
                    "`scripts/verify-upstream-tarball.sh <X.Y.Z>` and a "
                    "reviewed edit to `versions.yaml`."
                    % series,
            "notes": [],
        })

    candidates = sorted({series_of(v) for v in rc_versions}, key=version_key)
    if candidates and version_key(candidates[-1]) > version_key(newest):
        series = candidates[-1]
        decisions.append({
            "branch": "",
            "action": "rc_series",
            "series": series,
            "oldest": oldest,
            "title": "Deprecation pending: branch %s (%s rc published)"
                     % (oldest, series),
            "body": "Upstream published a release candidate for series %s. "
                    "Per §9.4 that is upstream's own end-of-life signal for "
                    "the oldest branch it still supports, so the README "
                    "matrix now carries the deprecation-pending suffix on "
                    "branch %s.\n\nIt is a notice, not a removal: %s keeps "
                    "receiving every patch release until upstream actually "
                    "ends it." % (series, oldest, oldest),
            "notes": [],
        })
    return decisions


def plan(root, observation, now, soak_hours, security=False):
    """Every decision this run owes, branch decisions first."""
    catalogue = catalog.load_catalog(root)
    state = catalog.load_state(root)
    published = observation.get("published_tags") or []

    decisions = []
    for branch in catalog.sorted_branches(catalogue):
        obs = dict((observation.get("branches") or {}).get(branch) or {})
        obs["published_tags"] = published
        decisions.append(decide(
            branch, catalog.entry_of(catalogue, branch),
            state["branches"].get(branch, {}), obs, now, soak_hours,
            security))

    decisions.extend(series_decisions(
        catalog.sorted_branches(catalogue),
        observation.get("stable_versions") or [],
        observation.get("rc_versions") or []))
    return decisions


# --------------------------------------------------------------------------
# Probes — everything below here talks to the world


def run_command(argv, timeout=900):
    """Run a command, return its stdout, raise CommandFailed on non-zero."""
    completed = subprocess.run(argv, stdout=subprocess.PIPE,
                               stderr=subprocess.PIPE, timeout=timeout,
                               check=False)
    stdout = completed.stdout.decode("utf-8", "replace")
    if completed.returncode != 0:
        raise CommandFailed(
            (stdout + completed.stderr.decode("utf-8", "replace")).strip(),
            completed.returncode)
    return stdout


def fetch_url(url, headers=None, timeout=30):
    """GET `url`, transparently gunzipping a gzip-encoded body.

    gzip is asked for because §9bis.2 requires politeness toward sources
    and it is 20x cheaper here: measured 2026-09-16, the stable listing is
    319 KB plain and 14 KB gzipped, fetched every hour forever.
    """
    request = urllib.request.Request(url, headers=dict(headers or {}))
    request.add_header("Accept-Encoding", "gzip")
    request.add_header("User-Agent",
                       "samba-ad-dc-watcher (+%s)" % catalog.REPO_URL)
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            raw = response.read()
            if (response.headers.get("Content-Encoding") or "").lower() \
                    == "gzip":
                raw = gzip.GzipFile(fileobj=io.BytesIO(raw)).read()
            return Response(response.status, raw.decode("utf-8", "replace"),
                            dict(response.headers.items()))
    except urllib.error.HTTPError as error:
        if error.code == 304:
            # Not an error: it is the answer the conditional request asked
            # for. urllib raises on any 3xx it does not follow.
            return Response(304, "", dict(error.headers.items()))
        raise


def fetch_listing(url, cache, parser, fetch=fetch_url, sleep=time.sleep,
                  attempts=3):
    """Read one directory listing, conditionally, and update its cache.

    Returns (versions, cache). §9bis.2 wants conditional requests; measured
    2026-09-16, download.samba.org sends neither `ETag` nor `Last-Modified`
    on these listings and answers an `If-Modified-Since` with a full 200.
    The validators are therefore sent whenever we hold one and stored
    whenever one is offered — the polite behaviour costs nothing and starts
    working by itself the day the server grows it — while the cached
    version list is only ever used on an actual 304.

    The cache holds ONLY what the server served: the validators and the
    parsed version list. Nothing that changes just because time passed —
    no "checked_at" — because this dict is written to `.build-state.json`,
    and a state file that differs on every run is a commit and a push to
    `main` every hour carrying no information. Idempotency (§9bis.3) is
    the property that an unchanged world produces an unchanged file.
    """
    cache = dict(cache or {})
    headers = {}
    if cache.get("etag"):
        headers["If-None-Match"] = cache["etag"]
    if cache.get("last_modified"):
        headers["If-Modified-Since"] = cache["last_modified"]

    last_error = None
    for attempt in range(1, attempts + 1):
        try:
            response = fetch(url, headers=headers)
        except Exception as error:  # noqa: BLE001 - retried, then reported
            last_error = error
            note("listing %s attempt %d failed: %s" % (url, attempt, error))
            if attempt < attempts:
                sleep(5 * attempt)
            continue

        if response.status == 304:
            versions = list(cache.get("versions") or [])
            if not versions:
                # A 304 with nothing cached is unusable: we would report
                # "upstream has no releases", which reads as "no action" and
                # silently stops the watcher. Ask again without validators.
                headers = {}
                last_error = Refusal("304 for %s with an empty cache" % url)
                if attempt < attempts:
                    sleep(5 * attempt)
                continue
            # Returned unchanged: a 304 says the listing is what the
            # cache already describes, so there is nothing to write.
            return versions, cache

        versions = parser(response.body)
        if not versions:
            last_error = Refusal("%s returned no release tarballs (status %s)"
                                 % (url, response.status))
            if attempt < attempts:
                sleep(5 * attempt)
            continue

        cache = {"etag": response.header("ETag"),
                 "last_modified": response.header("Last-Modified"),
                 "versions": versions}
        return versions, cache

    raise Refusal("could not read %s after %d attempts: %s"
                  % (url, attempts, last_error))


def resolve_digest(ref, run=run_command, sleep=time.sleep, attempts=3):
    """The manifest digest a floating base tag resolves to, right now.

    Retried: registry reads rate-limit and hiccup, and a red run every time
    would be noise. An empty or malformed answer is a hard failure, never a
    value — an empty digest compares unequal to the stored one, which would
    trigger a revision release and then write "" into the state, making
    every subsequent run bump again.
    """
    last = ""
    for attempt in range(1, attempts + 1):
        try:
            output = run(["docker", "buildx", "imagetools", "inspect", ref,
                          "--format", "{{println .Manifest.Digest}}"])
        except CommandFailed as error:
            last = error.output
            output = ""
        digest = next((line.strip() for line in (output or "").splitlines()
                       if line.strip()), "")
        if DIGEST_RE.match(digest):
            return digest
        last = digest or last
        note("digest probe for %s attempt %d gave %r" % (ref, attempt, last))
        if attempt < attempts:
            sleep(10)
    raise Refusal("could not resolve a manifest digest for %s after %d "
                  "attempts (last answer: %r)" % (ref, attempts, last))


def package_closure_hash(root, ref, run=run_command):
    """The 16-hex package-closure hash of `ref` (SPEC §9bis.1.c).

    Delegated to scripts/pkg-closure-hash.sh, which is THE definition of
    that value — the same script the Phase 2 seed and a local check call.
    A second implementation here would be a second opinion about when a
    rebuild is owed, which is exactly the drift that script exists to
    prevent. Only the shape of its answer is validated.
    """
    script = os.path.join(root, "scripts", "pkg-closure-hash.sh")
    try:
        output = run(["sh", script, ref])
    except CommandFailed as error:
        raise Refusal("package closure probe failed for %s: %s"
                      % (ref, error.output))
    value = (output or "").strip()
    if not CLOSURE_HASH_RE.match(value):
        raise Refusal("package closure probe for %s printed %r, which is not "
                      "a 16-hex hash" % (ref, value))
    return value


def published_tags(image, org=DEFAULT_ORG, run=run_command):
    """Image tags currently on GHCR for this package.

    A package that has never been published answers 404, which is the
    normal state of this repository until the first release — "no tags",
    not a failure. Anything else is a real failure (a missing
    `packages: read` scope answers 403, and swallowing that would make the
    watcher re-publish every hour).
    """
    try:
        output = run(["gh", "api",
                      "/orgs/%s/packages/container/%s/versions" % (org, image),
                      "--paginate", "--jq",
                      ".[].metadata.container.tags[]"])
    except CommandFailed as error:
        text = (error.output or "").lower()
        if "404" in text or "not found" in text:
            note("no package %s/%s on GHCR yet: no published tags"
                 % (org, image))
            return []
        raise Refusal("could not list published tags for %s/%s: %s"
                      % (org, image, error.output))
    return sorted({line.strip() for line in output.splitlines()
                   if line.strip()})


def observe(root, image=DEFAULT_IMAGE, org=DEFAULT_ORG, fetch=fetch_url,
            run=run_command, sleep=time.sleep, now=None, write_state=True):
    """Probe every monitored source once and return the observation."""
    now = now or datetime.datetime.now(UTC)
    catalogue = catalog.load_catalog(root)
    state = catalog.load_state(root)
    sources = state.setdefault("sources", {})

    stable_versions, sources["stable"] = fetch_listing(
        STABLE_URL, sources.get("stable"), parse_stable_listing,
        fetch=fetch, sleep=sleep)
    rc_versions, sources["rc"] = fetch_listing(
        RC_URL, sources.get("rc"), parse_rc_listing, fetch=fetch, sleep=sleep)

    # Three branches share one base image today, so the probes are memoised
    # per reference: one registry read and one apt dry-run instead of three
    # identical ones. The key is the reference itself, so the day a branch
    # pins a different base they are probed separately without a change here.
    digests = {}
    hashes = {}
    branches = {}
    for branch in catalog.sorted_branches(catalogue):
        entry = catalog.entry_of(catalogue, branch)
        per_branch = {"latest_patch": latest_patch(stable_versions, branch)}
        for key, name in BASE_DIGEST_KEYS:
            ref = entry["base"][key]
            floating = ref.split("@", 1)[0]
            if floating not in digests:
                digests[floating] = resolve_digest(floating, run=run,
                                                   sleep=sleep)
            per_branch[name] = digests[floating]
        # Against the PINNED runtime reference, not the digest just
        # resolved: the hash answers "did the package set for the base we
        # build from move", and the base we build from is the pin.
        runtime_ref = entry["base"]["runtime"]
        if runtime_ref not in hashes:
            hashes[runtime_ref] = package_closure_hash(root, runtime_ref,
                                                       run=run)
        per_branch["pkg_index_hash"] = hashes[runtime_ref]
        branches[branch] = per_branch

    document = {
        "generated_at": now.isoformat(),
        "image": image,
        "org": org,
        "stable_versions": stable_versions,
        "rc_versions": rc_versions,
        "published_tags": published_tags(image, org=org, run=run),
        "branches": branches,
    }
    if write_state:
        catalog.save_state(root, state)
    return document


# --------------------------------------------------------------------------
# apply


def sync_dockerfile_args(root, entry, dry_run=False):
    """Mirror the default branch's pins into the Dockerfile ARG defaults.

    The Dockerfile carries those pins so that a bare `docker build .` with
    no --build-arg reproduces the pinned build, and
    `scripts/check-pins-consistency.sh` asserts every pair against
    versions.yaml's default branch. The watcher therefore cannot move the
    catalog alone: it would push a commit whose own CI is red. Line-targeted
    substitution, the same discipline catalog.py uses on versions.yaml, so
    the file's comments survive.

    Returns the list of ARG names changed.
    """
    path = os.path.join(root, "Dockerfile")
    if not os.path.exists(path):
        raise Refusal("no Dockerfile at %s" % path)
    runtime = entry["base"]["runtime"]
    values = {
        "SAMBA_VERSION": str(entry["samba_version"]),
        "SAMBA_TARBALL_SHA256": str(entry["tarball_sha256"]),
        "BUILDER_BASE": entry["base"]["builder"],
        "RUNTIME_BASE": runtime,
        "GOBUILD_BASE": entry["base"]["gobuild"],
        "BASE_NAME": runtime.split("@", 1)[0],
        "BASE_DIGEST": runtime.split("@", 1)[1] if "@" in runtime else "",
    }
    if not values["BASE_DIGEST"]:
        raise Refusal("catalog base.runtime is not digest-pinned: %s" % runtime)

    with open(path, encoding="utf-8") as handle:
        lines = handle.read().splitlines(keepends=True)

    changed = []
    seen = set()
    for index, line in enumerate(lines):
        for name in MIRRORED_ARGS:
            prefix = "ARG %s=" % name
            if not line.startswith(prefix):
                continue
            seen.add(name)
            wanted = prefix + values[name] + "\n"
            if line != wanted:
                lines[index] = wanted
                if name not in changed:
                    changed.append(name)
    missing = [name for name in MIRRORED_ARGS if name not in seen]
    if missing:
        # The mirror is a contract, and a silently absent ARG would mean the
        # watcher published a build whose Dockerfile default no longer says
        # what the catalog says.
        raise Refusal("Dockerfile has no ARG default for: %s"
                      % ", ".join(missing))
    if changed and not dry_run:
        with open(path, "w", encoding="utf-8") as handle:
            handle.write("".join(lines))
    return changed


def catalog_call(root, argv):
    """Drive catalog.py through its own entry point.

    Not `import catalog; catalog.set_keys(...)`: the refusals, the
    validation and the write discipline all live behind that entry point,
    and the watcher must go through exactly the door a human at the shell
    goes through.
    """
    code = catalog.main(["--root", root] + argv)
    if code != 0:
        raise Refusal("catalog.py %s failed with exit code %d"
                      % (" ".join(argv), code))


def verify_tarball(root, version, run=run_command):
    """Fail-closed GPG verification, returning the checksum to pin.

    §9bis.8.a: a detected release is CONFIRMED against the authoritative
    source before anything is written. A checksum is a trusted-forever pin,
    so it is never minted from an unverified download.
    """
    script = os.path.join(root, "scripts", "verify-upstream-tarball.sh")
    try:
        output = run(["sh", script, version])
    except CommandFailed as error:
        raise Refusal("upstream verification failed for %s: %s"
                      % (version, error.output))
    for line in (output or "").splitlines():
        if line.startswith("sha256="):
            checksum = line.split("=", 1)[1].strip()
            if re.match(r"^[0-9a-f]{64}$", checksum):
                return checksum
    raise Refusal("verification of %s printed no sha256= line" % version)


def apply(root, decisions, dry_run=False, run=run_command,
          existing_tags=None):
    """Carry the decisions out. Returns the workflow outputs.

    Every branch is edited first and the README matrix is rendered last,
    from the final catalog, so one run that touches two branches produces
    one correct matrix rather than two successive ones.

    Not transactional, and it does not need to be: a refusal half way
    through leaves edits in the runner's working tree, the workflow's
    commit step never runs (a failed step fails the job), and the next
    hourly run starts from a fresh checkout and reaches the same decisions.
    Idempotency is what replaces a rollback here (§9bis.3).

    `existing_tags` is the tag list the remote already carries. It splits
    the decided tags into `new_tags` (create and push) and `existing_tags`
    (leave alone, dispatch against). Without that split the `publish`
    self-heal works exactly once and then wedges the watcher: run N pushes
    v4.24.7-r1 and loses its dispatch, run N+1 decides `publish` again for
    the same unpublished tag, re-creating a tag name the remote already
    has — which `git push --atomic` rejects, taking `main` down with it,
    on that run and on every run after it, for every branch.
    """
    catalogue = catalog.load_catalog(root)
    known = set(catalog.sorted_branches(catalogue))
    default_branch = catalogue["default_branch"]

    for decision in decisions:
        branch = decision.get("branch")
        if branch and branch not in known:
            # catalog.py's `state set` happily writes a branch the catalog
            # has never heard of, which would be a state entry nothing ever
            # reads again. This is where a typo stops.
            raise Refusal("decision names branch %s, which is not in the "
                          "catalog (%s)" % (branch, ", ".join(sorted(known))))

    tags = []
    dispatch = []
    causes = []
    rc = next((d for d in decisions if d.get("action") == "rc_series"), None)

    for decision in decisions:
        branch = decision.get("branch")
        action = decision.get("action")
        if not branch or action in ("new_series", "rc_series"):
            continue

        entry = catalog.entry_of(catalog.load_catalog(root), branch)
        if action == "version":
            version = decision["version"]
            tag = "%s-r1" % version
            note("%s: upstream %s -> %s" % (branch, entry["samba_version"],
                                            version))
            if not dry_run:
                checksum = verify_tarball(root, version, run=run)
                catalog_call(root, ["bump-version", branch, version, checksum])
        elif action == "revision":
            tag = "%s-r%d" % (entry["samba_version"],
                              int(entry["revision"]) + 1)
            note("%s: rebuild owed (%s) -> %s" % (branch, decision["cause"],
                                                  tag))
            if not dry_run:
                catalog_call(root, ["bump-revision", branch])
        elif action == "publish":
            tag = decision["tag"]
            note("%s: %s is not published yet — dispatching it unchanged"
                 % (branch, tag))
        else:
            tag = ""

        if not dry_run:
            # The package-closure hash is a catalog value (CI injects it as
            # --build-arg PKG_INDEX_HASH), so a rebuild that exists BECAUSE
            # the closure moved has to carry the new hash or it busts
            # nothing. Its Dockerfile ARG default floats on purpose and is
            # deliberately not mirrored.
            new_hash = (decision.get("state") or {}).get("pkg_index_hash")
            if new_hash and new_hash != str(entry["pkg_index_hash"]):
                catalog_call(root, ["set", branch, "pkg_index_hash", new_hash])
            for key, name in BASE_DIGEST_KEYS:
                digest = (decision.get("state") or {}).get(name)
                if not digest:
                    continue
                ref = entry["base"][key]
                moved = "%s@%s" % (ref.split("@", 1)[0], digest)
                if moved != ref:
                    catalog_call(root, ["set", branch, "base.%s" % key, moved])
            if branch == default_branch and action in ("version", "revision"):
                changed = sync_dockerfile_args(
                    root, catalog.entry_of(catalog.load_catalog(root), branch))
                if changed:
                    note("%s: Dockerfile ARG defaults follow the catalog: %s"
                         % (branch, ", ".join(changed)))

            if decision.get("clear_pending"):
                catalog_call(root, ["state", "clear-pending", branch])
            if decision.get("pending"):
                for key, value in sorted(decision["pending"].items()):
                    catalog_call(root, ["state", "set", branch,
                                        "pending.%s" % key, value])
            for key, value in sorted((decision.get("state") or {}).items()):
                catalog_call(root, ["state", "set", branch, key, value])

            if action in ("version", "revision"):
                catalog_call(root, ["changelog-entry", branch,
                                    decision["cause"]])
            if tag:
                # A record of the last tag DISPATCHED, not proof that it
                # was published: the release runs afterwards and can fail.
                # Nothing decides on this value — the authoritative answer
                # to "is this tag published" is the registry probe, which
                # is what makes a lost dispatch self-heal on the next run.
                catalog_call(root, ["state", "set", branch, "published_tag",
                                    tag])

        if tag:
            tags.append("v" + tag)
            dispatch.append("%s:v%s:%s" % (branch, tag, decision["cause"]))
            if decision["cause"] not in causes:
                causes.append(decision["cause"])

    if not dry_run:
        # Always re-render, so the §9.4 deprecation suffix appears the run a
        # candidate is detected and disappears the run it stops applying.
        matrix = ["update-readme-matrix"]
        if rc:
            matrix += ["--rc-series", rc["series"]]
        catalog_call(root, matrix)

    # A tag already on the remote is never re-created and never re-pushed;
    # it is still dispatched, because "the tag exists" and "the image is
    # published" are different facts and only the second one was checked.
    remote = set(existing_tags or [])
    already = [tag for tag in tags if tag in remote]
    for tag in already:
        note("%s already exists on the remote: not re-created, dispatched "
             "as it stands" % tag)
    return {"tags": " ".join(tags),
            "new_tags": " ".join(tag for tag in tags if tag not in remote),
            "existing_tags": " ".join(already),
            "dispatch": " ".join(dispatch),
            "causes": " ".join(causes)}


# --------------------------------------------------------------------------
# CLI


def note(message):
    print("watch: %s" % message, file=sys.stderr)


def summarise(decisions):
    """The plan as GitHub job-summary markdown."""
    lines = ["### Upstream check", "",
             "| Branch | Action | Cause | Tag | Notes |",
             "|--------|--------|-------|-----|-------|"]
    for decision in decisions:
        if not decision.get("branch"):
            continue
        lines.append("| %s | %s | %s | %s | %s |"
                     % (decision["branch"], decision["action"],
                        decision["cause"] or "—", decision.get("tag") or "—",
                        "; ".join(decision.get("notes") or []) or "—"))
    for decision in decisions:
        if decision.get("branch"):
            continue
        lines += ["", "**%s** — %s" % (decision["action"], decision["title"])]
    return "\n".join(lines) + "\n"


def load_json(path):
    with open(path, encoding="utf-8") as handle:
        return json.load(handle)


def write_json(path, document):
    with open(path, "w", encoding="utf-8") as handle:
        json.dump(document, handle, indent=2, sort_keys=False)
        handle.write("\n")


def build_parser():
    parser = argparse.ArgumentParser(
        description="Watch upstream, the base images and the package index.")
    parser.add_argument("--root", default=os.path.dirname(
        os.path.dirname(os.path.abspath(__file__))),
        help="repository root (default: this script's repository)")
    sub = parser.add_subparsers(dest="command", required=True)

    observe_cmd = sub.add_parser("observe", help="probe every source")
    observe_cmd.add_argument("--output", help="write the observation here")
    observe_cmd.add_argument("--image",
                             default=os.environ.get("IMAGE_NAME")
                             or DEFAULT_IMAGE)
    observe_cmd.add_argument("--org", default=DEFAULT_ORG)
    observe_cmd.add_argument("--dry-run", action="store_true",
                             help="do not update the source cache in "
                                  ".build-state.json")

    plan_cmd = sub.add_parser("plan", help="decide from an observation")
    plan_cmd.add_argument("--observe", required=True,
                          help="observation JSON from `observe`")
    plan_cmd.add_argument("--now", help="ISO-8601 instant (default: now, UTC)")
    plan_cmd.add_argument("--soak-hours", default="24")
    plan_cmd.add_argument("--security", action="store_true",
                          help="pre-announced security release: soak 0 h "
                               "(§9bis.5)")
    plan_cmd.add_argument("--output", help="write the decisions here")

    apply_cmd = sub.add_parser("apply", help="carry the decisions out")
    apply_cmd.add_argument("--decisions", required=True)
    apply_cmd.add_argument("--dry-run", action="store_true",
                           help="print what would be done, change nothing")
    apply_cmd.add_argument("--existing-tags-file",
                           help="tags the remote already carries, one per "
                                "line (git ls-remote --tags); they are "
                                "reported separately and never re-created")

    published_cmd = sub.add_parser(
        "published", help="branches whose catalog tag is on the registry")
    published_cmd.add_argument("--observe", required=True)

    return parser


def main(argv=None):
    args = build_parser().parse_args(argv)
    root = args.root
    try:
        if args.command == "observe":
            document = observe(root, image=args.image, org=args.org,
                               write_state=not args.dry_run)
            text = json.dumps(document, indent=2) + "\n"
            if args.output:
                write_json(args.output, document)
                note("observation written to %s" % args.output)
            else:
                sys.stdout.write(text)
            return 0

        if args.command == "plan":
            now = parse_time(args.now) if args.now \
                else datetime.datetime.now(UTC)
            decisions = plan(root, load_json(args.observe), now,
                             float(args.soak_hours), args.security)
            for decision in decisions:
                for line in decision.get("notes") or []:
                    note("%s: %s" % (decision.get("branch") or "catalog", line))
            if args.output:
                write_json(args.output, decisions)
                note("plan written to %s" % args.output)
            sys.stdout.write(summarise(decisions))
            return 0

        if args.command == "apply":
            existing = []
            if args.existing_tags_file:
                with open(args.existing_tags_file, encoding="utf-8") as handle:
                    existing = [line.strip() for line in handle
                                if line.strip()]
            outputs = apply(root, load_json(args.decisions),
                            dry_run=args.dry_run, existing_tags=existing)
            for key in ("tags", "new_tags", "existing_tags", "dispatch",
                        "causes"):
                print("%s=%s" % (key, outputs[key]))
            return 0

        if args.command == "published":
            observation = load_json(args.observe)
            catalogue = catalog.load_catalog(root)
            for branch in catalog.sorted_branches(catalogue):
                tag = catalog.tag_of(catalog.entry_of(catalogue, branch))
                if tag in (observation.get("published_tags") or []):
                    print("%s\t%s" % (branch, tag))
            return 0
    except Refusal as refusal:
        print("watch: %s" % refusal, file=sys.stderr)
        return refusal.code

    raise AssertionError("unhandled subcommand")  # pragma: no cover


if __name__ == "__main__":
    sys.exit(main())
