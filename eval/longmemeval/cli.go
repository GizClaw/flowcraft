package longmemeval

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/GizClaw/flowcraft/eval/internal/cliflags"
)

// RegisterCobra attaches the `longmemeval` command group to parent.
// LongMemEval reuses eval/locomo's runner — there is no separate
// `run` subcommand. The group exists so the converter has a
// discoverable home (`eval longmemeval convert`) and so future
// LongMemEval-specific tools (per-category prompts, abstention-aware
// scoring) can land alongside the converter without re-organising the
// CLI tree.
func RegisterCobra(parent *cobra.Command, _ *cliflags.Global) {
	group := &cobra.Command{
		Use:   "longmemeval",
		Short: "LongMemEval helpers (converter; run via `eval locomo run`)",
		Long: `LongMemEval (ICLR 2025) is the LoCoMo successor: 500 instances /
five abilities / haystacks up to 500 sessions long. The data shape
overlaps enough with LoCoMo that the locomo runner can drive it
end-to-end; this group ships the converter and reserves space for
future LongMemEval-specific tooling.

  eval longmemeval convert    upstream longmemeval_*.json → eval JSONL

Run the actual eval with:
  eval locomo run --dataset eval/longmemeval/data/longmemeval_oracle.jsonl ...`,
	}
	addLMEConvert(group)
	parent.AddCommand(group)
}

func addLMEConvert(parent *cobra.Command) {
	var (
		in    string
		out   string
		limit int
	)
	cmd := &cobra.Command{
		Use:   "convert",
		Short: "Convert upstream longmemeval_*.json → eval JSONL",
		Long: `Map LongMemEval instances onto the (Conversation, Question) schema.

  instance.question_id        → Conversation.ID, Question.ID, Question.ConversationID
  instance.haystack_sessions  → Conversation.Turns (flattened, session-tagged)
  instance.haystack_dates     → embedded inline "[<date>] <role>: <text>"
  instance.question           → Question.Query
  instance.answer             → Question.GoldAnswers (single-element)
  instance.question_type      → Question.Tags ["qtype:<type>", "abs" if _abs id]
  instance.question_date      → Question.AskedAt (verbatim, used by the
                                answer prompt to anchor relative-time
                                expressions like "last week")
  haystack turns has_answer   → promoted to Question.EvidenceIDs (turn-
                                level) when present; recall.k_hit then
                                checks "did we recall a gold-evidence
                                turn". Falls back to
                                instance.answer_session_ids (session-
                                level) when no turn carries has_answer.`,
		RunE: func(c *cobra.Command, _ []string) error {
			if in == "" || out == "" {
				return fmt.Errorf("--in and --out are required")
			}
			raw, err := os.ReadFile(in)
			if err != nil {
				return fmt.Errorf("read input: %w", err)
			}
			var instances []lmeRawInstance
			if err := json.Unmarshal(raw, &instances); err != nil {
				return fmt.Errorf("parse input: %w", err)
			}
			if limit > 0 && len(instances) > limit {
				instances = instances[:limit]
			}

			f, err := os.Create(out)
			if err != nil {
				return fmt.Errorf("create output: %w", err)
			}
			defer f.Close()
			w := bufio.NewWriter(f)
			defer w.Flush()
			enc := json.NewEncoder(w)

			var (
				totalConv, totalQ int
				typeCounts        = map[string]int{}
			)
			for _, inst := range instances {
				if inst.QuestionID == "" || len(inst.HaystackSessions) == 0 {
					continue
				}
				convID := inst.QuestionID
				turns, hasAnswerEvIDs := lmeFlattenSessions(inst, convID)
				if len(turns) == 0 {
					continue
				}
				if err := enc.Encode(lmeOutConv{Type: "conversation", ID: convID, Turns: turns}); err != nil {
					return fmt.Errorf("encode conv %s: %w", convID, err)
				}
				totalConv++

				qtype := inst.QuestionType
				tags := []string{"qtype:" + qtype}
				if strings.HasSuffix(inst.QuestionID, "_abs") {
					tags = append(tags, "abs")
				}
				typeCounts[qtype]++

				// EvidenceIDs preference: turn-level (`has_answer`)
				// when ANY turn carries it — every Recall hit on a
				// save_with_context run lands the upstream turn's
				// EvidenceID into MemoryEntry.ID, so turn-level
				// recall.k_hit is the only granularity that
				// matches what `mem.Recall` actually returns. We
				// fall back to session-level `answer_session_ids`
				// only when no turn-level evidence exists so the
				// metric stays meaningful for legacy datasets that
				// pre-date the `has_answer` flag.
				var ev []string
				if len(hasAnswerEvIDs) > 0 {
					ev = hasAnswerEvIDs
				} else {
					ev = make([]string, 0, len(inst.AnswerSessionIDs))
					for _, s := range inst.AnswerSessionIDs {
						if s == "" {
							continue
						}
						ev = append(ev, convID+":"+s)
					}
				}

				gold := []string{lmeAnswerToString(inst.Answer)}
				if gold[0] == "" {
					gold = []string{"(no answer)"}
				}

				if err := enc.Encode(lmeOutQuestion{
					Type:           "question",
					ID:             convID,
					ConversationID: convID,
					Query:          inst.Question,
					GoldAnswers:    gold,
					Tags:           tags,
					EvidenceIDs:    ev,
					AskedAt:        strings.TrimSpace(inst.QuestionDate),
				}); err != nil {
					return fmt.Errorf("encode qa: %w", err)
				}
				totalQ++
			}

			fmt.Printf("wrote %s: %d conversations, %d questions\n", out, totalConv, totalQ)
			fmt.Println("per-type breakdown:")
			for t, n := range typeCounts {
				fmt.Printf("  %-30s %d\n", t, n)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&in, "in", "", "path to upstream longmemeval_*.json")
	cmd.Flags().StringVar(&out, "out", "", "path to write .jsonl dataset")
	cmd.Flags().IntVar(&limit, "limit", 0, "if >0, keep only the first N instances (debug)")
	parent.AddCommand(cmd)
}

type lmeRawTurn struct {
	Role      string `json:"role"`
	Content   string `json:"content"`
	HasAnswer bool   `json:"has_answer,omitempty"`
}

type lmeRawInstance struct {
	QuestionID         string          `json:"question_id"`
	QuestionType       string          `json:"question_type"`
	Question           string          `json:"question"`
	Answer             json.RawMessage `json:"answer"`
	QuestionDate       string          `json:"question_date"`
	HaystackSessionIDs []string        `json:"haystack_session_ids"`
	HaystackDates      []string        `json:"haystack_dates"`
	HaystackSessions   [][]lmeRawTurn  `json:"haystack_sessions"`
	AnswerSessionIDs   []string        `json:"answer_session_ids"`
}

type lmeOutTurn struct {
	Role       string `json:"role"`
	Content    string `json:"content"`
	EvidenceID string `json:"evidence_id,omitempty"`
	SessionID  string `json:"session_id,omitempty"`
	HasAnswer  bool   `json:"has_answer,omitempty"`
}

type lmeOutConv struct {
	Type  string       `json:"type"`
	ID    string       `json:"id"`
	Turns []lmeOutTurn `json:"turns"`
}

type lmeOutQuestion struct {
	Type           string   `json:"type"`
	ID             string   `json:"id"`
	ConversationID string   `json:"conversation_id"`
	Query          string   `json:"query"`
	GoldAnswers    []string `json:"gold_answers"`
	Tags           []string `json:"tags,omitempty"`
	EvidenceIDs    []string `json:"evidence_ids,omitempty"`
	AskedAt        string   `json:"asked_at,omitempty"`
}

func lmeAnswerToString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		return n.String()
	}
	return strings.TrimSpace(string(raw))
}

// lmeFlattenSessions returns the conversation turns AND the list of
// turn-level evidence IDs (those upstream turns whose `has_answer:true`
// marks them as containing the gold evidence for the question).
//
// Turn index `ti` is preserved verbatim across content-empty skips so
// the produced EvidenceID (`{convID}:{sessID}:t{ti}`) stays aligned
// with the upstream turn's position in `haystack_sessions[i]` — that
// alignment is what lets the runner stamp it onto MemoryEntry.ID and
// what makes recall.k_hit meaningful.
func lmeFlattenSessions(inst lmeRawInstance, convID string) ([]lmeOutTurn, []string) {
	var (
		out            []lmeOutTurn
		hasAnswerEvIDs []string
	)
	for i, sess := range inst.HaystackSessions {
		sessID := ""
		if i < len(inst.HaystackSessionIDs) {
			sessID = inst.HaystackSessionIDs[i]
		}
		scopedSessID := ""
		if sessID != "" {
			scopedSessID = convID + ":" + sessID
		}
		date := ""
		if i < len(inst.HaystackDates) {
			date = inst.HaystackDates[i]
		}
		for ti, t := range sess {
			role := strings.ToLower(strings.TrimSpace(t.Role))
			if role != "user" && role != "assistant" {
				role = "user"
			}
			content := strings.TrimSpace(t.Content)
			if content == "" {
				continue
			}
			if date != "" {
				content = "[" + date + "] " + role + ": " + content
			} else {
				content = role + ": " + content
			}
			evID := ""
			if scopedSessID != "" {
				evID = fmt.Sprintf("%s:t%d", scopedSessID, ti)
			}
			out = append(out, lmeOutTurn{
				Role:       role,
				Content:    content,
				EvidenceID: evID,
				SessionID:  scopedSessID,
				HasAnswer:  t.HasAnswer,
			})
			if t.HasAnswer && evID != "" {
				hasAnswerEvIDs = append(hasAnswerEvIDs, evID)
			}
		}
	}
	return out, hasAnswerEvIDs
}
