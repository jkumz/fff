package fff

import (
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestAIMode(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	root := gitRepo(t, map[string]string{"edited.txt": "before\n"})
	edited := filepath.Join(root, "edited.txt")
	writeFile(t, edited, "after\n")
	// A modified file edited half an hour ago scores 7 on the editor scale, which decays over
	// a week, and 4 on the AI scale, which decays over four hours.
	old := time.Now().Add(-30 * time.Minute)
	if err := os.Chtimes(edited, old, old); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		ai   bool
		want int64
	}{{"editor", false, 7}, {"AI", true, 4}} {
		t.Run(c.name, func(t *testing.T) {
			inst := newInstance(t, Options{
				BasePath:       root,
				FrecencyDBPath: filepath.Join(t.TempDir(), "frecency"),
				AIMode:         c.ai,
			})
			waitForScan(t, inst)
			if _, err := inst.RefreshGitStatus(); err != nil {
				t.Fatal(err)
			}
			page, err := inst.Search(SearchQuery{Query: "edited"})
			if err != nil {
				t.Fatal(err)
			}
			item := at(t, page.Items, 0)
			if item.Path != "edited.txt" || item.GitStatus != "modified" || item.ModificationFrecency != c.want {
				t.Fatalf("item = %+v, want ModificationFrecency %d", item, c.want)
			}
		})
	}
}

func TestContentIndexing(t *testing.T) {
	root := writeTree(t, map[string]string{"a.txt": "zebra\n", "b.txt": "apple\n", "c.txt": "melon\n", "d.txt": "grape\n"})
	// The content index limits grep to the files holding every character pair of the query.
	for _, c := range []struct {
		name     string
		indexed  bool
		filtered uint32
	}{{"plain", false, 4}, {"indexed", true, 1}} {
		t.Run(c.name, func(t *testing.T) {
			inst := newInstance(t, Options{BasePath: root, EnableContentIndexing: c.indexed})
			waitForScan(t, inst)
			for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
				progress, err := inst.ScanProgress()
				if err != nil {
					t.Fatal(err)
				}
				if progress.IsWarmupComplete {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("content index not built within 10s")
				}
			}
			page, err := inst.Grep(GrepQuery{Query: "zebra"})
			if err != nil {
				t.Fatal(err)
			}
			if got := matchPaths(page.Matches); !slices.Equal(got, []string{"a.txt"}) || page.FilteredFiles != c.filtered {
				t.Fatalf("matches in %v from %d filtered files, want [a.txt] from %d", got, page.FilteredFiles, c.filtered)
			}
		})
	}
}

func TestCacheBudget(t *testing.T) {
	root := writeTree(t, map[string]string{"small.txt": "needle\n", "big.txt": "needle" + strings.Repeat(" padding", 16) + "\n"})
	// Grep skips files over CacheBudgetMaxFileSize. The other two limits only cap the persistent
	// content cache, which FFF does not report.
	for _, c := range []struct {
		name    string
		maxSize uint64
		want    []string
	}{{"default", 0, []string{"big.txt", "small.txt"}}, {"16 bytes", 16, []string{"small.txt"}}} {
		t.Run(c.name, func(t *testing.T) {
			inst := newInstance(t, Options{BasePath: root, CacheBudgetMaxFileSize: c.maxSize})
			waitForScan(t, inst)
			page, err := inst.Grep(GrepQuery{Query: "needle"})
			if err != nil {
				t.Fatal(err)
			}
			got := matchPaths(page.Matches)
			slices.Sort(got)
			if !slices.Equal(got, c.want) {
				t.Fatalf("matches in %v, want %v", got, c.want)
			}
		})
	}
}

func TestRootAndHomeScanning(t *testing.T) {
	// Indexing / with EnableFSRootScanning would walk the whole file system, so only the refusal is tested.
	const refusal = "file system root or home directories"
	if inst, err := New(Options{BasePath: "/"}); err == nil {
		inst.Close()
		t.Fatal("New indexed the file system root")
	} else if !strings.Contains(err.Error(), refusal) {
		t.Fatalf("New(/) err = %v", err)
	}

	home := writeTree(t, fixtureFiles)
	t.Setenv("HOME", home)
	if inst, err := New(Options{BasePath: home}); err == nil {
		inst.Close()
		t.Fatal("New indexed the home directory")
	} else if !strings.Contains(err.Error(), refusal) {
		t.Fatalf("New(home) err = %v", err)
	}
	inst := newInstance(t, Options{BasePath: home, EnableHomeDirScanning: true})
	waitForScan(t, inst)
	page, err := inst.Search(SearchQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := sortedPaths(page.Items), slices.Sorted(maps.Keys(fixtureFiles)); !slices.Equal(got, want) {
		t.Fatalf("indexed %v, want %v", got, want)
	}
}

// LogFilePath installs process-wide hooks, so the logging runs in a child test process.
func TestLogFile(t *testing.T) {
	if dir := os.Getenv("FFF_GO_LOG_DIR"); dir != "" {
		writeLog(t, dir, os.Getenv("FFF_GO_LOG_LEVEL"))
		return
	}
	for _, c := range []struct {
		level   string
		want    []string
		notWant []string
	}{
		{"info", []string{
			"FFF tracing initialized",
			"warmup=true, content_indexing=true, mode=Ai",
			"Refusing to index filesystem root",
		}, nil},
		{"error", []string{"Refusing to index filesystem root"}, []string{
			"FFF tracing initialized",
			"Spawning background threads",
		}},
	} {
		t.Run(c.level, func(t *testing.T) {
			dir := t.TempDir()
			cmd := exec.Command(os.Args[0], "-test.run=^TestLogFile$")
			cmd.Env = append(slices.DeleteFunc(os.Environ(), func(kv string) bool {
				return strings.HasPrefix(kv, "RUST_LOG=")
			}), "FFF_GO_LOG_DIR="+dir, "FFF_GO_LOG_LEVEL="+c.level)
			start := time.Now().Unix()
			out, err := cmd.CombinedOutput()
			end := time.Now().Unix()
			if err != nil {
				t.Fatalf("child test: %v\n%s", err, out)
			}

			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 {
				t.Fatalf("log directory holds %v, want one session log", entries)
			}
			name := entries[0].Name()
			var secs int64
			if _, err := fmt.Sscanf(name, "fff+%d+", &secs); err != nil || secs < start-1 || secs > end+1 ||
				name != fmt.Sprintf("fff+%d+%d.log", secs, cmd.Process.Pid) {
				t.Fatalf("session log %q, want fff+<unix secs>+%d.log", name, cmd.Process.Pid)
			}
			b, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range c.want {
				if !strings.Contains(string(b), s) {
					t.Errorf("log lacks %q:\n%s", s, b)
				}
			}
			for _, s := range c.notWant {
				if strings.Contains(string(b), s) {
					t.Errorf("log at %s level has %q:\n%s", c.level, s, b)
				}
			}
		})
	}
}

func writeLog(t *testing.T, dir, level string) {
	newInstance(t, Options{
		BasePath:              writeTree(t, fixtureFiles),
		LogFilePath:           filepath.Join(dir, "fff.log"),
		LogLevel:              level,
		EnableMmapCache:       true,
		EnableContentIndexing: true,
		AIMode:                true,
	})
	logs, err := filepath.Glob(filepath.Join(dir, "fff+*.log"))
	if err != nil || len(logs) != 1 {
		t.Fatalf("session logs = %v, %v", logs, err)
	}
	if inst, err := New(Options{BasePath: "/"}); err == nil {
		inst.Close()
		t.Fatal("New indexed the file system root")
	}
	// One thread writes the log in order, so once the refusal is there every earlier line is too.
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		b, err := os.ReadFile(logs[0])
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "Refusing to index filesystem root") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no root refusal logged within 10s:\n%s", b)
		}
	}
}
