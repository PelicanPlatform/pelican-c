/***************************************************************
 *
 * Copyright (C) 2026, Pelican Project, Morgridge Institute for Research
 *
 * Licensed under the Apache License, Version 2.0 (the "License"); you
 * may not use this file except in compliance with the License.  You may
 * obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 ***************************************************************/

// Package main implements the cgo shim behind libpelicanclient.
//
// The exported functions here use internal `pelicanc_*` names; the public,
// const-correct entry points declared in include/pelican/client.h are thin
// forwarding wrappers defined in bridge.c.  Keep the three layers in sync:
// client.h (public ABI), bridge.c (wrappers), this file (implementation).
package main

/*
#cgo CFLAGS: -I${SRCDIR}/include
#include <stdlib.h>
#include "bridge.h"
*/
import "C"

import (
	"context"
	"fmt"
	"runtime/cgo"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/pelicanplatform/pelican/client"
	"github.com/pelicanplatform/pelican/config"
	"github.com/spf13/viper"
)

var (
	initOnce    sync.Once
	initialized atomic.Bool
	versionOnce sync.Once
	versionCStr *C.char
)

// makeError converts a Go error into a caller-owned pelican_error.
func makeError(err error) *C.pelican_error {
	if err == nil {
		return nil
	}
	ce := C.pelicanc_error_alloc()
	ce.message = C.CString(err.Error())
	if client.ShouldRetry(err) {
		ce.retryable = 1
	}
	return ce
}

// guard runs fn, converting any Go panic into an error instead of letting
// it cross the cgo boundary and abort the host process.
func guard(fn func() *C.pelican_error) (ce *C.pelican_error) {
	defer func() {
		if r := recover(); r != nil {
			ce = makeError(fmt.Errorf("panic in pelican client: %v", r))
		}
	}()
	return fn()
}

func requireInit() error {
	if !initialized.Load() {
		return fmt.Errorf("pelican client library is not initialized; call pelican_client_init first")
	}
	return nil
}

// goCtxFor resolves the optional C cancellation handle to a Go context.
func goCtxFor(cctx *C.struct_pelican_context) context.Context {
	if cctx == nil || cctx.handle == 0 {
		return context.Background()
	}
	return cgo.Handle(cctx.handle).Value().(*cancelContext).ctx
}

type cancelContext struct {
	ctx    context.Context
	cancel context.CancelFunc
}

// buildOptions translates the C options struct into client.TransferOption
// values.  The library always runs non-interactively: it must never block
// the host process on an OAuth device-flow prompt.
func buildOptions(copts *C.pelican_transfer_opts) (opts []client.TransferOption, recursive bool) {
	opts = append(opts, client.WithNonInteractive(true))
	if copts == nil {
		return
	}
	if copts.token != nil {
		opts = append(opts, client.WithToken(C.GoString(copts.token)))
	}
	if copts.token_location != nil {
		opts = append(opts, client.WithTokenLocation(C.GoString(copts.token_location)))
	}
	recursive = copts.recursive != 0
	if copts.progress != nil {
		fn := copts.progress
		userData := copts.progress_data
		opts = append(opts, client.WithCallback(func(path string, downloaded int64, totalSize int64, completed bool) {
			cPath := C.CString(path)
			cCompleted := C.int(0)
			if completed {
				cCompleted = 1
			}
			C.pelicanc_invoke_progress(fn, cPath, C.longlong(downloaded), C.longlong(totalSize), cCompleted, userData)
			C.free(unsafe.Pointer(cPath))
		}))
	}
	return
}

// pelicanModuleVersion reports the version of the pelican module compiled
// into this library, so pelican_version() is meaningful even though the
// upstream ldflags-based version stamping does not apply here.
func pelicanModuleVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, dep := range bi.Deps {
			if dep.Path == "github.com/pelicanplatform/pelican" {
				if dep.Replace != nil {
					return dep.Replace.Version
				}
				return dep.Version
			}
		}
	}
	return ""
}

// Upstream stamps its version via ldflags on its own builds; that never
// applies when pelican is an imported module, so stamp the module version
// at load time to keep pelican_version() (and the User-Agent) meaningful.
func init() {
	if v := pelicanModuleVersion(); v != "" {
		config.SetVersion(v)
	}
}

// fillResults converts per-object transfer results into a C array.
// fallbackSource covers download results, which (unlike the error path)
// are produced without TransferResults.Source populated.
func fillResults(results []client.TransferResults, fallbackSource string, cresults **C.pelican_result, nresults *C.size_t) {
	if cresults == nil || nresults == nil {
		return
	}
	*cresults = nil
	*nresults = 0
	if len(results) == 0 {
		return
	}
	arr := C.pelicanc_result_alloc(C.size_t(len(results)))
	slice := unsafe.Slice(arr, len(results))
	for i, r := range results {
		source := r.Source
		if source == "" {
			source = fallbackSource
		}
		slice[i].source = C.CString(source)
		slice[i].transferred_bytes = C.longlong(r.TransferredBytes)
		slice[i].attempts = C.int(len(r.Attempts))
		if n := len(r.Attempts); n > 0 {
			last := r.Attempts[n-1]
			slice[i].endpoint = C.CString(last.Endpoint)
			slice[i].transfer_time_s = C.double(last.TransferTime.Seconds())
		}
		slice[i].error = makeError(r.Error)
	}
	*cresults = arr
	*nresults = C.size_t(len(results))
}

//export pelicanc_config_set
func pelicanc_config_set(key, value *C.char) *C.pelican_error {
	return guard(func() *C.pelican_error {
		if initialized.Load() {
			return makeError(fmt.Errorf("pelican_config_set must be called before pelican_client_init"))
		}
		if key == nil {
			return makeError(fmt.Errorf("configuration key may not be NULL"))
		}
		viper.Set(C.GoString(key), C.GoString(value))
		return nil
	})
}

//export pelicanc_client_init
func pelicanc_client_init(configFile *C.char) *C.pelican_error {
	return guard(func() *C.pelican_error {
		var initErr error
		initOnce.Do(func() {
			if configFile != nil {
				path := C.GoString(configFile)
				// Validate up front: config.InitClient aborts the process
				// (cobra.CheckErr) on an unreadable or malformed config
				// file, which must never happen inside a host application.
				probe := viper.New()
				probe.SetConfigType("yaml")
				probe.SetConfigFile(path)
				if err := probe.ReadInConfig(); err != nil {
					initErr = fmt.Errorf("cannot use pelican config file: %w", err)
					return
				}
				viper.Set("config", path)
			}
			if initErr = config.InitClient(); initErr == nil {
				initialized.Store(true)
			}
		})
		if initErr == nil && !initialized.Load() {
			initErr = fmt.Errorf("a previous pelican_client_init attempt failed; library is unusable in this process")
		}
		return makeError(initErr)
	})
}

//export pelicanc_version
func pelicanc_version() *C.char {
	versionOnce.Do(func() {
		versionCStr = C.CString(config.GetVersion())
	})
	return versionCStr
}

//export pelicanc_context_new
func pelicanc_context_new() *C.struct_pelican_context {
	cctx := C.pelicanc_context_alloc()
	ctx, cancel := context.WithCancel(context.Background())
	cctx.handle = C.uintptr_t(cgo.NewHandle(&cancelContext{ctx: ctx, cancel: cancel}))
	return cctx
}

//export pelicanc_context_cancel
func pelicanc_context_cancel(cctx *C.struct_pelican_context) {
	if cctx == nil || cctx.handle == 0 {
		return
	}
	cgo.Handle(cctx.handle).Value().(*cancelContext).cancel()
}

//export pelicanc_context_free
func pelicanc_context_free(cctx *C.struct_pelican_context) {
	if cctx == nil {
		return
	}
	if cctx.handle != 0 {
		h := cgo.Handle(cctx.handle)
		h.Value().(*cancelContext).cancel()
		h.Delete()
	}
	C.free(unsafe.Pointer(cctx))
}

//export pelicanc_get
func pelicanc_get(cctx *C.struct_pelican_context, remoteUrl, localPath *C.char, copts *C.pelican_transfer_opts, cresults **C.pelican_result, nresults *C.size_t) *C.pelican_error {
	return guard(func() *C.pelican_error {
		if err := requireInit(); err != nil {
			return makeError(err)
		}
		opts, recursive := buildOptions(copts)
		remote := C.GoString(remoteUrl)
		results, err := client.DoGet(goCtxFor(cctx), remote, C.GoString(localPath), recursive, opts...)
		fillResults(results, remote, cresults, nresults)
		return makeError(err)
	})
}

//export pelicanc_put
func pelicanc_put(cctx *C.struct_pelican_context, localPath, remoteUrl *C.char, copts *C.pelican_transfer_opts, cresults **C.pelican_result, nresults *C.size_t) *C.pelican_error {
	return guard(func() *C.pelican_error {
		if err := requireInit(); err != nil {
			return makeError(err)
		}
		opts, recursive := buildOptions(copts)
		remote := C.GoString(remoteUrl)
		results, err := client.DoPut(goCtxFor(cctx), C.GoString(localPath), remote, recursive, opts...)
		fillResults(results, remote, cresults, nresults)
		return makeError(err)
	})
}

func fillFileInfo(dst *C.pelican_file_info, src *client.FileInfo) {
	dst.name = C.CString(src.Name)
	dst.size = C.longlong(src.Size)
	dst.mtime = C.longlong(src.ModTime.Unix())
	if src.IsCollection {
		dst.is_collection = 1
	}
}

//export pelicanc_stat
func pelicanc_stat(cctx *C.struct_pelican_context, remoteUrl *C.char, copts *C.pelican_transfer_opts, cinfo **C.pelican_file_info) *C.pelican_error {
	return guard(func() *C.pelican_error {
		if err := requireInit(); err != nil {
			return makeError(err)
		}
		if cinfo == nil {
			return makeError(fmt.Errorf("output file info pointer may not be NULL"))
		}
		*cinfo = nil
		opts, _ := buildOptions(copts)
		info, err := client.DoStat(goCtxFor(cctx), C.GoString(remoteUrl), opts...)
		if err != nil {
			return makeError(err)
		}
		out := C.pelicanc_file_info_alloc(1)
		fillFileInfo(out, info)
		*cinfo = out
		return nil
	})
}

//export pelicanc_list
func pelicanc_list(cctx *C.struct_pelican_context, remoteUrl *C.char, copts *C.pelican_transfer_opts, cinfos **C.pelican_file_info, ninfos *C.size_t) *C.pelican_error {
	return guard(func() *C.pelican_error {
		if err := requireInit(); err != nil {
			return makeError(err)
		}
		if cinfos == nil || ninfos == nil {
			return makeError(fmt.Errorf("output file info pointers may not be NULL"))
		}
		*cinfos = nil
		*ninfos = 0
		opts, _ := buildOptions(copts)
		infos, err := client.DoList(goCtxFor(cctx), C.GoString(remoteUrl), opts...)
		if err != nil {
			return makeError(err)
		}
		if len(infos) > 0 {
			arr := C.pelicanc_file_info_alloc(C.size_t(len(infos)))
			slice := unsafe.Slice(arr, len(infos))
			for i := range infos {
				fillFileInfo(&slice[i], &infos[i])
			}
			*cinfos = arr
			*ninfos = C.size_t(len(infos))
		}
		return nil
	})
}

//export pelicanc_delete
func pelicanc_delete(cctx *C.struct_pelican_context, remoteUrl *C.char, copts *C.pelican_transfer_opts) *C.pelican_error {
	return guard(func() *C.pelican_error {
		if err := requireInit(); err != nil {
			return makeError(err)
		}
		opts, recursive := buildOptions(copts)
		return makeError(client.DoDelete(goCtxFor(cctx), C.GoString(remoteUrl), recursive, opts...))
	})
}

// main is required for -buildmode=c-shared; it is never executed.
func main() {}
