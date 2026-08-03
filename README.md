# DAC

DAC locks remote files and keeps them in a local content-addressed cache. It is
small by design. It gives deployment and infrastructure tools one stable way to
fetch data files, policy bundles, provider binaries, and similar build inputs.

DAC does not unpack or install assets. It returns the verified cache path. The
calling script decides how to use that path.

## Install

DAC requires Go 1.26 and targets Unix. The cache coordinates concurrent
processes with `flock`, so there is no Windows build.

```bash
go install github.com/tom/dac/cmd/dac@latest
```

To build from a checkout instead:

```bash
mise run build
```

The command writes `bin/dac`.

## Quick start

```bash
dac init
dac add geo-database@2026.08 \
  --source https://example.com/geo/2026.08/database.bin --pin
dac pull
geo_database="$(dac path geo-database@2026.08)"
```

`add` resolves the new asset and updates both project files, creating the lock
file if the project does not have one yet. Commit `dac.json` and
`dac-lock.json`. A failed resolution changes neither file.

`--pin` records the digest the asset resolved to as its `integrity` value, so
every later command holds the publisher to the bytes this `add` saw. Without it
the lock file still pins the bytes; the manifest just does not say so.

DAC returns a path and stops there. Extraction belongs to whatever consumes it:

```bash
tar -xzf "$(dac path toolchain@1.4)" -C vendor/toolchain
```

Use `add --offline` to update only `dac.json`. The command makes no network
request and does not change `dac-lock.json`. Run `dac pull --update-lock` later
to update it.

In CI or a deployment job, run:

```bash
dac pull
```

A plain `pull` refuses a lock file that does not describe the manifest, so it
reproduces the project as committed rather than as the manifest now reads. It
uses a matching cache object without a network request.

Editing `dac.json` by hand is the other way in. Nothing resolves it until you
ask:

```bash
dac pull --update-lock
```

That resolves the assets the lock file does not describe, writes it, and names
what it locked. An asset the lock already describes costs no request at all. Use
`dac pull --refresh-lock` to resolve every asset against its origin instead,
which is how you pick up bytes that moved behind a stable URL.

To find out whether an origin has moved without changing what your project uses,
run `dac verify --refresh` on a schedule. It resolves every asset, writes
nothing, and exits `1` with the code `lock_drift` and the drifted asset names
when the origins no longer serve the locked bytes. Resolving an asset means
downloading it, so a refresh warms the cache on its way past.

## Commands

| Command | Result |
|---|---|
| `dac init [--force]` | Create matching empty project files. |
| `dac add <name>@<version> --source <url> [--pin] [--integrity <digest>] [--force] [--allow-insecure-http] [--offline] [--no-rewrite]` | Add one asset. Resolve it unless offline mode is active. |
| `dac remove <name>@<version>` | Remove one asset without network access. |
| `dac info [<name>@<version>]` | Show asset, request, lock, and cache information. |
| `dac pull [--update-lock] [--refresh-lock] [--offline] [--distdir <dir>] [--no-rewrite]` | Download missing locked assets, updating the lock file when asked. |
| `dac path <name>@<version>` | Return a verified cache path. |
| `dac verify [--refresh]` | Check that the manifest and lock file agree, and with `--refresh` that the origins still serve the locked bytes. |
| `dac export --file <tar>` | Write locked objects and metadata to a cache bundle. |
| `dac import --file <tar>` | Install objects from a cache bundle into the local cache. |
| `dac cache verify [--all] [--repair]` | Hash cache objects and report the ones that no longer match. |
| `dac cache gc [--max-age <age>] [--dry-run]` | Remove cache objects that nothing has used recently. |
| `dac cache dir` | Print the resolved cache directory. |
| `dac completion <shell>` | Write a shell completion script. |

DAC accepts one active version for each asset name. The `add`, `remove`, and
`path` commands require exactly one `name@version` coordinate. The `info`
command accepts zero or one coordinate. Names and versions must not be empty or
contain another `@`.

`info` does not use the network. It reports manifest and request information
when the lock is missing or stale. Lock and cache information is unavailable in
that state.

Use `dac --help`, `dac <command> --help`, and `dac --version` for CLI
help. DAC has no command aliases.

Version 5 removed `dac lock`. Resolving an asset stores its bytes in the cache,
so the command that resolved everything and the command that installed
everything were the same command under two names. `dac lock` is now
`dac pull --update-lock`, `dac lock --refresh` is `dac pull --refresh-lock`, and
`dac lock --check` is `dac verify --refresh`. Nothing writes the lock file
without being asked: `add` and `remove` maintain it because they are already
changing the project, and `pull` only when a lock flag says so.

`export`, `import`, and `cache gc` do not use the network. `import` does not
read the project files. `cache gc` collects the whole shared cache.

## Project files

`dac.json` records source intent:

```json
{
  "schemaVersion": 1,
  "assets": {
    "geo-database": {
      "version": "2026.08",
      "url": "https://example.com/database.bin",
      "integrity": "sha256:optional-publisher-digest",
      "allowInsecureHttp": false
    }
  }
}
```

`dac-lock.json` records resolved bytes:

```json
{
  "lockVersion": 1,
  "manifestDigest": "sha256:...",
  "assets": {
    "geo-database": {
      "version": "2026.08",
      "url": "https://example.com/database.bin",
      "digest": "sha256:...",
      "size": 123
    }
  }
}
```

The `integrity` field is an optional publisher SHA-256 digest. DAC reads it in
either the canonical `sha256:<hex>` form or the Subresource Integrity
`sha256-<base64>` form, and always writes the canonical form. DAC requires
HTTPS. It permits HTTP for loopback hosts. Use `--allow-insecure-http` on
`add` to permit another HTTP source.

An asset with an `integrity` value that the cache already holds lets an update
finish without a request, which also means it never confirms that the URL still
serves those bytes. Use `dac pull --refresh-lock` to check every asset against
its origin.

An asset the manifest leaves unpinned records an `etag` when the origin sends
one. A refresh replays it as an `If-None-Match` hint and skips the download on a
`304`: an origin that answers `304` has confirmed the asset just as well as one
that sends the bytes again. A pinned asset neither sends nor records an ETag, so
its lock entry omits the field.

DAC rejects unknown JSON fields, duplicate keys, unsupported schema versions,
and stale lock files. It writes project files with atomic renames.

## Options

Global options:

| Option | Environment | Default |
|---|---|---|
| `--manifest` | | `dac.json` |
| `--lock` | | `dac-lock.json` |
| `--cache-dir` | `DAC_CACHE_DIR` | Local XDG cache |
| `--json`, `-j` | | `false` |

Request and policy options appear only on commands that use them:

| Option | Environment | Default | Commands |
|---|---|---|---|
| `--timeout` | `DAC_TIMEOUT` | `5m` | `add`, `pull`, `verify` |
| `--retries` | `DAC_RETRIES` | `2` | `add`, `pull`, `verify` |
| `--concurrency` | `DAC_CONCURRENCY` | `4` | `pull`, `verify` |
| `--max-size` | `DAC_MAX_SIZE` | `2GiB` | `add`, `pull`, `verify` |
| `--progress` | | `true` | `add`, `pull`, `verify` |
| `--no-rewrite` | | `false` | `add`, `pull`, `verify` |
| `--credential-helper` | `DAC_CREDENTIAL_HELPER` | | `add`, `pull`, `verify` |
| `--distdir` | `DAC_DISTDIR` | | `pull` |

The `verify` options apply only to `--refresh`. A plain `verify` reads the two
project files and stops.

`--timeout` is an inactivity timeout. DAC retries transient network failures.
It requests identity encoding and checks redirects with the same URL policy.

`--max-size` bounds a response whose size DAC does not know ahead of time, so it
exists to stop a runaway stream rather than to express a policy about asset
sizes. Raise it for a project with genuinely larger assets.

## Credentials

DAC gets request credentials only from a credential helper. Pass
`--credential-helper <command>` for all hosts. Pass
`--credential-helper <host>=<command>` for one host. A host-specific helper
wins over a general helper.

DAC runs the helper as `<command> get`, writes one request to its standard
input, and reads its headers from standard output:

```json
{"uri":"https://files.example.com/geo/database.bin"}
```

```json
{"headers":{"Authorization":["Bearer ..."]}}
```

A helper has 30 seconds to answer. A failing helper fails the command. DAC never
writes helper output to a log or an error message. DAC clears credentials on
each redirect and asks again for the new host, so one host's credentials never
reach another.

## Rewrite config

A manifest records where an asset comes from upstream. A rewrite config decides
where DAC actually sends the request, so a site that proxies its downloads does
not have to edit every project that uses them. The manifest and the lock file
keep the canonical URL.

```text
# Block each host that no allow rule permits.
block *

# Permit requests to this host.
allow releases.internal

# Send vendor requests to the mirror.
rewrite ^vendor\.example\.com/(.*)$ https://mirror.internal/vendor/$1

# Permit requests to the rewritten host.
allow mirror.internal
```

Save the config as `dac-rewrite.cfg` beside the manifest. DAC loads this file
automatically when it exists. `DAC_REWRITE_CONFIG` can specify a different
file, and that file must exist. Use `--no-rewrite` with `add`, `pull`, or
`verify --refresh` to disable the complete config.

Run `dac info` to show each source URL, request URL, and host policy result. Add
an asset coordinate to show only that asset.

A `rewrite` pattern is a Go regular expression. It matches `host/path` with any
query string appended. Its replacement can use `$1` groups. A replacement
without a scheme keeps the original URL scheme. A URL fails when multiple
rewrite rules match it.

DAC applies one rewrite before it checks `allow` and `block` rules. An `allow`
match overrides all `block` matches. Directive order does not change this host
policy. A host with no match is allowed. The `*` pattern matches every host.
Other patterns match a host and its subdomains.

Redirect targets use the same host policy, but DAC does not rewrite them. Add a
bare `allow_insecure_http` directive to permit plain HTTP for rewritten URLs.

A rewrite cannot change what DAC accepts: `pull` still checks the locked digest,
so a mirror that serves the wrong bytes fails exactly as a corrupted transfer
does. Resolving an asset with no `integrity` value has no expected digest yet,
so a rewrite config is trusted input in the same way a manifest URL is.

## Output contract

DAC writes human-readable command results to standard output by default.
For example, `path` writes only the verified cache path. `info` writes one
detailed block for each selected asset.

Use `--json` or `-j` to write one versioned JSON document to standard output:

```json
{"outputVersion":2,"ok":true,"command":"path","data":{}}
```

`info` always returns an `assets` array in JSON mode, alongside a `summary`
object holding the counts. A coordinate filters the array to one item. A missing
or stale lock sets `cacheStatus` to `unavailable` and omits digest, size, and
path data; a damaged object sets it to `corrupt`.

JSON errors use the same stream and framing:

```json
{"outputVersion":2,"ok":false,"command":"pull","error":{"code":"network_error","message":"The asset request failed.","cause":"https://example.com/db: unconditional request returned HTTP 404","details":{"asset":"geo","status":404,"url":"https://example.com/db"}}}
```

`code` and `message` are stable enough to branch on, which is exactly why
neither says anything specific about a failure. `cause` carries the part an
operator acts on, and `details` repeats the useful parts of it as data: `url`
and `status` for a request failure, expected and actual digests for a content
failure.

Human errors, help, and progress go to standard error. JSON mode does not write
human summaries or error messages. The `pull` command also disables progress in
JSON mode.

Output version `2` moved the `info` counters into `summary` and dropped
`allowedCount`, and added `cause` to every error.

Exit status `0` means success. Status `1` means that the command failed.
Status `2` means that DAC rejected the arguments.

On an interactive terminal, DAC uses concurrent progress bars from
[`mpb`](https://github.com/vbauerster/mpb). Each bar shows the asset name,
bytes, percentage, speed, and completion state. An unknown response size uses a
spinner and a byte count. A non-terminal stream gets one start line and one
completion line for each asset. Use `--progress=false` to disable both forms.
The `pull` command disables both forms in JSON mode.

## Cache behavior

DAC stores each object and a small sidecar describing it at:

```text
<cache>/blobs/sha256/<digest>
<cache>/blobs/sha256/<digest>.meta
```

A download first enters a temporary file. DAC hashes and checks all bytes
before one atomic rename installs the object. Each digest has a Unix process
lock. Concurrent DAC processes cannot install the same object at the same time.

An update can use a publisher digest or a cached object before it makes a
request. It uses a stored ETag as a request hint, and only for an asset with no
`integrity` value. A publisher digest already settles which bytes are correct,
so a pinned asset is answered from the cache or downloaded outright, and its
lock entry carries no `etag`.

A content check that fails reports the digest DAC received alongside the one it
expected, in the error cause and in the JSON `details`. The two cases that
produces are worth telling apart: a corrupted transfer, and a publisher who
replaced the bytes behind a stable URL.

### What a cache object is trusted for

A content-addressed name is a claim about content, so the question a cache has
to answer is whether the object still holds the bytes DAC installed. Hashing it
to find out is not free: an asset can be gigabytes, and `dac path` runs in the
middle of a build.

So each object carries a sidecar recording the size and modification time it had
at install. A stat that still matches means nothing has written to the object,
and DAC uses it without reading it. Anything else — an object edited in place, a
truncated one, a restored backup, a cache written by an older DAC — costs one
hash, after which the object is either recorded as good or reported as corrupt.
That check runs on every command that reaches the cache.

The object's own timestamp is the evidence, so DAC never touches it after
install. The sidecar carries the liveness signal that collection uses instead.

What this does not cover is storage that changes bytes without changing the
stat. `dac cache verify` answers that by hashing every object it checks, in
exchange for reading all of them:

```bash
dac cache verify              # the objects this project locked
dac cache verify --all        # every object in the shared cache
dac cache verify --repair     # remove the ones that fail
```

A corrupt object makes the command exit `1` with the code `cache_object_corrupt`
unless `--repair` removes it. Corrupt objects are worth nothing, and `dac pull`
replaces one by downloading it again, so `--repair` followed by `pull` restores
a damaged cache.

Damage is reported wherever it turns up. `dac path` and `dac export` refuse,
because neither can do anything about it — and a bundle is the one artifact that
carries cache damage onto machines that cannot tell where it came from. `dac
info` reports `cache: corrupt`. `dac pull` downloads the asset again and
installs good bytes over the bad ones, reporting the asset as `repaired`.

### Collection

Every cache hit refreshes the object's sidecar timestamp, because age is the
only liveness signal a content-addressed store has. `dac cache gc` removes
objects that no project has used within `--max-age`, which accepts `30d`, `2w`,
or any Go duration, and defaults to `30d`. It also removes temporary files left
behind by an interrupted download, and sidecars whose object is gone. Use
`--dry-run` to see what it would remove.

Collection never removes a digest lock file. Unlinking a lock that another
process holds would let a later process take the same lock through a new inode.

### Cache bundles

`dac export --file <tar>` writes every locked object to one tar bundle. `dac
import --file <tar>` validates the bundle and installs its objects in the local
cache. This supports a cold cache on an isolated machine.

```bash
dac export --file ./cache.tar      # on a machine with network access
dac import --file ./cache.tar      # on an isolated machine
```

The format uses two ideas from the OCI image layout. The tar root contains an
`index.json` file. Object bytes use `blobs/sha256/<hex>` paths. The simpler DAC
index lists each asset name, version, source URL, file path, digest, and size
directly.

`export` hashes each object while it writes the tar. `import` checks the index,
entry type, path, size, and digest. It rejects an invalid bundle before it
installs the applicable object.

`dac pull --distdir <dir>` still accepts an unpacked distribution directory.
Each file in that directory must use its SHA-256 hexadecimal digest as its name.

## Non-goals

DAC does not unpack, install, or run anything. It hosts no mirror or remote
cache, resumes no partial download, and records no signature or provenance. It
has no registry and no plugin language. Extraction and installation belong to
whatever consumes the path that `dac path` returns.

DAC does not track upstream releases. `dac verify --refresh` reports that an
origin changed what it serves at a URL you already have; nothing tells you that
version `2026.09` exists. Bumping a version stays a manual edit, and because DAC keeps
one version of each asset name, `dac add --force` at a new version retires the
old coordinate and says so.
