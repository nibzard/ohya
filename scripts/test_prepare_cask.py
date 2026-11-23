#!/usr/bin/env python3
import hashlib
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

class CaskPreparationTest(unittest.TestCase):
    def test_checksum_and_foreign_outputs(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            name = "Ohya_v0.1.0_macos.dmg"
            (root / name).write_bytes(b"fixture disk image")
            digest = hashlib.sha256((root / name).read_bytes()).hexdigest()
            (root / "SHA256SUMS.macos-app").write_text(f"{digest}  {name}\n")
            output = root / "ohya-cask.rb"
            command = [sys.executable, str(Path(__file__).with_name("prepare-cask.py")), "--version", "v0.1.0", "--artifacts", str(root), "--output", str(output)]
            first = subprocess.run(command, capture_output=True, text=True)
            self.assertEqual(first.returncode, 0, first.stderr)
            text = output.read_text()
            self.assertIn('version "0.1.0"', text)
            self.assertIn(f'sha256 "{digest}"', text)
            self.assertIn("releases/download/v0.1.0/Ohya_v0.1.0_macos.dmg", text)
            again = subprocess.run(command, capture_output=True, text=True)
            self.assertNotEqual(again.returncode, 0)
            output.unlink()
            (root / "SHA256SUMS.macos-app").write_text("incorrect manifest\n")
            invalid = subprocess.run(command, capture_output=True, text=True)
            self.assertNotEqual(invalid.returncode, 0)
            self.assertFalse(output.exists())

if __name__ == "__main__":
    unittest.main()
