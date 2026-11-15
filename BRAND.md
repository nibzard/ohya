# ohya brand

This document tells you how to use the ohya™ logo, icon, and colors. For the
use of the name, see the [trademark policy](TRADEMARKS.md).

## The idea

The mark is an elephant. An elephant does not forget, and ohya is memory for
coding agents. The head and the trunk make the letter "y" of "ohya". The
purple ear is the only color in the mark.

## Files

All files are in [`docs/assets/brand/`](docs/assets/brand/).

| File | Use |
| --- | --- |
| `logo-light.svg` | Mark and wordmark on light backgrounds |
| `logo-dark.svg` | Mark and wordmark on dark backgrounds |
| `icon.svg` | Mark on the purple tile, 32 px and larger |
| `icon-small.svg` | Mark on the purple tile with a heavier line, smaller than 32 px |
| `icon-512.png` | Avatar for GitHub and other services |
| `social-preview.svg` | Source of the social preview |
| `social-preview.png` | Repository social preview, 1280 × 640 |

Use the logo (mark and wordmark) when you have space. Use the icon (mark on
the purple tile) at 64 px and smaller, for example a favicon or an avatar.
At small sizes, the tile and its color identify ohya.

On GitHub, select the light or dark logo with `<picture>`. GitHub does not
apply its theme to `prefers-color-scheme` inside an SVG file. The README
header shows the pattern.

## Regenerate the files

`scripts/brand-logo.py` draws all brand files. Change the script, not the
SVG files. Then run:

```sh
python3 scripts/brand-logo.py --png   # needs rsvg-convert for the PNG files
python3 scripts/test_brand_logo.py    # the SVG files must match the script
```

## Colors

| Name | Light theme | Dark theme |
| --- | --- | --- |
| Ink (head, trunk, wordmark) | `#1f2328` | `#e6edf3` |
| Ear purple | `#8250df` | `#a371f7` |
| Tile purple | `#8250df` | `#8250df` |
| Ear on the tile | `#d8c7ff` | `#d8c7ff` |

## Wordmark

The wordmark letters are drawn with the same round stroke as the mark. They
are not a font. Do not set "ohya" in a different typeface next to the mark.
In running text, write "ohya" in lowercase.

## Rules

- Keep clear space around the logo. The clear space is equal to the height
  of the "o" in the wordmark.
- Do not show the mark smaller than 16 px. Below 32 px, use `icon-small.svg`.
- Do not change the colors, add effects, or rotate, stretch, or outline the
  logo.
- Do not add an eye, a tusk, or other details to the mark. The simple form
  makes it clear at small sizes.
- Do not put the light logo on a dark background or the dark logo on a light
  background.

## License

The ohya logo, wordmark, icons, and social preview are not open source. They
are copyright Niko, all rights reserved. This includes `scripts/brand-logo.py`,
which draws them. The MIT License of this repository does not apply to them. The name and the marks are also trademarks; see
[TRADEMARKS.md](TRADEMARKS.md). See
[`docs/assets/brand/LICENSE`](docs/assets/brand/LICENSE). To use them, ask
for written permission.

## Manual setup

The social preview and the avatar are not set from files in the repository.
Upload `social-preview.png` in the repository settings under **Social
preview**, and upload `icon-512.png` as the avatar.
