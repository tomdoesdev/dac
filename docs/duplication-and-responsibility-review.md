# dac: duplication & misplaced-responsibility review + refactor plan

## Context

`dac` is ~2,600 lines of non-test Go outside `kit/`, split into `cmd/dac/command`
and five `internal/` packages. The package doc comments describe clean, narrow
responsibilities, but the actual code has drifted: several packages have grown a
second, unrelated job, and a handful of rules (HTTP header case-folding, digest
parsing, existence checks, CLI command syntax) are implemented in three or four
places each.

The review below catalogues every instance found. The refactor plan that follows
is staged so each stage is independently shippable and independently revertable.

**Two structural problems account for most of the findings:**

1. `internal/project` is two packages wearing one name — the project path layout
   *and* the error taxonomy for the entire program. Because the error taxonomy
   lives there, **every other internal package imports `internal/project`**,
   including `internal/asset`, which otherwise has nothing to do with project
   layout. There is no dependency inversion anywhere; the graph is a straight
   line with `project` at the bottom.

2. Library packages know the `dac` CLI exists. `internal/lockfile` formats shell
   commands (`run: dac lock -- 'name'`) and implements POSIX shell quoting.
   `internal/project` hardcodes ``run `dac init` ``. This is the exact shape the
   review was asked to look for.

---

# Part 1 — Findings

## A. Misplaced responsibility

### A1. `internal/project` owns the whole program's error taxonomy
`internal/project/error.go:1-121` defines `Error`, `ErrorKind`, six constructors
and three options. `internal/project/doc.go:1-3` admits the split responsibility
outright: *"discovers dac projects, owns their persistent path layout, **and**
defines the operation errors shared across dac's internal packages."*

Consequence — every internal package imports it, whether or not it cares about
project paths:

| Package | Imports `project` for | Cares about project layout? |
|---|---|---|
| `internal/asset` (`download.go:16`) | 16 error constructions | No — it's an HTTP/digest package |
| `internal/manifest` (`manifest.go:15`, `resolve.go:10`) | 25 error constructions | No |
| `internal/lockfile` (`lockfile.go:12`, `current.go:10`) | 14 error constructions | No |
| `internal/output` (`output.go:13`) | error taxonomy + `Paths` | Only reads `Paths.Root` |

`internal/asset` is the clearest violation: a transport package that streams
bytes and hashes them has a compile-time dependency on the package that knows
where `dac.toml` lives.

### A2. `internal/lockfile` formats CLI commands and does shell quoting
`internal/lockfile/current.go:122-133`:

```go
func Hint(name string) string {
    if name == "" { return "run `dac lock --all`" }
    return fmt.Sprintf("run: dac lock -- %s", shellQuote(name))
}
func shellQuote(value string) string {
    return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
```

The lock-state package implements POSIX shell single-quote escaping and knows
`dac lock`'s flag surface. `Hint` is exported but has exactly one caller —
`staleError` on line 119 of the same file — so the export buys nothing.

CLI command syntax is hardcoded in **four** places across three packages:
- `internal/project/paths.go:83` — ``run `dac init` ``
- `internal/lockfile/lockfile.go:41` and `:93` — ``run `dac lock --all` ``
- `internal/lockfile/current.go:125,127` — ``run `dac lock --all` `` and `dac lock -- <name>`
- `cmd/dac/command/lock.go:65` — ``run `dac lock --all` `` (the only legitimate one)

### A3. `internal/output` parses URLs and redacts credentials
`internal/output/output.go:17` declares `var urlToken = regexp.MustCompile(...)`
and `:262-276` regex-scans arbitrary error text for URLs, parses each match, and
strips userinfo/query/fragment. `:161-169` parses a URL again to extract a
hostname for the throbber message.

A rendering package is the last line of defence for secret leakage. That is
backwards: the redaction contract belongs next to the code that puts URLs into
error strings (`asset`, `manifest`), or in a shared `internal/redact`. As
written, any new error path that embeds a credentialed URL is safe only by
accident of `output` catching it downstream.

### A4. `internal/output` decides process exit codes
`internal/output/output.go:214-245` — `Writer.Error` returns `2` for usage and
configuration errors, `1` otherwise, and `cmd/dac/main.go:19,37` feeds that
straight into `os.Exit`. Exit-status mapping is a process-boundary policy, not a
rendering concern; a writer should write.

### A5. `cmd/dac/command` owns multi-file transaction machinery
`cmd/dac/command/lock.go:228-299` — `lockTransaction`, `commit`, `rollback` — is
~70 lines of reversible-commit orchestration over `atomic.Commit` living in the
CLI handler package. `cleanupObsoleteDownloads` (`:158-202`) does `Lstat` /
`Remove` / directory `Sync` against `*os.Root`, i.e. garbage-collects the
downloads directory that `internal/project` owns.

`statusOrphans` (`cmd/dac/command/status.go:91-116`) similarly does
`fs.ReadDir` on the downloads root and encodes the "referenced by lock" rule.

### A6. `runtime.calculatePin` is a domain workflow on a CLI struct
`cmd/dac/command/runtime.go:43-64` — open downloads, download, capture digest,
discard bytes. This is the trust-on-first-use protocol, shared by `add` and
`update`, sitting on the CLI dependency-injection struct.

### A7. `internal/asset` resolves environment variables
`internal/asset/http.go:104-115` and `:124-142` implement dac's `env:VARNAME`
header-value indirection — a *manifest configuration* syntax — inside the HTTP
transport package. `asset` calls `os.LookupEnv`. The header *name* grammar is
genuinely HTTP and belongs here; the `env:` value scheme is dac configuration.

### A8. `internal/asset` parses human-authored size/duration strings
`internal/asset/policy.go:40-81` — `ParseMaxSize` ("4GiB", "500mb") and
`ParseIdleTimeout` parse strings that only ever come from `dac.toml` or a CLI
flag. The `TransferPolicy` *values* belong to `asset`; the *string grammar* is a
configuration concern, which is why `manifest` and `command` both have to reach
into `asset` to validate flags (see B4).

### A9. `internal/lockfile.Validate` mutates its argument
`internal/lockfile/lockfile.go:105-115` normalizes each digest and writes it back
via `value.Files[name] = file`. A function named `Validate` silently rewrites the
caller's map. `Load` (`:50`), `Stage` (`:71`) and `Evaluate`
(`current.go:39`) all call it, so normalization happens on paths that only meant
to check.

---

## B. Duplication

### B1. Two identical "dac.toml already exists" errors
- `internal/manifest/errors.go:9` — `ErrAlreadyExists = errors.New("dac.toml already exists")`
- `cmd/dac/command/errors.go:9` — `ErrManifestAlreadyExists = errors.New("dac.toml already exists")`

Byte-identical messages in two packages. Worse, the *check* is duplicated too:
`cmd/dac/command/init.go:28-32` does an `os.Stat` pre-check and returns the
`command` one, while `manifest.Create` (`manifest.go:90-97`) does the real
race-free `CommitNoReplace` check and returns the `manifest` one. Two code paths,
two error values, one condition.

### B2. Colliding error-variable names across packages
| Name | Locations | Note |
|---|---|---|
| `ErrInvalidDigest` | `asset/digest.go:15`, `lockfile/errors.go:19` | lockfile's wraps asset's |
| `ErrInvalidResolvedURL` | `manifest/errors.go:31`, `lockfile/errors.go:17` | lockfile's wraps manifest's |
| `ErrInvalidHeader` | `asset/http.go:21`, `command/errors.go:38` | unrelated meanings, same name |
| `ErrDecode` | `manifest/errors.go:7`, `lockfile/errors.go:9` | parallel, arguably fine |
| `ErrUnsupportedVersion` | `manifest/errors.go:11`, `lockfile/errors.go:11` | parallel, arguably fine |

### B3. Every flag value is parsed twice; some three times
`add` and `update` parse in `Validate()`, throw the result away, then parse again
in `Run()`:

- `cmd/dac/command/add.go:38,41` (Validate) → `:70,74` (Run) — `parseSets`, `parseHeaders`
- `cmd/dac/command/update.go:55,59,68,72` (Validate) → `:156,166,176,189` (in `apply`) — all four parsers

Then `manifest.Validate` parses the *same* strings a third time on write
(`manifest.go:129-142`), and `manifest.Resolve` parses max-size/idle-timeout a
fourth time (`resolve.go:57,60`) — that time discarding errors with `_` because
`Validate` already ran. The `_` is load-bearing: if `Validate` and `Resolve` ever
disagree, `Resolve` silently produces a zero policy (meaning *no limit*).

### B4. `ParseMaxSize` / `ParseIdleTimeout` called from three layers
`asset/policy.go` is called by `command/add.go:45,50`, `command/update.go:82,87`,
`manifest/manifest.go:134,139`, and `manifest/resolve.go:57,60` — CLI, validation
and resolution each independently re-deriving the same value.

### B5. HTTP header case-insensitivity implemented in four packages
| Location | Mechanism |
|---|---|
| `asset/http.go:86` | `strings.ToLower` for duplicate tracking |
| `command/runtime.go:100` | `strings.ToLower` in `parseHeaders` |
| `command/runtime.go:136` | `strings.ToLower` in `parseHeaderNames` |
| `command/update.go:77` | `strings.ToLower` for conflict detection |
| `command/update.go:238-244` | `strings.EqualFold` in `headerKey` |
| `lockfile/configuration.go:46` | `strings.ToLower` via `writeDigestMap(foldKeys: true)` |

The CLI package and the persistence package both encode HTTP's identity rules.
`command/runtime.go:130-143` is the worst of it — `parseHeaderNames` validates a
bare header *name* by constructing a throwaway map
(`asset.ValidateHeaders(map[string]string{value: ""})`) because `asset`'s
`validHeaderName` (`http.go:117`) is unexported.

### B6. Four near-identical "parse repeated flag into a deduped set" functions
`cmd/dac/command/runtime.go:72-143` — `parseSets`, `parseHeaders`,
`parseVariableNames`, `parseHeaderNames`. All four: iterate, split or validate,
detect duplicates, return a map. Two return `map[string]string`, one
`map[string]bool`, one `map[string]string` used as normalized→original.

### B7. Name→bool set construction repeated five times over the same collection
- `command/lock.go:100-103` — `validNames`
- `command/lock.go:205-209` — `known` in `selectedNames`
- `command/status.go:102-105` — `referenced`
- `command/lock.go:161-164` — `retained` in `cleanupObsoleteDownloads`
- `lockfile/current.go:43-45` — `manifestNames` in `Evaluate`

`manifest.Resolve` returns a `[]ResolvedAsset`, so every consumer that needs
lookup rebuilds an index — including `findResolved` (`runtime.go:145-152`), a
linear scan that returns a **zero-value struct on miss** with no `ok` flag.
`add.go:100` and `update.go:126` both consume that result without checking.

### B8. Lockfile existence is checked three times in one function
`cmd/dac/command/lock.go:135-156` — `os.Lstat(paths.Lockfile())`,
`lockfile.LoadOptional` (which itself does an `os.Stat`, `lockfile.go:59`), then
`os.Stat(paths.Lockfile())` again on the error path. Three stats, three
`errors.Is(err, fs.ErrNotExist)` branches, for one question.

### B9. `Paths.OpenDownloads` / `OpenDownloadsOptional` are near-copies
`internal/project/paths.go:29-62` — same open-root, same
`filepath.Join(".dac", "downloads")`, same error wrapping; they differ only in
whether they `MkdirAll` and how they treat `ErrNotExist`. The `.dac/downloads`
literal appears three times in the file (`:23`, `:35`, `:54`) with no constant.

### B10. Two quoting schemes for the same thing
`cmd/dac/command/pull.go:98-104` quotes asset names for human errors with
`strconv.Quote`; `internal/lockfile/current.go:131-133` quotes asset names for
human errors with POSIX shell rules. Same data, same audience, different output.

### B11. Copy-and-hash implemented twice in `internal/asset`
`digest.go:38-45` `copyWithDigest` and `download.go:189-205`
`copyResponseWithDigest` both do `sha256.New()` + `io.MultiWriter` + `io.Copy` +
`formatSHA256Digest`. The latter superseded the former, which is now dead (D1).

### B12. `Writer.Error` re-creates a styler it already holds
`internal/output/output.go:215-216` takes a `stderr io.Writer` parameter and
builds `cli.NewStyler(stderr, cli.ColorAuto)` — despite `Writer` already holding
`writer.stderr` (`:77`) and `writer.stderrStyler` (`:79`). `main.go:37` passes
the very same `stderr` the writer was constructed with. The parameter and the
second styler are both redundant, and they can silently diverge.

### B13. `ValidateOptions` boilerplate repeated in all six commands
`internal/output/output.go:101` is a one-line pass-through to
`options.Validate()`, and every command's `Validate()` opens with it —
`init.go:19`, `add.go:35`, `update.go:46`, `lock.go:26`, `pull.go:25`,
`status.go:19`. Global option validation is being hand-dispatched per command.

---

## C. Hidden state / smaller issues

### C1. `output.Writer` keeps cross-call state to dedupe its own output
`internal/output/output.go:82` — `reportedDownloads map[string]bool`, written by
the throbber's end callback (`:149`) and read by `Success` (`:114`) to suppress a
duplicate line. The renderer's output now depends on hidden history, and `Writer`
is no longer safe to share across concurrent operations.

### C2. `output` takes `project.Paths` to read one field
`Success` (`:104`) and `Status` (`:182`) accept a `project.Paths` and use only
`paths.Root` (`:110`, `:184`).

### C3. `Writer` has unreachable configuration
`throbberMode` and `throbberInterval` (`:80-81`) are set once in `New` (`:93-94`)
and never varied — fields pretending to be options.

## D. Dead code (verified: no non-test callers)

| Symbol | Location | Status |
|---|---|---|
| `Paths.Downloads()` | `internal/project/paths.go:23` | Zero references anywhere, including tests |
| `copyWithDigest` | `internal/asset/digest.go:38` | Only `digest_test.go:42` |
| `stageResponse` | `internal/asset/download.go:145` | Only `download_test.go:177` |
| `ErrLockSelectionRequired` | `cmd/dac/command/errors.go:16` | Self-documented as deprecated, zero references |
| `EntryState.Exists` | `internal/lockfile/current.go:25` | Set at `:47`, never read |
| `lockfile.Hint` | `internal/lockfile/current.go:123` | Exported; sole caller is `:119` in the same file |

---

# Part 2 — Staged refactor plan

Each stage compiles and passes tests on its own. Stages 1–2 are mechanical;
3–5 move package boundaries; 6 is optional polish.

## Stage 1 — Delete dead code and collapse duplicate errors

Low risk, shrinks the surface before anything moves.

1. Delete every symbol in table **D**, plus the tests that exist only to cover
   `copyWithDigest` and `stageResponse` (rewrite `download_test.go:177`'s case to
   drive `stageResponseWithPolicy` directly).
2. Unexport `lockfile.Hint` → `hint`.
3. Delete `command.ErrManifestAlreadyExists`; drop the `os.Stat` pre-check in
   `cmd/dac/command/init.go:28-32` and let `manifest.Create`'s existing
   `CommitNoReplace` path be the single source of truth (it is already race-free
   and already returns `manifest.ErrAlreadyExists`).
4. Rename `command.ErrInvalidHeader` → `ErrHeaderFormat` to stop colliding with
   `asset.ErrInvalidHeader`.

**Files:** `internal/project/paths.go`, `internal/asset/digest.go`,
`internal/asset/download.go`, `internal/lockfile/current.go`,
`cmd/dac/command/errors.go`, `cmd/dac/command/init.go`.

## Stage 2 — Parse once, pass the parsed value

Fixes B3/B4 and removes the load-bearing `_` in `resolve.go`.

1. Give `command` a per-command `parsedOptions` struct built **once**, in
   `Validate()`, stored on the command, and consumed by `Run()`. `add.Validate`
   and `update.Validate` already do all the parsing — keep the results instead of
   discarding them (`add.go:38-53`, `update.go:55-90`). `update.apply`
   (`:149-218`) then takes the parsed maps as parameters instead of re-parsing.
2. Move max-size/idle-timeout **parsing** out of `manifest.Resolve`: have
   `manifest.Validate` (or a new `parseTransfer`) return the parsed
   `asset.TransferPolicy`, and have `Resolve` use it. No `_`-discarded parse
   anywhere.
3. Export `asset.ValidHeaderName` (currently `validHeaderName`, `http.go:117`) so
   `command.parseHeaderNames` can stop building a throwaway map to validate one
   name.

**Files:** `cmd/dac/command/{add,update,runtime}.go`,
`internal/manifest/{manifest,resolve}.go`, `internal/asset/http.go`.

## Stage 3 — Split `internal/project` into `internal/fault` + `internal/project`

The highest-payoff change; unblocks 4 and 5.

1. Create `internal/fault` (name it `oops`/`failure`/`derr` to taste) holding the
   entire contents of `internal/project/error.go` — `Error`, `ErrorKind`, the six
   `New*Error` constructors, `WithAsset`/`WithHint`/`WithIntegrity`. Pure
   dependency-free package.
2. `internal/project` keeps only `paths.go` + `errors.go` (`ErrProjectNotFound`,
   `ErrFlockUnsupported`) and imports `fault` like everyone else.
3. Mechanical rename across all 13 import sites: `project.NewXError` →
   `fault.NewXError`, `project.WithAsset` → `fault.WithAsset`, `*project.Error` →
   `*fault.Error`. `internal/asset` then has **no** dependency on project layout.
4. Update `internal/project/doc.go` to state one responsibility.

**Files:** new `internal/fault/`, `internal/project/{doc,error,error_test}.go`,
and every file in the A1 table.

## Stage 4 — Move CLI presentation out of the library packages

Fixes A2, A3, A4, B10.

1. **Hints.** Replace the free-text `WithHint(string)` with a small typed
   recovery value — e.g. `fault.WithRecovery(fault.Recovery{Command: "lock",
   Args: []string{name}})` — and let `cmd/dac/command` (or `internal/output`)
   render it into `run: dac lock -- 'name'`. Delete `shellQuote` and `Hint` from
   `internal/lockfile`; delete the hardcoded ``run `dac …` `` strings from
   `lockfile/lockfile.go:41,93` and `project/paths.go:83`.
   Consolidate with `command/pull.go`'s `quoteAssetNames` so asset names in
   user-facing text are quoted exactly one way.
2. **Redaction.** Move `sanitizeError` + `urlToken` (`output.go:17,262-276`) into
   `internal/redact`, and call it where credentialed URLs *enter* error text —
   `asset/download.go`'s transport paths and `manifest`'s decode path — instead of
   scanning finished strings at the render boundary. Keep the render-time call as
   defence-in-depth if you prefer, but it should no longer be the only guard.
3. **Exit codes.** Change `Writer.Error` to return only an error, expose the kind
   via the existing `operation.Kind()`, and map kind → exit status in
   `cmd/dac/main.go` (`Run`). While there, drop `Error`'s redundant `stderr`
   parameter and the second `cli.NewStyler` (B12) in favour of `writer.stderr` /
   `writer.stderrStyler`.

**Files:** `internal/fault/`, `internal/lockfile/{current,lockfile}.go`,
`internal/project/paths.go`, new `internal/redact/`, `internal/output/output.go`,
`cmd/dac/main.go`, `cmd/dac/command/pull.go`.

## Stage 5 — Move state management out of `cmd/dac/command`

Fixes A5, A6, B7, B8, B9.

1. Create `internal/workspace` (or extend `project`) owning the downloads
   directory as a *thing*, not a path: fold
   `OpenDownloads`/`OpenDownloadsOptional` into one
   `OpenDownloads(create bool)` behind a `downloadsDir` constant (B9), and give
   it the two behaviours currently written inline in `command`:
   - `PruneObsolete(previous, next)` ← `command/lock.go:158-202`
   - `ListUnreferenced(referenced)` ← the `fs.ReadDir` half of
     `command/status.go:91-116`
   Both need the same "files referenced by the lock" set — build it once there.
2. Move `lockTransaction` / `commit` / `rollback` (`command/lock.go:228-299`)
   into an `internal/lockfile` (or `internal/workspace`) transaction type. The
   `lock` handler should read as: resolve → download → stage → commit → report.
3. Move `runtime.calculatePin` (`command/runtime.go:43-64`) next to the
   downloader as e.g. `asset.CalculateDigest(ctx, downloads, request)`, with the
   progress callback injected so `asset` still does not import `output`.
4. Collapse `lockCommand.loadCurrent`'s three existence checks (B8) into a single
   `lockfile.LoadOptional` call plus one branch on `exists`.
5. Have `manifest.Resolve` return a type with a `Get(name) (ResolvedAsset, bool)`
   lookup (keeping ordered iteration), then delete `findResolved`
   (`runtime.go:145-152`) and the repeated set-building at `lock.go:100-103,
   205-209`, `status.go:102-105`, `lockfile/current.go:43-45`.

**Files:** new/extended `internal/workspace`, `internal/lockfile/`,
`internal/asset/download.go`, `internal/manifest/resolve.go`,
`cmd/dac/command/{lock,status,runtime}.go`.

## Stage 6 — Optional: tighten the remaining seams

- **A7** — move the `env:` header-value scheme out of `internal/asset/http.go`
  into the configuration layer: `manifest` (or a `internal/secret` resolver)
  resolves `env:` references and hands `asset` a plain `map[string]string`.
  `asset` keeps `ValidateHeaders`' HTTP name/value grammar and stops calling
  `os.LookupEnv`. Note the constraint in `http.go:124-125` — resolution must
  still happen immediately before the request, so this is an injected resolver
  func, not eager resolution.
- **A8** — move `ParseMaxSize`/`ParseIdleTimeout` (string grammar) into
  `manifest`, leaving `TransferPolicy` + `DefaultTransferPolicy` (values) in
  `asset`. Depends on Stage 2 already having centralized the parse.
- **A9** — split `lockfile.Validate` into a pure `Validate` and an explicit
  `Normalize`, so validation stops mutating the caller's map.
- **B5/B6** — one `headerSet` helper owning HTTP header identity, used by
  `command`'s four parsers and by `lockfile/configuration.go:46`'s `foldKeys`
  path. Collapse `parseSets`/`parseVariableNames` and
  `parseHeaders`/`parseHeaderNames` into two generic "parse repeated flag,
  reject duplicates" helpers.
- **C1** — replace `reportedDownloads` with an explicit flag on `output.Result`
  (e.g. `AlreadyReported bool`) set by the caller, so `Success` is a pure
  function of its arguments.
- **C2** — change `Success`/`Status` to take `root string` instead of
  `project.Paths`, dropping `output`'s dependency on `project` entirely.
- **C3** — either accept `ThrobberMode`/interval as `New` options or inline the
  constants.

---

# Verification

No behaviour change is intended by any stage. The existing suite is the contract:

```sh
go build ./...
go vet ./...
go test ./...            # cmd/dac/main_test.go (538 lines) is the end-to-end net
```

`cmd/dac/main_test.go` drives `Run(ctx, args, stdout, stderr)` with isolated
streams and asserts exit codes and output, so it covers Stage 4's exit-code move
and Stage 3's error-type rename directly.

Per-stage additions:

- **Stage 1** — confirm `go vet ./...` reports no unused symbols and
  `grep -rn "ErrManifestAlreadyExists\|copyWithDigest\|Paths.Downloads" .`
  returns nothing outside `kit/`.
- **Stage 2** — add a `manifest.Resolve` test with a valid-but-unusual
  `max_size` proving the parsed policy reaches `TransferPolicy` (guards the
  removed `_` discard).
- **Stage 3** — pure rename; `go build ./...` plus the full suite is sufficient.
  Assert the boundary holds: `go list -deps ./internal/asset` must not contain
  `internal/project`.
- **Stage 4** — extend `internal/output/output_test.go`'s redaction case to the
  new `internal/redact` package, and add a `main_test.go` case asserting exit
  code 2 for a configuration error and 1 for an integrity error.
- **Stage 5** — the existing lock/status tests cover prune and orphan listing;
  add a direct unit test for `PruneObsolete` now that it is reachable outside the
  CLI package.

Manual smoke test against the checked-in `dac.toml` (its `test` entry points at
a real GitHub raw URL; the two `127.0.0.1:8080` entries need `dacserve.json`'s
local server or should be removed from the working copy first):

```sh
go run ./cmd/dac init      # in a scratch dir
go run ./cmd/dac add test https://raw.githubusercontent.com/github/gitignore/main/Go.gitignore
go run ./cmd/dac lock
go run ./cmd/dac pull
go run ./cmd/dac status
go run ./cmd/dac status --json
go run ./cmd/dac pull --offline
```
