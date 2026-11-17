# Install ohya

This archive contains the ohya executable, its license, and these instructions.
Use only a release qualified for your operating system and architecture.

The verified installer configures memory, its user service, and detected agents.
For manual installation, put `ohya` in a user-owned executable directory and run
`ohya setup`. You do not need Go, a C compiler, or a separate SQLite install.

Run `ohya setup --dry-run --json` to inspect the planned changes. Use
`--agents claude,codex,pi` to select clients. Existing agent sessions may need
a reload. Run `ohya doctor` to inspect integration health.

To join an existing hub, run the installer with `--join`:

```sh
curl -fsSL <release>/install.sh | sh -s -- --join
```

The installer reads the join code from the terminal (`/dev/tty`) without
echo, so the code stays out of shell history and the screen. The code comes
from `ohya setup --hub` or `ohya devices invite` on the hub and carries the
hub's address; treat it as a secret and never put it in a command argument.
Without a terminal — and always under `--json`, which prompts for nothing —
the code must arrive through a variable: pass `--join-code-env NAME` with
the code already in that environment variable. Setup consumes the variable
before child processes start, saves the connection for later runs, and
keeps joined-device checkpoints separate from local memory.

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
