# Ordered backlog

These are dependencies and acceptance criteria, not a promise of parallel work.

1. **Local Claude Code vertical slice.** Depends on bootstrap. Verify current
   source format; add synthetic fixtures and parser/fuzz tests. Persist source
   identity, complete records and cursor transactionally; preserve raw records,
   handle partial lines and file replacement, and test crash/retry behavior.
   Provide a tested migration runner and durable SQLite configuration.
2. **Local MCP.** Depends on the local slice. Implement read-only `sql(query)`
   with connection-level authorizer, single statement, deadline and output limits;
   implement bounded atomic `annotate(operations)` with hub-assigned writer and
   ownership. Test adversarial SQL, limits, isolation, and unchanged source data.
3. **Additional sources.** Depends on the local slice. Codex and Pi formats are
   verified against installed versions (2026-11-13, see NOTES.md); add synthetic
   fixtures, deterministic parsers and recovery/fuzz tests before enabling each
   source.
4. **Sync and authorization.** Depends on the local slice and a Tailcat feasibility
   spike. Model-check cursor/acknowledgment and authorization/revocation invariants
   with stated failure assumptions before implementation. Test durable commit before
   acknowledgment, replay, conflicts, offline recovery and revocation.
5. **Distribution and operations.** Depends on working local use. Verify native
   macOS builds for supported architectures, signing/notarization, install,
   upgrades, backup and recovery; document measured limits and operational steps.
6. **OpenCode spike.** Depends on evidence of demand; OpenCode v2 stores sessions
   in SQLite (`opencode.db`), so a byte cursor does not apply. Design a
   poll-based source cursor before committing to implementation.
