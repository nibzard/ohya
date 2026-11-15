#!/usr/bin/env python3
import hashlib
import io
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import unittest

class FormulaPreparationTest(unittest.TestCase):
    def test_checksums_and_foreign_outputs(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            for target in ("darwin_arm64", "darwin_amd64", "linux_amd64"):
                name = f"ohya_v0.1.0_{target}.tar.gz"
                with tarfile.open(root / name, "w:gz") as archive:
                    for entry in ("ohya", "INSTALL.md", "LICENSE"):
                        data = b"fixture"
                        info = tarfile.TarInfo(entry)
                        info.size = len(data)
                        archive.addfile(info, io.BytesIO(data))
                digest = hashlib.sha256((root / name).read_bytes()).hexdigest()
                (root / ("SHA256SUMS." + target.replace("_", "-"))).write_text(f"{digest}  {name}\n")
            output = root / "ohya.rb"
            command = [sys.executable, str(Path(__file__).with_name("prepare-formula.py")), "--version", "v0.1.0", "--artifacts", str(root), "--output", str(output)]
            first = subprocess.run(command, capture_output=True, text=True)
            self.assertEqual(first.returncode, 0, first.stderr)
            self.assertIn('version "0.1.0"', output.read_text())
            again = subprocess.run(command, capture_output=True, text=True)
            self.assertNotEqual(again.returncode, 0)
            output.unlink()
            (root / "SHA256SUMS.darwin-arm64").write_text("incorrect manifest\n")
            invalid = subprocess.run(command, capture_output=True, text=True)
            self.assertNotEqual(invalid.returncode, 0)
            self.assertFalse(output.exists())

if __name__ == "__main__":
    unittest.main()
