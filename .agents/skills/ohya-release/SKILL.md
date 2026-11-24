---
name: ohya-release
description: Cut an ohya release through the qualified-candidate pipeline. Use when asked to cut, publish, prepare, or verify an ohya release such as vX.Y.Z.
---

# ohya release runbook

Policy, gates, and signing rules live in `docs/release-operations.md`. Read it
first. This file holds only the operator sequence. Publication needs the
user's explicit approval. The candidate workflow is read-only and creates no
tags or releases.

## Preconditions

- Clean tree on `main`, in sync with `origin/main`.
- Version chosen and approved by the user, for example `v0.1.2`.
- `gh` authenticated to `nibzard`.

## 1. Local gates

```sh
make check && make tla
```

Both must pass with no failures. Test output must be pristine.

## 2. Candidate workflow

Dispatch with the FULL commit SHA. `actions/checkout` cannot fetch a short
SHA; it treats one as a ref glob and fails.

```sh
REV=$(git rev-parse HEAD)
gh workflow run release-candidate.yml --repo nibzard/ohya --ref main \
    -f version=vX.Y.Z -f ref="$REV"
```

Watch without piping through `tail`; a pipe hides the exit code. Confirm the
JSON conclusion, not only the watch exit.

```sh
gh run watch <run-id> --repo nibzard/ohya --exit-status --interval 30
gh run view <run-id> --repo nibzard/ohya --json conclusion --jq .conclusion
```

All eight jobs must conclude `success`: resolve, three native builds,
macos-app, assemble, two Homebrew smokes.

## 3. Verify the evidence

```sh
gh run download <run-id> --repo nibzard/ohya \
    --name unpublished-release-candidates --dir dist/release-vX.Y.Z
cd dist/release-vX.Y.Z
shasum -a 256 -c SHA256SUMS
```

Check every `candidate.*.txt` file: `version` matches, `revision` equals the
release SHA, `dirty=false`. Check `native-libraries.*.txt` for system
libraries only. `signing=unverified` (archives) and `signing=adhoc`
(`candidate.macos-app.txt`) are expected until a Developer ID certificate
exists; state both in the release notes, including that the app needs
**Open Anyway** on first launch.

## 4. Tag and publish

```sh
git tag -a vX.Y.Z -m "vX.Y.Z" <full-sha>
git push origin vX.Y.Z

gh release create vX.Y.Z --repo nibzard/ohya --title "vX.Y.Z" --notes "..." \
    dist/release-vX.Y.Z/ohya_vX.Y.Z_{darwin_amd64,darwin_arm64,linux_amd64}.tar.gz \
    dist/release-vX.Y.Z/Ohya_vX.Y.Z_macos.dmg \
    dist/release-vX.Y.Z/SHA256SUMS dist/release-vX.Y.Z/install.sh
```

The release carries exactly six assets: three archives, the app DMG,
`SHA256SUMS`, and `install.sh`. The generated `ohya.rb` and `ohya-cask.rb`
stay unpublished until a Homebrew tap exists. Notes state what changed, the qualification with a link to the
run, and known limitations. Keep the release non-prerelease so `install.sh`
resolves it as latest. Do not replace assets on a published release.

## 5. Verify the release

```sh
curl -s -o /dev/null -w '%{url_effective}\n' -L \
    https://github.com/nibzard/ohya/releases/latest
```

The URL must resolve to the new tag. Then upgrade the local install as the
end-to-end test, from the repository root:

```sh
sh install.sh
```

Expect exit 0, `service: running`, `memory: ready`, and a clean `ohya doctor`.

## 6. Records

Update `docs/release-operations.md` (current verification limits), `NOTES.md`,
and stale version claims in `README.md`. Commit locally. Push only with
approval.

## Gotchas learned the hard way

- A short SHA in `ref` fails checkout with a bare git exit code.
- `gh run watch` piped through `tail` reports success on failure.
- With no stable release, GitHub redirects `/releases/latest` to `/releases`,
  and `install.sh` reports a misleading version-format error.
- GitHub can drop a queued macOS arm64 job ("not acquired by Runner ...
  after multiple attempts"). Rerun only the failed jobs on the same run with
  `gh run rerun <run-id> --failed`; the commit and artifacts stay the same.
- Another process can clear the gitignored `dist/` during a long release.
  Download the qualified artifacts outside the repository (for example
  `/tmp/ohya-release-vX.Y.Z`), verify checksums there, and publish from it.
- CI macOS runners are headless. They lack `resource coalition` and
  `jetsam coalition` blocks in `launchctl print`, so runner-green proves less
  than a real GUI Mac.
