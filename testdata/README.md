# Synthetic fixtures

Put small, hand-authored session fixtures here as parsers are added. Never commit
real transcripts, credentials, or private data. Preserve framing and offsets in
fixtures where those details are under test.

Use one directory per agent, named after the agent (`claude-code/`, `codex/`,
`pi/`). Give each directory a README that documents the verified format and
lists each fixture with the byte range of every record, so tests can assert
exact offsets.

- [claude-code/](claude-code/README.md) — verified against Claude Code 2.1.280.
- [codex/](codex/README.md) — verified against codex-cli 0.166.0.
- [pi/](pi/README.md) — verified against the Pi agent 0.84.2 and 1.5.2.
