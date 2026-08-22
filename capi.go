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
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"runtime/cgo"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"github.com/pelicanplatform/pelican/client"
	"github.com/pelicanplatform/pelican/config"
	"github.com/pelicanplatform/pelican/error_codes"
	log "github.com/sirupsen/logrus"
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
// values, validating values whose setters (being plain C) could not.
// The library always runs non-interactively: it must never block the
// host process on an OAuth device-flow prompt.  If sink is non-nil,
// progress reports are handed to it instead of invoking the C callback
// from a library thread.
func buildOptions(copts *C.pelican_transfer_opts, sink progressSink) (opts []client.TransferOption, recursive bool, err error) {
	opts = append(opts, client.WithNonInteractive(true))
	if copts == nil {
		return
	}
	if copts.n_caches > 0 {
		names := unsafe.Slice(copts.caches, copts.n_caches)
		caches := make([]*url.URL, 0, len(names))
		for _, name := range names {
			cacheStr := C.GoString(name)
			cacheUrl, parseErr := url.Parse(cacheStr)
			if parseErr != nil {
				return nil, false, fmt.Errorf("invalid preferred cache URL %q: %w", cacheStr, parseErr)
			}
			caches = append(caches, cacheUrl)
		}
		opts = append(opts, client.WithCaches(caches...))
	}
	if copts.n_checksum_requests > 0 {
		names := unsafe.Slice(copts.checksum_requests, copts.n_checksum_requests)
		types := make([]client.ChecksumType, 0, len(names))
		for _, name := range names {
			digest := C.GoString(name)
			ctype := client.ChecksumFromHttpDigest(digest)
			if ctype == client.AlgUnknown {
				return nil, false, fmt.Errorf("unknown checksum digest %q; known digests: %v", digest, client.KnownChecksumTypesAsHttpDigest())
			}
			types = append(types, ctype)
		}
		opts = append(opts, client.WithRequestChecksums(types))
	}
	if copts.require_checksum != 0 {
		opts = append(opts, client.WithRequireChecksum())
	}
	if copts.token != nil {
		opts = append(opts, client.WithToken(C.GoString(copts.token)))
	}
	if copts.token_location != nil {
		opts = append(opts, client.WithTokenLocation(C.GoString(copts.token_location)))
	}
	if copts.source_token != nil {
		opts = append(opts, client.WithSourceToken(C.GoString(copts.source_token)))
	}
	if copts.source_token_location != nil {
		opts = append(opts, client.WithSourceTokenLocation(C.GoString(copts.source_token_location)))
	}
	if copts.dest_token != nil {
		opts = append(opts, client.WithDestinationToken(C.GoString(copts.dest_token)))
	}
	if copts.dest_token_location != nil {
		opts = append(opts, client.WithDestinationTokenLocation(C.GoString(copts.dest_token_location)))
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
	if r.ETag != "" {
		res.etag = C.CString(r.ETag)
	}
	checksums := r.ServerChecksums
	if len(checksums) == 0 {
		checksums = r.ClientChecksums
	}
	if len(checksums) > 0 {
		arr := C.pelicanc_checksum_alloc(C.size_t(len(checksums)))
		slice := unsafe.Slice(arr, len(checksums))
		for i, ck := range checksums {
			slice[i]._type = C.CString(client.HttpDigestFromChecksum(ck.Algorithm))
			slice[i].value = C.CString(hex.EncodeToString(ck.Value))
		}
		res.checksums = arr
		res.n_checksums = C.size_t(len(checksums))
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
 * Logging                                                            *
 * ------------------------------------------------------------------ */

// maxQueuedLogs bounds the queue used by PELICAN_LOG_QUEUED delivery.  A
// host that stops pumping (or a sudden log storm) must not be able to
// grow memory without limit, so the oldest records are dropped and
// counted instead.
const maxQueuedLogs = 8192

type logRecord struct {
	level   C.int
	message string
}

// logState holds the host's log callback and the queue feeding it.
type logState struct {
	mu       sync.Mutex
	fn       C.pelican_log_fn
	userData unsafe.Pointer
	direct   bool
	queue    []logRecord
	dropped  uint64
	pipe     notifyPipe
	hasPipe  bool
}

var logs logState

// hookOnce guards installation of the logrus hook: it stays registered
// for the process lifetime, and swapping callbacks only retargets it.
var hookOnce sync.Once

// levelFor maps a logrus level onto the public enum.  Panic, fatal, and
// error all surface as PELICAN_LOG_ERROR: a host cares that the record
// is an error, and the library never actually exits the process.
func levelFor(level log.Level) C.int {
	switch level {
	case log.PanicLevel, log.FatalLevel, log.ErrorLevel:
		return C.PELICAN_LOG_ERROR
	case log.WarnLevel:
		return C.PELICAN_LOG_WARNING
	case log.InfoLevel:
		return C.PELICAN_LOG_INFO
	case log.DebugLevel:
		return C.PELICAN_LOG_DEBUG
	default:
		return C.PELICAN_LOG_TRACE
	}
}

// logrusLevelFor maps the public enum onto a logrus level.
func logrusLevelFor(level C.int) (log.Level, error) {
	switch level {
	case C.PELICAN_LOG_ERROR:
		return log.ErrorLevel, nil
	case C.PELICAN_LOG_WARNING:
		return log.WarnLevel, nil
	case C.PELICAN_LOG_INFO:
		return log.InfoLevel, nil
	case C.PELICAN_LOG_DEBUG:
		return log.DebugLevel, nil
	case C.PELICAN_LOG_TRACE:
		return log.TraceLevel, nil
	}
	return log.InfoLevel, fmt.Errorf("invalid log level %d", int(level))
}

// hostLogHook forwards logrus records to the host's callback.  logrus
// fires hooks without holding its own lock, so this may take the log
// state mutex safely; it must not log, which would recurse.
type hostLogHook struct{}

func (hostLogHook) Levels() []log.Level { return log.AllLevels }

func (hostLogHook) Fire(entry *log.Entry) error {
	var sb strings.Builder
	sb.WriteString(entry.Message)
	if len(entry.Data) > 0 {
		keys := make([]string, 0, len(entry.Data))
		for k := range entry.Data {
			keys = append(keys, k)
		}
		// Sorted so a record's rendering is stable run to run.
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&sb, " %s=%v", k, entry.Data[k])
		}
	}
	logs.emit(levelFor(entry.Level), sb.String())
	return nil
}

// emit delivers a record directly or queues it, depending on the mode
// the host selected.
func (ls *logState) emit(level C.int, message string) {
	ls.mu.Lock()
	if ls.fn == nil {
		ls.mu.Unlock()
		return
	}
	if ls.direct {
		fn, userData := ls.fn, ls.userData
		ls.mu.Unlock()
		invokeLog(fn, userData, level, message)
		return
	}
	if len(ls.queue) >= maxQueuedLogs {
		// Drop the oldest: under a storm the most recent records are the
		// ones a host needs to see.
		ls.queue = ls.queue[1:]
		ls.dropped++
	}
	ls.queue = append(ls.queue, logRecord{level: level, message: message})
	if ls.hasPipe {
		ls.pipe.wake()
	}
	ls.mu.Unlock()
}

// invokeLog calls the host's callback on the current thread.
func invokeLog(fn C.pelican_log_fn, userData unsafe.Pointer, level C.int, message string) {
	cMsg := C.CString(message)
	C.pelicanc_invoke_log(fn, level, cMsg, userData)
	C.free(unsafe.Pointer(cMsg))
}

//export pelicanc_log_set_callback
func pelicanc_log_set_callback(fn C.pelican_log_fn, userData unsafe.Pointer, delivery C.int) *C.pelican_error {
	return guard(func() *C.pelican_error {
		if delivery != C.PELICAN_LOG_QUEUED && delivery != C.PELICAN_LOG_DIRECT {
			return makeError(fmt.Errorf("invalid log delivery mode %d", int(delivery)))
		}
		logs.mu.Lock()
		logs.fn = fn
		logs.userData = userData
		logs.direct = delivery == C.PELICAN_LOG_DIRECT
		if fn == nil {
			// Uninstalled: drop anything queued, since nothing will
			// collect it.
			logs.queue = nil
			logs.dropped = 0
		} else if !logs.direct && !logs.hasPipe {
			pipe, err := newNotifyPipe()
			if err != nil {
				logs.mu.Unlock()
				return makeError(err)
			}
			logs.pipe = pipe
			logs.hasPipe = true
		}
		logs.mu.Unlock()

		if fn == nil {
			// Restore the library's own output.
			log.SetOutput(os.Stderr)
			return nil
		}
		hookOnce.Do(func() { log.AddHook(hostLogHook{}) })
		// The host owns log output now; writing to stderr (or a
		// configured log file) as well would duplicate every record.
		log.SetOutput(io.Discard)
		return nil
	})
}

//export pelicanc_log_notify_fd
func pelicanc_log_notify_fd() C.int {
	logs.mu.Lock()
	defer logs.mu.Unlock()
	if !logs.hasPipe || logs.fn == nil || logs.direct {
		return -1
	}
	return C.int(logs.pipe.readFd)
}

//export pelicanc_log_pump
func pelicanc_log_pump() C.size_t {
	var delivered C.size_t
	for {
		logs.mu.Lock()
		if logs.fn == nil || logs.direct || len(logs.queue) == 0 {
			// Quiesce the notification fd now that nothing is pending.
			if logs.hasPipe {
				logs.pipe.drain()
			}
			logs.mu.Unlock()
			return delivered
		}
		batch := logs.queue
		logs.queue = nil
		dropped := logs.dropped
		logs.dropped = 0
		fn, userData := logs.fn, logs.userData
		logs.mu.Unlock()

		// Invoked with the lock released, so a callback may log or call
		// back into the library without deadlocking.
		if dropped > 0 {
			invokeLog(fn, userData, C.PELICAN_LOG_WARNING,
				fmt.Sprintf("pelican-c: dropped %d log record(s); the log queue overflowed", dropped))
			delivered++
		}
		for _, rec := range batch {
			invokeLog(fn, userData, rec.level, rec.message)
			delivered++
		}
	}
}

//export pelicanc_log_set_level
func pelicanc_log_set_level(level C.int) *C.pelican_error {
	return guard(func() *C.pelican_error {
		goLevel, err := logrusLevelFor(level)
		if err != nil {
			return makeError(err)
		}
		log.SetLevel(goLevel)
		return nil
	})
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
		opts, recursive, optErr := buildOptions(copts, nil)
		if optErr != nil {
			return makeError(optErr)
		}
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
		opts, recursive, optErr := buildOptions(copts, nil)
		if optErr != nil {
			return makeError(optErr)
		}
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
// notifyPipe is the wakeup mechanism shared by asynchronous transfers
// and operations: a non-blocking, close-on-exec pipe whose read end the
// host registers with its event loop.
//
// Discipline for every user: wake() is called only while holding the
// owning state's mutex and only after checking that the state has not
// been freed, so a byte can never be written to a closed (and possibly
// recycled) descriptor.
type notifyPipe struct {
	readFd  int
	writeFd int
}

func newNotifyPipe() (notifyPipe, error) {
	var fds [2]int
	if err := syscall.Pipe(fds[:]); err != nil {
		return notifyPipe{}, fmt.Errorf("failed to create notification pipe: %w", err)
	}
	for _, fd := range fds {
		_ = syscall.SetNonblock(fd, true)
		syscall.CloseOnExec(fd)
	}
	return notifyPipe{readFd: fds[0], writeFd: fds[1]}, nil
}

func (p notifyPipe) wake() {
	_, _ = syscall.Write(p.writeFd, []byte{1})
}

// drain empties the pipe, quiescing the host's event loop.  Callers hold
// the owning mutex and have observed that there is nothing left to
// report; a concurrent producer re-arms the pipe afterward.
func (p notifyPipe) drain() {
	buf := make([]byte, 256)
	for {
		n, err := syscall.Read(p.readFd, buf)
		if n <= 0 || err != nil {
			return
		}
	}
}

func (p notifyPipe) close() {
	_ = syscall.Close(p.readFd)
	_ = syscall.Close(p.writeFd)
}

type transferState struct {
	notifyPipe
	mu      sync.Mutex
	queue   []*C.pelican_result
	done    bool
	freed   bool
	termErr *C.pelican_error
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

// xferKind selects which engine job an async or engine-backed sync
// operation creates.
type xferKind int

const (
	kindGet xferKind = iota
	kindPut
	kindCopy
	kindPrestage
)

// makeJob builds the engine job for the given kind.  For get/put, a is
// the remote URL and b the local path; for copy, a is the source URL and
// b the destination URL; for prestage, a is the remote URL.
func makeJob(ctx context.Context, tc *client.TransferClient, kind xferKind, a, b string, recursive bool, opts []client.TransferOption) (*client.TransferJob, error) {
	switch kind {
	case kindGet, kindPut:
		pUrl, err := client.ParseRemoteAsPUrl(ctx, a)
		if err != nil {
			return nil, err
		}
		return tc.NewTransferJob(ctx, pUrl.GetRawUrl(), b, kind == kindPut, recursive, opts...)
	case kindCopy:
		srcUrl, err := url.Parse(a)
		if err != nil {
			return nil, fmt.Errorf("failed to parse copy source URL: %w", err)
		}
		destUrl, err := url.Parse(b)
		if err != nil {
			return nil, fmt.Errorf("failed to parse copy destination URL: %w", err)
		}
		return tc.NewCopyJob(ctx, srcUrl, destUrl, recursive, opts...)
	case kindPrestage:
		pUrl, err := client.ParseRemoteAsPUrl(ctx, a)
		if err != nil {
			return nil, err
		}
		return tc.NewPrestageJob(ctx, pUrl.GetRawUrl(), opts...)
	}
	return nil, fmt.Errorf("unknown transfer kind %d", kind)
}

// runTransfer is the goroutine driving one asynchronous transfer,
// streaming per-object results into the state as the engine produces
// them.
func runTransfer(ctx context.Context, ts *transferState, te *client.TransferEngine, kind xferKind, a, b string, recursive bool, opts []client.TransferOption) {
	defer func() {
		if r := recover(); r != nil {
			ts.finish(fmt.Errorf("panic in pelican transfer: %v", r))
		}
	}()
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
	tj, err := makeJob(ctx, tc, kind, a, b, recursive, opts)
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
		ts.enqueue(makeCResult(&r, a))
	}
	_, lookupErr := tj.GetLookupStatus()
	if lookupErr == nil {
		lookupErr = ctx.Err()
	}
	ts.finish(lookupErr)
}

// runSyncEngineJob runs one engine-backed job to completion, blocking the
// caller.  Backs the synchronous copy and prestage entry points, which
// have no client.Do* convenience wrapper.
func runSyncEngineJob(cctx *C.pelican_context, kind xferKind, a, b string, copts *C.pelican_transfer_opts, listOut **C.pelican_result_list) *C.pelican_error {
	return guard(func() *C.pelican_error {
		if err := requireInit(); err != nil {
			return makeError(err)
		}
		te, err := getEngine()
		if err != nil {
			return makeError(fmt.Errorf("failed to start transfer engine: %w", err))
		}
		opts, recursive, optErr := buildOptions(copts, nil)
		if optErr != nil {
			return makeError(optErr)
		}
		tc, err := te.NewClient(opts...)
		if err != nil {
			return makeError(err)
		}
		ctx := goCtxFor(cctx)
		tj, err := makeJob(ctx, tc, kind, a, b, recursive, opts)
		if err != nil {
			tc.Cancel()
			return makeError(err)
		}
		if err := tc.Submit(tj); err != nil {
			tc.Cancel()
			return makeError(err)
		}
		results, err := tc.Shutdown()
		if listOut != nil {
			*listOut = makeResultList(results, a)
		}
		if err == nil {
			_, err = tj.GetLookupStatus()
		}
		if err == nil {
			for i := range results {
				if results[i].Error != nil {
					err = results[i].Error
					break
				}
			}
		}
		return makeError(err)
	})
}

func startTransfer(kind xferKind, a, b string, copts *C.pelican_transfer_opts, out **C.pelican_transfer) *C.pelican_error {
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
		pipe, err := newNotifyPipe()
		if err != nil {
			return makeError(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		ts := &transferState{notifyPipe: pipe, cancel: cancel}
		var sink progressSink
		if copts != nil && copts.progress != nil {
			ts.progressFn = copts.progress
			ts.progressData = copts.progress_data
			sink = ts.enqueueProgress
		}
		opts, recursive, optErr := buildOptions(copts, sink)
		if optErr != nil {
			cancel()
			pipe.close()
			return makeError(optErr)
		}
		cxfer := C.pelicanc_transfer_alloc()
		cxfer.handle = C.uintptr_t(cgo.NewHandle(ts))
		cxfer.notify_fd = C.int(pipe.readFd)
		go runTransfer(ctx, ts, te, kind, a, b, recursive, opts)
		*out = cxfer
		return nil
	})
}

//export pelicanc_get_start
func pelicanc_get_start(remoteUrl, localPath *C.char, copts *C.pelican_transfer_opts, out **C.pelican_transfer) *C.pelican_error {
	return startTransfer(kindGet, C.GoString(remoteUrl), C.GoString(localPath), copts, out)
}

//export pelicanc_put_start
func pelicanc_put_start(localPath, remoteUrl *C.char, copts *C.pelican_transfer_opts, out **C.pelican_transfer) *C.pelican_error {
	return startTransfer(kindPut, C.GoString(remoteUrl), C.GoString(localPath), copts, out)
}

//export pelicanc_copy_start
func pelicanc_copy_start(sourceUrl, destUrl *C.char, copts *C.pelican_transfer_opts, out **C.pelican_transfer) *C.pelican_error {
	return startTransfer(kindCopy, C.GoString(sourceUrl), C.GoString(destUrl), copts, out)
}

//export pelicanc_prestage_start
func pelicanc_prestage_start(remoteUrl *C.char, copts *C.pelican_transfer_opts, out **C.pelican_transfer) *C.pelican_error {
	return startTransfer(kindPrestage, C.GoString(remoteUrl), "", copts, out)
}

//export pelicanc_copy
func pelicanc_copy(cctx *C.pelican_context, sourceUrl, destUrl *C.char, copts *C.pelican_transfer_opts, listOut **C.pelican_result_list) *C.pelican_error {
	return runSyncEngineJob(cctx, kindCopy, C.GoString(sourceUrl), C.GoString(destUrl), copts, listOut)
}

//export pelicanc_prestage
func pelicanc_prestage(cctx *C.pelican_context, remoteUrl *C.char, copts *C.pelican_transfer_opts, listOut **C.pelican_result_list) *C.pelican_error {
	return runSyncEngineJob(cctx, kindPrestage, C.GoString(remoteUrl), "", copts, listOut)
}

//export pelicanc_cache_info
func pelicanc_cache_info(cctx *C.pelican_context, remoteUrl *C.char, copts *C.pelican_transfer_opts, ageOut, sizeOut *C.longlong) *C.pelican_error {
	return guard(func() *C.pelican_error {
		if err := requireInit(); err != nil {
			return makeError(err)
		}
		opts, _, optErr := buildOptions(copts, nil)
		if optErr != nil {
			return makeError(optErr)
		}
		age, size, err := client.DoCacheInfo(goCtxFor(cctx), C.GoString(remoteUrl), opts...)
		if err != nil {
			return makeError(err)
		}
		if ageOut != nil {
			*ageOut = C.longlong(age)
		}
		if sizeOut != nil {
			*sizeOut = C.longlong(size)
		}
		return nil
	})
}

//export pelicanc_evict
func pelicanc_evict(cctx *C.pelican_context, remoteUrl *C.char, immediate C.int, copts *C.pelican_transfer_opts, messageOut **C.char) *C.pelican_error {
	return guard(func() *C.pelican_error {
		if err := requireInit(); err != nil {
			return makeError(err)
		}
		if messageOut != nil {
			*messageOut = nil
		}
		opts, _, optErr := buildOptions(copts, nil)
		if optErr != nil {
			return makeError(optErr)
		}
		message, err := client.DoEvict(goCtxFor(cctx), C.GoString(remoteUrl), immediate != 0, opts...)
		if err != nil {
			return makeError(err)
		}
		if messageOut != nil && message != "" {
			*messageOut = C.CString(message)
		}
		return nil
	})
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
	ts.drain()
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
		ts.close()
		ts.mu.Unlock()
		h.Delete()
	}
	C.free(unsafe.Pointer(cxfer))
}

/* ------------------------------------------------------------------ *
 * Asynchronous single-shot operations                                *
 * ------------------------------------------------------------------ */

// opKind tags which result an operation produced, so the take_*
// functions can refuse a mismatched request and opFree can release an
// uncollected payload.
type opKind int

const (
	opNone opKind = iota
	opFileInfo
	opFileInfoList
	opFile
	opCount
	opData
	opCacheInfo
	opMessage
)

// opResult is what an operation's goroutine produces.  Pointer payloads
// are C-allocated and owned by the operation until taken.
type opResult struct {
	kind  opKind
	ptr   unsafe.Pointer
	count int64
	age   int64
	size  int64
}

// opState is the Go side of a pelican_op.  Unlike a transfer, an
// operation completes exactly once, so the notification fd is written
// once and left readable: the host's event loop sees a level-triggered
// "ready" that persists until the operation is freed.
type opState struct {
	notifyPipe
	mu     sync.Mutex
	done   bool
	freed  bool
	err    *C.pelican_error
	result opResult
	cancel context.CancelFunc
	// onDone runs when the operation completes or is abandoned; used to
	// clear the owning file's busy flag.
	onDone func()
}

// freeResult releases an uncollected payload.
func freeResult(r opResult) {
	switch r.kind {
	case opFileInfo:
		C.pelican_file_info_free((*C.pelican_file_info)(r.ptr))
	case opFileInfoList:
		C.pelican_file_info_list_free((*C.pelican_file_info_list)(r.ptr))
	case opFile:
		// An unclaimed open() result still owns a live PelicanFS and its
		// transfer engine, so it must be closed rather than freed.
		_ = closeFileHandle((*C.pelican_file)(r.ptr))
	case opData, opMessage:
		C.free(r.ptr)
	}
}

func (os_ *opState) finish(r opResult, err error) {
	// Release the owning resource (e.g. the file's busy flag) BEFORE
	// publishing completion: a host woken by the notification fd may
	// immediately start the next operation on that file, and must not
	// be told it is busy by the operation that just finished.
	if os_.onDone != nil {
		os_.onDone()
	}
	os_.mu.Lock()
	defer os_.mu.Unlock()
	if os_.freed {
		freeResult(r)
		return
	}
	os_.done = true
	os_.result = r
	os_.err = makeError(err)
	os_.wake()
}

func opStateFor(cop *C.pelican_op) *opState {
	return cgo.Handle(cop.handle).Value().(*opState)
}

// startOp submits fn to run on a library goroutine and hands back a
// pelican_op.  fn must not touch caller-owned C memory: everything it
// needs (options, data to write) is captured on the calling thread
// before this returns.
func startOp(fn func(ctx context.Context) (opResult, error), onDone func(), out **C.pelican_op) *C.pelican_error {
	return guard(func() *C.pelican_error {
		if err := requireInit(); err != nil {
			if onDone != nil {
				onDone()
			}
			return makeError(err)
		}
		if out == nil {
			if onDone != nil {
				onDone()
			}
			return makeError(fmt.Errorf("output operation pointer may not be NULL"))
		}
		*out = nil
		pipe, err := newNotifyPipe()
		if err != nil {
			if onDone != nil {
				onDone()
			}
			return makeError(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		st := &opState{notifyPipe: pipe, cancel: cancel, onDone: onDone}
		cop := C.pelicanc_op_alloc()
		cop.handle = C.uintptr_t(cgo.NewHandle(st))
		cop.notify_fd = C.int(pipe.readFd)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					st.finish(opResult{}, fmt.Errorf("panic in pelican client: %v", r))
				}
			}()
			result, err := fn(ctx)
			st.finish(result, err)
		}()
		*out = cop
		return nil
	})
}

//export pelicanc_op_is_done
func pelicanc_op_is_done(cop *C.pelican_op) C.int {
	st := opStateFor(cop)
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.done {
		return 1
	}
	return 0
}

//export pelicanc_op_error
func pelicanc_op_error(cop *C.pelican_op) *C.pelican_error {
	st := opStateFor(cop)
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.err
}

//export pelicanc_op_cancel
func pelicanc_op_cancel(cop *C.pelican_op) {
	if cop == nil || cop.handle == 0 {
		return
	}
	opStateFor(cop).cancel()
}

//export pelicanc_op_free
func pelicanc_op_free(cop *C.pelican_op) {
	if cop == nil {
		return
	}
	if cop.handle != 0 {
		h := cgo.Handle(cop.handle)
		st := h.Value().(*opState)
		st.mu.Lock()
		st.freed = true
		freeResult(st.result)
		st.result = opResult{}
		C.pelican_error_free(st.err)
		st.err = nil
		st.close()
		st.mu.Unlock()
		// Cancel outside the lock: cancellation can race the operation's
		// own completion, which needs the same mutex.
		st.cancel()
		h.Delete()
	}
	C.free(unsafe.Pointer(cop))
}

// takeResult hands the payload of a completed operation to the caller if
// it matches the requested kind.
func takeResult(cop *C.pelican_op, want opKind) (opResult, bool) {
	st := opStateFor(cop)
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.done || st.err != nil || st.result.kind != want {
		return opResult{}, false
	}
	r := st.result
	st.result = opResult{}
	return r, true
}

//export pelicanc_op_take_file_info
func pelicanc_op_take_file_info(cop *C.pelican_op, out **C.pelican_file_info) C.int {
	r, ok := takeResult(cop, opFileInfo)
	if !ok {
		return 0
	}
	if out != nil {
		*out = (*C.pelican_file_info)(r.ptr)
	} else {
		freeResult(r)
	}
	return 1
}

//export pelicanc_op_take_file_info_list
func pelicanc_op_take_file_info_list(cop *C.pelican_op, out **C.pelican_file_info_list) C.int {
	r, ok := takeResult(cop, opFileInfoList)
	if !ok {
		return 0
	}
	if out != nil {
		*out = (*C.pelican_file_info_list)(r.ptr)
	} else {
		freeResult(r)
	}
	return 1
}

//export pelicanc_op_take_file
func pelicanc_op_take_file(cop *C.pelican_op, out **C.pelican_file) C.int {
	r, ok := takeResult(cop, opFile)
	if !ok {
		return 0
	}
	if out != nil {
		*out = (*C.pelican_file)(r.ptr)
	} else {
		freeResult(r)
	}
	return 1
}

//export pelicanc_op_take_count
func pelicanc_op_take_count(cop *C.pelican_op, out *C.longlong) C.int {
	r, ok := takeResult(cop, opCount)
	if !ok {
		return 0
	}
	if out != nil {
		*out = C.longlong(r.count)
	}
	return 1
}

//export pelicanc_op_take_data
func pelicanc_op_take_data(cop *C.pelican_op, buf *unsafe.Pointer, length *C.size_t) C.int {
	r, ok := takeResult(cop, opData)
	if !ok {
		return 0
	}
	if buf == nil {
		freeResult(r)
		return 1
	}
	*buf = r.ptr
	if length != nil {
		*length = C.size_t(r.count)
	}
	return 1
}

//export pelicanc_op_take_cache_info
func pelicanc_op_take_cache_info(cop *C.pelican_op, ageOut, sizeOut *C.longlong) C.int {
	r, ok := takeResult(cop, opCacheInfo)
	if !ok {
		return 0
	}
	if ageOut != nil {
		*ageOut = C.longlong(r.age)
	}
	if sizeOut != nil {
		*sizeOut = C.longlong(r.size)
	}
	return 1
}

//export pelicanc_op_take_message
func pelicanc_op_take_message(cop *C.pelican_op, out **C.char) C.int {
	r, ok := takeResult(cop, opMessage)
	if !ok {
		return 0
	}
	if out != nil {
		*out = (*C.char)(r.ptr)
	} else {
		freeResult(r)
	}
	return 1
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
		opts, _, optErr := buildOptions(copts, nil)
		if optErr != nil {
			return makeError(optErr)
		}
		info, err := client.DoStat(goCtxFor(cctx), C.GoString(remoteUrl), opts...)
		if err != nil {
			return makeError(err)
		}
		*cinfo = makeFileInfo(info)
		return nil
	})
}

// makeFileInfo converts one client.FileInfo into a C file info.
func makeFileInfo(info *client.FileInfo) *C.pelican_file_info {
	out := C.pelicanc_file_info_alloc()
	fillFileInfo(out, info.Name, info.Size, info.ModTime.Unix(), info.IsCollection)
	return out
}

// makeFileInfoList converts a listing into a C file info list.
func makeFileInfoList(infos []client.FileInfo) *C.pelican_file_info_list {
	list := C.pelicanc_file_info_list_alloc(C.size_t(len(infos)))
	if len(infos) > 0 {
		items := unsafe.Slice(list.items, len(infos))
		for i := range infos {
			items[i] = makeFileInfo(&infos[i])
		}
		list.count = C.size_t(len(infos))
	}
	return list
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
		opts, _, optErr := buildOptions(copts, nil)
		if optErr != nil {
			return makeError(optErr)
		}
		infos, err := client.DoList(goCtxFor(cctx), C.GoString(remoteUrl), opts...)
		if err != nil {
			return makeError(err)
		}
		*listOut = makeFileInfoList(infos)
		return nil
	})
}

//export pelicanc_delete
func pelicanc_delete(cctx *C.pelican_context, remoteUrl *C.char, copts *C.pelican_transfer_opts) *C.pelican_error {
	return guard(func() *C.pelican_error {
		if err := requireInit(); err != nil {
			return makeError(err)
		}
		opts, recursive, optErr := buildOptions(copts, nil)
		if optErr != nil {
			return makeError(optErr)
		}
		return makeError(client.DoDelete(goCtxFor(cctx), C.GoString(remoteUrl), recursive, opts...))
	})
}

/* Asynchronous namespace and cache operations.  Options are translated
 * on the calling thread so the caller may free them as soon as the
 * *_start call returns. */

//export pelicanc_stat_start
func pelicanc_stat_start(remoteUrl *C.char, copts *C.pelican_transfer_opts, out **C.pelican_op) *C.pelican_error {
	opts, _, optErr := buildOptions(copts, nil)
	if optErr != nil {
		return makeError(optErr)
	}
	remote := C.GoString(remoteUrl)
	return startOp(func(ctx context.Context) (opResult, error) {
		info, err := client.DoStat(ctx, remote, opts...)
		if err != nil {
			return opResult{}, err
		}
		return opResult{kind: opFileInfo, ptr: unsafe.Pointer(makeFileInfo(info))}, nil
	}, nil, out)
}

//export pelicanc_list_start
func pelicanc_list_start(remoteUrl *C.char, copts *C.pelican_transfer_opts, out **C.pelican_op) *C.pelican_error {
	opts, _, optErr := buildOptions(copts, nil)
	if optErr != nil {
		return makeError(optErr)
	}
	remote := C.GoString(remoteUrl)
	return startOp(func(ctx context.Context) (opResult, error) {
		infos, err := client.DoList(ctx, remote, opts...)
		if err != nil {
			return opResult{}, err
		}
		return opResult{kind: opFileInfoList, ptr: unsafe.Pointer(makeFileInfoList(infos))}, nil
	}, nil, out)
}

//export pelicanc_delete_start
func pelicanc_delete_start(remoteUrl *C.char, copts *C.pelican_transfer_opts, out **C.pelican_op) *C.pelican_error {
	opts, recursive, optErr := buildOptions(copts, nil)
	if optErr != nil {
		return makeError(optErr)
	}
	remote := C.GoString(remoteUrl)
	return startOp(func(ctx context.Context) (opResult, error) {
		return opResult{}, client.DoDelete(ctx, remote, recursive, opts...)
	}, nil, out)
}

//export pelicanc_cache_info_start
func pelicanc_cache_info_start(remoteUrl *C.char, copts *C.pelican_transfer_opts, out **C.pelican_op) *C.pelican_error {
	opts, _, optErr := buildOptions(copts, nil)
	if optErr != nil {
		return makeError(optErr)
	}
	remote := C.GoString(remoteUrl)
	return startOp(func(ctx context.Context) (opResult, error) {
		age, size, err := client.DoCacheInfo(ctx, remote, opts...)
		if err != nil {
			return opResult{}, err
		}
		return opResult{kind: opCacheInfo, age: int64(age), size: size}, nil
	}, nil, out)
}

//export pelicanc_evict_start
func pelicanc_evict_start(remoteUrl *C.char, immediate C.int, copts *C.pelican_transfer_opts, out **C.pelican_op) *C.pelican_error {
	opts, _, optErr := buildOptions(copts, nil)
	if optErr != nil {
		return makeError(optErr)
	}
	remote := C.GoString(remoteUrl)
	now := immediate != 0
	return startOp(func(ctx context.Context) (opResult, error) {
		message, err := client.DoEvict(ctx, remote, now, opts...)
		if err != nil {
			return opResult{}, err
		}
		return opResult{kind: opMessage, ptr: unsafe.Pointer(C.CString(message))}, nil
	}, nil, out)
}

/* ------------------------------------------------------------------ *
 * File I/O (PelicanFS)                                               *
 * ------------------------------------------------------------------ */

// fileState is the Go side of a pelican_file.  Each open file owns a
// PelicanFS instance (and thus a TransferEngine); PelicanFS exposes no
// shutdown, so the context is cancelled on close to reap the engine's
// goroutines.
// A PelicanFile is not safe for concurrent use (reads mutate the file
// position and share a pipe), so each handle admits one operation at a
// time; busy tracks that, letting a second call fail cleanly instead of
// corrupting the stream.
type fileState struct {
	file   fs.File
	cancel context.CancelFunc
	mu     sync.Mutex
	busy   bool
}

// acquire claims the handle for one operation.
func (fst *fileState) acquire() error {
	fst.mu.Lock()
	defer fst.mu.Unlock()
	if fst.busy {
		return fmt.Errorf("file handle is busy: another operation is already in flight")
	}
	fst.busy = true
	return nil
}

func (fst *fileState) release() {
	fst.mu.Lock()
	fst.busy = false
	fst.mu.Unlock()
}

func fileStateFor(cfile *C.pelican_file) *fileState {
	return cgo.Handle(cfile.handle).Value().(*fileState)
}

// openFile performs the blocking open, shared by the synchronous and
// asynchronous entry points.
func openFile(remote string, goFlags int, opts []client.TransferOption) (*C.pelican_file, error) {
	u, err := url.Parse(remote)
	if err != nil {
		return nil, fmt.Errorf("failed to parse remote URL: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("remote URL %q must include a scheme and federation host (e.g. pelican://federation/path)", u.String())
	}
	ctx, cancel := context.WithCancel(context.Background())
	pfs := client.NewPelicanFSWithPrefix(ctx, u.Scheme+"://"+u.Host, opts...)
	f, err := pfs.OpenFile(u.Path, goFlags)
	if err != nil {
		cancel()
		return nil, err
	}
	cfile := C.pelicanc_file_alloc()
	cfile.handle = C.uintptr_t(cgo.NewHandle(&fileState{file: f, cancel: cancel}))
	return cfile, nil
}

// closeFileHandle closes the underlying file, reaps the PelicanFS
// context, and releases the handle.  Shared by the synchronous close,
// the asynchronous close, and the disposal of an open() result that the
// caller never collected.
func closeFileHandle(cfile *C.pelican_file) error {
	if cfile == nil {
		return nil
	}
	var err error
	if cfile.handle != 0 {
		h := cgo.Handle(cfile.handle)
		fst := h.Value().(*fileState)
		func() {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("panic in pelican client: %v", r)
				}
			}()
			err = fst.file.Close()
		}()
		fst.cancel()
		h.Delete()
	}
	C.free(unsafe.Pointer(cfile))
	return err
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
		opts, _, optErr := buildOptions(copts, nil)
		if optErr != nil {
			return makeError(optErr)
		}
		cfile, err := openFile(C.GoString(remoteUrl), goFlags, opts)
		if err != nil {
			return makeError(err)
		}
		*out = cfile
		return nil
	})
}

// ioGuard wraps the read/write/seek-style exports: panics become errors,
// and the (result, error) pair is mapped to the C convention of
// count-or-negative-one with an optional error out-parameter.  The file
// handle is claimed for the duration, so a synchronous call cannot
// interleave with an asynchronous operation on the same file.
func ioGuard(cfile *C.pelican_file, errOut **C.pelican_error, fn func(fst *fileState) (int64, error)) (ret C.longlong) {
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
		fst := fileStateFor(cfile)
		if err = fst.acquire(); err != nil {
			return
		}
		defer fst.release()
		n, err = fn(fst)
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
	return ioGuard(cfile, errOut, func(st *fileState) (int64, error) {
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
	return ioGuard(cfile, errOut, func(st *fileState) (int64, error) {
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
	return ioGuard(cfile, errOut, func(st *fileState) (int64, error) {
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
	return ioGuard(cfile, errOut, func(st *fileState) (int64, error) {
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
		fst := fileStateFor(cfile)
		if err := fst.acquire(); err != nil {
			return makeError(err)
		}
		defer fst.release()
		info, err := fst.file.Stat()
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
	if cfile.handle != 0 {
		// Claimed and never released: the handle ceases to exist here.
		if err := fileStateFor(cfile).acquire(); err != nil {
			return makeError(err)
		}
	}
	return makeError(closeFileHandle(cfile))
}

/* ------------------------------------------------------------------ *
 * Asynchronous file I/O                                              *
 * ------------------------------------------------------------------ */

// startFileOp claims the file handle, runs fn on a library goroutine,
// and releases the handle when the operation settles.
func startFileOp(cfile *C.pelican_file, fn func(fst *fileState) (opResult, error), out **C.pelican_op) *C.pelican_error {
	if cfile == nil || cfile.handle == 0 {
		return makeError(fmt.Errorf("file handle may not be NULL"))
	}
	fst := fileStateFor(cfile)
	if err := fst.acquire(); err != nil {
		return makeError(err)
	}
	return startOp(func(context.Context) (opResult, error) {
		return fn(fst)
	}, fst.release, out)
}

//export pelicanc_fs_open_start
func pelicanc_fs_open_start(remoteUrl *C.char, flags C.int, copts *C.pelican_transfer_opts, out **C.pelican_op) *C.pelican_error {
	goFlags, err := translateOpenFlags(flags)
	if err != nil {
		return makeError(err)
	}
	opts, _, optErr := buildOptions(copts, nil)
	if optErr != nil {
		return makeError(optErr)
	}
	remote := C.GoString(remoteUrl)
	return startOp(func(context.Context) (opResult, error) {
		cfile, err := openFile(remote, goFlags, opts)
		if err != nil {
			return opResult{}, err
		}
		return opResult{kind: opFile, ptr: unsafe.Pointer(cfile)}, nil
	}, nil, out)
}

// readIntoC allocates a library-owned C buffer and fills it using read.
// The buffer belongs to the operation, so nothing the caller owns has to
// stay alive for the duration of the I/O.
func readIntoC(length C.size_t, read func(p []byte) (int, error)) (opResult, error) {
	if length == 0 {
		return opResult{kind: opData}, nil
	}
	buf := C.malloc(length)
	if buf == nil {
		return opResult{}, fmt.Errorf("failed to allocate %d bytes for read buffer", int(length))
	}
	n, err := read(unsafe.Slice((*byte)(buf), int(length)))
	if err != nil && err != io.EOF {
		C.free(buf)
		return opResult{}, err
	}
	if n <= 0 {
		// End of file: report zero bytes rather than an error.
		C.free(buf)
		return opResult{kind: opData}, nil
	}
	return opResult{kind: opData, ptr: buf, count: int64(n)}, nil
}

//export pelicanc_file_read_start
func pelicanc_file_read_start(cfile *C.pelican_file, length C.size_t, out **C.pelican_op) *C.pelican_error {
	return startFileOp(cfile, func(fst *fileState) (opResult, error) {
		return readIntoC(length, fst.file.Read)
	}, out)
}

//export pelicanc_file_pread_start
func pelicanc_file_pread_start(cfile *C.pelican_file, length C.size_t, offset C.longlong, out **C.pelican_op) *C.pelican_error {
	return startFileOp(cfile, func(fst *fileState) (opResult, error) {
		ra, ok := fst.file.(io.ReaderAt)
		if !ok {
			return opResult{}, fmt.Errorf("positional reads are not supported for this file")
		}
		return readIntoC(length, func(p []byte) (int, error) {
			return ra.ReadAt(p, int64(offset))
		})
	}, out)
}

//export pelicanc_file_write_start
func pelicanc_file_write_start(cfile *C.pelican_file, buf unsafe.Pointer, length C.size_t, out **C.pelican_op) *C.pelican_error {
	// Copy the payload up front so the caller may reuse or free its
	// buffer as soon as this returns.
	data := C.GoBytes(buf, C.int(length))
	return startFileOp(cfile, func(fst *fileState) (opResult, error) {
		w, ok := fst.file.(io.Writer)
		if !ok {
			return opResult{}, fmt.Errorf("file is not open for writing")
		}
		n, err := w.Write(data)
		if err != nil {
			return opResult{}, err
		}
		return opResult{kind: opCount, count: int64(n)}, nil
	}, out)
}

//export pelicanc_file_close_start
func pelicanc_file_close_start(cfile *C.pelican_file, out **C.pelican_op) *C.pelican_error {
	if cfile == nil || cfile.handle == 0 {
		return makeError(fmt.Errorf("file handle may not be NULL"))
	}
	// Claimed and never released: this consumes the handle.
	if err := fileStateFor(cfile).acquire(); err != nil {
		return makeError(err)
	}
	cerr := startOp(func(context.Context) (opResult, error) {
		return opResult{}, closeFileHandle(cfile)
	}, nil, out)
	if cerr != nil {
		// The operation never started, so nothing will close the file:
		// do it here rather than leaking the handle and its engine.
		_ = closeFileHandle(cfile)
	}
	return cerr
}

// main is required for -buildmode=c-shared; it is never executed.
func main() {}
