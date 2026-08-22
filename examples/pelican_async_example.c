/*
 * pelican_async_example.c - event-loop-style libpelicanclient usage.
 *
 * Demonstrates the two facilities aimed at single-threaded daemons such
 * as HTCondor DaemonCore:
 *
 *   1. An asynchronous download: pelican_get_start() returns
 *      immediately; a poll() loop watches the transfer's notification
 *      fd and pops per-object results as they complete, exactly as a
 *      DaemonCore Register_Pipe handler would.
 *
 *   2. PelicanFS file I/O: the downloaded object is re-read directly
 *      from the federation — sequential read plus a positional pread —
 *      without staging to local disk.
 *
 * Usage:
 *   pelican_async_example <remote-url> <local-path>
 *
 * Copyright (C) 2026, Pelican Project, Morgridge Institute for Research
 * SPDX-License-Identifier: Apache-2.0
 */

#include <poll.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include <pelican/client.h>

static int
fail(const char *what, pelican_error *err)
{
    fprintf(stderr, "%s failed: %s (retryable: %s)\n", what,
            pelican_error_message(err),
            pelican_error_is_retryable(err) ? "yes" : "no");
    pelican_error_free(err);
    return 1;
}

static int
run_async_download(const char *url, const char *local_path)
{
    pelican_error *err;
    pelican_transfer *xfer = NULL;

    if ((err = pelican_get_start(url, local_path, NULL, &xfer)) != NULL)
        return fail("pelican_get_start", err);
    printf("transfer submitted; notify fd = %d\n",
           pelican_transfer_notify_fd(xfer));

    /* The event loop.  In a real daemon this fd would be registered
     * with the loop (e.g. DaemonCore Register_Pipe) instead. */
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

        /* Wakeup: drain every pending result, then check for
         * completion.  next_result never blocks. */
        pelican_result *res;
        while (pelican_transfer_next_result(xfer, &res)) {
            const pelican_error *rerr = pelican_result_error(res);
            if (rerr != NULL) {
                fprintf(stderr, "object %s failed: %s\n",
                        pelican_result_source(res),
                        pelican_error_message(rerr));
                failures++;
            } else {
                printf("object %s done: %lld bytes from %s in %.3fs\n",
                       pelican_result_source(res),
                       pelican_result_transferred_bytes(res),
                       pelican_result_endpoint(res)
                           ? pelican_result_endpoint(res) : "?",
                       pelican_result_transfer_time_s(res));
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
    return failures ? 1 : 0;
}

static int
run_fs_read(const char *url)
{
    pelican_error *err;
    pelican_file *file = NULL;

    if ((err = pelican_fs_open(url, PELICAN_O_RDONLY, NULL, &file)) != NULL)
        return fail("pelican_fs_open", err);

    pelican_file_info *info = NULL;
    if ((err = pelican_file_stat(file, &info)) != NULL) {
        pelican_error_free(pelican_file_close(file));
        return fail("pelican_file_stat", err);
    }
    printf("fs stat: name=%s size=%lld\n", pelican_file_info_name(info),
           pelican_file_info_size(info));
    pelican_file_info_free(info);

    /* Sequential read of the whole object. */
    char buf[4096];
    long long n, total = 0;
    while ((n = pelican_file_read(file, buf, sizeof(buf), &err)) > 0)
        total += n;
    if (n < 0) {
        pelican_error_free(pelican_file_close(file));
        return fail("pelican_file_read", err);
    }
    printf("fs read: %lld bytes sequentially\n", total);

    /* Positional read of the first few bytes, without seeking. */
    n = pelican_file_pread(file, buf, sizeof(buf) - 1, 0, &err);
    if (n < 0) {
        pelican_error_free(pelican_file_close(file));
        return fail("pelican_file_pread", err);
    }
    buf[n] = '\0';
    printf("fs pread(0): %lld bytes: %.40s%s\n", n, buf,
           n > 40 ? "..." : "");

    if ((err = pelican_file_close(file)) != NULL)
        return fail("pelican_file_close", err);
    return 0;
}

int
main(int argc, char **argv)
{
    pelican_error *err;

    if (argc < 3) {
        fprintf(stderr, "usage: %s <remote-url> <local-path>\n", argv[0]);
        return 2;
    }

    if ((err = pelican_client_init(NULL)) != NULL)
        return fail("pelican_client_init", err);

    if (run_async_download(argv[1], argv[2]) != 0)
        return 1;
    return run_fs_read(argv[1]);
}
