#!/usr/bin/env python3
import argparse
import hashlib
from pathlib import Path
import re

parser = argparse.ArgumentParser(description="Prepare an unpublished Homebrew cask from verified menu bar app DMG bytes")
parser.add_argument("--version", required=True)
parser.add_argument("--artifacts", type=Path, required=True)
parser.add_argument("--output", type=Path, required=True)
options = parser.parse_args()
if not re.fullmatch(r"v[0-9]+\.[0-9]+\.[0-9]+(?:[-+][A-Za-z0-9.+-]+)?", options.version):
    parser.error("invalid release version")
name = f"Ohya_{options.version}_macos.dmg"
checksum = hashlib.sha256()
with (options.artifacts / name).open("rb") as dmg_file:
    for chunk in iter(lambda: dmg_file.read(1024 * 1024), b""):
        checksum.update(chunk)
digest = checksum.hexdigest()
expected = (options.artifacts / "SHA256SUMS.macos-app").read_text().strip().split()
if expected != [digest, name]:
    parser.error(f"checksum manifest does not match {name}")

cask = f'''cask "ohya" do
  version "{options.version[1:]}"
  sha256 "{digest}"

  url "https://github.com/nibzard/ohya/releases/download/{options.version}/{name}"
  name "ohya"
  desc "Menu bar app for shared local-first memory for coding agents"
  homepage "https://github.com/nibzard/ohya"

  depends_on macos: :sequoia

  app "Ohya.app"

  uninstall quit: "com.nibzard.ohya.menubar"

  zap trash: "~/Library/Preferences/com.nibzard.ohya.menubar.plist"

  caveats <<~EOS
    Open ohya from Applications and choose Use this Mac or Join a hub.
    Run ohya uninstall before brew uninstall --cask ohya to remove the
    capture service and agent entries. Memory, credentials, and recovery
    points are preserved.
  EOS
end
'''
with options.output.open("x") as output:
    output.write(cask)
print(f"Prepared unpublished cask: {options.output}")
