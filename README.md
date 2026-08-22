# pelican-c

C bindings for the [Pelican Platform](https://pelicanplatform.org) client,
allowing C and C++ applications — most notably HTCondor — to perform
Pelican/OSDF object transfers in-process instead of shelling out to the
`pelican` binary.

The library is a cgo `c-shared` build: the Go client from
[pelicanplatform/pelican](https://github.com/pelicanplatform/pelican) is
compiled into a single `libpelicanclient` shared library exposing the
C API declared in [`include/pelican/client.h`](include/pelican/client.h).

Every type in the API is opaque — all access goes through getter/setter
functions, so struct layouts are never part of the ABI.

## Building

Requires Go ≥ 1.26 and a C compiler.

```sh
make                   # builds build/libpelicanclient.{so,dylib}
make example           # builds the example clients
make test              # offline smoke test
make integration-test  # in-process federation test (no XRootD needed)
make install           # PREFIX=/usr/local by default; DESTDIR supported
```

`make install` lays down the versioned shared library and its
development symlink, `include/pelican/client.h`, and a pkg-config file,
so consumers build with:

```sh
cc myapp.c $(pkg-config --cflags --libs libpelicanclient)
```

The library carries an soname, so on Linux a prefix outside the
loader's default search path needs the usual configuration at run time —
`ldconfig`, `LD_LIBRARY_PATH`, or linking the consumer with
`-Wl,-rpath,<libdir>`.  (The pkg-config file deliberately does not force
an rpath, which would interfere with distribution packaging.)  macOS
records an absolute install name, so nothing extra is required there.

CI runs the build/smoke matrix and the federation integration test —
which launches a complete in-process Pelican federation using the
pure-Go serving paths (posixv2 origin, V2 cache) and drives this
library's C API against it, with TLS fully verified against the
federation's generated CA — on Linux and macOS.

## Releases

Pushing a `v*` tag builds and attaches release archives (shared library
plus `include/pelican/client.h`) for linux/{amd64,arm64} and
darwin/{amd64,arm64}, with SHA-256 checksums.  The Linux archives are
built inside an AlmaLinux 8 container, so they link against **glibc
2.28** — running on EL8 and newer, Debian 10+, and Ubuntu 20.04+ (musl
distributions like Alpine are not covered).  A `workflow_dispatch` run
of the release workflow builds the same archives as workflow artifacts
without publishing anything.

## Quick start (synchronous)

```c
#include <pelican/client.h>

pelican_error *err = pelican_client_init(NULL);
if (err) { /* pelican_error_message(err), ..._is_retryable(err) */ }

pelican_result_list *results;
err = pelican_get(NULL,
        "pelican://osg-htc.org/ospool/uc-shared/public/OSG-Staff/validation/test.txt",
        "/tmp/test.txt", NULL, &results);
...
pelican_result_list_free(results);
```

## Asynchronous transfers for event loops

Designed for single-threaded, fd-driven daemons (HTCondor DaemonCore):
`pelican_get_start`/`pelican_put_start` return immediately, per-object
results stream as they complete (never buffered into one long list), and
a notification fd — suitable for `Register_Pipe`, `poll`, `epoll`, or
`kqueue` — becomes readable whenever there is something to collect.  No
call in the async API blocks.

```c
pelican_transfer *xfer;
err = pelican_get_start(url, local_path, NULL, &xfer);
register_fd(pelican_transfer_notify_fd(xfer));   /* your event loop */

/* fd-readable handler: */
pelican_result *res;
while (pelican_transfer_next_result(xfer, &res)) {
    /* pelican_result_source/transferred_bytes/endpoint/error ... */
    pelican_result_free(res);
}
if (pelican_transfer_is_done(xfer)) {
    const pelican_error *terr = pelican_transfer_error(xfer);
    /* handle terr, then: */
    pelican_transfer_free(xfer);
}
```

## Asynchronous namespace operations and file I/O

Everything else has a non-blocking form too, built on a single-shot
`pelican_op` handle with the same notification-fd pattern: `stat`,
`list`, `delete`, `cache_info`, `evict`, and the file calls (`open`,
`read`, `pread`, `write`, `close`).  A daemon can therefore reach the
federation without ever blocking its event loop.

```c
pelican_op *op;
err = pelican_stat_start(url, NULL, &op);
register_fd(pelican_op_notify_fd(op));   /* readable once complete */

/* handler: */
if (pelican_op_is_done(op)) {
    pelican_file_info *info;
    if (pelican_op_take_file_info(op, &info)) { /* ... */ }
    pelican_op_free(op);
}
```

Asynchronous reads and writes buffer internally, so no caller-supplied
buffer has to outlive the call, and a file handle admits one operation at
a time — a second call fails cleanly instead of corrupting the stream.

## File I/O (PelicanFS)

Remote objects can be read and written directly — sequential reads,
positional `pread` (HTTP range requests under the hood), seek, and
streamed uploads — without staging through local disk:

```c
pelican_file *f;
err = pelican_fs_open(url, PELICAN_O_RDONLY, NULL, &f);
long long n = pelican_file_pread(f, buf, sizeof(buf), offset, &err);
err = pelican_file_close(f);
```

These calls block on network I/O; from an event loop, use them where
blocking is acceptable or on worker threads/processes.

See [`examples/`](examples/) for complete programs and
[`docs/design.md`](docs/design.md) for the architecture, the
notification-fd protocol, and API conventions.

## API overview

| Area | Functions |
| --- | --- |
| Setup | `pelican_client_init`, `pelican_config_set`, `pelican_version` |
| Options | `pelican_transfer_opts_new/_free`, `..._set_token(_location)`, `..._set_source_token(_location)`, `..._set_destination_token(_location)`, `..._set_recursive`, `..._set_progress`, `..._add_cache`, `..._add_checksum_request`, `..._set_require_checksum` |
| Sync transfers | `pelican_get`, `pelican_put`, `pelican_copy` (third-party copy), `pelican_prestage` (+ `pelican_result_list_*` accessors) |
| Async transfers | `pelican_get_start`, `pelican_put_start`, `pelican_copy_start`, `pelican_prestage_start`, `pelican_transfer_notify_fd`, `..._next_result`, `..._is_done`, `..._error`, `..._cancel`, `..._free` |
| Results | `pelican_result_source/_transferred_bytes/_endpoint/_transfer_time_s/_attempts/_error/_etag/_checksum_*` |
| Namespace | `pelican_stat`, `pelican_list`, `pelican_delete` (+ `pelican_file_info_*` accessors) |
| Cache management | `pelican_cache_info`, `pelican_evict` |
| File I/O | `pelican_fs_open`, `pelican_file_read/_pread/_write/_seek/_stat/_close` |
| Async operations | `pelican_stat_start`, `pelican_list_start`, `pelican_delete_start`, `pelican_cache_info_start`, `pelican_evict_start`, `pelican_fs_open_start`, `pelican_file_read_start/_pread_start/_write_start/_close_start` |
| Operation handles | `pelican_op_notify_fd`, `..._is_done`, `..._error`, `..._cancel`, `..._free`, `..._take_file_info/_file_info_list/_file/_count/_data/_cache_info/_message` |
| Errors | `pelican_error_message`, `..._is_retryable`, `..._code`, `..._free` |

Every fallible call returns a `pelican_error *` (NULL on success) carrying
a human-readable message, a `retryable` flag derived from the client's
error classification — the flag HTCondor needs for its transfer-plugin
retry decisions — and, when classified, the numeric code from the Pelican
error taxonomy (e.g. 5011 for object-not-found).

## Notes for HTCondor integration

- Transfers always run in non-interactive mode; the library will never
  block on an OAuth device-flow prompt.
- Configuration follows the standard Pelican client rules: `pelican.yaml`
  search paths, `PELICAN_*` environment variables, and — when running
  under HTCondor with a job ad present — `PelicanCfg_*` job-ad attributes.
- Tokens can be passed per-operation (contents or file path) via
  `pelican_transfer_opts`; token discovery (e.g. `BEARER_TOKEN_FILE`,
  WLCG bearer-token conventions) otherwise applies.
- In the async API, progress callbacks are never invoked from library
  threads: they queue internally (waking the notification fd) and fire
  on your own thread from inside `pelican_transfer_next_result` —
  DaemonCore-safe.  Only the synchronous calls invoke callbacks from
  library-owned threads, since the caller is blocked.
- **Fork caution:** the Go runtime starts threads when the library is
  loaded. A `fork()` without `exec()` leaves the child's copy of the
  runtime unusable — load and use the library only in the process that
  performs transfers.

## License

Apache-2.0, matching the upstream Pelican Platform.
