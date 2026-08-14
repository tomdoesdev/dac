# dac

`dac` (“download a copy”) locks arbitrary HTTP(S) files so a project can later
reproduce exactly the bytes it accepted. It sits between `curl` and a package
manager: it has URLs, optional template variables, authentication headers,
SHA-256 pins, and a lock file—but no registries, dependency graph, cache, or
installation lifecycle.

```sh
dac init
dac add artifact https://example.com/releases/artifact.ear
dac pull --update-lockfile
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
resolution, SHA-256 digest, and size that `pull --update-lockfile` explicitly
accepted. An ordinary `pull` never changes either trust decision; it only
verifies or restores files under `.dac/downloads/`.

## Workflow

Use variables for versioned URLs and filenames:

```sh
dac add --set VERSION=3.9.0 --file artifact.ear artifact \
  'https://example.com/artifact-{{.VERSION}}.ear'
dac pull --update-lockfile
```

A variable declared with `--set` belongs to one asset. A value several assets
share — a release train, a mirror host — is a global variable instead, declared
once with `--gset` and referenced through its own namespace:

```sh
dac add --gset VERSION=3.9.0 --file 'artifact-{{.global.VERSION}}.ear' artifact \
  'https://example.com/artifact-{{.global.VERSION}}.ear'
dac add --file 'plugin-{{.global.VERSION}}.jar' plugin \
  'https://example.com/plugin-{{.global.VERSION}}.jar'
```

The two scopes never merge. `{{.VERSION}}` reads the asset's own variables and
`{{.global.VERSION}}` reads the project's globals, so a template always states
where its value came from and one asset may use both keys at once. Referencing
an undefined global fails to resolve rather than rendering an empty string.

Because a global is shared, `--gset` defines it and nothing more: redefining an
existing global is an error on both `add` and `update`, since a silent rebind
would move every asset that references it. Change one by editing `dac.toml`,
which makes the affected assets' locks stale like any other source change.

Changing a URL, filename, variable, header, pin, or transfer policy makes that
asset's lock entry stale. Run `dac pull <asset> --update-lockfile` to accept and
install new upstream bytes for only that asset, or use a bare `dac pull
--update-lockfile` to refresh every stale asset and remove lock-only entries.
The same command creates `dac.lock` when it is absent. A scoped initial update
creates a partial lock, so unrelated assets remain stale until they are updated.

Both ordinary and updating pulls accept one or more asset names. A scoped pull
validates, downloads, and reports only those assets; stale unselected entries
and lock-only entries do not block it. Only a bare pull requires the complete
lock to match the manifest exactly, and only a bare updating pull cleans up
lock-only entries and their formerly managed downloads.

An optional manifest pin limits the bytes lock is allowed to accept:

```sh
# Supply a publisher or independently verified checksum.
dac add --pin=sha256:... artifact https://example.com/artifact.ear

# Or calculate a trust-on-first-use pin. This discards the downloaded bytes;
# The updating pull downloads independently before it records accepted bytes.
dac add --pin artifact https://example.com/artifact.ear
```

`dac pull --offline` verifies selected local files without using the network.
`dac pull --force` downloads every selected locked artifact and still requires
each current digest to match. The flags cannot be combined, and `--offline`
also cannot be combined with `--update-lockfile`.

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

[globals]
CHANNEL = "stable"

[files.artifact]
url = "https://internal.example/{{.global.CHANNEL}}/artifact-{{.VERSION}}.ear"
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

CLI edits canonically rewrite `dac.toml`, so comments and custom formatting are
not retained. Commit `dac.toml` and `dac.lock`; normally add `.dac/` to
`.gitignore` because downloads are derived and independently verified.

## Transfer

`dac` asks for the first 16 MiB of an asset as an HTTP byte range. A host that
supports ranges then serves the rest the same way, one chunk at a time on the
same connection, so a connection lost part-way through a large artifact costs
the current chunk instead of every byte already transferred: the next request
resumes at the first byte `dac` has not accepted. Each chunk carries the entity
tag or modification date the first response returned, so an asset that changes
mid-transfer is rejected rather than stitched together from two versions. A
host that ignores or rejects the range serves the asset in one response, exactly
as before, and nothing about the result differs — the digest and size recorded
in `dac.lock` are of the assembled bytes either way.

`pull` transfers several assets at the same time, four by default. Use `--jobs`
(`-j`) to choose another number between 1 and 16; `--jobs 1` gives a host
exactly one request at a time. Concurrency is between assets only: one asset is
still one ordered sequence of chunks on one connection, and the transfers
never share a destination file. Whatever order they finish in, `dac` reports
results, writes `dac.lock` when requested, and reports a failure in manifest
order. The first failure stops the transfers still running. When accepted state
is changing, every selected download and `dac.lock` commit or roll back as one
transaction.

Response compression is not negotiated. Byte ranges, digests, and size limits
are all expressed in the asset's own bytes, and a content encoding applied in
transit would put a different sequence of bytes between them.

Every asset has an effective maximum response size and idle body-read timeout.
The defaults are 4 GiB and 30 seconds. `max_size` accepts integral byte values,
decimal units (`MB`, `GB`) and IEC units (`MiB`, `GiB`); `idle_timeout` uses Go
duration syntax. Set either value to `"0"` to disable that limit. The idle
timeout applies only while waiting for response-body progress, not while writing
bytes locally, and neither setting is a total transfer deadline.

Managed downloads are opened relative to the project root. Symlinks may point
elsewhere inside the project, but cannot redirect a download outside it.

## Automation and support

Use `--json` for versioned structured success on stdout and structured errors
on stderr. `--quiet` suppresses human success messages and is intentionally
incompatible with `--json`. Exit status `2` denotes invalid invocation or
project configuration; other operational failures exit `1`.

Lock format version 2 fingerprints the complete non-secret source policy. Lock
version 1 and malformed or invalid lockfiles are intentionally rejected by
pull, including `--update-lockfile`; repair or remove an invalid lockfile before
rebuilding it with `dac pull --update-lockfile`.

The MVP supports macOS, Linux, DragonFlyBSD, FreeBSD, NetBSD, OpenBSD, and
Illumos, where the included project lock uses `flock(2)`. Other platforms build
but report unsupported locking at runtime.

This repository intentionally resolves `github.com/tomdoesdev/kit` through its
sibling `kit` module. Run `mise check` for vet plus race-enabled tests across
both modules, or invoke `go vet ./... ./kit/...` and `go test -race ./...
./kit/...` directly. Use `go run ./cmd/dac --help` during development;
standalone module installation is not supported.
