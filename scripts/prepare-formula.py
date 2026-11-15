#!/usr/bin/env python3
import argparse
import hashlib
from pathlib import Path
import re
import tarfile

parser = argparse.ArgumentParser(description="Prepare an unpublished Homebrew formula from verified archive bytes")
parser.add_argument("--version", required=True)
parser.add_argument("--artifacts", type=Path, required=True)
parser.add_argument("--output", type=Path, required=True)
options = parser.parse_args()
if not re.fullmatch(r"v[0-9]+\.[0-9]+\.[0-9]+(?:[-+][A-Za-z0-9.+-]+)?", options.version):
    parser.error("invalid release version")
checksums = {}
for target in ("darwin_arm64", "darwin_amd64", "linux_amd64"):
    name = f"ohya_{options.version}_{target}.tar.gz"
    checksum = hashlib.sha256()
    with (options.artifacts / name).open("rb") as archive_file:
        for chunk in iter(lambda: archive_file.read(1024 * 1024), b""):
            checksum.update(chunk)
    digest = checksum.hexdigest()
    manifest = options.artifacts / ("SHA256SUMS." + target.replace("_", "-"))
    expected = manifest.read_text().strip().split()
    if expected != [digest, name]:
        parser.error(f"checksum manifest does not match {name}")
    with tarfile.open(options.artifacts / name, "r:gz") as archive_file:
        members = archive_file.getmembers()
        if sorted(item.name for item in members) != ["INSTALL.md", "LICENSE", "ohya"] or not all(item.isfile() for item in members):
            parser.error(f"invalid archive layout in {name}")
    checksums[target] = (name, digest)

def archive(target, indent):
    name, digest = checksums[target]
    return f'{indent}url "https://github.com/nibzard/ohya/releases/download/{options.version}/{name}"\n{indent}sha256 "{digest}"\n'

formula = '''class Ohya < Formula
  desc "Shared local-first memory for coding agents"
  homepage "https://github.com/nibzard/ohya"
'''
formula += f'  version "{options.version[1:]}"\n  license "MIT"\n\n'
formula += '  on_macos do\n    on_arm do\n' + archive("darwin_arm64", "      ") + '    end\n    on_intel do\n' + archive("darwin_amd64", "      ") + '    end\n  end\n\n'
formula += '  on_linux do\n    depends_on arch: :x86_64\n' + archive("linux_amd64", "    ") + '  end\n\n'
formula += '''  def install
    bin.install "ohya"
  end

  def caveats
    <<~EOS
      Run ohya setup to configure memory and installed agents.
      Run ohya uninstall before brew uninstall to remove owned integrations.
      Memory, credentials, and recovery points are preserved.
    EOS
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/ohya version")
    system "#{bin}/ohya", "setup", "--dry-run", "--no-agents", "--no-service", "--no-modify-path", "--state", testpath/"state"
  end
end
'''
with options.output.open("x") as output:
    output.write(formula)
print(f"Prepared unpublished formula: {options.output}")
