/*
 * pelican_example.c - demonstrate synchronous libpelicanclient usage.
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
    fprintf(stderr, "%s failed: %s (retryable: %s, code: %d)\n", what,
            pelican_error_message(err),
            pelican_error_is_retryable(err) ? "yes" : "no",
            pelican_error_code(err));
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
         * not a crash. */
        pelican_file_info *info = NULL;
        err = pelican_stat(NULL, "not-a-valid-url", NULL, &info);
        if (err == NULL) {
            fprintf(stderr, "expected an error from a bogus stat\n");
            return 1;
        }
        printf("bogus stat correctly failed: %s\n",
               pelican_error_message(err));
        pelican_error_free(err);
        puts("smoke test passed");
        return 0;
    }

    const char *url = argv[1];

    pelican_file_info *info = NULL;
    if ((err = pelican_stat(NULL, url, NULL, &info)) != NULL)
        return fail("pelican_stat", err);
    printf("stat: name=%s size=%lld mtime=%lld collection=%d\n",
           pelican_file_info_name(info), pelican_file_info_size(info),
           pelican_file_info_mtime(info),
           pelican_file_info_is_collection(info));
    int is_collection = pelican_file_info_is_collection(info);
    pelican_file_info_free(info);

    if (is_collection && argc < 3) {
        pelican_file_info_list *entries = NULL;
        if ((err = pelican_list(NULL, url, NULL, &entries)) != NULL)
            return fail("pelican_list", err);
        for (size_t i = 0; i < pelican_file_info_list_count(entries); i++) {
            const pelican_file_info *e = pelican_file_info_list_get(entries, i);
            printf("  %c %12lld %s\n",
                   pelican_file_info_is_collection(e) ? 'd' : '-',
                   pelican_file_info_size(e), pelican_file_info_name(e));
        }
        pelican_file_info_list_free(entries);
    }

    if (argc >= 3) {
        pelican_transfer_opts *opts = pelican_transfer_opts_new();
        pelican_transfer_opts_set_progress(opts, progress, NULL);

        pelican_context *ctx = pelican_context_new();
        pelican_result_list *results = NULL;
        err = pelican_get(ctx, url, argv[2], opts, &results);
        pelican_context_free(ctx);
        pelican_transfer_opts_free(opts);
        if (err != NULL)
            return fail("pelican_get", err);
        for (size_t i = 0; i < pelican_result_list_count(results); i++) {
            const pelican_result *r = pelican_result_list_get(results, i);
            printf("downloaded %s: %lld bytes from %s in %.3fs "
                   "(%d attempt%s)\n",
                   pelican_result_source(r),
                   pelican_result_transferred_bytes(r),
                   pelican_result_endpoint(r) ? pelican_result_endpoint(r)
                                              : "?",
                   pelican_result_transfer_time_s(r),
                   pelican_result_attempts(r),
                   pelican_result_attempts(r) == 1 ? "" : "s");
        }
        pelican_result_list_free(results);
    }

    return 0;
}
