use super::grep::{GrepContext, perform_grep};
use super::overlapping::OverlappingSink;
use super::prefilter::prefilter_files;
use super::sink::{SinkState, debug_assert_newline_terminator, strip_line_terminators};
use super::types::{GrepResult, GrepSearchOptions};
use crate::error::Error;
use crate::index::{BigramFilter, BigramOverlay, bigram_boundary, literal_candidates};
use crate::types::{ContentCacheBudget, FileItem, FileSliceExt};
use aho_corasick::{AhoCorasick, MatchKind};
use fff_grep::{
    Searcher, SearcherBuilder, Sink, SinkMatch,
    matcher::{Match, Matcher, NoError},
};
use smallvec::SmallVec;
use std::path::Path;
use std::sync::atomic::AtomicBool;

/// A `grep_matcher::Matcher` backed by Aho-Corasick for multi-pattern search.
///
/// Finds the first occurrence of any pattern starting at the given offset.
/// Always reports `\n` as the line terminator for the fast candidate-line path.
struct AhoCorasickMatcher<'a> {
    ac: &'a AhoCorasick,
}

impl Matcher for AhoCorasickMatcher<'_> {
    type Error = NoError;

    #[inline]
    fn find_at(&self, haystack: &[u8], at: usize) -> std::result::Result<Option<Match>, NoError> {
        let hay = &haystack[at..];
        let found: Option<aho_corasick::Match> = self.ac.find(hay);
        Ok(found.map(|m| Match::new(at + m.start(), at + m.end())))
    }

    #[inline]
    fn line_terminator(&self) -> Option<fff_grep::LineTerminator> {
        Some(fff_grep::LineTerminator::byte(b'\n'))
    }
}

/// Sink for Aho-Corasick multi-pattern mode.
///
/// Collects all pattern match positions on each matched line for highlighting.
struct AhoCorasickSink<'a> {
    state: SinkState<'a>,
    ac: &'a AhoCorasick,
}

impl Sink for AhoCorasickSink<'_> {
    type Error = std::io::Error;

    fn matched(&mut self, searcher: &Searcher, mat: &SinkMatch<'_>) -> Result<bool, Self::Error> {
        debug_assert_newline_terminator(searcher);
        let line_bytes = mat.bytes();
        let (display_bytes, display_len, line_number, byte_offset) =
            SinkState::prepare_line(line_bytes, mat);

        let line_content = String::from_utf8_lossy(display_bytes);
        let mut match_byte_offsets: SmallVec<[(u32, u32); 4]> = SmallVec::new();
        let mut col = 0usize;
        let mut first = true;

        for m in self.ac.find_iter(display_bytes as &[u8]) {
            let abs_start = m.start() as u32;
            let abs_end = (m.end() as u32).min(display_len);
            if first {
                col = abs_start as usize;
                first = false;
            }
            match_byte_offsets.push((abs_start, abs_end));
        }

        let Some(is_definition) = self.state.definition_mode.classify_match(
            self.state.file,
            strip_line_terminators(line_bytes),
            &match_byte_offsets,
            false,
        ) else {
            return Ok(true);
        };
        let (context_before, context_after) = self.state.extract_context(mat);
        Ok(self.state.push_match(
            line_number,
            col,
            byte_offset,
            line_content.into_owned(),
            match_byte_offsets,
            is_definition,
            context_before,
            context_after,
        ))
    }

    fn finish(&mut self, _: &Searcher, _: &fff_grep::SinkFinish) -> Result<(), Self::Error> {
        Ok(())
    }
}

/// Multi-pattern OR search using Aho-Corasick.
///
/// Builds a single automaton from all patterns and searches each file in one
/// pass. This is significantly faster than regex alternation for literal text
/// searches because Aho-Corasick uses SIMD-accelerated multi-needle matching.
///
/// Returns the same `GrepResult` type as `grep_search`. With `overlapping`, every
/// pattern hit is reported over the full line, and the second value holds each
/// range's pattern index in match order; otherwise it is empty.
///
/// Fails with [`Error::EmptyOverlappingPattern`] when `overlapping` and any pattern
/// is empty; the default mode never fails.
#[allow(clippy::too_many_arguments)]
pub(crate) fn multi_grep_search_with<'a>(
    files: &'a [FileItem],
    patterns: &[&str],
    constraints: &[fff_query_parser::Constraint<'_>],
    options: &GrepSearchOptions,
    budget: &ContentCacheBudget,
    bigram_index: Option<&BigramFilter>,
    bigram_overlay: Option<&BigramOverlay>,
    abort_signal: &AtomicBool,
    base_path: &Path,
    arena: crate::simd_path::ArenaPtr,
    overflow_arena: crate::simd_path::ArenaPtr,
    overlapping: bool,
) -> Result<(GrepResult<'a>, Vec<u32>), Error> {
    if overlapping && patterns.iter().any(|p| p.is_empty()) {
        return Err(Error::EmptyOverlappingPattern);
    }
    let total_files = files.live_count();

    if patterns.is_empty() || patterns.iter().all(|p| p.is_empty()) {
        return Ok((GrepResult::empty(total_files, total_files), Vec::new()));
    }

    let bigram_candidates = literal_candidates(bigram_index, bigram_overlay, patterns);
    let base_file_count = bigram_boundary(bigram_overlay, files.len());

    // Constraints are separate from patterns, so a miss must not broaden the search.
    let (mut files_to_search, filtered_file_count) = prefilter_files(
        files,
        constraints,
        bigram_candidates.as_deref(),
        base_file_count,
        options,
        arena,
        overflow_arena,
    );

    if files_to_search.is_empty() {
        return Ok((
            GrepResult::empty(total_files, filtered_file_count),
            Vec::new(),
        ));
    }

    let casing = options.effective_casing();
    let case_insensitive = patterns.iter().all(|p| casing.is_insensitive_for(p));

    // Standard is the only match kind `find_overlapping_iter` accepts.
    let ac = aho_corasick::AhoCorasickBuilder::new()
        .match_kind(MatchKind::Standard)
        .ascii_case_insensitive(case_insensitive)
        .build(patterns)
        .expect("Aho-Corasick build should not fail for literal patterns");

    let searcher = {
        let mut b = SearcherBuilder::new();
        b.line_number(true);
        b
    }
    .build();

    let ac_matcher = AhoCorasickMatcher { ac: &ac };
    let ctx = GrepContext {
        total_files,
        filtered_file_count,
        budget,
        base_path,
        arena,
        overflow_arena,
        prefilter: None, // no memmem prefilter for multi-pattern search
        abort_signal,
    };
    if overlapping {
        let (result, per_file) = perform_grep(
            &mut files_to_search,
            options,
            &ctx,
            |file: &FileItem, file_bytes: &[u8], max_matches: usize| {
                let mut sink = OverlappingSink {
                    state: sink_state(file, options, max_matches),
                    ac: &ac,
                    trim_whitespace: options.trim_whitespace,
                    pattern_indexes: Vec::new(),
                };
                if let Err(e) = searcher.search_slice(&ac_matcher, file_bytes, &mut sink) {
                    tracing::error!(error = %e, "Grep (aho-corasick overlapping) search failed");
                }
                (sink.state.matches, sink.pattern_indexes)
            },
        );
        // The sink already trimmed, so the trim in perform_grep finds nothing left to strip.
        return Ok((result, flatten(per_file)));
    }

    let (result, _) = perform_grep(
        &mut files_to_search,
        options,
        &ctx,
        |file: &FileItem, file_bytes: &[u8], max_matches: usize| {
            let mut sink = AhoCorasickSink {
                state: sink_state(file, options, max_matches),
                ac: &ac,
            };
            if let Err(e) = searcher.search_slice(&ac_matcher, file_bytes, &mut sink) {
                tracing::error!(error = %e, "Grep (aho-corasick multi) search failed");
            }
            (sink.state.matches, ())
        },
    );
    Ok((result, Vec::new()))
}

fn sink_state<'f>(file: &'f FileItem, options: &GrepSearchOptions, max: usize) -> SinkState<'f> {
    SinkState {
        file,
        definition_mode: options.classify_definitions,
        file_index: 0,
        matches: Vec::with_capacity(4),
        max_matches: max,
        before_context: options.before_context,
        after_context: options.after_context,
    }
}

// Joins the per-file index vectors, growing the first one instead of allocating a new one.
fn flatten(per_file: Vec<Vec<u32>>) -> Vec<u32> {
    let total: usize = per_file.iter().map(Vec::len).sum();
    let mut files = per_file.into_iter();
    let Some(mut all) = files.next() else {
        return Vec::new();
    };
    all.reserve_exact(total - all.len());
    for indexes in files {
        all.extend(indexes);
    }
    all
}
