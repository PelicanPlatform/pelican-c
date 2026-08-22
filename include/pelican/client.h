/**
 * pelican/client.h - C interface to the Pelican Platform client
 *
 * A thin, ABI-stable C wrapper around the Go client library from
 * github.com/pelicanplatform/pelican, intended for embedding Pelican
 * transfers into C/C++ applications such as HTCondor.
 *
 * Conventions:
 *  - Every type is opaque; all access goes through getter/setter
 *    functions so struct layout is never part of the ABI.
 *  - Every function that can fail returns a `pelican_error *`; NULL means
 *    success.  Errors must be released with pelican_error_free() unless
 *    documented as borrowed.
 *  - Strings returned by getters are borrowed: valid until the owning
 *    object is freed, and must not be freed by the caller.
 *  - All functions are thread-safe once pelican_client_init() has
 *    returned, but see the per-section notes on which calls block.
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
 * Opaque types                                                       *
 *                                                                    *
 * Declared together so the asynchronous operation API can reference   *
 * results defined in later sections.  No layout is ever exposed:      *
 * every field is reached through the accessors below.                 *
 * ------------------------------------------------------------------ */

typedef struct pelican_error pelican_error;
typedef struct pelican_context pelican_context;
typedef struct pelican_transfer_opts pelican_transfer_opts;
typedef struct pelican_result pelican_result;
typedef struct pelican_result_list pelican_result_list;
typedef struct pelican_transfer pelican_transfer;
typedef struct pelican_op pelican_op;
typedef struct pelican_file_info pelican_file_info;
typedef struct pelican_file_info_list pelican_file_info_list;
typedef struct pelican_file pelican_file;

/* ------------------------------------------------------------------ *
 * Errors                                                             *
 * ------------------------------------------------------------------ */

/** Human-readable description; borrowed, never NULL. */
const char *pelican_error_message(const pelican_error *err);

/** Nonzero if retrying the operation may succeed (the same
 *  classification the pelican CLI uses for its retry decisions). */
int pelican_error_is_retryable(const pelican_error *err);

/** Numeric code from the Pelican error taxonomy, or 0 if unclassified. */
int pelican_error_code(const pelican_error *err);

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
 * Logging                                                            *
 * ------------------------------------------------------------------ */

/**
 * Severity of a log record, mapped from the Go client's levels: its
 * panic, fatal, and error levels all arrive as PELICAN_LOG_ERROR.
 */
typedef enum {
    PELICAN_LOG_ERROR = 0,
    PELICAN_LOG_WARNING,
    PELICAN_LOG_INFO,
    PELICAN_LOG_DEBUG,
    PELICAN_LOG_TRACE
} pelican_log_level;

/**
 * Receives one log record.  `message` is borrowed for the duration of
 * the call: copy it if you need to keep it.  Any structured fields the
 * library attached are appended as ` key=value` pairs.
 */
typedef void (*pelican_log_fn)(pelican_log_level level, const char *message,
                               void *user_data);

/** How log records reach the callback. */
typedef enum {
    /**
     * Records queue internally and are delivered only from
     * pelican_log_pump(), on the thread that calls it.  Nothing is
     * invoked from a library thread — the right choice for
     * aggressively thread-unsafe hosts such as HTCondor DaemonCore.
     */
    PELICAN_LOG_QUEUED = 0,
    /**
     * Records are delivered immediately, from whichever library thread
     * produced them.  The callback must be thread-safe and must not
     * call back into the library.
     */
    PELICAN_LOG_DIRECT = 1
} pelican_log_delivery;

/**
 * Route the client's log output to `fn`, replacing any previous
 * callback, and stop the library writing to stderr or a configured log
 * file.  Passing NULL uninstalls the callback and restores the
 * library's own stderr output.
 *
 * Call this AFTER pelican_client_init(): initialization applies the
 * logging configuration and would otherwise reinstate the library's own
 * output.
 *
 * With PELICAN_LOG_QUEUED, records accumulate in a bounded queue; if the
 * host stops pumping, the oldest records are dropped and the next
 * delivery reports how many were lost, so a log storm cannot grow
 * memory without bound.
 */
pelican_error *pelican_log_set_callback(pelican_log_fn fn, void *user_data,
                                        pelican_log_delivery delivery);

/**
 * Readable while queued records await delivery, for hosts that wait on
 * descriptors.  Returns -1 if no queued callback is installed.  Owned
 * by the library: do not read from or close it.  It quiesces once
 * pelican_log_pump() has drained the queue.
 */
int pelican_log_notify_fd(void);

/**
 * Deliver all pending log records on the calling thread, in the order
 * produced, and return how many callbacks were invoked.  Safe to call
 * at any time; a no-op when the queue is empty or delivery is direct.
 */
size_t pelican_log_pump(void);

/**
 * Set the minimum severity the library emits.  Records below this level
 * are never generated, so raising the threshold costs nothing at the
 * call site.
 */
pelican_error *pelican_log_set_level(pelican_log_level level);

/* ------------------------------------------------------------------ *
 * Cancellation contexts (for the synchronous calls)                  *
 * ------------------------------------------------------------------ */

/**
 * A cancellation handle for the *synchronous* operations.  Optional:
 * pass NULL wherever a pelican_context* is accepted to run without
 * external cancellation.  pelican_context_cancel() may be called from
 * any thread; in-flight operations using the context return promptly
 * with a non-retryable "context canceled" error.  (Asynchronous
 * transfers and open files carry their own cancellation — see
 * pelican_transfer_cancel and pelican_file_close.)
 */

pelican_context *pelican_context_new(void);
void pelican_context_cancel(pelican_context *ctx);
/** Free the context.  Do not free while operations using it are in flight. */
void pelican_context_free(pelican_context *ctx);

/* ------------------------------------------------------------------ *
 * Transfer options                                                   *
 * ------------------------------------------------------------------ */

/**
 * Progress callback.  The invoking thread depends on the API used:
 *
 *  - Asynchronous transfers (pelican_get_start/pelican_put_start):
 *    NEVER invoked from a library thread.  Progress reports queue
 *    internally (waking the notification fd) and are delivered on the
 *    host's own thread from inside pelican_transfer_next_result() —
 *    safe for aggressively thread-unsafe hosts such as HTCondor
 *    DaemonCore.  Reports are coalesced per object: the callback sees
 *    each object's latest state, not every intermediate update.
 *
 *  - Synchronous calls (pelican_get/pelican_put): invoked periodically
 *    from a library-owned thread while the caller blocks; the callback
 *    must be thread-safe and must not call back into the library.
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
 * Options accepted by transfer, namespace, and file operations.
 * Caller-owned: create with pelican_transfer_opts_new(), release with
 * pelican_transfer_opts_free() (safe once the call it was passed to has
 * returned; string values are copied by the setters).
 */

pelican_transfer_opts *pelican_transfer_opts_new(void);
void pelican_transfer_opts_free(pelican_transfer_opts *opts);

/** Bearer token contents to authenticate with (copied; NULL clears). */
void pelican_transfer_opts_set_token(pelican_transfer_opts *opts,
                                     const char *token);
/** Path to a file holding the bearer token (copied; NULL clears). */
void pelican_transfer_opts_set_token_location(pelican_transfer_opts *opts,
                                              const char *path);
/** Nonzero to transfer/delete collections recursively. */
void pelican_transfer_opts_set_recursive(pelican_transfer_opts *opts,
                                         int recursive);
/** Progress callback and its opaque user pointer (NULL fn clears). */
void pelican_transfer_opts_set_progress(pelican_transfer_opts *opts,
                                        pelican_progress_fn fn,
                                        void *user_data);

/**
 * Add a preferred cache to try before the director's ordering (e.g.
 * "https://cache.example.com:8443").  May be called repeatedly; order is
 * preserved.  By default ONLY the listed caches are tried; add the
 * special value "+" as the final entry to fall back to the director's
 * list after the explicit preferences.  Invalid URLs are reported by
 * the operation the options are passed to.
 */
void pelican_transfer_opts_add_cache(pelican_transfer_opts *opts,
                                     const char *cache_url);

/**
 * Ask the server to provide a checksum of the given type (HTTP digest
 * name: "md5", "crc32c", "crc32", or "sha").  May be called repeatedly.
 * Unknown names are reported by the operation the options are passed to.
 */
void pelican_transfer_opts_add_checksum_request(pelican_transfer_opts *opts,
                                                const char *digest_name);

/** Nonzero to fail the transfer if checksum verification cannot be
 *  performed (rather than transferring unverified). */
void pelican_transfer_opts_set_require_checksum(pelican_transfer_opts *opts,
                                                int require);

/* For third-party copies, the source and destination may need different
 * credentials; these override the plain token/token_location for the
 * respective side.  (All values copied; NULL clears.) */
void pelican_transfer_opts_set_source_token(pelican_transfer_opts *opts,
                                            const char *token);
void pelican_transfer_opts_set_source_token_location(
    pelican_transfer_opts *opts, const char *path);
void pelican_transfer_opts_set_destination_token(pelican_transfer_opts *opts,
                                                 const char *token);
void pelican_transfer_opts_set_destination_token_location(
    pelican_transfer_opts *opts, const char *path);

/* ------------------------------------------------------------------ *
 * Per-object transfer results                                        *
 * ------------------------------------------------------------------ */


/** Remote object path this result describes; borrowed. */
const char *pelican_result_source(const pelican_result *res);
/** Bytes moved for this object. */
long long pelican_result_transferred_bytes(const pelican_result *res);
/** host:port of the cache/origin used, or NULL if none was reached. */
const char *pelican_result_endpoint(const pelican_result *res);
/** Wall time of the final attempt, in seconds. */
double pelican_result_transfer_time_s(const pelican_result *res);
/** Number of attempts made. */
int pelican_result_attempts(const pelican_result *res);
/** Per-object failure, or NULL on success.  Borrowed: owned by the
 *  result; do NOT pass to pelican_error_free(). */
const pelican_error *pelican_result_error(const pelican_result *res);
/** ETag reported by the server, or NULL. */
const char *pelican_result_etag(const pelican_result *res);
/** Checksums for the object: server-reported when available, otherwise
 *  client-computed.  Types are HTTP digest names (e.g. "md5",
 *  "adler32"); values are hex-encoded.  Getters return NULL if i is out
 *  of range. */
size_t pelican_result_checksum_count(const pelican_result *res);
const char *pelican_result_checksum_type(const pelican_result *res, size_t i);
const char *pelican_result_checksum_value(const pelican_result *res, size_t i);
/** Release a result popped from pelican_transfer_next_result(). */
void pelican_result_free(pelican_result *res);

/** An immutable list of results, as returned by the synchronous calls. */

size_t pelican_result_list_count(const pelican_result_list *list);
/** Borrowed element; valid until the list is freed.  NULL if out of range. */
const pelican_result *pelican_result_list_get(const pelican_result_list *list,
                                              size_t i);
void pelican_result_list_free(pelican_result_list *list);

/* ------------------------------------------------------------------ *
 * Synchronous transfers                                              *
 * ------------------------------------------------------------------ */

/**
 * Download `remote_url` (e.g. "pelican://osg-htc.org/ospool/.../file")
 * to `local_path`, blocking until the transfer completes.  On return,
 * `*results` (if non-NULL) holds one entry per object transferred; free
 * with pelican_result_list_free().
 */
pelican_error *pelican_get(pelican_context *ctx,
                           const char *remote_url,
                           const char *local_path,
                           const pelican_transfer_opts *opts,
                           pelican_result_list **results);

/** Upload `local_path` to `remote_url`.  Same semantics as pelican_get. */
pelican_error *pelican_put(pelican_context *ctx,
                           const char *local_path,
                           const char *remote_url,
                           const pelican_transfer_opts *opts,
                           pelican_result_list **results);

/**
 * Third-party copy: instruct the destination to copy the object directly
 * from the source (WebDAV COPY), without the data flowing through this
 * client.  Both URLs are remote.  Use the source/destination token
 * setters when the two sides need different credentials.
 */
pelican_error *pelican_copy(pelican_context *ctx,
                            const char *source_url,
                            const char *dest_url,
                            const pelican_transfer_opts *opts,
                            pelican_result_list **results);

/**
 * Ask the federation caches to stage `remote_url` (recursively, if opts
 * says so) without downloading it locally.  Results report per-object
 * staging outcomes.
 */
pelican_error *pelican_prestage(pelican_context *ctx,
                                const char *remote_url,
                                const pelican_transfer_opts *opts,
                                pelican_result_list **results);

/* ------------------------------------------------------------------ *
 * Asynchronous transfers                                             *
 * ------------------------------------------------------------------ */

/**
 * An in-flight transfer.  Designed for single-threaded event loops
 * (e.g. HTCondor DaemonCore): no call in this section blocks, and
 * completed per-object results are streamed as they finish rather than
 * buffered into one final list.
 *
 * Usage:
 *   1. pelican_get_start() / pelican_put_start() submits the transfer
 *      and returns immediately.
 *   2. Register pelican_transfer_notify_fd() for read events in your
 *      event loop.  The fd becomes readable whenever results or
 *      progress reports are queued or the transfer finishes.  Do not
 *      read or close it.
 *   3. On wakeup, call pelican_transfer_next_result() until it returns
 *      0, freeing each popped result.  Any pending progress callbacks
 *      fire on your thread from inside next_result, before results are
 *      reported.
 *   4. When pelican_transfer_is_done() reports completion, check
 *      pelican_transfer_error() for a transfer-level failure, then
 *      release everything with pelican_transfer_free().
 *
 * All calls on a given pelican_transfer must come from one thread (or
 * be externally serialized); distinct transfers are independent.
 */

/** Begin an asynchronous download.  Fails only on malformed arguments
 *  or an uninitialized library; transfer-time errors are reported
 *  through the handle. */
pelican_error *pelican_get_start(const char *remote_url,
                                 const char *local_path,
                                 const pelican_transfer_opts *opts,
                                 pelican_transfer **xfer);

/** Begin an asynchronous upload. */
pelican_error *pelican_put_start(const char *local_path,
                                 const char *remote_url,
                                 const pelican_transfer_opts *opts,
                                 pelican_transfer **xfer);

/** Begin an asynchronous third-party copy (see pelican_copy). */
pelican_error *pelican_copy_start(const char *source_url,
                                  const char *dest_url,
                                  const pelican_transfer_opts *opts,
                                  pelican_transfer **xfer);

/** Begin an asynchronous prestage (see pelican_prestage). */
pelican_error *pelican_prestage_start(const char *remote_url,
                                      const pelican_transfer_opts *opts,
                                      pelican_transfer **xfer);

/**
 * Notification fd: becomes readable when results are pending or the
 * transfer completes.  Owned by the library — register it with your
 * event loop (level-triggered readiness), but never read from or close
 * it.  Valid until pelican_transfer_free().
 */
int pelican_transfer_notify_fd(const pelican_transfer *xfer);

/**
 * Pop the next completed per-object result without blocking.
 * Returns 1 and stores an owned result in *res (free with
 * pelican_result_free), or 0 if none are pending — in which case the
 * notification fd is quiesced until new events arrive.
 *
 * If a progress callback was set, all pending progress reports are
 * delivered on the calling thread before the result queue is examined.
 */
int pelican_transfer_next_result(pelican_transfer *xfer,
                                 pelican_result **res);

/** Nonzero once the transfer has finished producing results.  Queued
 *  results may still be pending; drain with next_result. */
int pelican_transfer_is_done(const pelican_transfer *xfer);

/**
 * Transfer-level failure (e.g. lookup or submission error), or NULL.
 * Meaningful once is_done; borrowed — owned by the transfer, do NOT
 * pass to pelican_error_free().  Per-object failures are reported on
 * the individual results instead.
 */
const pelican_error *pelican_transfer_error(const pelican_transfer *xfer);

/** Cancel an in-flight transfer.  Results already queued remain
 *  poppable; the transfer finishes with a cancellation error. */
void pelican_transfer_cancel(pelican_transfer *xfer);

/** Cancel if needed and release the transfer, its queued results, and
 *  the notification fd. */
void pelican_transfer_free(pelican_transfer *xfer);

/* ------------------------------------------------------------------ *
 * Asynchronous single-shot operations                                *
 * ------------------------------------------------------------------ */

/**
 * A single-shot asynchronous operation: the namespace calls, cache
 * management, and file I/O below all have a `*_start` form that returns
 * one of these instead of blocking.  Unlike pelican_transfer (which
 * streams many results), an operation produces exactly one outcome.
 *
 * Usage:
 *   1. Call a *_start function; it returns immediately.
 *   2. Register pelican_op_notify_fd() for read events.  It becomes
 *      readable when the operation completes and STAYS readable
 *      thereafter.  Do not read or close it.
 *   3. On wakeup (or any time), pelican_op_is_done() reports completion.
 *   4. Check pelican_op_error(), collect the result with the matching
 *      pelican_op_take_* function, then pelican_op_free().
 *
 * The take_* functions transfer ownership and may be called only once;
 * a result that is never taken is released by pelican_op_free().  All
 * calls on one operation must come from a single thread (or be
 * externally serialized); distinct operations are independent.  No call
 * in this section blocks.
 */

/** Readable once the operation has completed; library-owned. */
int pelican_op_notify_fd(const pelican_op *op);

/** Nonzero once the operation has completed. */
int pelican_op_is_done(const pelican_op *op);

/**
 * Failure of a completed operation, or NULL on success (or if still
 * running).  Borrowed — owned by the operation; do NOT pass to
 * pelican_error_free().
 */
const pelican_error *pelican_op_error(const pelican_op *op);

/** Request cancellation; the operation completes with an error. */
void pelican_op_cancel(pelican_op *op);

/**
 * Release the operation and any result not taken.  Safe while the
 * operation is still in flight: it is cancelled and detached, and its
 * result discarded when it finishes.
 */
void pelican_op_free(pelican_op *op);

/* Result collection.  Each returns 1 on success (ownership transferred
 * to the caller) or 0 if the operation is unfinished, failed, produced a
 * different result type, or was already taken. */

int pelican_op_take_file_info(pelican_op *op, pelican_file_info **info);
int pelican_op_take_file_info_list(pelican_op *op,
                                   pelican_file_info_list **entries);
int pelican_op_take_file(pelican_op *op, pelican_file **file);
/** Byte count produced by an I/O operation (bytes read or written). */
int pelican_op_take_count(pelican_op *op, long long *count);
/** Data read by pelican_file_read_start/pelican_file_pread_start.  The
 *  buffer is caller-owned; release it with pelican_buffer_free(). */
int pelican_op_take_data(pelican_op *op, void **buf, size_t *len);
/** Cache age (seconds; -1 if unknown) and size, from
 *  pelican_cache_info_start.  Either out-pointer may be NULL. */
int pelican_op_take_cache_info(pelican_op *op, long long *age_s,
                               long long *size);
/** Status message from pelican_evict_start; release with
 *  pelican_string_free(). */
int pelican_op_take_message(pelican_op *op, char **message);

/** Free a buffer obtained from pelican_op_take_data(). */
void pelican_buffer_free(void *buf);

/* ------------------------------------------------------------------ *
 * Namespace operations (synchronous)                                 *
 * ------------------------------------------------------------------ */


/** Object name; borrowed. */
const char *pelican_file_info_name(const pelican_file_info *info);
/** Size in bytes. */
long long pelican_file_info_size(const pelican_file_info *info);
/** Modification time, Unix seconds. */
long long pelican_file_info_mtime(const pelican_file_info *info);
/** Nonzero for collections (directories). */
int pelican_file_info_is_collection(const pelican_file_info *info);
void pelican_file_info_free(pelican_file_info *info);


size_t pelican_file_info_list_count(const pelican_file_info_list *list);
/** Borrowed element; valid until the list is freed.  NULL if out of range. */
const pelican_file_info *
pelican_file_info_list_get(const pelican_file_info_list *list, size_t i);
void pelican_file_info_list_free(pelican_file_info_list *list);

/** Stat a remote object.  On success *info must be freed with
 *  pelican_file_info_free(). */
pelican_error *pelican_stat(pelican_context *ctx,
                            const char *remote_url,
                            const pelican_transfer_opts *opts,
                            pelican_file_info **info);

/** List a remote collection.  On success *entries must be freed with
 *  pelican_file_info_list_free(). */
pelican_error *pelican_list(pelican_context *ctx,
                            const char *remote_url,
                            const pelican_transfer_opts *opts,
                            pelican_file_info_list **entries);

/** Delete a remote object (or collection, if opts is recursive). */
pelican_error *pelican_delete(pelican_context *ctx,
                              const char *remote_url,
                              const pelican_transfer_opts *opts);

/** Query whether (and how) a cache holds the object: *age_s is the
 *  cached copy's age in seconds (-1 if unknown/not cached) and *size
 *  its size in bytes.  Either out-pointer may be NULL. */
pelican_error *pelican_cache_info(pelican_context *ctx,
                                  const char *remote_url,
                                  const pelican_transfer_opts *opts,
                                  long long *age_s,
                                  long long *size);

/**
 * Evict the object from federation caches.  If immediate is nonzero,
 * request synchronous eviction.  On success *message (if non-NULL)
 * receives a human-readable status; free it with pelican_string_free().
 */
pelican_error *pelican_evict(pelican_context *ctx,
                             const char *remote_url,
                             int immediate,
                             const pelican_transfer_opts *opts,
                             char **message);

/** Free a string returned via an out-parameter (e.g. pelican_evict). */
void pelican_string_free(char *s);

/* Asynchronous forms of the above; collect results with the noted
 * pelican_op_take_* function.  See "Asynchronous single-shot
 * operations". */

/** Completes with pelican_op_take_file_info(). */
pelican_error *pelican_stat_start(const char *remote_url,
                                  const pelican_transfer_opts *opts,
                                  pelican_op **op);
/** Completes with pelican_op_take_file_info_list(). */
pelican_error *pelican_list_start(const char *remote_url,
                                  const pelican_transfer_opts *opts,
                                  pelican_op **op);
/** Completes with no result; check pelican_op_error(). */
pelican_error *pelican_delete_start(const char *remote_url,
                                    const pelican_transfer_opts *opts,
                                    pelican_op **op);
/** Completes with pelican_op_take_cache_info(). */
pelican_error *pelican_cache_info_start(const char *remote_url,
                                        const pelican_transfer_opts *opts,
                                        pelican_op **op);
/** Completes with pelican_op_take_message(). */
pelican_error *pelican_evict_start(const char *remote_url, int immediate,
                                   const pelican_transfer_opts *opts,
                                   pelican_op **op);

/* ------------------------------------------------------------------ *
 * File I/O (PelicanFS)                                               *
 * ------------------------------------------------------------------ */

/**
 * POSIX-like access to remote objects without staging them to local
 * disk, backed by the Go client's PelicanFS.  These calls BLOCK on
 * network I/O — from a single-threaded event loop, use them on worker
 * threads/processes or where blocking is acceptable.
 */

/* Open flags (library-defined values; do not pass POSIX O_* here). */
#define PELICAN_O_RDONLY 0
#define PELICAN_O_WRONLY 1
#define PELICAN_O_RDWR   2
#define PELICAN_O_CREATE 4

/** Open a remote object by URL.  On success *file must eventually be
 *  released with pelican_file_close(). */
pelican_error *pelican_fs_open(const char *remote_url,
                               int flags,
                               const pelican_transfer_opts *opts,
                               pelican_file **file);

/**
 * Read up to `len` bytes at the current position.  Returns the number
 * of bytes read, 0 at end-of-file, or -1 on error (with *err set if
 * err is non-NULL).
 */
long long pelican_file_read(pelican_file *file, void *buf, size_t len,
                            pelican_error **err);

/** Positional read (does not move the file position).  Same return
 *  conventions as pelican_file_read. */
long long pelican_file_pread(pelican_file *file, void *buf, size_t len,
                             long long offset, pelican_error **err);

/** Append `len` bytes to an object opened for writing.  Returns bytes
 *  written or -1 on error.  Uploads are streamed; the object is
 *  finalized by pelican_file_close(). */
long long pelican_file_write(pelican_file *file, const void *buf,
                             size_t len, pelican_error **err);

/** Reposition the read offset.  whence is SEEK_SET(0)/SEEK_CUR(1)/
 *  SEEK_END(2).  Returns the new offset or -1 on error. */
long long pelican_file_seek(pelican_file *file, long long offset,
                            int whence, pelican_error **err);

/** Stat the open file.  On success *info must be freed with
 *  pelican_file_info_free(). */
pelican_error *pelican_file_stat(pelican_file *file,
                                 pelican_file_info **info);

/**
 * Close the file and release the handle (the handle is freed even if
 * an error is returned).  For writes, this finalizes the upload and
 * reports its outcome — always check the result.
 */
pelican_error *pelican_file_close(pelican_file *file);

/* ------------------------------------------------------------------ *
 * Asynchronous file I/O                                              *
 * ------------------------------------------------------------------ */

/**
 * Non-blocking counterparts to the calls above, for event-loop hosts.
 * Each returns a pelican_op (see "Asynchronous single-shot
 * operations") that completes when the I/O finishes.
 *
 * A pelican_file permits only ONE operation at a time: while an
 * asynchronous operation is in flight, other calls on that handle —
 * synchronous or asynchronous — fail with a "busy" error.  Reads and
 * writes buffer internally, so no caller-supplied buffer has to stay
 * alive; nothing the caller owns is touched after the call returns.
 */

/** Completes with pelican_op_take_file(). */
pelican_error *pelican_fs_open_start(const char *remote_url, int flags,
                                     const pelican_transfer_opts *opts,
                                     pelican_op **op);

/** Read up to `len` bytes from the current position.  Completes with
 *  pelican_op_take_data(); zero length signals end-of-file. */
pelican_error *pelican_file_read_start(pelican_file *file, size_t len,
                                       pelican_op **op);

/** Positional read; does not move the file position.  Completes with
 *  pelican_op_take_data(). */
pelican_error *pelican_file_pread_start(pelican_file *file, size_t len,
                                        long long offset, pelican_op **op);

/** Append `len` bytes (copied before returning).  Completes with
 *  pelican_op_take_count(). */
pelican_error *pelican_file_write_start(pelican_file *file, const void *buf,
                                        size_t len, pelican_op **op);

/**
 * Close the file asynchronously, finalizing an upload.  Takes ownership
 * of `file`: the handle must not be used again once this returns, even
 * before the operation completes.  The operation produces no result;
 * check pelican_op_error() for the close outcome.
 */
pelican_error *pelican_file_close_start(pelican_file *file, pelican_op **op);

#ifdef __cplusplus
} /* extern "C" */
#endif

#endif /* PELICAN_CLIENT_H */
