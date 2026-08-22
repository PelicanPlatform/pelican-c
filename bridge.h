/*
 * bridge.h - internal cgo helpers for the pelican-c shim.
 *
 * Defines the layouts behind the opaque public types.  Nothing here is
 * installed; the public API lives in include/pelican/client.h and the
 * layouts may change freely between releases.
 *
 * Copyright (C) 2026, Pelican Project, Morgridge Institute for Research
 * SPDX-License-Identifier: Apache-2.0
 */

#ifndef PELICAN_C_BRIDGE_H
#define PELICAN_C_BRIDGE_H

#include <stdint.h>

#include "pelican/client.h"

struct pelican_error {
    char *message;
    int   retryable;
    int   code;
};

/* Handles wrapping Go-side state (a cgo.Handle value). */
struct pelican_context {
    uintptr_t handle;
};

struct pelican_transfer {
    uintptr_t handle;
    int       notify_fd; /* read end of the wakeup pipe */
};

struct pelican_file {
    uintptr_t handle;
};

struct pelican_transfer_opts {
    char *token;
    char *token_location;
    char *source_token;
    char *source_token_location;
    char *dest_token;
    char *dest_token_location;
    int   recursive;
    pelican_progress_fn progress;
    void *progress_data;
};

struct pelican_checksum {
    char *type;  /* HTTP digest name */
    char *value; /* hex-encoded */
};

struct pelican_result {
    char          *source;
    char          *endpoint;
    char          *etag;
    long long      transferred_bytes;
    double         transfer_time_s;
    int            attempts;
    pelican_error *error;
    struct pelican_checksum *checksums;
    size_t         n_checksums;
};

struct pelican_result_list {
    pelican_result **items;
    size_t           count;
};

struct pelican_file_info {
    char     *name;
    long long size;
    long long mtime;
    int       is_collection;
};

struct pelican_file_info_list {
    pelican_file_info **items;
    size_t              count;
};

/* Trampoline so Go code can invoke a C function pointer. */
void pelicanc_invoke_progress(pelican_progress_fn fn, const char *object,
                              long long transferred, long long total,
                              int completed, void *user_data);

/* calloc-based allocators so Go hands out C-owned memory. */
pelican_error *pelicanc_error_alloc(void);
pelican_result *pelicanc_result_alloc(void);
struct pelican_checksum *pelicanc_checksum_alloc(size_t n);
pelican_result_list *pelicanc_result_list_alloc(size_t n);
pelican_file_info *pelicanc_file_info_alloc(void);
pelican_file_info_list *pelicanc_file_info_list_alloc(size_t n);
struct pelican_context *pelicanc_context_alloc(void);
struct pelican_transfer *pelicanc_transfer_alloc(void);
struct pelican_file *pelicanc_file_alloc(void);

#endif /* PELICAN_C_BRIDGE_H */
