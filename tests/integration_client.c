/*
 * integration_client.c - scriptable driver exercising libpelicanclient.
 *
 * Run by the federation integration test (integration/integration_test.go),
 * which launches a Pelican federation and invokes these subcommands:
 *
 *   integration_client stat      <url>
 *   integration_client list      <url>
 *   integration_client get       <url> <local-path> [opt...]
 *   integration_client get-async <url> <local-path>
 *   integration_client put       <local-path> <url>
 *   integration_client copy      <source-url> <dest-url>
 *   integration_client fs-read   <url>
 *   integration_client delete    <url>
 *   integration_client stat-async    <url>
 *   integration_client list-async    <url>
 *   integration_client fs-read-async <url>
 *   integration_client logged-get    <url> <local-path>
 *
 * Trailing [opt...] arguments for get: "checksum=<digest>",
 * "cache=<url>", "require-checksum".
 *
 * The *-async subcommands drive the single-shot operation API through a
 * poll() loop, the way a DaemonCore Register_Pipe handler would.
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
            if (pelican_result_endpoint(r) != NULL)
                printf("endpoint=%s\n", pelican_result_endpoint(r));
            if (pelican_result_etag(r) != NULL)
                printf("etag=%s\n", pelican_result_etag(r));
            for (size_t c = 0; c < pelican_result_checksum_count(r); c++)
                printf("checksum=%s:%s\n",
                       pelican_result_checksum_type(r, c),
                       pelican_result_checksum_value(r, c));
        }
    }
    pelican_result_list_free(results);
    return failures;
}

static int
apply_opt_args(pelican_transfer_opts *opts, int argc, char **argv, int start)
{
    for (int i = start; i < argc; i++) {
        if (strncmp(argv[i], "checksum=", 9) == 0)
            pelican_transfer_opts_add_checksum_request(opts, argv[i] + 9);
        else if (strncmp(argv[i], "cache=", 6) == 0)
            pelican_transfer_opts_add_cache(opts, argv[i] + 6);
        else if (strcmp(argv[i], "require-checksum") == 0)
            pelican_transfer_opts_set_require_checksum(opts, 1);
        else {
            fprintf(stderr, "unknown option argument: %s\n", argv[i]);
            return -1;
        }
    }
    return 0;
}

static int
cmd_get(const char *url, const char *local_path, int argc, char **argv)
{
    pelican_transfer_opts *opts = pelican_transfer_opts_new();
    if (apply_opt_args(opts, argc, argv, 4) != 0) {
        pelican_transfer_opts_free(opts);
        return 2;
    }
    pelican_result_list *results = NULL;
    pelican_error *err = pelican_get(NULL, url, local_path, opts, &results);
    pelican_transfer_opts_free(opts);
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

static int
cmd_copy(const char *source_url, const char *dest_url)
{
    pelican_result_list *results = NULL;
    pelican_error *err = pelican_copy(NULL, source_url, dest_url, NULL, &results);
    if (err != NULL)
        return fail("copy", err);
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

/* Block in poll() until the operation's notification fd fires, exactly
 * as an event loop would before dispatching its read handler. */
static int
await_op(pelican_op *op)
{
    struct pollfd pfd;
    pfd.fd = pelican_op_notify_fd(op);
    pfd.events = POLLIN;
    while (!pelican_op_is_done(op)) {
        if (poll(&pfd, 1, -1) < 0) {
            perror("poll");
            return -1;
        }
    }
    return 0;
}

static int
cmd_stat_async(const char *url)
{
    pelican_op *op = NULL;
    pelican_error *err = pelican_stat_start(url, NULL, &op);
    if (err != NULL)
        return fail("stat_start", err);
    if (await_op(op) != 0) {
        pelican_op_free(op);
        return 1;
    }
    const pelican_error *oerr = pelican_op_error(op);
    if (oerr != NULL) {
        fprintf(stderr, "stat-async failed: %s\n",
                pelican_error_message(oerr));
        pelican_op_free(op);
        return 1;
    }
    pelican_file_info *info = NULL;
    if (!pelican_op_take_file_info(op, &info)) {
        fprintf(stderr, "stat-async produced no file info\n");
        pelican_op_free(op);
        return 1;
    }
    pelican_op_free(op);
    printf("name=%s\nsize=%lld\nis_collection=%d\n",
           pelican_file_info_name(info), pelican_file_info_size(info),
           pelican_file_info_is_collection(info));
    pelican_file_info_free(info);
    return 0;
}

static int
cmd_list_async(const char *url)
{
    pelican_op *op = NULL;
    pelican_error *err = pelican_list_start(url, NULL, &op);
    if (err != NULL)
        return fail("list_start", err);
    if (await_op(op) != 0) {
        pelican_op_free(op);
        return 1;
    }
    const pelican_error *oerr = pelican_op_error(op);
    if (oerr != NULL) {
        fprintf(stderr, "list-async failed: %s\n",
                pelican_error_message(oerr));
        pelican_op_free(op);
        return 1;
    }
    pelican_file_info_list *entries = NULL;
    if (!pelican_op_take_file_info_list(op, &entries)) {
        fprintf(stderr, "list-async produced no listing\n");
        pelican_op_free(op);
        return 1;
    }
    pelican_op_free(op);
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

/* Open, read to EOF, and close a remote object using only the
 * non-blocking calls.  Also checks that the handle refuses a second
 * concurrent operation. */
static int
cmd_fs_read_async(const char *url)
{
    pelican_error *err;
    pelican_op *op = NULL;

    if ((err = pelican_fs_open_start(url, PELICAN_O_RDONLY, NULL, &op)) != NULL)
        return fail("fs_open_start", err);
    if (await_op(op) != 0) {
        pelican_op_free(op);
        return 1;
    }
    const pelican_error *oerr = pelican_op_error(op);
    if (oerr != NULL) {
        fprintf(stderr, "fs_open_start failed: %s\n",
                pelican_error_message(oerr));
        pelican_op_free(op);
        return 1;
    }
    pelican_file *file = NULL;
    if (!pelican_op_take_file(op, &file)) {
        fprintf(stderr, "fs_open_start produced no file\n");
        pelican_op_free(op);
        return 1;
    }
    pelican_op_free(op);

    long long total = 0;
    int busy_rejected = 0;
    for (;;) {
        pelican_op *read_op = NULL;
        if ((err = pelican_file_read_start(file, 65536, &read_op)) != NULL) {
            pelican_error_free(pelican_file_close(file));
            return fail("file_read_start", err);
        }

        /* A second operation on a busy handle must be refused rather
         * than corrupting the stream. */
        if (total == 0) {
            pelican_op *conflict = NULL;
            pelican_error *berr =
                pelican_file_read_start(file, 16, &conflict);
            if (berr != NULL) {
                busy_rejected = 1;
                pelican_error_free(berr);
            } else {
                fprintf(stderr, "THREADING VIOLATION: concurrent read on "
                                "a busy handle was accepted\n");
                pelican_op_free(conflict);
            }
        }

        if (await_op(read_op) != 0) {
            pelican_op_free(read_op);
            pelican_error_free(pelican_file_close(file));
            return 1;
        }
        oerr = pelican_op_error(read_op);
        if (oerr != NULL) {
            fprintf(stderr, "file_read_start failed: %s\n",
                    pelican_error_message(oerr));
            pelican_op_free(read_op);
            pelican_error_free(pelican_file_close(file));
            return 1;
        }
        void *buf = NULL;
        size_t len = 0;
        if (!pelican_op_take_data(read_op, &buf, &len)) {
            fprintf(stderr, "read produced no data result\n");
            pelican_op_free(read_op);
            pelican_error_free(pelican_file_close(file));
            return 1;
        }
        pelican_op_free(read_op);
        if (len == 0) { /* end of file */
            pelican_buffer_free(buf);
            break;
        }
        fwrite(buf, 1, len, stdout);
        total += (long long)len;
        pelican_buffer_free(buf);
    }
    fprintf(stderr, "fs_read_bytes=%lld\n", total);

    /* Asynchronous close takes ownership of the handle. */
    if ((err = pelican_file_close_start(file, &op)) != NULL)
        return fail("file_close_start", err);
    if (await_op(op) != 0) {
        pelican_op_free(op);
        return 1;
    }
    oerr = pelican_op_error(op);
    if (oerr != NULL) {
        fprintf(stderr, "file_close_start failed: %s\n",
                pelican_error_message(oerr));
        pelican_op_free(op);
        return 1;
    }
    pelican_op_free(op);

    printf("busy_rejected=%d\n", busy_rejected);
    return busy_rejected ? 0 : 1;
}

/* State for the queued-logging check. */
static pthread_t log_pump_thread;
static int log_records;
static int log_off_thread;
static int log_levels_seen[PELICAN_LOG_TRACE + 1];

static void
log_sink(pelican_log_level level, const char *message, void *user_data)
{
    (void)user_data;
    log_records++;
    if (level >= PELICAN_LOG_ERROR && level <= PELICAN_LOG_TRACE)
        log_levels_seen[level]++;
    if (!pthread_equal(pthread_self(), log_pump_thread))
        log_off_thread++;
    /* Stand-in for a host logging subsystem (e.g. condor's dprintf). */
    fprintf(stderr, "[pelican:%d] %s\n", (int)level, message);
}

/* Run a transfer with the client's log output routed into a host
 * callback, verifying that queued records are delivered only from
 * pelican_log_pump() on the pumping thread. */
static int
cmd_logged_get(const char *url, const char *local_path)
{
    pelican_error *err;

    log_pump_thread = pthread_self();

    if ((err = pelican_log_set_callback(log_sink, NULL, PELICAN_LOG_QUEUED)) != NULL)
        return fail("log_set_callback", err);
    if ((err = pelican_log_set_level(PELICAN_LOG_DEBUG)) != NULL)
        return fail("log_set_level", err);

    int fd = pelican_log_notify_fd();
    if (fd < 0) {
        fprintf(stderr, "queued logging did not provide a notification fd\n");
        return 1;
    }

    /* Nothing may be delivered while the transfer runs: records only
     * reach the callback from pump(), below. */
    pelican_transfer *xfer = NULL;
    if ((err = pelican_get_start(url, local_path, NULL, &xfer)) != NULL)
        return fail("get_start", err);

    struct pollfd pfds[2];
    pfds[0].fd = pelican_transfer_notify_fd(xfer);
    pfds[0].events = POLLIN;
    pfds[1].fd = fd;
    pfds[1].events = POLLIN;

    int failures = 0;
    for (;;) {
        if (poll(pfds, 2, -1) < 0) {
            perror("poll");
            pelican_transfer_free(xfer);
            return 1;
        }
        /* A real daemon would dispatch these as two independent fd
         * handlers; both run on this thread. */
        pelican_log_pump();

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

    /* Collect anything logged during teardown. */
    pelican_log_pump();

    /* Uninstalling must stop delivery; records logged afterward are
     * discarded rather than queued forever. */
    if ((err = pelican_log_set_callback(NULL, NULL, PELICAN_LOG_QUEUED)) != NULL)
        return fail("log_set_callback(NULL)", err);
    if (pelican_log_notify_fd() >= 0) {
        fprintf(stderr, "notification fd survived callback removal\n");
        failures++;
    }

    printf("log_records=%d\nlog_off_thread=%d\nlog_debug_records=%d\n",
           log_records, log_off_thread, log_levels_seen[PELICAN_LOG_DEBUG]);
    if (log_off_thread > 0) {
        fprintf(stderr,
                "THREADING VIOLATION: %d log record(s) were delivered off "
                "the pumping thread\n",
                log_off_thread);
        failures++;
    }
    if (log_records == 0) {
        fprintf(stderr, "no log records were delivered\n");
        failures++;
    }
    return failures ? 1 : 0;
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
        return cmd_get(argv[2], argv[3], argc, argv);
    if (strcmp(cmd, "get-async") == 0 && argc >= 4)
        return cmd_get_async(argv[2], argv[3]);
    if (strcmp(cmd, "put") == 0 && argc >= 4)
        return cmd_put(argv[2], argv[3]);
    if (strcmp(cmd, "copy") == 0 && argc >= 4)
        return cmd_copy(argv[2], argv[3]);
    if (strcmp(cmd, "fs-read") == 0)
        return cmd_fs_read(argv[2]);
    if (strcmp(cmd, "delete") == 0)
        return cmd_delete(argv[2]);
    if (strcmp(cmd, "stat-async") == 0)
        return cmd_stat_async(argv[2]);
    if (strcmp(cmd, "list-async") == 0)
        return cmd_list_async(argv[2]);
    if (strcmp(cmd, "fs-read-async") == 0)
        return cmd_fs_read_async(argv[2]);
    if (strcmp(cmd, "logged-get") == 0 && argc >= 4)
        return cmd_logged_get(argv[2], argv[3]);

    fprintf(stderr, "unknown subcommand: %s\n", cmd);
    return 2;
}
