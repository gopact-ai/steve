---
name: steve-release
description: Cut a Steve release and publish its Homebrew formula after the owner approves the version.
---

A release publishes binaries publicly. Ask the owner to confirm the version
before tagging.

1. Confirm `master` is green and contains the intended changes. Pick the next
   semantic version `vX.Y.Z`.
2. Tag and push: `git tag -a vX.Y.Z -m vX.Y.Z && git push origin vX.Y.Z`.
3. The `release` workflow rebuilds the console, runs GoReleaser (darwin/linux,
   amd64/arm64, checksums, SBOMs) and attaches the macOS app zip to a draft
   release. Wait for both jobs, then check the assets and `checksums.txt`.
4. Publish the draft release once the owner agrees.
5. In `gopact-ai/homebrew-tap`, render the formula from the published checksums:
   `./scripts/render-steve-formula.sh X.Y.Z checksums.txt > Formula/steve.rb`,
   open a pull request and wait for `brew audit` and `brew test` in CI.
6. Verify on a clean machine: `brew install gopact-ai/tap/steve`,
   `steve version` and `steve init`.
