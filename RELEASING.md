# Releasing BurstyRouter

BurstyRouter releases are tag-driven.

## GitHub And Homebrew

1. Make sure the tree is clean and all gates pass.
2. Create and push a version tag:

```bash
git tag -a v0.1.0 -m "v0.1.0"
git push origin v0.1.0
```

3. GitHub Actions runs GoReleaser for `v*` tags. GoReleaser builds darwin, linux, and windows archives, writes checksums, creates the GitHub release from the tag, and publishes the Homebrew formula to `Lore-Hex/homebrew-tap` as `burstyrouter`.

The release workflow expects a repository secret named `HOMEBREW_TAP_GITHUB_TOKEN` with permission to push to `Lore-Hex/homebrew-tap`. Without it, the workflow emits a warning before GoReleaser runs; the Homebrew publishing step may fail because GoReleaser requires the token for the configured tap.

## Local GoReleaser Check

If GoReleaser is installed locally:

```bash
goreleaser check
```

Do not install GoReleaser as part of routine release validation just to run this check.

## Docker

Build a local image:

```bash
docker build --build-arg VERSION="$(git describe --tags --always --dirty)" -t burstyrouter:local .
```

Run it against a host Ollama process:

```bash
docker run --rm -p 8383:8383 \
  -e BURSTY_LOCAL_URL=http://host.docker.internal:11434 \
  burstyrouter:local
```
