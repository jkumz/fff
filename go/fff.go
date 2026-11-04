package fff

/*
#cgo pkg-config: fff_c
#include <fff.h>

extern void fffGoWatchBatch(uint64_t watch_id, struct FffWatchEventBatch *batch, uintptr_t user_data);

static void fff_go_watch_shim(uint64_t watch_id, struct FffWatchEventBatch *batch, void *user_data) {
	fffGoWatchBatch(watch_id, batch, (uintptr_t)user_data);
}

static struct FffResult *fff_go_set_watch_callback(void *handle, uintptr_t user_data) {
	return fff_set_watch_callback(handle, fff_go_watch_shim, (void *)user_data);
}
*/
import "C"

import (
	"errors"
	"runtime"
	"runtime/cgo"
	"strings"
	"time"
	"unicode/utf8"
	"unsafe"
)

// ErrClosed is returned by every Instance method except Close and IsScanning after Close.
var ErrClosed = errors.New("fff: instance is closed")

var (
	errNUL         = errors.New("fff: string contains a NUL byte")
	errInvalidUTF8 = errors.New("fff: string is not valid UTF-8")
)

// Instance owns one FFF file picker, whose native resources leak unless Close is called.
// Methods are safe for concurrent use as FFF locks its state; Close must not overlap other calls.
type Instance struct {
	handle  unsafe.Pointer
	watches *watchSet
	self    cgo.Handle
}

// Options configures New. Empty strings and zero numbers select FFF's defaults.
type Options struct {
	BasePath       string // directory to index, required
	FrecencyDBPath string // frecency database, empty to skip frecency tracking
	HistoryDBPath  string // query history database, empty to skip query tracking

	EnableMmapCache       bool // map the most frecent files into memory after the initial scan
	EnableContentIndexing bool // build a content index after the initial scan for faster grep
	Watch                 bool // keep the index current with a file watcher, needed by Instance.Watch
	// AIMode tunes FFF for agents: Grep parses queries differently, frecency decays faster and
	// the watcher counts file edits as accesses.
	AIMode bool

	// LogFilePath enables tracing to <stem>+<unix secs>+<pid>.<ext> in its directory, deleting all
	// but the newest 20 <stem>+*.<ext> files there. The first log setting in a process wins.
	// It installs FFF's process-wide panic hook and SIGSEGV handler, after which any Go nil
	// dereference kills the process instead of panicking.
	LogFilePath string
	// LogLevel is "trace", "debug", "info", "warn" or "error", and "info" when empty.
	// A valid RUST_LOG in the environment overrides it.
	LogLevel string

	// Content cache limits: file count, total bytes and bytes per file. All three zero sizes them
	// from the repository after the initial scan; otherwise a zero one is fixed at 10,000 files,
	// 256 MB or 10 MB. A nonzero CacheBudgetMaxFileSize also makes Grep skip larger files.
	CacheBudgetMaxFiles    uint64
	CacheBudgetMaxBytes    uint64
	CacheBudgetMaxFileSize uint64

	// Off by default: indexing / or the home directory floods the watcher with churn,
	// and cyclic symlinks can wedge it.
	EnableFSRootScanning  bool
	EnableHomeDirScanning bool
	FollowSymlinks        bool
}

// New creates an instance and starts indexing opts.BasePath in the background.
func New(opts Options) (*Instance, error) {
	var strs pinnedStrings
	defer strs.pins.Unpin()

	copts := C.struct_FffCreateOptions{
		version:                    C.FFF_CREATE_OPTIONS_VERSION,
		base_path:                  strs.add(opts.BasePath),
		frecency_db_path:           strs.add(opts.FrecencyDBPath),
		history_db_path:            strs.add(opts.HistoryDBPath),
		enable_mmap_cache:          C.bool(opts.EnableMmapCache),
		enable_content_indexing:    C.bool(opts.EnableContentIndexing),
		watch:                      C.bool(opts.Watch),
		ai_mode:                    C.bool(opts.AIMode),
		log_file_path:              strs.add(opts.LogFilePath),
		log_level:                  strs.add(opts.LogLevel),
		cache_budget_max_files:     C.uint64_t(opts.CacheBudgetMaxFiles),
		cache_budget_max_bytes:     C.uint64_t(opts.CacheBudgetMaxBytes),
		cache_budget_max_file_size: C.uint64_t(opts.CacheBudgetMaxFileSize),
		enable_fs_root_scanning:    C.bool(opts.EnableFSRootScanning),
		enable_home_dir_scanning:   C.bool(opts.EnableHomeDirScanning),
		follow_symlinks:            C.bool(opts.FollowSymlinks),
	}
	if strs.err != nil {
		return nil, strs.err
	}
	handle, _, err := unwrap(C.fff_create_instance_with(&copts))
	if err != nil {
		return nil, err
	}

	// One callback per instance routes every subscription by its watch id.
	watches := &watchSet{fns: map[uint64]func([]WatchEvent){}}
	self := cgo.NewHandle(watches)
	if _, _, err := unwrap(C.fff_go_set_watch_callback(handle, C.uintptr_t(self))); err != nil {
		C.fff_destroy(handle)
		self.Delete()
		return nil, err
	}
	return &Instance{handle: handle, watches: watches, self: self}, nil
}

// Close stops and frees the instance, first waiting for a running watch callback to return.
// Later calls get ErrClosed without reaching FFF, except Close (nil) and IsScanning (false).
func (i *Instance) Close() error {
	if i.handle == nil {
		return nil
	}
	// A callback running while fff_destroy waits for it must not subscribe again.
	i.watches.mu.Lock()
	i.watches.closed = true
	i.watches.mu.Unlock()
	C.fff_destroy(i.handle)
	i.handle = nil
	i.self.Delete()
	// The callbacks, and whatever they capture, must not outlive the instance.
	i.watches.mu.Lock()
	clear(i.watches.fns)
	i.watches.mu.Unlock()
	return nil
}

type pinnedStrings struct {
	pins runtime.Pinner
	err  error
}

func (p *pinnedStrings) add(s string) *C.char {
	if p.err != nil {
		return nil
	}
	c, err := cString(s)
	if err != nil {
		p.err = err
		return nil
	}
	p.pins.Pin(c)
	return c
}

func unwrap(r *C.struct_FffResult) (unsafe.Pointer, int64, error) {
	handle, value := r.handle, int64(r.int_value)
	var err error
	if !r.success {
		err = errors.New("fff: " + C.GoString(r.error))
	}
	C.fff_free_result(r)
	return handle, value, err
}

func millis(d time.Duration) C.uint64_t {
	// Zero means "now" to the wait calls and "no budget" to grep, so round up.
	if d <= 0 {
		return 0
	}
	ms := d / time.Millisecond
	if d%time.Millisecond != 0 {
		ms++
	}
	return C.uint64_t(ms)
}

func cString(s string) (*C.char, error) {
	if err := checkString(s); err != nil {
		return nil, err
	}
	// Go memory, so C may read it only for the duration of one call.
	b := make([]byte, len(s)+1)
	copy(b, s)
	return (*C.char)(unsafe.Pointer(&b[0])), nil
}

func checkString(s string) error {
	// FFF reads every string as UTF-8 up to the first NUL, and treats invalid UTF-8 as absent.
	if strings.IndexByte(s, 0) >= 0 {
		return errNUL
	}
	if !utf8.ValidString(s) {
		return errInvalidUTF8
	}
	return nil
}

func cView(p *C.char) string {
	// Borrowed: valid only until the C owner is freed.
	start := unsafe.Pointer(p)
	n := 0
	for *(*byte)(unsafe.Add(start, n)) != 0 {
		n++
	}
	return unsafe.String((*byte)(start), n)
}

func reuse(prev string, p *C.char) string {
	if v := cView(p); v != prev {
		return strings.Clone(v)
	}
	return prev
}
