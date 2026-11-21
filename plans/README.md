# Implementation plans

| Plan | Priority | Dependencies | Status |
| --- | --- | --- | --- |
| [001: Runtime observability](001-runtime-observability.md) | P1 | None; six ordered milestones inside the spec | IMPLEMENTED — milestones 1–6 done (including `ohya status --remote` and `Auth.tla` `ReadDiagnostics`) |
| [002: Usage and friction instrumentation](002-usage-instrumentation.md) | P1 | 001 milestones 1–3 | IN DELIVERY — usage record, `ohya usage`, ADR 0010, remote client record done (T092, T094, T095, T108); baseline (T099) open |
| [003: Recall tool improvements](003-recall-tool-improvements.md) | P2 | 002 baseline (R1 is independent) | IN DELIVERY — R1, R2, R5 done (T103, T104, T107); R3 (T105) and R4 (T106) open |
| [004: Storage efficiency](004-storage-efficiency.md) | P2 cheap items; rest deferred | S1–S3 none; D1–D3 need the 002 acceptance gate | IN DELIVERY — S1–S3 done (T100–T102); D1–D3 (T110–T112) open, deferred |
| [005: macOS menu bar app](005-macos-menubar.md) | P2 | M1/M5.1 none; M5.2 needs Apple Developer ID signing on macOS | IN DELIVERY — M1 and remote-hub data plane done; M2–M4 SwiftPM app implemented under `macos/OhyaMenuBar/` (macOS compile/signing deferred) |

Plan 001 was written against `33df75d` on 2026-11-15. Implement local collection before remote aggregates; measure overhead before optimizing product behavior. Its milestones 1–4 shipped on 2026-11-16 and 2026-11-17.

Order of work (2026-11-16): instrumentation first (001 milestones 1–4 and 6, then 002), then cheap storage items (004 S1–S3) in parallel, then tool improvements (003) guided by usage data. Larger storage changes (004 D1–D3) wait for the 002 acceptance gate. Plan 002 keeps a field classification so anonymized, opt-in telemetry can be added later; no exporter is planned now.
