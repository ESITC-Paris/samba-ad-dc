"""Tests for scripts/cve-ledger.py — the unfixable-CVE ledger sync.

The ledger is a security document with a review clock on it, so the
properties tested here are the ones that decide whether it stays
trustworthy: a review date is never silently renewed, an entry never
outlives the finding that justified it, and an entry a human wrote by
hand is never rewritten by the machine.

Run:  python3 -m unittest scripts/cve_ledger_test.py -v
"""

import datetime
import importlib.util
import json
import os
import sys
import tempfile
import unittest

try:
    import yaml
except ImportError:  # pragma: no cover
    sys.exit("PyYAML is required to run these tests")


def _load():
    """Import cve-ledger.py, whose filename is not an identifier."""
    path = os.path.join(os.path.dirname(os.path.abspath(__file__)),
                        "cve-ledger.py")
    spec = importlib.util.spec_from_file_location("cve_ledger", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


ledger = _load()
check_cve = None

TODAY = datetime.date(2026, 9, 16)

HEADER = """\
# Unfixable-CVE ledger (SPEC §5.4).
# Written by scripts/cve-ledger.py sync.
---
"""


def vulnerability(pkg, cve, severity="HIGH", installed="1.0-1", fixed=None):
    record = {"PkgName": pkg, "VulnerabilityID": cve, "Severity": severity,
              "InstalledVersion": installed}
    if fixed:
        record["FixedVersion"] = fixed
    return record


def report(vulnerabilities):
    return {"Results": [{"Target": "img (debian 13.7)", "Class": "os-pkgs",
                         "Vulnerabilities": vulnerabilities}]}


class Fixture(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.mkdtemp(prefix="cve-ledger-")
        self.ledger_path = os.path.join(self.dir, "cve-exceptions.yaml")
        self.report_path = os.path.join(self.dir, "trivy.json")
        self.write_ledger([])

    def write_ledger(self, entries):
        with open(self.ledger_path, "w", encoding="utf-8") as handle:
            handle.write(ledger.render(HEADER, entries))

    def write_report(self, vulnerabilities):
        with open(self.report_path, "w", encoding="utf-8") as handle:
            json.dump(report(vulnerabilities), handle)

    def read_entries(self):
        with open(self.ledger_path, encoding="utf-8") as handle:
            return (yaml.safe_load(handle) or {}).get("exceptions") or []

    def read_text(self):
        with open(self.ledger_path, encoding="utf-8") as handle:
            return handle.read()

    def sync(self, branch="4.24", tag="4.24.7-r1", now="2026-09-16",
             issue_body=None, review_days=90):
        argv = ["sync", branch, self.report_path, "--tag", tag,
                "--file", self.ledger_path, "--now", now,
                "--review-days", str(review_days)]
        if issue_body:
            argv += ["--issue-body", issue_body]
        return ledger.main(argv)


class Add(Fixture):
    def test_one_entry_per_package_not_per_cve(self):
        self.write_report([
            vulnerability("libxml2", "CVE-2026-0001", installed="2.9.14-2"),
            vulnerability("libxml2", "CVE-2026-0002", "CRITICAL",
                          installed="2.9.14-2"),
            vulnerability("perl-base", "CVE-2026-0003", installed="5.40.1-6"),
        ])
        self.assertEqual(self.sync(), 0)
        entries = self.read_entries()
        self.assertEqual([e["package"] for e in entries],
                         ["libxml2", "perl-base"])
        libxml2 = entries[0]
        self.assertEqual(libxml2["cves"],
                         ["CVE-2026-0001", "CVE-2026-0002"])
        self.assertEqual(libxml2["installed"], "2.9.14-2")
        # The worst severity of the group, so a reader sorting the file by
        # severity is not misled by whichever CVE happened to come first.
        self.assertEqual(libxml2["severity"], "CRITICAL")
        self.assertEqual(libxml2["branches"], ["4.24"])
        self.assertEqual(libxml2["introduced"], "4.24.7-r1")
        self.assertEqual(libxml2["review_by"], TODAY
                         + datetime.timedelta(days=90))
        self.assertIn("no fixed version", libxml2["reason"])

    def test_a_fixable_finding_never_enters_the_ledger(self):
        # §9bis.8.c: a CVE with a fixed version is the build gate's problem
        # and arrives as a package delta. An entry for it would be exactly
        # the suppression §5.4 forbids.
        self.write_report([
            vulnerability("gzip", "CVE-2026-0010", fixed="1.13-1+deb13u1"),
        ])
        self.assertEqual(self.sync(), 0)
        self.assertEqual(self.read_entries(), [])

    def test_low_and_medium_findings_are_not_recorded(self):
        self.write_report([vulnerability("bash", "CVE-2026-0011", "MEDIUM")])
        self.assertEqual(self.sync(), 0)
        self.assertEqual(self.read_entries(), [])

    def test_the_header_comment_survives_the_rewrite(self):
        self.write_report([vulnerability("libxml2", "CVE-2026-0001")])
        self.sync()
        self.assertTrue(self.read_text().startswith(HEADER))

    def test_the_written_file_passes_the_review_clock_gate(self):
        # The sync's whole output has to be readable by the gate that
        # enforces §5.4, or the ledger fails the build it is meant to
        # document.
        self.write_report([vulnerability("libxml2", "CVE-2026-0001")])
        self.sync()
        self.assertEqual(check_cve.main(["--file", self.ledger_path]), 0)


class CarryOver(Fixture):
    def test_a_review_date_is_never_renewed_by_a_sync(self):
        # The one property that makes the clock meaningful: if a sync moved
        # `review_by`, the ledger would renew itself every hour and nothing
        # would ever be reviewed.
        self.write_report([vulnerability("libxml2", "CVE-2026-0001")])
        self.sync(now="2026-06-01")
        first = self.read_entries()[0]["review_by"]
        self.write_report([vulnerability("libxml2", "CVE-2026-0001"),
                           vulnerability("libxml2", "CVE-2026-0099")])
        self.sync(now="2026-09-16")
        entry = self.read_entries()[0]
        self.assertEqual(entry["review_by"], first)
        self.assertEqual(entry["introduced"], "4.24.7-r1")
        # A new CVE on a package already listed updates the entry in place.
        self.assertEqual(entry["cves"], ["CVE-2026-0001", "CVE-2026-0099"])

    def test_a_human_written_reason_survives_a_sync(self):
        # `reason` is the one field of an entry that carries judgement.
        # A sync that overwrote it with the machine default would erase a
        # reviewer's work silently, every hour.
        self.write_report([vulnerability("libxml2", "CVE-2026-0001")])
        self.sync()
        entries = self.read_entries()
        entries[0]["reason"] = ("bundled by Samba, not reachable from the "
                                "DC role; tracked upstream as BUG-1234")
        self.write_ledger(entries)
        self.write_report([vulnerability("libxml2", "CVE-2026-0001"),
                           vulnerability("libxml2", "CVE-2026-0099")])
        self.sync(now="2026-10-01")
        entry = self.read_entries()[0]
        self.assertIn("tracked upstream as BUG-1234", entry["reason"])
        # The rest of the entry still syncs.
        self.assertEqual(entry["cves"], ["CVE-2026-0001", "CVE-2026-0099"])

    def test_the_machine_default_reason_is_still_refreshed(self):
        # An entry nobody has touched keeps tracking the default, so a
        # future change of wording reaches every untouched entry.
        self.write_report([vulnerability("libxml2", "CVE-2026-0001")])
        self.sync()
        self.assertEqual(self.read_entries()[0]["reason"], ledger.REASON)
        self.sync(now="2026-10-01")
        self.assertEqual(self.read_entries()[0]["reason"], ledger.REASON)

    def test_introduced_keeps_the_tag_that_first_shipped_the_package(self):
        self.write_report([vulnerability("libxml2", "CVE-2026-0001")])
        self.sync(tag="4.24.7-r1")
        self.sync(tag="4.24.9-r1", now="2026-11-01")
        self.assertEqual(self.read_entries()[0]["introduced"], "4.24.7-r1")

    def test_a_second_branch_is_added_to_the_same_entry(self):
        self.write_report([vulnerability("libxml2", "CVE-2026-0001")])
        self.sync(branch="4.24")
        self.sync(branch="4.23")
        entries = self.read_entries()
        self.assertEqual(len(entries), 1)
        self.assertEqual(entries[0]["branches"], ["4.23", "4.24"])

    def test_an_unchanged_sync_rewrites_nothing(self):
        # Idempotency: the watcher commits whatever changed, so a sync that
        # reported "changed" on identical input would push an empty commit
        # every hour.
        self.write_report([vulnerability("libxml2", "CVE-2026-0001")])
        self.sync()
        before = self.read_text()
        self.sync(now="2026-10-01")
        self.assertEqual(self.read_text(), before)


class Removal(Fixture):
    def test_a_package_that_lost_its_findings_leaves_the_ledger(self):
        self.write_report([vulnerability("libxml2", "CVE-2026-0001"),
                           vulnerability("perl-base", "CVE-2026-0003")])
        self.sync()
        self.write_report([vulnerability("libxml2", "CVE-2026-0001")])
        self.sync()
        self.assertEqual([e["package"] for e in self.read_entries()],
                         ["libxml2"])

    def test_a_package_still_affecting_another_branch_only_loses_that_branch(self):
        self.write_report([vulnerability("libxml2", "CVE-2026-0001")])
        self.sync(branch="4.24")
        self.sync(branch="4.23")
        self.write_report([])
        self.sync(branch="4.23")
        entries = self.read_entries()
        self.assertEqual(len(entries), 1)
        self.assertEqual(entries[0]["branches"], ["4.24"])

    def test_a_hand_written_per_cve_entry_is_left_alone(self):
        # §5.4's own shape, used for a bundled component the scanner cannot
        # see (Samba's in-tree Heimdal, ngtcp2). The sync owns entries that
        # carry a `package` key and nothing else.
        hand = {"cve": "CVE-2026-7777", "component": "bundled ngtcp2",
                "branches": ["4.24"], "reason": "not a Debian package",
                "introduced": "4.24.7-r1",
                "review_by": datetime.date(2027, 1, 1)}
        self.write_ledger([hand])
        self.write_report([vulnerability("libxml2", "CVE-2026-0001")])
        self.sync()
        entries = self.read_entries()
        self.assertEqual(entries[0], hand)
        self.assertEqual(entries[1]["package"], "libxml2")


class NewPackageDetection(Fixture):
    def capture(self, **kwargs):
        import io
        import contextlib
        buffer = io.StringIO()
        with contextlib.redirect_stdout(buffer):
            with contextlib.redirect_stderr(io.StringIO()):
                self.sync(**kwargs)
        return dict(line.split("=", 1) for line in
                    buffer.getvalue().splitlines() if "=" in line)

    def test_a_brand_new_package_is_reported_for_the_advisory_issue(self):
        self.write_report([vulnerability("libxml2", "CVE-2026-0001")])
        outputs = self.capture()
        self.assertEqual(outputs["new_packages"], "libxml2")
        self.assertEqual(outputs["changed"], "yes")

        # A new CVE on an already-listed package is NOT a new package: the
        # issue would be reopened on every CVE feed refresh otherwise.
        self.write_report([vulnerability("libxml2", "CVE-2026-0001"),
                           vulnerability("libxml2", "CVE-2026-0002")])
        outputs = self.capture()
        self.assertEqual(outputs["new_packages"], "")
        self.assertEqual(outputs["changed"], "yes")

    def test_an_identical_sync_reports_no_change(self):
        self.write_report([vulnerability("libxml2", "CVE-2026-0001")])
        self.capture()
        outputs = self.capture()
        self.assertEqual(outputs["changed"], "no")
        self.assertEqual(outputs["new_packages"], "")

    def test_a_removed_package_is_reported(self):
        self.write_report([vulnerability("libxml2", "CVE-2026-0001")])
        self.capture()
        self.write_report([])
        outputs = self.capture()
        self.assertEqual(outputs["removed_packages"], "libxml2")
        self.assertEqual(outputs["entries"], "0")

    def test_the_issue_body_names_the_packages_and_their_cves(self):
        self.write_report([
            vulnerability("libxml2", "CVE-2026-0001", "CRITICAL",
                          installed="2.9.14-2")])
        path = os.path.join(self.dir, "issue.md")
        self.capture(issue_body=path)
        with open(path, encoding="utf-8") as handle:
            body = handle.read()
        self.assertIn("libxml2", body)
        self.assertIn("CVE-2026-0001", body)
        self.assertIn("4.24.7-r1", body)


def _load_check_cve():
    path = os.path.join(os.path.dirname(os.path.abspath(__file__)),
                        "check-cve-exceptions.py")
    spec = importlib.util.spec_from_file_location("check_cve_exceptions", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


check_cve = _load_check_cve()


if __name__ == "__main__":
    unittest.main()
