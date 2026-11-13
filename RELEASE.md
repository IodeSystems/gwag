# Release process

Step-by-step for cutting a tagged release of `github.com/iodesystems/gwag/v2`.

## Before the first 1.0

Once 1.0 ships, the `// Stability: stable` markers and
[`docs/stability.md`](./docs/stability.md) lock the public surface
under SemVer. Drop renames, signature changes, and behavioural
breaks need to land **before** the 1.0 tag — after the tag they
become 2.0 work with a deprecation cycle.

## Cutting a release

1. **Working tree clean, main up-to-date.**

   ```
   git switch main
   git fetch origin && git rebase origin/main
   git status                  # no uncommitted changes
   ```

2. **Update `CHANGELOG.md`.**

   Move the `## Unreleased` section to a dated version section:

   ```markdown
   ## Unreleased

   ## v1.0.0 — 2026-MM-DD

   ### Added
   …
   ```

   Keep `Unreleased` empty (the `Item shape` reminder in
   `AGENTS.md` covers the next-PR convention).

3. **Verify the build.**

   ```
   bin/build                   # full build (UI codegen + Vite + Go)
   go test ./...               # full suite, ~60s for the gw/ package
   go vet ./...
   ```

   Everything green. If the perf matrix changed, optionally:

   ```
   bin/bench perf              # regenerates docs/perf.md
   ```

4. **Commit the changelog flip.**

   ```
   git add CHANGELOG.md
   git commit -m "docs(release): cut v1.0.0"
   ```

5. **Tag and push.**

   ```
   git tag -a v1.0.0 -m "v1.0.0"
   git push origin main
   git push origin v1.0.0
   ```

   Tags are annotated, not lightweight — `git describe` and Go
   module proxies expect the metadata.

6. **Let the release workflow publish.**

   Pushing the tag starts `.github/workflows/release.yml`. It runs
   GoReleaser (`.goreleaser.yaml`): builds the UI, then `gwag` for
   linux and darwin on amd64 and arm64, writes `checksums.txt`,
   creates the GitHub release with a changelog from git, and pushes
   the multi-arch image `ghcr.io/iodesystems/gwag` (tags `1.0.0` and
   `latest`). Watch the run in the Actions tab.

   Dry run without publishing:

   ```
   goreleaser release --snapshot --clean --skip=publish
   ```

   Edit the release notes on GitHub afterwards if the generated list
   needs the `CHANGELOG.md` section instead. If the workflow fails
   after the release exists, fix the cause and re-run the failed
   job; do not move the tag.

7. **Verify the Go proxy picked it up.**

   ```
   go list -m -versions github.com/iodesystems/gwag/v2
   ```

   `v1.0.0` should appear within a minute. If not, the proxy is
   lagging; `GOPROXY=direct` to bypass. Check the install channels
   in [`docs/install.md`](./docs/install.md): `install.sh`,
   `docker pull ghcr.io/iodesystems/gwag:1.0.0`.

## Patch and minor releases

Same flow, different version bump. Reminder of the policy from
[`docs/stability.md`](./docs/stability.md):

- **PATCH** (`1.x.y → 1.x.(y+1)`) — bug fixes, doc fixes, internal
  performance improvements. No surface change.
- **MINOR** (`1.y.z → 1.(y+1).0`) — adds stable symbols, promotes
  experimental → stable, additive proto / metric fields, raises
  defaults.
- **MAJOR** (`1.y.z → 2.0.0`) — drops or renames stable symbols,
  changes wire format, demotes stable → experimental. Bring up
  a `v2/` import path under `gw/v2/`; never break the existing
  module's tag history.

## Backports

We don't currently maintain release branches. Bug fixes ride main;
patch releases tag main. If a downstream pinning makes a backport
necessary, branch `release/v1.x` off the tag, cherry-pick the fix,
tag `v1.x.(y+1)`, push the branch.

## Future flow (not yet wired)

- **`bin/release`.** A wrapper around the manual flow above when
  the steps stabilise enough that scripting them adds value.
- **CI release verification.** The release workflow does not run the
  test suite; `.github/workflows/ci.yml` does on every push and PR.
  Gating the release on a green CI run for the tagged commit is not
  wired.
