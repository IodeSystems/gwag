# Install

Release binaries and the container image are published by the release
workflow when a `v*` tag is pushed. Until the first tag cut after this
page landed, only `go install` and a source build work.

## Release binary

Linux and macOS, amd64 and arm64:

```
curl -fsSL https://raw.githubusercontent.com/iodesystems/gwag/main/install.sh | sh
```

`install.sh` downloads the archive and `checksums.txt` for the latest
release, checks the sha256, and installs `gwag` to `$PREFIX/bin`
(default `~/.local/bin`). No sudo. It warns when that directory is not
on `PATH`.

| Variable | Default | Meaning |
| --- | --- | --- |
| `GWAG_VERSION` | latest | Release tag, for example `v2.12.0` |
| `PREFIX` | `$HOME/.local` | Install prefix |

Manual install: download `gwag_<version>_<os>_<arch>.tar.gz` and
`checksums.txt` from the
[releases page](https://github.com/iodesystems/gwag/releases), check
with `sha256sum -c --ignore-missing checksums.txt`, extract, and put
`gwag` on `PATH`.

Release binaries embed the admin UI.

## go install

```
go install github.com/iodesystems/gwag/v2/gw/cmd/gwag@latest
```

This binary has the fallback UI page only: the module proxy carries no
UI build, so `/` serves a "UI bundle not built" page. The API, admin
endpoints and CLI work. For the admin UI use a release binary, the
image, or a source build.

## Container image

```
docker pull ghcr.io/iodesystems/gwag
docker run --rm -p 8080:8080 -v gwag-data:/data ghcr.io/iodesystems/gwag
```

Tags: `<version>` (without the `v`) and `latest`; linux/amd64 and
linux/arm64. The image is distroless, runs as uid 65532, keeps state in
`/data`, and embeds the admin UI. The default command is
`up --data-dir /data --no-context-write`.

## From source

```
git clone https://github.com/iodesystems/gwag
cd gwag
bin/build            # UI codegen + Vite + Go
bin/gwag up
```

`make install` builds the UI and installs `gwag` to `$PREFIX/bin`
(default `~/.local/bin`; `DESTDIR` stages for packagers). The binary is
written under a temporary name and renamed, so a running copy is never
half-written. `make install-bin` skips the UI rebuild. To run the result
as a service, see [running.md](./running.md#run-as-a-service).

Needs Go (version in `go.mod`), Node and pnpm (version in
`ui/package.json`). `bin/build --no-ui` builds Go only; the UI then is
the fallback page unless `ui/dist/` already holds a build.

## Version

Release builds stamp the tag into the binary. `go install ...@vX.Y.Z`
reads it from the module version. A checkout build reports `devel`.
