# 0008: Integrated installation and setup

Status: accepted design, implementation in progress, 2026-11-15.

## Context

Users need one pasted command that installs memory and connects their agents.
A downloaded executable alone does not meet that requirement. Setup must also
support coding agents without prompts or provider credentials.

## Decision

A shell bootstrap downloads and verifies a release executable. The executable
owns setup orchestration, user services, client adapters, health checks, repair,
and removal. The detailed contract is in `docs/installer-plan.md`.

Default setup uses local ingestion and user-scoped agent integrations. Remote
join is an explicit alternative. Existing agent policies remain authoritative.
Setup does not change model selection or disable permission checks.

Client configuration edits require ownership receipts. For JSON clients, setup
preserves unrelated values, rejects malformed objects and file symlinks, and
writes through the existing durable file helper. A receipt is prepared before
the client entry is changed. It records both the intended entry and the prior
owned value so an interrupted update can resume. An existing unowned entry is
a conflict. Removal preserves entries edited after setup.

Setup must serialize its own operations. Optimistic rereads detect external
edits before replacement; they cannot provide a transaction with an unrelated
client process. Configuration adapters must report conflicts and avoid whole
file rollback over later user edits. Private backups and receipts can contain
client configuration and must remain owner-only.

Success requires protocol verification, not only a successful file write.
Report partial completion explicitly and keep completed components repairable.
The setup receipt is not an ingestion checkpoint and never advances a Source
Cursor or supplies a hub Acknowledgment.

## Consequences

The application gains operational setup commands, but retains one binary.
Client-specific integration stays outside ingestion, synchronization, and SQL.
The bootstrap stays small. Native release testing and signing remain necessary
before a public installer can claim support for a platform.

No release is published by this decision.
