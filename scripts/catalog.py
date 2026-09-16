#!/usr/bin/env python3
"""Read and edit the release catalog (versions.yaml) and build state.

Every release workflow needs the same handful of answers — what is the
current tag for a branch, which aliases move with it, what did we last
publish — and every one of them would otherwise reimplement YAML parsing
in shell. This tool is the single implementation, so the workflows stay
declarative and the parsing rules are tested once, here.

Two files are owned by this tool:

  versions.yaml      the pin contract (SPEC §9bis). Human-edited and
                     watcher-edited, and its comments are documentation,
                     so writes are line-targeted substitutions followed by
                     a re-parse assertion rather than a load/dump cycle
                     (PyYAML's dumper would drop every comment).
  .build-state.json  the watcher's memory across runs: the digests and
                     package-index hash a branch was last built from, the
                     tag last dispatched, and any version still soaking.
                     Machine-owned, so it is rewritten whole.

Usage: python3 scripts/catalog.py <subcommand> ...  (--help lists them)
Exit codes: 0 success, 2 refusal (bad input, missing entry, mismatch),
3 duplicate CHANGELOG entry.
"""

import argparse
import datetime
import json
import os
import re
import sys

try:
    import yaml
except ImportError:  # pragma: no cover - environment failure, not logic
    sys.exit("PyYAML is required: apt install python3-yaml / pip install pyyaml")

# Registry names are part of the published contract (README, release
# notes, cosign commands), so they live here rather than in each workflow.
GHCR_IMAGE = "ghcr.io/esitc-paris/samba-ad-dc"
HUB_IMAGE = "docker.io/esitcparis/samba-ad-dc"
# The owner is spelled `ESITC-Paris` here, not lowercase like the registry
# names: this URL is also the Fulcio certificate identity that cosign matches
# with --certificate-identity-regexp, and that match is case-sensitive. GitHub
# web URLs are case-insensitive, so the CHANGELOG and Release links still work.
REPO_URL = "https://github.com/ESITC-Paris/samba-ad-dc"
OIDC_ISSUER = "https://token.actions.githubusercontent.com"

# SPEC §10.4 causes plus the two this repository adds: `base-digest` for a
# rebuild forced by a moved base image, `first-publication` for the very
# first tag of a branch, which has no preceding revision to compare to.
CAUSES = ("samba-release", "pkg-update", "base-digest", "manual",
          "first-publication")

# Catalog keys, split by depth because the writer needs to know which
# indentation level to target.
SCALAR_KEYS = ("samba_version", "revision", "tarball_sha256",
               "pkg_index_hash")
BASE_KEYS = ("base.builder", "base.runtime", "base.gobuild")
CATALOG_KEYS = SCALAR_KEYS + BASE_KEYS

STATE_KEYS = ("runtime_digest", "builder_digest", "gobuild_digest",
              "pkg_index_hash", "published_tag", "pending.version",
              "pending.first_seen")

# Lifecycle labels by position, newest branch first (§9.4: upstream's
# status relayed, never decided here). Anything past the third branch is
# out of upstream support.
LIFECYCLE = ("current", "maintenance", "security fixes only")
EOL = "discontinued (EOL)"

MATRIX_START = "<!-- matrix:start -->"
MATRIX_END = "<!-- matrix:end -->"

TAG_RE = re.compile(r"^v(\d+)\.(\d+)\.(\d+)-r(\d+)$")
VERSION_RE = re.compile(r"^\d+\.\d+\.\d+$")
SERIES_RE = re.compile(r"^\d+\.\d+$")


class Refusal(Exception):
    """A refusal with an exit code: the caller prints it and exits."""

    def __init__(self, message, code=2):
        super().__init__(message)
        self.code = code


# --------------------------------------------------------------------------
# File access


def catalog_path(root):
    return os.path.join(root, "versions.yaml")


def state_path(root):
    return os.path.join(root, ".build-state.json")


def read_text(path):
    try:
        with open(path, encoding="utf-8") as handle:
            return handle.read()
    except FileNotFoundError:
        raise Refusal("not found: %s" % path)


def write_text(path, text):
    with open(path, "w", encoding="utf-8") as handle:
        handle.write(text)


def load_catalog(root):
    data = yaml.safe_load(read_text(catalog_path(root)))
    if not isinstance(data, dict) or "branches" not in data:
        raise Refusal("%s has no `branches` mapping" % catalog_path(root))
    return data


def entry_of(catalog, branch):
    branches = catalog["branches"]
    if branch not in branches:
        raise Refusal("no such branch in the catalog: %s (have: %s)"
                      % (branch, " ".join(sorted_branches(catalog))))
    return branches[branch]


def sorted_branches(catalog, descending=False):
    """Branches ordered numerically, so 4.9 sorts before 4.10."""
    return sorted(catalog["branches"], key=series_key, reverse=descending)


def series_key(series):
    return tuple(int(part) for part in series.split("."))


def tag_of(entry):
    return "%s-r%s" % (entry["samba_version"], entry["revision"])


def aliases_of(catalog, branch):
    """Image tags that must point at this branch's current build.

    `X` and `latest` belong to the default branch alone: they are the only
    aliases that are not scoped to a series, so two branches claiming them
    would make the pointer ambiguous.
    """
    entry = entry_of(catalog, branch)
    version = entry["samba_version"]
    tags = [tag_of(entry), version, branch]
    if branch == catalog.get("default_branch"):
        tags += [version.split(".")[0], "latest"]
    return tags


# --------------------------------------------------------------------------
# Catalog writing — targeted substitution, because comments are content


def _branch_block(lines, branch):
    """Return (start, end) line indices of a branch's block.

    The block starts at the `"<branch>":` line and ends before the next
    line indented two spaces or less, which is either the next branch or a
    top-level key.
    """
    head = re.compile(r'^  ["\']?%s["\']?:\s*(?:#.*)?$' % re.escape(branch))
    start = None
    for index, line in enumerate(lines):
        if head.match(line):
            start = index
            break
    if start is None:
        raise Refusal("no block for branch %s in versions.yaml" % branch)
    for index in range(start + 1, len(lines)):
        stripped = lines[index].strip()
        if stripped and len(lines[index]) - len(lines[index].lstrip()) <= 2:
            return start, index
    return start, len(lines)


def _key_line(lines, start, end, key, indent):
    """Index of `key:` at the given indentation inside [start, end)."""
    wanted = re.compile(r"^ {%d}%s:" % (indent, re.escape(key)))
    for index in range(start, end):
        if wanted.match(lines[index]):
            return index
    raise Refusal("key %s not found where expected in versions.yaml" % key)


def _substitute(line, key, indent, value):
    """Replace a scalar value in place, keeping quoting and any comment."""
    pattern = re.compile(
        r"^(?P<head> {%d}%s:[ \t]*)"
        r"(?P<value>\"[^\"]*\"|'[^']*'|[^#\n]*?)"
        r"(?P<trail>[ \t]*(?:#[^\n]*)?)(?P<eol>\r?\n?)$" % (indent,
                                                            re.escape(key)))
    match = pattern.match(line)
    if match is None:
        raise Refusal("cannot parse the %s line in versions.yaml" % key)
    old = match.group("value")
    quote = old[0] if old[:1] in ('"', "'") else ""
    if quote and quote in value:
        raise Refusal("value for %s contains its own quote character: %s"
                      % (key, value))
    return "%s%s%s%s%s%s" % (match.group("head"), quote, value, quote,
                             match.group("trail"), match.group("eol"))


def set_keys(root, branch, pairs):
    """Apply key/value edits to one branch, then re-parse to prove them.

    The re-parse is the safety net for the regex writer: if a substitution
    landed on the wrong line or produced something YAML reads differently,
    the assertion fails before anything downstream trusts the file.

    The edited bytes only ever stay on disk once they have re-parsed and
    read back what was written. versions.yaml is the build's pin contract
    and the watcher edits it unattended, so a failed edit must leave the
    previous file intact — a corrupted catalog would be inherited by every
    later step of the run and by the next run.
    """
    path = catalog_path(root)
    original = read_text(path)
    lines = original.splitlines(keepends=True)
    start, end = _branch_block(lines, branch)
    for key, value in pairs:
        if key in BASE_KEYS:
            base = _key_line(lines, start, end, "base", 4)
            index = _key_line(lines, base + 1, end, key.split(".", 1)[1], 6)
            indent = 6
        elif key in SCALAR_KEYS:
            index = _key_line(lines, start, end, key, 4)
            indent = 4
        else:
            raise Refusal("unknown catalog key: %s (known: %s)"
                          % (key, " ".join(CATALOG_KEYS)))
        lines[index] = _substitute(lines[index], key.split(".")[-1], indent,
                                   value)
    write_text(path, "".join(lines))

    try:
        entry = entry_of(load_catalog(root), branch)
        for key, value in pairs:
            got = entry["base"][key.split(".", 1)[1]] if key in BASE_KEYS \
                else entry[key]
            if str(got) != value:
                raise Refusal("%s reads back as %r, not %r"
                              % (key, str(got), value))
    except (yaml.YAMLError, KeyError, Refusal) as failure:
        # Roll back before reporting: an unparseable or wrong file is a
        # refusal (exit 2), never a traceback and never a file left for
        # the next command to trust. PyYAML's errors are multi-line, so
        # they are collapsed to keep the refusal one line.
        write_text(path, original)
        raise Refusal(
            "edit of %s on branch %s did not verify: %s — versions.yaml "
            "left unchanged" % (", ".join(key for key, _ in pairs), branch,
                                " ".join(str(failure).split())))


# --------------------------------------------------------------------------
# Build state


def load_state(root):
    path = state_path(root)
    if not os.path.exists(path):
        return {"branches": {}}
    data = json.loads(read_text(path))
    data.setdefault("branches", {})
    return data


def save_state(root, data):
    write_text(state_path(root), json.dumps(data, indent=2,
                                            sort_keys=False) + "\n")


def state_get(root, branch, key):
    if key not in STATE_KEYS:
        raise Refusal("unknown state key: %s (known: %s)"
                      % (key, " ".join(STATE_KEYS)))
    # A branch with no state yet is the normal first-run case, not an
    # error: callers test the printed value for emptiness.
    entry = load_state(root)["branches"].get(branch, {})
    if "." in key:
        outer, inner = key.split(".", 1)
        return entry.get(outer, {}).get(inner, "")
    return entry.get(key, "")


def state_set(root, branch, key, value):
    if key not in STATE_KEYS:
        raise Refusal("unknown state key: %s (known: %s)"
                      % (key, " ".join(STATE_KEYS)))
    data = load_state(root)
    entry = data["branches"].setdefault(branch, {})
    if "." in key:
        outer, inner = key.split(".", 1)
        entry.setdefault(outer, {})[inner] = value
    else:
        entry[key] = value
    save_state(root, data)


def state_clear_pending(root, branch):
    data = load_state(root)
    data["branches"].get(branch, {}).pop("pending", None)
    save_state(root, data)


# --------------------------------------------------------------------------
# Compatibility matrix


def matrix_rows(catalog, rc_series=None):
    branches = sorted_branches(catalog, descending=True)
    statuses = [LIFECYCLE[i] if i < len(LIFECYCLE) else EOL
                for i in range(len(branches))]

    # §9.4: a release candidate for a newer series is upstream's own
    # end-of-life signal for the oldest branch it still supports, and the
    # matrix must relay it as soon as it is detected.
    if rc_series and branches and series_key(rc_series) > series_key(branches[0]):
        supported = [i for i, status in enumerate(statuses) if status != EOL]
        if supported:
            oldest = supported[-1]
            statuses[oldest] += " — deprecation pending (%s rc published)" \
                % rc_series

    rows = []
    for branch, status in zip(branches, statuses):
        tags = aliases_of(catalog, branch)
        aliases = ", ".join("`%s`" % tag for tag in tags[1:])
        rows.append("| %s | %s | %s | %s |" % (branch, tags[0], aliases,
                                               status))
    return rows


def render_matrix(catalog, rc_series=None):
    header = ("| Upstream branch | Latest image tag | Aliases "
              "| Upstream support status |")
    rule = ("|-----------------|------------------|---------"
            "|-------------------------|")
    return "\n".join([header, rule] + matrix_rows(catalog, rc_series))


def update_readme_matrix(root, catalog, rc_series=None):
    path = os.path.join(root, "README.md")
    lines = read_text(path).splitlines(keepends=True)
    bounds = []
    for marker in (MATRIX_START, MATRIX_END):
        found = [i for i, line in enumerate(lines) if line.strip() == marker]
        if len(found) != 1:
            raise Refusal("README.md must contain exactly one %s line (found "
                          "%d)" % (marker, len(found)))
        bounds.append(found[0])
    start, end = bounds
    if start > end:
        raise Refusal("README.md has %s after %s" % (MATRIX_START, MATRIX_END))
    table = [line + "\n" for line in render_matrix(catalog, rc_series).split("\n")]
    write_text(path, "".join(lines[:start + 1] + table + lines[end:]))


# --------------------------------------------------------------------------
# CHANGELOG and release notes


def today_utc():
    return datetime.datetime.now(datetime.timezone.utc).date().isoformat()


def changelog_entry(root, catalog, branch, cause, notes=None):
    entry = entry_of(catalog, branch)
    tag = tag_of(entry)
    path = os.path.join(root, "CHANGELOG.md")
    text = read_text(path)
    if re.search(r"^## %s " % re.escape(tag), text, re.MULTILINE):
        raise Refusal("CHANGELOG.md already has an entry for %s" % tag, code=3)

    block = "\n".join([
        "## %s — %s" % (tag, today_utc()),
        "",
        "- Samba: %s (branch %s)" % (entry["samba_version"], branch),
        "- Trigger: %s" % cause,
        "- Image changes: %s" % (notes or "none"),
        "- Fixed CVEs: see the GitHub Release",
        "- Digests, signature and attestations: %s/releases/tag/v%s"
        % (REPO_URL, tag),
    ]) + "\n"

    lines = text.splitlines(keepends=True)
    heading = next((i for i, line in enumerate(lines)
                    if line.startswith("## ")), None)
    if heading is None:
        # No entries yet: the file is the header and its prose, so the
        # first entry goes at the end, one blank line below it.
        if not text.endswith("\n"):
            text += "\n"
        write_text(path, text + "\n" + block)
    else:
        write_text(path, "".join(lines[:heading]) + block + "\n"
                   + "".join(lines[heading:]))


def release_notes(catalog, branch, cause, digest_ghcr, digest_hub=None,
                  degraded="none", pending_arch=None):
    entry = entry_of(catalog, branch)
    tag = tag_of(entry)
    parts = [
        "**Samba %s** — image revision r%s, built from verified upstream "
        "source and signed." % (entry["samba_version"], entry["revision"]),
        "",
        "**Images**",
        "",
        "- `%s:%s` (source of truth)" % (GHCR_IMAGE, tag),
    ]
    # The mirror is listed only when its digest is known, so the notes
    # never claim a push that did not happen.
    if digest_hub:
        parts.append("- `%s:%s` (mirror)" % (HUB_IMAGE, tag))
    parts += ["", "**Digests:** GHCR `%s`" % digest_ghcr
              + (" · Docker Hub `%s`" % digest_hub if digest_hub else ""),
              "", "**Trigger:** %s" % cause]

    if degraded != "none":
        parts += ["", _degraded_paragraph(degraded, pending_arch, tag)]

    parts += [
        "",
        "**Verify:**",
        "",
        "```",
        "cosign verify %s@%s \\" % (GHCR_IMAGE, digest_ghcr),
        "  --certificate-identity-regexp '%s/.*' \\" % REPO_URL,
        "  --certificate-oidc-issuer %s" % OIDC_ISSUER,
        "```",
        "",
        "SBOM and provenance attestations are attached to the image. Pin "
        "production deployments by digest.",
    ]
    return "\n".join(parts) + "\n"


def _degraded_paragraph(mode, arch, tag):
    """Disclosure required by SPEC §9.6 whenever a degraded mode was used."""
    if mode == "emulated":
        return ("**Degraded publication mode — SPEC §9.6 Mode 1 (emulated "
                "build).** The `%s` image was built under QEMU emulation on "
                "the same public hosted CI as every other build. No gate was "
                "waived: the full E2E suite ran under emulation. Emulation "
                "degrades build speed, not provenance — the trust boundary "
                "is unchanged." % arch)
    return ("**Degraded publication mode — SPEC §9.6 Mode 2 "
            "(architecture-staggered publication).** This manifest does not "
            "include `%s`: a platform incident made that architecture "
            "unavailable within the security-driven publication deadline. "
            "Users on `%s` remain on the previous revision. `%s` is immutable "
            "and will never be amended in place; the complete multi-arch "
            "manifest is published as the next revision once the platform "
            "recovers." % (arch, arch, tag))


# --------------------------------------------------------------------------
# CLI


def build_parser():
    parser = argparse.ArgumentParser(
        description="Read and edit versions.yaml and .build-state.json.")
    parser.add_argument(
        "--root", default=os.path.dirname(os.path.dirname(
            os.path.abspath(__file__))),
        help="repository root holding versions.yaml (default: this "
             "script's repository)")
    sub = parser.add_subparsers(dest="command", required=True)

    get = sub.add_parser("get", help="print one catalog value")
    get.add_argument("branch")
    get.add_argument("key")

    sub.add_parser("branches", help="branches, one per line, ascending")
    sub.add_parser("default", help="the default branch")

    for name, helptext in (("tag", "X.Y.Z-rN"), ("git-tag", "vX.Y.Z-rN"),
                           ("aliases", "image tags, one per line")):
        node = sub.add_parser(name, help=helptext)
        node.add_argument("branch")

    branch_of_tag = sub.add_parser(
        "branch-of-tag", help="X.Y for a vX.Y.Z-rN tag, if the catalog agrees")
    branch_of_tag.add_argument("tag")

    setter = sub.add_parser("set", help="edit one catalog value in place")
    setter.add_argument("branch")
    setter.add_argument("key", choices=CATALOG_KEYS)
    setter.add_argument("value")

    bump_version = sub.add_parser(
        "bump-version", help="new upstream version; resets the revision to 1")
    bump_version.add_argument("branch")
    bump_version.add_argument("version")
    bump_version.add_argument("sha256")

    bump_revision = sub.add_parser("bump-revision", help="revision + 1")
    bump_revision.add_argument("branch")

    state = sub.add_parser("state", help="read or write .build-state.json")
    state_sub = state.add_subparsers(dest="state_command", required=True)
    state_get_cmd = state_sub.add_parser("get")
    state_get_cmd.add_argument("branch")
    state_get_cmd.add_argument("key")
    state_set_cmd = state_sub.add_parser("set")
    state_set_cmd.add_argument("branch")
    state_set_cmd.add_argument("key")
    state_set_cmd.add_argument("value")
    state_clear = state_sub.add_parser("clear-pending")
    state_clear.add_argument("branch")

    for name, helptext in (("render-matrix", "print the compatibility matrix"),
                           ("update-readme-matrix",
                            "rewrite the matrix block in README.md")):
        node = sub.add_parser(name, help=helptext)
        node.add_argument("--rc-series", metavar="X.Y",
                          help="upstream series with a published release "
                               "candidate (§9.4 deprecation signal)")

    changelog = sub.add_parser("changelog-entry",
                               help="prepend an entry to CHANGELOG.md")
    changelog.add_argument("branch")
    changelog.add_argument("cause", choices=CAUSES)
    changelog.add_argument("--notes", help="image changes (default: none)")

    notes = sub.add_parser("release-notes",
                           help="GitHub Release body, on stdout")
    notes.add_argument("branch")
    notes.add_argument("--cause", choices=CAUSES, required=True)
    notes.add_argument("--digest-ghcr", required=True)
    notes.add_argument("--digest-hub")
    notes.add_argument("--degraded", choices=("none", "emulated", "staggered"),
                       default="none")
    notes.add_argument("--pending-arch",
                       help="architecture affected by the degraded mode")
    return parser


def dispatch(args):
    """Run one subcommand.

    Returns the text to print, or None for subcommands that only write
    files — distinct from the empty string, which is what an unset state
    key legitimately reads back as.
    """
    root = args.root
    command = args.command

    if command == "branches":
        return "\n".join(sorted_branches(load_catalog(root)))
    if command == "default":
        return load_catalog(root)["default_branch"]
    if command == "get":
        entry = entry_of(load_catalog(root), args.branch)
        if args.key in BASE_KEYS:
            return str(entry["base"][args.key.split(".", 1)[1]])
        if args.key in SCALAR_KEYS:
            return str(entry[args.key])
        raise Refusal("unknown catalog key: %s (known: %s)"
                      % (args.key, " ".join(CATALOG_KEYS)))
    if command == "tag":
        return tag_of(entry_of(load_catalog(root), args.branch))
    if command == "git-tag":
        return "v" + tag_of(entry_of(load_catalog(root), args.branch))
    if command == "aliases":
        return "\n".join(aliases_of(load_catalog(root), args.branch))

    if command == "branch-of-tag":
        match = TAG_RE.match(args.tag)
        if match is None:
            raise Refusal("not a release tag (expected vX.Y.Z-rN): %s"
                          % args.tag)
        major, minor, patch, revision = match.groups()
        branch = "%s.%s" % (major, minor)
        entry = entry_of(load_catalog(root), branch)
        version = "%s.%s.%s" % (major, minor, patch)
        if entry["samba_version"] != version:
            raise Refusal("tag %s does not match branch %s, whose version is "
                          "%s" % (args.tag, branch, entry["samba_version"]))
        if str(entry["revision"]) != revision:
            raise Refusal("tag %s does not match branch %s, whose revision is "
                          "%s" % (args.tag, branch, entry["revision"]))
        return branch

    if command == "set":
        set_keys(root, args.branch, [(args.key, args.value)])
        return None
    if command == "bump-version":
        if not VERSION_RE.match(args.version):
            raise Refusal("not an upstream version (expected X.Y.Z): %s"
                          % args.version)
        if args.version.rsplit(".", 1)[0] != args.branch:
            raise Refusal("version %s does not belong to branch %s"
                          % (args.version, args.branch))
        # A new upstream version is revision 1 by definition: the revision
        # counts our rebuilds of one upstream version (SPEC §3.2).
        set_keys(root, args.branch,
                 [("samba_version", args.version),
                  ("tarball_sha256", args.sha256),
                  ("revision", "1")])
        return None
    if command == "bump-revision":
        entry = entry_of(load_catalog(root), args.branch)
        set_keys(root, args.branch,
                 [("revision", str(int(entry["revision"]) + 1))])
        return None

    if command == "state":
        if args.state_command == "get":
            return str(state_get(root, args.branch, args.key))
        if args.state_command == "set":
            state_set(root, args.branch, args.key, args.value)
        else:
            state_clear_pending(root, args.branch)
        return None

    if command in ("render-matrix", "update-readme-matrix"):
        if args.rc_series and not SERIES_RE.match(args.rc_series):
            raise Refusal("not an upstream series (expected X.Y): %s"
                          % args.rc_series)
        catalog = load_catalog(root)
        if command == "render-matrix":
            return render_matrix(catalog, args.rc_series)
        update_readme_matrix(root, catalog, args.rc_series)
        return None

    if command == "changelog-entry":
        changelog_entry(root, load_catalog(root), args.branch, args.cause,
                        args.notes)
        return None

    if command == "release-notes":
        if args.degraded != "none" and not args.pending_arch:
            raise Refusal("--degraded %s requires --pending-arch"
                          % args.degraded)
        return release_notes(load_catalog(root), args.branch, args.cause,
                             args.digest_ghcr, args.digest_hub,
                             args.degraded, args.pending_arch).rstrip("\n")

    raise Refusal("unhandled subcommand: %s" % command)  # pragma: no cover


def main(argv=None):
    args = build_parser().parse_args(argv)
    try:
        output = dispatch(args)
    except Refusal as refusal:
        print("catalog: %s" % refusal, file=sys.stderr)
        return refusal.code
    if output is not None:
        print(output)
    return 0


if __name__ == "__main__":
    sys.exit(main())
