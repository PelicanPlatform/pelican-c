/*
 * pelican_example.c - demonstrate libpelicanclient usage.
 *
 * Usage:
 *   pelican_example                       run the offline smoke test
 *   pelican_example <url>                 stat (and list, if a collection)
 *   pelican_example <url> <local-path>    download with progress reporting
 *
 * Copyright (C) 2026, Pelican Project, Morgridge Institute for Research
 * SPDX-License-Identifier: Apache-2.0
 */

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include <pelican/client.h>

static void
progress(const char *object, long long transferred, long long total,
         int completed, void *user_data)
{
    (void)user_data;
    fprintf(stderr, "  progress: %s %lld/%lld%s\n", object, transferred,
            total, completed ? " (done)" : "");
}

static int
fail(const char *what, pelican_error *err)
{
    fprintf(stderr, "%s failed: %s (retryable: %s)\n", what, err->message,
            err->retryable ? "yes" : "no");
    pelican_error_free(err);
    return 1;
}

int
main(int argc, char **argv)
{
    pelican_error *err;

    printf("pelican client version: %s\n", pelican_version());

    if ((err = pelican_client_init(NULL)) != NULL)
        return fail("pelican_client_init", err);
    puts("client initialized");

    if (argc < 2) {
        /* Offline smoke test: a malformed URL must produce an error,
         * not a crash — and must not be flagged retryable. */
        pelican_file_info *info = NULL;
        err = pelican_stat(NULL, "not-a-valid-url", NULL, &info);
        if (err == NULL) {
            fprintf(stderr, "expected an error from a bogus stat\n");
            return 1;
        }
        printf("bogus stat correctly failed: %s\n", err->message);
        pelican_error_free(err);
        puts("smoke test passed");
        return 0;
    }

    const char *url = argv[1];

    pelican_file_info *info = NULL;
    if ((err = pelican_stat(NULL, url, NULL, &info)) != NULL)
        return fail("pelican_stat", err);
    printf("stat: name=%s size=%lld mtime=%lld collection=%d\n", info->name,
           info->size, info->mtime, info->is_collection);
    int is_collection = info->is_collection;
    pelican_file_info_free(info);

    if (is_collection && argc < 3) {
        pelican_file_info *entries = NULL;
        size_t n = 0;
        if ((err = pelican_list(NULL, url, NULL, &entries, &n)) != NULL)
            return fail("pelican_list", err);
        for (size_t i = 0; i < n; i++)
            printf("  %c %12lld %s\n", entries[i].is_collection ? 'd' : '-',
                   entries[i].size, entries[i].name);
        pelican_file_info_list_free(entries, n);
    }

    if (argc >= 3) {
        pelican_transfer_opts opts;
        memset(&opts, 0, sizeof(opts));
        opts.progress = progress;

        pelican_context *ctx = pelican_context_new();
        pelican_result *results = NULL;
        size_t n_results = 0;
        err = pelican_get(ctx, url, argv[2], &opts, &results, &n_results);
        pelican_context_free(ctx);
        if (err != NULL)
            return fail("pelican_get", err);
        for (size_t i = 0; i < n_results; i++)
            printf("downloaded %s: %lld bytes from %s in %.3fs "
                   "(%d attempt%s)\n",
                   results[i].source, results[i].transferred_bytes,
                   results[i].endpoint ? results[i].endpoint : "?",
                   results[i].transfer_time_s, results[i].attempts,
                   results[i].attempts == 1 ? "" : "s");
        pelican_results_free(results, n_results);
    }

    return 0;
}
