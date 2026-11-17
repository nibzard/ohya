#!/usr/bin/env python3
import json
import os
import re
import shlex
import subprocess
import sys
import tempfile
import time
from pathlib import Path

from PIL import Image, ImageDraw, ImageFont

HERE = Path(__file__).resolve().parent
LOGO = HERE.parent / "docs/assets/brand/logo-dark.svg"
WIDTH, HEIGHT = 100, 30
WORK_SPEED = 6
TYPING_SPEED = 3
FPS = 30
ZOOM = 2
ZOOM_SECONDS = (0.4, 1.6, 0.4)
INK, MUTED = "#e6edf3", "#9198a1"
FONTS = Path("/usr/share/fonts/truetype/liberation")
AGG = [os.environ.get("AGG", "agg"), "--theme", "github-dark", "--font-family", "DejaVu Sans Mono",
       "--font-size", "20", "--idle-time-limit", "10", "--last-frame-duration", "0"]
ANSI = re.compile(r"\x1b\[[0-9;?<>=]*[ -/]*[@-~]|\x1b[\]P].*?(?:\x07|\x1b\\)|\x1b.")

STYLES = {
    "title": ("LiberationSans-Bold.ttf", 0.064, INK, "\x1b[1;37m"),
    "body": ("LiberationSans-Regular.ttf", 0.044, INK, "\x1b[37m"),
    "note": ("LiberationSans-Regular.ttf", 0.034, MUTED, "\x1b[2;37m"),
}

CARDS = {
    "opening": ([
        ("logo", 0.5), ("gap", 0.1),
        ("title", "One memory for all your coding agents."),
        ("note", "Claude Code · Codex · Pi"),
    ], 3.5),
    "claude": ([
        ("title", "Earlier, in Claude Code"), ("gap", 0.03),
        ("body", "A test fails on CI every evening."),
        ("body", "Claude finds out why."), ("gap", 0.03),
        ("note", "Agent work runs at 6× speed."),
    ], 3.5),
    "codex": ([
        ("title", "Later, in a new Codex session"), ("gap", 0.03),
        ("body", "Codex never saw that chat."),
        ("body", "It asks ohya."),
    ], 2.5),
    "closing": ([
        ("title", "Claude Code fixed the bug."),
        ("title", "Codex remembered why."), ("gap", 0.06),
        ("logo", 0.28), ("gap", 0.06),
        ("note", "github.com/nibzard/ohya"),
    ], 4.0),
}


def load(path):
    lines = Path(path).read_text().splitlines()
    events = [json.loads(line) for line in lines[1:]]
    return [[t, kind, data.replace("\u00a0", " ")] for t, kind, data in events]


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


def scenes(work):
    claude, codex = load(Path(work) / "claude.cast"), load(Path(work) / "codex.cast")
    claude_sent = first(claude, r"esc\s*to\s*interrupt")
    claude_done = first(claude, r"·\s*done")
    codex_idle = first(codex, r"Ask\s*Codex")
    codex_sent = first(codex, r"Working")
    codex_done = first(codex, r"Worked\s*for")
    return [
        ("card", "opening"),
        ("card", "claude"),
        ("scene", retime(claude, [
            (0, claude_sent, TYPING_SPEED),
            (claude_sent, claude_done, WORK_SPEED),
            (claude_done, claude_done + 0.5, 1),
        ]), 2.0, None),
        ("card", "codex"),
        ("scene", retime(codex, [
            (0, codex_idle, 1000),
            (codex_idle, codex_sent, TYPING_SPEED),
            (codex_sent, codex_done, WORK_SPEED),
            (codex_done, codex_done + 1.0, 1),
        ]), 4.0, r"Called\s*ohya\.sql"),
        ("card", "closing"),
    ]


def text_card(items):
    lines = []
    for kind, value in items:
        if kind == "logo":
            lines.append(("\x1b[1;37m", "ohya"))
        elif kind == "gap":
            lines.append(("", ""))
        else:
            lines.append((STYLES[kind][3], value))
    top = (HEIGHT - len(lines)) // 2
    body = "".join(
        f"\x1b[{top + i + 1};1H{style}{text.center(WIDTH).rstrip()}\x1b[0m"
        for i, (style, text) in enumerate(lines)
    )
    return "\x1b[?1049l\x1b[0m\x1b[2J\x1b[3J\x1b[H\x1b[?25l" + body + f"\x1b[{HEIGHT};{WIDTH}H"


def write_cast(path, events):
    header = {"version": 2, "width": WIDTH, "height": HEIGHT, "env": {"TERM": "xterm-256color"}}
    with open(path, "w") as cast:
        cast.write(json.dumps(header) + "\n")
        for t, kind, data in events:
            cast.write(json.dumps([round(t, 6), kind, data], ensure_ascii=False) + "\n")


def build_cast(parts, path):
    events, clock = [], 0.0
    for part in parts:
        if part[0] == "card":
            items, seconds = CARDS[part[1]]
            events.append([clock, "o", text_card(items)])
            clock += seconds
        else:
            (moved, length), hold, focus = part[1:]
            events.extend([clock + t, kind, data] for t, kind, data in moved)
            clock += length + hold + (sum(ZOOM_SECONDS) if focus else 0)
    events.append([clock, "o", "\x1b[0m"])
    write_cast(path, events)


def logo(width, tmp):
    path = tmp / f"logo-{width}.png"
    subprocess.run(["convert", "-background", "none", "-density", "600", str(LOGO),
                    "-resize", f"{width}x", str(path)], check=True)
    return Image.open(path).convert("RGBA")


def image_card(items, size, background, tmp, path):
    width, height = size
    blocks = []
    for kind, value in items:
        if kind == "logo":
            blocks.append(("image", logo(int(width * value), tmp)))
        elif kind == "gap":
            blocks.append(("gap", int(height * value)))
        else:
            font_file, scale, color, _ = STYLES[kind]
            font = ImageFont.truetype(str(FONTS / font_file), int(height * scale))
            blocks.append(("text", (value, font, color, int(height * scale * 1.45))))
    total = sum(b[1].height if b[0] == "image" else b[1] if b[0] == "gap" else b[1][3] for b in blocks)
    card = Image.new("RGB", size, background)
    draw = ImageDraw.Draw(card)
    y = (height - total) // 2
    for kind, block in blocks:
        if kind == "image":
            card.paste(block, ((width - block.width) // 2, y), block)
            y += block.height
        elif kind == "gap":
            y += block
        else:
            text, font, color, line_height = block
            draw.text((width // 2, y + line_height // 2), text, font=font, fill=color, anchor="mm")
            y += line_height
    card.save(path)


def screen_lines(events, tmp):
    stream = tmp / "screen.out"
    stream.write_text("".join(data for _, _, data in events))
    tmux = ["tmux", "-L", f"ohya-demo-build-{os.getpid()}", "-f", "/dev/null"]
    command = f"sleep 1; cat {shlex.quote(str(stream))}; sleep 60"
    subprocess.run([*tmux, "new-session", "-d", "-s", "screen", "-x", str(WIDTH), "-y", str(HEIGHT), command],
                   check=True)
    subprocess.run([*tmux, "set", "-t", "screen", "window-size", "manual"], check=True)
    subprocess.run([*tmux, "resize-window", "-t", "screen", "-x", str(WIDTH), "-y", str(HEIGHT)], check=True)
    time.sleep(3)
    lines = subprocess.run([*tmux, "capture-pane", "-p", "-t", "screen"], check=True,
                           capture_output=True, text=True).stdout.splitlines()
    subprocess.run([*tmux, "kill-server"], check=True)
    return lines


def last_frame(gif):
    image = Image.open(gif)
    image.seek(image.n_frames - 1)
    return image.convert("RGB")


def zoom_frames(frame, row, tmp):
    width, height = frame.size
    row_height = height / HEIGHT
    box_width, box_height = width / ZOOM, height / ZOOM
    top = min(max(0.0, (row - 1.5) * row_height), height - box_height)
    target = (0.0, top, box_width, top + box_height)
    full = (0.0, 0.0, float(width), float(height))
    zoom_in, hold, zoom_out = (round(seconds * FPS) for seconds in ZOOM_SECONDS)

    def box(progress):
        eased = progress * progress * (3 - 2 * progress)
        return tuple(a + (b - a) * eased for a, b in zip(full, target))

    boxes = [box(i / zoom_in) for i in range(1, zoom_in + 1)]
    boxes += [target] * hold
    boxes += [box(1 - i / zoom_out) for i in range(1, zoom_out + 1)]
    for i, crop in enumerate(boxes):
        frame.resize((width, height), Image.LANCZOS, box=crop).save(tmp / f"zoom-{i:03d}.png")
    return str(tmp / "zoom-%03d.png")


def build_video(parts, out):
    with tempfile.TemporaryDirectory() as name:
        tmp = Path(name)
        clips = []
        for i, part in enumerate(parts):
            if part[0] == "card":
                clips.append(part)
                continue
            (events, _), hold, focus = part[1:]
            cast, gif = tmp / f"scene-{i}.cast", tmp / f"scene-{i}.gif"
            write_cast(cast, events)
            subprocess.run([*AGG, str(cast), str(gif)], check=True, capture_output=True)
            if not focus:
                clips.append(("scene", gif, hold))
                continue
            row = next(n for n, line in enumerate(screen_lines(events, tmp)) if re.search(focus, line))
            final = tmp / f"final-{i}.png"
            last_frame(gif).save(final)
            clips += [("scene", gif, 0), ("frames", zoom_frames(last_frame(gif), row, tmp)), ("still", final, hold)]

        first_scene = next(c[1] for c in clips if c[0] == "scene")
        size = Image.open(first_scene).size
        background = Image.open(first_scene).convert("RGB").getpixel((2, 2))

        inputs, filters = [], []
        for i, clip in enumerate(clips):
            if clip[0] == "card":
                items, seconds = CARDS[clip[1]]
                png = tmp / f"card-{i}.png"
                image_card(items, size, background, tmp, png)
                inputs += ["-loop", "1", "-framerate", str(FPS), "-t", str(seconds), "-i", str(png)]
                filters.append(f"[{i}]fps={FPS},format=yuv420p,setsar=1[v{i}]")
            elif clip[0] == "still":
                inputs += ["-loop", "1", "-framerate", str(FPS), "-t", str(clip[2]), "-i", str(clip[1])]
                filters.append(f"[{i}]fps={FPS},format=yuv420p,setsar=1[v{i}]")
            elif clip[0] == "frames":
                inputs += ["-framerate", str(FPS), "-i", clip[1]]
                filters.append(f"[{i}]fps={FPS},format=yuv420p,setsar=1[v{i}]")
            else:
                inputs += ["-i", str(clip[1])]
                filters.append(
                    f"[{i}]fps={FPS},tpad=stop_mode=clone:stop_duration={clip[2]},format=yuv420p,setsar=1[v{i}]"
                )
        joined = "".join(f"[v{i}]" for i in range(len(clips)))
        graph = ";".join(filters) + f";{joined}concat=n={len(clips)}:v=1:a=0,scale=trunc(iw/2)*2:trunc(ih/2)*2[out]"
        mp4, gif = out / "demo.mp4", out / "demo.gif"
        subprocess.run(["ffmpeg", "-loglevel", "error", "-y", *inputs, "-filter_complex", graph,
                        "-map", "[out]", "-c:v", "libx264", "-crf", "20", "-movflags", "faststart",
                        str(mp4)], check=True)
        subprocess.run(["ffmpeg", "-loglevel", "error", "-y", "-i", str(mp4), "-vf",
                        "fps=12,split[a][b];[a]palettegen=max_colors=128[p];[b][p]paletteuse=dither=none",
                        str(gif)], check=True)


def main(work, out):
    out = Path(out)
    parts = scenes(work)
    build_cast(parts, out / "final.cast")
    build_video(parts, out)
    print(f"wrote {out / 'final.cast'}, {out / 'demo.mp4'}, and {out / 'demo.gif'}")


if __name__ == "__main__":
    main(*sys.argv[1:3])
