# Samba release key — out-of-band pinning evidence (SPEC.md §4.4)

- Pinned: 2026-08-16
- Key file: `samba-release-key.asc`
- Primary key fingerprint: `81F5E2832BD2545A1897B713AA99442FB680B620`
  (RSA 4096, created 2020-12-21, expires 2027-12-12; encryption subkey
  `8ECB3E1AD3C66464249027DE97EF9386FBFD4002`)
- User ID(s): `Samba Distribution Verification Key <samba-bugs@samba.org>`

The vendored file is the upstream key block byte-for-byte as served
(SHA-256
`4a2e1d9c3b8844f9172a2d4611c77b0e48d068d101464c9583b983f4ee5d9473`,
10638 bytes). Despite the `.asc` name, upstream serves this file in
**binary** OpenPGP form, not ASCII armor — `gpg --import` accepts it as
is, and it must NOT be passed through `gpg --dearmor`. Git therefore
stores it as a binary blob, so a future rotation PR is reviewed by
re-running the two-source fingerprint procedure below, not by reading
the diff.

The file carries a second, older public key that upstream ships in the same
bundle: `9147A339719518EE9011BCB54793916113084025` — `Samba Library
Distribution Key <samba-bugs@samba.org>` (RSA 2048, created 2011-01-12,
expires 2027-12-13, encryption subkey
`541FB94987D491A3C201D8B1B1DD08BF65F64DAB`). Release tarballs are signed
by the primary key above; the library key is not used for release
verification here.

## Independent sources (fingerprints matched)

1. `https://download.samba.org/pub/samba/samba-pubkey.asc` (samba.org
   download server, TLS) — fetched 2026-08-16.
2. `https://salsa.debian.org/samba-team/samba/-/raw/master/debian/upstream/signing-key.asc`
   (Debian samba packaging, salsa.debian.org — independent
   infrastructure and maintainership) — fetched 2026-08-16.

Comparison result: both sources carry
`81F5E2832BD2545A1897B713AA99442FB680B620` and its subkey
`8ECB3E1AD3C66464249027DE97EF9386FBFD4002` with the identical user ID.
Source 1 is a superset — it additionally ships the legacy library
distribution key `9147A339719518EE9011BCB54793916113084025`. No
fingerprint present in source 2 is absent from source 1, and no
fingerprint conflicts between the two.

## Functional verification

`samba-4.24.6.tar.asc` verified against this key over the uncompressed
tarball on 2026-08-16: Good signature, fingerprint matched.

```text
gpg: Signature made Thu Aug 13 16:28:24 2026 CEST
gpg:                using RSA key 81F5E2832BD2545A1897B713AA99442FB680B620
gpg: Good signature from "Samba Distribution Verification Key <samba-bugs@samba.org>" [unknown]
      81F5E2832BD2545A1897B713AA99442FB680B620
```

Samba signs the **uncompressed** tarball: `samba-<ver>.tar.asc` is a
detached signature over `samba-<ver>.tar`, not over the shipped
`samba-<ver>.tar.gz`. The Phase 1 Dockerfile builder stage must
therefore `gunzip` before `gpg --verify`.

## Rotation policy

Key rotation happens ONLY via a reviewed pull request that references
the upstream rotation announcement and repeats this two-source procedure
(SPEC.md §4.4). The watcher never rotates keys.

Note the pinned key expires 2027-12-12; renewal of the expiry date is
itself a key change and goes through the same reviewed procedure.
