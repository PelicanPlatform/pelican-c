/*
 * bridge.h - internal cgo helpers for the pelican-c shim.
 *
 * Not installed; the public API lives in include/pelican/client.h.
 *
 * Copyright (C) 2026, Pelican Project, Morgridge Institute for Research
 * SPDX-License-Identifier: Apache-2.0
 */

#ifndef PELICAN_C_BRIDGE_H
#define PELICAN_C_BRIDGE_H

#include <stdint.h>

#include "pelican/client.h"

/* The opaque cancellation handle: wraps a Go cgo.Handle. */
struct pelican_context {
    uintptr_t handle;
};

/* Trampoline so Go code can invoke a C function pointer. */
void pelicanc_invoke_progress(pelican_progress_fn fn, const char *object,
                              long long transferred, long long total,
                              int completed, void *user_data);

/* calloc-based allocators so Go hands out C-owned memory. */
pelican_error *pelicanc_error_alloc(void);
pelican_result *pelicanc_result_alloc(size_t n);
pelican_file_info *pelicanc_file_info_alloc(size_t n);
struct pelican_context *pelicanc_context_alloc(void);

#endif /* PELICAN_C_BRIDGE_H */
