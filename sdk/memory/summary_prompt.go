package memory

const leafPrompt = `Summarize the following conversation segment. Preserve:
- Specific file/code changes and their reasons
- Tool calls and their results
- Concrete decisions made and their rationale
- Timestamps and sequence of events

End with a line: [Expand for details about: <comma-separated key topics>]

Conversation:
%s

Summary:`

const condensedD1Prompt = `Condense the following summaries into a single, shorter summary. Focus on:
- What changed compared to earlier state
- New information and decisions (drop repeated confirmations)
- Key outcomes and their causes

Do NOT repeat information already stated in earlier summaries.
End with: [Expand for details about: <key topics>]

Summaries to condense:
%s

Condensed summary:`

const condensedD2Prompt = `Create a high-level, self-contained narrative from these summaries.
Structure as goal-action-result arcs: "To achieve X, did Y, resulting in Z."
Drop implementation details, keep architectural decisions and outcomes.
End with: [Expand for details about: <key topics>]

Summaries:
%s

Narrative summary:`

const condensedD3Prompt = `Extract only persistent, project-level knowledge from these summaries:
- Architecture decisions that affect future work
- Lessons learned and patterns established
- Key relationships between components
- User preferences and constraints discovered

Summaries:
%s

Persistent knowledge:`

var summaryDepthPrompts = map[int]string{
	0: leafPrompt,
	1: condensedD1Prompt,
	2: condensedD2Prompt,
	3: condensedD3Prompt,
}

// depthPrompt returns the prompt template for the given summary depth.
// Depths >= 3 reuse the d3+ template.
func depthPrompt(depth int) string {
	if p, ok := summaryDepthPrompts[depth]; ok {
		return p
	}
	return condensedD3Prompt
}
