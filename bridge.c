/*
 * bridge.c - internal cgo helpers plus the public API entry points.
 *
 * The Go side exports internal `pelicanc_*` symbols (cgo cannot express
 * `const char *` in export signatures); the public, const-correct
 * functions declared in include/pelican/client.h are defined here as
 * thin forwarders.  Memory released by the *_free functions was
 * allocated with malloc/calloc (C.CString and the pelicanc_*_alloc
 * helpers), so plain free() is correct.
 *
 * Copyright (C) 2026, Pelican Project, Morgridge Institute for Research
 * SPDX-License-Identifier: Apache-2.0
 */

#include <stdlib.h>

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
pelicanc_result_alloc(size_t n)
{
    return calloc(n, sizeof(pelican_result));
}

pelican_file_info *
pelicanc_file_info_alloc(size_t n)
{
    return calloc(n, sizeof(pelican_file_info));
}

struct pelican_context *
pelicanc_context_alloc(void)
{
    return calloc(1, sizeof(struct pelican_context));
}

/* ------------------------------------------------------------------ *
 * Public API: deallocation (pure C)                                  *
 * ------------------------------------------------------------------ */

void
pelican_error_free(pelican_error *err)
{
    if (!err)
        return;
    free(err->message);
    free(err);
}

void
pelican_results_free(pelican_result *results, size_t n)
{
    size_t i;
    if (!results)
        return;
    for (i = 0; i < n; i++) {
        free(results[i].source);
        free(results[i].endpoint);
        pelican_error_free(results[i].error);
    }
    free(results);
}

void
pelican_file_info_free(pelican_file_info *info)
{
    if (!info)
        return;
    free(info->name);
    free(info);
}

void
pelican_file_info_list_free(pelican_file_info *infos, size_t n)
{
    size_t i;
    if (!infos)
        return;
    for (i = 0; i < n; i++)
        free(infos[i].name);
    free(infos);
}

/* ------------------------------------------------------------------ *
 * Public API: forwarders into the Go implementation                  *
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
            pelican_result **results, size_t *n_results)
{
    return pelicanc_get(ctx, (char *)remote_url, (char *)local_path,
                        (pelican_transfer_opts *)opts, results, n_results);
}

pelican_error *
pelican_put(pelican_context *ctx, const char *local_path,
            const char *remote_url, const pelican_transfer_opts *opts,
            pelican_result **results, size_t *n_results)
{
    return pelicanc_put(ctx, (char *)local_path, (char *)remote_url,
                        (pelican_transfer_opts *)opts, results, n_results);
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
             const pelican_transfer_opts *opts, pelican_file_info **infos,
             size_t *n_infos)
{
    return pelicanc_list(ctx, (char *)remote_url,
                         (pelican_transfer_opts *)opts, infos, n_infos);
}

pelican_error *
pelican_delete(pelican_context *ctx, const char *remote_url,
               const pelican_transfer_opts *opts)
{
    return pelicanc_delete(ctx, (char *)remote_url,
                           (pelican_transfer_opts *)opts);
}
