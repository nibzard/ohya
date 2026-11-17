#!/usr/bin/env python3
import json
import re
import sys
from pathlib import Path

WIDTH, HEIGHT = 100, 30
WORK_SPEED = 6
TYPING_SPEED = 2
ANSI = re.compile(r"\x1b\[[0-9;?<>=]*[ -/]*[@-~]|\x1b[\]P].*?(?:\x07|\x1b\\)|\x1b.")


def load(path):
    lines = Path(path).read_text().splitlines()
    events = [json.loads(line) for line in lines[1:]]
    return [[t, kind, data.replace(" ", " ")] for t, kind, data in events]


def first(events, pattern):
    return next(t for t, _, data in events if re.search(pattern, ANSI.sub("", data)))


def retime(events, segments):
    out, offset = [], 0.0
    for start, end, speed in segments:
        for t, kind, data in events:
            if start <= t < end:
                out.append([offset + (t - start) / speed, kind, data])
        offset += (end - start) / speed
    return out, offset


def card(lines):
    top = (HEIGHT - len(lines)) // 2
    body = "".join(
        f"\x1b[{top + i + 1};1H{style}{text.center(WIDTH).rstrip()}\x1b[0m"
        for i, (style, text) in enumerate(lines)
    )
    return "\x1b[?1049l\x1b[0m\x1b[2J\x1b[3J\x1b[H\x1b[?25l" + body + f"\x1b[{HEIGHT};{WIDTH}H"


def main(work, output):
    claude, codex = load(Path(work) / "claude.cast"), load(Path(work) / "codex.cast")
    bold, dim = "\x1b[1;37m", "\x1b[2;37m"

    claude_sent = first(claude, r"esc\s*to\s*interrupt")
    claude_done = first(claude, r"·\s*done")
    codex_idle = first(codex, r"Ask\s*Codex")
    codex_sent = first(codex, r"Working")
    codex_done = first(codex, r"Worked\s*for")

    scenes = [
        (card([(bold, "ohya"), (dim, ""), (dim, "one memory for all your coding agents")]), 3.0),
        (card([
            (bold, "Earlier, in Claude Code"),
            (dim, ""),
            (dim, "A test fails on CI every evening. Claude finds out why."),
            (dim, ""),
            (dim, "(agent work runs at 6x speed)"),
        ]), 3.5),
        retime(claude, [
            (0, claude_sent, TYPING_SPEED),
            (claude_sent, claude_done, WORK_SPEED),
            (claude_done, claude_done + 1.0, 1),
        ]),
        5.0,
        (card([
            (bold, "Later, in a new Codex session"),
            (dim, ""),
            (dim, "Codex never saw that chat. It asks ohya."),
        ]), 3.0),
        retime(codex, [
            (0, codex_idle, 1000),
            (codex_idle, codex_sent, TYPING_SPEED),
            (codex_sent, codex_done, WORK_SPEED),
            (codex_done, codex_done + 1.0, 1),
        ]),
        8.0,
        (card([
            (bold, "Claude Code fixed the bug. Codex remembered why."),
            (dim, ""),
            (dim, "ohya keeps one local-first memory for Claude Code, Codex, and Pi."),
            (dim, ""),
            (bold, "github.com/nibzard/ohya"),
        ]), 5.0),
    ]

    events, clock = [], 0.0
    for scene in scenes:
        if isinstance(scene, float):
            clock += scene
        elif isinstance(scene[0], str):
            events.append([round(clock, 6), "o", scene[0]])
            clock += scene[1]
        else:
            moved, length = scene
            events.extend([round(clock + t, 6), kind, data] for t, kind, data in moved)
            clock += length
    events.append([round(clock, 6), "o", "\x1b[0m"])

    header = {"version": 2, "width": WIDTH, "height": HEIGHT, "env": {"TERM": "xterm-256color"}}
    with open(output, "w") as cast:
        cast.write(json.dumps(header) + "\n")
        for event in events:
            cast.write(json.dumps(event, ensure_ascii=False) + "\n")
    print(f"wrote {output}: {clock:.1f}s")


if __name__ == "__main__":
    main(*sys.argv[1:3])
