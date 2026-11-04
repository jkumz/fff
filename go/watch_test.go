package fff

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestWatch(t *testing.T) {
	root := writeTree(t, map[string]string{"keep.txt": "keep\n"})
	inst := newWatchedInstance(t, root)

	// Batches reach subscriptions in the order they were made, so once all has an event
	// the others have seen its batch too.
	var glob, dir, plain, globIgnore, all eventLog
	ids := []uint64{
		watch(t, inst, "**/*.txt", WatchOptions{}, &glob),
		watch(t, inst, root, WatchOptions{Ignore: []string{"*.log"}}, &dir),
		watch(t, inst, root, WatchOptions{}, &plain),
		watch(t, inst, "**/*", WatchOptions{Ignore: []string{"*.log", "*.tmp"}}, &globIgnore),
		watch(t, inst, "", WatchOptions{}, &all),
	}

	// FFF reports a path it has not indexed yet as created, and later writes as modified.
	hello := filepath.Join(root, "hello.txt")
	all.writeUntilSeen(t, hello)
	if first := all.first(hello); first.Kind != Created {
		t.Fatalf("first event for a new file = %+v", first)
	}
	for _, l := range []*eventLog{&glob, &dir, &plain, &globIgnore} {
		l.waitFor(t, func(e WatchEvent) bool { return e.Path == hello })
	}
	all.writeUntil(t, hello, func(e WatchEvent) bool { return e == WatchEvent{Path: hello, Kind: Modified} })

	noise, noiseTmp := filepath.Join(root, "noise.log"), filepath.Join(root, "noise.tmp")
	all.writeUntilSeen(t, noise)
	all.writeUntilSeen(t, noiseTmp)
	if glob.has(func(e WatchEvent) bool { return e.Path == noise }) {
		t.Fatal("noise.log reached the **/*.txt subscription")
	}
	if dir.has(func(e WatchEvent) bool { return e.Path == noise }) {
		t.Fatal("ignored noise.log reached the directory subscription")
	}
	// The same directory without Ignore gets both, so only Ignore kept them from the others.
	for _, path := range []string{noise, noiseTmp} {
		if !plain.has(func(e WatchEvent) bool { return e.Path == path }) {
			t.Fatalf("%s did not reach the directory subscription without Ignore", path)
		}
		if globIgnore.has(func(e WatchEvent) bool { return e.Path == path }) {
			t.Fatalf("ignored %s reached the **/* subscription", path)
		}
	}

	if err := os.Remove(hello); err != nil {
		t.Fatal(err)
	}
	all.waitFor(t, func(e WatchEvent) bool { return e == WatchEvent{Path: hello, Kind: Removed} })

	// A rename arrives as one event carrying both paths.
	from := filepath.Join(root, "to_rename.txt")
	to := filepath.Join(root, "renamed.txt")
	all.writeUntilSeen(t, from)
	if err := os.Rename(from, to); err != nil {
		t.Fatal(err)
	}
	all.waitFor(t, func(e WatchEvent) bool { return e == WatchEvent{Path: to, Kind: Renamed, From: from} })

	for _, id := range ids {
		if existed, err := inst.Unwatch(id); err != nil || !existed {
			t.Fatalf("Unwatch(%d) = %v, %v", id, existed, err)
		}
	}
	if existed, err := inst.Unwatch(ids[1]); err != nil || existed {
		t.Fatalf("repeated Unwatch = %v, %v", existed, err)
	}
}

func TestWatchPaths(t *testing.T) {
	root := writeTree(t, map[string]string{"sub/keep.txt": "keep\n"})
	inst := newWatchedInstance(t, root)
	var exact, sub, notSub, all eventLog
	watch(t, inst, "exact.txt", WatchOptions{}, &exact)
	watch(t, inst, "sub", WatchOptions{}, &sub)
	watch(t, inst, "", WatchOptions{Ignore: []string{"sub/"}}, &notSub)
	watch(t, inst, "", WatchOptions{}, &all)

	// Subscribed last, all sees each batch after the others have. Events for the tree written
	// before the watcher started may still arrive, so only the paths written here count.
	exactPath, inSub, other := filepath.Join(root, "exact.txt"), filepath.Join(root, "sub/in.txt"), filepath.Join(root, "other.txt")
	written := []string{exactPath, inSub, other}
	for _, path := range written {
		all.writeUntilSeen(t, path)
	}
	for _, c := range []struct {
		name string
		log  *eventLog
		want []string
	}{
		{"exact.txt", &exact, []string{exactPath}},
		{"sub", &sub, []string{inSub}},
		{"Ignore sub/", &notSub, []string{exactPath, other}},
	} {
		if got := c.log.seen(written); !slices.Equal(got, c.want) {
			t.Errorf("%s subscription got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestWatchRescan(t *testing.T) {
	root := writeTree(t, map[string]string{"keep.txt": "keep\n"})
	inst := newWatchedInstance(t, root)
	var goFiles eventLog
	watch(t, inst, "**/*.go", WatchOptions{}, &goFiles)
	// A changed .gitignore makes FFF rescan, which reaches every subscription with the base path.
	goFiles.writeUntil(t, filepath.Join(root, ".gitignore"), func(e WatchEvent) bool {
		return e == WatchEvent{Path: root, Kind: Rescan}
	})
}

func TestWatchNeedsWatcher(t *testing.T) {
	_, err := fixture.Watch("", WatchOptions{}, func([]WatchEvent) {})
	want := "fff: Failed to subscribe: File system watching is disabled for this picker"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
}

func TestWatchRejectsOutsidePaths(t *testing.T) {
	inst := newWatchedInstance(t, writeTree(t, map[string]string{"keep.txt": "keep\n"}))
	outside := t.TempDir()
	const watchErr = "watch patterns must be inside the indexed base path"
	const ignoreErr = "ignore patterns must be inside the indexed base path"
	for _, c := range []struct {
		pattern string
		ignore  []string
		want    string
	}{
		{outside, nil, watchErr},
		{"../x", nil, watchErr},
		{"", []string{outside}, ignoreErr},
		{"", []string{"../x"}, ignoreErr},
	} {
		_, err := inst.Watch(c.pattern, WatchOptions{Ignore: c.ignore}, func([]WatchEvent) {})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Watch(%q, Ignore %q) = %v, want %q", c.pattern, c.ignore, err, c.want)
		}
	}
}

func TestWatchDropsUnknownID(t *testing.T) {
	root := writeTree(t, map[string]string{"keep.txt": "keep\n"})
	inst := newWatchedInstance(t, root)
	var forgotten, all eventLog
	id := watch(t, inst, "", WatchOptions{}, &forgotten)
	forgotten.writeUntilSeen(t, filepath.Join(root, "before.txt"))
	forgetWatch(inst, id)

	// Batches reach subscriptions in the order they were made, so FFF delivered hello.txt
	// to the forgotten id before all saw it.
	watch(t, inst, "", WatchOptions{}, &all)
	hello := filepath.Join(root, "hello.txt")
	all.writeUntilSeen(t, hello)
	if forgotten.has(func(e WatchEvent) bool { return e.Path == hello }) {
		t.Fatal("a batch reached a forgotten subscription")
	}

	// FFF still delivers to the id, so the batch above was dropped on the Go side.
	inst.watches.mu.Lock()
	inst.watches.fns[id] = forgotten.add
	inst.watches.mu.Unlock()
	forgotten.writeUntilSeen(t, filepath.Join(root, "after.txt"))
}

func TestConcurrentCalls(t *testing.T) {
	root := writeTree(t, fixtureFiles)
	inst := newWatchedInstance(t, root)
	var batches atomic.Int32
	if _, err := inst.Watch("", WatchOptions{}, func([]WatchEvent) { batches.Add(1) }); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	errs := make(chan error, 6)
	var wg sync.WaitGroup
	run := func(call func(k int) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := 0; ; k++ {
				select {
				case <-stop:
					return
				default:
				}
				if err := call(k); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	defer func() {
		close(stop)
		wg.Wait()
	}()
	run(func(int) error { _, err := inst.Search(SearchQuery{Query: "two"}); return err })
	run(func(int) error { _, err := inst.Grep(GrepQuery{Query: "needle"}); return err })
	run(func(int) error { _, err := inst.SearchMixed(SearchQuery{Query: "three"}); return err })
	for range 2 {
		run(func(int) error {
			id, err := inst.Watch("**/*.txt", WatchOptions{}, func([]WatchEvent) {})
			if err != nil {
				return err
			}
			if existed, err := inst.Unwatch(id); err != nil || !existed {
				return fmt.Errorf("Unwatch(%d) = %v, %v", id, existed, err)
			}
			return nil
		})
	}
	run(func(k int) error {
		time.Sleep(time.Millisecond)
		return os.WriteFile(filepath.Join(root, fmt.Sprintf("w%d.txt", k%8)), []byte{byte(k)}, 0o644)
	})

	deadline := time.After(10 * time.Second)
	for batches.Load() < 10 {
		select {
		case err := <-errs:
			t.Fatal(err)
		case <-deadline:
			t.Fatalf("%d watch batches within 10s, want 10", batches.Load())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestCloseWaitsForWatchCallbacks(t *testing.T) {
	root := writeTree(t, map[string]string{"keep.txt": "keep\n"})
	inst := newWatchedInstance(t, root)
	entered, release, closed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	enter, releaseOnce := sync.OnceFunc(func() { close(entered) }), sync.OnceFunc(func() { close(release) })
	var returned atomic.Bool
	var id uint64
	late := make(chan [2]error, 1)
	id, err := inst.Watch("", WatchOptions{}, func([]WatchEvent) {
		enter()
		<-release
		_, watchErr := inst.Watch("", WatchOptions{}, func([]WatchEvent) {})
		_, unwatchErr := inst.Unwatch(id)
		select {
		case late <- [2]error{watchErr, unwatchErr}:
		default:
		}
		returned.Store(true)
	})
	if err != nil {
		t.Fatal(err)
	}
	if !rewriteUntil(t, filepath.Join(root, "hello.txt"), func() bool {
		select {
		case <-entered:
			return true
		default:
			return false
		}
	}) {
		t.Fatal("no watch callback within 10s")
	}

	var closeErr error
	go func() {
		defer close(closed)
		closeErr = inst.Close()
	}()
	// The cleanup Close must not overlap this one.
	defer func() {
		releaseOnce()
		<-closed
	}()
	select {
	case <-closed:
		t.Fatal("Close returned while a watch callback was running")
	case <-time.After(200 * time.Millisecond):
	}
	// The callback calls Watch and Unwatch once released, so Close must have marked the instance
	// closed.
	for deadline := time.Now().Add(10 * time.Second); !closedFlag(inst); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("Close did not mark the instance closed within 10s")
		}
	}
	releaseOnce()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return after the callback did")
	}
	if closeErr != nil || !returned.Load() {
		t.Fatalf("Close = %v, callback returned %v", closeErr, returned.Load())
	}
	if errs := <-late; !errors.Is(errs[0], ErrClosed) || !errors.Is(errs[1], ErrClosed) {
		t.Fatalf("Watch, Unwatch from a callback while Close waited = %v, %v, want ErrClosed",
			errs[0], errs[1])
	}
	inst.watches.mu.RLock()
	kept := len(inst.watches.fns)
	inst.watches.mu.RUnlock()
	if kept != 0 {
		t.Fatalf("%d watch callbacks kept after Close", kept)
	}
}

func TestWatchFromCallback(t *testing.T) {
	cases := []struct {
		name string
		call func(inst *Instance, self uint64, inner *eventLog) error
		// The call subscribed inner, which must then receive events.
		subscribes bool
	}{
		{name: "Unwatch itself", call: func(inst *Instance, self uint64, _ *eventLog) error {
			if existed, err := inst.Unwatch(self); err != nil || !existed {
				return fmt.Errorf("Unwatch(%d) = %v, %v", self, existed, err)
			}
			return nil
		}},
		{name: "Watch", subscribes: true, call: func(inst *Instance, _ uint64, inner *eventLog) error {
			_, err := inst.Watch("", WatchOptions{}, inner.add)
			return err
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := writeTree(t, map[string]string{"keep.txt": "keep\n"})
			inst, err := New(Options{BasePath: root, Watch: true})
			if err != nil {
				t.Fatal(err)
			}
			// Close waits for the callback, so one stuck in a deadlock must fail the test, not hang it.
			t.Cleanup(func() {
				closed := make(chan error, 1)
				go func() { closed <- inst.Close() }()
				select {
				case err := <-closed:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(10 * time.Second):
					t.Error("Close did not return within 10s")
				}
			})
			waitForScan(t, inst)
			if ready, err := inst.WaitForWatcher(10 * time.Second); err != nil || !ready {
				t.Fatalf("WaitForWatcher = %v, %v", ready, err)
			}

			ids, done := make(chan uint64, 1), make(chan error, 1)
			var fired atomic.Bool
			var inner eventLog
			id, err := inst.Watch("", WatchOptions{}, func([]WatchEvent) {
				if fired.CompareAndSwap(false, true) {
					done <- c.call(inst, <-ids, &inner)
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			ids <- id

			var callErr error
			if !rewriteUntil(t, filepath.Join(root, "hello.txt"), func() bool {
				select {
				case callErr = <-done:
					return true
				default:
					return false
				}
			}) {
				t.Fatal("no watch callback finished within 10s")
			}
			if callErr != nil {
				t.Fatal(callErr)
			}
			if c.subscribes {
				inner.writeUntilSeen(t, filepath.Join(root, "after.txt"))
			}
		})
	}
}

type eventLog struct {
	mu     sync.Mutex
	events []WatchEvent
}

func (l *eventLog) add(events []WatchEvent) {
	l.mu.Lock()
	l.events = append(l.events, events...)
	l.mu.Unlock()
}

func (l *eventLog) has(match func(WatchEvent) bool) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.ContainsFunc(l.events, match)
}

func (l *eventLog) waitFor(t *testing.T, match func(WatchEvent) bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		if l.has(match) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	t.Fatalf("no matching event in %+v", l.events)
}

func (l *eventLog) first(path string) WatchEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	if k := slices.IndexFunc(l.events, func(e WatchEvent) bool { return e.Path == path }); k >= 0 {
		return l.events[k]
	}
	return WatchEvent{}
}

func (l *eventLog) seen(paths []string) []string {
	var got []string
	for _, path := range paths {
		if l.has(func(e WatchEvent) bool { return e.Path == path }) {
			got = append(got, path)
		}
	}
	return got
}

func (l *eventLog) writeUntilSeen(t *testing.T, path string) {
	t.Helper()
	l.writeUntil(t, path, func(e WatchEvent) bool { return e.Path == path })
}

func (l *eventLog) writeUntil(t *testing.T, path string, match func(WatchEvent) bool) {
	t.Helper()
	if rewriteUntil(t, path, func() bool { return l.has(match) }) {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	t.Fatalf("no matching event after writing %s, got %+v", path, l.events)
}

func newWatchedInstance(t *testing.T, root string) *Instance {
	t.Helper()
	inst := newInstance(t, Options{BasePath: root, Watch: true})
	waitForScan(t, inst)
	if ready, err := inst.WaitForWatcher(10 * time.Second); err != nil || !ready {
		t.Fatalf("WaitForWatcher = %v, %v", ready, err)
	}
	return inst
}

func watch(t *testing.T, inst *Instance, pattern string, opts WatchOptions, log *eventLog) uint64 {
	t.Helper()
	id, err := inst.Watch(pattern, opts, log.add)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func closedFlag(inst *Instance) bool {
	inst.watches.mu.RLock()
	defer inst.watches.mu.RUnlock()
	return inst.watches.closed
}

func forgetWatch(inst *Instance, id uint64) {
	// Only the Go side forgets, so FFF keeps delivering batches for id.
	inst.watches.mu.Lock()
	delete(inst.watches.fns, id)
	inst.watches.mu.Unlock()
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func rewriteUntil(t *testing.T, path string, arrived func() bool) bool {
	t.Helper()
	// FSEvents can miss writes made just after its stream starts, so keep writing.
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		writeFile(t, path, time.Now().String())
		for range 50 {
			if arrived() {
				return true
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	return false
}
