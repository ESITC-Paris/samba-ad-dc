"""Tests for scripts/watch.py — the release watcher's decision logic.

The decision table is the part of the watcher that decides what gets
published, so it is tested exhaustively and in isolation: `decide()` is a
pure function of (catalog entry, remembered state, observation, clock),
and every probe that touches the network or docker is injected. Nothing
here opens a socket or runs a container.

`apply()` is tested against real files in a temporary repository, for the
same reason catalog_test.py is: its whole job is editing on-disk
catalog/state/markdown, and a mocked filesystem would only prove the
mocks agree with themselves.

Run:  python3 -m unittest scripts/watch_test.py -v
"""

import datetime
import json
import os
import shutil
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import catalog  # noqa: E402  (needs the sys.path line above)
import watch  # noqa: E402

UTC = datetime.timezone.utc

# Forty lines copied verbatim from https://download.samba.org/pub/samba/stable/
# as served on 2026-09-16, trimmed to the rows that matter. Real markup, not a
# reconstruction: the parser's only job is to survive what that server emits,
# and a hand-written approximation would test the approximation. Three traps
# are deliberately kept in it: the `.tar.asc` rows next to every tarball, the
# `samba-4.24.0rc4.tar.gz` row (a release candidate that leaked into the stable
# listing in the real file's history — the regex must not match it), and the
# 4.23.9/4.23.10 pair, which orders correctly only under numeric comparison.
STABLE_LISTING = """\
<!DOCTYPE HTML PUBLIC "-//W3C//DTD HTML 4.01//EN" "http://www.w3.org/TR/html4/strict.dtd">
<html>
 <head>
  <title>Index of /pub/samba/stable</title>
  <link rel="stylesheet" href="https://www.samba.org/samba/docs/.samba.css" type="text/css">
 </head>
 <body>
  <h1 id="indextitle">Index of /pub/samba/stable</h1>
  <table id="indexlist">
   <tr class="indexhead"><th class="indexcolicon"><img src="/icons/blank.gif" alt="[ICO]"></th><th class="indexcolname"><a href="?C=N;O=D">Name</a></th><th class="indexcollastmod"><a href="?C=M;O=A">Last modified</a></th><th class="indexcolsize"><a href="?C=S;O=A">Size</a></th><th class="indexcoldesc"><a href="?C=D;O=A">Description</a></th></tr>
   <tr class="indexbreakrow"><th colspan="5"><hr></th></tr>
   <tr class="even"><td class="indexcolicon"><img src="/icons/back.gif" alt="[PARENTDIR]"></td><td class="indexcolname"><a href="/pub/samba/">Parent Directory</a></td><td class="indexcollastmod">&nbsp;</td><td class="indexcolsize">  - </td><td class="indexcoldesc">&nbsp;</td></tr>
   <tr class="odd"><td class="indexcolicon"><img src="/icons/compressed.gif" alt="[   ]"></td><td class="indexcolname"><a href="samba-4.22.10.tar.gz">samba-4.22.10.tar.gz</a></td><td class="indexcollastmod">2026-08-13 14:28  </td><td class="indexcolsize"> 40M</td><td class="indexcoldesc">&nbsp;</td></tr>
   <tr class="even"><td class="indexcolicon"><img src="/icons/text.gif" alt="[TXT]"></td><td class="indexcolname"><a href="samba-4.22.11.tar.asc">samba-4.22.11.tar.asc</a></td><td class="indexcollastmod">2026-09-09 15:14  </td><td class="indexcolsize">833 </td><td class="indexcoldesc">&nbsp;</td></tr>
   <tr class="odd"><td class="indexcolicon"><img src="/icons/compressed.gif" alt="[   ]"></td><td class="indexcolname"><a href="samba-4.22.11.tar.gz">samba-4.22.11.tar.gz</a></td><td class="indexcollastmod">2026-09-09 15:14  </td><td class="indexcolsize"> 40M</td><td class="indexcoldesc">&nbsp;</td></tr>
   <tr class="even"><td class="indexcolicon"><img src="/icons/compressed.gif" alt="[   ]"></td><td class="indexcolname"><a href="samba-4.23.9.tar.gz">samba-4.23.9.tar.gz</a></td><td class="indexcollastmod">2026-06-25 09:02  </td><td class="indexcolsize"> 41M</td><td class="indexcoldesc">&nbsp;</td></tr>
   <tr class="odd"><td class="indexcolicon"><img src="/icons/compressed.gif" alt="[   ]"></td><td class="indexcolname"><a href="samba-4.23.10.tar.gz">samba-4.23.10.tar.gz</a></td><td class="indexcollastmod">2026-07-22 15:28  </td><td class="indexcolsize"> 41M</td><td class="indexcoldesc">&nbsp;</td></tr>
   <tr class="even"><td class="indexcolicon"><img src="/icons/compressed.gif" alt="[   ]"></td><td class="indexcolname"><a href="samba-4.23.11.tar.gz">samba-4.23.11.tar.gz</a></td><td class="indexcollastmod">2026-08-13 14:28  </td><td class="indexcolsize"> 41M</td><td class="indexcoldesc">&nbsp;</td></tr>
   <tr class="odd"><td class="indexcolicon"><img src="/icons/compressed.gif" alt="[   ]"></td><td class="indexcolname"><a href="samba-4.23.12.tar.gz">samba-4.23.12.tar.gz</a></td><td class="indexcollastmod">2026-09-09 15:14  </td><td class="indexcolsize"> 41M</td><td class="indexcoldesc">&nbsp;</td></tr>
   <tr class="even"><td class="indexcolicon"><img src="/icons/text.gif" alt="[TXT]"></td><td class="indexcolname"><a href="samba-4.24.0rc4.tar.asc">samba-4.24.0rc4.tar.asc</a></td><td class="indexcollastmod">2026-03-04 11:20  </td><td class="indexcolsize">833 </td><td class="indexcoldesc">&nbsp;</td></tr>
   <tr class="odd"><td class="indexcolicon"><img src="/icons/compressed.gif" alt="[   ]"></td><td class="indexcolname"><a href="samba-4.24.0rc4.tar.gz">samba-4.24.0rc4.tar.gz</a></td><td class="indexcollastmod">2026-03-04 11:20  </td><td class="indexcolsize"> 41M</td><td class="indexcoldesc">&nbsp;</td></tr>
   <tr class="even"><td class="indexcolicon"><img src="/icons/text.gif" alt="[TXT]"></td><td class="indexcolname"><a href="samba-4.24.0.tar.asc">samba-4.24.0.tar.asc</a></td><td class="indexcollastmod">2026-03-18 10:09  </td><td class="indexcolsize">833 </td><td class="indexcoldesc">&nbsp;</td></tr>
   <tr class="odd"><td class="indexcolicon"><img src="/icons/compressed.gif" alt="[   ]"></td><td class="indexcolname"><a href="samba-4.24.0.tar.gz">samba-4.24.0.tar.gz</a></td><td class="indexcollastmod">2026-03-18 10:09  </td><td class="indexcolsize"> 41M</td><td class="indexcoldesc">&nbsp;</td></tr>
   <tr class="even"><td class="indexcolicon"><img src="/icons/compressed.gif" alt="[   ]"></td><td class="indexcolname"><a href="samba-4.24.6.tar.gz">samba-4.24.6.tar.gz</a></td><td class="indexcollastmod">2026-08-13 14:28  </td><td class="indexcolsize"> 41M</td><td class="indexcoldesc">&nbsp;</td></tr>
   <tr class="even"><td class="indexcolicon"><img src="/icons/text.gif" alt="[TXT]"></td><td class="indexcolname"><a href="samba-4.24.7.tar.asc">samba-4.24.7.tar.asc</a></td><td class="indexcollastmod">2026-09-09 15:14  </td><td class="indexcolsize">833 </td><td class="indexcoldesc">&nbsp;</td></tr>
   <tr class="odd"><td class="indexcolicon"><img src="/icons/compressed.gif" alt="[   ]"></td><td class="indexcolname"><a href="samba-4.24.7.tar.gz">samba-4.24.7.tar.gz</a></td><td class="indexcollastmod">2026-09-09 15:14  </td><td class="indexcolsize"> 41M</td><td class="indexcoldesc">&nbsp;</td></tr>
   <tr class="indexbreakrow"><th colspan="5"><hr></th></tr>
  </table>
 </body></html>
"""

# https://download.samba.org/pub/samba/rc/ as served on 2026-09-16. The
# `4.24.0rc/` directory row is kept on purpose: a series must be read from a
# tarball name, never from a leftover directory, or the watcher would report
# every series that ever had a candidate.
RC_LISTING = """\
  <table id="indexlist">
   <tr class="odd"><td class="indexcolicon"><img src="/icons/folder.gif" alt="[DIR]"></td><td class="indexcolname"><a href="4.24.0rc/">4.24.0rc/</a></td><td class="indexcollastmod">2026-08-10 17:03  </td><td class="indexcolsize">  - </td><td class="indexcoldesc">&nbsp;</td></tr>
   <tr class="even"><td class="indexcolicon"><img src="/icons/text.gif" alt="[TXT]"></td><td class="indexcolname"><a href="samba-4.25.0rc2.WHATSNEW.txt">samba-4.25.0rc2.WHATSNEW.txt</a></td><td class="indexcollastmod">2026-09-06 15:22  </td><td class="indexcolsize">9.2K</td><td class="indexcoldesc">&nbsp;</td></tr>
   <tr class="odd"><td class="indexcolicon"><img src="/icons/text.gif" alt="[TXT]"></td><td class="indexcolname"><a href="samba-4.25.0rc2.tar.asc">samba-4.25.0rc2.tar.asc</a></td><td class="indexcollastmod">2026-09-06 15:22  </td><td class="indexcolsize">833 </td><td class="indexcoldesc">&nbsp;</td></tr>
   <tr class="even"><td class="indexcolicon"><img src="/icons/compressed.gif" alt="[   ]"></td><td class="indexcolname"><a href="samba-4.25.0rc1.tar.gz">samba-4.25.0rc1.tar.gz</a></td><td class="indexcollastmod">2026-08-24 11:41  </td><td class="indexcolsize"> 42M</td><td class="indexcoldesc">&nbsp;</td></tr>
   <tr class="odd"><td class="indexcolicon"><img src="/icons/compressed.gif" alt="[   ]"></td><td class="indexcolname"><a href="samba-4.25.0rc2.tar.gz">samba-4.25.0rc2.tar.gz</a></td><td class="indexcollastmod">2026-09-06 15:22  </td><td class="indexcolsize"> 42M</td><td class="indexcoldesc">&nbsp;</td></tr>
  </table>
"""

DIGEST_A = "sha256:" + "a" * 64
DIGEST_B = "sha256:" + "b" * 64
DIGEST_GO = "sha256:" + "c" * 64
HASH_A = "6b233ad0c4404928"
HASH_B = "0123456789abcdef"

NOW = datetime.datetime(2026, 9, 16, 12, 0, tzinfo=UTC)


def entry(version="4.24.7", revision=1):
    """A catalog entry for branch 4.24, as load_catalog would return it."""
    return {
        "samba_version": version,
        "revision": revision,
        "tarball_sha256": "0" * 64,
        "base": {
            "builder": "debian:trixie-slim@" + DIGEST_A,
            "runtime": "debian:trixie-slim@" + DIGEST_A,
            "gobuild": "golang:1.25-trixie@" + DIGEST_GO,
        },
        "pkg_index_hash": HASH_A,
    }


def state(**overrides):
    """Remembered state for branch 4.24 that agrees with entry()."""
    data = {
        "runtime_digest": DIGEST_A,
        "builder_digest": DIGEST_A,
        "gobuild_digest": DIGEST_GO,
        "pkg_index_hash": HASH_A,
    }
    data.update(overrides)
    return data


def observation(**overrides):
    """An observation for branch 4.24 that agrees with entry() and state()."""
    data = {
        "latest_patch": "4.24.7",
        "runtime_digest": DIGEST_A,
        "builder_digest": DIGEST_A,
        "gobuild_digest": DIGEST_GO,
        "pkg_index_hash": HASH_A,
        "published_tags": ["4.24.7-r1"],
    }
    data.update(overrides)
    return data


def decide(obs=None, st=None, ent=None, now=NOW, soak_hours=24,
           security=False):
    return watch.decide("4.24", ent or entry(), st or state(),
                        obs or observation(), now, soak_hours, security)


class VersionOrdering(unittest.TestCase):
    def test_patch_numbers_compare_numerically_not_lexically(self):
        # The one ordering bug a string compare would introduce, and the
        # reason `sort -V` semantics are required by the brief: "4.24.9"
        # sorts above "4.24.10" as text.
        self.assertGreater(watch.version_key("4.24.10"),
                           watch.version_key("4.24.9"))
        self.assertGreater(watch.version_key("4.25.0"),
                           watch.version_key("4.24.99"))

    def test_latest_patch_picks_the_highest_of_its_own_branch(self):
        versions = ["4.23.9", "4.23.10", "4.24.0", "4.24.7"]
        self.assertEqual(watch.latest_patch(versions, "4.23"), "4.23.10")
        self.assertEqual(watch.latest_patch(versions, "4.24"), "4.24.7")
        self.assertEqual(watch.latest_patch(versions, "4.21"), "")


class ListingParser(unittest.TestCase):
    def test_stable_listing_yields_releases_and_never_candidates(self):
        versions = watch.parse_stable_listing(STABLE_LISTING)
        self.assertIn("4.24.7", versions)
        self.assertIn("4.23.12", versions)
        self.assertIn("4.22.11", versions)
        # samba-4.24.0rc4.tar.gz is in the fixture; the regex requires the
        # patch number to be followed immediately by `.tar.gz`.
        self.assertNotIn("4.24.0rc4", versions)
        self.assertNotIn("4.24.04", versions)
        # `.tar.asc` rows carry the same version and must not double-count.
        self.assertEqual(len(versions), len(set(versions)))
        self.assertEqual(versions, sorted(versions, key=watch.version_key))

    def test_stable_listing_orders_10_above_9(self):
        self.assertEqual(watch.latest_patch(
            watch.parse_stable_listing(STABLE_LISTING), "4.23"), "4.23.12")

    def test_rc_listing_reads_series_from_tarballs_only(self):
        self.assertEqual(watch.parse_rc_listing(RC_LISTING),
                         ["4.25.0rc1", "4.25.0rc2"])
        self.assertEqual(watch.rc_series(RC_LISTING), ["4.25"])

    def test_an_empty_body_parses_to_nothing_rather_than_raising(self):
        # A truncated response must read as "no information", which the
        # caller turns into "no action" — never into a downgrade.
        self.assertEqual(watch.parse_stable_listing(""), [])


class DecisionTable(unittest.TestCase):
    def test_a_newly_seen_higher_patch_starts_the_soak_and_acts_on_nothing(self):
        decision = decide(observation(latest_patch="4.24.8"))
        self.assertEqual(decision["action"], "none")
        self.assertEqual(decision["pending"],
                         {"version": "4.24.8", "first_seen": NOW.isoformat()})
        self.assertIn("soak", " ".join(decision["notes"]).lower())

    def test_a_version_still_inside_its_soak_acts_on_nothing(self):
        seen = (NOW - datetime.timedelta(hours=6)).isoformat()
        decision = decide(
            observation(latest_patch="4.24.8"),
            state(pending={"version": "4.24.8", "first_seen": seen}))
        self.assertEqual(decision["action"], "none")
        # Still soaking: the pending record is left exactly as it was, or
        # first_seen would be restamped on every hourly run and the soak
        # would never elapse.
        self.assertIsNone(decision["pending"])
        self.assertIn("18.0 h", " ".join(decision["notes"]))

    def test_a_version_whose_soak_elapsed_is_released(self):
        seen = (NOW - datetime.timedelta(hours=24)).isoformat()
        decision = decide(
            observation(latest_patch="4.24.8"),
            state(pending={"version": "4.24.8", "first_seen": seen}))
        self.assertEqual(decision["action"], "version")
        self.assertEqual(decision["cause"], "samba-release")
        self.assertEqual(decision["version"], "4.24.8")

    def test_a_security_run_releases_without_waiting(self):
        # §9bis.5: security fixes bypass the soak (0 hours). The pending
        # record does not even have to exist yet.
        decision = decide(observation(latest_patch="4.24.8"), security=True)
        self.assertEqual(decision["action"], "version")
        self.assertEqual(decision["cause"], "samba-release")
        self.assertEqual(decision["version"], "4.24.8")

    def test_a_moved_base_digest_is_a_revision(self):
        decision = decide(observation(runtime_digest=DIGEST_B))
        self.assertEqual(decision["action"], "revision")
        self.assertEqual(decision["cause"], "base-digest")
        self.assertEqual(decision["state"]["runtime_digest"], DIGEST_B)

    def test_a_moved_builder_or_gobuild_digest_is_a_revision_too(self):
        for key in ("builder_digest", "gobuild_digest"):
            decision = decide(observation(**{key: DIGEST_B}))
            self.assertEqual(decision["action"], "revision", key)
            self.assertEqual(decision["cause"], "base-digest", key)

    def test_a_moved_package_closure_hash_is_a_revision(self):
        decision = decide(observation(pkg_index_hash=HASH_B))
        self.assertEqual(decision["action"], "revision")
        self.assertEqual(decision["cause"], "pkg-update")
        self.assertEqual(decision["state"]["pkg_index_hash"], HASH_B)

    def test_an_unpublished_current_tag_publishes_itself(self):
        # The self-heal: nothing changed upstream, but the tag the catalog
        # names is not on the registry (first publication, or a dispatch
        # that was lost).
        decision = decide(observation(published_tags=[]))
        self.assertEqual(decision["action"], "publish")
        self.assertEqual(decision["cause"], "first-publication")
        self.assertEqual(decision["tag"], "4.24.7-r1")

    def test_a_published_and_unchanged_branch_does_nothing(self):
        decision = decide()
        self.assertEqual(decision["action"], "none")
        self.assertEqual(decision["cause"], "")
        self.assertEqual(decision["state"], {})

    def test_an_older_upstream_version_is_ignored_with_a_warning(self):
        # The monotonicity guard. A transient mirror or a partial listing
        # must never be able to auto-publish a downgrade as :latest.
        decision = decide(observation(latest_patch="4.24.6"))
        self.assertEqual(decision["action"], "none")
        self.assertIn("4.24.6", " ".join(decision["notes"]))
        self.assertIn("ignor", " ".join(decision["notes"]).lower())

    def test_an_older_upstream_version_does_not_mask_a_digest_change(self):
        decision = decide(observation(latest_patch="4.24.6",
                                      runtime_digest=DIGEST_B))
        self.assertEqual(decision["action"], "revision")
        self.assertEqual(decision["cause"], "base-digest")

    def test_an_empty_latest_patch_is_no_information_not_a_downgrade(self):
        decision = decide(observation(latest_patch=""))
        self.assertEqual(decision["action"], "none")

    def test_an_empty_digest_is_never_compared_and_never_written(self):
        # An empty digest compares unequal to the stored one, which would
        # trigger a revision and then write "" into the state — making
        # every later run bump again. observe() fails hard instead; decide()
        # refuses to act on one as a second line of defence.
        decision = decide(observation(runtime_digest=""))
        self.assertEqual(decision["action"], "none")

    def test_a_withdrawn_pending_version_is_forgotten(self):
        # Upstream pulled 4.24.8 while it was soaking: the listing no longer
        # offers anything above the pin, so the stale pending record has to
        # go or it would be compared against forever.
        decision = decide(
            observation(latest_patch="4.24.7"),
            state(pending={"version": "4.24.8",
                           "first_seen": NOW.isoformat()}))
        self.assertEqual(decision["action"], "none")
        self.assertTrue(decision["clear_pending"])

    def test_a_superseded_pending_version_restarts_the_soak(self):
        # 4.24.8 was soaking, 4.24.9 appeared: the soak restarts on the new
        # version rather than inheriting the old one's clock.
        seen = (NOW - datetime.timedelta(hours=23)).isoformat()
        decision = decide(
            observation(latest_patch="4.24.9"),
            state(pending={"version": "4.24.8", "first_seen": seen}))
        self.assertEqual(decision["action"], "none")
        self.assertEqual(decision["pending"]["version"], "4.24.9")
        self.assertEqual(decision["pending"]["first_seen"], NOW.isoformat())

    def test_a_version_bump_carries_the_observed_digests_into_state(self):
        seen = (NOW - datetime.timedelta(hours=48)).isoformat()
        decision = decide(
            observation(latest_patch="4.24.8", runtime_digest=DIGEST_B,
                        pkg_index_hash=HASH_B),
            state(pending={"version": "4.24.8", "first_seen": seen}))
        self.assertEqual(decision["action"], "version")
        self.assertEqual(decision["state"]["runtime_digest"], DIGEST_B)
        self.assertEqual(decision["state"]["pkg_index_hash"], HASH_B)
        self.assertTrue(decision["clear_pending"])


class SeriesDetection(unittest.TestCase):
    branches = ["4.22", "4.23", "4.24"]

    def test_a_release_candidate_for_a_newer_series_is_a_deprecation_signal(self):
        decisions = watch.series_decisions(
            self.branches, ["4.24.7"], ["4.25.0rc2"])
        kinds = {d["action"]: d for d in decisions}
        self.assertIn("rc_series", kinds)
        self.assertEqual(kinds["rc_series"]["series"], "4.25")
        self.assertEqual(kinds["rc_series"]["oldest"], "4.22")
        self.assertEqual(kinds["rc_series"]["title"],
                         "Deprecation pending: branch 4.22 (4.25 rc published)")
        self.assertNotIn("new_series", kinds)

    def test_no_candidate_above_the_catalog_is_silent(self):
        # 4.24.0rc4 is a candidate for a series already in the catalog.
        self.assertEqual(
            watch.series_decisions(self.branches, ["4.24.7"], ["4.24.0rc4"]),
            [])

    def test_a_stable_release_of_an_unknown_series_asks_a_human(self):
        decisions = watch.series_decisions(
            self.branches, ["4.24.7", "4.25.0"], ["4.25.0rc2"])
        kinds = {d["action"]: d for d in decisions}
        self.assertEqual(kinds["new_series"]["series"], "4.25")
        self.assertEqual(kinds["new_series"]["title"],
                         "New upstream series 4.25 — catalog decision required")
        # The rc signal stops once the series is out: 4.25 is no longer
        # "greater than the newest catalog branch" in a way the matrix can
        # relay, it is a catalog decision.
        self.assertIn("rc_series", kinds)

    def test_only_the_highest_stable_series_is_reported(self):
        # Old series (4.21, 4.20 …) sit in the same listing forever and are
        # not news.
        decisions = watch.series_decisions(
            self.branches, ["4.21.9", "4.24.7"], [])
        self.assertEqual(decisions, [])


class Plan(unittest.TestCase):
    def setUp(self):
        self.root = tempfile.mkdtemp(prefix="watch-plan-")
        self.addCleanup(shutil.rmtree, self.root, ignore_errors=True)
        shutil.copy(os.path.join(REPO, "versions.yaml"),
                    os.path.join(self.root, "versions.yaml"))
        shutil.copy(os.path.join(REPO, ".build-state.json"),
                    os.path.join(self.root, ".build-state.json"))

    def observation_document(self):
        catalogue = catalog.load_catalog(self.root)
        branches = {}
        for branch in catalog.sorted_branches(catalogue):
            item = catalog.entry_of(catalogue, branch)
            branches[branch] = {
                "latest_patch": item["samba_version"],
                "runtime_digest": item["base"]["runtime"].split("@", 1)[1],
                "builder_digest": item["base"]["builder"].split("@", 1)[1],
                "gobuild_digest": item["base"]["gobuild"].split("@", 1)[1],
                "pkg_index_hash": item["pkg_index_hash"],
            }
        return {
            "stable_versions": ["4.22.11", "4.23.12", "4.24.7"],
            "rc_versions": ["4.25.0rc2"],
            "published_tags": [],
            "branches": branches,
        }

    def test_nothing_published_yet_plans_a_publication_per_branch(self):
        decisions = watch.plan(self.root, self.observation_document(),
                               NOW, 24, False)
        by_branch = {d["branch"]: d for d in decisions if d.get("branch")}
        self.assertEqual(sorted(by_branch), ["4.22", "4.23", "4.24"])
        for branch, decision in by_branch.items():
            self.assertEqual(decision["action"], "publish", branch)
            self.assertEqual(decision["cause"], "first-publication", branch)
        # The 4.25 candidate is relayed in the same run.
        self.assertEqual([d["action"] for d in decisions if not d.get("branch")],
                         ["rc_series"])

    def test_a_published_catalog_plans_nothing(self):
        document = self.observation_document()
        document["published_tags"] = ["4.22.11-r1", "4.23.12-r1", "4.24.7-r1"]
        decisions = watch.plan(self.root, document, NOW, 24, False)
        self.assertEqual(
            [d["action"] for d in decisions if d.get("branch")],
            ["none", "none", "none"])


class ObserveIdempotency(unittest.TestCase):
    """Two runs against an unchanged world must leave an unchanged file.

    `observe` persists the source cache into .build-state.json and the
    workflow commits whatever changed there, so any field that moves on its
    own — a timestamp, most obviously — is a commit and a push to `main`
    every hour carrying no information. That is the opposite of §9bis.3,
    and it is invisible in every other test because they all look at the
    decisions rather than at the bytes.
    """

    def setUp(self):
        self.root = tempfile.mkdtemp(prefix="watch-observe-")
        self.addCleanup(shutil.rmtree, self.root, ignore_errors=True)
        shutil.copy(os.path.join(REPO, "versions.yaml"),
                    os.path.join(self.root, "versions.yaml"))
        shutil.copy(os.path.join(REPO, ".build-state.json"),
                    os.path.join(self.root, ".build-state.json"))

    @staticmethod
    def fetch(url, headers=None, **_):
        body = RC_LISTING if url.endswith("/rc/") else STABLE_LISTING
        return watch.Response(200, body, {})

    @staticmethod
    def command(argv, **_):
        # Not named `run`: TestCase.run(result) is the test runner's own
        # entry point, and shadowing it makes every test in the class fail
        # inside unittest rather than in the code under test.
        joined = " ".join(argv)
        if "imagetools" in joined:
            return DIGEST_GO + "\n" if "golang" in joined else DIGEST_A + "\n"
        if "pkg-closure-hash.sh" in joined:
            return HASH_A + "\n"
        if argv[0] == "gh":
            return "4.24.7-r1\n"
        raise AssertionError("unexpected command: " + joined)

    def state_bytes(self):
        with open(os.path.join(self.root, ".build-state.json"),
                  encoding="utf-8") as handle:
            return handle.read()

    def observe(self, now):
        return watch.observe(self.root, fetch=self.fetch, run=self.command,
                             sleep=lambda _: None, now=now)

    def test_an_unchanged_world_leaves_the_state_file_byte_identical(self):
        self.observe(NOW)
        first = self.state_bytes()
        # An hour later, same answers from every source.
        self.observe(NOW + datetime.timedelta(hours=1))
        self.assertEqual(self.state_bytes(), first)

    def test_the_cache_holds_only_what_the_server_served(self):
        self.observe(NOW)
        state = json.loads(self.state_bytes())
        self.assertEqual(sorted(state["sources"]["stable"]),
                         ["etag", "last_modified", "versions"])

    def test_a_dry_run_observe_writes_no_state_at_all(self):
        before = self.state_bytes()
        watch.observe(self.root, fetch=self.fetch, run=self.command,
                      sleep=lambda _: None, now=NOW, write_state=False)
        self.assertEqual(self.state_bytes(), before)


class Probes(unittest.TestCase):
    def test_a_digest_probe_retries_and_then_gives_up_loudly(self):
        attempts = []

        def run(argv, **_):
            attempts.append(argv)
            return ""

        with self.assertRaises(watch.Refusal):
            watch.resolve_digest("debian:trixie-slim", run=run, sleep=lambda _: None)
        self.assertEqual(len(attempts), 3)

    def test_a_digest_probe_accepts_the_first_non_empty_answer(self):
        answers = ["", DIGEST_A]

        def run(argv, **_):
            return answers.pop(0)

        self.assertEqual(
            watch.resolve_digest("debian:trixie-slim", run=run,
                                 sleep=lambda _: None),
            DIGEST_A)

    def test_a_digest_probe_refuses_anything_that_is_not_a_digest(self):
        with self.assertRaises(watch.Refusal):
            watch.resolve_digest("debian:trixie-slim",
                                 run=lambda argv, **_: "not-a-digest",
                                 sleep=lambda _: None)

    def test_the_package_closure_hash_comes_from_the_shared_script(self):
        # scripts/pkg-closure-hash.sh is THE definition (SPEC §9bis.1.c);
        # the watcher calls it and validates its shape, and must never grow
        # a second implementation of the same rule.
        calls = []

        def run(argv, **_):
            calls.append(argv)
            return HASH_A + "\n"

        value = watch.package_closure_hash(
            "/repo", "debian:trixie-slim@" + DIGEST_A, run=run)
        self.assertEqual(value, HASH_A)
        self.assertIn("pkg-closure-hash.sh", " ".join(calls[0]))
        self.assertIn("debian:trixie-slim@" + DIGEST_A, calls[0])

    def test_a_malformed_package_closure_hash_is_refused(self):
        for bad in ("", "not hex", "6b233ad0c440492"):  # 15 characters
            with self.assertRaises(watch.Refusal):
                watch.package_closure_hash("/repo", "ref",
                                           run=lambda argv, **_: bad)

    def test_a_missing_package_on_ghcr_reads_as_no_tags(self):
        def run(argv, **_):
            raise watch.CommandFailed("gh: Package not found. (HTTP 404)", 1)

        self.assertEqual(watch.published_tags("samba-ad-dc", run=run), [])

    def test_any_other_gh_failure_is_not_swallowed(self):
        def run(argv, **_):
            raise watch.CommandFailed("gh: Bad credentials (HTTP 401)", 1)

        with self.assertRaises(watch.Refusal):
            watch.published_tags("samba-ad-dc", run=run)

    def test_a_not_modified_listing_reuses_the_cached_versions(self):
        cache = {"etag": '"abc"', "versions": ["4.24.7"]}
        calls = []

        def fetch(url, headers=None, **_):
            calls.append((url, headers))
            return watch.Response(304, "", {})

        versions, updated = watch.fetch_listing(
            "https://example.invalid/stable/", cache,
            watch.parse_stable_listing, fetch=fetch, sleep=lambda _: None)
        self.assertEqual(versions, ["4.24.7"])
        self.assertEqual(updated["etag"], '"abc"')
        self.assertEqual(calls[0][1].get("If-None-Match"), '"abc"')

    def test_a_200_listing_replaces_the_cache(self):
        def fetch(url, headers=None, **_):
            return watch.Response(200, STABLE_LISTING,
                                  {"ETag": '"new"',
                                   "Last-Modified": "Wed, 16 Sep 2026 00:00:00 GMT"})

        versions, updated = watch.fetch_listing(
            "https://example.invalid/stable/", {},
            watch.parse_stable_listing, fetch=fetch, sleep=lambda _: None)
        self.assertIn("4.24.7", versions)
        self.assertEqual(updated["etag"], '"new"')
        self.assertEqual(updated["last_modified"],
                         "Wed, 16 Sep 2026 00:00:00 GMT")
        self.assertEqual(updated["versions"], versions)


REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

DOCKERFILE_FIXTURE = """\
# syntax=docker/dockerfile:1
ARG BUILDER_BASE=debian:trixie-slim@{digest_a}
ARG RUNTIME_BASE=debian:trixie-slim@{digest_a}
ARG GOBUILD_BASE=golang:1.25-trixie@{digest_go}
ARG SAMBA_VERSION=4.24.7
ARG SAMBA_TARBALL_SHA256={sha}
FROM scratch AS runtime
ARG SAMBA_VERSION=4.24.7
ARG PKG_INDEX_HASH=bootstrap
ARG BASE_NAME=debian:trixie-slim
ARG BASE_DIGEST={digest_a}
""".format(digest_a=DIGEST_A, digest_go=DIGEST_GO, sha="0" * 64)


class Apply(unittest.TestCase):
    """apply() against real files, as the watcher runs it."""

    def setUp(self):
        self.root = tempfile.mkdtemp(prefix="watch-apply-")
        self.addCleanup(shutil.rmtree, self.root, ignore_errors=True)
        for name in ("versions.yaml", ".build-state.json", "CHANGELOG.md",
                     "README.md"):
            shutil.copy(os.path.join(REPO, name),
                        os.path.join(self.root, name))
        with open(os.path.join(self.root, "Dockerfile"), "w",
                  encoding="utf-8") as handle:
            handle.write(DOCKERFILE_FIXTURE)
        self.run_calls = []

    def fake_run(self, argv, **_):
        self.run_calls.append(argv)
        if "verify-upstream-tarball.sh" in " ".join(argv):
            return "sha256=" + "f" * 64 + "\n"
        return ""

    def read(self, name):
        with open(os.path.join(self.root, name), encoding="utf-8") as handle:
            return handle.read()

    def snapshot(self):
        return {name: self.read(name) for name in
                ("versions.yaml", ".build-state.json", "CHANGELOG.md",
                 "README.md", "Dockerfile")}

    def publish_decision(self, branch="4.24", tag="4.24.7-r1"):
        return {"branch": branch, "action": "publish",
                "cause": "first-publication", "tag": tag, "version": "4.24.7",
                "state": {}, "pending": None, "clear_pending": False,
                "notes": []}

    def test_a_dry_run_changes_no_file_and_still_reports_the_tags(self):
        before = self.snapshot()
        outputs = watch.apply(self.root, [self.publish_decision()],
                              dry_run=True, run=self.fake_run)
        self.assertEqual(self.snapshot(), before)
        self.assertEqual(outputs["tags"], "v4.24.7-r1")
        self.assertEqual(outputs["dispatch"],
                         "4.24:v4.24.7-r1:first-publication")
        self.assertEqual(self.run_calls, [])

    def test_a_publication_edits_no_pin_and_records_the_dispatch(self):
        before = self.snapshot()
        outputs = watch.apply(self.root, [self.publish_decision()],
                              run=self.fake_run)
        self.assertEqual(self.read("versions.yaml"), before["versions.yaml"])
        self.assertEqual(self.read("CHANGELOG.md"), before["CHANGELOG.md"])
        self.assertEqual(outputs["tags"], "v4.24.7-r1")
        self.assertEqual(
            catalog.state_get(self.root, "4.24", "published_tag"),
            "4.24.7-r1")

    def test_an_unknown_branch_is_refused(self):
        # catalog.py's `state set` does not validate the branch against the
        # catalog, so a typo would silently create a state entry nothing
        # ever reads. apply() is where that is caught.
        with self.assertRaises(watch.Refusal):
            watch.apply(self.root, [self.publish_decision(branch="9.99")],
                        run=self.fake_run)

    def test_a_version_bump_moves_catalog_dockerfile_state_and_changelog(self):
        decision = {
            "branch": "4.24", "action": "version", "cause": "samba-release",
            "tag": "4.24.7-r1", "version": "4.24.8",
            "state": {"runtime_digest": DIGEST_B, "builder_digest": DIGEST_B,
                      "gobuild_digest": DIGEST_GO, "pkg_index_hash": HASH_B},
            "pending": None, "clear_pending": True, "notes": [],
        }
        outputs = watch.apply(self.root, [decision], run=self.fake_run)

        self.assertEqual(outputs["tags"], "v4.24.8-r1")
        self.assertEqual(outputs["causes"], "samba-release")
        self.assertIn("4.24.8", self.read("versions.yaml"))
        self.assertIn("f" * 64, self.read("versions.yaml"))
        self.assertIn(HASH_B, self.read("versions.yaml"))
        # 4.24 is the catalog's default branch, and the Dockerfile mirrors
        # that branch's pins as ARG defaults — check-pins-consistency.sh
        # asserts the pair, so both move together or CI goes red.
        dockerfile = self.read("Dockerfile")
        self.assertIn("ARG SAMBA_VERSION=4.24.8", dockerfile)
        self.assertNotIn("ARG SAMBA_VERSION=4.24.7", dockerfile)
        self.assertIn("ARG RUNTIME_BASE=debian:trixie-slim@" + DIGEST_B,
                      dockerfile)
        self.assertIn("ARG BASE_DIGEST=" + DIGEST_B, dockerfile)
        self.assertIn("ARG BASE_NAME=debian:trixie-slim", dockerfile)
        # PKG_INDEX_HASH floats on purpose (its ARG default is the fallback
        # for a bare `docker build .`; CI injects the catalog value).
        self.assertIn("ARG PKG_INDEX_HASH=bootstrap", dockerfile)
        self.assertIn("## 4.24.8-r1", self.read("CHANGELOG.md"))
        self.assertEqual(catalog.state_get(self.root, "4.24", "pending.version"),
                         "")
        self.assertEqual(
            catalog.state_get(self.root, "4.24", "runtime_digest"), DIGEST_B)
        self.assertTrue(any("verify-upstream-tarball.sh" in " ".join(argv)
                            for argv in self.run_calls))

    def test_a_revision_bump_counts_up_and_names_its_cause(self):
        decision = {
            "branch": "4.23", "action": "revision", "cause": "pkg-update",
            "tag": "4.23.12-r1", "version": "4.23.12",
            "state": {"pkg_index_hash": HASH_B}, "pending": None,
            "clear_pending": False, "notes": [],
        }
        outputs = watch.apply(self.root, [decision], run=self.fake_run)
        self.assertEqual(outputs["tags"], "v4.23.12-r2")
        self.assertIn("Trigger: pkg-update", self.read("CHANGELOG.md"))
        # A non-default branch never touches the Dockerfile's ARG defaults:
        # they mirror the default branch alone.
        self.assertIn("ARG SAMBA_VERSION=4.24.7", self.read("Dockerfile"))

    def test_a_soaking_branch_writes_its_pending_record_and_no_tag(self):
        # The state-only commit case: no bump, no tag, but first_seen has
        # to survive to the next hourly run or the soak restarts forever.
        decision = {
            "branch": "4.24", "action": "none", "cause": "", "tag": "4.24.7-r1",
            "version": "4.24.7", "state": {},
            "pending": {"version": "4.24.8", "first_seen": NOW.isoformat()},
            "clear_pending": False, "notes": [],
        }
        outputs = watch.apply(self.root, [decision], run=self.fake_run)
        self.assertEqual(outputs["tags"], "")
        self.assertEqual(
            catalog.state_get(self.root, "4.24", "pending.version"), "4.24.8")
        self.assertEqual(
            catalog.state_get(self.root, "4.24", "pending.first_seen"),
            NOW.isoformat())

    def test_a_release_candidate_decision_only_moves_the_readme_matrix(self):
        decision = {"branch": "", "action": "rc_series", "series": "4.25",
                    "oldest": "4.22", "title": "t", "body": "b", "notes": []}
        outputs = watch.apply(self.root, [decision], run=self.fake_run)
        self.assertEqual(outputs["tags"], "")
        self.assertIn("deprecation pending (4.25 rc published)",
                      self.read("README.md"))

    def test_a_tag_the_remote_already_carries_is_not_recreated(self):
        # The lost-dispatch replay: run N pushed the tag and its dispatch
        # never landed, so run N+1 decides `publish` for the same tag. It
        # must be dispatched again and NOT re-created — re-pushing an
        # existing tag name is rejected, and `--atomic` takes main down
        # with it, on that run and on every run after it.
        outputs = watch.apply(self.root, [self.publish_decision()],
                              run=self.fake_run,
                              existing_tags=["v4.24.7-r1", "v4.20.0-r1"])
        self.assertEqual(outputs["tags"], "v4.24.7-r1")
        self.assertEqual(outputs["new_tags"], "")
        self.assertEqual(outputs["existing_tags"], "v4.24.7-r1")
        # Still dispatched: "the tag exists" and "the image is published"
        # are different facts, and only the second one was checked.
        self.assertEqual(outputs["dispatch"],
                         "4.24:v4.24.7-r1:first-publication")

    def test_an_unknown_remote_tag_list_makes_every_tag_new(self):
        outputs = watch.apply(self.root, [self.publish_decision()],
                              run=self.fake_run, existing_tags=[])
        self.assertEqual(outputs["new_tags"], "v4.24.7-r1")
        self.assertEqual(outputs["existing_tags"], "")

    def test_one_stuck_tag_does_not_hold_back_another_branch(self):
        outputs = watch.apply(
            self.root,
            [self.publish_decision("4.23", "4.23.12-r1"),
             self.publish_decision("4.24", "4.24.7-r1")],
            run=self.fake_run, existing_tags=["v4.23.12-r1"])
        self.assertEqual(outputs["new_tags"], "v4.24.7-r1")
        self.assertEqual(outputs["existing_tags"], "v4.23.12-r1")
        self.assertEqual(outputs["dispatch"],
                         "4.23:v4.23.12-r1:first-publication "
                         "4.24:v4.24.7-r1:first-publication")

    def test_two_branches_release_together(self):
        decisions = [self.publish_decision("4.23", "4.23.12-r1"),
                     self.publish_decision("4.24", "4.24.7-r1")]
        outputs = watch.apply(self.root, decisions, run=self.fake_run)
        self.assertEqual(outputs["tags"], "v4.23.12-r1 v4.24.7-r1")
        self.assertEqual(outputs["dispatch"],
                         "4.23:v4.23.12-r1:first-publication "
                         "4.24:v4.24.7-r1:first-publication")


if __name__ == "__main__":
    unittest.main()
