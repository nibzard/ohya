#!/usr/bin/env python3
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

SCRIPTS = Path(__file__).resolve().parent
BRAND = SCRIPTS.parent / "docs" / "assets" / "brand"
GENERATED = ("logo-light.svg", "logo-dark.svg", "icon.svg", "icon-small.svg", "menubar-template.svg", "social-preview.svg")

class BrandLogoTest(unittest.TestCase):
    def test_committed_svgs_match_generator(self):
        with tempfile.TemporaryDirectory() as temporary:
            out = Path(temporary)
            subprocess.run([sys.executable, str(SCRIPTS / "brand-logo.py"), "--out", str(out)], check=True)
            for name in GENERATED:
                with self.subTest(name=name):
                    self.assertEqual((out / name).read_text(encoding="utf-8"), (BRAND / name).read_text(encoding="utf-8"), f"{name} differs; run scripts/brand-logo.py --png")

    def test_masks_have_unique_ids_per_file(self):
        for name in GENERATED:
            with self.subTest(name=name):
                content = (BRAND / name).read_text(encoding="utf-8")
                ids = [part.split('"', 1)[0] for part in content.split('<mask id="')[1:]]
                self.assertEqual(len(ids), len(set(ids)))

    def test_menubar_template_is_monochrome_and_tile_free(self):
        content = (BRAND / "menubar-template.svg").read_text(encoding="utf-8")
        self.assertIn('width="18" height="18"', content)
        self.assertNotIn('#8250df', content)
        self.assertNotIn('<rect width="64"', content)
        self.assertIn('fill="#000000"', content)

if __name__ == "__main__":
    unittest.main()
