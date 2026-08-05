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
dac trust add example.com
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

`add` resolves one asset and updates both project files. Use `add --offline` to
change only the manifest. Then, run `dac lock` to resolve the change.

`pull` installs missing locked objects. It does not change project files or
download valid cached objects.

`lock --refresh` resolves all assets and accepts the current origin bytes.
`verify --refresh` checks for the same changes without modifying the lock file.

## Commands

| Command | Purpose |
|---|---|
| `dac init [--force]` | Create empty project files. |
| `dac add <coordinate> <url> [options]` | Add one asset version. |
| `dac remove <coordinate>` | Remove one asset version. |
| `dac info [<asset>[@<version>]]` | Show project and cache state. |
| `dac lock [--refresh] [--concurrency <n>]` | Update the lock file. |
| `dac pull [<asset>[@<version>]...] [options]` | Install locked objects. |
| `dac path <asset>[@<version>]` | Print one verified object path. |
| `dac verify [--refresh] [--concurrency <n>]` | Check project files and optional origin drift. |
| `dac unpack [<asset>[@<version>]...] [options]` | Write cached assets to a directory. |
| `dac cache <dir\|list\|gc\|clear\|remove\|scrub>` | Inspect or maintain the cache. |
| `dac trust <list\|add\|remove\|gc\|path>` | Manage trusted hosts. |
| `dac config <path\|show>` | Inspect the configuration. |
| `dac completion <shell>` | Write a shell completion script. |

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

`path` accepts an asset without a version only when the asset has one version.
Otherwise, it reports `asset_ambiguous`.

Enable shell completion with:

```bash
eval "$(dac completion bash)"
```

Supported shells are Bash, Zsh, Fish, and PowerShell.

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
      "filename": "geo.db"
    }
  }
}
```

`integrity` accepts `sha256:<hex>` and SRI `sha256-<base64>`. DAC writes the
canonical form. The `add --pin` option saves the resolved digest as the manifest
integrity value.

DAC requires HTTPS but permits HTTP loopback URLs. Use
`add --allow-insecure-http` for another HTTP source. The policy also applies to
redirects.

## Trusted hosts

DAC downloads only from hosts in its trusted-hosts file. This check applies to
source URLs, redirects, retries, and split downloads.

```bash
dac trust add example.com
dac trust add https://example.com/asset
dac trust list
```

Host matches ignore case and port, but they do not include subdomains. DAC does
not support wildcards.

By default, the file is `dac/trusted-hosts.json` in the XDG data directory. Use
`dac trust path` to print the active path. The `--trust-file`, `DAC_TRUST_FILE`,
and `trust.file` settings can select a different file.

Use `dac add --trust` to trust the source host before the download. This option
does not trust redirect hosts. Use `--insecure-trust-all` to skip checks for one
run without changing the trust file.

`dac trust gc` removes hosts that exceed `trust.max-age`. The default limit is
180 days. It does not remove hosts that have no usage timestamps.

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

[trust]
file = "/var/lib/dac/trusted-hosts.json"
max-age = "180d"
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
