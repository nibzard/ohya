# Implementation plans

| Plan | Priority | Dependencies | Status |
| --- | --- | --- | --- |
| [001: Runtime observability](001-runtime-observability.md) | P1 | None; six ordered milestones inside the spec | PROPOSED |
| [002: Usage and friction instrumentation](002-usage-instrumentation.md) | P1 | 001 milestones 1–3 | PROPOSED |
| [003: Recall tool improvements](003-recall-tool-improvements.md) | P2 | 002 baseline (R1 is independent) | PROPOSED |
| [004: Storage efficiency](004-storage-efficiency.md) | P2 cheap items; rest deferred | S1–S3 none; D1–D3 need the 002 acceptance gate | PROPOSED |

Plan 001 was written against `33df75d` on 2026-11-15. Implement local collection before remote aggregates; measure overhead before optimizing product behavior. No source implementation is included.

Order of work (2026-11-16): instrumentation first (001 milestones 1–4 and 6, then 002), then cheap storage items (004 S1–S3) in parallel, then tool improvements (003) guided by usage data. Larger storage changes (004 D1–D3) wait for the 002 acceptance gate. Plan 002 keeps a field classification so anonymized, opt-in telemetry can be added later; no exporter is planned now.
