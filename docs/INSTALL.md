# Install ohya

This archive contains the ohya executable, its license, and these instructions.
Use only a release qualified for your operating system and architecture.

The verified installer configures memory, its user service, and detected agents.
For manual installation, put `ohya` in a user-owned executable directory and run
`ohya setup`. You do not need Go, a C compiler, or a separate SQLite install.

Run `ohya setup --dry-run --json` to inspect the planned changes. Use
`--agents claude,codex,pi` to select clients. Existing agent sessions may need
a reload. Run `ohya doctor` to inspect integration health.

To join an existing hub, set a one-time invitation in a private environment
variable and run `ohya setup --join-code-env OHYA_JOIN_CODE --control URL`.
Do not put the code in a command argument. Setup saves the connection for
later runs and keeps joined-device checkpoints separate from local memory.

Rerun setup to repair unchanged owned configuration or add a new agent.
Use `ohya uninstall` to remove owned services, agent entries, and PATH blocks.
It preserves memory, credentials, recovery points, and user-edited entries.
An unchanged standalone executable is removed unless `--keep-binary` is set.
Manual and package manager executables stay under their existing owner.
Executable recovery copies and lock files are retained.

Before an upgrade opens an existing local database, setup saves a consistent
SQLite recovery point. This database copy is not a complete backup of device
state and checkpoints. Stop all writers and restore matching state as a set.
Never combine an older database with newer acknowledgments. A binary downgrade
alone cannot undo a newer schema or parser output. See the release operations
guide before a writable restore.
