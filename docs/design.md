# pelican-c design notes

## Goal

Give C/C++ hosts (HTCondor first) an in-process interface to the Pelican
client: transfers, namespace operations, retryability classification, and
cancellation — without shelling out to the `pelican` binary and
re-parsing its output.

## Architecture: three layers

1. **`include/pelican/client.h`** — the public, const-correct, ABI-stable
   header. The only header installed or included by consumers.
2. **`bridge.c` / `bridge.h`** — plain C. Defines the public entry points
   as thin forwarders, the `*_free` deallocators, the opaque
   `struct pelican_context`, allocation helpers, and the progress-callback
   trampoline.
3. **`capi.go`** — the cgo implementation, exporting internal
   `pelicanc_*` symbols that the bridge forwards to.

Two constraints force this split:

- cgo cannot express `const char *` in `//export` signatures, and the
  generated prototypes in `_cgo_export.h` would conflict with a
  const-qualified public header. Internal names + C forwarders keep the
  public API const-correct.
- Files containing `//export` may not define functions in their cgo
  preamble, so the helpers Go calls (trampoline, allocators) live in a
  real C file.

## Conventions

- **Errors**: every fallible function returns `pelican_error *`; NULL is
  success. `retryable` comes from `client.ShouldRetry`, i.e. the same
  classification the CLI uses. `code` is reserved for a future mapping of
  the upstream `error_codes.PelicanError` taxonomy.
- **Memory**: all library-returned memory is malloc-family and released
  by the typed `*_free` functions (Go hands out `C.CString` /
  `calloc`-backed structs, so plain `free()` in bridge.c is correct).
  The caller owns and zero-initializes `pelican_transfer_opts`.
- **Threading**: all entry points are thread-safe after
  `pelican_client_init`. Progress callbacks arrive on library-owned
  threads and must not call back into the library.
- **Panics**: every export runs under a recover() guard; a Go panic
  surfaces as a `pelican_error` instead of aborting the host process.
- **Cancellation**: `pelican_context` wraps a Go `context.Context` via a
  `cgo.Handle` stashed in an opaque struct; `pelican_context_cancel` is
  safe from any thread.
- **Non-interactive always**: `client.WithNonInteractive(true)` is
  unconditionally applied; an embedded library must never trigger an
  OAuth device-flow prompt.
- **Version**: upstream stamps its version with ldflags, which never
  applies to module consumers; a package `init()` stamps the pelican
  module version from Go build info so `pelican_version()` and the
  HTTP User-Agent are meaningful.

## Known caveats / upstream work items

- `config.InitClient` uses `cobra.CheckErr` (→ `os.Exit`) on several
  config-load failures. The shim pre-parses a caller-supplied config file
  with a scratch viper instance to close the common hole, but rarer paths
  (e.g. a malformed file found via the default search path, or via
  `PELICAN_CONFIG_FILE`) still exit the process. Upstream fix: return
  errors from `InitClient` instead of `CheckErr`.
- `TransferResults.Source` is unpopulated on successful downloads (only
  the error path goes through `newTransferResults`); the shim falls back
  to the request URL. Worth fixing upstream, at which point recursive
  downloads get accurate per-object sources.
- One global configuration per process: `pelican_client_init` is
  once-only, and a failed init leaves the library unusable (viper global
  state cannot be safely re-initialized). Matches HTCondor's usage but
  worth documenting.
- Go runtime + `fork()` without `exec()` is unsupported; see README.

## Roadmap ideas

- Streaming reads/writes (wrap `client.WithWriter` / `WithReader` or
  `PelicanFS`) for transfer without touching local disk.
- Expose prestage (`client.DoPrestage`) and cache eviction.
- Surface checksums and ETags in `pelican_result`.
- Map `error_codes.PelicanError` numbers into `pelican_error.code`.
- pkg-config file + install target; Linux/macOS CI.
