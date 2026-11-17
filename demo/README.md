# Demo

`demo.gif` and `demo.mp4` show one handoff between two agents in about 40
seconds. The video opens with the ohya logo and closes with it:

1. In Claude Code, a developer asks why a promotion test fails on CI every
   evening. Claude finds the cause and fixes it. The cause is a local-date
   check against UTC dates. The developer asks to keep promotion dates in
   UTC.
2. In a new Codex session, the developer asks about a happy-hour promotion
   and whether the team had time zone trouble before. Codex queries ohya
   with the `sql` tool. It recalls the earlier failure and the UTC decision,
   cites the event IDs, and applies the decision to the new task.

`final.cast` is the same recording for asciinema players. A terminal cannot
show the logo or large type, so its cards are plain text.

## What is real and what is edited

- Both agents run in their normal interactive terminal user interfaces
  (TUIs). Every prompt, tool call, and answer comes from the live session.
- The recording speeds up agent work 6 times and typing 3 times. It also
  shortens the pauses after each agent finishes. The cards between scenes
  say that agent work is sped up.
- After Codex answers, the video zooms in on the `Called ohya.sql` tool
  call, then zooms back out to the full answer.
- The build replaces non-breaking spaces with spaces, because `agg` draws
  them as a visible glyph.
- The Claude Code scene loads project and local settings only. User hooks
  would print errors that a new user does not see.
- The Codex scene uses its own `CODEX_HOME`, logged in with
  `OPENAI_API_KEY`. That home holds only the ohya server, so personal
  instructions and usage-limit banners stay out of the scene.
- The ohya hub is a fresh database under `/tmp/ohya-demo` that holds only
  the Claude Code session. This keeps unrelated history out of the answer.

## Rebuild

You need `claude`, `codex`, `ohya`, `tmux`, `asciinema` 2.4, `agg`,
`ffmpeg`, ImageMagick `convert`, Python Imaging Library (Pillow), and the
Liberation Sans font. The script uses your Claude Code login and
`OPENAI_API_KEY`.

1. Record both scenes. The script seeds `~/code/shopfront` from
   `shopfront/` and replaces any existing copy.

   ```sh
   demo/record.sh
   ```

2. Build `final.cast`, `demo.mp4`, and `demo.gif` from the takes. Set
   `AGG` if `agg` is not on your `PATH`.

   ```sh
   python3 demo/build.py /tmp/ohya-demo demo
   ```

   The build renders each agent scene with `agg`. It draws the cards as
   images with the logo from `docs/assets/brand/logo-dark.svg`. Then
   `ffmpeg` joins the cards and the scenes.

Agent answers change from take to take. Watch each take before you publish
it.

## Recording notes

- `agg` holds the last frame of a GIF for 3 seconds by default. The build
  sets `--last-frame-duration 0` and adds its own holds.
- The build finds the row of the zoom target by replaying the Codex output
  into a tmux pane and reading the screen back.
- `ffmpeg` shows the last frame of a GIF for only one frame. The build
  holds each scene's last frame with the `tpad` filter instead.
- The cards take their background color from the `github-dark` terminal
  theme, so cards and scenes match.
- Claude Code draws some spaces as cursor moves. `build.py` matches its
  markers with patterns that allow missing spaces.
- Codex starts one background app server for each `CODEX_HOME`. The script
  stops it before it deletes that home.
- The script runs its own tmux server with `tmux.conf`. This keeps tmux
  hints out of the agent screens and leaves your tmux sessions alone.
