# dac

`dac` (“download a copy”) locks arbitrary HTTP(S) files so a project can later
reproduce exactly the bytes it accepted. It sits between `curl` and a package
manager: it has URLs, optional template variables, authentication headers,
SHA-256 pins, and a lock file—but no registries, dependency graph, cache, or
installation lifecycle.

```sh
dac init
dac add https://example.com/releases/artifact.ear
dac pull --update-lockfile
dac pull
```

`add` accepts one or more URLs and uses each static final path segment as both
the asset name and its filename. For example, a URL ending in `/Curam.ear`
creates an asset named `Curam.ear` in `.dac/downloads/`. The complete batch is
rejected if a filename is unsafe, templated, duplicated, or equivalent on a
case-insensitive or Unicode-normalizing filesystem.

`dac.toml` is the human-edited desired state and `dac.lock` records the source
resolution, SHA-256 digest, and size that `pull --update-lockfile` explicitly
accepted. An ordinary `pull` never changes either trust decision; it only
verifies or restores files under `.dac/downloads/`.

## Workflow

Add a release train before its values are known:

```sh
dac add \
  'https://repo.internal/curam/{{$.VERSION}}/Curam.ear' \
  'https://repo.internal/rest/{{$.VERSION}}/rest.ear' \
  'https://repo.internal/services/{{$.SERVICE_VERSION}}/CuramServices.ear'

dac update --set '$.VERSION=2.0.0' \
  --set '$.SERVICE_VERSION=1.2.2'
dac pull --update-lockfile
```

A template reference declares a variable even before it has a value. `add`
therefore writes unresolved desired state without downloading anything. A bare
`pull` remains a configuration error until every asset resolves and fails
before making a network request.

`{{$.VERSION}}` reads a project variable shared by any number of assets.
`{{.VERSION}}` reads a variable local to one asset. Templates intentionally
support only these direct substitutions; conditionals, pipelines, functions,
and the manifest-v1 `.global.VERSION` spelling are rejected.

`update` has one operation, `--set`. A nameless update accepts only project
variables with the `$.` prefix:

```sh
dac update --set '$.VERSION=4.0.0'
```

A named update accepts only variables local to that asset:

```sh
dac update CuramServices.ear --set VERSION=4.0.0
```

An assignment may change a stored value or provide the first value for an exact
template reference. An unknown, unreferenced key is rejected, as are duplicate
assignments and updates that make no effective change. Project and local
assignments cannot be mixed in one invocation.

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

An optional `pin` in `dac.toml` limits the bytes lock is allowed to accept to a
publisher-provided or independently verified SHA-256 digest.

`dac pull --offline` verifies selected local files without using the network.
`dac pull --force` downloads every selected locked artifact and still requires
each current digest to match. The flags cannot be combined, and `--offline`
also cannot be combined with `--update-lockfile`.

Edit `dac.toml` directly for exceptional URLs, filenames, headers, pins, and
transfer limits. `add` and `update` deliberately do not expose a parallel
general-purpose manifest editing language.

## Manifest and authentication

```toml
version = 2

[globals]
CHANNEL = "stable"

[files.artifact]
url = "https://internal.example/{{$.CHANNEL}}/artifact-{{.VERSION}}.ear"
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

Manifest version 2 replaces the version-1 `{{.global.KEY}}` spelling with
`{{$.KEY}}` and restricts templates to direct placeholders. Migration is
manual: update the version and template spellings in the human-owned manifest;
`dac` never silently rewrites version 1.

Header values beginning with `env:` are resolved only for a request and are
never written to the lock file or emitted in errors/JSON. Configured headers
are removed if a redirect crosses origin. Do not put secrets in literal header
values or URL query strings: those are persisted user configuration.

CLI additions and variable updates canonically rewrite `dac.toml`, so comments
and custom formatting are not retained. Commit `dac.toml` and `dac.lock`;
normally add `.dac/` to `.gitignore` because downloads are derived and
independently verified.

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

Interactive pulls show one byte-progress bar per active asset, including the
declared total, percentage, and transfer rate when the server provides a size.
`SIGINT` and `SIGTERM` cancel the HTTP transfers and stop and join all progress
renderers before `dac` exits, so no download or terminal animation is left
running.

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
