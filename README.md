# samba-ad-dc

Production-grade container image for a **Samba Active Directory Domain
Controller**, built from verified upstream source with Samba's bundled
Heimdal Kerberos.

> **Status: pre-release.** No image has been published yet. Everything
> below the status line describes the target state and is completed
> before the first release; empty sections are intentionally present as
> the documented contract (SPEC.md §10.1).

## Non-affiliation notice

This is an independent community build. It is **not affiliated with,
endorsed by, or supported by the Samba Team or the Samba project**. The
Samba name is used solely to describe the packaged software. Samba
itself is © the Samba Team, licensed GPL-3.0-or-later; this build
repository is licensed Apache-2.0 (see `LICENSE`).

## Quickstart

*(Completed in Phase 5 — working copy-paste compose example: macvlan
network, `cap_drop: ALL` plus the CI-established capability set,
read-only rootfs, `*_FILE` secrets.)*

## Configuration reference

*(Completed in Phase 5 — exhaustive environment variable table from the
entrypoint contract in `docs/adaptation-profile.md`.)*

## Non-negotiable deployment constraints

See `docs/adaptation-profile.md` (authoritative): no NAT (macvlan/ipvlan
or host networking only), xattr+ACL-capable filesystem for
`/var/lib/samba` (NFS unsupported), host time discipline, file-based
secrets only.

## Volumes and backup

*(Completed in Phase 5 — volume list and pointers to the backup/restore
runbook in the deployment guide.)*

## Tags, pinning and support policy

Primary tags are `X.Y.Z-rN` (immutable); aliases `X.Y.Z`, `X.Y`, `X`.
`latest` is **not** production-usable. Production deployments should pin
by digest. Full policy: `docs/update-guide.md` *(completed in Phase 5)*.

## Verifying images

*(Completed in Phase 4 — `cosign verify` command with the expected
identity, plus SBOM/provenance inspection commands.)*

## Compatibility matrix

| Upstream branch | Maintained tags | Upstream support status |
|-----------------|-----------------|-------------------------|
| *(populated at first release; kept current by the watcher)* | | |

## Security

See `SECURITY.md`. This project conforms to the vendored publishing
specification (`SPEC.md`); conformance is asserted per image via the
`org.esitc-paris.spec-version` OCI label.
