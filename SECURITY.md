# Security Policy

## Reporting a vulnerability

Report vulnerabilities **privately** via GitHub private vulnerability
reporting:
<https://github.com/esitc-paris/samba-ad-dc/security/advisories/new>.
Do not open public issues for security reports.

## Response times

- Acknowledgment: within **2 business days**.
- Initial assessment and severity classification: within **7 days**.

## Scope

- **This repository** (Dockerfile, entrypoint, CI, watcher integration):
  report here.
- **Samba itself**: report upstream to the Samba Team
  (<https://www.samba.org/samba/security/>). This project does not patch
  Samba; it republishes upstream fixes under its service-level
  commitments (fixable CRITICAL: 48 h; HIGH: 7 days — see SPEC.md §9.2).

## Published-image security

Every published image ships with a cosign signature, SBOM, and SLSA
provenance; verification instructions are in the README from the first
release onward.

Known unfixable CVEs — findings with no available fix, which a rebuild
cannot resolve — are tracked in `security/cve-exceptions.yaml` with a
justification and a review date (SPEC.md §5.4). The ledger records **one
entry per affected package**, not per CVE, and it is not hand-written: the
hourly watcher scans each published image's SBOM and synchronises the file
through `scripts/cve-ledger.py sync`. A sync never moves a review date.
`scripts/check-cve-exceptions.py` is the clock that keeps the ledger from
rotting into the permanent suppression list §5.4 forbids: it fails the
build once a `review_by` date has passed, until a human re-reads the entry
and either extends the date with fresh reasoning or removes it. Nothing in
that file suppresses a finding — the build gate already lets CVEs without
a fixed version through, and the full unfiltered scan is published as a CI
artifact with every build.

Operational procedures around this pipeline are in `docs/operations.md`.
