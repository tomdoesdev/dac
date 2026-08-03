# DAC

DAC locks remote files and keeps them in a local content-addressed cache. It is
small by design. It gives deployment and infrastructure tools one stable way to
fetch data files, policy bundles, provider binaries, and similar build inputs.

DAC does not unpack or install assets. It returns the verified cache path. The
calling script decides how to use that path.

## Install

DAC requires Go 1.26.

```bash
mise run build
```

The command writes `bin/dac`.

## Quick start

```bash
dac init
dac add geo-database@2026.08 \
  --source https://example.com/geo/2026.08/database.bin
dac pull
geo_database="$(dac path geo-database@2026.08)"
```

`add` resolves the new asset and updates both project files. Commit
`dac.json` and `dac-lock.json`. A failed resolution changes neither file.

Use `add --offline` to update only `dac.json`. The command makes no network
request and does not change `dac-lock.json`. Run `dac lock` later to update it.

In CI or a deployment job, run:

```bash
dac verify
dac pull
```

`verify` checks the project files without network or cache access. `pull`
uses a matching cache object without a network request.

## Commands

| Command | Result |
|---|---|
| `dac init [--force]` | Create matching empty project files. |
| `dac add <name>@<version> --source <url> [--offline] [--no-rewrite]` | Add one asset. Resolve it unless offline mode is active. |
| `dac remove <name>@<version>` | Remove one asset without network access. |
| `dac info [<name>@<version>]` | Show asset, request, lock, and cache information. |
| `dac lock [--refresh] [--no-rewrite]` | Resolve all manifest assets and write a new lock file. |
| `dac pull [--offline] [--distdir <dir>] [--no-rewrite]` | Download missing locked assets. |
| `dac path <name>@<version>` | Return a verified cache path. |
| `dac verify` | Check that the manifest and lock file agree. |
| `dac export --dir <dir>` | Copy locked objects into a distribution directory. |
| `dac cache gc [--max-age <age>] [--dry-run]` | Remove cache objects that nothing has used recently. |

DAC accepts one active version for each asset name. The `add`, `remove`, and
`path` commands require exactly one `name@version` coordinate. The `info`
command accepts zero or one coordinate. Names and versions must not be empty or
contain another `@`.

`info` does not use the network. It reports manifest and request information
when the lock is missing or stale. Lock and cache information is unavailable in
that state.

Use `dac --help`, `dac <command> --help`, and `dac --version` for CLI
help. DAC has no command aliases.

`export` and `cache gc` do not use the network. `cache gc` does not read the
project files: it collects the whole cache, which several projects share.

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

An asset with an `integrity` value that the cache already holds lets `dac lock`
finish without a request, which also means it never confirms that the URL still
serves those bytes. Use `dac lock --refresh` to check every asset against its
origin.

An asset the manifest leaves unpinned records an `etag` when the origin sends
one. The next `dac lock` replays it as an `If-None-Match` hint and skips the
download on a `304`. A pinned asset neither sends nor records one, so its lock
entry omits the field.

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
| `--timeout` | `DAC_TIMEOUT` | `5m` | `add`, `lock`, `pull` |
| `--retries` | `DAC_RETRIES` | `2` | `add`, `lock`, `pull` |
| `--concurrency` | `DAC_CONCURRENCY` | `4` | `lock`, `pull` |
| `--max-size` | `DAC_MAX_SIZE` | `32GiB` | `add`, `lock` |
| `--progress` | | `true` | `add`, `lock`, `pull` |
| `--no-rewrite` | | `false` | `add`, `lock`, `pull` |
| `--credential-helper` | `DAC_CREDENTIAL_HELPER` | | `add`, `lock`, `pull` |
| `--distdir` | `DAC_DISTDIR` | | `pull` |

`--timeout` is an inactivity timeout. DAC retries transient network failures.
It requests identity encoding and checks redirects with the same URL policy.

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
file, and that file must exist. Use `--no-rewrite` with `add`, `lock`, or `pull`
to disable the complete config.

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
does. A `lock` of an asset with no `integrity` value has no expected digest yet,
so a rewrite config is trusted input in the same way a manifest URL is.

## Output contract

DAC writes human-readable command results to standard output by default.
For example, `path` writes only the verified cache path. `info` writes one
detailed block for each selected asset.

Use `--json` or `-j` to write one versioned JSON document to standard output:

```json
{"outputVersion":1,"ok":true,"command":"path","data":{}}
```

`info` always returns an `assets` array in JSON mode. A coordinate filters this
array to one item. A missing or stale lock sets `cacheStatus` to `unavailable`
and omits digest, size, and path data.

JSON errors use the same stream and framing:

```json
{"outputVersion":1,"ok":false,"command":"pull","error":{"code":"content_mismatch","message":"...","details":{}}}
```

Human errors, help, and progress go to standard error. JSON mode does not write
human summaries or error messages. The `pull` command also disables progress in
JSON mode.

Exit status `0` means success. Status `1` means that the command failed.
Status `2` means that DAC rejected the arguments.

On an interactive terminal, DAC uses concurrent progress bars from
[`mpb`](https://github.com/vbauerster/mpb). Each bar shows the asset name,
bytes, percentage, speed, and completion state. An unknown response size uses a
spinner and a byte count. A non-terminal stream gets one start line and one
completion line for each asset. Use `--progress=false` to disable both forms.
The `pull` command disables both forms in JSON mode.

## Cache behavior

DAC stores each object at:

```text
<cache>/blobs/sha256/<digest>
```

A download first enters a temporary file. DAC hashes and checks all bytes
before one atomic rename installs the object. Each digest has a Unix process
lock. Concurrent DAC processes cannot install the same object at the same time.

`pull` trusts an object when its cache path and size match the lock file. A
missing object is fetched without an ETag and checked against the locked digest
and size. `lock` can use a publisher digest or a cached object before it makes
a request. It uses a stored ETag only as a lock-time request hint, and only for
an asset with no `integrity` value. A publisher digest already settles which
bytes are correct, so a pinned asset is answered from the cache or downloaded
outright, and its lock entry carries no `etag`.

A content check that fails reports the digest DAC received alongside the one it
expected, in the human message and in the JSON `details`. The two cases that
produces are worth telling apart: a corrupted transfer, and a publisher who
replaced the bytes behind a stable URL.

### Collection

Every cache hit refreshes the object's timestamp, because age is the only
liveness signal a content-addressed store has. `dac cache gc` removes objects
that no project has used within `--max-age`, which accepts `30d`, `2w`, or any
Go duration, and defaults to `30d`. It also removes temporary files left behind
by an interrupted download. Use `--dry-run` to see what it would remove.

Collection never removes a digest lock file. Unlinking a lock that another
process holds would let a later process take the same lock through a new inode.

### Distribution directories

`dac export --dir <dir>` copies every locked object into a directory, named by
its digest. `dac pull --distdir <dir>` installs from such a directory before it
makes any request, which gives `--offline` a cold cache to start from on an
isolated machine.

```bash
dac export --dir ./bundle          # on a machine with network access
dac pull --offline --distdir ./bundle
```

A file named for a digest is a claim DAC checks, and a mismatch fails the pull.
DAC also accepts a file named after the last element of the asset URL; that name
is only a guess, so a mismatch there is skipped rather than fatal.

## Non-goals

DAC does not unpack, install, or run anything. It hosts no mirror or remote
cache, resumes no partial download, and records no signature or provenance. It
has no registry and no plugin language. Extraction and installation belong to
whatever consumes the path that `dac path` returns.
