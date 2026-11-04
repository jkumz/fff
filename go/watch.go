package fff

/*
#include <fff.h>
*/
import "C"

import (
	"runtime/cgo"
	"sync"
	"unsafe"
)

// WatchEventKind says what happened to WatchEvent.Path.
type WatchEventKind uint8

// Rescan means events were lost, so re-stat the files you care about. It reaches every
// subscription, whatever its pattern and Ignore, with Path set to the base path.
const (
	Created WatchEventKind = iota
	Modified
	Removed
	Rescan
	Renamed
)

// Pins the kinds to the values a comment in fff.h documents; watchEvents converts them as is.
var _ [0]byte = [(Created ^ 0) | (Modified ^ 1) | (Removed ^ 2) |
	(Rescan ^ 3) | (Renamed ^ 4)]byte{}

// WatchEvent is one file system change.
type WatchEvent struct {
	Path string // absolute, unlike the relative paths search and grep return
	Kind WatchEventKind
	From string // the previous absolute path when Kind is Renamed
}

// WatchOptions configures one Watch subscription.
type WatchOptions struct {
	// Ignore drops events matching any entry: one with a wildcard is a glob relative to the base
	// path, any other a path prefix relative to it. An entry outside the base path fails Watch.
	Ignore []string
}

type watchSet struct {
	mu     sync.RWMutex
	fns    map[uint64]func([]WatchEvent)
	closed bool
}

// Watch calls fn with events matching pattern: all if empty, else a glob, file or subtree relative
// to the base path. It needs Options.Watch and fails until WaitForWatcher is ready. One thread runs
// an instance's callbacks in turn, so a slow fn delays every subscription. RestartIndex and Close
// wait for fn: it may call Watch and Unwatch, but not Close or wait on a goroutine calling either.
func (i *Instance) Watch(pattern string, opts WatchOptions, fn func([]WatchEvent)) (uint64, error) {
	// Batches wait on the read lock until fn is stored; fff_watch never waits on them.
	i.watches.mu.Lock()
	defer i.watches.mu.Unlock()
	if i.watches.closed {
		return 0, ErrClosed
	}
	var strs pinnedStrings
	defer strs.pins.Unpin()

	cpattern := strs.add(pattern)
	copts := C.struct_FffWatchOptions{
		version:      C.FFF_WATCH_OPTIONS_VERSION,
		ignore_count: C.uint32_t(len(opts.Ignore)),
	}
	if len(opts.Ignore) > 0 {
		ignore := make([]*C.char, len(opts.Ignore))
		for k, glob := range opts.Ignore {
			ignore[k] = strs.add(glob)
		}
		strs.pins.Pin(&ignore[0])
		copts.ignore = &ignore[0]
	}
	if strs.err != nil {
		return 0, strs.err
	}
	_, id, err := unwrap(C.fff_watch(i.handle, cpattern, &copts))
	if err != nil {
		return 0, err
	}
	i.watches.fns[uint64(id)] = fn
	return uint64(id), nil
}

// Unwatch ends a subscription and reports whether it existed, or returns ErrClosed once Close
// has started. A batch already being delivered may still reach its function.
func (i *Instance) Unwatch(id uint64) (bool, error) {
	// Under the lock, as in Watch, so a callback cannot reach a handle fff_destroy has taken.
	i.watches.mu.Lock()
	defer i.watches.mu.Unlock()
	if i.watches.closed {
		return false, ErrClosed
	}
	delete(i.watches.fns, id)
	_, existed, err := unwrap(C.fff_unwatch(i.handle, C.uint64_t(id)))
	return existed == 1, err
}

//export fffGoWatchBatch
func fffGoWatchBatch(id C.uint64_t, batch *C.struct_FffWatchEventBatch, userData C.uintptr_t) {
	set := cgo.Handle(userData).Value().(*watchSet)
	// Unlocked before fn runs, so fn can call Watch and Unwatch, which take the write lock.
	set.mu.RLock()
	fn := set.fns[uint64(id)]
	set.mu.RUnlock()

	// Batches for an id already unwatched are dropped.
	if fn == nil {
		C.fff_free_watch_events(batch)
		return
	}
	events := watchEvents(batch)
	C.fff_free_watch_events(batch)
	fn(events)
}

func watchEvents(b *C.struct_FffWatchEventBatch) []WatchEvent {
	cevents := unsafe.Slice(b.events, b.count)
	// rename_sources is NULL when the batch has no renames.
	var sources []*C.char
	if b.rename_sources != nil {
		sources = unsafe.Slice(b.rename_sources, b.count)
	}
	events := make([]WatchEvent, len(cevents))
	for k := range cevents {
		events[k] = WatchEvent{Path: C.GoString(cevents[k].path), Kind: WatchEventKind(cevents[k].kind)}
		if sources != nil && sources[k] != nil {
			events[k].From = C.GoString(sources[k])
		}
	}
	return events
}
