# DAC

DAC locks remote files and stores verified bytes in a local content-addressed
cache. It is designed for data files, policy bundles, provider binaries, and
other build inputs.

DAC does not install assets or extract their internal archives. A caller can use
the verified cache path or materialize cached files into a directory.

## Install

DAC requires Go 1.26 and targets Unix.

```bash
go install github.com/tomdoesdev/dac/cmd/dac@latest
```

To build a checkout, run:

```bash
mise run build
```

## Quick start

```bash
dac init
dac trust add example.com
dac add backend/geo@2026.08 \
  https://example.com/geo/2026.08/database.bin --pin --name geo.db
dac pull
geo_database="$(dac path backend/geo@2026.08)"
dac unpack backend/geo@2026.08 --dest ./inputs
```

Commit both `dac.json` and `dac-lock.json`.

An asset coordinate has this form:

```text
<namespace>/<name>@<version>
```

A project can contain several versions of one asset. DAC does not order
versions or select a latest version. Two versions can resolve to identical
bytes.

`add` resolves one asset and updates both project files. Use `add --offline` to
write only the manifest. Run `dac lock` later to resolve offline changes.

`pull` reads a current lock file and installs missing objects. It does not
change project files. It uses valid cached objects without a network request.

`lock` updates the lock file. `lock --refresh` resolves every asset and accepts
the bytes that each origin now serves. `verify --refresh` performs the same
origin check but does not write the lock file. It reports `lock_drift` when the
bytes differ.

## Commands

| Command | Purpose |
|---|---|
| `dac init [--force]` | Create matching empty project files. |
| `dac add <coordinate> <url> [options]` | Add one asset version. |
| `dac remove <coordinate>` | Remove one asset version. |
| `dac info [<asset>[@<version>]]` | Show manifest, lock, and cache state. |
| `dac lock [--refresh] [--concurrency <n>]` | Update the lock file. |
| `dac pull [<asset>[@<version>]...] [options]` | Install locked objects. |
| `dac path <asset>[@<version>]` | Print one verified object path. |
| `dac verify [--refresh] [--concurrency <n>]` | Check project files and optional origin drift. |
| `dac unpack [<asset>[@<version>]...] [--dest <dir>] [--force]` | Write verified cached assets into a directory. |
| `dac cache dir` | Print the resolved cache directory. |
| `dac cache list [--all]` | List cache objects. |
| `dac cache gc [options]` | Collect old objects and apply a size bound. |
| `dac cache clear [--dry-run]` | Remove every cache object. |
| `dac cache remove <coordinate>... [--force]` | Remove selected objects. |
| `dac cache scrub [--all] [--repair]` | Hash objects and report damage. |
| `dac trust list` | List the trusted hosts and when each was last used. |
| `dac trust add <host\|url>...` | Trust one or more hosts. |
| `dac trust remove <host\|url>...` | Withdraw trust from one or more hosts. |
| `dac trust gc [--max-age <d>] [--dry-run]` | Withdraw trust from unused hosts. |
| `dac trust path` | Print the resolved trusted-hosts file. |
| `dac config path` | Print the config files that DAC read. |
| `dac config show` | Print effective settings. |
| `dac completion <shell>` | Write a shell completion script. |

Run `dac --help` or `dac <command> --help` for all flags.

### Asset selection

`info`, `pull`, and `unpack` use the same selection rules:

```bash
dac pull                         # the whole project
dac pull backend/geo             # every version in this group
dac pull backend/geo@2026.08     # one exact version
```

Several selectors form one set. Repeated or overlapping selectors do not run an
asset twice. An unknown selector fails with `asset_unknown`.

`path` also accepts a group without a version. It succeeds only when the group
contains one version. Otherwise, it reports `asset_ambiguous`.

### Shell completion

```bash
eval "$(dac completion bash)"  # bash, zsh, fish, or pwsh
```

DAC reads the current manifest to complete coordinates. Completion works for
`unpack` and the other commands that accept asset arguments. Repeatable
commands omit coordinates that are already present.

## Cache-backed unpack

`dac unpack` writes selected lock objects from the local cache. It never uses
the network. Run `dac pull` first when an object is missing or corrupt.

The default destination is the current directory. DAC selects each output name
in this order:

1. The manifest `filename`.
2. The resolved lock `filename`.
3. The coordinate name.

Every name must be one safe path element. DAC rejects all selected name
collisions before it writes a destination file.

DAC refuses existing destinations unless `--force` is set. It never replaces a
directory. A forced write replaces a file symlink itself and does not follow the
link. The destination directory must not be a symlink.

DAC copies every selected object to a temporary file in the destination. It
hashes and checks all staged files before commit. If commit fails, DAC restores
replaced files and removes new files.

JSON unpack results contain these fields:

```json
{
  "directory": "/work/inputs",
  "projectCount": 3,
  "fileCount": 1,
  "byteCount": 123,
  "files": [
    {
      "coordinate": "backend/geo@2026.08",
      "filename": "geo.db",
      "path": "/work/inputs/geo.db",
      "digest": "sha256:...",
      "size": 123
    }
  ]
}
```

## Project files

The manifest records source intent:

```json
{
  "schemaVersion": 2,
  "assets": {
    "backend/geo@2026.08": {
      "url": "https://example.com/geo.bin",
      "integrity": "sha256:optional-publisher-digest",
      "filename": "geo.db",
      "allowInsecureHttp": false
    }
  }
}
```

The lock records resolved bytes:

```json
{
  "lockVersion": 2,
  "manifestDigest": "sha256:...",
  "assets": {
    "backend/geo@2026.08": {
      "url": "https://example.com/geo.bin",
      "digest": "sha256:...",
      "size": 123,
      "etag": "optional",
      "filename": "geo.db"
    }
  }
}
```

`integrity` accepts canonical `sha256:<hex>` and SRI `sha256-<base64>` input.
DAC writes the canonical form. `--pin` records the resolved digest as the
manifest integrity value.

DAC requires HTTPS by default. It permits HTTP loopback URLs. Use
`add --allow-insecure-http` to permit another HTTP source. The same URL policy
applies to redirects.

A lock can record an ETag for an unpinned asset. DAC can use it for conditional
refresh requests. A pinned asset does not send or record an ETag.

One URL can serve only its current bytes. `add` warns when asset coordinates
share a source URL. DAC does not reject the shared source.

## Configuration

DAC reads XDG config files named `dac/config.toml`. User values override site
values one setting at a time. `--config` or `DAC_CONFIG` selects one required
file.

```toml
schema-version = 2

[transfer]
timeout = "5m"
retries = 2
concurrency = 4
download-parts = 4
max-size = "2GiB"
progress = true

[cache]
dir = "/var/cache/dac"
max-age = "30d"
max-size = "none"

[trust]
file = "/var/lib/dac/trusted-hosts.json"
max-age = "180d"
```

`--concurrency` and `DAC_CONCURRENCY` override transfer concurrency for one
run. `--no-progress` disables progress. `--cache-dir` and `DAC_CACHE_DIR`
override the configured cache directory. Otherwise, DAC uses the XDG cache
location.

Global output flags are `--json`, `--color`, and `--debug`. `DAC_COLOR` and
`DAC_DEBUG` set the same modes. `NO_COLOR`, `CLICOLOR`, and `CLICOLOR_FORCE`
apply in automatic color mode.

## Downloads and HTTP

DAC hashes every download before it installs the object. It checks the locked
digest and size. A content failure reports expected and actual values.

The client retries eligible failures and applies the same URL policy after each
redirect. It can split a large download when the origin supports byte ranges.
The range-request budget is shared across active assets.

Progress is written to standard error. Terminals get progress bars. Other
streams get stable start and finish lines. JSON and debug modes disable progress
rendering.

`--debug` writes request, retry, range, and cache decisions to standard error.
It does not write authorization data because DAC has no credential feature.

## Trusted hosts

DAC refuses to download from a host that is not in its trusted-hosts file. The
URL policy says which schemes DAC will request; the trust list says who DAC is
willing to request them from, so an edited manifest cannot move a download to a
host nobody chose.

```bash
dac trust add example.com          # a bare host
dac trust add https://example.com/asset   # or the URL you already have
dac trust list
```

The file is `dac/trusted-hosts.json` under the XDG data location, which is
`~/.local/share/dac/trusted-hosts.json` unless `XDG_DATA_HOME` says otherwise.
`--trust-file`, `DAC_TRUST_FILE`, and `trust.file` select a different one.
`dac trust path` prints the one in use. A file that does not exist yet trusts
nothing; a file DAC cannot parse fails the command rather than being ignored.

```json
{
  "schemaVersion": 1,
  "hosts": {
    "example.com": {
      "addedAt": "2026-08-05T09:14:22Z",
      "lastUsed": "2026-08-05T11:02:48Z"
    }
  }
}
```

A host matches exactly, ignoring case and port. `example.com` does not cover
`cdn.example.com`, and there are no wildcards. A Unicode host is trusted in the
punycode form its URL carries.

The check runs in the transport, so it applies to every request a download
makes: the asset, each redirect it follows, and each range of a split download.
A URL on a trusted host that redirects to an untrusted one is refused, and the
error names the host it was refused at rather than the one you asked for. A
refusal is never retried.

Refusals report the code `host_not_trusted` with the offending host in
`details.host`, and the message names the command that changes the answer:

```text
Error: DAC will not download from cdn.example.com. Run dac trust add cdn.example.com.
```

Two flags change what a downloading command does about trust:

- `--insecure-trust-all` skips the check for one run, on `add`, `pull`, `lock`,
  and `verify`. It records nothing. There is no environment variable for it, so
  it cannot be left switched on for everything a shell later runs.
- `dac add --trust` records the source URL's host before the request goes out,
  which is `dac trust add` and `dac add` in one step. It trusts only the host
  you typed; a redirect elsewhere is still refused.

`dac info` reports `trust: trusted` or `trust: untrusted` per asset, with the
host in `host` and a count of the assets a pull would refuse in
`summary.untrustedCount`.

DAC records when a download last reached each trusted host, once per run rather
than once per request. `dac trust gc` withdraws trust from the hosts nothing has
downloaded from within `trust.max-age`, which defaults to 180 days. A host added
by hand with no timestamps is never collected, because nothing about it can be
shown to be stale.

## Cache behavior

Objects use this layout:

```text
<cache>/blobs/sha256/<hex-digest>
<cache>/blobs/sha256/<hex-digest>.meta
```

Several coordinates can share one object. DAC installs objects with an atomic
rename and uses a process lock for each digest.

The sidecar records object state and last use. DAC hashes an object when its
file state changes. `dac cache scrub` always hashes the selected objects.

```bash
dac cache list
dac cache list --all
dac cache gc --max-age 30d --max-size 20GiB --dry-run
dac cache clear --dry-run
dac cache remove backend/geo@2026.08
dac cache scrub --all --repair
```

Collection removes old objects, abandoned temporary files, and orphaned
sidecars. A size bound then evicts least-recently-used objects until the cache
fits. The bound applies during collection; it is not a download quota.

`cache remove` refuses to remove a shared object unless `--force` accepts the
effect on other coordinates. Removed or corrupt objects can be restored with
`dac pull`.

## Output contract

Human summaries go to standard output. Errors and progress go to standard
error. Help and version output also go to standard error so that standard
output stays safe for command results.

`--json` writes one document to standard output. The current contract is
version 8.

```json
{"outputVersion":8,"ok":true,"command":"path","data":{}}
```

Failures contain a stable `code`, a short `message`, optional `cause`, and a
`details` object. Exit status `0` means success, `1` means a command failure,
and `2` means a usage error.

## Extension boundaries

The application package exposes `Fetcher`, `FetcherDecorator`, and `Reporter`
as adapter boundaries. `DecorateFetcher` keeps the first decorator outermost.

The HTTP package exposes `TransportDecorator` through client options. The first
transport decorator is also outermost. Decorators see direct requests,
redirects, retries, and range requests. URL policy checks run before transport
decorators.

The trusted-host check is a transport decorator, which is what a decorator is
for: it needs to see every request rather than every asset. A decorator that
refuses a request should make the refusal permanent, by returning an error that
answers to one of the sentinels the client treats as final. Otherwise the same
refusal is re-asked once per retry and once per range.

DAC does not load plugins or define an external plugin protocol.

## Development

```bash
mise run test    # go test -race ./...
mise run vet
mise run lint
mise run vuln
mise run check
```

## Non-goals

DAC does not provide an archive format, air-gap delivery, a remote cache, a
registry, dependency resolution, signatures, or provenance records. It does
not resume partial downloads.

`dac unpack` materializes verified cache objects. It does not extract a tar,
zip, or other archive stored as an asset.
