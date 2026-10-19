//! `core/skill/render.go`: the per-turn "## Skills" section.
//!
//! The section names the paired tools by convention -- this crate renders it,
//! and the application ships `skill_search` and `skill_read` on top of
//! [`Service::rank_scored`](crate::Service::rank_scored) and
//! [`Service::read_full`](crate::Service::read_full) -- and lists one line per
//! skill. A body is never inlined: the model opens one on demand.

use crate::parse::{MAX_DESCRIPTION_LEN, Metadata, truncate_runes};

/// The fixed preamble of the per-turn section.
pub const RENDER_HEADER: &str = "## Skills\nSkills relevant to this turn. To use one, search with \
     skill_search and load its full instructions with skill_read. The user can also activate a \
     skill by mentioning $name.\n";

/// Renders the per-turn "## Skills" list.
///
/// One line per skill carries the name, the description and the file path; an
/// empty list renders the empty string.
pub fn render_section(skills: &[Metadata]) -> String {
    if skills.is_empty() {
        return String::new();
    }
    let mut out = String::from(RENDER_HEADER);
    for skill in skills {
        out.push_str("- ");
        out.push_str(&skill.name);
        out.push_str(": ");
        out.push_str(&truncate_runes(&skill.description, MAX_DESCRIPTION_LEN));
        out.push_str(" (file: ");
        out.push_str(&skill.path);
        out.push_str(")\n");
    }
    out.trim_end_matches('\n').to_owned()
}
