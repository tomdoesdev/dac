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
go install github.com/tomdoesdev/dac/cmd/dac@latest
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
  https://example.com/geo/2026.08/database.bin --pin
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
request and does not change `dac-lock.json`. Run `dac lock` later to update it.

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
dac lock
```

That resolves the assets the lock file does not describe, writes it, and names
what it locked. An asset the lock already describes costs no request at all. Use
`dac lock --refresh` to resolve every asset against its origin instead. An
origin that has replaced the bytes a version is locked to stops the command
rather than rewriting the lock file: see
[What a version means](#what-a-version-means).

`dac lock` is the only command whose product is the lock file. It writes that
file and nothing else, and `pull` installs and writes nothing at all, so the two
halves of "what does this project use" and "fetch what it uses" stay separate
decisions. Resolving an asset downloads it, so a lock warms the cache for the
assets it settles; the pull behind it then has nothing left to fetch.

To find out whether an origin has moved without changing what your project uses,
run `dac verify --refresh` on a schedule. It resolves every asset, writes
nothing, and exits `1` with the code `lock_drift` and the drifted asset names
when the origins no longer serve the locked bytes. Resolving an asset means
downloading it, so a refresh warms the cache on its way past.

## Commands

| Command | Result |
|---|---|
| `dac init [--force]` | Create matching empty project files. |
| `dac add <coordinate> <url> [--pin] [--integrity <digest>] [--force] [--rebind] [--allow-insecure-http] [--offline] [--no-rewrite]` | Add one asset version. Resolve it unless offline mode is active. |
| `dac remove <coordinate>` | Remove one asset version without network access. |
| `dac info [<namespace>/<name>[@<version>]]` | Show asset, request, lock, and cache information. |
| `dac lock [--refresh] [--rebind] [--concurrency <n>] [--no-rewrite]` | Resolve the manifest assets the lock file does not describe and write it. |
| `dac pull [<namespace>/<name>[@<version>]...] [--offline] [--concurrency <n>] [--no-rewrite]` | Download missing locked assets, or the ones named. |
| `dac path <namespace>/<name>[@<version>]` | Return a verified cache path. The version may be left off when the project holds one. |
| `dac verify [--refresh] [--concurrency <n>]` | Check that the manifest and lock file agree, and with `--refresh` that the origins still serve the locked bytes. |
| `dac pack [<archive>]` | Write locked assets to a dacpack under the names their origins give them. |
| `dac unpack [<archive> [<directory>]] [--force]` | Write the assets a dacpack carries into a directory. |
| `dac cache dir` | Print the resolved cache directory. |
| `dac cache list [--all]` | List cached objects with their size, last use, and the assets they belong to. |
| `dac cache import <dacpack\|directory>` | Install objects from a dacpack, or from a directory of digest-named files. |
| `dac cache gc [--max-age <age>] [--max-size <size>] [--dry-run]` | Remove cache objects that nothing has used recently, then evict until the cache fits. |
| `dac cache clear [--dry-run]` | Remove every cache object. |
| `dac cache remove <coordinate>... [--force]` | Remove the objects specific asset versions resolved to. |
| `dac cache scrub [--all] [--repair]` | Hash cache objects and report the ones that no longer match. |
| `dac config path` | Print the config files DAC read, most important first. |
| `dac config show` | Print the effective configuration as a config file. |
| `dac completion <shell>` | Write a shell completion script. |

A project holds as many versions of an asset as it names, so how much of a
coordinate a command needs depends on what it does with it.

`add` and `remove` write, and they require the complete coordinate. A command
that picked a version for itself would work until somebody added a second one,
and then rewrite or delete whichever it guessed.

`path` and `info` only read, so they also accept a bare `<namespace>/<name>`.
`path` answers it when the project holds exactly one version of that asset, and
otherwise refuses with `asset_ambiguous` and the versions to choose from — DAC
does not order versions, so there is no latest for it to fall back on. `info`
lists every version, because answering what a project has is its job; it accepts
nothing, one coordinate, or one `<namespace>/<name>`.

`pull` takes any number of either, and no arguments means the whole project:

```bash
dac pull                              # everything the lock file names
dac pull backend-app/geo-database     # every version of one asset
dac pull tools/toolchain@1.4          # one version
```

A project holds every asset every job built from it needs, and a job needs the
ones it needs. Naming them narrows what is fetched and nothing else: the whole
project is still read, and the lock file still has to describe the manifest,
because whether this is the project that was committed is not a question a
command fetching half of it gets to skip. Naming an asset the project does not
have fails with `asset_unknown` rather than fetching nothing and reporting
success.

A namespace and a name are lowercase letters, digits, and `.`, `_`, or `-`. A
version also takes uppercase and `+`, because it is copied from whatever the
publisher calls a release. Every part starts and ends alphanumeric.

`info` does not use the network. It reports manifest and request information
when the lock is missing or stale. Lock and cache information is unavailable in
that state.

Use `dac --help`, `dac <command> --help`, and `dac --version` for CLI
help. DAC has no command aliases.

### Shell completion

```bash
eval "$(dac completion bash)"     # or zsh, fish, pwsh
```

Source it from your shell's startup file to keep it. Beyond command and option
names, DAC completes the argument you actually have to get exactly right:

```text
dac path <TAB>              backend-app/geo-database@2026.08  tools/toolchain@1.4
dac cache remove app/<TAB>  app/geo@1.0.0  app/geo@2.0.0
```

Coordinates come from the manifest for the project the shell is standing in,
which `--manifest` selects like it does everywhere else. Completion suggests the
whole coordinate rather than the bare `<namespace>/<name>` that `path` and `info`
also take, drops the versions already on the command line, and answers with
nothing outside a project — where a shell falls back to completing file names.

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
and has decided to accept the change; it belongs to the commands that write the
lock file, which are `dac lock` and `dac add`.

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

`dac lock` owns every operation that writes the lock file on purpose. Version 5
had folded them onto `pull` as flags, on the grounds that resolving an asset
stores its bytes and so a command that resolved everything already installed
everything. That is true of the bytes and wrong about the decision: whether a
lock file may be rewritten is the question a deployment job most needs answered
"no", and it should not be a flag away from the command that job runs. So
`dac pull --update-lock` is now `dac lock`, `dac pull --refresh-lock` is
`dac lock --refresh`, and `--rebind` moves with them. `dac lock --check` remains
`dac verify --refresh`, which reports drift and writes nothing.

Nothing writes the lock file without being asked: `add` and `remove` maintain it
because they are already changing the project, `lock` because that is what it is
for, and `pull` never.

`pack`, `unpack`, and every `cache` command except `remove` do not use the
network. `unpack` and `cache import` do not read the project files. `cache gc`,
`cache clear`, and `cache scrub --all` cover the whole shared cache.

Version 7 moved the transfer options off the command line and into a
[config file](#config-file). `--timeout`, `--retries`, `--download-parts`,
`--max-size`, and `--credential-helper` were not decisions anybody makes per
invocation; they were flags because there was nowhere else to put them, and the
price was that four commands each carried all five. `--progress` became
`--no-progress`, which is how the flag beside it was already spelled. The
rewrite config moved with them, because it always described a site rather than a
project. `DAC_*` variables went from nine to three, and `dac pull` from fourteen
options to nine.

The cache gained the verbs it was missing. Emptying it used to mean
`dac cache gc --max-age 1s` — a collection with an age short enough that
everything fell outside it, which means something slightly different from what
it was being used for and races with any DAC process running alongside. Seeing
into the cache was not possible at all, and neither was dropping one asset. So
`dac cache clear`, `dac cache list`, and `dac cache remove`. `dac cache verify`
became `dac cache scrub`: `dac verify`, `dac verify --refresh`, and that command
were three operations under one word, costing nothing, a full re-download, and a
full read of the cache respectively.

`dac pull --distdir` is now an argument to `dac cache import`, which already
installed the same digest-named objects out of an archive.

Version 8 leaves DAC with one archive. There were two: a cache bundle, written
by `dac export` and read by `dac import`, holding every object under its digest;
and a dacpack, written by `dac pack` and read by `dac unpack`, holding the same
bytes under the names their origins gave them. They carried the same delivery
and the choice between them had to be made before the file was written, by
somebody who could not yet know what the machine receiving it would want to do.
A bundle handed to a machine that did not run DAC was a tar full of files named
by hash.

So the import reads a dacpack and `dac export` is gone: `dac pack` writes the
archive both halves read. The import moved to `dac cache import` with it,
because the cache is what it writes and it reads nothing else — as a top-level
command it sat among `pull` and `lock` looking like part of a project's
workflow. Running either old spelling says where the work went.

What that costs is the free validation the digest layout had: the only path an
item could claim in a bundle was the one its digest spelled. A dacpack's paths
carry names that came from a remote server, so both readers recompute every path
from the coordinate it belongs to and refuse an index claiming anything else.
What it also costs is size, for a project whose assets overlap — see
[dacpacks](#dacpacks).

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
      "size": 123,
      "filename": "database.bin"
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
serves those bytes. Use `dac lock --refresh` to check every asset against
its origin.

An asset the manifest leaves unpinned records an `etag` when the origin sends
one. A refresh replays it as an `If-None-Match` hint and skips the download on a
`304`: an origin that answers `304` has confirmed the asset just as well as one
that sends the bytes again. A pinned asset neither sends nor records an ETag, so
its lock entry omits the field.

The `filename` field records what the origin calls the asset, which a cache path
cannot carry: that path is a digest, and a digest is the right name for bytes
and the wrong name for anything that reads an extension. DAC takes the name from
a `Content-Disposition` header when the origin sends one, and otherwise from the
last element of the URL the request finished at, so an asset served through a
redirect is named where it is served rather than where it was asked for. A name
that is not a single path element — one holding a separator, a control byte, a
leading `-`, `.`, or `..` — is refused rather than repaired, and the next source
answers instead. An asset that nothing names omits the field.

The name is advisory. Nothing decides anything by it, and it takes no part in
the check that asks whether a lock still describes its manifest, so a lock
written before the field existed is not stale and does not have to re-resolve
anything. `dac lock` fills in what the URL spells for those
entries once, without a request.

It belongs to the lock rather than to the cached object because it describes the
source and not the bytes. Two coordinates that resolve to the same object share
one file in the cache and may well disagree about what it is called.

DAC rejects unknown JSON fields, duplicate keys, unsupported schema versions,
and stale lock files. It writes project files with atomic renames.

## Options

Global options:

| Option | Environment | Default |
|---|---|---|
| `--manifest` | | `dac.json` |
| `--lock` | | `dac-lock.json` beside the manifest |
| `--cache-dir` | `DAC_CACHE_DIR` | `cache.dir`, then the XDG cache |
| `--config` | `DAC_CONFIG` | The XDG config search path |
| `--json`, `-j` | | `false` |
| `--debug` | `DAC_DEBUG` | `false` |

`--lock` follows `--manifest` unless it is given. A project is its two files
together, so `dac --manifest sub/dac.json` reads `sub/dac-lock.json`.

`--debug` writes a trace of what a command actually did. See
[Seeing what happened](#seeing-what-happened).

Per-command options are decisions about one run:

| Option | Environment | Commands |
|---|---|---|
| `--concurrency` | `DAC_CONCURRENCY` | `lock`, `pull`, `verify` |
| `--no-progress` | | `add`, `lock`, `pull`, `verify` |
| `--no-rewrite` | | `add`, `lock`, `pull`, `verify` |
| `--offline` | | `add`, `pull` |
| `--refresh` | | `lock`, `verify` |
| `--rebind` | | `add`, `lock` |

The `verify` options apply only to `--refresh`. A plain `verify` reads the two
project files and stops.

Everything else lives in the [config file](#config-file), because how long this
machine waits and which helper answers for which host are not decisions anybody
makes per invocation. Passing one of the options that moved says where it went.

## Config file

DAC reads `config.toml` from the XDG base directories, most important first:

1. `--config <path>`, or `DAC_CONFIG`. This file must exist.
2. `$XDG_CONFIG_HOME/dac/config.toml`, or `~/.config/dac/config.toml`.
3. `<dir>/dac/config.toml` for each `$XDG_CONFIG_DIRS` entry, or `/etc/xdg`.

Files merge **per setting**, so a site can install one under `/etc/xdg` and a
person can override one line of it without restating the rest. Tables merge by
key; arrays replace whole, because a host policy is one policy and half of two
of them is a policy nobody wrote. A flag beats the file, and the file beats the
built-in default.

```toml
schema-version = 1

[transfer]
timeout        = "5m"      # inactivity limit for one transfer
retries        = 2
concurrency    = 4         # assets at once
download-parts = 4         # see Split downloads
max-size       = "2GiB"    # or "none"
progress       = true

[cache]
dir      = "/var/cache/dac"  # absolute; unset means the XDG cache
max-age  = "30d"             # the default dac cache gc collects by
max-size = "20GiB"           # or "none"; what dac cache gc leaves behind

[credentials]
default             = "/usr/local/bin/dac-cred"
"files.example.com" = "/usr/local/bin/dac-cred-artifacts"

[[rewrite]]
pattern     = '^vendor\.example\.com/(.*)$'
replacement = "https://mirror.internal/vendor/$1"

[hosts]
block               = ["*"]
allow               = ["releases.internal", "mirror.internal"]
allow-insecure-http = false
```

Unknown keys, unreadable values, and a `schema-version` DAC does not know are
all refused rather than ignored, as they are in the project files. The
`schema-version` key is optional; a file that carries one must carry `1`.

DAC refuses a config file that group or other can write. The `credentials` table
names programs DAC runs, so a writable config is a way to choose them — the same
reason `ssh` refuses a writable `~/.ssh/config`.

Two commands answer what a merged search path settled on:

```bash
dac config path    # the files that were read, one per line
dac config show    # the effective values, and where each came from
```

`dac config show` writes a config file. What DAC is using is therefore also the
starting point for changing it, and saving the output loads back to the same
settings.

`transfer.timeout` is an inactivity timeout. DAC retries transient network
failures. It requests identity encoding and checks redirects with the same URL
policy.

`transfer.max-size` bounds a response whose size DAC does not know ahead of
time, so it exists to stop a runaway stream rather than to express a policy
about asset sizes. Raise it for a project with genuinely larger assets.
`"none"` removes the bound, and nothing else does: an empty value, a zero, and a
count too large for DAC to hold are all rejected rather than read as no limit.

`transfer.download-parts` sets how many requests one download may be split
across. See [Split downloads](#split-downloads). It is a budget for the whole
command rather than a per-asset multiplier, so raising it speeds up a project of
one large asset without opening `concurrency` times as many connections for a
project of many. Set it to `1` to send one request per asset.

## Split downloads

DAC finishes one large download over several parallel requests when the origin
serves byte ranges. The first response carries the first 8MiB, and the rest of
the asset arrives as `Range` requests for the 8MiB pieces after it, so splitting
costs no extra round trip. A fixed piece size rather than a share of the asset is
what bounds the cost: bytes are hashed in order, so a piece that arrives early
waits its turn in memory, and only a few are ever in flight.

Three conditions have to hold, and DAC streams the single response it already has
whenever one does not:

- `transfer.download-parts` is more than `1` and the command has a part to spare.
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
`transfer.retries` and backoff as a whole request: its bytes have not reached the hash
yet, so the retry costs one piece rather than the asset.

## Seeing what happened

DAC is pointed at a mirror by a rewrite rule, handed credentials by a program it
starts, and told to retry and to split downloads across range requests. When any
of that misbehaves, the only thing on screen is the failure at the end of it.

`--debug`, or `DAC_DEBUG`, writes a trace of the decisions behind it to standard
error:

```bash
dac --debug pull
```

```text
level=DEBUG msg="rewrote request" from=https://vendor.example.com/db.bin to=https://mirror.internal/vendor/db.bin
level=DEBUG msg="running credential helper" host=mirror.internal command=/usr/local/bin/dac-cred
level=DEBUG msg="credential helper answered" host=mirror.internal command=/usr/local/bin/dac-cred headers="[Authorization]"
level=DEBUG msg=fetching url=https://mirror.internal/vendor/db.bin conditional=false
level=DEBUG msg="splitting download" url=https://mirror.internal/vendor/db.bin length=20971520 chunks=3 workers=2 precondition=If-Match
level=DEBUG msg=range url=https://mirror.internal/vendor/db.bin start=8388608 end=16777215 status=206
level=DEBUG msg=installed digest=sha256:6adfa077... size=20971520
```

It answers the questions nothing else does: which URL was requested after a
rewrite, which helper answered for which host, how many retries happened and
what each one saw, whether an object came from the cache or the origin, and what
a collection evicted.

A download that arrives over one connection when it should have split says why,
and every reason is something the origin decided rather than a setting to go and
correct:

```text
level=DEBUG msg="streaming one response" url=... reason="origin does not serve byte ranges" length=20971520
level=DEBUG msg="streaming one response" url=... reason="origin sent no strong validator" length=20971520
level=DEBUG msg="streaming one response" url=... reason="no parts to spare" length=20971520
```

Two things a trace never carries. It is not part of the [output
contract](#output-contract) — it goes to standard error, it is written for a
person, and its wording is free to change, so nothing should parse it. And it
never carries what a credential helper returned: the helper's answer is the
secret itself, and a trace reports which helper ran and which header names it
set, never a value.

Tracing turns progress bars off. Both write to standard error and the bars
redraw in place, so the two together produce a display that is neither.

## Credentials

DAC gets request credentials only from a credential helper, named in the config
file's `credentials` table:

```toml
[credentials]
default             = "/usr/local/bin/dac-cred"
"files.example.com" = "/usr/local/bin/dac-cred-artifacts"
```

`default` applies to every host. A host key applies to one, and wins over
`default`. The table is where this lives because it is a map from host to
command, and the flag it replaced could only say that by inventing
`<host>=<command>` and being repeated once per host.

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

## Rewriting and host policy

A manifest records where an asset comes from upstream. Rewrite rules decide
where DAC actually sends the request, so a site that proxies its downloads does
not have to edit every project that uses them. The manifest and the lock file
keep the canonical URL.

Because that is a statement about a site rather than about a project, the rules
live in the [config file](#config-file):

```toml
[[rewrite]]
pattern     = '^vendor\.example\.com/(.*)$'
replacement = "https://mirror.internal/vendor/$1"

[hosts]
block               = ["*"]                # refuse every host no allow permits
allow               = ["releases.internal", "mirror.internal"]
allow-insecure-http = false                # plain HTTP for rewritten URLs
```

A project that means to say something about itself can still carry a
`dac-rewrite.cfg` beside its manifest, in the directive form:

```text
block *
allow releases.internal
rewrite ^vendor\.example\.com/(.*)$ https://mirror.internal/vendor/$1
allow mirror.internal
allow_insecure_http
```

That file wins outright when it exists. It is a whole policy rather than an
addition to one, and merging it with the site's would produce rules neither
wrote. Use `--no-rewrite` with `add`, `lock`, `pull`, or `verify --refresh` to
disable both.

Run `dac info` to show each source URL, request URL, and host policy result. Add
an asset coordinate to show only that asset.

A `rewrite` pattern is a Go regular expression. It matches `host/path` with any
query string appended. Its replacement can use `$1` groups. A replacement
without a scheme keeps the original URL scheme. A URL fails when multiple
rewrite rules match it.

DAC applies one rewrite before it checks `allow` and `block` rules. An `allow`
match overrides all `block` matches. Rule order does not change this host
policy. A host with no match is allowed. The `*` pattern matches every host.
Other patterns match a host and its subdomains.

Redirect targets use the same host policy, but DAC does not rewrite them.

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
{"outputVersion":6,"ok":true,"command":"path","data":{}}
```

`info` always returns an `assets` array in JSON mode, alongside a `summary`
object holding the counts. A coordinate filters the array to one item, and a
`<namespace>/<name>` filters it to that asset's versions. A missing
or stale lock sets `cacheStatus` to `unavailable` and omits digest, size,
filename, and path data; a damaged object sets it to `corrupt`.

Every asset carries the optional `filename` its lock entry records, which is
what the origin calls the asset. The cache path names the bytes, so this is the
half a script needs to put a file somewhere a later tool will recognize. It is
absent for an asset nothing names, and it was added without a version bump
because an added optional field breaks no consumer.

A `pull` result carries `projectCount` alongside `assetCount`, which is how
many assets the project has rather than how many this pull took. The two differ
only when the pull was narrowed, and the difference is what tells "there was one
asset" from "one asset was asked for". It was added without a version bump for
the same reason `filename` was.

JSON errors use the same stream and framing:

```json
{"outputVersion":6,"ok":false,"command":"pull","error":{"code":"network_error","message":"The asset request failed.","cause":"https://example.com/db: unconditional request returned HTTP 404","details":{"asset":"backend-app/geo@1.0.0","status":404,"url":"https://example.com/db"}}}
```

`code` and `message` are stable enough to branch on, which is exactly why
neither says anything specific about a failure. `cause` carries the part an
operator acts on, and `details` repeats the useful parts of it as data: `url`
and `status` for a request failure, expected and actual digests for a content
failure, the locked and resolved digests for a `version_rebind`, and the two
versions and the object they share for a `version_collision`.

Human errors, help, and progress go to standard error. JSON mode does not write
human summaries or error messages. The `lock` and `pull` commands also disable
progress in JSON mode.

Output version `5` came with the move to a config file. `dac cache verify`
became `dac cache scrub`, so the `command` field of that result changed, and
`pull` dropped both the `distdir` asset status and the `distdir_read_failed`
code along with the flag that produced them.

Output version `6` came with the two archives becoming one. `export` is gone
along with the `bundle_invalid` code it shared with `import`, and an import's
`command` field is now `cache.import`. The result's `bundle` field is `source`,
because what it names is a dacpack or a directory rather than the one format
that used to have a name of its own. An invalid archive reports
`dacpack_invalid` from either half that reads one.

Output version `4` moved the lock operations off `pull` onto `dac lock`. A
`pull` result no longer carries `locked`, because a pull no longer writes the
lock file; a `lock` result carries it, along with `changed` for whether the file
on disk actually moved.

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
completion line for each asset. Use `--no-progress`, or `transfer.progress` in
the config file, to disable both forms.
The `lock` and `pull` commands disable both forms in JSON mode.

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
stat. `dac cache scrub` answers that by hashing every object it checks, in
exchange for reading all of them:

```bash
dac cache scrub               # the objects this project locked
dac cache scrub --all         # every object in the shared cache
dac cache scrub --repair      # remove the ones that fail
```

A corrupt object makes the command exit `1` with the code `cache_object_corrupt`
unless `--repair` removes it. Corrupt objects are worth nothing, and `dac pull`
replaces one by downloading it again, so `--repair` followed by `pull` restores
a damaged cache.

Damage is reported wherever it turns up. `dac path` and `dac pack` refuse,
because neither can do anything about it — and an archive is the one artifact
that carries cache damage onto machines that cannot tell where it came from.
`dac info` reports `cache: corrupt`. `dac pull` downloads the asset again and
installs good bytes over the bad ones, reporting the asset as `repaired`.

### Managing the cache

```bash
dac cache dir                        # where it is
dac cache list                       # what this project has in it
dac cache list --all                 # everything, with sizes and last use
dac cache import ./delivery.dacpack  # fill it from an archive
dac cache gc                         # collect by age
dac cache gc --max-size 20GiB        # and evict until it fits
dac cache clear                      # empty it
dac cache remove app/geo@2026.08     # forget one asset's bytes
dac cache scrub --all                # read every byte and check it
```

`dac cache list` reports each object's digest, size, when a project last used
it, and the coordinates it belongs to. It does not count as using the cache:
reaching an object refreshes the timestamp collection runs on, so a listing built
that way would quietly keep everything alive.

`dac cache remove` takes coordinates rather than digests, because a coordinate is
what you have. Two coordinates that resolved to the same bytes share one object,
so removing one can uncache the other — it refuses with `cache_object_shared`
and names what it would cost, and `--force` accepts that.

Nothing here needs confirming. DAC prompts nowhere, a removed object costs a
`dac pull` and nothing that cannot be got back, and `--dry-run` is already the
careful path for `gc` and `clear`.

### Collection

Every cache hit refreshes the object's sidecar timestamp, because age is the
only liveness signal a content-addressed store has. `dac cache gc` removes
objects that no project has used within `--max-age`, which accepts `30d`, `2w`,
or any Go duration, and defaults to `cache.max-age` in the config file, or `30d`
where nothing sets it. It also removes temporary files left behind by an
interrupted download, and sidecars whose object is gone. Use `--dry-run` to see
what it would remove.

`dac cache clear` removes every object regardless of age, along with the same
temporary files and orphaned sidecars. It is what "empty the cache" means, and
it exists because saying it as a collection with an age short enough that
everything fell outside it meant something slightly different and raced with any
DAC process running alongside.

Neither one removes a digest lock file. Unlinking a lock that another process
holds would let a later process take the same lock through a new inode.

### Keeping the cache to a size

Age answers whether anything is still using an object. It does not answer
whether there is room, and on a build machine that is the question: a set of
projects that genuinely uses more than the disk has to spare has nothing old
enough to collect and is still too full. The only lever used to be guessing an
age short enough to hurt.

`--max-size`, or `cache.max-size` in the config file, is the other half. A
collection first removes what nothing has used within `--max-age`, and then, if
the cache is still over the bound, evicts the least recently used objects until
it fits:

```bash
dac cache gc --max-size 20GiB --dry-run    # what it would take
dac cache gc --max-size 20GiB
```

It accepts the same sizes `transfer.max-size` does, and `none` — the default —
means no bound, which is what collection by age alone amounts to.

Eviction is reported apart from collection, in `evictedCount` and
`evictedBytes` and in the summary line, because the two mean different things.
Taking an object nothing has touched in a month is a cache doing its job.
Taking one a project used yesterday is a cache too small for what this machine
builds, and the next `dac pull` downloads it again. Both counts are included in
the totals beside them.

An object something reaches for while the collection is waiting for its digest
lock is left alone: it has just become the most recently used thing in the
cache rather than the least. That can end a run still over the bound, which the
next collection settles.

This is a bound on collection rather than a quota on the cache. Nothing stands
between a download and the disk, so a single `dac pull` larger than the bound
still lands, and `gc` brings the cache back under afterwards. Set the bound
below the disk you have rather than at it.

### Moving a cache

`dac pack` writes every locked asset to one archive. `dac cache import
<dacpack>` validates that archive and installs its objects in the local cache.
That is the cold cache on an isolated machine:

```bash
dac pack ./delivery.dacpack             # on a machine with network access
dac cache import ./delivery.dacpack     # on an isolated machine
dac pull --offline                      # everything it needs is already there
```

`pack` hashes each object while it writes the archive. `cache import` checks the
index, entry type, path, size, and digest, and refuses the whole archive rather
than install the part of it that read cleanly. Neither half needs the network,
and the importing machine needs no project files at all — an archive says what
it carries, and the cache is keyed by digest.

The same file is what `dac unpack` reads, so what crosses the air gap does not
have to be decided by whoever wrote it. A machine that runs DAC imports it; a
machine that does not extracts it with `tar` and gets real files with real
names. DAC used to write a separate cache bundle for the first case, holding
every object under `blobs/sha256/<hex>`, and delivering one to anybody in the
second case handed them a tar full of files named by hash.

`dac cache import <dir>` also accepts a distribution directory, for a delivery
that arrives on a mounted share rather than as a file. Each file in it must use
its SHA-256 hexadecimal digest as its name; anything else is left alone, so a
`README` beside the objects costs nothing. Files whose contents do not match the
name they carry are refused.

```bash
dac cache import /mnt/dist && dac pull --offline
```

This replaces `dac pull --distdir`, which installed the same digest-named
objects from the same kind of directory as a flag on an unrelated command. One
difference is worth knowing: `--distdir` read only the objects the lock named,
where an import installs everything in the directory. A share holding more than
this project needs will therefore cost more cache than it used to.

### dacpacks

A dacpack is the project, materialized. Its files carry the names their origins
gave them, so unpacking one — or just extracting it with `tar` — leaves a
directory of real files with real extensions:

```bash
dac pack                           # writes ./dac.dacpack
dac unpack                         # writes ./assets/... from it
dac unpack build.dacpack /opt/in   # or name both
```

`unpack` writes files and **never touches the cache**. That is the whole
difference from `dac cache import`, which reads the same archive and installs
the same bytes under their digests: one hands a project's assets to something
that is not DAC at all, the other moves a cache to a machine that runs it. It
reads no project files and needs no cache directory, so it runs anywhere the
archive does.

The archive defaults to `dac.dacpack` and the destination to the working
directory, because a dacpack is a build output that a project makes one of. An
import has no default, because the archive it reads arrived from somewhere else
and is wherever whoever delivered it put it. Every one of them is an argument
either way: whether a path has a default and whether it is a flag are separate
questions, and a required flag is a flag in name only.

The layout keys each file on the coordinate it belongs to, and the name comes
from the lock file's `filename` — falling back to the name half of the
coordinate for an asset that has none:

```text
index.json
assets/java/sdk/11/jdk-11.tar.gz
assets/java/sdk/17/jdk-17.tar.gz
```

The coordinate supplies the directories because two versions of one asset almost
always share a file name, and two assets easily can. A layout keyed on the name
alone would have the second file silently overwrite the first.

Each file lands at the same path under the destination that it has inside the
archive, so `dac unpack <archive> <dir>` and `tar -xf <archive> -C <dir>` put the
files in the same places. `index.json` records what each one is — coordinate,
source URL, path, file name, digest, and size — and both readers check every
file against its digest as they go. A file name says nothing about the bytes
under it, so that digest is the only claim there is to check.

`unpack` refuses to replace anything unless `--force` is given, naming every
file that is in the way and writing nothing. The destination defaults to the
directory the command was run in, where a mistake costs work that was never
DAC's to lose. A failed unpack leaves nothing behind either: an archive is not
known to be sound until it has been read to the end, so anything already written
is taken back rather than left looking like a complete tree.

Two coordinates that resolved to one object are one object in the cache and two
files in a dacpack, because each is materialized under its own name and a file
cannot be in two places. Packing a project whose assets overlap therefore costs
more than the cache it came from. Importing one costs nothing extra: both files
are read and checked, and the cache holds the one object they agree on, which is
what `objectCount` and `byteCount` report.

A dacpack's paths carry names that came from a remote server, so a reader
recomputes every path from the coordinate it belongs to and refuses an index
claiming anything else. A file name that is not a single safe path element is
refused outright, and a symlink where a file is going counts as something
already there rather than as empty space — following one is how an extraction
writes outside the directory it was pointed at. An import derives no path at all:
the cache decides where an object lives, from its digest.

## Non-goals

DAC does not extract, install, or run an asset. It hosts no mirror or remote
cache, resumes no partial download, and records no signature or provenance. It
has no registry and no plugin language. Extraction and installation belong to
whatever consumes the path that `dac path` returns.

`dac unpack` is not an exception to that. It writes the files a dacpack
carries, which is DAC's own archive of a project's assets; what is inside those
files stays sealed, and a tarball among them is still yours to `tar -xzf`.

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
