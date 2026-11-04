package fff

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"
)

// Every numeric field holds a distinct nonzero value somewhere below, so swapping two in a
// mapping fails. Base and FuzzyScore are opaque fuzzy scores, so they are read back, not pinned.
func TestFieldMapping(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	// notes/zz makes the file count differ from the directory count.
	root := gitRepo(t, map[string]string{"notes/edited.txt": "draft\n", "notes/zz": "z\n", "pkgdir/main.go": "package main\n"})
	state := t.TempDir()
	inst := newInstance(t, Options{
		BasePath: root, Watch: true, AIMode: true,
		FrecencyDBPath: filepath.Join(state, "frecency"), HistoryDBPath: filepath.Join(state, "history"),
	})
	waitForScan(t, inst)
	if ready, err := inst.WaitForWatcher(10 * time.Second); err != nil || !ready {
		t.Fatalf("WaitForWatcher = %v, %v", ready, err)
	}

	// AI mode tracks one access per watched write. Refreshing git status then scores the modified
	// file: access 1, and modification 16 as it changed within 30 s.
	edited := filepath.Join(root, "notes/edited.txt")
	const content = "first line\nedited by the watcher\n"
	if !rewriteWith(t, edited, content, func() bool {
		page, err := inst.Search(SearchQuery{Query: "edited.txt"})
		return err == nil && len(page.Items) == 1 && page.Items[0].AccessFrecency != 0
	}) {
		t.Fatal("AI mode tracked no access for the written file")
	}
	if _, err := inst.RefreshGitStatus(); err != nil {
		t.Fatal(err)
	}
	if tracked, err := inst.TrackQuery("edited.txt", edited); err != nil || !tracked {
		t.Fatalf("TrackQuery = %v, %v", tracked, err)
	}
	info, err := os.Stat(edited)
	if err != nil {
		t.Fatal(err)
	}
	file := FileItem{
		Path: "notes/edited.txt", FileName: "edited.txt", GitStatus: "modified", Size: uint64(len(content)),
		Modified: uint64(info.ModTime().Unix()), AccessFrecency: 1, ModificationFrecency: 16, TotalFrecency: 17,
	}

	// Expected scores follow fff-core's scoring from the base score. Total also holds two hidden
	// terms: git recency, 1 for a file in the only commit, and 15% of base for a modified file.
	for _, c := range []struct {
		q    SearchQuery
		path string
		want func(b int32) Score
	}{
		{SearchQuery{Query: "edited.txt", CurrentFile: "pkgdir/main.go", MinComboCount: 1}, file.Path, func(b int32) Score {
			return Score{
				Total: b + b/5*2 + b*17/100 + b*15/100 + 1 - 1 + 100, Base: b, FilenameBonus: b / 5 * 2,
				FrecencyBoost: b * 17 / 100, DistancePenalty: -1, ComboMatchBoost: 100,
				ExactMatch: true, MatchType: "exact_filename",
			}
		}},
		{SearchQuery{Query: "edited.txt", CurrentFile: file.Path}, file.Path, func(b int32) Score {
			return Score{
				Total: b + b/5*2 + b*17/100 + b*15/100 + 1 - b/4, Base: b, FilenameBonus: b / 5 * 2,
				FrecencyBoost: b * 17 / 100, CurrentFilePenalty: -(b / 4),
				ExactMatch: true, MatchType: "exact_filename",
			}
		}},
		{SearchQuery{Query: "pkgdir"}, "pkgdir/main.go", func(b int32) Score {
			return Score{
				Total: b + b*5/100 + 1, Base: b, FilenameBonus: b * 5 / 100, SpecialFilenameBonus: b * 5 / 100,
				MatchType: "fuzzy_path",
			}
		}},
		{SearchQuery{Query: "notes/edited.txt"}, file.Path, func(b int32) Score {
			return Score{
				Total: b + b*17/100 + b*15/100 + 1 + b, Base: b, FrecencyBoost: b * 17 / 100,
				PathAlignmentBonus: b, ExactMatch: true, MatchType: "exact_path",
			}
		}},
	} {
		page, err := inst.Search(c.q)
		if err != nil {
			t.Fatal(err)
		}
		item, score := at(t, page.Items, 0), at(t, page.Scores, 0)
		if item.Path != c.path || score.Base <= 0 || score != c.want(score.Base) {
			t.Errorf("Search(%+v) = %s %+v, want %+v", c.q, item.Path, score, c.want(score.Base))
		}
	}

	page, err := inst.Search(SearchQuery{Query: "edited.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if got := at(t, page.Items, 0); got != file || page.TotalMatched != 1 || page.TotalFiles != 3 {
		t.Errorf("Search item = %+v, matched %d of %d, want %+v, 1 of 3", got, page.TotalMatched, page.TotalFiles, file)
	}
	dirs, err := inst.SearchDirectories(SearchQuery{Query: "notes"})
	if err != nil {
		t.Fatal(err)
	}
	wantDir := DirItem{Path: "notes/", Name: "notes/", MaxAccessFrecency: 1}
	if got := at(t, dirs.Items, 0); got != wantDir || dirs.TotalMatched != 1 || dirs.TotalDirs != 2 {
		t.Errorf("SearchDirectories item = %+v, matched %d of %d, want %+v, 1 of 2", got, dirs.TotalMatched, dirs.TotalDirs, wantDir)
	}
	mixed, err := inst.SearchMixed(SearchQuery{Query: "edited"})
	if err != nil {
		t.Fatal(err)
	}
	if got := at(t, mixed.Items, 0); got != (MixedItem{FileItem: file}) || mixed.TotalMatched != 1 || mixed.TotalFiles != 3 || mixed.TotalDirs != 2 {
		t.Errorf("SearchMixed = %+v, want %+v, 1 of 3 files and 2 dirs", mixed, file)
	}

	grep, err := inst.Grep(GrepQuery{Query: "watcher", Mode: Fuzzy})
	if err != nil {
		t.Fatal(err)
	}
	m := at(t, grep.Matches, 0)
	want := GrepMatch{
		Path: file.Path, FileName: file.FileName, GitStatus: file.GitStatus, Size: file.Size, Modified: file.Modified,
		TotalFrecency: 17, AccessFrecency: 1, ModificationFrecency: 16, Line: "edited by the watcher", LineNumber: 2,
		LineStart: 11, Col: 14, Ranges: []MatchRange{{14, 21}}, ContextBefore: []string{}, ContextAfter: []string{},
		FuzzyScore: m.FuzzyScore, HasFuzzyScore: true,
	}
	if m.FuzzyScore == 0 || slices.Contains([]uint64{want.Size, 17, 1, 16, 2, 11, 14}, uint64(m.FuzzyScore)) ||
		!reflect.DeepEqual(m, want) {
		t.Errorf("Grep match = %+v, want %+v", m, want)
	}
}

func rewriteWith(t *testing.T, path, content string, arrived func() bool) bool {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		writeFile(t, path, content)
		for range 50 {
			if arrived() {
				return true
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	return false
}
