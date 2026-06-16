# Release Runbook

Use this checklist for every public release so the GitHub release, tag, and
source archive stay consistent.

## Naming

- Tag: `vX.Y.Z`
- Release title: `Omnia vX.Y.Z`
- Uploaded source archive: `omnia-vX.Y.Z.tar.gz`
- Archive root directory: `omnia-X.Y.Z/`

For example, tag `v0.0.2` uses asset `omnia-v0.0.2.tar.gz` and archive root
`omnia-0.0.2/`.

## Preflight

1. Start from an up-to-date `origin/master`.
2. Confirm the worktree is clean.
3. Run:

```bash
go test ./...
go build -o /tmp/omnia ./cmd/omnia
go build -o /tmp/omnia-daemon ./cmd/omnia-daemon
```

Build `cmd/bench` too when benchmark code or shared store/search code changes:

```bash
go build -o /tmp/omnia-bench ./cmd/bench
```

## Create The Tag

```bash
VERSION=vX.Y.Z
git tag -a "$VERSION" -m "Release $VERSION"
git push origin HEAD:master
git push origin "$VERSION"
```

## Release Notes Template

````markdown
# Omnia vX.Y.Z

Short summary of the release.

## Highlights

- User-facing change.
- Operational or daemon change.
- Build, packaging, or install change.

## Install From Source

```bash
go install github.com/wiruzman/omnia/cmd/omnia@vX.Y.Z
go install github.com/wiruzman/omnia/cmd/omnia-daemon@vX.Y.Z
```

## Homebrew Tap

```bash
brew tap wiruzman/tap
brew install omnia
```

Start the daemon with:

```bash
brew services start wiruzman/tap/omnia
```

Stop it with:

```bash
brew services stop wiruzman/tap/omnia
```

## Notes

- Requires macOS and Go 1.25+ when building from source.
- Include any compatibility notes, known issues, or follow-up actions.
````

Create or update the release notes:

```bash
gh release create "$VERSION" \
  --repo wiruzman/omnia \
  --title "Omnia $VERSION" \
  --notes-file /tmp/omnia-release-notes.md
```

If the release already exists:

```bash
gh release edit "$VERSION" \
  --repo wiruzman/omnia \
  --title "Omnia $VERSION" \
  --notes-file /tmp/omnia-release-notes.md
```

## Build And Upload The Source Archive

The release asset is a source archive generated from the exact tag:

```bash
VERSION=vX.Y.Z
ARCHIVE_VERSION="${VERSION#v}"
git archive \
  --format=tar.gz \
  --prefix="omnia-${ARCHIVE_VERSION}/" \
  -o "/tmp/omnia-${VERSION}.tar.gz" \
  "$VERSION"
tar -tzf "/tmp/omnia-${VERSION}.tar.gz" | head
shasum -a 256 "/tmp/omnia-${VERSION}.tar.gz"
```

Upload the archive after the GitHub release exists:

```bash
gh release upload "$VERSION" "/tmp/omnia-${VERSION}.tar.gz" --repo wiruzman/omnia
```

## Post-Release Checks

1. Verify the GitHub release has the `omnia-vX.Y.Z.tar.gz` asset.
2. Verify the archive SHA-256 is shown in the release asset metadata.
3. Check CI on `origin/master`.
4. Check the release page:

```bash
gh release view "$VERSION" --repo wiruzman/omnia --json tagName,name,assets,url
```
