/*
 * integration_client.c - scriptable driver exercising libpelicanclient.
 *
 * Run by the federation integration test (integration/integration_test.go),
 * which launches a Pelican federation and invokes these subcommands:
 *
 *   integration_client stat      <url>
 *   integration_client list      <url>
 *   integration_client get       <url> <local-path>
 *   integration_client get-async <url> <local-path>
 *   integration_client put       <local-path> <url>
 *   integration_client fs-read   <url>
 *   integration_client delete    <url>
 *
 * Output is line-oriented `key=value` pairs on stdout for the test
 * harness to assert on.  get-async additionally verifies the async-mode
 * threading contract: every progress callback must run on the thread
 * calling pelican_transfer_next_result, never on a library thread.
 *
 * Copyright (C) 2026, Pelican Project, Morgridge Institute for Research
 * SPDX-License-Identifier: Apache-2.0
 */

#include <poll.h>
#include <pthread.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include <pelican/client.h>

static int
fail(const char *what, pelican_error *err)
{
    fprintf(stderr, "%s failed: %s (retryable=%d code=%d)\n", what,
            pelican_error_message(err), pelican_error_is_retryable(err),
            pelican_error_code(err));
    pelican_error_free(err);
    return 1;
}

static int
cmd_stat(const char *url)
{
    pelican_file_info *info = NULL;
    pelican_error *err = pelican_stat(NULL, url, NULL, &info);
    if (err != NULL)
        return fail("stat", err);
    printf("name=%s\nsize=%lld\nis_collection=%d\n",
           pelican_file_info_name(info), pelican_file_info_size(info),
           pelican_file_info_is_collection(info));
    pelican_file_info_free(info);
    return 0;
}

static int
cmd_list(const char *url)
{
    pelican_file_info_list *entries = NULL;
    pelican_error *err = pelican_list(NULL, url, NULL, &entries);
    if (err != NULL)
        return fail("list", err);
    printf("count=%zu\n", pelican_file_info_list_count(entries));
    for (size_t i = 0; i < pelican_file_info_list_count(entries); i++) {
        const pelican_file_info *e = pelican_file_info_list_get(entries, i);
        printf("entry=%s size=%lld collection=%d\n",
               pelican_file_info_name(e), pelican_file_info_size(e),
               pelican_file_info_is_collection(e));
    }
    pelican_file_info_list_free(entries);
    return 0;
}

static int
report_results(pelican_result_list *results)
{
    int failures = 0;
    for (size_t i = 0; i < pelican_result_list_count(results); i++) {
        const pelican_result *r = pelican_result_list_get(results, i);
        const pelican_error *rerr = pelican_result_error(r);
        if (rerr != NULL) {
            fprintf(stderr, "object %s failed: %s\n",
                    pelican_result_source(r), pelican_error_message(rerr));
            failures++;
        } else {
            printf("object=%s bytes=%lld\n", pelican_result_source(r),
                   pelican_result_transferred_bytes(r));
        }
    }
    pelican_result_list_free(results);
    return failures;
}

static int
cmd_get(const char *url, const char *local_path)
{
    pelican_result_list *results = NULL;
    pelican_error *err = pelican_get(NULL, url, local_path, NULL, &results);
    if (err != NULL)
        return fail("get", err);
    return report_results(results) ? 1 : 0;
}

static int
cmd_put(const char *local_path, const char *url)
{
    pelican_result_list *results = NULL;
    pelican_error *err = pelican_put(NULL, local_path, url, NULL, &results);
    if (err != NULL)
        return fail("put", err);
    return report_results(results) ? 1 : 0;
}

/* State for the async threading check. */
static pthread_t loop_thread;
static int progress_calls;
static int progress_off_thread;

static void
progress(const char *object, long long transferred, long long total,
         int completed, void *user_data)
{
    (void)object;
    (void)transferred;
    (void)total;
    (void)completed;
    (void)user_data;
    progress_calls++;
    if (!pthread_equal(pthread_self(), loop_thread))
        progress_off_thread++;
}

static int
cmd_get_async(const char *url, const char *local_path)
{
    loop_thread = pthread_self();

    pelican_transfer_opts *opts = pelican_transfer_opts_new();
    pelican_transfer_opts_set_progress(opts, progress, NULL);

    pelican_transfer *xfer = NULL;
    pelican_error *err = pelican_get_start(url, local_path, opts, &xfer);
    pelican_transfer_opts_free(opts);
    if (err != NULL)
        return fail("get_start", err);

    struct pollfd pfd;
    pfd.fd = pelican_transfer_notify_fd(xfer);
    pfd.events = POLLIN;

    int failures = 0;
    for (;;) {
        if (poll(&pfd, 1, -1) < 0) {
            perror("poll");
            pelican_transfer_free(xfer);
            return 1;
        }
        pelican_result *res;
        while (pelican_transfer_next_result(xfer, &res)) {
            const pelican_error *rerr = pelican_result_error(res);
            if (rerr != NULL) {
                fprintf(stderr, "object %s failed: %s\n",
                        pelican_result_source(res),
                        pelican_error_message(rerr));
                failures++;
            } else {
                printf("object=%s bytes=%lld\n",
                       pelican_result_source(res),
                       pelican_result_transferred_bytes(res));
            }
            pelican_result_free(res);
        }
        if (pelican_transfer_is_done(xfer))
            break;
    }

    const pelican_error *terr = pelican_transfer_error(xfer);
    if (terr != NULL) {
        fprintf(stderr, "transfer failed: %s\n",
                pelican_error_message(terr));
        failures++;
    }
    pelican_transfer_free(xfer);

    printf("progress_calls=%d\nprogress_off_thread=%d\n", progress_calls,
           progress_off_thread);
    if (progress_off_thread > 0) {
        fprintf(stderr,
                "THREADING VIOLATION: %d progress callback(s) ran off "
                "the event-loop thread\n",
                progress_off_thread);
        failures++;
    }
    return failures ? 1 : 0;
}

static int
cmd_fs_read(const char *url)
{
    pelican_file *file = NULL;
    pelican_error *err = pelican_fs_open(url, PELICAN_O_RDONLY, NULL, &file);
    if (err != NULL)
        return fail("fs_open", err);

    char buf[65536];
    long long n, total = 0;
    while ((n = pelican_file_read(file, buf, sizeof(buf), &err)) > 0) {
        fwrite(buf, 1, (size_t)n, stdout);
        total += n;
    }
    if (n < 0) {
        pelican_error_free(pelican_file_close(file));
        return fail("fs_read", err);
    }
    fprintf(stderr, "fs_read_bytes=%lld\n", total);

    if ((err = pelican_file_close(file)) != NULL)
        return fail("fs_close", err);
    return 0;
}

static int
cmd_delete(const char *url)
{
    pelican_error *err = pelican_delete(NULL, url, NULL);
    if (err != NULL)
        return fail("delete", err);
    puts("deleted=1");
    return 0;
}

int
main(int argc, char **argv)
{
    if (argc < 3) {
        fprintf(stderr, "usage: %s <subcommand> <args...>\n", argv[0]);
        return 2;
    }

    pelican_error *err = pelican_client_init(NULL);
    if (err != NULL)
        return fail("client_init", err);

    const char *cmd = argv[1];
    if (strcmp(cmd, "stat") == 0)
        return cmd_stat(argv[2]);
    if (strcmp(cmd, "list") == 0)
        return cmd_list(argv[2]);
    if (strcmp(cmd, "get") == 0 && argc >= 4)
        return cmd_get(argv[2], argv[3]);
    if (strcmp(cmd, "get-async") == 0 && argc >= 4)
        return cmd_get_async(argv[2], argv[3]);
    if (strcmp(cmd, "put") == 0 && argc >= 4)
        return cmd_put(argv[2], argv[3]);
    if (strcmp(cmd, "fs-read") == 0)
        return cmd_fs_read(argv[2]);
    if (strcmp(cmd, "delete") == 0)
        return cmd_delete(argv[2]);

    fprintf(stderr, "unknown subcommand: %s\n", cmd);
    return 2;
}
