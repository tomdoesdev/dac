# dac

`dac` (“download a copy”) locks arbitrary HTTP(S) files so a project can later
reproduce exactly the bytes it accepted. It sits between `curl` and a package
manager: it has URLs, optional template variables, authentication headers,
SHA-256 pins, and a lock file—but no registries, dependency graph, cache, or
installation lifecycle.

```sh
dac init
dac add artifact https://example.com/releases/artifact.ear
dac lock
dac pull
```

Asset names are opaque identifiers and may use printable non-whitespace
Unicode and punctuation, so teams can adopt conventions such as
`namespace/asset@version` or `@scope/package:version`. A name beginning with a
dash must follow the standard `--` option terminator:

```sh
dac add --file artifact.bin -- '-namespace/asset@version' https://example.com/artifact.bin
```

Names never become paths. The separate `file` value must resolve to one safe
filename directly inside `.dac/downloads/`.

`dac.toml` is the human-edited desired state and `dac.lock` records the source
resolution, SHA-256 digest, and size that `dac lock` explicitly accepted.
`pull` never changes either trust decision; it only verifies or restores files
under `.dac/downloads/`.

## Workflow

Use variables for versioned URLs and filenames:

```sh
dac add --set VERSION=3.9.0 --file artifact.ear artifact \
  'https://example.com/artifact-{{.VERSION}}.ear'
dac lock artifact
```

Changing a URL, filename, variable, or pin makes the lock stale. Follow with
`dac lock <asset>` (or `dac lock`) before `dac pull` can run again.

An optional manifest pin limits the bytes lock is allowed to accept:

```sh
# Supply a publisher or independently verified checksum.
dac add --pin=sha256:... artifact https://example.com/artifact.ear

# Or calculate a trust-on-first-use pin. This discards the downloaded bytes;
# `dac lock` downloads independently before it records accepted bytes.
dac add --pin artifact https://example.com/artifact.ear
```

`dac pull --offline` verifies local files without using the network. `dac pull
--force` downloads every locked artifact and still requires each digest to
match. The flags cannot be combined.

## Manifest and authentication

```toml
version = 1

[files.artifact]
url = "https://internal.example/artifact-{{.VERSION}}.ear"
file = "artifact.ear"
pin = "sha256:..."

[files.artifact.variables]
VERSION = "3.9.0"

[files.artifact.headers]
Authorization = "env:ARTIFACT_TOKEN"
X-Repository = "releases"
```

Header values beginning with `env:` are resolved only for a request and are
never written to the lock file or emitted in errors/JSON. Configured headers
are removed if a redirect crosses origin. Do not put secrets in literal header
values or URL query strings: those are persisted user configuration.

Managed downloads are opened relative to the project root. Symlinks may point
elsewhere inside the project, but cannot redirect a download outside it.

CLI edits canonically rewrite `dac.toml`, so comments and custom formatting are
not retained. Commit `dac.toml` and `dac.lock`; normally add `.dac/` to
`.gitignore` because downloads are derived and independently verified.

## Automation and support

Use `--json` for versioned structured success on stdout and structured errors
on stderr. `--quiet` suppresses human success messages and is intentionally
incompatible with `--json`. Exit status `2` denotes invalid invocation or
project configuration; other operational failures exit `1`.

The MVP supports macOS, Linux, DragonFlyBSD, FreeBSD, NetBSD, OpenBSD, and
Illumos, where the included project lock uses `flock(2)`. Other platforms build
but report unsupported locking at runtime.

This repository intentionally resolves `github.com/tomdoesdev/kit` through its
sibling `kit` module. Build and test it from this workspace with `go test ./...`
and `go run ./cmd/dac --help`; standalone module installation is not supported.
