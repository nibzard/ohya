# 0018: Device-observed source workspace

Status: accepted, 2026-11-24.

## Context

Antigravity transcripts carry no working directory. A parser never reads the
filesystem ([ADR 0009](0009-workspace-repository-observations.md)): the
device observes, the hub stores. So every Antigravity session on the
deployed hub has no `cwd` and no `repo` (73 of 73 on 2026-11-24). An agent
cannot scope Antigravity history by repository, and `sessions.repo` is the
project key that cross-device recall filters on.

Antigravity keeps its own index next to the brain root:
`conversation_summaries.db`. Its table `conversation_summaries` has
`conversation_id` (the primary key) and `workspace_uris` (a JSON list). The
conversation ID is the name of the conversation directory, which
`antigravity.ConversationIDFromPath` derives from a source path. On the
device of the hub, 68 of 68 rows hold one `file:///path` entry. The file is
not documented.

## Decision

**The device reports the workspace of a source, and the hub stores it as
part of the source.** One rule: the `cwd` of a session is the first
non-empty record `cwd`, else the workspace of a source that holds the events
of the session. The parser stays file-free and its output version stays 1,
so no re-derivation is forced.

### Storage

- Schema 9
  ([migration 009](../../internal/store/migrations/009_source_workspace.sql))
  adds `workspace_path` to `sources` and to `purged_sources`.
- `store.SetSourceWorkspace` is the only write. It requires a registered
  source of the calling device. The path must pass the checks of a workspace
  observation: at most 4,096 bytes, valid text, and no NUL byte.
- The write is write-once. The first path stays. The same path again is a
  no-op, and a different path is ignored.
- The write fills `cwd`, and the repository of a known `workspaces` row, for
  the sessions of the source that have no `cwd`. Ingestion and re-derivation
  apply the rule again for later events.
- A purge keeps `workspace_path` in the tombstone, and reallow restores it,
  as they do with `first_native_id`
  ([ADR 0013](0013-owner-scoped-session-purge.md)). So write-once has no
  exception.

### Authorization

`tla/Auth.tla` models the write with the action `SetSourceWorkspace`. It
requires an authorized session, a registered source, and
`srcOwner[s] = sesDevice[c]`. The action never binds ownership, because
registration already did. The invariants
`SourceWorkspaceWriterFromSession` and `SourceWorkspaceHasWriter` and the
property `SourceWorkspaceWriteOnce` state the rule.

### Transport

A new device-to-hub frame `sourceWorkspace` (marker `F`) carries the device,
the source, and the path. The hub answers with `ack`. Capability bit 2
(`CapSourceWorkspace`) tells a device that the hub accepts the frame. An old
hub does not advertise the bit, so a new device sends no frame
([sync-protocol](../sync-protocol.md)). A joined device looks the workspace
of a source up once per connection. A source that the hub acknowledged is
not sent again in that process, and a source without a workspace is looked
up again on each new connection, never on each pass. A hub error for the
frame never blocks the batches of the source.

### Who looks the workspace up

Only a device can read its own Antigravity index, so the lookup lives on
the device side:

- `internal/uploader` takes a lookup function,
  `Options.SourceWorkspace`. The package knows nothing about Antigravity.
- `cmd/ohya` supplies the lookup for the Antigravity root. It opens
  `conversation_summaries.db` with `immutable=1`, reads `workspace_uris` of
  the conversation, and decodes the first `file://` entry with `net/url`, so
  `%20` becomes a space.
- Local ingestion backfills the sources of the hub's own device once per
  pass. It lists `store.SourcesWithoutWorkspace`, looks each source up, and
  calls `store.SetSourceWorkspace`. It retries a source without a result only
  in a pass that inserted events of that source.
- A joined device sends the frame when the hub confirms the source, and
  always before the next batch: after the registration acknowledgment of a
  new source, or after the cursor probe of a source that the hub knows.

## Consequences

- An Antigravity session gets a `cwd`, and a `repo` when its directory still
  has a usable repository.
- The first workspace of a source is final. A later change in the index is
  ignored.
- The index file is not documented. When Antigravity changes it, sessions
  stay without a `cwd`, as before. The lookup fails softly: a missing file, a
  changed schema, a malformed list, or a missing row is "not found". The
  first failure to read the index is reported once per process.
- `immutable=1` creates no `-wal` or `-shm` file and never blocks
  Antigravity. It reads the main file only and not the write-ahead log, so it
  can miss rows that Antigravity has not checkpointed yet. A source without
  a row is looked up again when a later pass inserts its events, and on each
  new connection of a joined device.
- The upgrade order is hub first. Schema 9 forces one full recovery copy of
  the hub database at the next `ohya setup` after the upgrade.
- A directory that is gone gets a `cwd` and no `repo`. That is correct: the
  device cannot read a repository there.

## Rejected alternatives

- **Read the index inside the parser:** this breaks parser purity, and
  [ADR 0009](0009-workspace-repository-observations.md) keeps parsers free of
  the filesystem.
- **Force a re-derivation with a new parser output version:** the
  re-derivation reads every stored record of the hub. The backfill reaches the
  same sessions from one stored value per source.
- **Look the workspace up at the hub for a joined device:** the hub cannot
  read the files of another device, and a lookup in the hub's store would put
  Antigravity knowledge outside `cmd/ohya`.
