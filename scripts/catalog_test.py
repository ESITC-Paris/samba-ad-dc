"""Tests for scripts/catalog.py and scripts/check-cve-exceptions.py.

Every case runs against real files in a fresh temporary directory: the
tools' whole job is editing on-disk catalog/state/markdown, so mocking the
filesystem would only prove the mocks agree with themselves.

Run:  python3 -m unittest scripts/catalog_test.py -v
"""

import contextlib
import datetime
import importlib.util
import io
import json
import os
import shutil
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import catalog  # noqa: E402  (needs the sys.path line above)


def _load_cve_checker():
    """Import check-cve-exceptions.py, whose filename is not an identifier."""
    path = os.path.join(os.path.dirname(os.path.abspath(__file__)),
                        "check-cve-exceptions.py")
    spec = importlib.util.spec_from_file_location("check_cve_exceptions", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


check_cve_exceptions = _load_cve_checker()

# A branch block carrying the same comment shapes as the real
# versions.yaml (whole-line, trailing, and one inside base:) so the
# comment-preservation assertions exercise all three.
BRANCH_TEMPLATE = """\
  "{branch}":
    samba_version: "{version}"
    revision: {revision}  # the N in X.Y.Z-rN
    tarball_sha256: "{sha}"
    base:
      builder: "debian:trixie-slim@sha256:{d}builder"
      runtime: "debian:trixie-slim@sha256:{d}runtime"
      # Toolchain for the Go entrypoint stage.
      gobuild: "golang:1.24-trixie@sha256:{d}gobuild"
    pkg_index_hash: "idx-{branch}"  # replaced by the watcher
"""

CATALOG_HEADER = """\
# Fixture catalog — same shape as the repository's versions.yaml.
#
# The comments in this file are load-bearing for the tests: `set` must
# leave them untouched.
---
default_branch: "{default}"
branches:
"""


def write_catalog(root, branches, default):
    """Write a versions.yaml fixture.

    branches: list of (branch, version, revision) in file order.
    """
    body = "".join(
        BRANCH_TEMPLATE.format(branch=b, version=v, revision=r,
                               sha="sha-" + b, d=b.replace(".", ""))
        for b, v, r in branches
    )
    path = os.path.join(root, "versions.yaml")
    with open(path, "w", encoding="utf-8") as handle:
        handle.write(CATALOG_HEADER.format(default=default) + body)
    return path


class CatalogTestCase(unittest.TestCase):
    """Base: a temp repo root plus a helper that runs the CLI in-process."""

    def setUp(self):
        self.root = tempfile.mkdtemp(prefix="catalog-test-")
        self.addCleanup(shutil.rmtree, self.root)
        write_catalog(self.root,
                      [("4.22", "4.22.9", 3),
                       ("4.23", "4.23.4", 2),
                       ("4.24", "4.24.6", 1)],
                      default="4.24")

    def run_cli(self, *args):
        """Return (exit_code, stdout, stderr) for one CLI invocation."""
        out, err = io.StringIO(), io.StringIO()
        argv = ["--root", self.root] + list(args)
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            try:
                code = catalog.main(argv)
            except SystemExit as exc:  # argparse errors
                code = exc.code if isinstance(exc.code, int) else 1
        return code, out.getvalue(), err.getvalue()

    def read(self, name):
        with open(os.path.join(self.root, name), encoding="utf-8") as handle:
            return handle.read()

    def write(self, name, text):
        path = os.path.join(self.root, name)
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "w", encoding="utf-8") as handle:
            handle.write(text)
        return path


class TestReads(CatalogTestCase):
    def test_get_scalar_and_nested_keys(self):
        self.assertEqual(self.run_cli("get", "4.24", "samba_version"),
                         (0, "4.24.6\n", ""))
        self.assertEqual(self.run_cli("get", "4.23", "revision"), (0, "2\n", ""))
        code, out, _ = self.run_cli("get", "4.24", "base.runtime")
        self.assertEqual(code, 0)
        self.assertEqual(out.strip(),
                         "debian:trixie-slim@sha256:424runtime")
        code, out, _ = self.run_cli("get", "4.22", "pkg_index_hash")
        self.assertEqual((code, out), (0, "idx-4.22\n"))

    def test_get_rejects_unknown_key_and_branch(self):
        code, _, err = self.run_cli("get", "4.24", "nope")
        self.assertEqual(code, 2)
        self.assertIn("nope", err)
        code, _, err = self.run_cli("get", "9.99", "samba_version")
        self.assertEqual(code, 2)
        self.assertIn("9.99", err)

    def test_branches_are_ascending(self):
        self.assertEqual(self.run_cli("branches"),
                         (0, "4.22\n4.23\n4.24\n", ""))

    def test_default_branch(self):
        self.assertEqual(self.run_cli("default"), (0, "4.24\n", ""))


class TestTagsAndAliases(CatalogTestCase):
    def test_tag_and_git_tag(self):
        self.assertEqual(self.run_cli("tag", "4.24"), (0, "4.24.6-r1\n", ""))
        self.assertEqual(self.run_cli("git-tag", "4.24"),
                         (0, "v4.24.6-r1\n", ""))
        self.assertEqual(self.run_cli("tag", "4.22"), (0, "4.22.9-r3\n", ""))

    def test_aliases_default_branch_carries_major_and_latest(self):
        code, out, _ = self.run_cli("aliases", "4.24")
        self.assertEqual(code, 0)
        self.assertEqual(out.split(),
                         ["4.24.6-r1", "4.24.6", "4.24", "4", "latest"])

    def test_aliases_non_default_branch_stops_at_minor(self):
        code, out, _ = self.run_cli("aliases", "4.23")
        self.assertEqual(code, 0)
        self.assertEqual(out.split(), ["4.23.4-r2", "4.23.4", "4.23"])


class TestBranchOfTag(CatalogTestCase):
    def test_match(self):
        self.assertEqual(self.run_cli("branch-of-tag", "v4.23.4-r2"),
                         (0, "4.23\n", ""))

    def test_version_mismatch_exits_2(self):
        code, out, err = self.run_cli("branch-of-tag", "v4.23.5-r2")
        self.assertEqual(code, 2)
        self.assertEqual(out, "")
        self.assertIn("4.23.4", err)

    def test_revision_mismatch_exits_2(self):
        code, _, err = self.run_cli("branch-of-tag", "v4.23.4-r9")
        self.assertEqual(code, 2)
        self.assertIn("revision", err)

    def test_unknown_branch_exits_2(self):
        code, _, err = self.run_cli("branch-of-tag", "v9.99.0-r1")
        self.assertEqual(code, 2)
        self.assertIn("9.99", err)

    def test_bad_format_exits_2(self):
        for bad in ["4.24.6-r1", "v4.24-r1", "v4.24.6", "v4.24.6-r0x", ""]:
            code, out, err = self.run_cli("branch-of-tag", bad)
            self.assertEqual(code, 2, "expected refusal for %r" % bad)
            self.assertEqual(out, "")
            self.assertTrue(err.strip(), "expected a message for %r" % bad)


class TestEdits(CatalogTestCase):
    def test_set_preserves_everything_outside_the_edited_line(self):
        before = self.read("versions.yaml").splitlines(keepends=True)
        self.assertEqual(self.run_cli("set", "4.23", "pkg_index_hash", "new"),
                         (0, "", ""))
        after = self.read("versions.yaml").splitlines(keepends=True)
        self.assertEqual(len(before), len(after))
        differing = [i for i, (a, b) in enumerate(zip(before, after)) if a != b]
        self.assertEqual(len(differing), 1)
        self.assertIn('pkg_index_hash: "new"', after[differing[0]])
        # The trailing comment on the edited line survives too.
        self.assertIn("# replaced by the watcher", after[differing[0]])
        self.assertEqual(self.run_cli("get", "4.23", "pkg_index_hash"),
                         (0, "new\n", ""))

    def test_set_nested_base_key(self):
        self.assertEqual(
            self.run_cli("set", "4.24", "base.runtime", "debian@sha256:new"),
            (0, "", ""))
        self.assertEqual(self.run_cli("get", "4.24", "base.runtime"),
                         (0, "debian@sha256:new\n", ""))
        self.assertIn("# Toolchain for the Go entrypoint stage.",
                      self.read("versions.yaml"))

    def test_set_revision_stays_an_unquoted_integer(self):
        self.assertEqual(self.run_cli("set", "4.24", "revision", "4"),
                         (0, "", ""))
        self.assertIn("revision: 4  # the N in X.Y.Z-rN",
                      self.read("versions.yaml"))
        self.assertEqual(self.run_cli("get", "4.24", "revision"), (0, "4\n", ""))

    def test_a_substitution_that_breaks_yaml_is_rolled_back(self):
        """A failed edit must leave the pin contract exactly as it was."""
        before = self.read("versions.yaml")
        original = catalog._substitute

        def breaks_the_file(line, key, indent, value):
            return '    samba_version: "a": "b"\n'

        catalog._substitute = breaks_the_file
        self.addCleanup(setattr, catalog, "_substitute", original)
        code, out, err = self.run_cli("set", "4.24", "samba_version", "4.24.7")
        self.assertEqual(code, 2)
        self.assertEqual(out, "")
        self.assertEqual(self.read("versions.yaml"), before)
        # One line, naming what failed — not a PyYAML traceback.
        self.assertEqual(len(err.strip().splitlines()), 1)
        self.assertIn("samba_version", err)
        self.assertIn("4.24", err)
        self.assertIn("left unchanged", err)

    def test_a_value_that_reads_back_differently_is_rolled_back(self):
        before = self.read("versions.yaml")
        code, out, err = self.run_cli("set", "4.24", "samba_version",
                                      "4.24.7\n  bogus")
        self.assertEqual(code, 2)
        self.assertEqual(out, "")
        self.assertEqual(self.read("versions.yaml"), before)
        self.assertIn("reads back as", err)
        self.assertIn("left unchanged", err)

    def test_a_failed_multi_key_edit_rolls_back_every_key(self):
        before = self.read("versions.yaml")
        original = catalog._substitute

        def breaks_the_file(line, key, indent, value):
            return '    samba_version: "a": "b"\n'

        catalog._substitute = breaks_the_file
        self.addCleanup(setattr, catalog, "_substitute", original)
        code, _, err = self.run_cli("bump-version", "4.24", "4.24.7", "abc")
        self.assertEqual(code, 2)
        self.assertEqual(self.read("versions.yaml"), before)
        for key in ("samba_version", "tarball_sha256", "revision"):
            self.assertIn(key, err)

    def test_bump_version_resets_revision_to_1(self):
        self.assertEqual(self.run_cli("set", "4.24", "revision", "5"),
                         (0, "", ""))
        self.assertEqual(
            self.run_cli("bump-version", "4.24", "4.24.7", "deadbeef"),
            (0, "", ""))
        self.assertEqual(self.run_cli("get", "4.24", "samba_version"),
                         (0, "4.24.7\n", ""))
        self.assertEqual(self.run_cli("get", "4.24", "tarball_sha256"),
                         (0, "deadbeef\n", ""))
        self.assertEqual(self.run_cli("get", "4.24", "revision"), (0, "1\n", ""))
        self.assertEqual(self.run_cli("tag", "4.24"), (0, "4.24.7-r1\n", ""))

    def test_bump_version_rejects_a_version_off_the_branch(self):
        code, _, err = self.run_cli("bump-version", "4.24", "4.23.9", "abc")
        self.assertEqual(code, 2)
        self.assertIn("4.24", err)

    def test_bump_revision(self):
        self.assertEqual(self.run_cli("bump-revision", "4.23"), (0, "", ""))
        self.assertEqual(self.run_cli("get", "4.23", "revision"), (0, "3\n", ""))
        self.assertEqual(self.run_cli("tag", "4.23"), (0, "4.23.4-r3\n", ""))


class TestState(CatalogTestCase):
    def state(self):
        return json.loads(self.read(".build-state.json"))

    def test_set_creates_the_file_and_nests_pending(self):
        self.assertFalse(os.path.exists(
            os.path.join(self.root, ".build-state.json")))
        self.assertEqual(
            self.run_cli("state", "set", "4.24", "runtime_digest", "sha256:aa"),
            (0, "", ""))
        self.assertEqual(
            self.run_cli("state", "set", "4.24", "pending.version", "4.24.7"),
            (0, "", ""))
        self.assertEqual(
            self.run_cli("state", "set", "4.24", "pending.first_seen",
                         "2026-09-16T17:00:00Z"),
            (0, "", ""))
        self.assertEqual(self.state(), {"branches": {"4.24": {
            "runtime_digest": "sha256:aa",
            "pending": {"version": "4.24.7",
                        "first_seen": "2026-09-16T17:00:00Z"}}}})
        self.assertTrue(self.read(".build-state.json").endswith("}\n"))

    def test_get_missing_is_empty_and_successful(self):
        self.assertEqual(self.run_cli("state", "get", "4.24", "published_tag"),
                         (0, "\n", ""))
        self.run_cli("state", "set", "4.24", "published_tag", "4.24.6-r1")
        self.assertEqual(self.run_cli("state", "get", "4.24", "published_tag"),
                         (0, "4.24.6-r1\n", ""))

    def test_get_rejects_an_unknown_key(self):
        code, _, err = self.run_cli("state", "get", "4.24", "nope")
        self.assertEqual(code, 2)
        self.assertIn("nope", err)

    def test_clear_pending_is_idempotent(self):
        self.run_cli("state", "set", "4.24", "pending.version", "4.24.7")
        self.run_cli("state", "set", "4.24", "published_tag", "4.24.6-r1")
        self.assertEqual(self.run_cli("state", "clear-pending", "4.24"),
                         (0, "", ""))
        self.assertEqual(self.state()["branches"]["4.24"],
                         {"published_tag": "4.24.6-r1"})
        self.assertEqual(self.run_cli("state", "clear-pending", "4.24"),
                         (0, "", ""))


class TestMatrix(CatalogTestCase):
    def rows(self, *args):
        code, out, err = self.run_cli("render-matrix", *args)
        self.assertEqual((code, err), (0, ""))
        return [line for line in out.splitlines() if line.startswith("| 4.")]

    def test_three_branches_descending_with_lifecycle_statuses(self):
        rows = self.rows()
        self.assertEqual(len(rows), 3)
        self.assertTrue(rows[0].startswith("| 4.24 | 4.24.6-r1 |"))
        self.assertTrue(rows[0].rstrip().endswith("| current |"))
        self.assertTrue(rows[1].startswith("| 4.23 |"))
        self.assertTrue(rows[1].rstrip().endswith("| maintenance |"))
        self.assertTrue(rows[2].startswith("| 4.22 |"))
        self.assertTrue(rows[2].rstrip().endswith("| security fixes only |"))
        self.assertIn("`4.24.6`", rows[0])
        self.assertIn("`latest`", rows[0])
        self.assertNotIn("latest", rows[1])

    def test_fourth_branch_is_discontinued(self):
        write_catalog(self.root,
                      [("4.21", "4.21.9", 1), ("4.22", "4.22.9", 3),
                       ("4.23", "4.23.4", 2), ("4.24", "4.24.6", 1)],
                      default="4.24")
        rows = self.rows()
        self.assertEqual(len(rows), 4)
        self.assertTrue(rows[3].startswith("| 4.21 |"))
        self.assertTrue(rows[3].rstrip().endswith("| discontinued (EOL) |"))

    def test_rc_series_marks_the_oldest_supported_branch(self):
        rows = self.rows("--rc-series", "4.25")
        self.assertNotIn("deprecation pending", rows[0])
        self.assertNotIn("deprecation pending", rows[1])
        self.assertTrue(rows[2].rstrip().endswith(
            "| security fixes only — deprecation pending (4.25 rc published) |"))

    def test_rc_series_skips_discontinued_branches(self):
        write_catalog(self.root,
                      [("4.21", "4.21.9", 1), ("4.22", "4.22.9", 3),
                       ("4.23", "4.23.4", 2), ("4.24", "4.24.6", 1)],
                      default="4.24")
        rows = self.rows("--rc-series", "4.25")
        self.assertIn("deprecation pending (4.25 rc published)", rows[2])
        self.assertNotIn("deprecation pending", rows[3])

    def test_rc_series_not_newer_than_the_catalog_changes_nothing(self):
        self.assertEqual(self.rows("--rc-series", "4.24"), self.rows())
        self.assertEqual(self.rows("--rc-series", "4.20"), self.rows())

    def test_header_names_the_four_columns(self):
        code, out, _ = self.run_cli("render-matrix")
        self.assertEqual(code, 0)
        self.assertEqual(
            out.splitlines()[0],
            "| Upstream branch | Latest image tag | Aliases "
            "| Upstream support status |")

    def test_update_readme_matrix_rewrites_only_the_marked_block(self):
        self.write("README.md",
                   "# Title\n\nIntro.\n\n## Compatibility matrix\n\n"
                   "<!-- matrix:start -->\n| old |\n|-----|\n| stale |\n"
                   "<!-- matrix:end -->\n\n## Security\n\nTail.\n")
        self.assertEqual(self.run_cli("update-readme-matrix"), (0, "", ""))
        readme = self.read("README.md")
        self.assertNotIn("stale", readme)
        self.assertIn("| 4.24 | 4.24.6-r1 |", readme)
        self.assertTrue(readme.startswith("# Title\n\nIntro.\n"))
        self.assertTrue(readme.endswith("## Security\n\nTail.\n"))
        self.assertIn("<!-- matrix:start -->", readme)
        self.assertIn("<!-- matrix:end -->", readme)

    def test_update_readme_matrix_is_idempotent(self):
        self.write("README.md",
                   "<!-- matrix:start -->\n<!-- matrix:end -->\n")
        self.run_cli("update-readme-matrix")
        once = self.read("README.md")
        self.run_cli("update-readme-matrix")
        self.assertEqual(self.read("README.md"), once)

    def test_update_readme_matrix_refuses_a_readme_without_markers(self):
        self.write("README.md", "# Title\n\nNo markers here.\n")
        code, _, err = self.run_cli("update-readme-matrix")
        self.assertEqual(code, 2)
        self.assertIn("matrix:start", err)


CHANGELOG_FIXTURE = """\
# Changelog

All releases of the `samba-ad-dc` image, newest first.

Entries are written automatically at bump time.
"""


class TestChangelog(CatalogTestCase):
    def setUp(self):
        super().setUp()
        self.write("CHANGELOG.md", CHANGELOG_FIXTURE)

    def test_entry_is_prepended_under_the_intro(self):
        self.assertEqual(
            self.run_cli("changelog-entry", "4.24", "samba-release"),
            (0, "", ""))
        text = self.read("CHANGELOG.md")
        self.assertTrue(text.startswith(CHANGELOG_FIXTURE))
        today = datetime.datetime.now(datetime.timezone.utc).date().isoformat()
        self.assertIn("## 4.24.6-r1 — %s\n" % today, text)
        self.assertIn("- Samba: 4.24.6 (branch 4.24)\n", text)
        self.assertIn("- Trigger: samba-release\n", text)
        self.assertIn("- Image changes: none\n", text)
        self.assertIn("- Fixed CVEs: see the GitHub Release\n", text)
        self.assertIn(
            "- Digests, signature and attestations: "
            "https://github.com/esitc-paris/samba-ad-dc/releases/tag/"
            "v4.24.6-r1\n", text)

    def test_notes_replace_the_none_placeholder(self):
        self.run_cli("changelog-entry", "4.24", "pkg-update",
                     "--notes", "rebuilt on a new base digest")
        self.assertIn("- Image changes: rebuilt on a new base digest\n",
                      self.read("CHANGELOG.md"))

    def test_newest_entry_goes_first(self):
        self.run_cli("changelog-entry", "4.23", "samba-release")
        self.run_cli("changelog-entry", "4.24", "samba-release")
        text = self.read("CHANGELOG.md")
        self.assertLess(text.index("## 4.24.6-r1"), text.index("## 4.23.4-r2"))

    def test_duplicate_tag_exits_3_and_leaves_the_file_alone(self):
        self.run_cli("changelog-entry", "4.24", "samba-release")
        before = self.read("CHANGELOG.md")
        code, _, err = self.run_cli("changelog-entry", "4.24", "manual")
        self.assertEqual(code, 3)
        self.assertIn("4.24.6-r1", err)
        self.assertEqual(self.read("CHANGELOG.md"), before)

    def test_unknown_cause_is_refused(self):
        code, _, err = self.run_cli("changelog-entry", "4.24", "because")
        self.assertNotEqual(code, 0)
        self.assertIn("because", err)

    def test_every_documented_cause_is_accepted(self):
        for cause in ["samba-release", "pkg-update", "base-digest", "manual",
                      "first-publication"]:
            self.write("CHANGELOG.md", CHANGELOG_FIXTURE)
            self.assertEqual(self.run_cli("changelog-entry", "4.24", cause),
                             (0, "", ""), cause)


class TestReleaseNotes(CatalogTestCase):
    def notes(self, *args):
        code, out, err = self.run_cli(
            "release-notes", "4.24", "--cause", "samba-release",
            "--digest-ghcr", "sha256:aaa", *args)
        self.assertEqual((code, err), (0, ""))
        return out

    def test_contains_tag_images_and_both_digests(self):
        out = self.notes("--digest-hub", "sha256:bbb")
        self.assertIn("4.24.6-r1", out)
        self.assertIn("ghcr.io/esitc-paris/samba-ad-dc:4.24.6-r1", out)
        self.assertIn("docker.io/esitcparis/samba-ad-dc:4.24.6-r1", out)
        self.assertIn("sha256:aaa", out)
        self.assertIn("sha256:bbb", out)
        self.assertIn("samba-release", out)

    def test_cosign_verify_command_pins_identity_and_issuer(self):
        out = self.notes()
        self.assertIn(
            "cosign verify ghcr.io/esitc-paris/samba-ad-dc@sha256:aaa", out)
        self.assertIn(
            "--certificate-identity-regexp "
            "'https://github.com/esitc-paris/samba-ad-dc/.*'", out)
        self.assertIn("--certificate-oidc-issuer "
                      "https://token.actions.githubusercontent.com", out)

    def test_hub_digest_is_optional(self):
        out = self.notes()
        self.assertNotIn("Docker Hub", out)
        self.assertIn("ghcr.io/esitc-paris/samba-ad-dc@sha256:aaa", out)

    def test_no_degraded_paragraph_by_default(self):
        self.assertNotIn("9.6", self.notes())
        self.assertNotIn("9.6", self.notes("--degraded", "none"))

    def test_emulated_mode_discloses_mode_1(self):
        out = self.notes("--degraded", "emulated", "--pending-arch", "arm64")
        self.assertIn("§9.6", out)
        self.assertIn("Mode 1", out)
        self.assertIn("arm64", out)

    def test_staggered_mode_discloses_mode_2_and_the_pending_arch(self):
        out = self.notes("--degraded", "staggered", "--pending-arch", "arm64")
        self.assertIn("§9.6", out)
        self.assertIn("Mode 2", out)
        self.assertIn("arm64", out)

    def test_degraded_mode_requires_a_pending_arch(self):
        code, _, err = self.run_cli(
            "release-notes", "4.24", "--cause", "samba-release",
            "--digest-ghcr", "sha256:aaa", "--degraded", "emulated")
        self.assertEqual(code, 2)
        self.assertIn("--pending-arch", err)


LEDGER = """\
# Fixture ledger.
exceptions:
  - cve: CVE-2026-00001
    component: libfoo
    branches: ["4.24"]
    reason: no fixed version published upstream
    introduced: 4.24.6-r1
    review_by: {date}
"""


class TestCveExceptions(CatalogTestCase):
    def check(self, text):
        path = self.write("security/cve-exceptions.yaml", text)
        out, err = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            code = check_cve_exceptions.main(["--file", path])
        return code, out.getvalue(), err.getvalue()

    @staticmethod
    def day(offset):
        today = datetime.datetime.now(datetime.timezone.utc).date()
        return (today + datetime.timedelta(days=offset)).isoformat()

    def test_past_review_date_fails_and_names_the_cve(self):
        code, _, err = self.check(LEDGER.format(date=self.day(-1)))
        self.assertEqual(code, 1)
        self.assertIn("CVE-2026-00001", err)

    def test_review_date_today_still_passes(self):
        code, out, _ = self.check(LEDGER.format(date=self.day(0)))
        self.assertEqual(code, 0)
        self.assertEqual(out.strip(), "cve exceptions: 1 entry, none expired")

    def test_future_review_date_passes(self):
        code, out, _ = self.check(LEDGER.format(date=self.day(30)))
        self.assertEqual(code, 0)
        self.assertIn("none expired", out)

    def test_empty_ledger_passes(self):
        code, out, _ = self.check("exceptions: []\n")
        self.assertEqual(code, 0)
        self.assertEqual(out.strip(), "cve exceptions: 0 entries, none expired")

    def test_plural_wording_for_several_entries(self):
        two = LEDGER.format(date=self.day(30)) + (
            "  - cve: CVE-2026-00002\n"
            "    component: libbar\n"
            "    branches: [\"4.24\"]\n"
            "    reason: not reachable in this image\n"
            "    introduced: 4.24.6-r1\n"
            "    review_by: %s\n" % self.day(60))
        code, out, _ = self.check(two)
        self.assertEqual(code, 0)
        self.assertEqual(out.strip(), "cve exceptions: 2 entries, none expired")

    def test_entry_missing_review_by_is_refused(self):
        code, _, err = self.check(
            "exceptions:\n  - cve: CVE-2026-00003\n    component: libfoo\n")
        self.assertEqual(code, 1)
        self.assertIn("review_by", err)

    def test_committed_ledger_is_valid(self):
        """The real file in the repository must pass its own gate."""
        repo = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
        out, err = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            code = check_cve_exceptions.main(
                ["--file", os.path.join(repo, "security",
                                        "cve-exceptions.yaml")])
        self.assertEqual(code, 0, err.getvalue())


class TestRealCatalog(unittest.TestCase):
    """Guards the shape of the committed versions.yaml, not a fixture."""

    def run_cli(self, *args):
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            code = catalog.main(list(args))
        return code, out.getvalue()

    def test_default_branch_tag_is_readable(self):
        code, out = self.run_cli("default")
        self.assertEqual(code, 0)
        branch = out.strip()
        code, out = self.run_cli("tag", branch)
        self.assertEqual(code, 0)
        self.assertRegex(out.strip(), r"^\d+\.\d+\.\d+-r\d+$")


if __name__ == "__main__":
    unittest.main()
