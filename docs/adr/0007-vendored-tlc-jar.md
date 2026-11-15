# 0007: Vendor the TLC jar in the repository

Status: accepted, 2026-11-15.

## Context

`make tla` and the macOS CI check job need the TLA+ tools jar. The runner
(`tla/run-tlc.sh`) downloaded `tla2tools.jar` from the GitHub release-tag URL
`https://github.com/tlaplus/tlaplus/releases/download/v1.8.0/tla2tools.jar`
whenever `tla/.tools` lacked the jar, verifying a pinned SHA-256. GitHub
release-tag URLs are mutable: upstream can delete and re-upload the asset under
the same tag.

That risk materialized on 2026-11-15. Upstream replaced the v1.8.0 asset —
identical file listing and builder metadata, only the embedded Build-TimeStamp
differs — so the old checksum failed every fresh download and the pin had to be
repaired by hand. The CI check job runs `make tla` on every push on a fresh
runner with no jar cache, so each upstream rebuild breaks CI until a manual
repin (NOTES.md, 2026-11-15).

## Decision

Vendor the jar in the repository at `tla/vendor/tla2tools-<version>.jar`.
`run-tlc.sh` never downloads: it verifies the vendored jar's SHA-256 against
the pin in the script on every run and fails with a `git checkout` remedy on
mismatch or absence. A checkout can no longer break because upstream rebuilt a
tag; `make tla` needs only Java.

Checksum verification stays mandatory. It no longer guards a download against
upstream drift — the bytes are in git — but it guards the vendored file
against corruption and accidental edits, and it makes every upgrade an
explicit, reviewable repin.

## Alternatives rejected

- **Anchor the download to an immutable artifact.** No immutable host carries
  tla2tools. Maven Central has no `org.lamport` or `org.tlaplus` `tla2tools`
  artifact at all (verified 404 on both coordinates, 2026-11-15). GitHub
  asset-ID URLs (`/releases/assets/<id>`) do serve one fixed upload, but a tag
  rebuild deletes the old asset, so the pinned URL would then 404 and the run
  would still break — that converts silent replacement into unavailability
  without fixing the outage.
- **Record a decision against hardening.** Leaves CI one upstream rebuild away
  from a manual repin. The incident already happened once, and the check job
  runs on every push, so the exposure is continuous.

## Consequences

- The repository carries one ~4.3 MiB jar per pinned TLC version; history
  grows only when the pin moves. No Git LFS: the jar is small enough to store
  as a plain blob.
- Upgrading TLC: download the new release jar out of band, place it at
  `tla/vendor/tla2tools-<version>.jar`, update `TLA_VERSION` and `TLA_SHA256`
  in `tla/run-tlc.sh`, delete the superseded jar, and commit jar and pin
  together. The checksum gate rejects a forgotten pin update.
- `curl` and network access leave the runner's requirements; Java 11 or later
  remains.
- Upstream provenance of the vendored 1.8.0 jar: the 2026-11-15 rebuild of the
  v1.8.0 tag, SHA-256
  `7e53aa636dac8a3cb0533f343115c0548e03fc0ce5edc7739becf58d3d0fb84f`, matched
  by a fresh download on the vendoring date and followed by a green run of
  every model.
