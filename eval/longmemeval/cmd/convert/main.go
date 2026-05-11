// Command convert-longmemeval maps the upstream LongMemEval JSON files
// (longmemeval_oracle.json, longmemeval_s.json, longmemeval_m.json) onto
// the .jsonl shape consumed by eval/locomo/cmd/eval.
//
// Usage:
//
//	go run ./eval/longmemeval/cmd/convert \
//	    -in  eval/longmemeval/data/longmemeval_oracle.json \
//	    -out eval/longmemeval/data/longmemeval_oracle.jsonl
//
// LongMemEval ships 500 *independent* instances per file — each instance
// has its own haystack (~40 sessions for _s, ~500 for _m) and exactly one
// question. We map this onto our (Conversation, Question) schema by giving
// each instance a Conversation named after question_id and a single
// Question scoped to it. That keeps eval/locomo's per-conversation user_id
// scoping working (one conv = one isolated memory store, no cross-instance
// pollution).
//
// Field mapping:
//
//	instance.question_id       -> Conversation.ID, Question.ID, Question.ConversationID
//	instance.haystack_sessions -> Conversation.Turns (flattened, session-tagged)
//	instance.haystack_dates    -> embedded inline as "[<date>] <role>: <text>"
//	instance.question          -> Question.Query
//	instance.answer            -> Question.GoldAnswers (single-element)
//	instance.question_type     -> Question.Tags ["qtype:<type>", "abs" if _abs id]
//	instance.answer_session_ids-> Question.EvidenceIDs (session-level grain)
//
// The has_answer turn-level marker is currently dropped because our
// `Question.EvidenceIDs` field is matched against `MemoryEntry.ID` and
// LongMemEval's session-level evidence id is more reliable than per-turn.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
)

type rawTurn struct {
	Role      string `json:"role"`
	Content   string `json:"content"`
	HasAnswer bool   `json:"has_answer,omitempty"`
}

type rawInstance struct {
	QuestionID   string `json:"question_id"`
	QuestionType string `json:"question_type"`
	Question     string `json:"question"`
	// Answer is loosely typed: most instances are strings but a non-trivial
	// minority (e.g. counting questions like "how many times did I…?") arrive
	// as raw integers. Decoded via answerToString below.
	Answer             json.RawMessage `json:"answer"`
	QuestionDate       string          `json:"question_date"`
	HaystackSessionIDs []string        `json:"haystack_session_ids"`
	HaystackDates      []string        `json:"haystack_dates"`
	HaystackSessions   [][]rawTurn     `json:"haystack_sessions"`
	AnswerSessionIDs   []string        `json:"answer_session_ids"`
}

type outTurn struct {
	Role       string `json:"role"`
	Content    string `json:"content"`
	EvidenceID string `json:"evidence_id,omitempty"`
	SessionID  string `json:"session_id,omitempty"`
}

type outConv struct {
	Type  string    `json:"type"`
	ID    string    `json:"id"`
	Turns []outTurn `json:"turns"`
}

type outQuestion struct {
	Type           string   `json:"type"`
	ID             string   `json:"id"`
	ConversationID string   `json:"conversation_id"`
	Query          string   `json:"query"`
	GoldAnswers    []string `json:"gold_answers"`
	Tags           []string `json:"tags,omitempty"`
	EvidenceIDs    []string `json:"evidence_ids,omitempty"`
}

func main() {
	in := flag.String("in", "", "path to upstream longmemeval_*.json")
	out := flag.String("out", "", "path to write .jsonl dataset")
	limit := flag.Int("limit", 0, "if >0, keep only the first N instances (debug)")
	flag.Parse()

	if *in == "" || *out == "" {
		log.Fatal("--in and --out are required")
	}

	raw, err := os.ReadFile(*in)
	if err != nil {
		log.Fatalf("read input: %v", err)
	}
	var instances []rawInstance
	if err := json.Unmarshal(raw, &instances); err != nil {
		log.Fatalf("parse input: %v", err)
	}
	if *limit > 0 && len(instances) > *limit {
		instances = instances[:*limit]
	}

	f, err := os.Create(*out)
	if err != nil {
		log.Fatalf("create output: %v", err)
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
		turns := flattenSessions(inst, convID)
		if len(turns) == 0 {
			continue
		}

		if err := enc.Encode(outConv{Type: "conversation", ID: convID, Turns: turns}); err != nil {
			log.Fatalf("encode conv %s: %v", convID, err)
		}
		totalConv++

		// Tag the question with its type so per-category breakdown works
		// post-hoc. Abstention instances carry an _abs suffix on the id
		// (per LongMemEval README) — surface it as its own tag.
		qtype := inst.QuestionType
		tags := []string{"qtype:" + qtype}
		if strings.HasSuffix(inst.QuestionID, "_abs") {
			tags = append(tags, "abs")
		}
		typeCounts[qtype]++

		// Scope session evidence ids to the conv id to keep them globally
		// unique (LongMemEval session ids like "s_2024-04-12_005" can
		// repeat across instances if the same backbone session is reused).
		ev := make([]string, 0, len(inst.AnswerSessionIDs))
		for _, s := range inst.AnswerSessionIDs {
			if s == "" {
				continue
			}
			ev = append(ev, convID+":"+s)
		}

		// LongMemEval answers are usually strings, occasionally integers
		// (count questions). Normalize to []string so any-match EM works.
		gold := []string{answerToString(inst.Answer)}
		if gold[0] == "" {
			// Abstention instances sometimes carry an empty answer
			// ("I don't know"-style). Sentinel keeps EM/F1 well-defined;
			// qa.judge is the real signal for these anyway.
			gold = []string{"(no answer)"}
		}

		if err := enc.Encode(outQuestion{
			Type:           "question",
			ID:             convID,
			ConversationID: convID,
			Query:          inst.Question,
			GoldAnswers:    gold,
			Tags:           tags,
			EvidenceIDs:    ev,
		}); err != nil {
			log.Fatalf("encode qa: %v", err)
		}
		totalQ++
	}

	fmt.Printf("wrote %s: %d conversations, %d questions\n", *out, totalConv, totalQ)
	fmt.Println("per-type breakdown:")
	for t, n := range typeCounts {
		fmt.Printf("  %-30s %d\n", t, n)
	}
}

// answerToString decodes the loosely-typed `answer` field. LongMemEval
// ships strings for most question types and bare JSON numbers for the
// counting questions; we surface either as a trimmed string so EM/F1
// stays well-defined and the judge sees a uniform shape.
func answerToString(raw json.RawMessage) string {
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
	// Last resort: leave the raw JSON in (e.g. arrays) so the question
	// isn't silently dropped — judge can still grade textual overlap.
	return strings.TrimSpace(string(raw))
}

// flattenSessions concatenates every haystack session into a single
// chronological turn slice and prefixes each turn's content with its
// session date — same trick the LoCoMo converter uses so temporal-
// reasoning questions ("When did the user mention X?") are answerable
// from text alone. Each turn also carries its SessionID so the eval
// runner can chunk extractor calls per session and stay inside the
// model's output budget.
func flattenSessions(inst rawInstance, convID string) []outTurn {
	var out []outTurn
	for i, sess := range inst.HaystackSessions {
		sessID := ""
		if i < len(inst.HaystackSessionIDs) {
			sessID = inst.HaystackSessionIDs[i]
		}
		// scope session id to the conv to dodge cross-instance collisions
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
			// Embed (session_id, turn_idx) into evidence_id so raw-ingest
			// mode (SaveRaw) preserves turn-level provenance. has_answer
			// turns are not specially marked because session-level
			// evidence already gives a reliable k_hit signal.
			evID := ""
			if scopedSessID != "" {
				evID = fmt.Sprintf("%s:t%d", scopedSessID, ti)
			}
			out = append(out, outTurn{
				Role:       role,
				Content:    content,
				EvidenceID: evID,
				SessionID:  scopedSessID,
			})
		}
	}
	return out
}
