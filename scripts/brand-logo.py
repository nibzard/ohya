#!/usr/bin/env python3
import argparse
import math
import shutil
import subprocess
from pathlib import Path

CX, CY, R = 34, 26, 13
STROKE = 7.0
TRUNK_TIP = 3.6
ARM_LEAN = 1.6
ARM_LENGTH = 11.5
LOCKUP_MARK_SCALE = 1.55
LOCKUP_VIEWBOX = (-2.0, 8.0, 252.0, 82.5)
EAR_POINTS = [(24.5, 18), (13, 16), (4, 22.5), (1.5, 33), (3.5, 43), (8.5, 51), (14.5, 53), (19.5, 47.5), (23.5, 40.5), (26.5, 34)]
THEMES = {
    "light": {"ink": "#1f2328", "ear": "#8250df"},
    "dark": {"ink": "#e6edf3", "ear": "#a371f7"},
}
TILE = {"fill": "#8250df", "ink": "#ffffff", "ear": "#d8c7ff"}
SOCIAL = {"background": "#0d1117", "title": "#e6edf3", "subtitle": "#9198a1"}


def bezier(p0, p1, p2, p3, n=24):
    points = []
    for i in range(n + 1):
        t = i / n
        points.append(tuple((1 - t) ** 3 * a + 3 * (1 - t) ** 2 * t * b + 3 * (1 - t) * t * t * c + t ** 3 * d for a, b, c, d in zip(p0, p1, p2, p3)))
    return points


def catmull_rom(points, n=10):
    out = []
    for i in range(len(points) - 1):
        p0, p1, p2, p3 = points[max(i - 1, 0)], points[i], points[i + 1], points[min(i + 2, len(points) - 1)]
        out.extend(_catmull_segment(p0, p1, p2, p3, n))
    out.append(points[-1])
    return out


def _catmull_segment(p0, p1, p2, p3, n):
    out = []
    for k in range(n):
        t = k / n
        t2, t3 = t * t, t * t * t
        out.append(tuple(0.5 * (2 * b + (-a + c) * t + (2 * a - 5 * b + 4 * c - d) * t2 + (-a + 3 * b - 3 * c + d) * t3) for a, b, c, d in zip(p0, p1, p2, p3)))
    return out


def closed_curve(points, n=12):
    padded = [points[-1]] + points + [points[0], points[1]]
    out = []
    for i in range(1, len(points) + 1):
        out.extend(_catmull_segment(padded[i - 1], padded[i], padded[i + 1], padded[i + 2], n))
    return "M" + " L".join(f"{x:.2f},{y:.2f}" for x, y in out) + " Z"


def normals(points):
    out = []
    for i in range(len(points)):
        a, b = points[max(i - 1, 0)], points[min(i + 1, len(points) - 1)]
        dx, dy = b[0] - a[0], b[1] - a[1]
        length = math.hypot(dx, dy) or 1
        out.append((-dy / length, dx / length))
    return out


def ribbon(points, width):
    width_at = width if callable(width) else (lambda i, n: width)
    n = len(points)
    left, right = [], []
    for i, ((x, y), (nx, ny)) in enumerate(zip(points, normals(points))):
        half = width_at(i, n) / 2
        left.append((x + nx * half, y + ny * half))
        right.append((x - nx * half, y - ny * half))
    path = "M" + " L".join(f"{x:.2f},{y:.2f}" for x, y in left + right[::-1]) + " Z"
    for i in (0, n - 1):
        half = width_at(i, n) / 2
        x, y = points[i]
        path += f" M{x + half:.2f},{y:.2f} a{half:.2f},{half:.2f} 0 1 0 {-2 * half:.2f},0 a{half:.2f},{half:.2f} 0 1 0 {2 * half:.2f},0 Z"
    return path


def circle(cx, cy, r):
    return f"M{cx + r},{cy} a{r},{r} 0 1 0 {-2 * r},0 a{r},{r} 0 1 0 {2 * r},0 Z"


def arm(side):
    return catmull_rom([(CX + side * R, CY), (CX + side * (R - 0.15 * ARM_LEAN), CY - ARM_LENGTH * 0.5), (CX + side * (R - ARM_LEAN), CY - ARM_LENGTH)], 8)


def y_strokes(stroke=STROKE):
    bowl = [(CX + R * math.cos(math.radians(a)), CY - R * math.sin(math.radians(a))) for a in range(180, 361, 4)]
    left = arm(-1)[::-1] + bowl[1:]
    trunk = bezier((CX + R, CY + 14), (CX + R, CY + 25), (CX + 5, CY + 30), (CX - 0.5, CY + 26))
    right = arm(1)[::-1] + [(CX + R, CY + k) for k in range(1, 15)] + trunk[1:]
    taper_from = next(i for i, (_, y) in enumerate(right) if y >= CY + 10)

    def right_width(i, n, extra=0.0):
        if i < taper_from:
            return stroke + extra
        return stroke + extra - (stroke - TRUNK_TIP) * ((i - taper_from) / (n - 1 - taper_from))

    return left, right, right_width


class Ids:
    def __init__(self):
        self.count = 0

    def next(self):
        self.count += 1
        return f"m{self.count}"


def mark(ink, ear_color, ids, stroke=STROKE, ear_scale=1.0):
    left, right, right_width = y_strokes(stroke)
    head = ribbon(left, stroke) + " " + ribbon(right, right_width)
    gap = ribbon(left, stroke + 6) + " " + ribbon(right, lambda i, n: right_width(i, n, 6))
    ax, ay = 26, 30
    ear = closed_curve([(ax + (x - ax) * ear_scale, ay + (y - ay) * ear_scale) for x, y in EAR_POINTS])
    mask = ids.next()
    return (
        f'<defs><mask id="{mask}" maskUnits="userSpaceOnUse" x="-20" y="-20" width="120" height="120">'
        f'<rect x="-20" y="-20" width="120" height="120" fill="#fff"/>'
        f'<path d="{gap}" fill="#000"/><path d="{circle(CX, CY, R + 3)}" fill="#000"/></mask></defs>'
        f'<path d="{ear}" fill="{ear_color}" mask="url(#{mask})"/>'
        f'<path d="{head}" fill="{ink}"/>'
    )


def ring(cx):
    outer, inner = R + STROKE / 2, R - STROKE / 2
    return f'<path d="{circle(cx, CY, outer)} {circle(cx, CY, inner)}" fill-rule="evenodd" fill="INK"/>'


def wordmark(ink):
    parts = [ring(16.5)]
    stem_x, arch_r = 43.5, 11.5
    parts.append(f'<path d="{ribbon([(stem_x, CY - R - 15), (stem_x, CY + R)], STROKE)}" fill="INK"/>')
    arch = [(stem_x + arch_r + arch_r * math.cos(math.radians(a)), CY - 1 - arch_r * math.sin(math.radians(a))) for a in range(180, -1, -6)]
    parts.append(f'<path d="{ribbon(arch + [(stem_x + 2 * arch_r, CY + R)], STROKE)}" fill="INK"/>')
    left, right, right_width = y_strokes()
    parts.append(f'<g transform="translate(59.5,0)"><path d="{ribbon(left, STROKE)}" fill="INK"/><path d="{ribbon(right, right_width)}" fill="INK"/></g>')
    a_x = 133.5
    parts.append(ring(a_x))
    parts.append(f'<path d="{ribbon([(a_x + R, CY - R), (a_x + R, CY + R)], STROKE)}" fill="INK"/>')
    return "".join(parts).replace("INK", ink)


def lockup(theme, ids):
    s = LOCKUP_MARK_SCALE
    return (
        f'<g transform="scale({s})">{mark(theme["ink"], theme["ear"], ids)}</g>'
        f'<g transform="translate(96,{CY * s - CY:.2f})">{wordmark(theme["ink"])}</g>'
    )


def svg(width, height, view_box, body):
    return f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="{view_box}" width="{width}" height="{height}" role="img" aria-label="ohya">{body}</svg>\n'


def logo(theme_name):
    x, y, w, h = LOCKUP_VIEWBOX
    return svg(round(w * 1.6), round(h * 1.6), f"{x} {y} {w} {h}", lockup(THEMES[theme_name], Ids()))


def icon(stroke=STROKE, ear_scale=1.0):
    body = (
        f'<rect width="64" height="64" rx="14" fill="{TILE["fill"]}"/>'
        f'<g transform="translate(8.4,4.8) scale(0.84)">{mark(TILE["ink"], TILE["ear"], Ids(), stroke, ear_scale)}</g>'
    )
    return svg(64, 64, "0 0 64 64", body)


def menubar_template(stroke=8.6, ear_scale=1.08):
    body = f'<g transform="translate(3.4,-4.8) scale(1.12)">{mark("#000000", "#000000", Ids(), stroke, ear_scale)}</g>'
    return svg(18, 18, "0 0 64 64", body)


def social_preview():
    x, y, w, _ = LOCKUP_VIEWBOX
    scale = 780 / w
    font = 'font-family="Helvetica Neue, Helvetica, Arial, sans-serif" text-anchor="middle"'
    body = (
        f'<rect width="1280" height="640" fill="{SOCIAL["background"]}"/>'
        f'<g transform="translate({640 - 390 - x * scale:.1f},{130 - y * scale:.1f}) scale({scale:.3f})">{lockup(THEMES["dark"], Ids())}</g>'
        f'<text x="640" y="480" {font} font-size="44" font-weight="bold" fill="{SOCIAL["title"]}">One memory for all your coding agents.</text>'
        f'<text x="640" y="540" {font} font-size="28" fill="{SOCIAL["subtitle"]}">Claude Code · Codex · Pi  —  local-first · SQLite · MCP</text>'
    )
    return svg(1280, 640, "0 0 1280 640", body)


def assets():
    return {
        "logo-light.svg": logo("light"),
        "logo-dark.svg": logo("dark"),
        "icon.svg": icon(),
        "icon-small.svg": icon(stroke=8.6, ear_scale=1.08),
        "menubar-template.svg": menubar_template(),
        "social-preview.svg": social_preview(),
    }


def render_png(source, target, size):
    converter = shutil.which("rsvg-convert")
    if converter is None:
        raise SystemExit("rsvg-convert is required for --png")
    subprocess.run([converter, "-w", str(size[0]), "-h", str(size[1]), "-o", str(target), str(source)], check=True)


def main():
    parser = argparse.ArgumentParser(description="Generate the ohya logo, icons, and social preview.")
    parser.add_argument("--out", type=Path, default=Path(__file__).resolve().parent.parent / "docs" / "assets" / "brand")
    parser.add_argument("--png", action="store_true", help="also render icon-512.png and social-preview.png with rsvg-convert")
    args = parser.parse_args()
    args.out.mkdir(parents=True, exist_ok=True)
    for name, content in assets().items():
        (args.out / name).write_text(content, encoding="utf-8")
    if args.png:
        render_png(args.out / "icon.svg", args.out / "icon-512.png", (512, 512))
        render_png(args.out / "social-preview.svg", args.out / "social-preview.png", (1280, 640))


if __name__ == "__main__":
    main()
