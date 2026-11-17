# Demo

`demo.gif` and `demo.mp4` show one handoff between two agents in about 50
seconds:

1. In Claude Code, a developer asks why a promotion test fails on CI every
   evening. Claude finds the cause and fixes it. The cause is a local-date
   check against UTC dates. The developer asks to keep promotion dates in
   UTC.
2. In a new Codex session, the developer asks about a happy-hour promotion
   and whether the team had time zone trouble before. Codex queries ohya
   with the `sql` tool. It recalls the earlier failure and the UTC decision,
   cites the event IDs, and applies the decision to the new task.

`final.cast` is the same recording for asciinema players.

## What is real and what is edited

- Both agents run in their normal interactive terminal user interfaces
  (TUIs). Every prompt, tool call, and answer comes from the live session.
- The recording speeds up agent work 6 times and typing 2 times. The cards
  between scenes say so.
- The editor replaces non-breaking spaces with spaces, because `agg` draws
  them as a visible glyph.
- The Claude Code scene loads project and local settings only. User hooks
  would print errors that a new user does not see.
- The Codex scene uses its own `CODEX_HOME`, logged in with
  `OPENAI_API_KEY`. That home holds only the ohya server, so personal
  instructions and usage-limit banners stay out of the scene.
- The ohya hub is a fresh database under `/tmp/ohya-demo` that holds only
  the Claude Code session. This keeps unrelated history out of the answer.

## Rebuild

You need `claude`, `codex`, `ohya`, `tmux`, `asciinema` 2.4, `agg`, and
`ffmpeg`. The script uses your Claude Code login and `OPENAI_API_KEY`.

1. Record both scenes. The script seeds `~/code/shopfront` from
   `shopfront/` and replaces any existing copy.

   ```sh
   demo/record.sh
   ```

2. Edit the takes into one cast:

   ```sh
   python3 demo/edit.py /tmp/ohya-demo demo/final.cast
   ```

3. Render the GIF and the MP4:

   ```sh
   agg --idle-time-limit 10 --font-family "DejaVu Sans Mono" --font-size 14 \
     demo/final.cast demo/demo.gif
   ffmpeg -i demo/demo.gif -movflags faststart -pix_fmt yuv420p \
     -vf "scale=trunc(iw/2)*2:trunc(ih/2)*2" demo/demo.mp4
   ```

Agent answers change from take to take. Watch each take before you publish
it.

## Recording notes

- `agg` limits each pause to 5 seconds by default. Keep
  `--idle-time-limit 10`, or the held answers end early.
- Claude Code draws some spaces as cursor moves. `edit.py` matches its
  markers with patterns that allow missing spaces.
- Codex starts one background app server for each `CODEX_HOME`. The script
  stops it before it deletes that home.
- The script runs its own tmux server with `tmux.conf`. This keeps tmux
  hints out of the agent screens and leaves your tmux sessions alone.
