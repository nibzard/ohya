# 0017: Native macOS menu bar companion app

Status: proposed, 2026-11-21.

## Context

ohya runs silently in the background under `launchd` (`com.nibzard.ohya` and,
on a hub, `com.nibzard.ohya.serve`). That invisibility is right for capture,
but it leaves macOS users with three recurring daily frictions:

1. **Silent uncertainty.** Users cannot tell at a glance whether capture is
   running, still draining an initial history import or relay upload, or
   degraded by a stopped source, stale process, or revoked credential.
2. **Terminal ceremony for routine actions.** Pausing capture, issuing a
   15-minute device invitation, revoking a device, exporting a diagnostics
   bundle, or reviewing agent-marked garbage (`tags.garbage`,
   [ADR 0016](0016-agent-marked-purge-scope.md)) all require opening a shell
   and remembering flags.
3. **Human recall and handoff friction.** Moving from one coding agent to
   another ("pick up where Claude Code left off in Codex") is ohya's primary
   workflow, yet finding a recent session ID or searching memory before opening
   a new agent session still starts in the terminal.

At the same time, [MANIFESTO.md](../../MANIFESTO.md) commits ohya to a single
authoritative SQLite writer, clear module boundaries, minimal dependencies,
and zero speculative infrastructure. Adding a web server, an Electron/WebView
runtime, or a second database client would violate those commitments.

## Decision

**Ship an optional, thin native macOS menu bar companion app (`ohya.app`)
written in Swift and SwiftUI (`MenuBarExtra`) with zero third-party
dependencies, communicating exclusively through the `ohya` executable's
versioned JSON CLI ([ADR 0005](0005-json-output.md)) and its published
`observe/` snapshot files.**

### Architectural boundaries

1. **CLI as the sole control and data plane.** The app never imports SQLite,
   never opens `hub.db` or `registry.db`, and never reads `credential.json` or
   `sources/*.json` directly. Every read and state mutation runs through
   bounded invocations of `ohya ... --json`. That guarantees one SQL
   authorizer, one egress secret-redaction pass
   ([ADR 0015](0015-egress-secret-redaction.md)), one service-manager adapter,
   and identical behavior in local and joined-device modes.
2. **OS service manager keeps daemon ownership.** `launchd` continues to own
   `ohya ingest`, `ohya join`, and `ohya serve`
   ([ADR 0008](0008-integrated-setup.md)). Quitting or crashing the menu bar
   app never interrupts capture or serving; launching the app never spawns an
   unmanaged background daemon.
3. **Zero-touch idle observation.** While the popover is closed, the app
   watches the resolved state directory's `observe/` tree (falling back to a
   gentle poll) and invokes `ohya status --json` on snapshot change or popover
   open. It never wakes the hub or scans SQLite in the background.
4. **Pure Apple SDK.** Target macOS 26 (Tahoe) Liquid Glass natively using
   SwiftUI (`MenuBarExtra(.window)`, `@Observable`, `GlassEffectContainer`,
   `.glassEffect()`, concentric geometry) with clean degradation to macOS 15
   (Sequoia) via `@available(macOS 26, *)`. Use `SMAppService.mainApp` for
   Launch at Login and `UNUserNotificationCenter` for opt-in failure alerts.
   Zero external Swift packages.
5. **No AI or classification in the UI.** Reasoning stays in the coding
   agents. The menu bar app shows facts, redacted excerpts, and one-click
   handoff prompts; it never calls an LLM or embeds a local model.

### Small CLI and brand prerequisites

To keep the app genuinely thin and DRY, four small additions land in `ohya`
itself before UI milestones consume them (each is independently useful to CLI
and script users):

- **Monochrome template mark:** `scripts/brand-logo.py` gains a
  `menubar-template.svg` target (monochrome elephant mark without the purple
  tile, heavier stroke tuned for 18 pt over macOS Tahoe's transparent menu bar)
  rendered with `isTemplate = true` so macOS adapts ink automatically across
  light, dark, and wallpaper-tinted bars ([BRAND.md](../../BRAND.md)).
- **`ohya service status|start|stop|restart --json`:** thin CLI wrapper over
  the owned service plans already in `internal/setup/service.go`, so the app
  never duplicates `launchctl` arguments or receipt checks.
- **`ohya search --json`, `ohya show --json`, and `ohya sessions --json`:**
  extend `cmd/ohya/recall.go` under the ADR 0005 JSON envelope so the Recall
  surface reads structured, egress-redacted results without parsing prose.
- **Remote aggregates (`ohya status --remote --json`):** already scoped in
  `plans/001-runtime-observability.md` Milestone 5; once shipped, a joined
  laptop displays hub health and storage honestly alongside local upload state.

### Security and privacy invariants

- **No secret persistence or logging.** One-time join codes from
  `ohya devices invite --json` stay in memory only until expiry, are never
  written to disk or `os.Logger`, are copied to `NSPasteboard` tagged with
  `org.nspasteboard.TransientType` and `org.nspasteboard.ConcealedType` so
  clipboard managers ignore them, and are cleared from the pasteboard after
  60 seconds if the clipboard's `changeCount` has not changed.
- **Egress redaction stays intact.** All search and session previews flow
  through `ohya search --json` and `ohya show --json`, which execute inside
  `internal/mcpserver`'s read-only engine and apply the ADR 0015 redaction
  table before bytes leave `ohya`.
- **Owner-decided destructive actions only.** Revoking a device and applying an
  agent-marked purge (`ohya purge --marked tags.garbage --apply`) require an
  explicit two-step preview and confirmation stating that revocation is final
  and purge tombstones the evicted sources.
- **Environment scrubbing.** Every child `ohya` process runs with an explicit
  bounded timeout, neutral working directory, and no inherited secret
  variables.

### Packaging and binary resolution

- **Initial companion mode:** `ohya.app` resolves the installed `ohya` binary
  (user override → `~/.local/bin/ohya` → `/opt/homebrew/bin/ohya` →
  `/usr/local/bin/ohya`) and checks `ohya version --json` on startup, reporting
  missing or incompatible CLI builds with an actionable fix instead of failing
  silently.
- **Bundled distribution mode (release milestone):** `ohya.app` bundles the
  matching universal or architecture-matched `ohya` executable inside
  `Contents/MacOS/ohya`, signed and notarized with Apple Developer ID and
  distributed as a DMG and Homebrew Cask alongside the existing CLI archive.

### Model checking

None required. The menu bar app introduces no sync, cursor, acknowledgment, or
authorization protocol transition; every mutation delegates to existing CLI
commands governed by `tla/Sync.tla` and `tla/Auth.tla`.

## Rejected alternatives

- **Go-only `ohya menubar` via cgo `NSStatusBar`/`NSMenu`:** keeps a single
  executable, but restricts the UI to flat `NSMenu` items (no interactive
  search, session preview, or clean copy feedback) and pulls macOS AppKit main
  thread lifecycle concerns into the CLI binary.
- **Localhost web UI + minimal tray:** adds an HTTP server, loopback
  authentication/CSRF boundary, and frontend toolchain for capabilities SwiftUI
  provides natively with lower memory and zero network surface.
- **Electron, Tauri, or Wails:** unacceptable memory footprint, WebView
  non-native feel under macOS Liquid Glass, and large third-party dependency
  surface relative to [MANIFESTO.md](../../MANIFESTO.md).
- **Direct SQLite access from Swift:** would duplicate schema knowledge, bypass
  the SQL authorizer and ADR 0015 egress redaction, and fail on joined devices
  whose database lives on the hub.

## Consequences

- ohya gains a native, minimalist macOS companion without compromising the core
  Go binary's architecture, single-writer SQLite discipline, or Linux server
  footprint.
- The Go CLI becomes cleaner and more scriptable for every user as a byproduct
  (`ohya service`, `ohya search/show/sessions --json`).
- The repository gains an isolated `macos/` SwiftPM target that builds and
  tests only on macOS; Linux CI continues to run `make check` unchanged, while
  macOS CI builds and tests the Swift package and its CLI contract decoding.
- Public distribution of `ohya.app` shares the existing Apple Developer ID
  signing and notarization gate ([README.md](../../README.md)).
