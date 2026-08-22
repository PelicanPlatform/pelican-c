# pelican-c

C bindings for the [Pelican Platform](https://pelicanplatform.org) client,
allowing C and C++ applications — most notably HTCondor — to perform
Pelican/OSDF object transfers in-process instead of shelling out to the
`pelican` binary.

The library is a cgo `c-shared` build: the Go client from
[pelicanplatform/pelican](https://github.com/pelicanplatform/pelican) is
compiled into a single `libpelicanclient` shared library exposing the
C API declared in [`include/pelican/client.h`](include/pelican/client.h).

## Building

Requires Go ≥ 1.26 and a C compiler.

```sh
make            # builds build/libpelicanclient.{so,dylib}
make example    # builds the example client
make test       # offline smoke test
```

## Quick start

```c
#include <pelican/client.h>

pelican_error *err = pelican_client_init(NULL);
if (err) { /* err->message, err->retryable; pelican_error_free(err) */ }

pelican_result *results; size_t n;
err = pelican_get(NULL,
        "pelican://osg-htc.org/ospool/uc-shared/public/OSG-Staff/validation/test.txt",
        "/tmp/test.txt", NULL, &results, &n);
...
pelican_results_free(results, n);
```

See [`examples/pelican_example.c`](examples/pelican_example.c) for stat,
list, progress callbacks, and cancellation contexts, and
[`docs/design.md`](docs/design.md) for the architecture and API
conventions.

## API overview

| Function | Purpose |
| --- | --- |
| `pelican_client_init` / `pelican_config_set` | one-time library setup |
| `pelican_get` / `pelican_put` | object download / upload, with per-object results |
| `pelican_stat` / `pelican_list` / `pelican_delete` | namespace operations |
| `pelican_context_new` / `_cancel` / `_free` | cross-thread cancellation |
| `pelican_error_free`, `pelican_results_free`, … | deallocation |

Every fallible call returns a `pelican_error *` (NULL on success) carrying
a human-readable message and a `retryable` flag derived from the client's
error classification — the flag HTCondor needs for its transfer-plugin
retry decisions.

## Notes for HTCondor integration

- Transfers always run in non-interactive mode; the library will never
  block on an OAuth device-flow prompt.
- Configuration follows the standard Pelican client rules: `pelican.yaml`
  search paths, `PELICAN_*` environment variables, and — when running
  under HTCondor with a job ad present — `PelicanCfg_*` job-ad attributes.
- Tokens can be passed per-operation (contents or file path) via
  `pelican_transfer_opts`; token discovery (e.g. `BEARER_TOKEN_FILE`,
  WLCG bearer-token conventions) otherwise applies.
- **Fork caution:** the Go runtime starts threads when the library is
  loaded. A `fork()` without `exec()` leaves the child's copy of the
  runtime unusable — load and use the library only in the process that
  performs transfers.

## License

Apache-2.0, matching the upstream Pelican Platform.
