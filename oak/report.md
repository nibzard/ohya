# OAK memory versus no OAK for coding agents in Papercut

Research date: 2026-11-18. Study owner: the coordinating agent. All materials are
under `/tmp/papercut-oak-study` (protocol.md frozen before data collection,
`PROTOCOL_HASHES.txt` verified unchanged after the run).

## Question and design

Does the uncommitted OAK-0001 integration in `/home/agent/papercut` (a
`knowledge/` index, a setup record, and metadata pointers) improve how a coding
agent answers repository questions, compared with the pre-OAK state at commit
`6ca98a4`? The treatment is the working-tree difference alone. Both conditions
share Git history, prompts, model (`glm-5.3[1m]`), settings, tools, output
limits, and time budget. Each worker ran in a fresh disposable copy with no MCP
servers, no session persistence, and read-only tools (`Read`, `Glob`, `Grep`,
plus read-only Git commands). Design: 15 fixed tasks in 5 categories (history,
freshness, unresolved work, provenance, authorization) x 2 conditions x 4
repetitions, order fully randomized (seed 20261117), concurrency capped at 4.
Scoring used a frozen 58-element rubric audited against the control tree
before the run; the grader never sees condition labels.

## Primary result: no detectable accuracy difference

Intention-to-treat, 120 of 120 runs analyzed. No timeouts, no failed runs, no
tree modifications, no exclusions.

| Measure | OAK | Control | Difference (OAK - control) |
|---|---:|---:|---|
| Accuracy score, mean (SD) | 0.918 (0.158) | 0.915 (0.145) | +0.0031 |
| 95% CI, paired t | | | -0.040 to +0.046 |
| 95% CI, bootstrap | | | -0.030 to +0.046 |
| Hodges-Lehmann (95% CI) | | | 0.000 (-0.031 to +0.021) |
| Exact Wilcoxon signed-rank p | | | 0.94 |
| Sign-flip permutation p | | | 0.94 |
| Standardized effect dz | | | 0.04 |

The primary hypothesis is not supported. The interval excludes the
pre-declared minimum important difference of 0.15 in both directions, so the
data are compatible only with small accuracy effects. Scores sat near ceiling:
44 of 60 OAK runs and 42 of 60 control runs scored a perfect 1.0, and 10 of 15
tasks had identical task means.

## Efficiency (secondary, Holm-corrected: none significant)

| Measure | OAK | Control | Relative change | dz | Permutation p |
|---|---:|---:|---:|---:|---:|
| Model turns | 6.5 | 8.3 | -21% | -0.45 | 0.107 |
| Tool calls | 5.5 | 7.3 | -24% | -0.45 | 0.107 |
| Output tokens | 1,113 | 1,299 | -14% | -0.17 | 0.532 |
| Wall seconds | 23.5 | 28.1 | -16% | -0.26 | 0.361 |
| Permission denials per run | 0.23 | 0.53 | -56% | | |

All four efficiency measures point the same way: with the knowledge index,
workers needed fewer turns, fewer tool calls, fewer output tokens, and less
time. The pattern is consistent but not significant after correction, so it
stays a hypothesis for a larger study. Input tokens were almost identical
(11,507 in both conditions). Cache-read tokens differed (OAK 31,756, control
48,399) because control workers re-sent more context across more turns;
following the protocol, input-token totals are not treated as a comparison
endpoint.

## Where the rubric separated the conditions

Per-task mean differences: U1 +0.25 (OAK better), H2 -0.10, P1 -0.083,
A2 -0.063 (control better), A3/F1 +0.04 (mixed), 10 tasks exactly 0.

- The single clean memory-served gain was element `u1_noauth` ("the proposals
  carry no completion or authorization evidence"): control missed it in 4 of 4
  runs, OAK in 0 of 4. The control workers found the MeasureTwice and SDK
  pilot proposals but did not state their unstarted status.
- The H2 difference traces to a rubric artifact, not knowledge. The
  `h2_open` regex accepts phrases like "all were fixed" or "none remain".
  OAK runs more often wrote "none - all findings fixed" or "no finding remains
  open", which the regex misses although the answers are correct. Spot checks
  of the three scoring OAK runs confirmed substantively correct answers.
  Grading stayed frozen; the artifact inflates noise and slightly disfavors
  OAK on H2.
- The memory-served subset scored +0.013 (dz 0.11) and the memory-neutral
  subset -0.005 (dz -0.15). Neither is significant (permutation p = 1.0).
- Hard elements missed in both conditions: `a3_match` (tag and crate version
  match rule), `a2_shadow` (shadow-mode label), `f3_skipped` (version 3
  skipped), `a1_plan` (update the plan first).

## Interpretation

For a strong model on a small, well-documented repository, an OAK knowledge
index did not change answer accuracy: the model reaches correct answers from
the raw repository either way. The index did change the path: fewer turns,
fewer tool calls, and fewer denied permission attempts, with the biggest
accuracy contribution on exactly one kind of question, the completion and
authorization status of proposed work. If OAK has practical value here, it is
efficiency and status synthesis, not correctness. That efficiency signal
(20% fewer turns and tool calls) did not reach significance in this design and
needs a larger sample to confirm.

## Limitations

- Ceiling effect: mean accuracy above 0.91 left little room to improve. A
  harder task set, a weaker model, or a larger repository could show more.
- One repository, one model, one integration state, 15 tasks. Results do not
  generalize beyond this setting.
- The treatment tree is dirty (uncommitted changes); workers could see that.
  It is part of the tested integration but differs from control.
- Rubric regexes are phrasing-sensitive; the H2 artifact shows false negatives
  that add noise (and slightly penalized OAK this time).
- The ohya session hub and axinbox MCP were isolated away from both
  conditions by design. The study tests the in-repo knowledge base alone, not
  session-recall memory.
- Coordinator session data existed during the experiment, but no worker could
  reach it (no MCP, unique working directories, no network).
- Power was adequate only for large effects at the assumed noise; the
  observed ceiling makes the effective power for small differences low.

## Reproduction

```
cd /tmp/papercut-oak-study
sha256sum -c PROTOCOL_HASHES.txt        # verify frozen inputs
python3 scripts/audit_rubric.py         # 58/58 ground-truth checks
python3 scripts/power.py                # power table -> results/power.json
python3 scripts/runner.py build         # 120 trees + randomized manifest
python3 scripts/runner.py run           # execute workers (about 20 minutes)
python3 scripts/grader.py               # blind, deterministic scoring
python3 scripts/analyze.py              # stats -> results/analysis.json
```

Raw data: `runs/<run_id>/` holds `stream.jsonl` (full event stream),
`meta.json` (command, timing, tree state), and `extracted.json` (answer,
turns, tool calls, tokens). Scores: `results/scores.csv` and
`results/element_detail.csv`. Frozen inputs: `protocol.md`, `tasks.json`,
`rubric.json`, `scripts/`.
