/*
 * bridge.c - internal cgo helpers plus the public API entry points.
 *
 * The Go side exports internal `pelicanc_*` symbols (cgo cannot express
 * `const` qualifiers in export signatures); the public, const-correct
 * functions declared in include/pelican/client.h are defined here —
 * either as pure C (accessors, allocation, deallocation) or as thin
 * forwarders into the Go implementation.  Memory released here was
 * allocated with malloc/calloc/strdup (C.CString and the pelicanc_*
 * allocators), so plain free() is correct.
 *
 * Copyright (C) 2026, Pelican Project, Morgridge Institute for Research
 * SPDX-License-Identifier: Apache-2.0
 */

#include <stdlib.h>
#include <string.h>

#include "bridge.h"
#include "_cgo_export.h"

/* ------------------------------------------------------------------ *
 * Helpers called from Go                                             *
 * ------------------------------------------------------------------ */

void
pelicanc_invoke_progress(pelican_progress_fn fn, const char *object,
                         long long transferred, long long total,
                         int completed, void *user_data)
{
    fn(object, transferred, total, completed, user_data);
}

pelican_error *
pelicanc_error_alloc(void)
{
    return calloc(1, sizeof(pelican_error));
}

pelican_result *
pelicanc_result_alloc(void)
{
    return calloc(1, sizeof(pelican_result));
}

struct pelican_checksum *
pelicanc_checksum_alloc(size_t n)
{
    return calloc(n, sizeof(struct pelican_checksum));
}

pelican_result_list *
pelicanc_result_list_alloc(size_t n)
{
    pelican_result_list *list = calloc(1, sizeof(*list));
    if (list && n > 0)
        list->items = calloc(n, sizeof(pelican_result *));
    return list;
}

pelican_file_info *
pelicanc_file_info_alloc(void)
{
    return calloc(1, sizeof(pelican_file_info));
}

pelican_file_info_list *
pelicanc_file_info_list_alloc(size_t n)
{
    pelican_file_info_list *list = calloc(1, sizeof(*list));
    if (list && n > 0)
        list->items = calloc(n, sizeof(pelican_file_info *));
    return list;
}

struct pelican_context *
pelicanc_context_alloc(void)
{
    return calloc(1, sizeof(struct pelican_context));
}

struct pelican_transfer *
pelicanc_transfer_alloc(void)
{
    return calloc(1, sizeof(struct pelican_transfer));
}

struct pelican_file *
pelicanc_file_alloc(void)
{
    return calloc(1, sizeof(struct pelican_file));
}

struct pelican_op *
pelicanc_op_alloc(void)
{
    return calloc(1, sizeof(struct pelican_op));
}

/* ------------------------------------------------------------------ *
 * Errors                                                             *
 * ------------------------------------------------------------------ */

const char *
pelican_error_message(const pelican_error *err)
{
    return err->message;
}

int
pelican_error_is_retryable(const pelican_error *err)
{
    return err->retryable;
}

int
pelican_error_code(const pelican_error *err)
{
    return err->code;
}

void
pelican_error_free(pelican_error *err)
{
    if (!err)
        return;
    free(err->message);
    free(err);
}

/* ------------------------------------------------------------------ *
 * Transfer options                                                   *
 * ------------------------------------------------------------------ */

pelican_transfer_opts *
pelican_transfer_opts_new(void)
{
    return calloc(1, sizeof(pelican_transfer_opts));
}

static void
free_string_list(char **list, size_t n)
{
    size_t i;
    for (i = 0; i < n; i++)
        free(list[i]);
    free(list);
}

void
pelican_transfer_opts_free(pelican_transfer_opts *opts)
{
    if (!opts)
        return;
    free(opts->token);
    free(opts->token_location);
    free(opts->source_token);
    free(opts->source_token_location);
    free(opts->dest_token);
    free(opts->dest_token_location);
    free_string_list(opts->caches, opts->n_caches);
    free_string_list(opts->checksum_requests, opts->n_checksum_requests);
    free(opts);
}

static void
append_string(char ***list, size_t *n, const char *value)
{
    char **grown;
    if (!value)
        return;
    grown = realloc(*list, (*n + 1) * sizeof(char *));
    if (!grown)
        return;
    *list = grown;
    (*list)[*n] = strdup(value);
    if ((*list)[*n])
        (*n)++;
}

static void
set_string_field(char **field, const char *value)
{
    free(*field);
    *field = value ? strdup(value) : NULL;
}

void
pelican_transfer_opts_set_token(pelican_transfer_opts *opts,
                                const char *token)
{
    set_string_field(&opts->token, token);
}

void
pelican_transfer_opts_set_token_location(pelican_transfer_opts *opts,
                                         const char *path)
{
    set_string_field(&opts->token_location, path);
}

void
pelican_transfer_opts_add_cache(pelican_transfer_opts *opts,
                                const char *cache_url)
{
    append_string(&opts->caches, &opts->n_caches, cache_url);
}

void
pelican_transfer_opts_add_checksum_request(pelican_transfer_opts *opts,
                                           const char *digest_name)
{
    append_string(&opts->checksum_requests, &opts->n_checksum_requests,
                  digest_name);
}

void
pelican_transfer_opts_set_require_checksum(pelican_transfer_opts *opts,
                                           int require)
{
    opts->require_checksum = require;
}

void
pelican_transfer_opts_set_source_token(pelican_transfer_opts *opts,
                                       const char *token)
{
    set_string_field(&opts->source_token, token);
}

void
pelican_transfer_opts_set_source_token_location(pelican_transfer_opts *opts,
                                                const char *path)
{
    set_string_field(&opts->source_token_location, path);
}

void
pelican_transfer_opts_set_destination_token(pelican_transfer_opts *opts,
                                            const char *token)
{
    set_string_field(&opts->dest_token, token);
}

void
pelican_transfer_opts_set_destination_token_location(
    pelican_transfer_opts *opts, const char *path)
{
    set_string_field(&opts->dest_token_location, path);
}

void
pelican_transfer_opts_set_recursive(pelican_transfer_opts *opts,
                                    int recursive)
{
    opts->recursive = recursive;
}

void
pelican_transfer_opts_set_progress(pelican_transfer_opts *opts,
                                   pelican_progress_fn fn, void *user_data)
{
    opts->progress = fn;
    opts->progress_data = fn ? user_data : NULL;
}

/* ------------------------------------------------------------------ *
 * Results                                                            *
 * ------------------------------------------------------------------ */

const char *
pelican_result_source(const pelican_result *res)
{
    return res->source;
}

long long
pelican_result_transferred_bytes(const pelican_result *res)
{
    return res->transferred_bytes;
}

const char *
pelican_result_endpoint(const pelican_result *res)
{
    return res->endpoint;
}

double
pelican_result_transfer_time_s(const pelican_result *res)
{
    return res->transfer_time_s;
}

int
pelican_result_attempts(const pelican_result *res)
{
    return res->attempts;
}

const pelican_error *
pelican_result_error(const pelican_result *res)
{
    return res->error;
}

const char *
pelican_result_etag(const pelican_result *res)
{
    return res->etag;
}

size_t
pelican_result_checksum_count(const pelican_result *res)
{
    return res->n_checksums;
}

const char *
pelican_result_checksum_type(const pelican_result *res, size_t i)
{
    return i < res->n_checksums ? res->checksums[i].type : NULL;
}

const char *
pelican_result_checksum_value(const pelican_result *res, size_t i)
{
    return i < res->n_checksums ? res->checksums[i].value : NULL;
}

void
pelican_result_free(pelican_result *res)
{
    size_t i;
    if (!res)
        return;
    free(res->source);
    free(res->endpoint);
    free(res->etag);
    for (i = 0; i < res->n_checksums; i++) {
        free(res->checksums[i].type);
        free(res->checksums[i].value);
    }
    free(res->checksums);
    pelican_error_free(res->error);
    free(res);
}

size_t
pelican_result_list_count(const pelican_result_list *list)
{
    return list->count;
}

const pelican_result *
pelican_result_list_get(const pelican_result_list *list, size_t i)
{
    return i < list->count ? list->items[i] : NULL;
}

void
pelican_result_list_free(pelican_result_list *list)
{
    size_t i;
    if (!list)
        return;
    for (i = 0; i < list->count; i++)
        pelican_result_free(list->items[i]);
    free(list->items);
    free(list);
}

/* ------------------------------------------------------------------ *
 * File info                                                          *
 * ------------------------------------------------------------------ */

const char *
pelican_file_info_name(const pelican_file_info *info)
{
    return info->name;
}

long long
pelican_file_info_size(const pelican_file_info *info)
{
    return info->size;
}

long long
pelican_file_info_mtime(const pelican_file_info *info)
{
    return info->mtime;
}

int
pelican_file_info_is_collection(const pelican_file_info *info)
{
    return info->is_collection;
}

void
pelican_file_info_free(pelican_file_info *info)
{
    if (!info)
        return;
    free(info->name);
    free(info);
}

size_t
pelican_file_info_list_count(const pelican_file_info_list *list)
{
    return list->count;
}

const pelican_file_info *
pelican_file_info_list_get(const pelican_file_info_list *list, size_t i)
{
    return i < list->count ? list->items[i] : NULL;
}

void
pelican_file_info_list_free(pelican_file_info_list *list)
{
    size_t i;
    if (!list)
        return;
    for (i = 0; i < list->count; i++)
        pelican_file_info_free(list->items[i]);
    free(list->items);
    free(list);
}

/* ------------------------------------------------------------------ *
 * Asynchronous transfer accessors                                    *
 * ------------------------------------------------------------------ */

int
pelican_transfer_notify_fd(const pelican_transfer *xfer)
{
    return xfer->notify_fd;
}

int
pelican_transfer_next_result(pelican_transfer *xfer, pelican_result **res)
{
    return pelicanc_transfer_next_result(xfer, res);
}

int
pelican_transfer_is_done(const pelican_transfer *xfer)
{
    return pelicanc_transfer_is_done((pelican_transfer *)xfer);
}

const pelican_error *
pelican_transfer_error(const pelican_transfer *xfer)
{
    return pelicanc_transfer_error((pelican_transfer *)xfer);
}

void
pelican_transfer_cancel(pelican_transfer *xfer)
{
    pelicanc_transfer_cancel(xfer);
}

void
pelican_transfer_free(pelican_transfer *xfer)
{
    pelicanc_transfer_free(xfer);
}

/* ------------------------------------------------------------------ *
 * Asynchronous operation accessors                                   *
 * ------------------------------------------------------------------ */

int
pelican_op_notify_fd(const pelican_op *op)
{
    return op->notify_fd;
}

int
pelican_op_is_done(const pelican_op *op)
{
    return pelicanc_op_is_done((pelican_op *)op);
}

const pelican_error *
pelican_op_error(const pelican_op *op)
{
    return pelicanc_op_error((pelican_op *)op);
}

void
pelican_op_cancel(pelican_op *op)
{
    pelicanc_op_cancel(op);
}

void
pelican_op_free(pelican_op *op)
{
    pelicanc_op_free(op);
}

int
pelican_op_take_file_info(pelican_op *op, pelican_file_info **info)
{
    return pelicanc_op_take_file_info(op, info);
}

int
pelican_op_take_file_info_list(pelican_op *op, pelican_file_info_list **entries)
{
    return pelicanc_op_take_file_info_list(op, entries);
}

int
pelican_op_take_file(pelican_op *op, pelican_file **file)
{
    return pelicanc_op_take_file(op, file);
}

int
pelican_op_take_count(pelican_op *op, long long *count)
{
    return pelicanc_op_take_count(op, count);
}

int
pelican_op_take_data(pelican_op *op, void **buf, size_t *len)
{
    return pelicanc_op_take_data(op, buf, len);
}

int
pelican_op_take_cache_info(pelican_op *op, long long *age_s, long long *size)
{
    return pelicanc_op_take_cache_info(op, age_s, size);
}

int
pelican_op_take_message(pelican_op *op, char **message)
{
    return pelicanc_op_take_message(op, message);
}

void
pelican_buffer_free(void *buf)
{
    free(buf);
}

/* ------------------------------------------------------------------ *
 * Forwarders into the Go implementation                              *
 * ------------------------------------------------------------------ */

pelican_error *
pelican_config_set(const char *key, const char *value)
{
    return pelicanc_config_set((char *)key, (char *)value);
}

pelican_error *
pelican_client_init(const char *config_file)
{
    return pelicanc_client_init((char *)config_file);
}

const char *
pelican_version(void)
{
    return pelicanc_version();
}

pelican_context *
pelican_context_new(void)
{
    return pelicanc_context_new();
}

void
pelican_context_cancel(pelican_context *ctx)
{
    pelicanc_context_cancel(ctx);
}

void
pelican_context_free(pelican_context *ctx)
{
    pelicanc_context_free(ctx);
}

pelican_error *
pelican_get(pelican_context *ctx, const char *remote_url,
            const char *local_path, const pelican_transfer_opts *opts,
            pelican_result_list **results)
{
    return pelicanc_get(ctx, (char *)remote_url, (char *)local_path,
                        (pelican_transfer_opts *)opts, results);
}

pelican_error *
pelican_put(pelican_context *ctx, const char *local_path,
            const char *remote_url, const pelican_transfer_opts *opts,
            pelican_result_list **results)
{
    return pelicanc_put(ctx, (char *)local_path, (char *)remote_url,
                        (pelican_transfer_opts *)opts, results);
}

pelican_error *
pelican_copy(pelican_context *ctx, const char *source_url,
             const char *dest_url, const pelican_transfer_opts *opts,
             pelican_result_list **results)
{
    return pelicanc_copy(ctx, (char *)source_url, (char *)dest_url,
                         (pelican_transfer_opts *)opts, results);
}

pelican_error *
pelican_prestage(pelican_context *ctx, const char *remote_url,
                 const pelican_transfer_opts *opts,
                 pelican_result_list **results)
{
    return pelicanc_prestage(ctx, (char *)remote_url,
                             (pelican_transfer_opts *)opts, results);
}

pelican_error *
pelican_copy_start(const char *source_url, const char *dest_url,
                   const pelican_transfer_opts *opts, pelican_transfer **xfer)
{
    return pelicanc_copy_start((char *)source_url, (char *)dest_url,
                               (pelican_transfer_opts *)opts, xfer);
}

pelican_error *
pelican_prestage_start(const char *remote_url,
                       const pelican_transfer_opts *opts,
                       pelican_transfer **xfer)
{
    return pelicanc_prestage_start((char *)remote_url,
                                   (pelican_transfer_opts *)opts, xfer);
}

pelican_error *
pelican_cache_info(pelican_context *ctx, const char *remote_url,
                   const pelican_transfer_opts *opts, long long *age_s,
                   long long *size)
{
    return pelicanc_cache_info(ctx, (char *)remote_url,
                               (pelican_transfer_opts *)opts, age_s, size);
}

pelican_error *
pelican_evict(pelican_context *ctx, const char *remote_url, int immediate,
              const pelican_transfer_opts *opts, char **message)
{
    return pelicanc_evict(ctx, (char *)remote_url, immediate,
                          (pelican_transfer_opts *)opts, message);
}

void
pelican_string_free(char *s)
{
    free(s);
}

pelican_error *
pelican_get_start(const char *remote_url, const char *local_path,
                  const pelican_transfer_opts *opts, pelican_transfer **xfer)
{
    return pelicanc_get_start((char *)remote_url, (char *)local_path,
                              (pelican_transfer_opts *)opts, xfer);
}

pelican_error *
pelican_put_start(const char *local_path, const char *remote_url,
                  const pelican_transfer_opts *opts, pelican_transfer **xfer)
{
    return pelicanc_put_start((char *)local_path, (char *)remote_url,
                              (pelican_transfer_opts *)opts, xfer);
}

pelican_error *
pelican_stat(pelican_context *ctx, const char *remote_url,
             const pelican_transfer_opts *opts, pelican_file_info **info)
{
    return pelicanc_stat(ctx, (char *)remote_url,
                         (pelican_transfer_opts *)opts, info);
}

pelican_error *
pelican_list(pelican_context *ctx, const char *remote_url,
             const pelican_transfer_opts *opts,
             pelican_file_info_list **entries)
{
    return pelicanc_list(ctx, (char *)remote_url,
                         (pelican_transfer_opts *)opts, entries);
}

pelican_error *
pelican_delete(pelican_context *ctx, const char *remote_url,
               const pelican_transfer_opts *opts)
{
    return pelicanc_delete(ctx, (char *)remote_url,
                           (pelican_transfer_opts *)opts);
}

/* ------------------------------------------------------------------ *
 * File I/O forwarders                                                *
 * ------------------------------------------------------------------ */

pelican_error *
pelican_fs_open(const char *remote_url, int flags,
                const pelican_transfer_opts *opts, pelican_file **file)
{
    return pelicanc_fs_open((char *)remote_url, flags,
                            (pelican_transfer_opts *)opts, file);
}

long long
pelican_file_read(pelican_file *file, void *buf, size_t len,
                  pelican_error **err)
{
    return pelicanc_file_read(file, buf, len, err);
}

long long
pelican_file_pread(pelican_file *file, void *buf, size_t len,
                   long long offset, pelican_error **err)
{
    return pelicanc_file_pread(file, buf, len, offset, err);
}

long long
pelican_file_write(pelican_file *file, const void *buf, size_t len,
                   pelican_error **err)
{
    return pelicanc_file_write(file, (void *)buf, len, err);
}

long long
pelican_file_seek(pelican_file *file, long long offset, int whence,
                  pelican_error **err)
{
    return pelicanc_file_seek(file, offset, whence, err);
}

pelican_error *
pelican_file_stat(pelican_file *file, pelican_file_info **info)
{
    return pelicanc_file_stat(file, info);
}

pelican_error *
pelican_file_close(pelican_file *file)
{
    return pelicanc_file_close(file);
}

/* ------------------------------------------------------------------ *
 * Asynchronous operation forwarders                                  *
 * ------------------------------------------------------------------ */

pelican_error *
pelican_stat_start(const char *remote_url, const pelican_transfer_opts *opts,
                   pelican_op **op)
{
    return pelicanc_stat_start((char *)remote_url,
                               (pelican_transfer_opts *)opts, op);
}

pelican_error *
pelican_list_start(const char *remote_url, const pelican_transfer_opts *opts,
                   pelican_op **op)
{
    return pelicanc_list_start((char *)remote_url,
                               (pelican_transfer_opts *)opts, op);
}

pelican_error *
pelican_delete_start(const char *remote_url,
                     const pelican_transfer_opts *opts, pelican_op **op)
{
    return pelicanc_delete_start((char *)remote_url,
                                 (pelican_transfer_opts *)opts, op);
}

pelican_error *
pelican_cache_info_start(const char *remote_url,
                         const pelican_transfer_opts *opts, pelican_op **op)
{
    return pelicanc_cache_info_start((char *)remote_url,
                                     (pelican_transfer_opts *)opts, op);
}

pelican_error *
pelican_evict_start(const char *remote_url, int immediate,
                    const pelican_transfer_opts *opts, pelican_op **op)
{
    return pelicanc_evict_start((char *)remote_url, immediate,
                                (pelican_transfer_opts *)opts, op);
}

pelican_error *
pelican_fs_open_start(const char *remote_url, int flags,
                      const pelican_transfer_opts *opts, pelican_op **op)
{
    return pelicanc_fs_open_start((char *)remote_url, flags,
                                  (pelican_transfer_opts *)opts, op);
}

pelican_error *
pelican_file_read_start(pelican_file *file, size_t len, pelican_op **op)
{
    return pelicanc_file_read_start(file, len, op);
}

pelican_error *
pelican_file_pread_start(pelican_file *file, size_t len, long long offset,
                         pelican_op **op)
{
    return pelicanc_file_pread_start(file, len, offset, op);
}

pelican_error *
pelican_file_write_start(pelican_file *file, const void *buf, size_t len,
                         pelican_op **op)
{
    return pelicanc_file_write_start(file, (void *)buf, len, op);
}

pelican_error *
pelican_file_close_start(pelican_file *file, pelican_op **op)
{
    return pelicanc_file_close_start(file, op);
}
