# Recall evaluation set

A small deterministic recall evaluation set (plan 003 R5, T107): synthetic
sessions with planted answers, plus recall questions whose known answers are
identified by unique markers. `internal/recalleval` ingests these files
through the production parse and write path, serves the questions through the
`sql` tool engine, and checks that every known answer event still ranks in the
top 10. The tests run as part of `make check`, so a change to the tool, the
FTS index, or storage that lowers recall quality fails the check.

Everything here is synthetic and hand-authored (2026-11-17). It contains no
real transcripts. Record shapes follow the verified formats documented in
`testdata/claude-code/`, `testdata/codex/`, and `testdata/pi/`. Byte offsets
and framing are not under test in this directory, so records are not listed
by byte range.

## Files

| File | Agent | Content |
| --- | --- | --- |
| `claude-main.jsonl` | claude-code | Uploader retry decision, a failed test tool result plus its diagnosis, a stale bound and its correction, decoy turns. |
| `claude-subagent.jsonl` | claude-code | Sidechain of the same session holding the invitation TTL finding. |
| `codex-session.jsonl` | codex | Registry lock error observed only in a `function_call_output`, and the `_txlock=immediate` decision. Derives `sessions.repo` = `example.com/agent/server`. |
| `pi-session.jsonl` | pi | Usage-key decision in an assistant message. |

## Markers

Each known answer carries one unique marker token (`oyr-41a1` and so on)
appended to its text. Markers never appear in a question's MATCH terms, so a
question cannot pass by matching its own marker. The harness resolves a marker
to an event ID with `SELECT id FROM events WHERE text LIKE '%<marker>%'` and
fails unless exactly one event matches, so a marker duplicated by a fixture
edit is caught immediately. Event IDs are not written down anywhere: they are
deterministic for a given corpus revision (pinned by
`TestRecallDeterministic`), and resolving them at run time keeps the set
editable without repinning numbers.

## Questions

| Key | Question | MATCH | Scope | Answer marker |
| --- | --- | --- | --- | --- |
| `uploader-backoff` | What retry backoff cap was decided for the device uploader? | `retry AND backoff AND cap` | | `oyr-41a1` |
| `cursor-diagnosis` | Have we seen the upload cursor regression failure before, and what was the root cause? | `cursor AND acknowledged AND commit` | | `oyr-52b2` |
| `batch-bound` | What is the ingestion batch byte bound after the correction? | `ingestion AND batch AND bound` | | `oyr-63c3` |
| `invitation-ttl` | How long is a pairing invitation valid, and is it single-use? | `invitation AND ttl` | | `oyr-74d4` |
| `registry-locked` | Have we seen SQLITE_BUSY on the registry, and under what conditions? | `database AND locked AND invitation` | | `oyr-85e5` |
| `registry-txlock` | What was decided about registry locking under concurrent joins? | `registry AND txlock` | | `oyr-96f6` |
| `server-repo-locked` | In the server repository, which invitation consumption was reported locked? | `invitation AND locked` | `sessions.repo` | `oyr-85e5` |
| `uploader-test-failure` | Which uploader test failed before the fix, and what did it report? | `fail AND regressed` | | `oyr-b8c8` |
| `pi-usage-key` | How does the Pi parser key token usage? | `responseid AND fallback` | | `oyr-a7b7` |

The questions mirror real recall shapes (README "What agents can ask"): a
decision, an error with its diagnosis, a later correction that must beat a
stale statement, a subagent finding, facts that exist only in tool outputs
(a Codex `function_call_output` and a Claude Code `tool_result`), and a
repository-scoped query. Several questions match more than the answer (the
failed test, the stale bound, the related decision), so BM25 ordering is
exercised, not just term presence. `server-repo-locked` also guards the
`sessions.repo` derivation, because a broken derivation empties the scoped
result.

## Measuring an index or storage change

Baseline recall is 9/9 at top 10. To measure a proposed change, build a hub
with `recalleval.Ingest`, apply the change, and compare `recalleval.Run`
before and after.

`TestRecallMeasuresUnindexedToolOutputs` demonstrates the recipe for plan 004
O6 (index fewer bytes): it removes the tool-output rows of both agents from
`events_fts` (Codex `function_call_output`: `kind = 'response_item'` with
`tool_name` NULL and a `callId` in metadata; Claude Code `tool_result`:
`kind = 'user'` with a `tool_result` first block) and asserts recall drops
from 9/9 to 6/9 — exactly the three questions whose answers live only in
tool outputs — while every other question still hits. An experiment that
excludes a different kind identifies its rows the same way and reports which
questions the exclusion costs.
