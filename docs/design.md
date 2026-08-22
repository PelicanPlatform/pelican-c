# pelican-c design notes

## Goal

Give C/C++ hosts (HTCondor first) an in-process interface to the Pelican
client: transfers, namespace operations, retryability classification,
cancellation, streamed results, and direct file I/O — without shelling
out to the `pelican` binary and re-parsing its output.

## Architecture: three layers

1. **`include/pelican/client.h`** — the public, const-correct, ABI-stable
   header. Every type is opaque; all access is via functions, so no
   struct layout is part of the ABI and fields can be added freely.
2. **`bridge.c` / `bridge.h`** — plain C. bridge.h holds the real struct
   layouts (internal only); bridge.c implements everything that needs no
   Go — accessors, option setters, deallocators, allocation helpers, the
   progress-callback trampoline — plus thin forwarders into the Go
   exports for the rest.
3. **`capi.go`** — the cgo implementation, exporting internal
   `pelicanc_*` symbols that the bridge forwards to.

Two cgo constraints force this split:

- cgo cannot express `const` qualifiers in `//export` signatures, and the
  generated prototypes in `_cgo_export.h` would conflict with a
  const-qualified public header. Internal names + C forwarders keep the
  public API const-correct.
- Files containing `//export` may not define functions in their cgo
  preamble, so the helpers Go calls live in a real C file.

## Asynchronous transfers and the notification fd

The async API maps one `pelican_transfer` onto the Go client's streaming
machinery: a shared, process-lifetime `TransferEngine` (started lazily on
first use), a per-transfer `TransferClient`, and a goroutine that
submits the job and ranges over `TransferClient.Results()` — so
per-object results are consumed as the engine produces them, never
buffered into one long list.

Wakeups use a self-pipe with both ends non-blocking and close-on-exec.
The invariants:

- **Producer** (Go goroutine): under the state mutex, append the result
  to the queue (or set the done flag / terminal error) and write one
  byte to the pipe. `EAGAIN` on a full pipe is ignored — the fd is
  already readable.
- **Consumer** (`pelican_transfer_next_result`): under the same mutex,
  pop one result if available; if the queue is empty, drain the pipe
  completely and return 0. Because producers write their byte under the
  lock, any event arriving after the drain re-arms the fd.

This gives level-triggered semantics with no busy-wake: readable means
"call `next_result` until it returns 0, then check `is_done`". The
consumer never reads the pipe directly and no async call ever blocks,
which is exactly the contract a DaemonCore `Register_Pipe` handler needs.

Progress callbacks follow the same discipline: in async mode they are
never invoked from a Go thread. Reports queue on the transfer state
(coalesced per object, so an unattended queue stays bounded) and wake
the fd; `next_result` delivers them on the calling thread — with the
mutex released, so a callback may safely call back into the library —
before examining the result queue. The integration driver asserts this
with `pthread_equal` on every callback.

Lifetime safety: the Go goroutine holds the state object directly (not
via the cgo handle), so `pelican_transfer_free` during a live transfer
is safe — free marks the state, releases queued C memory, and closes the
pipe under the mutex; subsequent producer events free their payload
immediately instead of enqueueing, and never touch the closed fds.

## File I/O (PelicanFS)

`pelican_fs_open` splits the URL into a federation prefix and object
path, builds a `PelicanFS` via `NewPelicanFSWithPrefix`, and wraps the
returned `fs.File`. Reads, positional reads (`io.ReaderAt` → HTTP range
requests), seeks, and streamed writes map directly onto the PelicanFile
implementation; `pelican_file_close` finalizes uploads and reports their
outcome, so its error must always be checked.

Each open file owns its own PelicanFS — and therefore its own
`TransferEngine`, because upstream `PelicanFS` neither shares an engine
nor exposes a shutdown. The wrapper cancels the FS's context on close to
reap the engine's goroutines. Upstream fix worth pursuing: give
`PelicanFS` a `Close()` (or accept a caller-provided engine), at which
point the shim can share the global engine across opens.

## Conventions

- **Errors**: every fallible function returns `pelican_error *`; NULL is
  success. `retryable` comes from `client.ShouldRetry` — the same
  classification the CLI uses. `code` carries the upstream
  `error_codes.PelicanError` number when the error is classified
  (e.g. 5011 Specification.FileNotFound), else 0.
- **Memory**: all library-returned memory is malloc-family and released
  by the typed `*_free` functions. Strings returned by getters are
  borrowed, valid until the owning object is freed. `pelican_result_error`
  and `pelican_transfer_error` return borrowed errors.
- **Threading**: all entry points are thread-safe after
  `pelican_client_init`, except that a single `pelican_transfer` or
  `pelican_file` expects its calls serialized. Progress callbacks arrive
  on library-owned threads and must not call back into the library.
- **Blocking**: sync transfers, namespace ops, and file I/O block; the
  entire async-transfer section never does.
- **Panics**: every export runs under a recover() guard; a Go panic
  surfaces as a `pelican_error` instead of aborting the host process.
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
- `PelicanFS` lacks a shutdown/engine accessor (see above).
- `launcher_utils.CheckDefaults` → `xrootd.CheckXrootdEnv` runs the
  `xrootd -v` version gate even for servers on the pure-Go paths
  (posixv2 origin, V2 cache) that never execute XRootD; the integration
  test satisfies it with a stub script. Upstream fix: gate the check on
  backends that actually launch XRootD.
- The async path submits jobs through the raw `TransferEngine`, so it
  skips `DoGet`'s destination-layout conveniences (downloading into an
  existing directory, collection guards). Callers should pass explicit
  destination file paths.
- One global configuration per process: `pelican_client_init` is
  once-only, and a failed init leaves the library unusable (viper global
  state cannot be safely re-initialized). Matches HTCondor's usage but
  worth documenting.
- Go runtime + `fork()` without `exec()` is unsupported; see README.

## Operation coverage

Transfers (get/put), third-party copy (WebDAV COPY via `NewCopyJob`),
and prestage all exist in both synchronous and asynchronous (fd-notified)
forms; copy and prestage share the same engine-backed machinery as
get/put, differing only in job construction.  Namespace ops (stat, list,
delete), cache management (`pelican_cache_info`, `pelican_evict`), and
PelicanFS file I/O are synchronous.  Results carry the server ETag and
checksums (server-reported, falling back to client-computed), hex-encoded
with HTTP digest names.

Transfer behavior knobs cover preferred caches and checksum requests
(see below).  Deliberately not exposed: interactive token acquisition
(the library is always non-interactive), sharing-URL creation, shadow
ingest, and the `object sync` synchronization semantics.

Preferred caches and checksum requests are list-valued options, appended
one at a time (`pelican_transfer_opts_add_cache`,
`..._add_checksum_request`) so the ABI needs no array-passing convention.
Because the setters are plain C they cannot validate, so `buildOptions`
does: an unparseable cache URL or unknown digest name surfaces as an
error from the operation the options are handed to. Preferred caches
follow the client's own semantics — only the listed caches are tried
unless the list ends with the sentinel `"+"`, which appends the
director's servers; the integration test pins down all three behaviors
(honored, exclusive without `+`, fallback with `+`).

## Roadmap ideas

- Async variants of stat/list and non-blocking file I/O (read request +
  notification-fd completion), if DaemonCore ends up needing them.
- pkg-config file + install target.
