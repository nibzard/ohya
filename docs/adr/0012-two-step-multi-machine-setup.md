# 0012: Two-step multi-machine setup

Status: accepted, 2026-11-16. Builds on
[ADR 0008](0008-integrated-setup.md) and
[ADR 0011](0011-tailcat-address-transport.md).

## Context

The first cross-machine setup (2026-11-15) took one hub service file, a
Tailscale certificate, a control URL, listen addresses, a code fix, and a
rebuilt binary on the device. The operator expected two machines and one
shared code. ADR 0011 removes the network requirements. This ADR defines
the steps that a person or a coding agent takes, and what each step reports.

A person needs few steps and clear failures. An agent needs commands that
run without prompts, JSON results (ADR 0005), and secrets that never appear
in its transcript unless the agent must handle them.

## Decision

### Two steps

1. On the hub machine, `ohya setup --hub` installs or keeps local ingestion,
   adds an owned `ohya serve` user service, creates the Tailcat identity,
   checks that the hub reaches a relay, and prints a join code with the
   device command.
2. On each other machine, the person pastes the device command. It installs
   ohya, reads the join code, pairs, and starts the uploader service and
   the remote MCP integrations.

`ohya devices invite` prints a new join code at any time. Setup reruns stay
idempotent, as in ADR 0008.

### Join code

- Format: `ohya1.<invitation>.<tailcat-address>`. The prefix names the
  format version and lets agents and humans recognize a code. The fields
  use characters that the separator `.` never appears in (hex and
  base64url).
- The invitation is single use and expires (default 15 minutes, `--ttl`).
  The Tailcat address does not expire. A leaked old code admits a
  handshake but no pairing and no data access.
- The previous `<invitation>:<preauth-key>` format is rejected with a
  message that names the new format.

### Device command

- For a person: `curl -fsSL <release>/install.sh | sh -s -- --join`. The
  installer reads the code from the terminal (`/dev/tty`) without echo, so
  the code stays out of shell history and the screen.
- For an agent or a script: the installer and `ohya setup` accept
  `--join-code-env NAME`, as today. The variable is removed from the
  environment before child processes start.
- With `--json`, no command prompts. A missing code is a usage error.

### Existing devices

A device whose join state already holds a credential sends a hello with that
credential to the address in the new code before it uses the invitation.

- When the hub accepts the hello, the device saves the new address and
  keeps its device identity, credential, and checkpoints. The invitation
  stays unused and expires.
- When the hub rejects the credential, setup fails and names the reason
  (revoked, or a different hub). It does not pair as a new device, because
  a new identity would upload the device's history again as new sources.

This is client behavior on top of the existing hello transition, so
`tla/Auth.tla` does not change. The migration from the ADR 0002 stack uses
this rule.

### What each step reports

- `ohya setup --hub` reports the relay region, the hub's public node key,
  the code expiry, and the device command. In JSON, the code is in one field
  marked secret.
- Device setup reports pairing, hub version and protocol, the path type
  (direct or relay), the round-trip time, and the upload backlog in bytes.
- `ohya doctor` on a device reports whether the hub answers, the path type,
  the relay region, the round-trip time, and the compatibility of versions.
  It reads the path from `tailcat.Client.DiscoPing`. It never invents a
  value that the library does not report.
- `ohya devices list` on the hub reports each device's last path type and
  its last sync time.
- While the first upload runs, the uploader reports bytes sent, bytes left,
  the current rate, and an estimated finish time. `ohya status` (T093)
  shows the same values when it exists.

### Failures

Each failure names the cause and one next step. The required cases:

| Cause | Message names |
| --- | --- |
| Expired or used invitation | `ohya devices invite` on the hub |
| Old code format | The `ohya1.` format and `ohya devices invite` |
| No relay reachable | Outbound HTTPS (TCP 443) |
| Hub not answering through the relay | Whether `ohya serve` runs on the hub |
| Protocol version mismatch | Both versions and the upgrade command |
| Credential rejected on rebind | Revocation or a different hub |

## Consequences

- A person runs one command on the hub and pastes one line on each device.
  An agent runs the same flow without prompts and reads JSON results.
- The installer gains terminal input. It must still work when no terminal
  exists, by requiring `--join-code-env`.
- Setup gains a hub mode with its own owned service file and receipt.
- The README section "Share memory across machines" shrinks to the two
  steps.
