/**
 * pelican/client.h - C interface to the Pelican Platform client
 *
 * A thin, ABI-stable C wrapper around the Go client library from
 * github.com/pelicanplatform/pelican, intended for embedding Pelican
 * transfers into C/C++ applications such as HTCondor.
 *
 * Conventions:
 *  - Every function that can fail returns a `pelican_error *`; NULL means
 *    success.  Errors must be released with pelican_error_free().
 *  - All output objects (results, file infos) are allocated by the library
 *    and released with the matching *_free() function.  The caller never
 *    allocates library structs except pelican_transfer_opts, which is
 *    caller-owned and must be zero-initialized (memset or `= {0}`) so that
 *    unset fields pick up defaults.
 *  - All functions are thread-safe once pelican_client_init() has returned.
 *  - Strings are NUL-terminated UTF-8.
 *
 * Copyright (C) 2026, Pelican Project, Morgridge Institute for Research
 * SPDX-License-Identifier: Apache-2.0
 */

#ifndef PELICAN_CLIENT_H
#define PELICAN_CLIENT_H

#include <stddef.h> /* size_t */

#ifdef __cplusplus
extern "C" {
#endif

/* ------------------------------------------------------------------ *
 * Errors                                                             *
 * ------------------------------------------------------------------ */

typedef struct pelican_error {
    char *message;   /* human-readable description; never NULL */
    int   retryable; /* nonzero if retrying the operation may succeed */
    int   code;      /* reserved for future error taxonomy; currently 0 */
} pelican_error;

/** Release an error returned by any pelican_* call.  NULL is a no-op. */
void pelican_error_free(pelican_error *err);

/* ------------------------------------------------------------------ *
 * Library setup                                                      *
 * ------------------------------------------------------------------ */

/**
 * Set a Pelican configuration parameter (e.g. "Client.WorkerCount",
 * "Federation.DiscoveryUrl") before initialization.  Returns an error if
 * called after pelican_client_init().
 */
pelican_error *pelican_config_set(const char *key, const char *value);

/**
 * Initialize the client library.  Must be called once, before any other
 * operation (except pelican_config_set / pelican_version).  Subsequent
 * calls are no-ops returning NULL.
 *
 * config_file: optional path to a pelican.yaml; pass NULL to use the
 * default search paths and PELICAN_* environment variables.
 */
pelican_error *pelican_client_init(const char *config_file);

/** Version string of the underlying Pelican client (static; do not free). */
const char *pelican_version(void);

/* ------------------------------------------------------------------ *
 * Cancellation contexts                                              *
 * ------------------------------------------------------------------ */

/**
 * A cancellation handle.  Optional: pass NULL wherever a
 * pelican_context* is accepted to run without external cancellation.
 * pelican_context_cancel() may be called from any thread; in-flight
 * operations using the context return promptly with a non-retryable
 * "context canceled" error.
 */
typedef struct pelican_context pelican_context;

pelican_context *pelican_context_new(void);
void pelican_context_cancel(pelican_context *ctx);
/** Free the context.  Do not free while operations using it are in flight. */
void pelican_context_free(pelican_context *ctx);

/* ------------------------------------------------------------------ *
 * Transfer options                                                   *
 * ------------------------------------------------------------------ */

/**
 * Progress callback.  Invoked periodically during a transfer from a
 * library-owned thread (NOT the calling thread); it must be thread-safe
 * and must not call back into the library.
 *
 * object:      remote object path being transferred
 * transferred: bytes moved so far
 * total:       total bytes, or -1 if unknown
 * completed:   nonzero when this object's transfer has finished
 */
typedef void (*pelican_progress_fn)(const char *object,
                                    long long transferred,
                                    long long total,
                                    int completed,
                                    void *user_data);

/**
 * Options accepted by transfer and namespace operations.  Caller-owned;
 * zero-initialize, then set what you need.  The struct may grow in
 * future releases, so always memset to 0 rather than setting fields
 * positionally.
 */
typedef struct pelican_transfer_opts {
    const char *token;          /* bearer token contents, or NULL          */
    const char *token_location; /* path to a token file, or NULL           */
    int         recursive;      /* transfer/delete collections recursively */
    pelican_progress_fn progress;      /* progress callback, or NULL       */
    void               *progress_data; /* opaque pointer passed to callback */
} pelican_transfer_opts;

/* ------------------------------------------------------------------ *
 * Transfers                                                          *
 * ------------------------------------------------------------------ */

/** Result of one object transfer (a recursive job yields one per object). */
typedef struct pelican_result {
    char          *source;            /* remote object path                */
    long long      transferred_bytes; /* bytes moved                       */
    char          *endpoint;          /* host:port actually used, or NULL  */
    double         transfer_time_s;   /* wall time of the last attempt     */
    int            attempts;          /* number of attempts made           */
    pelican_error *error;             /* per-object failure, NULL if OK    */
} pelican_result;

/** Free an array of `n` results returned by pelican_get/pelican_put. */
void pelican_results_free(pelican_result *results, size_t n);

/**
 * Download `remote_url` (e.g. "pelican://osg-htc.org/ospool/.../file")
 * to `local_path`.  On success — and on per-object failure inside a
 * recursive transfer — `*results`/`*n_results` describe each object
 * transferred; free with pelican_results_free().  `results`/`n_results`
 * may be NULL if the caller does not want per-object detail.
 */
pelican_error *pelican_get(pelican_context *ctx,
                           const char *remote_url,
                           const char *local_path,
                           const pelican_transfer_opts *opts,
                           pelican_result **results,
                           size_t *n_results);

/** Upload `local_path` to `remote_url`.  Same result semantics as get. */
pelican_error *pelican_put(pelican_context *ctx,
                           const char *local_path,
                           const char *remote_url,
                           const pelican_transfer_opts *opts,
                           pelican_result **results,
                           size_t *n_results);

/* ------------------------------------------------------------------ *
 * Namespace operations                                               *
 * ------------------------------------------------------------------ */

typedef struct pelican_file_info {
    char     *name;          /* object name                       */
    long long size;          /* size in bytes                     */
    long long mtime;         /* modification time (Unix seconds)  */
    int       is_collection; /* nonzero for directories           */
} pelican_file_info;

void pelican_file_info_free(pelican_file_info *info);
void pelican_file_info_list_free(pelican_file_info *infos, size_t n);

/** Stat a remote object.  On success *info must be freed with
 *  pelican_file_info_free(). */
pelican_error *pelican_stat(pelican_context *ctx,
                            const char *remote_url,
                            const pelican_transfer_opts *opts,
                            pelican_file_info **info);

/** List a remote collection.  On success *infos (length *n_infos) must be
 *  freed with pelican_file_info_list_free(). */
pelican_error *pelican_list(pelican_context *ctx,
                            const char *remote_url,
                            const pelican_transfer_opts *opts,
                            pelican_file_info **infos,
                            size_t *n_infos);

/** Delete a remote object (or collection, if opts->recursive). */
pelican_error *pelican_delete(pelican_context *ctx,
                              const char *remote_url,
                              const pelican_transfer_opts *opts);

#ifdef __cplusplus
} /* extern "C" */
#endif

#endif /* PELICAN_CLIENT_H */
