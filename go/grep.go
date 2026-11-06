package fff

/*
#include <fff.h>

struct fff_go_grep_options {
	uint64_t max_file_size;
	uint32_t max_matches_per_file;
	bool smart_case;
	uint32_t file_offset;
	uint32_t page_limit;
	uint64_t time_budget_ms;
	bool enforce_time_budget;
	uint32_t before_context;
	uint32_t after_context;
	uint8_t classify_definitions;
};

// The trailing arguments fff_live_grep_ex, fff_multi_grep_ex and fff_multi_grep_overlapping share.
#define FFF_GO_GREP_OPTIONS(o) (o)->max_file_size, (o)->max_matches_per_file, (o)->smart_case, \
	(o)->file_offset, (o)->page_limit, (o)->time_budget_ms, (o)->enforce_time_budget, \
	(o)->before_context, (o)->after_context, (o)->classify_definitions

static struct FffResult *fff_go_live_grep(void *handle, const char *query, uint8_t mode,
		const struct fff_go_grep_options *o) {
	return fff_live_grep_ex(handle, query, mode, FFF_GO_GREP_OPTIONS(o));
}

static struct FffResult *fff_go_multi_grep(void *handle, const char *patterns, const char *constraints,
		const struct fff_go_grep_options *o) {
	return fff_multi_grep_ex(handle, patterns, constraints, FFF_GO_GREP_OPTIONS(o));
}

static struct FffResult *fff_go_multi_grep_overlapping(void *handle, const char *patterns,
		const char *constraints, const struct fff_go_grep_options *o) {
	return fff_multi_grep_overlapping(handle, patterns, constraints, FFF_GO_GREP_OPTIONS(o));
}
*/
import "C"

import (
	"errors"
	"strings"
	"time"
	"unsafe"
)

var (
	errPatternNewline = errors.New("fff: pattern contains a newline")
	errPatternEmpty   = errors.New("fff: pattern is empty")
	errGrepMode       = errors.New("fff: unknown grep mode")
)

// GrepMode selects how Grep interprets GrepQuery.Query.
type GrepMode uint8

// An invalid Regex falls back to PlainText and sets GrepPage.RegexFallbackError.
const (
	PlainText GrepMode = iota
	Regex
	Fuzzy
)

// DefinitionMode controls whether matches are classified as code definitions.
type DefinitionMode uint8

// DefinitionsClassify sets GrepMatch.IsDefinition; the last two keep only one kind of match.
const (
	DefinitionsOff DefinitionMode = iota
	DefinitionsClassify
	DefinitionsOnly
	UsagesOnly
)

// GrepOptions holds the paging, limit and output settings Grep and MultiGrep share.
// Zero numbers select FFF's defaults.
type GrepOptions struct {
	FileOffset        uint32        // NextFileOffset of the previous page
	PageLimit         uint32        // soft cap, 50 by default; a page may pass it to finish a file
	MaxMatchesPerFile uint32        // unlimited by default
	MaxFileSize       uint64        // bytes, 10 MB by default; larger files are skipped
	TimeBudget        time.Duration // zero or less means no budget
	EnforceTimeBudget bool          // apply the budget before any match is found too
	SmartCase         bool          // case-insensitive unless the query has an uppercase letter
	BeforeContext     uint32        // lines of context before each match
	AfterContext      uint32        // lines of context after each match
	Definitions       DefinitionMode
}

// GrepQuery drives Grep. The query may carry constraints such as "*.go" or "/src/"
// before the search text.
type GrepQuery struct {
	Query string
	Mode  GrepMode
	GrepOptions
}

// MultiGrepQuery drives MultiGrep, which matches any of Patterns as plain text.
// Each pattern must be non-empty and contain no newline.
type MultiGrepQuery struct {
	Patterns    []string
	Constraints string // such as "*.go /src/"
	// Overlapping reports every hit, overlapping ones too ("aa" 3 times in "aaaa"), and sets
	// PatternIndexes. Off, hits never overlap and end within Line's first 512 bytes.
	Overlapping bool
	GrepOptions
}

// GrepPage is one page of matches. Pass NextFileOffset as the next FileOffset to continue.
type GrepPage struct {
	Matches            []GrepMatch
	FilesSearched      uint32
	FilteredFiles      uint32
	TotalFiles         uint32
	NextFileOffset     uint32 // 0 when the search is complete
	RegexFallbackError string // set when an invalid regex fell back to plain text
}

// GrepMatch is one matched line. Matches in the same file share one Path string.
type GrepMatch struct {
	Path                 string // relative to the base path
	FileName             string
	GitStatus            string
	Size                 uint64 // bytes
	Modified             uint64 // seconds since the Unix epoch
	TotalFrecency        int64
	AccessFrecency       int64
	ModificationFrecency int64
	IsBinary             bool
	// Line holds at most the first 512 bytes of the line, with invalid UTF-8 replaced,
	// so Ranges may not line up with it on such lines.
	Line       string
	LineNumber uint64 // 1-based
	LineStart  uint64 // byte offset of the line from the start of the file
	// Col is the byte offset of the first match within the line. With Overlapping it is
	// the smallest range start, which need not be Ranges[0].Start.
	Col uint32
	// Ranges are byte offsets into the raw line, in end-offset order when Overlapping.
	// Only Overlapping reports matches past its 512th byte.
	Ranges []MatchRange
	// PatternIndexes holds the MultiGrepQuery.Patterns index of each range.
	// Only an Overlapping MultiGrep sets it.
	PatternIndexes []uint32
	ContextBefore  []string
	ContextAfter   []string
	FuzzyScore     uint16
	HasFuzzyScore  bool
	IsDefinition   bool
}

// MatchRange holds byte offsets into the matched line, so LineStart+Start is the file offset.
type MatchRange struct {
	Start, End uint32
}

// grepPage reads FffMatchRange arrays in place as MatchRange, so a size mismatch fails the build.
var _ [0]byte = [unsafe.Sizeof(C.struct_FffMatchRange{}) - unsafe.Sizeof(MatchRange{})]byte{}

// Grep searches file contents for one query.
func (i *Instance) Grep(q GrepQuery) (GrepPage, error) {
	if i.handle == nil {
		return GrepPage{}, ErrClosed
	}
	if q.Mode > Fuzzy {
		return GrepPage{}, errGrepMode
	}
	query, err := cString(q.Query)
	if err != nil {
		return GrepPage{}, err
	}
	opts := q.c()
	return grepPage(C.fff_go_live_grep(i.handle, query, C.uint8_t(q.Mode), &opts))
}

// MultiGrep searches file contents for any of several plain-text patterns in one pass.
func (i *Instance) MultiGrep(q MultiGrepQuery) (GrepPage, error) {
	if i.handle == nil {
		return GrepPage{}, ErrClosed
	}
	patterns, err := joinPatterns(q.Patterns)
	if err != nil {
		return GrepPage{}, err
	}
	constraints, err := cString(q.Constraints)
	if err != nil {
		return GrepPage{}, err
	}
	opts := q.c()
	if q.Overlapping {
		return grepPage(C.fff_go_multi_grep_overlapping(i.handle, patterns, constraints, &opts))
	}
	return grepPage(C.fff_go_multi_grep(i.handle, patterns, constraints, &opts))
}

func (o *GrepOptions) c() C.struct_fff_go_grep_options {
	return C.struct_fff_go_grep_options{
		max_file_size:        C.uint64_t(o.MaxFileSize),
		max_matches_per_file: C.uint32_t(o.MaxMatchesPerFile),
		smart_case:           C.bool(o.SmartCase),
		file_offset:          C.uint32_t(o.FileOffset),
		page_limit:           C.uint32_t(o.PageLimit),
		time_budget_ms:       millis(o.TimeBudget),
		enforce_time_budget:  C.bool(o.EnforceTimeBudget),
		before_context:       C.uint32_t(o.BeforeContext),
		after_context:        C.uint32_t(o.AfterContext),
		classify_definitions: C.uint8_t(o.Definitions),
	}
}

func grepPage(res *C.struct_FffResult) (GrepPage, error) {
	handle, _, err := unwrap(res)
	if err != nil {
		return GrepPage{}, err
	}
	r := (*C.struct_FffGrepResult)(handle)
	cmatches := unsafe.Slice(r.items, r.count)

	// Size one backing array each for ranges and context lines across the whole page.
	var nranges, ncontext int
	for k := range cmatches {
		c := &cmatches[k]
		nranges += int(c.match_ranges_count)
		ncontext += int(c.context_before_count) + int(c.context_after_count)
	}
	ranges := make([]MatchRange, 0, nranges)
	context := make([]string, 0, ncontext)
	// One entry per range across the page, set only by an overlapping multi-pattern grep.
	var patternIndexes []uint32
	if r.match_pattern_indexes_count > 0 {
		patternIndexes = make([]uint32, r.match_pattern_indexes_count)
		copy(patternIndexes, unsafe.Slice((*uint32)(unsafe.Pointer(r.match_pattern_indexes)), r.match_pattern_indexes_count))
	}

	page := GrepPage{
		Matches:        make([]GrepMatch, len(cmatches)),
		FilesSearched:  uint32(r.total_files_searched),
		FilteredFiles:  uint32(r.filtered_file_count),
		TotalFiles:     uint32(r.total_files),
		NextFileOffset: uint32(r.next_file_offset),
	}
	if r.regex_fallback_error != nil {
		page.RegexFallbackError = C.GoString(r.regex_fallback_error)
	}

	var path, name, status string
	for k := range cmatches {
		c := &cmatches[k]
		path = reuse(path, c.relative_path)
		name = reuse(name, c.file_name)
		status = reuse(status, c.git_status)

		start := len(ranges)
		ranges = append(ranges, unsafe.Slice((*MatchRange)(unsafe.Pointer(c.match_ranges)), c.match_ranges_count)...)
		before := len(context)
		context = appendStrings(context, c.context_before, c.context_before_count)
		after := len(context)
		context = appendStrings(context, c.context_after, c.context_after_count)

		end := len(ranges)
		page.Matches[k] = GrepMatch{
			Path:                 path,
			FileName:             name,
			GitStatus:            status,
			Size:                 uint64(c.size),
			Modified:             uint64(c.modified),
			TotalFrecency:        int64(c.total_frecency_score),
			AccessFrecency:       int64(c.access_frecency_score),
			ModificationFrecency: int64(c.modification_frecency_score),
			IsBinary:             bool(c.is_binary),
			Line:                 C.GoString(c.line_content),
			LineNumber:           uint64(c.line_number),
			LineStart:            uint64(c.byte_offset),
			Col:                  uint32(c.col),
			Ranges:               ranges[start:end:end],
			ContextBefore:        context[before:after:after],
			ContextAfter:         context[after:len(context):len(context)],
			FuzzyScore:           uint16(c.fuzzy_score),
			HasFuzzyScore:        bool(c.has_fuzzy_score),
			IsDefinition:         bool(c.is_definition),
		}
		if patternIndexes != nil {
			page.Matches[k].PatternIndexes = patternIndexes[start:end:end]
		}
	}
	C.fff_free_grep_result(r)
	return page, nil
}

func appendStrings(dst []string, p **C.char, count C.uint32_t) []string {
	for _, s := range unsafe.Slice(p, count) {
		dst = append(dst, C.GoString(s))
	}
	return dst
}

func joinPatterns(patterns []string) (*C.char, error) {
	n := len(patterns)
	for _, p := range patterns {
		if p == "" {
			return nil, errPatternEmpty
		}
		if strings.IndexByte(p, '\n') >= 0 {
			return nil, errPatternNewline
		}
		if err := checkString(p); err != nil {
			return nil, err
		}
		n += len(p)
	}
	b := make([]byte, 0, n)
	for k, p := range patterns {
		if k > 0 {
			b = append(b, '\n')
		}
		b = append(b, p...)
	}
	b = append(b, 0)
	return (*C.char)(unsafe.Pointer(&b[0])), nil
}
