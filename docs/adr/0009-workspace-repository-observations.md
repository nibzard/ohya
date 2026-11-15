# 0009: Device-observed workspace repositories

Status: accepted, 2026-11-15.

## Context

One project has a different working directory on each device. For example,
dioclet is `/home/agent/dioclet` on the hub and `/Users/nikola/dev/dioclet` on
a MacBook. An agent that filters sessions by its own `cwd` sees only the
sessions from its own device. On 2026-11-15, two agents asked the same hub for
the last dioclet session. Each got a different, incomplete answer. One agent
also guessed that the missing work was never recorded.

`sessions.repo` is the shared project key, but only Codex records carry a
repository URL. Claude Code and Pi records carry `cwd` and, for Claude Code,
`gitBranch`, but no remote. In the hub, 1,144 MacBook sessions and most hub
sessions had a NULL `repo`.

DB.md forbids git discovery at ingestion time. Derived rows must come from
stored inputs only, so `ohya rederive` can rebuild them without file access.
A parser that reads `.git/config` breaks that rule.

## Decision

A device observes the repository of each session working directory. The hub
stores the observation as a separate, durable input called a workspace
observation. Parsers stay pure.

- **Workspace:** one directory on one device where agent sessions ran,
  identified by `(device_id, path)`. The path is the session's `cwd`.
- **Workspace observation:** the normalized remote URL found for that
  directory, or no repository. The device that owns the directory writes it.
  It is write-once: the first observation wins, like `cwd` and `branch`.
- **Derivation:** `sessions.repo` is the first repository URL in the session's
  own records. If the records carry none, `repo` is the observation for
  `(device_id, cwd)`. Ingestion, a later observation, and `ohya rederive` all
  apply this one rule. All three read only stored data.

### Who observes

Only the hub parses records, so only the hub knows a device's session
directories. The device does not parse records to find them.

1. The device sends a `workspaces` frame. The frame holds its new
   observations, which can be none, for one agent.
2. The hub stores them in one transaction. Then it replies with up to 256
   pending paths. A pending path is the `cwd` of a session on that device and
   agent. The session has a NULL `repo`, and the path has no observation yet.
3. The device resolves each path and sends the results. The exchange repeats
   until no paths are pending, or until the only pending paths are ones that
   failed to resolve in this round.

The uploader runs this exchange after it connects, and after any pass that
inserted events. Local ingestion (`ohya ingest`) runs the same loop directly
against the database. `internal/workspace.Sync` holds the loop for both paths.

### How a device resolves a path

`internal/workspace.Resolve` reads files and runs no programs:

- A path that does not exist resolves to no repository. A deleted worktree
  stays without one.
- Starting at the path, the device walks up to the first `.git` entry. A
  `.git` file (a worktree or a submodule) names its git directory. That
  directory's `commondir` names the shared directory that holds `config`.
- From `config`, the device picks the `origin` remote. If there is no
  `origin`, it picks the only remote. If there are several remotes and none
  is `origin`, the result is no repository.
- The URL is normalized with the same rule as Codex: host and path, with the
  scheme, user information, and `.git` removed. The device sends only the
  normalized value, so a token in an `https://user:token@` URL never leaves
  the device. Local paths and `file://` URLs resolve to no repository.
- Any other read error, for example permission denied, is transient. The
  device sends no observation, so the path stays pending for a later pass.

The hub treats each observation as untrusted input. It accepts an observation
only for a path that is the `cwd` of one of that device's sessions. The
repository value must already be in normalized form. A device can observe
only its own workspaces, because the hub takes the device from the
authenticated connection, as it does for annotations.

## Consequences

- Agents can find one project's history across devices with
  `WHERE repo = 'github.com/nibzard/dioclet'`. The MCP guidance teaches this.
- Observations are write-once. A later remote rename or a directory reused for
  another project does not change an existing observation. To refresh it, an
  operator must delete the row and let it be observed again.
- A directory that is missing when observed, for example on an unmounted
  volume, records no repository for good.
- The minimal config reader ignores `include` directives and
  `url.<base>.insteadOf` rewrites.
- An older hub closes the connection when it gets the new frame, because the
  marker is unknown. The uploader then stops workspace exchanges for the rest
  of the process and reports the reason once. Uploads continue.
- `tla/Auth.tla` models the observation write: only a live, authorized session
  writes, the writer's device owns the observation, and an observation never
  changes after its first write.

## Rejected alternatives

- **Parse git config inside the parsers:** this breaks parser purity and
  re-derivation.
- **Inject synthetic records into the source byte stream:** a batch must be
  exactly the file's bytes between two cursors.
- **Have the device parse records to find `cwd`:** this doubles parsing and
  splits the knowledge of record formats between device and hub.
- **Match projects by the last path segment:** two unrelated projects with the
  same folder name would merge.
- **Run `git remote get-url`:** this needs git on every device, starts a
  process for each directory, and depends on the safe-directory rules.
