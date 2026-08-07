# DAC

DAC locks remote files and stores verified bytes in a local content-addressed
cache. Use it for data files, policy bundles, provider binaries, and other build
inputs.

DAC does not install assets or extract archives. Use the verified cache path, or
write cached files to a directory with `dac unpack`.

## Install

DAC requires Go 1.26 and a Unix system.

```bash
go install github.com/tomdoesdev/dac/cmd/dac@latest
```

Use `mise run build` to build a checkout.

## Quick start

```bash
dac init
dac add backend/geo@2026.08 \
  https://example.com/geo/2026.08/database.bin --pin --name geo.db
dac pull
dac unpack backend/geo@2026.08 --dest ./inputs
```

Commit `dac.json` and `dac-lock.json`.

An asset coordinate has this form:

```text
<namespace>/<name>@<version>
```

DAC permits multiple versions of an asset. It does not order versions or select
a latest version. Multiple coordinates can use the same cached bytes.

`add` changes only the manifest and makes no request by default. `add --pin` is
the exception: it downloads that asset to save its digest as manifest integrity,
but still leaves the lock file unchanged. The `--offline` flag can be used to
explicitly forbid the request required by `--pin`.

`pull` installs missing locked objects. It does not download valid cached
objects. It creates a missing lock file and reconciles a stale one, resolving
only the entries changed in the manifest. A current lock file is left
byte-for-byte unchanged.

`pull --refresh` resolves assets and accepts the current origin bytes, then
rewrites the lock file. Naming assets narrows which origins it reaches; the lock
file it writes still describes the whole project.

`check` is a completely offline comparison of the normalized manifest and the
complete lock file. `check upstream [<coordinate>...]` first performs that
comparison, then issues HEAD requests for the selected assets (or every asset)
and compares the locked size plus any stored ETag and Last-Modified values. It
never downloads asset bodies. Add `--verify` to download, hash, and discard each
selected asset instead; byte verification deliberately skips metadata checks and
never writes to the cache. Neither upstream form modifies project files or the
cache.

`remove` also changes only the manifest. It neither reads nor writes the lock
file, so it works when that file is missing, stale, or invalid. Run `dac pull`
after add or remove to reconcile the project. Apart from `init` creating the
initial empty project files, pull is the only command that writes lock state.

## Commands

| Command | Purpose |
|---|---|
| `dac init [--force]` | Create empty project files. |
| `dac add <coordinate> <url> [options]` | Add one asset version to the manifest; download only with `--pin`. |
| `dac remove <coordinate>` | Remove one asset version from the manifest. |
| `dac info [<asset>[@<version>]]` | Show project and cache state. |
| `dac pull [<asset>[@<version>]...] [--refresh] [options]` | Reconcile lock state and install locked objects. |
| `dac check` | Check the manifest and lock file offline. |
| `dac check upstream [<coordinate>...] [--verify] [options]` | Check upstream metadata, or download and verify locked bytes. |
| `dac unpack [<asset>[@<version>]...] [options]` | Write cached assets to a directory. |
| `dac cache <dir\|list\|gc\|clear\|remove\|scrub>` | Inspect or maintain the cache. |
| `dac config <path\|show>` | Inspect the configuration. |

Run `dac --help` or `dac <command> --help` for all flags.

### Asset selection

`info`, `pull`, and `unpack` use the same selection rules:

```bash
dac pull
dac pull backend/geo
dac pull backend/geo@2026.08
```

These commands select the full project, all versions of one asset, or one exact
version. Multiple selectors form one set. An unknown selector reports
`asset_unknown`.

## Project files

The manifest records source intent:

```json
{
  "schemaVersion": 2,
  "assets": {
    "backend/geo@2026.08": {
		"url": "https://example.com/geo.bin",
		"integrity": "sha256:optional-publisher-digest",
		"filename": "geo.db"
    }
  }
}
```

The lock file records resolved bytes:

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
      "lastModified": "Wed, 21 Oct 2015 07:28:00 GMT",
      "filename": "geo.db"
    }
  }
}
```

`integrity` accepts `sha256:<hex>` and SRI `sha256-<base64>`. DAC writes the
canonical form. The `add --pin` option saves the resolved digest as the manifest
integrity value.

DAC fetches HTTP and HTTPS sources, including redirects. URLs with embedded
credentials and schemes other than HTTP or HTTPS are rejected.

## Configuration

DAC reads XDG files named `dac/config.toml`. User settings override site
settings. `--config` or `DAC_CONFIG` selects one required file.

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

```

Command flags and these environment variables override configured values:

- `DAC_CONCURRENCY`
- `DAC_CACHE_DIR`
- `DAC_COLOR`
- `DAC_DEBUG`

DAC also supports `NO_COLOR`, `CLICOLOR`, and `CLICOLOR_FORCE` in automatic
color mode. Run `dac config show` to print the effective settings.

## Cache and unpack

DAC verifies each downloaded object against its locked digest and size. It
stores objects by SHA-256 digest, so multiple coordinates can share one object.

`dac cache gc` removes old objects and applies a size limit. `dac cache scrub`
hashes objects and can repair cache metadata. `dac pull` restores missing or
damaged objects.

### The object catalog

An object is named on disk by its digest and nothing else, so a project that is
edited or deleted takes with it the only record of what its objects were. DAC
keeps that record apart from the project, in `catalog.json` at the root of the
cache it describes: `~/.cache/dac/catalog.json` by default, and beside the
objects of whichever cache `--cache-dir`, `DAC_CACHE_DIR`, or `cache.dir`
selects. Collection walks the object directories and never the root, so
emptying a cache does not take its catalog with it.

Each record holds the size of an object, when DAC first stored it, and every
coordinate it has been seen under, each with its own URL, file name, and time
last seen. DAC adds to it when it downloads an object, and refreshes it whenever
a command reads a project. A record is dropped when `dac cache gc`,
`dac cache remove`, or `dac cache clear` removes the object it describes.

`dac cache list --all` reports what the catalog knows, and runs in a directory
with no project at all. That is the command to run against a cache full of
objects nothing accounts for: it names each one and reports how many it still
cannot explain.

The catalog is a description rather than a decision, so nothing fails when it
cannot be read or written. Delete the file to start it again.

`dac unpack` uses only verified cache objects and never uses the network. It
selects each output name from the manifest filename, lock filename, or
coordinate name. It rejects unsafe names and name collisions.

Existing files require `--force`. DAC stages and verifies all selected files
before it replaces destinations. If a replacement fails, DAC restores the
previous files.

## Output

Human and JSON results use standard output. Errors, progress, help, and version
information use standard error.

`--json` writes one JSON document with output contract version 8. Failures
include a stable `code`, a `message`, optional `cause`, and `details`.

Exit status `0` means success. Status `1` means a command failure. Status `2`
means a usage error.

## Development

```bash
mise run test
mise run vet
mise run lint
mise run vuln
mise run check
```
