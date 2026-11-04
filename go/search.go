package fff

/*
#include <fff.h>
*/
import "C"

import "unsafe"

const itemTypeDir = 1 // FffMixedItem.item_type of a directory; a file is 0

// SearchQuery drives Search, Glob, SearchDirectories and SearchMixed; zero numbers select
// FFF's defaults. Combo fields apply to Search and SearchMixed only.
type SearchQuery struct {
	Query string // the glob pattern for Glob
	// CurrentFile is the caller's open file relative to the base path, or empty. Search, Glob and
	// SearchMixed rank it lower, and all but Glob favour paths near it.
	CurrentFile          string
	MaxThreads           uint32 // chosen by FFF by default
	PageIndex            uint32 // items to skip, not a page number
	PageSize             uint32 // 100 by default
	ComboBoostMultiplier int32  // 100 by default
	MinComboCount        uint32 // 3 by default
}

// SearchPage is one page of ranked files from Search or Glob.
type SearchPage struct {
	Items        []FileItem
	Scores       []Score // parallel to Items
	TotalMatched uint32
	TotalFiles   uint32
	Location     Location // parsed from a query suffix such as "file.go:12"
}

// FileItem is one indexed file.
type FileItem struct {
	Path                 string // relative to the base path
	FileName             string
	GitStatus            string
	Size                 uint64 // bytes
	Modified             uint64 // seconds since the Unix epoch
	AccessFrecency       int64
	ModificationFrecency int64
	TotalFrecency        int64
	IsBinary             bool
}

// Score breaks down how an item was ranked.
type Score struct {
	Total                int32
	Base                 int32
	FilenameBonus        int32
	SpecialFilenameBonus int32
	FrecencyBoost        int32
	DistancePenalty      int32
	CurrentFilePenalty   int32
	ComboMatchBoost      int32
	PathAlignmentBonus   int32
	ExactMatch           bool
	MatchType            string
}

// LocationKind says which Location fields a query suffix set.
type LocationKind uint8

// Location kinds, as parsed from "f", "f:12", "f:12:4" and "f:12:4-14:20".
const (
	NoLocation LocationKind = iota
	LineLocation
	PositionLocation
	RangeLocation
)

// Location is a line, position or range parsed from the end of a search query.
type Location struct {
	Kind    LocationKind
	Line    int32
	Col     int32
	EndLine int32
	EndCol  int32
}

// DirPage is one page of ranked directories from SearchDirectories.
type DirPage struct {
	Items        []DirItem
	Scores       []Score // parallel to Items
	TotalMatched uint32
	TotalDirs    uint32
}

// DirItem is one indexed directory.
type DirItem struct {
	Path              string // relative to the base path
	Name              string
	MaxAccessFrecency int32
}

// MixedPage is one page of files and directories from SearchMixed.
type MixedPage struct {
	Items        []MixedItem
	Scores       []Score // parallel to Items
	TotalMatched uint32
	TotalFiles   uint32
	TotalDirs    uint32
	Location     Location
}

// MixedItem is a file or, when IsDir is set, a directory. A directory has Path, FileName and
// the highest child access frecency in AccessFrecency and TotalFrecency; other fields are zero.
type MixedItem struct {
	FileItem
	IsDir bool
}

// mixedItem reads FffMixedItem past item_type as FffFileItem, so a size mismatch fails the build.
var _ [0]byte = [unsafe.Sizeof(C.struct_FffMixedItem{}) - unsafe.Sizeof(C.struct_FffFileItem{}) -
	unsafe.Offsetof(C.struct_FffMixedItem{}.relative_path)]byte{}

// Search fuzzy-matches indexed file paths.
func (i *Instance) Search(q SearchQuery) (SearchPage, error) {
	query, current, err := i.searchStrings(&q)
	if err != nil {
		return SearchPage{}, err
	}
	return searchPage(C.fff_search(i.handle, query, current, C.uint32_t(q.MaxThreads),
		C.uint32_t(q.PageIndex), C.uint32_t(q.PageSize), C.int32_t(q.ComboBoostMultiplier),
		C.uint32_t(q.MinComboCount)))
}

// Glob filters indexed files by the glob in q.Query and ranks them by frecency.
func (i *Instance) Glob(q SearchQuery) (SearchPage, error) {
	query, current, err := i.searchStrings(&q)
	if err != nil {
		return SearchPage{}, err
	}
	return searchPage(C.fff_glob(i.handle, query, current, C.uint32_t(q.MaxThreads),
		C.uint32_t(q.PageIndex), C.uint32_t(q.PageSize)))
}

// SearchDirectories fuzzy-matches indexed directories.
func (i *Instance) SearchDirectories(q SearchQuery) (DirPage, error) {
	query, current, err := i.searchStrings(&q)
	if err != nil {
		return DirPage{}, err
	}
	return dirPage(C.fff_search_directories(i.handle, query, current, C.uint32_t(q.MaxThreads),
		C.uint32_t(q.PageIndex), C.uint32_t(q.PageSize)))
}

// SearchMixed fuzzy-matches files and directories into one list ordered by score.
func (i *Instance) SearchMixed(q SearchQuery) (MixedPage, error) {
	query, current, err := i.searchStrings(&q)
	if err != nil {
		return MixedPage{}, err
	}
	return mixedPage(C.fff_search_mixed(i.handle, query, current, C.uint32_t(q.MaxThreads),
		C.uint32_t(q.PageIndex), C.uint32_t(q.PageSize), C.int32_t(q.ComboBoostMultiplier),
		C.uint32_t(q.MinComboCount)))
}

func (i *Instance) searchStrings(q *SearchQuery) (*C.char, *C.char, error) {
	if i.handle == nil {
		return nil, nil, ErrClosed
	}
	query, err := cString(q.Query)
	if err != nil {
		return nil, nil, err
	}
	current, err := cString(q.CurrentFile)
	return query, current, err
}

func searchPage(res *C.struct_FffResult) (SearchPage, error) {
	handle, _, err := unwrap(res)
	if err != nil {
		return SearchPage{}, err
	}
	r := (*C.struct_FffSearchResult)(handle)
	citems := unsafe.Slice(r.items, r.count)
	page := SearchPage{
		Items:        make([]FileItem, len(citems)),
		Scores:       scores(r.scores, r.count),
		TotalMatched: uint32(r.total_matched),
		TotalFiles:   uint32(r.total_files),
		Location:     location(&r.location),
	}
	var status string
	for k := range citems {
		page.Items[k] = fileItem(&citems[k], &status)
	}
	C.fff_free_search_result(r)
	return page, nil
}

func dirPage(res *C.struct_FffResult) (DirPage, error) {
	handle, _, err := unwrap(res)
	if err != nil {
		return DirPage{}, err
	}
	r := (*C.struct_FffDirSearchResult)(handle)
	citems := unsafe.Slice(r.items, r.count)
	page := DirPage{
		Items:        make([]DirItem, len(citems)),
		Scores:       scores(r.scores, r.count),
		TotalMatched: uint32(r.total_matched),
		TotalDirs:    uint32(r.total_dirs),
	}
	for k := range citems {
		c := &citems[k]
		page.Items[k] = DirItem{
			Path:              C.GoString(c.relative_path),
			Name:              C.GoString(c.dir_name),
			MaxAccessFrecency: int32(c.max_access_frecency),
		}
	}
	C.fff_free_dir_search_result(r)
	return page, nil
}

func mixedPage(res *C.struct_FffResult) (MixedPage, error) {
	handle, _, err := unwrap(res)
	if err != nil {
		return MixedPage{}, err
	}
	r := (*C.struct_FffMixedSearchResult)(handle)
	citems := unsafe.Slice(r.items, r.count)
	page := MixedPage{
		Items:        make([]MixedItem, len(citems)),
		Scores:       scores(r.scores, r.count),
		TotalMatched: uint32(r.total_matched),
		TotalFiles:   uint32(r.total_files),
		TotalDirs:    uint32(r.total_dirs),
		Location:     location(&r.location),
	}
	var status string
	for k := range citems {
		page.Items[k] = mixedItem(&citems[k], &status)
	}
	C.fff_free_mixed_search_result(r)
	return page, nil
}

func fileItem(c *C.struct_FffFileItem, status *string) FileItem {
	// Consecutive items mostly share a git status, so one string is reused across the page.
	*status = reuse(*status, c.git_status)
	return FileItem{
		Path:                 C.GoString(c.relative_path),
		FileName:             C.GoString(c.file_name),
		GitStatus:            *status,
		Size:                 uint64(c.size),
		Modified:             uint64(c.modified),
		AccessFrecency:       int64(c.access_frecency_score),
		ModificationFrecency: int64(c.modification_frecency_score),
		TotalFrecency:        int64(c.total_frecency_score),
		IsBinary:             bool(c.is_binary),
	}
}

func mixedItem(c *C.struct_FffMixedItem, status *string) MixedItem {
	// From relative_path on, FffMixedItem is an FffFileItem with display_name as file_name.
	file := (*C.struct_FffFileItem)(unsafe.Pointer(&c.relative_path))
	return MixedItem{FileItem: fileItem(file, status), IsDir: c.item_type == itemTypeDir}
}

func scores(p *C.struct_FffScore, count C.uint32_t) []Score {
	cscores := unsafe.Slice(p, count)
	out := make([]Score, len(cscores))
	var matchType string
	for k := range cscores {
		c := &cscores[k]
		matchType = reuse(matchType, c.match_type)
		out[k] = Score{
			Total:                int32(c.total),
			Base:                 int32(c.base_score),
			FilenameBonus:        int32(c.filename_bonus),
			SpecialFilenameBonus: int32(c.special_filename_bonus),
			FrecencyBoost:        int32(c.frecency_boost),
			DistancePenalty:      int32(c.distance_penalty),
			CurrentFilePenalty:   int32(c.current_file_penalty),
			ComboMatchBoost:      int32(c.combo_match_boost),
			PathAlignmentBonus:   int32(c.path_alignment_bonus),
			ExactMatch:           bool(c.exact_match),
			MatchType:            matchType,
		}
	}
	return out
}

func location(c *C.struct_FffLocation) Location {
	return Location{
		Kind:    LocationKind(c.tag),
		Line:    int32(c.line),
		Col:     int32(c.col),
		EndLine: int32(c.end_line),
		EndCol:  int32(c.end_col),
	}
}
