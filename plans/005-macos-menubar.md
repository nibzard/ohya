# Native macOS menu bar app (`ohya.app`)

Status: in delivery. Milestone 1 (CLI & brand prerequisites) and Milestone 5.1
(`ohya status --remote --json` + Swift remote-hub wiring) implemented and
tested 2026-11-21; Milestones 2–4 SwiftPM package implemented under
`macos/OhyaMenuBar/` (deferred pending native macOS build/test verification);
signed/notarized universal `.app` & Homebrew Cask distribution open.
Planned against `74f5542`, 2026-11-21.
Decision record: [ADR 0017](../docs/adr/0017-macos-menubar-app.md).
Priority: P2; effort M, delivered in small increments starting with Go CLI
prerequisites.

## Outcome

A macOS user sees ohya's live memory health at a glance in the menu bar,
pauses or inspects capture without opening a terminal, searches and hands off
recent cross-agent sessions in two keystrokes, and invites or revokes devices
safely — all inside a quiet, minimalist native macOS interface that feels built
by Apple and costs almost nothing at idle.

The app stays strictly optional. `ohya` remains a complete CLI and `launchd`
service; quitting `ohya.app` never stops ingestion, upload, or serving.

## Design philosophy: minimalist, native, and delightful

Every screen and interaction follows current macOS Human Interface Guidelines
(macOS 26 Tahoe **Liquid Glass** with graceful macOS 15 Sequoia fallback) and
the restraint of [MANIFESTO.md](../MANIFESTO.md).

### 1. Quiet by default, progressive by exception

- **Serene steady state:** When everything is healthy and caught up, the
  popover shows only what earns its pixels — a single-line status header, a
  compact agent summary, and the primary workspace (Status or Recall). No
  always-on graphs, no dense tables, no raw JSON dumps, no promotional chrome.
- **Exception-driven surfacing:** Attention banners (stopped source, pending
  upload backlog, stale service, revoked device) and cleanup notices
  (agent-marked `tags.garbage` sessions) appear **only** when their count is
  non-zero and disappear smoothly once resolved.
- **Single compact surface:** Keep the `MenuBarExtra(.window)` popover narrow
  and scannable (**340 pt wide**, adaptive height capped at **460 pt**). Never
  embed multi-pane navigation stacks or heavy tables inside a menu bar popover;
  settings live in a dedicated macOS `Settings` window.

### 2. Liquid Glass & macOS 26 Tahoe craft

- **Let the system draw the glass:** Use SwiftUI's native
  `MenuBarExtra(.window)` chrome without legacy `NSVisualEffectView` wrappers
  or opaque custom background fills that fight Tahoe's dynamic refraction.
- **Navigation layer only:** Follow Apple's Liquid Glass layering rule —
  apply `.glassEffect()` inside a `GlassEffectContainer` selectively to
  floating control bars (the segmented mode switcher, search capsule, and
  footer action bar) while keeping content rows clean on the base popover
  surface with native semantic foregrounds (`.primary`, `.secondary`,
  `.tertiary`).
- **Concentric geometry:** Match inner card, capsule, and row highlight corner
  radii concentrically to the popover container (`ConcentricRectangle` on
  macOS 26, continuous `RoundedRectangle` fallback on macOS 15).
- **Transparent menu bar legibility:** macOS 26 renders the menu bar fully
  transparent over wallpaper. The elephant mark must ship as a monochrome
  template image (`isTemplate = true`, no purple tile, tuned stroke weight at
  18 pt) so macOS automatically switches dark/light/tinted ink and maintains
  contrast over any desktop wallpaper.
- **Intentional color restraint:** Reserve color strictly for semantic signals
  and brand identity — monochrome ink at rest, ohya purple (`#8250df` /
  `#a371f7` from [BRAND.md](../BRAND.md)) only for active brand accents when
  appropriate, system amber/red only for genuine degraded/error findings.

### 3. Delightful micro-interactions (without visual noise)

- **Quiet icon state grammar:**
  - *Caught up:* crisp monochrome elephant.
  - *Active backlog / importing:* subtle corner badge dot (never a frantic
    spinning icon in the menu bar).
  - *Attention needed (exit 3 / stopped source / dead service):* clean
    warning badge so the user notices without an intrusive modal.
  - *Paused:* reduced-opacity elephant.
- **Tactile transitions:**
  - Use `.contentTransition(.numericText())` for live backlog byte counts,
    session counts, and the 15-minute invite expiry countdown so digits roll
    cleanly instead of jittering.
  - Use `.contentTransition(.symbolEffect(.replace))` for inline feedback:
    clicking **Copy Handoff Prompt** or **Copy Fix Command** morphs
    `doc.on.doc` → `checkmark` with a brief inline label (*"Copied"*) for
    1.5 s — never a pop-up alert or toast stack.
  - Use subtle hover states (`Color.primary.opacity(0.06)` rounded row fills)
    and spring disclosure when expanding a session excerpt inline.
- **Keyboard-first flow:**
  - Optional user-configured global shortcut (off by default until the user
    opts in, e.g. `⌥⌘O`) opens the popover straight into Recall with the search
    field focused.
  - Inside the popover: `⌘1` / `⌘2` / `⌘3` switch sections (Status / Recall /
    Devices), `⌘K` or `/` focuses search, `↑` / `↓` moves selection, `Return`
    copies the handoff prompt, `⌥Return` toggles inline event preview, `⌘,`
    opens Settings, `Esc` clears search first and then dismisses the popover.
- **Accessibility & user preferences from day one:**
  - Honor `@Environment(\.accessibilityReduceMotion)` (disable springs and
    symbol bounce, fall back to instant opacity/state swap).
  - Honor `@Environment(\.accessibilityReduceTransparency)` and **Increase
    Contrast** (solidify control surfaces and hairline dividers).
  - Support Dynamic Type scaling, Full Keyboard Access, and concise VoiceOver
    labels/values for every status row and menu bar icon state (e.g., *"ohya:
    caught up"* or *"ohya: 1 stopped source"*).

### 4. Notification discipline

- Off by default or strictly opt-in during first run; **never** send routine
  "synced 12 events" chatter.
- Fire a debounced `UNUserNotificationCenter` notification **only** on state
  transitions that require human action:
  1. A source stops (malformed line or record above `--max-record-bytes`).
  2. This device's credential is revoked by the hub.
  3. An owned `launchd` service stops unexpectedly and stays down past one
     recheck interval.
- Clicking a notification opens the popover directly to the relevant finding.

---

## Architecture and runtime boundaries

### Integration points (existing and new)

| Component | Responsibility |
| --- | --- |
| `ohya status --json` | Primary health, topology, agent roots, sources, backlog, storage, and findings snapshot |
| `ohya service status\|start\|stop\|restart --json` *(M1)* | Pause/resume and restart owned `launchd` services without duplicating plist/launchctl logic |
| `ohya search --json`, `ohya show --json`, `ohya sessions --json` *(M1)* | Bounded, ADR 0015 egress-redacted recall and recent session list (local or joined `--remote`) |
| `ohya doctor --json` | Read-only integration health (Claude Code, Codex, Pi MCP wiring) and stopped-source remedies |
| `ohya usage --json` | Compact 24h / 7d count of agent `sql` and `annotate` calls |
| `ohya logs --level warn --limit 20 --json` | Sanitized recent warnings/errors (zero transcript content) |
| `ohya diagnose --output FILE` | Private diagnostics export via native `NSSavePanel` |
| `ohya devices list\|invite\|revoke --json` | Hub device roster, one-time 15-minute invitations, and final revocation |
| `ohya purge --marked tags.garbage [--apply] --json` | Owner-decided preview and execution of agent-nominated cleanup |
| `ohya version --json` | CLI discovery and version compatibility check |
| `scripts/brand-logo.py` *(M1)* | Authoritative generator for `menubar-template.svg` (+ `@1x`/`@2x` assets) |

### Swift module layout (`macos/OhyaMenuBar/`)

Pure Swift 6 (strict concurrency) + SwiftPM executable target, zero external
dependencies:

```text
macos/OhyaMenuBar/
├── Package.swift
├── Resources/
│   └── Assets.xcassets/           # MenuBarIcon template imageset + AppIcon
├── Sources/OhyaMenuBar/
│   ├── OhyaMenuBarApp.swift       # @main App: MenuBarExtra(.window) + Settings scene
│   ├── CLI/
│   │   ├── BinaryLocator.swift    # Override -> ~/.local/bin -> Homebrew -> /usr/local/bin
│   │   ├── OhyaRunner.swift       # actor: bounded Process execution, timeout, exit-code map
│   │   └── Envelope.swift         # Codable ADR 0005 envelope + typed command payloads
│   ├── State/
│   │   ├── AppModel.swift         # @MainActor @Observable root state
│   │   ├── ObserveWatcher.swift   # DispatchSource watcher on <state>/observe/
│   │   ├── ClipboardGuard.swift   # Transient/Concealed pasteboard + 60s changeCount clear
│   │   ├── LoginItemStore.swift   # SMAppService.mainApp wrapper synced on scene active
│   │   └── Notifier.swift         # Transition-only debounced UNUserNotificationCenter
│   └── Views/
│       ├── PopoverRootView.swift  # Header, segmented glass control, active tab, footer
│       ├── StatusView.swift       # Quiet agents summary, backlog, storage, findings, cleanup
│       ├── RecallView.swift       # Search capsule, filters, recent sessions, handoff copy
│       ├── DevicesView.swift      # Hub roster, invite countdown card, final-revoke confirm
│       ├── LogsSheetView.swift    # Compact sanitized warning viewer
│       └── SettingsView.swift     # Launch at Login, CLI path, notifications, hotkey
└── Tests/OhyaMenuBarTests/
    ├── EnvelopeDecodingTests.swift # Golden JSON fixtures shared with Go ADR 0005 tests
    ├── FTSQueryFormatterTests.swift# Auto-quoting hyphenated/punctuated terms safely
    └── TransitionTests.swift       # Icon state & notification debounce state machine
```

### Critical macOS `MenuBarExtra` implementation details

- **Accessory app lifecycle:** Set `LSUIElement = true` in `Info.plist` so the
  app never clutters the Dock or `⌘Tab`.
- **Reliable `Settings` opening:** Because `SettingsLink` can fail silently
  inside a `MenuBarExtra` render tree on accessory apps (including macOS 26),
  open settings via an explicit window action (`@Environment(\.openWindow)` or
  managed settings scene) followed by `NSApp.activate(ignoringOtherApps: true)`.
- **Panels and confirmations in front:** Call `NSApp.activate(ignoringOtherApps: true)`
  before presenting `NSSavePanel` (`ohya diagnose`) or destructive confirmation
  dialogs (device revocation, purge apply) so they never hide behind active
  desktop windows.
- **Launch at Login truth:** Read `SMAppService.mainApp.status` every time
  Settings opens (never cache a stale `UserDefaults` boolean), because users
  can toggle login items directly in macOS System Settings.
- **Safe FTS5 input ergonomics:** In `RecallView`, normalize bare punctuated
  tokens (such as `auth-refresh` or `src/auth.go`) into quoted FTS5 phrases
  unless the user writes explicit FTS operators (`AND`, `OR`, `NOT`, `"..."`),
  preventing the `no such column` FTS5 error while keeping full FTS5 syntax
  available.
- **Exit-code fidelity:** `OhyaRunner` maps exit `0` to healthy, exit `3` to
  valid-but-degraded (still decoding `data` and displaying findings), and exit
  `1`/`2` to structured error states with `error.hint`.

---

## Popover information architecture

```text
┌──────────────────────────────────────────┐
│ [elephant] ohya · Hub       ● Caught up  │  <- Brand mark, mode, live dot/text
│ ( Status )   ( Recall )   ( Devices )    │  <- GlassEffectContainer segmented bar
├──────────────────────────────────────────┤
│  STATUS (serene default)                 │
│  Claude Code    412 sources · 4s ago     │  <- Quiet hairline rows, tabular nums
│  Codex          188 sources · 4s ago     │     (excluded count shown only if >0)
│  Pi              19 sources · 4s ago     │
│                                          │
│  Storage    1.42 GB (+18 MB WAL)         │  <- Local only; joined notes hub-owned
│  Recall     47 queries · 6 marks (24h)   │  <- From ohya usage --json (cached)
│                                          │
│  [Only when active/degraded:]            │
│  ⚠ 1 stopped source · malformed line     │  <- Inline remedy + Copy Fix button
│  ↑ Uploading Codex · 18.4 MB remaining   │  <- Animated numericText() progress
│  🧹 3 sessions marked for cleanup  Review│  <- ADR 0016 tags.garbage dry-run link
├──────────────────────────────────────────┤
│ ⏸ Pause   Diagnose   Logs   ⚙   Quit     │  <- Compact glass footer bar
└──────────────────────────────────────────┘
```

- **Recall tab:** Focused search capsule at top, slim filter chips (`All`,
  `Claude`, `Codex`, `Pi`), scrollable list of recent sessions (when query is
  empty) or BM25 matches (when searching). Each row shows project (`repo` or
  folder basename), agent icon/label, device, relative time, subtle `active`
  badge when inside the 10-minute `events_settled` window, and one primary
  hover/keyboard action: **Copy Handoff Prompt**. Expanding a row loads the
  latest excerpted events via `ohya show --json` inline.
- **Devices tab:** On a hub, lists paired devices and active invitations with
  an **Invite Device** button (revealing a clean ephemeral card with live
  countdown, **Copy Join Command**, and **Copy Code** using `ClipboardGuard`)
  and per-device **Revoke…** behind a two-step confirmation stating revocation
  is permanent. On a joined device, shows hub connection health and the local
  device ID instead (avoiding an empty or confusing tab).

---

## Ordered milestones

### M1. Go CLI & brand prerequisites *(buildable & testable on Linux + macOS)*

Each item delivers standalone value to CLI users and unlocks the Swift app:

1. **Brand template mark (`scripts/brand-logo.py`):**
   - Generate `docs/assets/brand/menubar-template.svg` (monochrome black `#000`
     elephant mark without tile, stroke tuned for 18 pt menu bar clarity) and
     verify with `scripts/test_brand_logo.py`. Update [BRAND.md](../BRAND.md).
2. **`ohya service` subcommand (`cmd/ohya/service.go`):**
   - `ohya service status|start|stop|restart [--hub] [--state DIR] [--json]`
   - Reuse `internal/setup/service.go` plans and ownership receipts so edited
     user units are respected and never overwritten.
   - Follow ADR 0005 JSON envelope and exit codes; add unit tests with stubbed
     service runners.
3. **Structured recall output (`cmd/ohya/recall.go`):**
   - Add `--json` to `ohya search` and `ohya show` under ADR 0005 (keeping text
     default byte-identical).
   - Add `ohya sessions [--limit N] [--repo R] [--agent A] [--remote] [--json]`
     returning recent sessions (id, agent, device, repo, cwd, branch, model,
     first_prompt excerpt, event_count, last_activity_ms, settled boolean) via
     the same read-only `mcpserver` engine (so egress redaction applies).
   - Add golden JSON tests and update [ADR 0005](../docs/adr/0005-json-output.md)
     and [docs/reference.md](../docs/reference.md).

### M2. SwiftUI MVP — Quiet status, controls & alerts *(macOS)*

1. Create `macos/OhyaMenuBar/` SwiftPM package (zero dependencies, macOS 15+
   deployment target with macOS 26 Tahoe Liquid Glass styling).
2. Implement `BinaryLocator`, actor-isolated `OhyaRunner`, `Envelope` decoders
   (pinned against golden fixtures from M1), and `ObserveWatcher`.
3. Ship elephant template icon with 4 states (caught up, syncing, degraded,
   paused), **Status** tab, **Pause / Resume** (`ohya service`), **Logs**
   sheet, **Export Diagnostics** (`NSSavePanel` + `ohya diagnose`), **Settings**
   window (`SMAppService.mainApp` Launch at Login, CLI path, notification
   toggles), and debounced transition alerts.
4. Verify memory/CPU idle budget on a real Mac (< 30 MB RSS, < 0.2% idle CPU,
   zero SQLite file descriptors opened by `ohya.app`).

### M3. Recall & cross-agent handoff *(macOS)*

1. Implement **Recall** tab: live debounced search (`ohya search --json`) with
   safe FTS5 token quoting, agent/repo filters, empty-query recent sessions
   (`ohya sessions --json`), and inline expandable event preview
   (`ohya show --json`).
2. Implement **Copy Handoff Prompt** (keyboard `Return` or hover button) with
   inline checkmark morph feedback and `events_settled` awareness (notes when a
   session is <10m active so the prompt tells the receiving agent to read
   `events` directly).
3. Add optional user-configured global shortcut in Settings to open straight
   into Recall.

### M4. Hub devices & agent-marked cleanup *(macOS)*

1. Implement **Devices** tab: paired list, **Invite Device** ephemeral card
   with rolling 15-minute countdown and `ClipboardGuard` (transient + concealed
   pasteboard + 60s auto-clear), and final-revocation confirmation flow.
2. Surface agent-nominated cleanup (`ohya purge --marked tags.garbage --json`
   dry-run count checked lazily when the popover opens on a hub) with a
   preview sheet listing sessions/bytes and explicit **Purge** confirmation.
3. Add compact agent-integration health row from `ohya doctor --json` with a
   one-click repair trigger.

### M4b. First-run setup *(macOS; [ADR 0017 amendment, 2026-11-23](../docs/adr/0017-macos-menubar-app.md))* — done 2026-11-23

1. Detect a missing memory service from `ohya service status --json`
   (`installed: false`) and show a first-run view in place of the normal
   surfaces; disable Pause/Resume and hide service findings in that state.
2. **Use this Mac** runs `ohya setup --json`; **Join a hub** reads the code
   from a secure field and runs `ohya setup --join-code-env NAME --json`.
   A bundled `ohya` runs `ohya install` with the same flags instead.
   Show per-component results; offer a rerun when setup is incomplete.
3. Decode the `ohya setup --json` report in `OhyaMenuBarCore` with fixture
   tests, including an incomplete report.
4. "Copy fix" copies a runnable command, or is hidden when a finding has
   none. Status findings carry prose hints only, so the control is removed.

### M5. Remote visibility & signed macOS distribution *(depends on Plan 001 M5 + Apple Developer ID)*

1. Consume `ohya status --remote --json` (from Plan 001 Milestone 5) so joined
   laptops display hub health, hub storage, and connection path honestly.
2. Build universal (`arm64` + `x86_64`) `ohya.app` bundling the signed `ohya`
   binary, signed and notarized with Apple Developer ID, with a DMG and
   Homebrew Cask release step alongside `scripts/package.sh`.
   *Status 2026-11-23:* `macos/OhyaMenuBar/scripts/package-app.sh`, the
   `macos-app` candidate job, and `scripts/prepare-cask.py` are done and
   produce an ad-hoc signed universal DMG and an unpublished cask. Open:
   Developer ID signing and notarization (needs the certificate) and a
   Homebrew tap to publish the formula and the cask.

---

## Verification and acceptance criteria

### Automated tests (CI)
- **Go (Linux & macOS):** `make check` passes for M1 (`ohya service`,
  `ohya search/show/sessions --json`), with golden JSON contract tests and
  egress secret-redaction tests on `ohya sessions --json`.
- **Brand (Linux & macOS):** `python3 scripts/test_brand_logo.py` pins
  `menubar-template.svg`.
- **Swift (macOS CI):** `swift test` in `macos/OhyaMenuBar/` verifies ADR 0005
  JSON decoding against Go-generated golden fixtures, FTS5 query quoting edge
  cases, icon-state derivation, and notification transition debouncing.

### Manual macOS UX & craft checklist (before tagging a release)
- [ ] Menu bar elephant icon is crisp at 18 pt on both light and dark desktop
      wallpapers under macOS 26 Tahoe's transparent menu bar.
- [ ] Popover uses native Liquid Glass cleanly on macOS 26 and renders with
      clean system materials on macOS 15; no clipping or double-blur artifacts.
- [ ] Reduce Transparency, Increase Contrast, Reduce Motion, and VoiceOver all
      behave cleanly without layout breakage.
- [ ] Full keyboard navigation works without touching the mouse (`⌘K`, arrows,
      `Return`, `⌥Return`, `⌘,`, `Esc`).
- [ ] `lsof -p <pid>` confirms `ohya.app` holds zero open descriptors to
      `hub.db`, `registry.db`, or `credential.json`.
- [ ] Quitting `ohya.app` leaves `com.nibzard.ohya` running and ingesting.
- [ ] Copying a join code sets `TransientType`/`ConcealedType` and auto-clears
      after 60 s when the user has not copied anything else.
- [ ] Idle RSS stays under 30 MB and idle CPU stays near 0% over a 10-minute
      sample.

## Non-goals

- No Linux or Windows GUI in this plan (Linux users keep the full CLI and
  `systemd --user` service; `ohya service` works on both platforms).
- No embedded SQLite reader, local HTTP server, Electron/WebView shell, or
  third-party SwiftUI packages.
- No built-in LLM summaries, automatic tagging, or automatic purging.
- No background auto-updater daemon (updates follow Homebrew / signed release
  downloads).
