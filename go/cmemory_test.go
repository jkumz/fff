package fff

import (
	"fmt"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/dmtrKovalenko/fff/go/internal/cprobe"
)

// Every call must free what FFF allocated for its result. One-time initialisation is
// allowed, so only growth that keeps rising with the call count counts as a leak.
func TestCallsFreeCMemory(t *testing.T) {
	if _, ok := cprobe.MallocInUse(); !ok {
		t.Skipf("no C heap probe on %s/%s: it needs macOS, or Linux with glibc 2.33 or later",
			runtime.GOOS, runtime.GOARCH)
	}
	// FFF and libgit2 keep per-thread state, allocated once on each new calling thread.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	watchedDir := writeTree(t, map[string]string{"w.txt": "w\n"})
	watched := newWatchedInstance(t, watchedDir)
	delivered := make(chan struct{}, 1)
	signal := func([]WatchEvent) {
		select {
		case delivered <- struct{}{}:
		default:
		}
	}
	deliver := func(path string) error {
		select {
		case <-delivered: // a late batch from the previous call
		default:
		}
		if !rewriteUntil(t, path, func() bool {
			select {
			case <-delivered:
				return true
			default:
				return false
			}
		}) {
			return fmt.Errorf("no watch batch for %s within 10s", path)
		}
		return nil
	}
	file := filepath.Join(watchedDir, "w.txt")
	restarted := newInstance(t, Options{BasePath: fixtureDir})
	waitForScan(t, restarted)

	calls := []struct {
		name string
		call func() error
	}{
		{"New/Close", func() error {
			inst, err := New(Options{BasePath: fixtureDir})
			if err != nil {
				return err
			}
			if _, err := inst.WaitForScan(10 * time.Second); err != nil {
				return err
			}
			return inst.Close()
		}},
		{"Search", func() error { _, err := fixture.Search(SearchQuery{Query: "two.txt:3"}); return err }},
		{"Glob", func() error { _, err := fixture.Glob(SearchQuery{Query: "**/*.txt"}); return err }},
		{"SearchDirectories", func() error { _, err := fixture.SearchDirectories(SearchQuery{}); return err }},
		{"SearchMixed", func() error { _, err := fixture.SearchMixed(SearchQuery{Query: "three"}); return err }},
		{"Grep", func() error {
			_, err := fixture.Grep(GrepQuery{Query: "haystack", GrepOptions: GrepOptions{BeforeContext: 1, AfterContext: 1}})
			return err
		}},
		{"GrepRegexFallback", func() error { _, err := fixture.Grep(GrepQuery{Query: "needle(", Mode: Regex}); return err }},
		{"MultiGrep", func() error {
			_, err := fixture.MultiGrep(MultiGrepQuery{Patterns: []string{"haystack", "needle"}, GrepOptions: GrepOptions{SmartCase: true}})
			return err
		}},
		{"MultiGrepOverlapping", func() error {
			_, err := fixture.MultiGrep(MultiGrepQuery{Patterns: []string{"need", "needle"}, Overlapping: true})
			return err
		}},
		{"MultiGrepError", func() error {
			if _, err := fixture.MultiGrep(MultiGrepQuery{}); err == nil {
				t.Fatal("MultiGrep without patterns succeeded")
			}
			return nil
		}},
		{"IsScanning", func() error { fixture.IsScanning(); return nil }},
		{"ScanFiles", func() error {
			if err := fixture.ScanFiles(); err != nil {
				return err
			}
			_, err := fixture.WaitForScan(10 * time.Second)
			return err
		}},
		{"ScanProgress", func() error { _, err := fixture.ScanProgress(); return err }},
		{"BasePath", func() error { _, err := fixture.BasePath(); return err }},
		{"RestartIndex", func() error {
			if err := restarted.RestartIndex(fixtureDir); err != nil {
				return err
			}
			_, err := restarted.WaitForScan(10 * time.Second)
			return err
		}},
		{"RefreshGitStatus", func() error { _, err := fixture.RefreshGitStatus(); return err }},
		{"TrackQuery", func() error {
			_, err := fixture.TrackQuery("thre", filepath.Join(fixtureDir, "c/three.go"))
			return err
		}},
		{"HistoricalQuery", func() error {
			if _, ok, err := fixture.HistoricalQuery(0); err != nil || !ok {
				t.Fatalf("HistoricalQuery(0) = %v, %v", ok, err)
			}
			return nil
		}},
		{"HealthCheck", func() error { _, err := fixture.HealthCheck(fixtureDir); return err }},
		{"package HealthCheck", func() error { _, err := HealthCheck(fixtureDir); return err }},
		{"WaitForWatcher", func() error { _, err := watched.WaitForWatcher(time.Second); return err }},
		{"Watch/Unwatch", func() error {
			id, err := watched.Watch("", WatchOptions{Ignore: []string{"*.log"}}, signal)
			if err != nil {
				return err
			}
			_, err = watched.Unwatch(id)
			return err
		}},
		{"WatchDeliver", func() error {
			id, err := watched.Watch("", WatchOptions{}, signal)
			if err != nil {
				return err
			}
			defer watched.Unwatch(id)
			return deliver(file)
		}},
		{"WatchDrop", func() error {
			// Batches reach subscriptions in order, so the dropped one is freed before signal runs.
			dropped, err := watched.Watch("", WatchOptions{}, signal)
			if err != nil {
				return err
			}
			defer watched.Unwatch(dropped)
			forgetWatch(watched, dropped)
			id, err := watched.Watch("", WatchOptions{}, signal)
			if err != nil {
				return err
			}
			defer watched.Unwatch(id)
			return deliver(file)
		}},
	}
	const k = 16
	for _, c := range calls {
		run := func(n int) {
			for range n {
				if err := c.call(); err != nil {
					t.Fatalf("%s: %v", c.name, err)
				}
			}
		}
		run(3)
		base := settledMallocInUse(t)
		run(k)
		first := settledMallocInUse(t) - base
		run(k)
		second := settledMallocInUse(t) - base
		// One leaked allocation per call makes the second growth at least k*MinLeak larger.
		if second-first >= k*cprobe.MinLeak {
			t.Errorf("%s: live C heap grew by %d %s after %d calls and by %d after %d",
				c.name, first, cprobe.Unit, k, second, 2*k)
		}
	}
}

func settledMallocInUse(t *testing.T) int64 {
	// Closed instances free on their own threads and watch batches arrive late, so wait
	// until the heap holds still for 100ms.
	prev, _ := cprobe.MallocInUse()
	for stable, n := 0, 0; n < 250; n++ {
		time.Sleep(20 * time.Millisecond)
		cur, _ := cprobe.MallocInUse()
		if cur != prev {
			stable = 0
		} else if stable++; stable == 5 {
			return int64(cur)
		}
		prev = cur
	}
	t.Fatal("C heap did not settle within 5s")
	return 0
}
