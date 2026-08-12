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
dac lock
```

Changing a URL, filename, variable, header, pin, or transfer policy makes the
lock stale. Follow with `dac lock <asset>` before `dac pull` can run again. A
bare `dac lock` creates the initial lock from every manifest asset and fails if
`dac.lock` already exists. Use one or more asset names for a targeted update,
or `dac lock --all` to replace the entire lock file. A targeted lock requires an
existing current lock for every asset it retains.

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

Use `dac status` to inspect desired, locked, and downloaded state without using
the network. It reports assets as `stale`, `missing`, `invalid`, or `verified`,
and reports lock-only assets and unreferenced download entries as `orphaned`.
Status is observational and exits successfully for a valid report; use `dac
pull --offline` when automation needs a failing verification gate.

`update` can change the complete desired source policy without editing TOML:

```sh
dac update artifact --url 'https://example.com/artifact-{{.VERSION}}.ear' \
  --file 'artifact-{{.VERSION}}.ear' --set VERSION=4.0.0 \
  --unset OLD_VERSION --header Authorization=env:ARTIFACT_TOKEN \
  --remove-header X-Old-Repository --max-size 2GiB --idle-timeout 1m
```

Unknown removals, conflicting edits, and updates that make no effective change
are rejected so automation cannot silently hide a typo.

## Manifest and authentication

```toml
version = 1

[files.artifact]
url = "https://internal.example/artifact-{{.VERSION}}.ear"
file = "artifact.ear"
pin = "sha256:..."
max_size = "4GiB"
idle_timeout = "30s"

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

Every asset has an effective maximum response size and idle body-read timeout.
The defaults are 4 GiB and 30 seconds. `max_size` accepts integral byte values,
decimal units (`MB`, `GB`) and IEC units (`MiB`, `GiB`); `idle_timeout` uses Go
duration syntax. Set either value to `"0"` to disable that limit. The idle
timeout applies only while waiting for response-body progress, not while writing
bytes locally, and neither setting is a total transfer deadline.

Managed downloads are opened relative to the project root. Symlinks may point
elsewhere inside the project, but cannot redirect a download outside it.

CLI edits canonically rewrite `dac.toml`, so comments and custom formatting are
not retained. Commit `dac.toml` and `dac.lock`; normally add `.dac/` to
`.gitignore` because downloads are derived and independently verified.

## Automation and support

Use `--json` for versioned structured success on stdout and structured errors
on stderr. Status JSON contains ordered `assets` and `orphans` arrays. `--quiet`
suppresses human success messages and is intentionally incompatible with
`--json`. Exit status `2` denotes invalid invocation or project configuration;
other operational failures exit `1`.

Lock format version 2 fingerprints the complete non-secret source policy. Lock
version 1 is intentionally rejected by pull, status, and targeted lock; replace
it explicitly with `dac lock --all`.

The MVP supports macOS, Linux, DragonFlyBSD, FreeBSD, NetBSD, OpenBSD, and
Illumos, where the included project lock uses `flock(2)`. Other platforms build
but report unsupported locking at runtime.

This repository intentionally resolves `github.com/tomdoesdev/kit` through its
sibling `kit` module. Run `mise check` for vet plus race-enabled tests across
both modules, or invoke `go vet ./... ./kit/...` and `go test -race ./...
./kit/...` directly. Use `go run ./cmd/dac --help` during development;
standalone module installation is not supported.
