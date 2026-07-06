# Releasing Magneton

Releases are automated via GitHub Actions + GoReleaser. Pushing a `v*` tag triggers the pipeline, builds binaries for all platforms, and publishes the GitHub Release.

## Steps

1. Make sure `main` is clean and all changes are pushed.

2. Pick a version following [semver](https://semver.org): `v0.1.0`, `v0.2.0`, `v1.0.0`, etc.

3. Tag and push:

   ```bash
   git tag v0.2.0
   git push origin v0.2.0
   ```

4. Watch the pipeline at `https://github.com/andresuarezz26/magneton/actions`.
   It takes ~3-5 minutes to build darwin/linux × arm64/amd64 and publish the release.

5. Verify the release at `https://github.com/andresuarezz26/magneton/releases`.
   It should contain four binaries (`magneton_darwin_arm64`, `magneton_darwin_amd64`,
   `magneton_linux_arm64`, `magneton_linux_amd64`), the tar.gz archives, and `checksums.txt`.

6. Test the installer:

   ```bash
   curl -fsSL https://raw.githubusercontent.com/andresuarezz26/magneton/main/install.sh | bash
   ```

## Re-releasing the same tag (e.g. to fix a bad release)

```bash
git push origin :v0.2.0   # delete from remote
git tag -d v0.2.0          # delete locally
git tag v0.2.0             # re-tag at current HEAD
git push origin v0.2.0     # push again — triggers the pipeline
```

## Homebrew tap (not yet active)

The `.goreleaser.yaml` has the Homebrew config commented out. To enable it:

1. Create the `andresuarezz26/homebrew-magneton` repo on GitHub.
2. Uncomment the `brews:` section in `.goreleaser.yaml`.
3. Add a `HOMEBREW_TAP_GITHUB_TOKEN` secret to the repo with a token that has
   write access to `homebrew-magneton`.
4. Tag a new release — GoReleaser will push the formula automatically.

Users can then install with:

```bash
brew install andresuarezz26/magneton/magneton
```
