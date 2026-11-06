use super::sink::{SinkState, debug_assert_newline_terminator, strip_line_terminators};
use super::types::GrepResult;
use aho_corasick::AhoCorasick;
use fff_grep::{Searcher, Sink, SinkMatch};
use smallvec::SmallVec;

/// Result of [`crate::FilePicker::multi_grep_overlapping`]: a [`GrepResult`] whose
/// ranges and `col` are offsets into the full line, plus each range's pattern index.
#[derive(Debug, Clone)]
pub struct OverlappingGrepResult<'a> {
    result: GrepResult<'a>,
    pattern_indexes: Vec<u32>,
    pattern_index_starts: Vec<usize>,
}

impl<'a> OverlappingGrepResult<'a> {
    pub(crate) fn new(result: GrepResult<'a>, pattern_indexes: Vec<u32>) -> Self {
        let mut pattern_index_starts = Vec::with_capacity(result.matches.len() + 1);
        let mut start = 0;
        pattern_index_starts.push(start);
        for m in &result.matches {
            start += m.match_byte_offsets.len();
            pattern_index_starts.push(start);
        }
        Self {
            result,
            pattern_indexes,
            pattern_index_starts,
        }
    }

    /// The matches, with ranges and `col` measured on the full line.
    pub fn result(&self) -> &GrepResult<'a> {
        &self.result
    }

    /// Pattern index of every range, over the matches in order, then range order.
    pub fn pattern_indexes(&self) -> &[u32] {
        &self.pattern_indexes
    }

    /// Pattern indexes of match `i`, one per entry of its `match_byte_offsets`.
    pub fn match_pattern_indexes(&self, i: usize) -> &[u32] {
        &self.pattern_indexes[self.pattern_index_starts[i]..self.pattern_index_starts[i + 1]]
    }

    /// The matches and the flat pattern index array, without the per-match starts.
    pub fn into_parts(self) -> (GrepResult<'a>, Vec<u32>) {
        (self.result, self.pattern_indexes)
    }
}

// Reports every pattern hit on a line over the untruncated line, in end-offset order.
pub(super) struct OverlappingSink<'a> {
    pub(super) state: SinkState<'a>,
    pub(super) ac: &'a AhoCorasick,
    pub(super) trim_whitespace: bool,
    pub(super) pattern_indexes: Vec<u32>,
}

impl Sink for OverlappingSink<'_> {
    type Error = std::io::Error;

    fn matched(&mut self, searcher: &Searcher, mat: &SinkMatch<'_>) -> Result<bool, Self::Error> {
        debug_assert_newline_terminator(searcher);
        let line_bytes = mat.bytes();
        let full_line = strip_line_terminators(line_bytes);
        let (display_bytes, _, line_number, byte_offset) = SinkState::prepare_line(line_bytes, mat);

        let mut match_byte_offsets: SmallVec<[(u32, u32); 4]> = SmallVec::new();
        let indexes_start = self.pattern_indexes.len();
        for m in self.ac.find_overlapping_iter(full_line) {
            match_byte_offsets.push((m.start() as u32, m.end() as u32));
            self.pattern_indexes.push(m.pattern().as_u32());
        }
        let col = match_byte_offsets.iter().map(|r| r.0).min().unwrap_or(0) as usize;

        let Some(is_definition) = self.state.definition_mode.classify_match(
            self.state.file,
            full_line,
            &match_byte_offsets,
            false,
        ) else {
            self.pattern_indexes.truncate(indexes_start);
            return Ok(true);
        };
        let (context_before, context_after) = self.state.extract_context(mat);
        let keep_going = self.state.push_match(
            line_number,
            col,
            byte_offset,
            String::from_utf8_lossy(display_bytes).into_owned(),
            match_byte_offsets,
            is_definition,
            context_before,
            context_after,
        );
        if self.trim_whitespace {
            let m = self
                .state
                .matches
                .last_mut()
                .expect("push_match appends the match");
            let strip_len = m.line_content.len() - m.line_content.trim_start().len();
            m.trim_leading(strip_len, leading_whitespace_len(full_line));
        }
        Ok(keep_going)
    }

    fn finish(&mut self, _: &Searcher, _: &fff_grep::SinkFinish) -> Result<(), Self::Error> {
        Ok(())
    }
}

// Byte length of the leading whitespace `trim_start` strips from the lossy line.
fn leading_whitespace_len(line: &[u8]) -> usize {
    let valid = line.utf8_chunks().next().map_or("", |c| c.valid());
    valid.len() - valid.trim_start().len()
}
