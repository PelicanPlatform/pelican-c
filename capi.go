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
// const-correct entry points declared in include/pelican/client.h are
// defined in bridge.c — pure C for accessors and deallocation, thin
// forwarders into this file for everything else.  Keep the three layers
// in sync: client.h (public ABI), bridge.c (wrappers), this file.
package main

/*
#cgo CFLAGS: -I${SRCDIR}/include
#include <stdlib.h>
#include "bridge.h"
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"runtime/cgo"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"github.com/pelicanplatform/pelican/client"
	"github.com/pelicanplatform/pelican/config"
	"github.com/pelicanplatform/pelican/error_codes"
	"github.com/spf13/viper"
)

var (
	initOnce    sync.Once
	initialized atomic.Bool
	versionOnce sync.Once
	versionCStr *C.char
	engineOnce  sync.Once
	engine      *client.TransferEngine
	engineErr   error
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
	var pe *error_codes.PelicanError
	if errors.As(err, &pe) {
		ce.code = C.int(pe.Code())
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

// getEngine lazily starts the shared TransferEngine backing the
// asynchronous API.  It lives for the remainder of the process.
func getEngine() (*client.TransferEngine, error) {
	engineOnce.Do(func() {
		engine, engineErr = client.NewTransferEngine(context.Background())
	})
	return engine, engineErr
}

// goCtxFor resolves the optional C cancellation handle to a Go context.
func goCtxFor(cctx *C.pelican_context) context.Context {
	if cctx == nil || cctx.handle == 0 {
		return context.Background()
	}
	return cgo.Handle(cctx.handle).Value().(*cancelContext).ctx
}

type cancelContext struct {
	ctx    context.Context
	cancel context.CancelFunc
}

// progressSink redirects progress reports away from direct C-callback
// invocation.  The asynchronous API uses it to queue events for delivery
// on the host's own thread — hosts like HTCondor DaemonCore are
// aggressively thread-unsafe, so the library must never invoke their
// callbacks from a Go-owned thread in that mode.
type progressSink func(path string, downloaded, total int64, completed bool)

// invokeProgress calls the C progress callback on the current thread.
func invokeProgress(fn C.pelican_progress_fn, userData unsafe.Pointer, path string, downloaded, total int64, completed bool) {
	cPath := C.CString(path)
	cCompleted := C.int(0)
	if completed {
		cCompleted = 1
	}
	C.pelicanc_invoke_progress(fn, cPath, C.longlong(downloaded), C.longlong(total), cCompleted, userData)
	C.free(unsafe.Pointer(cPath))
}

// buildOptions translates the C options struct into client.TransferOption
// values.  The library always runs non-interactively: it must never block
// the host process on an OAuth device-flow prompt.  If sink is non-nil,
// progress reports are handed to it instead of invoking the C callback
// from a library thread.
func buildOptions(copts *C.pelican_transfer_opts, sink progressSink) (opts []client.TransferOption, recursive bool) {
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
		cb := func(path string, downloaded int64, totalSize int64, completed bool) {
			invokeProgress(fn, userData, path, downloaded, totalSize, completed)
		}
		if sink != nil {
			cb = sink
		}
		opts = append(opts, client.WithCallback(cb))
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

// makeCResult converts one per-object transfer result.  fallbackSource
// covers download results, which (unlike the error path) are produced
// without TransferResults.Source populated.
func makeCResult(r *client.TransferResults, fallbackSource string) *C.pelican_result {
	res := C.pelicanc_result_alloc()
	source := r.Source
	if source == "" {
		source = fallbackSource
	}
	res.source = C.CString(source)
	res.transferred_bytes = C.longlong(r.TransferredBytes)
	res.attempts = C.int(len(r.Attempts))
	if n := len(r.Attempts); n > 0 {
		last := r.Attempts[n-1]
		res.endpoint = C.CString(last.Endpoint)
		res.transfer_time_s = C.double(last.TransferTime.Seconds())
	}
	res.error = makeError(r.Error)
	return res
}

func makeResultList(results []client.TransferResults, fallbackSource string) *C.pelican_result_list {
	list := C.pelicanc_result_list_alloc(C.size_t(len(results)))
	if len(results) > 0 {
		items := unsafe.Slice(list.items, len(results))
		for i := range results {
			items[i] = makeCResult(&results[i], fallbackSource)
		}
		list.count = C.size_t(len(results))
	}
	return list
}

/* ------------------------------------------------------------------ *
 * Library setup                                                      *
 * ------------------------------------------------------------------ */

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

/* ------------------------------------------------------------------ *
 * Cancellation contexts                                              *
 * ------------------------------------------------------------------ */

//export pelicanc_context_new
func pelicanc_context_new() *C.pelican_context {
	cctx := C.pelicanc_context_alloc()
	ctx, cancel := context.WithCancel(context.Background())
	cctx.handle = C.uintptr_t(cgo.NewHandle(&cancelContext{ctx: ctx, cancel: cancel}))
	return cctx
}

//export pelicanc_context_cancel
func pelicanc_context_cancel(cctx *C.pelican_context) {
	if cctx == nil || cctx.handle == 0 {
		return
	}
	cgo.Handle(cctx.handle).Value().(*cancelContext).cancel()
}

//export pelicanc_context_free
func pelicanc_context_free(cctx *C.pelican_context) {
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

/* ------------------------------------------------------------------ *
 * Synchronous transfers                                              *
 * ------------------------------------------------------------------ */

//export pelicanc_get
func pelicanc_get(cctx *C.pelican_context, remoteUrl, localPath *C.char, copts *C.pelican_transfer_opts, listOut **C.pelican_result_list) *C.pelican_error {
	return guard(func() *C.pelican_error {
		if err := requireInit(); err != nil {
			return makeError(err)
		}
		opts, recursive := buildOptions(copts, nil)
		remote := C.GoString(remoteUrl)
		results, err := client.DoGet(goCtxFor(cctx), remote, C.GoString(localPath), recursive, opts...)
		if listOut != nil {
			*listOut = makeResultList(results, remote)
		}
		return makeError(err)
	})
}

//export pelicanc_put
func pelicanc_put(cctx *C.pelican_context, localPath, remoteUrl *C.char, copts *C.pelican_transfer_opts, listOut **C.pelican_result_list) *C.pelican_error {
	return guard(func() *C.pelican_error {
		if err := requireInit(); err != nil {
			return makeError(err)
		}
		opts, recursive := buildOptions(copts, nil)
		remote := C.GoString(remoteUrl)
		results, err := client.DoPut(goCtxFor(cctx), C.GoString(localPath), remote, recursive, opts...)
		if listOut != nil {
			*listOut = makeResultList(results, remote)
		}
		return makeError(err)
	})
}

/* ------------------------------------------------------------------ *
 * Asynchronous transfers                                             *
 * ------------------------------------------------------------------ */

// transferState is the Go side of a pelican_transfer.  Completed results
// queue here; a byte written to the self-pipe wakes the host's event
// loop.  The pipe is drained only when the queue is observed empty, so
// readability is level-triggered: readable ⇒ call next_result until it
// reports nothing pending.
type transferState struct {
	mu      sync.Mutex
	queue   []*C.pelican_result
	done    bool
	freed   bool
	termErr *C.pelican_error
	readFd  int
	writeFd int
	cancel  context.CancelFunc
	tc      *client.TransferClient

	// Progress callbacks are never invoked from library threads in
	// async mode: events queue here (coalesced per object, since only
	// the latest state matters) and are delivered on the host's thread
	// from within pelican_transfer_next_result.
	progressFn   C.pelican_progress_fn
	progressData unsafe.Pointer
	progressQ    []progressEvent
	progressIdx  map[string]int
}

type progressEvent struct {
	path       string
	downloaded int64
	total      int64
	completed  bool
}

// wake writes one byte to the notification pipe.  Callers hold mu.  A
// full pipe returns EAGAIN, which is fine — the fd is already readable.
func (ts *transferState) wake() {
	_, _ = syscall.Write(ts.writeFd, []byte{1})
}

// drainPipe empties the notification pipe.  Callers hold mu and have
// observed an empty queue; any concurrent enqueue re-arms the pipe
// after we release the lock.
func (ts *transferState) drainPipe() {
	buf := make([]byte, 256)
	for {
		n, err := syscall.Read(ts.readFd, buf)
		if n <= 0 || err != nil {
			return
		}
	}
}

// enqueueProgress records a progress report (from a Go thread) for later
// delivery on the host's thread, coalescing by object so an unattended
// queue stays bounded.
func (ts *transferState) enqueueProgress(path string, downloaded, total int64, completed bool) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.freed {
		return
	}
	ev := progressEvent{path: path, downloaded: downloaded, total: total, completed: completed}
	if ts.progressIdx == nil {
		ts.progressIdx = make(map[string]int)
	}
	if i, ok := ts.progressIdx[path]; ok {
		ts.progressQ[i] = ev
	} else {
		ts.progressIdx[path] = len(ts.progressQ)
		ts.progressQ = append(ts.progressQ, ev)
	}
	ts.wake()
}

// deliverProgress invokes any queued progress callbacks on the calling
// (host) thread.  The mutex is released during the invocations, so a
// callback may safely call back into the library.
func (ts *transferState) deliverProgress() {
	for {
		ts.mu.Lock()
		if ts.freed || len(ts.progressQ) == 0 {
			ts.mu.Unlock()
			return
		}
		events := ts.progressQ
		ts.progressQ = nil
		ts.progressIdx = nil
		fn, userData := ts.progressFn, ts.progressData
		ts.mu.Unlock()
		for _, e := range events {
			invokeProgress(fn, userData, e.path, e.downloaded, e.total, e.completed)
		}
	}
}

func (ts *transferState) enqueue(res *C.pelican_result) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.freed {
		C.pelican_result_free(res)
		return
	}
	ts.queue = append(ts.queue, res)
	ts.wake()
}

func (ts *transferState) finish(err error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.freed {
		return
	}
	ts.done = true
	ts.termErr = makeError(err)
	ts.wake()
}

func transferStateFor(cxfer *C.pelican_transfer) *transferState {
	return cgo.Handle(cxfer.handle).Value().(*transferState)
}

// runTransfer is the goroutine driving one asynchronous transfer,
// streaming per-object results into the state as the engine produces
// them.
func runTransfer(ctx context.Context, ts *transferState, te *client.TransferEngine, remote, local string, upload, recursive bool, opts []client.TransferOption) {
	defer func() {
		if r := recover(); r != nil {
			ts.finish(fmt.Errorf("panic in pelican transfer: %v", r))
		}
	}()
	pUrl, err := client.ParseRemoteAsPUrl(ctx, remote)
	if err != nil {
		ts.finish(err)
		return
	}
	tc, err := te.NewClient(opts...)
	if err != nil {
		ts.finish(err)
		return
	}
	ts.mu.Lock()
	if ts.freed {
		ts.mu.Unlock()
		tc.Cancel()
		return
	}
	ts.tc = tc
	ts.mu.Unlock()
	tj, err := tc.NewTransferJob(ctx, pUrl.GetRawUrl(), local, upload, recursive, opts...)
	if err != nil {
		tc.Close()
		ts.finish(err)
		return
	}
	if err := tc.Submit(tj); err != nil {
		tc.Close()
		ts.finish(err)
		return
	}
	tc.Close()
	for r := range tc.Results() {
		ts.enqueue(makeCResult(&r, remote))
	}
	_, lookupErr := tj.GetLookupStatus()
	if lookupErr == nil {
		lookupErr = ctx.Err()
	}
	ts.finish(lookupErr)
}

func startTransfer(remote, local string, upload bool, copts *C.pelican_transfer_opts, out **C.pelican_transfer) *C.pelican_error {
	return guard(func() *C.pelican_error {
		if err := requireInit(); err != nil {
			return makeError(err)
		}
		if out == nil {
			return makeError(fmt.Errorf("output transfer pointer may not be NULL"))
		}
		*out = nil
		te, err := getEngine()
		if err != nil {
			return makeError(fmt.Errorf("failed to start transfer engine: %w", err))
		}
		var fds [2]int
		if err := syscall.Pipe(fds[:]); err != nil {
			return makeError(fmt.Errorf("failed to create notification pipe: %w", err))
		}
		for _, fd := range fds {
			_ = syscall.SetNonblock(fd, true)
			syscall.CloseOnExec(fd)
		}
		ctx, cancel := context.WithCancel(context.Background())
		ts := &transferState{readFd: fds[0], writeFd: fds[1], cancel: cancel}
		var sink progressSink
		if copts != nil && copts.progress != nil {
			ts.progressFn = copts.progress
			ts.progressData = copts.progress_data
			sink = ts.enqueueProgress
		}
		cxfer := C.pelicanc_transfer_alloc()
		cxfer.handle = C.uintptr_t(cgo.NewHandle(ts))
		cxfer.notify_fd = C.int(fds[0])
		opts, recursive := buildOptions(copts, sink)
		go runTransfer(ctx, ts, te, remote, local, upload, recursive, opts)
		*out = cxfer
		return nil
	})
}

//export pelicanc_get_start
func pelicanc_get_start(remoteUrl, localPath *C.char, copts *C.pelican_transfer_opts, out **C.pelican_transfer) *C.pelican_error {
	return startTransfer(C.GoString(remoteUrl), C.GoString(localPath), false, copts, out)
}

//export pelicanc_put_start
func pelicanc_put_start(localPath, remoteUrl *C.char, copts *C.pelican_transfer_opts, out **C.pelican_transfer) *C.pelican_error {
	return startTransfer(C.GoString(remoteUrl), C.GoString(localPath), true, copts, out)
}

//export pelicanc_transfer_next_result
func pelicanc_transfer_next_result(cxfer *C.pelican_transfer, res **C.pelican_result) C.int {
	ts := transferStateFor(cxfer)
	// Deliver pending progress callbacks on this (the host's) thread
	// before reporting results.
	ts.deliverProgress()
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if len(ts.queue) > 0 {
		popped := ts.queue[0]
		ts.queue = ts.queue[1:]
		if res != nil {
			*res = popped
		} else {
			C.pelican_result_free(popped)
		}
		return 1
	}
	// Nothing pending: quiesce the notification fd.  A result or the
	// completion event arriving after this drain re-arms it, because
	// producers write their wake byte under the same lock.
	ts.drainPipe()
	return 0
}

//export pelicanc_transfer_is_done
func pelicanc_transfer_is_done(cxfer *C.pelican_transfer) C.int {
	ts := transferStateFor(cxfer)
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.done {
		return 1
	}
	return 0
}

//export pelicanc_transfer_error
func pelicanc_transfer_error(cxfer *C.pelican_transfer) *C.pelican_error {
	ts := transferStateFor(cxfer)
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.termErr
}

//export pelicanc_transfer_cancel
func pelicanc_transfer_cancel(cxfer *C.pelican_transfer) {
	if cxfer == nil || cxfer.handle == 0 {
		return
	}
	ts := transferStateFor(cxfer)
	ts.mu.Lock()
	cancel, tc := ts.cancel, ts.tc
	ts.mu.Unlock()
	cancel()
	if tc != nil {
		tc.Cancel()
	}
}

//export pelicanc_transfer_free
func pelicanc_transfer_free(cxfer *C.pelican_transfer) {
	if cxfer == nil {
		return
	}
	if cxfer.handle != 0 {
		pelicanc_transfer_cancel(cxfer)
		h := cgo.Handle(cxfer.handle)
		ts := h.Value().(*transferState)
		ts.mu.Lock()
		ts.freed = true
		for _, r := range ts.queue {
			C.pelican_result_free(r)
		}
		ts.queue = nil
		C.pelican_error_free(ts.termErr)
		ts.termErr = nil
		_ = syscall.Close(ts.readFd)
		_ = syscall.Close(ts.writeFd)
		ts.mu.Unlock()
		h.Delete()
	}
	C.free(unsafe.Pointer(cxfer))
}

/* ------------------------------------------------------------------ *
 * Namespace operations                                               *
 * ------------------------------------------------------------------ */

func fillFileInfo(dst *C.pelican_file_info, name string, size int64, mtime int64, isCollection bool) {
	dst.name = C.CString(name)
	dst.size = C.longlong(size)
	dst.mtime = C.longlong(mtime)
	if isCollection {
		dst.is_collection = 1
	}
}

//export pelicanc_stat
func pelicanc_stat(cctx *C.pelican_context, remoteUrl *C.char, copts *C.pelican_transfer_opts, cinfo **C.pelican_file_info) *C.pelican_error {
	return guard(func() *C.pelican_error {
		if err := requireInit(); err != nil {
			return makeError(err)
		}
		if cinfo == nil {
			return makeError(fmt.Errorf("output file info pointer may not be NULL"))
		}
		*cinfo = nil
		opts, _ := buildOptions(copts, nil)
		info, err := client.DoStat(goCtxFor(cctx), C.GoString(remoteUrl), opts...)
		if err != nil {
			return makeError(err)
		}
		out := C.pelicanc_file_info_alloc()
		fillFileInfo(out, info.Name, info.Size, info.ModTime.Unix(), info.IsCollection)
		*cinfo = out
		return nil
	})
}

//export pelicanc_list
func pelicanc_list(cctx *C.pelican_context, remoteUrl *C.char, copts *C.pelican_transfer_opts, listOut **C.pelican_file_info_list) *C.pelican_error {
	return guard(func() *C.pelican_error {
		if err := requireInit(); err != nil {
			return makeError(err)
		}
		if listOut == nil {
			return makeError(fmt.Errorf("output list pointer may not be NULL"))
		}
		*listOut = nil
		opts, _ := buildOptions(copts, nil)
		infos, err := client.DoList(goCtxFor(cctx), C.GoString(remoteUrl), opts...)
		if err != nil {
			return makeError(err)
		}
		list := C.pelicanc_file_info_list_alloc(C.size_t(len(infos)))
		if len(infos) > 0 {
			items := unsafe.Slice(list.items, len(infos))
			for i := range infos {
				items[i] = C.pelicanc_file_info_alloc()
				fillFileInfo(items[i], infos[i].Name, infos[i].Size, infos[i].ModTime.Unix(), infos[i].IsCollection)
			}
			list.count = C.size_t(len(infos))
		}
		*listOut = list
		return nil
	})
}

//export pelicanc_delete
func pelicanc_delete(cctx *C.pelican_context, remoteUrl *C.char, copts *C.pelican_transfer_opts) *C.pelican_error {
	return guard(func() *C.pelican_error {
		if err := requireInit(); err != nil {
			return makeError(err)
		}
		opts, recursive := buildOptions(copts, nil)
		return makeError(client.DoDelete(goCtxFor(cctx), C.GoString(remoteUrl), recursive, opts...))
	})
}

/* ------------------------------------------------------------------ *
 * File I/O (PelicanFS)                                               *
 * ------------------------------------------------------------------ */

// fileState is the Go side of a pelican_file.  Each open file owns a
// PelicanFS instance (and thus a TransferEngine); PelicanFS exposes no
// shutdown, so the context is cancelled on close to reap the engine's
// goroutines.
type fileState struct {
	file   fs.File
	cancel context.CancelFunc
}

func fileStateFor(cfile *C.pelican_file) *fileState {
	return cgo.Handle(cfile.handle).Value().(*fileState)
}

func translateOpenFlags(flags C.int) (int, error) {
	goFlags := 0
	switch flags & 3 {
	case C.PELICAN_O_RDONLY:
		goFlags = os.O_RDONLY
	case C.PELICAN_O_WRONLY:
		goFlags = os.O_WRONLY
	case C.PELICAN_O_RDWR:
		goFlags = os.O_RDWR
	default:
		return 0, fmt.Errorf("invalid access mode in open flags %#x", int(flags))
	}
	remaining := flags &^ 3
	if remaining&C.PELICAN_O_CREATE != 0 {
		goFlags |= os.O_CREATE
		remaining &^= C.PELICAN_O_CREATE
	}
	if remaining != 0 {
		return 0, fmt.Errorf("unsupported open flags %#x", int(flags))
	}
	return goFlags, nil
}

//export pelicanc_fs_open
func pelicanc_fs_open(remoteUrl *C.char, flags C.int, copts *C.pelican_transfer_opts, out **C.pelican_file) *C.pelican_error {
	return guard(func() *C.pelican_error {
		if err := requireInit(); err != nil {
			return makeError(err)
		}
		if out == nil {
			return makeError(fmt.Errorf("output file pointer may not be NULL"))
		}
		*out = nil
		goFlags, err := translateOpenFlags(flags)
		if err != nil {
			return makeError(err)
		}
		u, err := url.Parse(C.GoString(remoteUrl))
		if err != nil {
			return makeError(fmt.Errorf("failed to parse remote URL: %w", err))
		}
		if u.Scheme == "" || u.Host == "" {
			return makeError(fmt.Errorf("remote URL %q must include a scheme and federation host (e.g. pelican://federation/path)", u.String()))
		}
		opts, _ := buildOptions(copts, nil)
		ctx, cancel := context.WithCancel(context.Background())
		pfs := client.NewPelicanFSWithPrefix(ctx, u.Scheme+"://"+u.Host, opts...)
		f, err := pfs.OpenFile(u.Path, goFlags)
		if err != nil {
			cancel()
			return makeError(err)
		}
		cfile := C.pelicanc_file_alloc()
		cfile.handle = C.uintptr_t(cgo.NewHandle(&fileState{file: f, cancel: cancel}))
		*out = cfile
		return nil
	})
}

// ioGuard wraps the read/write/seek-style exports: panics become errors,
// and the (result, error) pair is mapped to the C convention of
// count-or-negative-one with an optional error out-parameter.
func ioGuard(errOut **C.pelican_error, fn func() (int64, error)) (ret C.longlong) {
	if errOut != nil {
		*errOut = nil
	}
	var n int64
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("panic in pelican client: %v", r)
			}
		}()
		n, err = fn()
	}()
	if err != nil {
		if errOut != nil {
			*errOut = makeError(err)
		}
		return -1
	}
	return C.longlong(n)
}

//export pelicanc_file_read
func pelicanc_file_read(cfile *C.pelican_file, buf unsafe.Pointer, length C.size_t, errOut **C.pelican_error) C.longlong {
	return ioGuard(errOut, func() (int64, error) {
		st := fileStateFor(cfile)
		n, err := st.file.Read(unsafe.Slice((*byte)(buf), int(length)))
		if err == io.EOF && n == 0 {
			return 0, nil
		}
		if n > 0 {
			return int64(n), nil
		}
		return int64(n), err
	})
}

//export pelicanc_file_pread
func pelicanc_file_pread(cfile *C.pelican_file, buf unsafe.Pointer, length C.size_t, offset C.longlong, errOut **C.pelican_error) C.longlong {
	return ioGuard(errOut, func() (int64, error) {
		st := fileStateFor(cfile)
		ra, ok := st.file.(io.ReaderAt)
		if !ok {
			return 0, fmt.Errorf("positional reads are not supported for this file")
		}
		n, err := ra.ReadAt(unsafe.Slice((*byte)(buf), int(length)), int64(offset))
		if err == io.EOF && n >= 0 {
			return int64(n), nil
		}
		if n > 0 {
			return int64(n), nil
		}
		return int64(n), err
	})
}

//export pelicanc_file_write
func pelicanc_file_write(cfile *C.pelican_file, buf unsafe.Pointer, length C.size_t, errOut **C.pelican_error) C.longlong {
	return ioGuard(errOut, func() (int64, error) {
		st := fileStateFor(cfile)
		w, ok := st.file.(io.Writer)
		if !ok {
			return 0, fmt.Errorf("file is not open for writing")
		}
		n, err := w.Write(unsafe.Slice((*byte)(buf), int(length)))
		if err != nil {
			return int64(n), err
		}
		return int64(n), nil
	})
}

//export pelicanc_file_seek
func pelicanc_file_seek(cfile *C.pelican_file, offset C.longlong, whence C.int, errOut **C.pelican_error) C.longlong {
	return ioGuard(errOut, func() (int64, error) {
		st := fileStateFor(cfile)
		s, ok := st.file.(io.Seeker)
		if !ok {
			return 0, fmt.Errorf("seeking is not supported for this file")
		}
		if whence < 0 || whence > 2 {
			return 0, fmt.Errorf("invalid whence %d", int(whence))
		}
		return s.Seek(int64(offset), int(whence))
	})
}

//export pelicanc_file_stat
func pelicanc_file_stat(cfile *C.pelican_file, cinfo **C.pelican_file_info) *C.pelican_error {
	return guard(func() *C.pelican_error {
		if cinfo == nil {
			return makeError(fmt.Errorf("output file info pointer may not be NULL"))
		}
		*cinfo = nil
		st := fileStateFor(cfile)
		info, err := st.file.Stat()
		if err != nil {
			return makeError(err)
		}
		out := C.pelicanc_file_info_alloc()
		fillFileInfo(out, info.Name(), info.Size(), info.ModTime().Unix(), info.IsDir())
		*cinfo = out
		return nil
	})
}

//export pelicanc_file_close
func pelicanc_file_close(cfile *C.pelican_file) *C.pelican_error {
	if cfile == nil {
		return nil
	}
	var err error
	if cfile.handle != 0 {
		h := cgo.Handle(cfile.handle)
		st := h.Value().(*fileState)
		func() {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("panic in pelican client: %v", r)
				}
			}()
			err = st.file.Close()
		}()
		st.cancel()
		h.Delete()
	}
	C.free(unsafe.Pointer(cfile))
	return makeError(err)
}

// main is required for -buildmode=c-shared; it is never executed.
func main() {}
