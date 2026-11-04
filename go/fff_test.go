package fff

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
	"unsafe"
)

var fixtureFiles = map[string]string{
	"a/one.txt":  "first line\n    needle in one\n", // indented match pins byte offsets
	"b/two.txt":  "needle at start\nhaystack\nanother needle here\n",
	"c/three.go": "package three\n\nfunc Needle() {}\n", // capitalised match exercises smart case
	"README.md":  "# Fixture\n",
}

var (
	fixture    *Instance
	fixtureDir string
)

func TestMain(m *testing.M) {
	code, err := runWithFixture(m)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}

func TestNewRejectsMissingBasePath(t *testing.T) {
	_, err := New(Options{BasePath: filepath.Join(t.TempDir(), "missing")})
	if err == nil {
		t.Fatal("New succeeded for a missing base path")
	}
}

func TestCloseTwice(t *testing.T) {
	inst := newInstance(t, Options{BasePath: writeTree(t, fixtureFiles)})
	if err := inst.Close(); err != nil {
		t.Fatal(err)
	}
	if err := inst.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSearch(t *testing.T) {
	page, err := fixture.Search(SearchQuery{Query: "two"})
	if err != nil {
		t.Fatal(err)
	}
	want := FileItem{Path: "b/two.txt", FileName: "two.txt", GitStatus: "clean", Size: 45}
	got := at(t, page.Items, 0)
	got.Modified = 0
	if got != want {
		t.Fatalf("first item = %+v, want %+v", got, want)
	}
	if len(page.Scores) != len(page.Items) || page.TotalFiles != 4 || page.Location != (Location{}) {
		t.Fatalf("page = %+v", page)
	}
}

func TestSearchLocation(t *testing.T) {
	tests := []struct {
		query string
		want  Location
	}{
		{"two.txt:3", Location{Kind: LineLocation, Line: 3}},
		{"two.txt:3:4", Location{Kind: PositionLocation, Line: 3, Col: 4}},
		{"two.txt:3:4-5:6", Location{Kind: RangeLocation, Line: 3, Col: 4, EndLine: 5, EndCol: 6}},
	}
	for _, tt := range tests {
		page, err := fixture.Search(SearchQuery{Query: tt.query})
		if err != nil {
			t.Fatal(err)
		}
		if page.Location != tt.want {
			t.Errorf("%q: location = %+v, want %+v", tt.query, page.Location, tt.want)
		}
		if path := at(t, page.Items, 0).Path; path != "b/two.txt" {
			t.Errorf("%q: first path = %q", tt.query, path)
		}
		mixed, err := fixture.SearchMixed(SearchQuery{Query: tt.query})
		if err != nil {
			t.Fatal(err)
		}
		if mixed.Location != tt.want {
			t.Errorf("%q: mixed location = %+v, want %+v", tt.query, mixed.Location, tt.want)
		}
	}
}

func TestCurrentFile(t *testing.T) {
	inst := newInstance(t, Options{BasePath: writeTree(t, fixtureFiles)})
	waitForScan(t, inst)
	tests := []struct {
		current   string
		want      []string
		penalised []string
	}{
		{"", []string{"a/one.txt", "b/two.txt", "c/three.go"}, nil},
		{"a/one.txt", []string{"b/two.txt", "a/one.txt", "c/three.go"}, []string{"a/one.txt"}},
	}
	for _, tt := range tests {
		page, err := inst.Search(SearchQuery{Query: "txt", CurrentFile: tt.current})
		if err != nil {
			t.Fatal(err)
		}
		var paths, penalised []string
		for k, item := range page.Items {
			paths = append(paths, item.Path)
			if at(t, page.Scores, k).CurrentFilePenalty < 0 {
				penalised = append(penalised, item.Path)
			}
		}
		if !slices.Equal(paths, tt.want) || !slices.Equal(penalised, tt.penalised) {
			t.Errorf("current %q: paths = %q, penalised %q, want %q, %q", tt.current, paths, penalised, tt.want, tt.penalised)
		}
	}

	// Glob penalises the current file by its frecency, which a git-modified file has.
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	const current = "near/widgets/a.txt"
	root := gitRepo(t, map[string]string{current: "a\n", "far/widgets/b.txt": "b\n"})
	writeFile(t, filepath.Join(root, current), "edited\n")
	repo := newInstance(t, Options{BasePath: root, FrecencyDBPath: filepath.Join(t.TempDir(), "frecency")})
	waitForScan(t, repo)
	if _, err := repo.RefreshGitStatus(); err != nil {
		t.Fatal(err)
	}
	filePath := func(f FileItem) string { return f.Path }
	for _, m := range []struct {
		name string
		run  func(q SearchQuery) ([]string, error)
		want []string // penalised for distance or for being the current file
	}{
		{"Search", func(q SearchQuery) ([]string, error) {
			page, err := repo.Search(q)
			return penalised(t, page.Items, page.Scores, filePath), err
		}, []string{"far/widgets/b.txt", current}},
		{"Glob", func(q SearchQuery) ([]string, error) {
			q.Query = "**/*.txt"
			page, err := repo.Glob(q)
			return penalised(t, page.Items, page.Scores, filePath), err
		}, []string{current}},
		{"SearchDirectories", func(q SearchQuery) ([]string, error) {
			page, err := repo.SearchDirectories(q)
			return penalised(t, page.Items, page.Scores, func(d DirItem) string { return d.Path }), err
		}, []string{"far/widgets/"}},
		{"SearchMixed", func(q SearchQuery) ([]string, error) {
			page, err := repo.SearchMixed(q)
			return penalised(t, page.Items, page.Scores, func(m MixedItem) string { return m.Path }), err
		}, []string{"far/widgets/", "far/widgets/b.txt", current}},
	} {
		for _, file := range []string{"", current} {
			got, err := m.run(SearchQuery{Query: "widgets", CurrentFile: file})
			if err != nil {
				t.Fatal(err)
			}
			want := m.want
			if file == "" {
				want = nil
			}
			if !slices.Equal(got, want) {
				t.Errorf("%s, current %q: penalised %q, want %q", m.name, file, got, want)
			}
		}
	}
}

func TestComboBoost(t *testing.T) {
	root := writeTree(t, fixtureFiles)
	inst := newInstance(t, Options{BasePath: root, HistoryDBPath: filepath.Join(t.TempDir(), "history")})
	waitForScan(t, inst)
	for range 2 {
		if _, err := inst.TrackQuery("thre", filepath.Join(root, "c/three.go")); err != nil {
			t.Fatal(err)
		}
	}
	// "thre" opened c/three.go twice. Zero selects a multiplier of 100 and a minimum of 3 opens.
	for _, c := range []struct {
		multiplier int32
		min        uint32
		want       int32
	}{{7, 2, 14}, {7, 3, 0}, {0, 2, 200}, {7, 0, 0}} {
		q := SearchQuery{Query: "thre", ComboBoostMultiplier: c.multiplier, MinComboCount: c.min}
		page, err := inst.Search(q)
		if err != nil {
			t.Fatal(err)
		}
		mixed, err := inst.SearchMixed(q)
		if err != nil {
			t.Fatal(err)
		}
		file, item := at(t, page.Items, 0), at(t, mixed.Items, 0)
		got := []int32{at(t, page.Scores, 0).ComboMatchBoost, at(t, mixed.Scores, 0).ComboMatchBoost}
		if file.Path != "c/three.go" || item.Path != "c/three.go" || !slices.Equal(got, []int32{c.want, c.want}) {
			t.Errorf("multiplier %d, min %d: first %q and %q boosted %v, want c/three.go boosted %d",
				c.multiplier, c.min, file.Path, item.Path, got, c.want)
		}
	}
}

func TestFollowSymlinks(t *testing.T) {
	outside := writeTree(t, map[string]string{"outside.txt": "o\n"})
	root := writeTree(t, map[string]string{"real/x.txt": "x\n", "top.txt": "t\n"})
	for link, target := range map[string]string{"linkdir": "real", "linkfile.txt": "top.txt", "extdir": outside} {
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		follow bool
		want   []string
	}{
		{false, []string{"real/x.txt", "top.txt"}},
		// A link back into the tree is not indexed a second time.
		{true, []string{"extdir/outside.txt", "linkfile.txt", "real/x.txt", "top.txt"}},
	}
	for _, tt := range tests {
		inst := newInstance(t, Options{BasePath: root, FollowSymlinks: tt.follow})
		waitForScan(t, inst)
		page, err := inst.Search(SearchQuery{})
		if err != nil {
			t.Fatal(err)
		}
		if got := sortedPaths(page.Items); !slices.Equal(got, tt.want) {
			t.Errorf("FollowSymlinks %v: paths = %q, want %q", tt.follow, got, tt.want)
		}
	}
}

func TestBinaryFiles(t *testing.T) {
	inst := newInstance(t, Options{BasePath: writeTree(t, map[string]string{"blob.bin": "needle\x00\x01\x02", "text.txt": "needle\n"})})
	waitForScan(t, inst)
	page, err := inst.Search(SearchQuery{})
	if err != nil {
		t.Fatal(err)
	}
	binary := map[string]bool{}
	for _, item := range page.Items {
		binary[item.Path] = item.IsBinary
	}
	if want := map[string]bool{"blob.bin": true, "text.txt": false}; !maps.Equal(binary, want) {
		t.Fatalf("IsBinary = %v, want %v", binary, want)
	}
	// Grep skips binary files, so GrepMatch.IsBinary is never set.
	grep, err := inst.Grep(GrepQuery{Query: "needle"})
	if err != nil {
		t.Fatal(err)
	}
	if got := matchPaths(grep.Matches); !slices.Equal(got, []string{"text.txt"}) {
		t.Fatalf("grep paths = %q", got)
	}
}

func TestGlob(t *testing.T) {
	page, err := fixture.Glob(SearchQuery{Query: "**/*.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := sortedPaths(page.Items), []string{"a/one.txt", "b/two.txt"}; !slices.Equal(got, want) {
		t.Fatalf("paths = %q, want %q", got, want)
	}
}

func TestSearchDirectories(t *testing.T) {
	page, err := fixture.SearchDirectories(SearchQuery{})
	if err != nil {
		t.Fatal(err)
	}
	want := []DirItem{{Path: "", Name: ""}, {Path: "a/", Name: "a/"}, {Path: "b/", Name: "b/"}, {Path: "c/", Name: "c/"}}
	if !slices.Equal(page.Items, want) || page.TotalDirs != 4 || at(t, page.Scores, 0).MatchType != "frecency" {
		t.Fatalf("page = %+v, want items %+v", page, want)
	}
}

func TestSearchMixed(t *testing.T) {
	page, err := fixture.SearchMixed(SearchQuery{Query: "three"})
	if err != nil {
		t.Fatal(err)
	}
	got := at(t, page.Items, 0)
	got.Modified = 0
	want := MixedItem{FileItem: FileItem{Path: "c/three.go", FileName: "three.go", GitStatus: "clean", Size: 32}}
	if got != want {
		t.Fatalf("first item = %+v, want %+v", got, want)
	}
	if page.TotalFiles != 4 || page.TotalDirs != 4 {
		t.Fatalf("page = %+v", page)
	}

	page, err = fixture.SearchMixed(SearchQuery{})
	if err != nil {
		t.Fatal(err)
	}
	var dirs []MixedItem
	for _, item := range page.Items {
		if item.IsDir {
			dirs = append(dirs, item)
		}
	}
	wantDirs := []MixedItem{
		{IsDir: true},
		{IsDir: true, FileItem: FileItem{Path: "a/", FileName: "a/"}},
		{IsDir: true, FileItem: FileItem{Path: "b/", FileName: "b/"}},
		{IsDir: true, FileItem: FileItem{Path: "c/", FileName: "c/"}},
	}
	if len(page.Items) != 8 || !slices.Equal(dirs, wantDirs) {
		t.Fatalf("items = %+v, want dirs %+v", page.Items, wantDirs)
	}
}

func TestGrepOffsets(t *testing.T) {
	page, err := fixture.Grep(GrepQuery{Query: "needle", GrepOptions: GrepOptions{SmartCase: true}})
	if err != nil {
		t.Fatal(err)
	}
	want := []lineMatch{
		{"a/one.txt", 2, 11, "    needle in one", []MatchRange{{4, 10}}},
		{"b/two.txt", 1, 0, "needle at start", []MatchRange{{0, 6}}},
		{"b/two.txt", 3, 25, "another needle here", []MatchRange{{8, 14}}},
		{"c/three.go", 3, 15, "func Needle() {}", []MatchRange{{5, 11}}},
	}
	if got := lineMatches(page.Matches); !reflect.DeepEqual(got, want) {
		t.Fatalf("matches = %+v, want %+v", got, want)
	}
	if page.NextFileOffset != 0 || page.TotalFiles != 4 {
		t.Fatalf("page = %+v", page)
	}

	// LineStart plus a range start is the file offset of the matched bytes.
	m := page.Matches[0]
	content := fixtureFiles[m.Path]
	if got := content[m.LineStart+uint64(m.Ranges[0].Start) : m.LineStart+uint64(m.Ranges[0].End)]; got != "needle" {
		t.Fatalf("bytes at range = %q", got)
	}
	if page.Matches[1].Path != page.Matches[2].Path || !samePointer(page.Matches[1].Path, page.Matches[2].Path) {
		t.Fatal("matches in one file do not share a Path string")
	}
}

func TestGrepSmartCase(t *testing.T) {
	page, err := fixture.Grep(GrepQuery{Query: "Needle", GrepOptions: GrepOptions{SmartCase: true}})
	if err != nil {
		t.Fatal(err)
	}
	want := []lineMatch{{"c/three.go", 3, 15, "func Needle() {}", []MatchRange{{5, 11}}}}
	if got := lineMatches(page.Matches); !reflect.DeepEqual(got, want) {
		t.Fatalf("matches = %+v, want %+v", got, want)
	}
}

func TestGrepContext(t *testing.T) {
	// One side at a time, so swapped before and after counts show.
	for _, opts := range []GrepOptions{{BeforeContext: 1}, {AfterContext: 1}} {
		page, err := fixture.Grep(GrepQuery{Query: "haystack", GrepOptions: opts})
		if err != nil {
			t.Fatal(err)
		}
		var before, after []string
		if opts.BeforeContext > 0 {
			before = []string{"needle at start"}
		} else {
			after = []string{"another needle here"}
		}
		m := at(t, page.Matches, 0)
		if !slices.Equal(m.ContextBefore, before) || !slices.Equal(m.ContextAfter, after) {
			t.Fatalf("%+v: context = %q / %q", opts, m.ContextBefore, m.ContextAfter)
		}
	}
}

func TestGrepMatchFields(t *testing.T) {
	page, err := fixture.Grep(GrepQuery{Query: "stack", GrepOptions: GrepOptions{BeforeContext: 1, AfterContext: 1}})
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(fixtureDir, "b/two.txt"))
	if err != nil {
		t.Fatal(err)
	}
	want := []GrepMatch{{
		Path: "b/two.txt", FileName: "two.txt", GitStatus: "clean", Size: 45, Modified: uint64(info.ModTime().Unix()),
		Line: "haystack", LineNumber: 2, LineStart: 16, Col: 3, Ranges: []MatchRange{{3, 8}},
		ContextBefore: []string{"needle at start"}, ContextAfter: []string{"another needle here"},
	}}
	if !reflect.DeepEqual(page.Matches, want) {
		t.Fatalf("matches = %+v, want %+v", page.Matches, want)
	}
}

func TestGrepRegexFallback(t *testing.T) {
	page, err := fixture.Grep(GrepQuery{Query: "needle(", Mode: Regex})
	if err != nil {
		t.Fatal(err)
	}
	if page.RegexFallbackError == "" || len(page.Matches) != 0 {
		t.Fatalf("page = %+v", page)
	}
}

func TestGrepFuzzy(t *testing.T) {
	page, err := fixture.Grep(GrepQuery{Query: "hystck", Mode: Fuzzy})
	if err != nil {
		t.Fatal(err)
	}
	m := at(t, page.Matches, 0)
	if m.Path != "b/two.txt" || m.LineNumber != 2 || !m.HasFuzzyScore {
		t.Fatalf("match = %+v", m)
	}
}

func TestGrepDefinitions(t *testing.T) {
	page, err := fixture.Grep(GrepQuery{Query: "Needle", GrepOptions: GrepOptions{Definitions: DefinitionsClassify}})
	if err != nil {
		t.Fatal(err)
	}
	if m := at(t, page.Matches, 0); m.Path != "c/three.go" || !m.IsDefinition {
		t.Fatalf("match = %+v", m)
	}
}

func TestGrepRejectsUnknownDefinitionMode(t *testing.T) {
	_, err := fixture.Grep(GrepQuery{Query: "needle", GrepOptions: GrepOptions{Definitions: 9}})
	want := "fff: classify_definitions must be 0 (off), 1 (classify), 2 (definition_only), or 3 (usage_only)"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v", err)
	}
}

func TestGrepRejectsUnknownMode(t *testing.T) {
	if _, err := fixture.Grep(GrepQuery{Query: "needle", Mode: Fuzzy + 1}); !errors.Is(err, errGrepMode) {
		t.Fatalf("err = %v", err)
	}
}

func TestMultiGrepPaging(t *testing.T) {
	q := MultiGrepQuery{Patterns: []string{"haystack", "needle"}, GrepOptions: GrepOptions{PageLimit: 1, SmartCase: true}}
	var pages [][]lineMatch
	for {
		if len(pages) == 10 {
			t.Fatalf("no last page after %d pages: %+v", len(pages), pages)
		}
		page, err := fixture.MultiGrep(q)
		if err != nil {
			t.Fatal(err)
		}
		pages = append(pages, lineMatches(page.Matches))
		if page.NextFileOffset == 0 {
			break
		}
		q.FileOffset = page.NextFileOffset
	}
	want := [][]lineMatch{
		{{"a/one.txt", 2, 11, "    needle in one", []MatchRange{{4, 10}}}},
		{
			{"b/two.txt", 1, 0, "needle at start", []MatchRange{{0, 6}}},
			{"b/two.txt", 2, 16, "haystack", []MatchRange{{0, 8}}},
			{"b/two.txt", 3, 25, "another needle here", []MatchRange{{8, 14}}},
		},
		{{"c/three.go", 3, 15, "func Needle() {}", []MatchRange{{5, 11}}}},
	}
	if !reflect.DeepEqual(pages, want) {
		t.Fatalf("pages = %+v, want %+v", pages, want)
	}
}

func TestMultiGrepConstraints(t *testing.T) {
	page, err := fixture.MultiGrep(MultiGrepQuery{Patterns: []string{"needle"}, Constraints: "*.go", GrepOptions: GrepOptions{SmartCase: true}})
	if err != nil {
		t.Fatal(err)
	}
	if got := lineMatches(page.Matches); len(got) != 1 || got[0].Path != "c/three.go" {
		t.Fatalf("matches = %+v", got)
	}
}

func TestMultiGrepRejectsNoPatterns(t *testing.T) {
	_, err := fixture.MultiGrep(MultiGrepQuery{})
	if err == nil || err.Error() != "fff: patterns_joined is null or empty" {
		t.Fatalf("err = %v", err)
	}
}

func TestScanState(t *testing.T) {
	if err := fixture.ScanFiles(); err != nil {
		t.Fatal(err)
	}
	if done, err := fixture.WaitForScan(10 * time.Second); err != nil || !done {
		t.Fatalf("WaitForScan = %v, %v", done, err)
	}
	if fixture.IsScanning() {
		t.Fatal("still scanning after WaitForScan")
	}
	progress, err := fixture.ScanProgress()
	if err != nil {
		t.Fatal(err)
	}
	if progress.ScannedFiles != 4 || progress.IsScanning || progress.IsWatcherReady {
		t.Fatalf("progress = %+v", progress)
	}
	watched := newWatchedInstance(t, writeTree(t, fixtureFiles))
	if progress, err := watched.ScanProgress(); err != nil || !progress.IsWatcherReady {
		t.Fatalf("watched progress = %+v, %v", progress, err)
	}
}

func TestBasePath(t *testing.T) {
	got, err := fixture.BasePath()
	if err != nil {
		t.Fatal(err)
	}
	if got != fixtureDir {
		t.Fatalf("base path = %q, want %q", got, fixtureDir)
	}
}

func TestRefreshGitStatus(t *testing.T) {
	n, err := fixture.RefreshGitStatus()
	if err != nil || n != 0 {
		t.Fatalf("RefreshGitStatus = %d, %v", n, err)
	}
}

func TestQueryHistory(t *testing.T) {
	tracked, err := fixture.TrackQuery("thre", filepath.Join(fixtureDir, "c/three.go"))
	if err != nil || !tracked {
		t.Fatalf("TrackQuery = %v, %v", tracked, err)
	}
	query, ok, err := fixture.HistoricalQuery(0)
	if err != nil || !ok || query != "thre" {
		t.Fatalf("HistoricalQuery(0) = %q, %v, %v", query, ok, err)
	}
	if query, ok, err := fixture.HistoricalQuery(1 << 20); err != nil || ok {
		t.Fatalf("HistoricalQuery past the end = %q, %v, %v", query, ok, err)
	}
}

func TestHealthCheck(t *testing.T) {
	h, err := fixture.HealthCheck(fixtureDir)
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(filepath.Dir(fixtureDir), "state")
	// The fixture is not in a git repository, so the git check fills Error.
	if h.Version == "" || h.Git.LibGit2Version == "" || !h.Git.Available || h.Git.RepositoryFound ||
		h.Git.Workdir != "" || h.Git.Error == "" ||
		!h.FilePicker.Initialized || h.FilePicker.BasePath != fixtureDir || h.FilePicker.IndexedFiles != 4 ||
		!h.Frecency.Initialized || h.Frecency.DB.Path != filepath.Join(state, "frecency") || h.Frecency.DB.DiskSize == 0 ||
		!h.QueryTracker.Initialized || h.QueryTracker.DB.Path != filepath.Join(state, "history") || h.QueryTracker.DB.DiskSize == 0 {
		t.Fatalf("health = %+v", h)
	}

	h, err = HealthCheck(fixtureDir)
	if err != nil {
		t.Fatal(err)
	}
	if h.Version == "" || h.FilePicker.Initialized || h.Frecency.Initialized {
		t.Fatalf("health without instance = %+v", h)
	}
}

func TestRestartIndex(t *testing.T) {
	inst := newInstance(t, Options{BasePath: writeTree(t, fixtureFiles)})
	waitForScan(t, inst)
	next := writeTree(t, map[string]string{"moved/only.txt": "x\n"})
	if err := inst.RestartIndex(next); err != nil {
		t.Fatal(err)
	}
	waitForScan(t, inst)
	page, err := inst.Search(SearchQuery{Query: "only"})
	if err != nil {
		t.Fatal(err)
	}
	if got := sortedPaths(page.Items); !slices.Equal(got, []string{"moved/only.txt"}) || page.TotalFiles != 1 {
		t.Fatalf("paths after restart = %q of %d files", got, page.TotalFiles)
	}
	if base, err := inst.BasePath(); err != nil || base != next {
		t.Fatalf("BasePath after restart = %q, %v", base, err)
	}
	if err := inst.RestartIndex(filepath.Join(next, "missing")); err == nil {
		t.Fatal("RestartIndex succeeded for a missing path")
	}
}

func TestSearchPaging(t *testing.T) {
	searches := map[string]func(SearchQuery) (SearchPage, error){"Search": fixture.Search, "Glob": fixture.Glob}
	for name, search := range searches {
		var paths []string
		for skip := range uint32(4) {
			q := SearchQuery{PageIndex: skip, PageSize: 1}
			if name == "Glob" {
				q.Query = "**/*"
			}
			page, err := search(q)
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Items) != 1 {
				t.Fatalf("%s page %d = %+v", name, skip, page.Items)
			}
			paths = append(paths, page.Items[0].Path)
		}
		slices.Sort(paths)
		if want := []string{"README.md", "a/one.txt", "b/two.txt", "c/three.go"}; !slices.Equal(paths, want) {
			t.Fatalf("%s paths = %q, want %q", name, paths, want)
		}
	}

	page, err := fixture.SearchDirectories(SearchQuery{PageIndex: 1, PageSize: 3})
	if err != nil {
		t.Fatal(err)
	}
	if want := []DirItem{{Path: "a/", Name: "a/"}, {Path: "b/", Name: "b/"}, {Path: "c/", Name: "c/"}}; !slices.Equal(page.Items, want) {
		t.Fatalf("directories = %+v, want %+v", page.Items, want)
	}
}

func TestGrepLimits(t *testing.T) {
	tests := []struct {
		opts GrepOptions
		want []string
	}{
		{GrepOptions{SmartCase: true, MaxMatchesPerFile: 1}, []string{"a/one.txt", "b/two.txt", "c/three.go"}},
		{GrepOptions{SmartCase: true, MaxFileSize: 40}, []string{"a/one.txt", "c/three.go"}}, // b/two.txt is 45 bytes
		{GrepOptions{SmartCase: true, Definitions: DefinitionsOnly}, []string{"c/three.go"}},
		{GrepOptions{SmartCase: true, Definitions: UsagesOnly}, []string{"a/one.txt", "b/two.txt", "b/two.txt"}},
	}
	for _, tt := range tests {
		page, err := fixture.Grep(GrepQuery{Query: "needle", GrepOptions: tt.opts})
		if err != nil {
			t.Fatal(err)
		}
		if got := matchPaths(page.Matches); !slices.Equal(got, tt.want) {
			t.Errorf("%+v: paths = %q, want %q", tt.opts, got, tt.want)
		}
	}
}

func TestGrepTimeBudget(t *testing.T) {
	files := make(map[string]string, 3000)
	filler := strings.Repeat("x", 4096) + "\n"
	for k := range 3000 {
		files[fmt.Sprintf("f%04d.txt", k)] = filler
	}
	inst := newInstance(t, Options{BasePath: writeTree(t, files)})
	waitForScan(t, inst)

	// A budget below a millisecond still applies, and without enforcement a search
	// that finds nothing ignores it.
	q := GrepQuery{Query: "absent", GrepOptions: GrepOptions{TimeBudget: time.Microsecond, EnforceTimeBudget: true}}
	page, err := inst.Grep(q)
	if err != nil {
		t.Fatal(err)
	}
	if page.NextFileOffset == 0 || page.FilesSearched >= page.FilteredFiles {
		t.Fatalf("enforced budget: page = %+v", page)
	}
	q.EnforceTimeBudget = false
	if page, err = inst.Grep(q); err != nil {
		t.Fatal(err)
	}
	if page.NextFileOffset != 0 || page.FilesSearched != page.FilteredFiles {
		t.Fatalf("unenforced budget: page = %+v", page)
	}
}

func TestWaitTimeouts(t *testing.T) {
	// Without a watcher the wait runs to its timeout, so a negative one must not wrap around.
	waits := []struct {
		name string
		wait func(time.Duration) (bool, error)
		want bool
	}{
		{"WaitForWatcher", fixture.WaitForWatcher, false},
		{"WaitForScan", fixture.WaitForScan, true},
	}
	for _, w := range waits {
		type result struct {
			done bool
			err  error
		}
		res := make(chan result, 1)
		go func() {
			done, err := w.wait(-time.Second)
			res <- result{done, err}
		}()
		select {
		case r := <-res:
			if r.err != nil || r.done != w.want {
				t.Errorf("%s(-1s) = %v, %v, want %v", w.name, r.done, r.err, w.want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s(-1s) did not return", w.name)
		}
	}
}

func TestMillis(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want uint64
	}{
		{-time.Second, 0},
		{0, 0},
		{time.Nanosecond, 1},
		{time.Millisecond, 1},
		{time.Millisecond + time.Nanosecond, 2},
		{1500 * time.Millisecond, 1500},
	}
	for _, tt := range tests {
		if got := uint64(millis(tt.d)); got != tt.want {
			t.Errorf("millis(%v) = %d, want %d", tt.d, got, tt.want)
		}
	}
}

func TestGitStatus(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	// A git hook exports GIT_DIR, and a leaked one must not reach the repository built next.
	sentinel := gitRepo(t, map[string]string{"s.txt": "s\n"})
	head := git(t, sentinel, "rev-parse", "HEAD")
	t.Setenv("GIT_DIR", filepath.Join(sentinel, ".git"))
	root := gitRepo(t, map[string]string{"changed.txt": "a\n", "clean.txt": "a\n", "staged.txt": "a\n", "deleted.txt": "a\n"})
	if got := git(t, sentinel, "rev-parse", "HEAD"); got != head {
		t.Fatalf("sentinel HEAD moved from %s to %s", head, got)
	}

	writeFile(t, filepath.Join(root, "changed.txt"), "changed\n")
	writeFile(t, filepath.Join(root, "staged.txt"), "staged\n")
	git(t, root, "add", "staged.txt")
	if err := os.Remove(filepath.Join(root, "deleted.txt")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "new.txt"), "new\n")

	inst := newInstance(t, Options{BasePath: root})
	waitForScan(t, inst)
	want := map[string]string{"changed.txt": "modified", "clean.txt": "clean", "new.txt": "untracked", "staged.txt": "staged_modified"}
	if got := gitStatuses(t, inst); !maps.Equal(got, want) {
		t.Fatalf("statuses = %v, want %v", got, want)
	}

	// Git also reports the clean and deleted files.
	writeFile(t, filepath.Join(root, "clean.txt"), "edited\n")
	if n, err := inst.RefreshGitStatus(); err != nil || n != 5 {
		t.Fatalf("RefreshGitStatus = %d, %v", n, err)
	}
	want["clean.txt"] = "modified"
	if got := gitStatuses(t, inst); !maps.Equal(got, want) {
		t.Fatalf("statuses after refresh = %v, want %v", got, want)
	}

	grep, err := inst.Grep(GrepQuery{Query: "changed"})
	if err != nil {
		t.Fatal(err)
	}
	if m := at(t, grep.Matches, 0); m.Path != "changed.txt" || m.GitStatus != "modified" {
		t.Fatalf("match = %+v", m)
	}

	// An empty path checks the working directory, which is otherwise inside this package's checkout.
	t.Chdir(root)
	for name, check := range map[string]func(string) (Health, error){"Instance": inst.HealthCheck, "package": HealthCheck} {
		for _, path := range []string{root, ""} {
			h, err := check(path)
			if err != nil {
				t.Fatal(err)
			}
			if !h.Git.RepositoryFound || h.Git.Workdir != root+"/" {
				t.Errorf("%s HealthCheck(%q) git = %+v", name, path, h.Git)
			}
		}
	}
}

func TestTrackQueryWithoutHistory(t *testing.T) {
	root := writeTree(t, fixtureFiles)
	inst := newInstance(t, Options{BasePath: root})
	waitForScan(t, inst)
	if tracked, err := inst.TrackQuery("thre", filepath.Join(root, "c/three.go")); err != nil || !tracked {
		t.Fatalf("TrackQuery = %v, %v", tracked, err)
	}
	if query, ok, err := inst.HistoricalQuery(0); err != nil || ok {
		t.Fatalf("HistoricalQuery(0) = %q, %v, %v", query, ok, err)
	}
}

func TestMultiGrepRejectsBadPatterns(t *testing.T) {
	for _, c := range []struct {
		patterns []string
		want     error
	}{
		{[]string{"needle\nhaystack"}, errPatternNewline},
		{[]string{"needle", ""}, errPatternEmpty},
	} {
		if _, err := fixture.MultiGrep(MultiGrepQuery{Patterns: c.patterns}); !errors.Is(err, c.want) {
			t.Errorf("MultiGrep(%q) err = %v, want %v", c.patterns, err, c.want)
		}
	}
}

func TestRejectsInvalidStrings(t *testing.T) {
	three := filepath.Join(fixtureDir, "c/three.go")
	calls := []struct {
		name string
		call func(s string) error
	}{
		{"New BasePath", func(s string) error { _, err := New(Options{BasePath: s}); return err }},
		{"New LogLevel", func(s string) error { _, err := New(Options{BasePath: fixtureDir, LogLevel: s}); return err }},
		{"Search Query", func(s string) error { _, err := fixture.Search(SearchQuery{Query: s}); return err }},
		{"Search CurrentFile", func(s string) error { _, err := fixture.Search(SearchQuery{CurrentFile: s}); return err }},
		{"Glob", func(s string) error { _, err := fixture.Glob(SearchQuery{Query: s}); return err }},
		{"SearchDirectories", func(s string) error { _, err := fixture.SearchDirectories(SearchQuery{Query: s}); return err }},
		{"SearchMixed", func(s string) error { _, err := fixture.SearchMixed(SearchQuery{Query: s}); return err }},
		{"Grep", func(s string) error { _, err := fixture.Grep(GrepQuery{Query: s}); return err }},
		{"MultiGrep Patterns", func(s string) error {
			_, err := fixture.MultiGrep(MultiGrepQuery{Patterns: []string{"needle", s}})
			return err
		}},
		{"MultiGrep Constraints", func(s string) error {
			_, err := fixture.MultiGrep(MultiGrepQuery{Patterns: []string{"needle"}, Constraints: s})
			return err
		}},
		{"RestartIndex", func(s string) error { return fixture.RestartIndex(s) }},
		{"TrackQuery query", func(s string) error { _, err := fixture.TrackQuery(s, three); return err }},
		{"TrackQuery filePath", func(s string) error { _, err := fixture.TrackQuery("thre", s); return err }},
		{"HealthCheck", func(s string) error { _, err := HealthCheck(s); return err }},
		{"Instance HealthCheck", func(s string) error { _, err := fixture.HealthCheck(s); return err }},
		{"Watch pattern", func(s string) error { _, err := fixture.Watch(s, WatchOptions{}, func([]WatchEvent) {}); return err }},
		{"Watch Ignore", func(s string) error {
			_, err := fixture.Watch("", WatchOptions{Ignore: []string{"*.log", s}}, func([]WatchEvent) {})
			return err
		}},
	}
	invalid := []struct {
		s    string
		want error
	}{
		{"a\x00", errNUL},
		{"a\xff", errInvalidUTF8},
	}
	for _, c := range calls {
		for _, bad := range invalid {
			if err := c.call(bad.s); !errors.Is(err, bad.want) {
				t.Errorf("%s(%q) err = %v, want %v", c.name, bad.s, err, bad.want)
			}
		}
	}
}

func TestClosedInstance(t *testing.T) {
	inst := newInstance(t, Options{BasePath: writeTree(t, fixtureFiles), Watch: true})
	if err := inst.Close(); err != nil {
		t.Fatal(err)
	}
	calls := []struct {
		name string
		call func() error
	}{
		{"Search", func() error { _, err := inst.Search(SearchQuery{}); return err }},
		{"Glob", func() error { _, err := inst.Glob(SearchQuery{Query: "*"}); return err }},
		{"SearchDirectories", func() error { _, err := inst.SearchDirectories(SearchQuery{}); return err }},
		{"SearchMixed", func() error { _, err := inst.SearchMixed(SearchQuery{}); return err }},
		{"Grep", func() error { _, err := inst.Grep(GrepQuery{Query: "needle"}); return err }},
		{"MultiGrep", func() error { _, err := inst.MultiGrep(MultiGrepQuery{Patterns: []string{"needle"}}); return err }},
		{"ScanFiles", inst.ScanFiles},
		{"WaitForScan", func() error { _, err := inst.WaitForScan(time.Second); return err }},
		{"WaitForWatcher", func() error { _, err := inst.WaitForWatcher(time.Second); return err }},
		{"ScanProgress", func() error { _, err := inst.ScanProgress(); return err }},
		{"BasePath", func() error { _, err := inst.BasePath(); return err }},
		{"RestartIndex", func() error { return inst.RestartIndex(fixtureDir) }},
		{"RefreshGitStatus", func() error { _, err := inst.RefreshGitStatus(); return err }},
		{"TrackQuery", func() error { _, err := inst.TrackQuery("thre", filepath.Join(fixtureDir, "c/three.go")); return err }},
		{"HistoricalQuery", func() error { _, _, err := inst.HistoricalQuery(0); return err }},
		{"HealthCheck", func() error { _, err := inst.HealthCheck(fixtureDir); return err }},
		{"Watch", func() error { _, err := inst.Watch("", WatchOptions{}, func([]WatchEvent) {}); return err }},
		{"Unwatch", func() error { _, err := inst.Unwatch(1); return err }},
	}
	for _, c := range calls {
		if err := c.call(); !errors.Is(err, ErrClosed) {
			t.Errorf("%s on a closed instance: err = %v, want ErrClosed", c.name, err)
		}
	}
	if inst.IsScanning() {
		t.Error("IsScanning on a closed instance reported true")
	}
}

type lineMatch struct {
	Path       string
	LineNumber uint64
	LineStart  uint64
	Line       string
	Ranges     []MatchRange
}

func lineMatches(matches []GrepMatch) []lineMatch {
	out := make([]lineMatch, len(matches))
	for k, m := range matches {
		out[k] = lineMatch{m.Path, m.LineNumber, m.LineStart, m.Line, m.Ranges}
	}
	return out
}

func matchPaths(matches []GrepMatch) []string {
	paths := make([]string, len(matches))
	for k, m := range matches {
		paths[k] = m.Path
	}
	return paths
}

func sortedPaths(items []FileItem) []string {
	paths := make([]string, len(items))
	for k, item := range items {
		paths[k] = item.Path
	}
	slices.Sort(paths)
	return paths
}

func penalised[T any](t *testing.T, items []T, scores []Score, path func(T) string) []string {
	t.Helper()
	var paths []string
	for k, item := range items {
		if s := at(t, scores, k); s.CurrentFilePenalty < 0 || s.DistancePenalty < 0 {
			paths = append(paths, path(item))
		}
	}
	slices.Sort(paths)
	return paths
}

func samePointer(a, b string) bool {
	return unsafe.StringData(a) == unsafe.StringData(b)
}

func at[T any](t *testing.T, s []T, k int) T {
	t.Helper()
	if k >= len(s) {
		t.Fatalf("want more than %d items, got %+v", k, s)
	}
	return s[k]
}

func gitStatuses(t *testing.T, inst *Instance) map[string]string {
	t.Helper()
	page, err := inst.Search(SearchQuery{})
	if err != nil {
		t.Fatal(err)
	}
	statuses := make(map[string]string, len(page.Items))
	for _, item := range page.Items {
		statuses[item.Path] = item.GitStatus
	}
	return statuses
}

func gitRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	clearGitEnv(t)
	root := writeTree(t, files)
	git(t, root, "init", "-q")
	git(t, root, "add", ".")
	git(t, root, "commit", "-q", "-m", "initial")
	return root
}

func clearGitEnv(t *testing.T) {
	t.Helper()
	// Variables such as GIT_DIR, set by a git hook running these tests, would redirect git.
	out, err := exec.Command("git", "rev-parse", "--local-env-vars").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range strings.Fields(string(out)) {
		t.Setenv(name, "") // restores the value after the test
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	config := []string{"-c", "user.name=fff", "-c", "user.email=fff@example.com", "-c", "commit.gpgsign=false", "-c", "init.defaultBranch=main"}
	cmd := exec.Command("git", append(config, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func runWithFixture(m *testing.M) (code int, err error) {
	root, err := os.MkdirTemp("", "fff-go-test-")
	if err != nil {
		return 0, err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(root)) }()
	if root, err = filepath.EvalSymlinks(root); err != nil {
		return 0, err
	}
	fixtureDir = filepath.Join(root, "tree")
	if err := writeFiles(fixtureDir, fixtureFiles); err != nil {
		return 0, err
	}

	fixture, err = New(Options{
		BasePath:       fixtureDir,
		FrecencyDBPath: filepath.Join(root, "state", "frecency"),
		HistoryDBPath:  filepath.Join(root, "state", "history"),
	})
	if err != nil {
		return 0, err
	}
	defer func() { err = errors.Join(err, fixture.Close()) }()
	if done, err := fixture.WaitForScan(10 * time.Second); err != nil || !done {
		return 0, errors.Join(errors.New("fixture scan did not finish"), err)
	}
	return m.Run(), nil
}

func newInstance(t *testing.T, opts Options) *Instance {
	t.Helper()
	inst, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := inst.Close(); err != nil {
			t.Error(err)
		}
	})
	return inst
}

func waitForScan(t *testing.T, inst *Instance) {
	t.Helper()
	if done, err := inst.WaitForScan(10 * time.Second); err != nil || !done {
		t.Fatalf("WaitForScan = %v, %v", done, err)
	}
}

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err == nil {
		err = writeFiles(root, files)
	}
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func writeFiles(root string, files map[string]string) error {
	for path, content := range files {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}
