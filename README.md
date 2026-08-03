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
dac add backend-app/geo-database@2026.08 \
  --source https://example.com/geo/2026.08/database.bin --pin
dac pull
geo_database="$(dac path backend-app/geo-database@2026.08)"
```

An asset is named by a coordinate: `<namespace>/<name>@<version>`. All three
parts are required, and together they are the whole identity of the asset. A
namespace lets `backend-app/database` and `frontend-app/database` be two
different files rather than an argument about who owns the name.

`add` resolves the new asset and updates both project files, creating the lock
file if the project does not have one yet. Commit `dac.json` and
`dac-lock.json`. A failed resolution changes neither file.

`--pin` records the digest the asset resolved to as its `integrity` value, so
every later command holds the publisher to the bytes this `add` saw. Without it
the lock file still pins the bytes; the manifest just does not say so.

DAC returns a path and stops there. Extraction belongs to whatever consumes it:

```bash
tar -xzf "$(dac path tools/toolchain@1.4)" -C vendor/toolchain
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
`dac pull --refresh-lock` to resolve every asset against its origin instead. An
origin that has replaced the bytes a version is locked to stops that pull rather
than rewriting the lock file: see [What a version means](#what-a-version-means).

To find out whether an origin has moved without changing what your project uses,
run `dac verify --refresh` on a schedule. It resolves every asset, writes
nothing, and exits `1` with the code `lock_drift` and the drifted asset names
when the origins no longer serve the locked bytes. Resolving an asset means
downloading it, so a refresh warms the cache on its way past.

## Commands

| Command | Result |
|---|---|
| `dac init [--force]` | Create matching empty project files. |
| `dac add <coordinate> --source <url> [--pin] [--integrity <digest>] [--force] [--rebind] [--allow-insecure-http] [--offline] [--no-rewrite]` | Add one asset version. Resolve it unless offline mode is active. |
| `dac remove <coordinate>` | Remove one asset version without network access. |
| `dac info [<namespace>/<name>[@<version>]]` | Show asset, request, lock, and cache information. |
| `dac pull [--update-lock] [--refresh-lock] [--rebind] [--offline] [--distdir <dir>] [--no-rewrite]` | Download missing locked assets, updating the lock file when asked. |
| `dac path <coordinate>` | Return a verified cache path. |
| `dac verify [--refresh]` | Check that the manifest and lock file agree, and with `--refresh` that the origins still serve the locked bytes. |
| `dac export --file <tar>` | Write locked objects and metadata to a cache bundle. |
| `dac import --file <tar>` | Install objects from a cache bundle into the local cache. |
| `dac cache verify [--all] [--repair]` | Hash cache objects and report the ones that no longer match. |
| `dac cache gc [--max-age <age>] [--dry-run]` | Remove cache objects that nothing has used recently. |
| `dac cache dir` | Print the resolved cache directory. |
| `dac completion <shell>` | Write a shell completion script. |

A project holds as many versions of an asset as it names. The `add`, `remove`,
and `path` commands require exactly one complete coordinate; a bare name would
work until somebody added a second version, which is the worst moment for a
command to start guessing. `info` is the exception, because answering what a
project has is its job: it accepts nothing, one coordinate, or one
`<namespace>/<name>` whose versions it should list.

A namespace and a name are lowercase letters, digits, and `.`, `_`, or `-`. A
version also takes uppercase and `+`, because it is copied from whatever the
publisher calls a release. Every part starts and ends alphanumeric.

`info` does not use the network. It reports manifest and request information
when the lock is missing or stale. Lock and cache information is unavailable in
that state.

Use `dac --help`, `dac <command> --help`, and `dac --version` for CLI
help. DAC has no command aliases.

### What a version means

A version is part of an asset's identity, not a field describing it. Two rules
follow from that, and between them they are the whole of what DAC knows about
versions:

**A locked coordinate always names the same bytes.** Once the lock file binds
`backend-app/database@1.0.0` to a digest, nothing rewrites that digest. An
origin that replaced what it serves behind a stable URL and a manifest edited to
point one version somewhere else both fail with `version_rebind`, which reports
the digest the lock holds and the digest that arrived. The way forward is a new
version. `--rebind` is for the project that genuinely tracks a rolling source
and has decided to accept the change; it writes the lock file, so it only works
alongside `--update-lock` or `--refresh-lock`.

**Two versions of one asset never name the same bytes.** Adding
`backend-app/database@2.0.0` with the source `backend-app/database@1.0.0`
already had resolves to bytes
that already have a version, and fails with `version_collision`. Editing the
version in place fails the same way: the old coordinate leaves the manifest, so
the two never appear together, but the previous lock file still remembers what
it named.

Neither rule reads anything but the project's own two files. DAC does not order
versions, compare them, parse them, or ask an origin what exists — deciding
which version to use is a package manager's job, and this is only about the
version you already wrote down meaning what it says.

Two versions of one asset served from one URL is not refused. Only one set of
bytes is at a URL, so a cold cache can restore only one of them, and `add`
reports that rather than deciding it for you.

Version 6 made a coordinate `<namespace>/<name>@<version>` and moved it into the
key of both project files, which is what lets a project hold several versions of
an asset. Manifest and lock version 2 have no `version` field: it would be a
second place to say what the key already says, and the place the two could
disagree. There is no migration. A version 1 project file is rejected with a
message naming the shape the new one takes.

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
  "schemaVersion": 2,
  "assets": {
    "backend-app/geo-database@2026.08": {
      "url": "https://example.com/geo/2026.08/database.bin",
      "integrity": "sha256:optional-publisher-digest",
      "allowInsecureHttp": false
    },
    "backend-app/geo-database@2026.09": {
      "url": "https://example.com/geo/2026.09/database.bin"
    }
  }
}
```

`dac-lock.json` records resolved bytes:

```json
{
  "lockVersion": 2,
  "manifestDigest": "sha256:...",
  "assets": {
    "backend-app/geo-database@2026.08": {
      "url": "https://example.com/geo/2026.08/database.bin",
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
| `--download-parts` | `DAC_DOWNLOAD_PARTS` | `4` | `add`, `pull`, `verify` |
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
sizes. Raise it for a project with genuinely larger assets. `--max-size none`
removes the bound, and nothing else does: an empty value, a zero, and a count
too large for DAC to hold are all rejected rather than read as no limit.

`--download-parts` sets how many requests one download may be split across. See
[Split downloads](#split-downloads). It is a budget for the whole command rather
than a per-asset multiplier, so raising it speeds up a project of one large asset
without opening `--concurrency` times as many connections for a project of many.
Set it to `1` to send one request per asset.

## Split downloads

DAC finishes one large download over several parallel requests when the origin
serves byte ranges. The first response carries the first 8MiB, and the rest of
the asset arrives as `Range` requests for the 8MiB pieces after it, so splitting
costs no extra round trip. A fixed piece size rather than a share of the asset is
what bounds the cost: bytes are hashed in order, so a piece that arrives early
waits its turn in memory, and only a few are ever in flight.

Three conditions have to hold, and DAC streams the single response it already has
whenever one does not:

- `--download-parts` is more than `1` and the command has a part to spare.
- The response says `Accept-Ranges: bytes` and is at least 16MiB.
- The response carries a strong `ETag` or a `Last-Modified` date.

The last condition is the one worth explaining. A split download reads one asset
over several requests, so it has to be sure every request answers about the same
bytes. DAC replays the validator as an `If-Match` or `If-Unmodified-Since`
precondition, which turns a publisher who replaced the asset mid-download into a
`412` rather than a file assembled from two versions. Without a validator to
build that precondition from, DAC does not split at all.

DAC also checks the `Content-Range` of every piece against the range it asked
for. A pinned asset's digest would catch a misassembled file in the end, but it
could only report that the bytes were wrong, and the reason they were wrong is
worth naming. A piece that fails is retried on its own, under the same
`--retries` and backoff as a whole request: its bytes have not reached the hash
yet, so the retry costs one piece rather than the asset.

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

DAC asks a helper once per URL per transfer, and keeps that answer for the range
requests a split download is made of rather than starting the helper again for
each one. If an origin then answers `401` or `403`, DAC forgets what it was
holding, asks once more, and retries the request; a second rejection fails the
command.

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
{"outputVersion":3,"ok":true,"command":"path","data":{}}
```

`info` always returns an `assets` array in JSON mode, alongside a `summary`
object holding the counts. A coordinate filters the array to one item, and a
`<namespace>/<name>` filters it to that asset's versions. A missing
or stale lock sets `cacheStatus` to `unavailable` and omits digest, size, and
path data; a damaged object sets it to `corrupt`.

JSON errors use the same stream and framing:

```json
{"outputVersion":3,"ok":false,"command":"pull","error":{"code":"network_error","message":"The asset request failed.","cause":"https://example.com/db: unconditional request returned HTTP 404","details":{"asset":"backend-app/geo@1.0.0","status":404,"url":"https://example.com/db"}}}
```

`code` and `message` are stable enough to branch on, which is exactly why
neither says anything specific about a failure. `cause` carries the part an
operator acts on, and `details` repeats the useful parts of it as data: `url`
and `status` for a request failure, expected and actual digests for a content
failure, the locked and resolved digests for a `version_rebind`, and the two
versions and the object they share for a `version_collision`.

Human errors, help, and progress go to standard error. JSON mode does not write
human summaries or error messages. The `pull` command also disables progress in
JSON mode.

Output version `3` gave every asset a `coordinate` and a `namespace` alongside
its `name` and `version`, dropped `replaced` from an `add` result because adding
a version no longer retires one, and added `siblings`, `sharedSources`, and
`remaining` so that a command changing one version says what happened to the
others. Output version `2` moved the `info` counters into `summary` and dropped
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

The cache is keyed by digest and nothing else, so several versions of an asset
sit in it side by side with no arrangement needed, and two assets that resolved
to the same bytes cost one object between them.

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
index lists each asset coordinate, source URL, file path, digest, and size
directly.

The index names each asset by its whole coordinate. Two assets that resolved to
one object share one blob, which is what makes a bundle for a project that
vendors the same file under two namespaces no larger than one that does not.

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

DAC does not order or compare versions. It has no idea whether `10` follows `9`,
or whether either is a number, and it never chooses a version for you: there are
no ranges, no constraints, no `latest`, and no dependencies between assets.
Choosing versions is what a package manager does. DAC only guarantees that the
version you wrote down is the one you get.

A namespace is a grouping label and nothing else. Nothing owns one, nothing
registers one, and two projects using the same namespace is not a conflict.

DAC does not track upstream releases. `dac verify --refresh` reports that an
origin changed what it serves at a URL you already have; nothing tells you that
version `2026.09` exists. Bumping a version stays a manual edit.
