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
provenance; verification instructions are in the README. Known
unfixable CVEs are tracked in `security/cve-exceptions.yaml` with review
dates (SPEC.md §5.4).
