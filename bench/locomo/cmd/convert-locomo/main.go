// Command convert-locomo turns the upstream snap-research/locomo
// `locomo10.json` into the `.jsonl` format consumed by `cmd/eval`.
//
// Usage:
//
//	go run ./bench/locomo/cmd/convert-locomo \
//	    -in  bench/locomo/data/locomo/data/locomo10.json \
//	    -out bench/locomo/data/locomo10.jsonl
//
// Mapping rules:
//
//   - One LoCoMo `sample` -> one Conversation; turns are flattened across all
//     sessions in chronological order.
//   - speaker_a -> "user", speaker_b -> "assistant" (LoCoMo conversations are
//     symmetric; we pick a stable mapping so the LTM extractor sees a clear
//     "user side").
//   - Each `qa` entry becomes a Question; `evidence` becomes evidence_ids.
//   - `category` (1..5) becomes a single tag like "cat1".
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
)

type rawTurn struct {
	Speaker string `json:"speaker"`
	DiaID   string `json:"dia_id"`
	Text    string `json:"text"`
	// Other fields (img_url, blip_caption, ...) are ignored.
}

type rawQA struct {
	Question  string   `json:"question"`
	Answer    any      `json:"answer"` // sometimes string, sometimes number
	AdvAnswer any      `json:"adversarial_answer,omitempty"`
	Evidence  []string `json:"evidence,omitempty"`
	Category  int      `json:"category"`
}

type rawSample struct {
	SampleID     string                     `json:"sample_id"`
	Conversation map[string]json.RawMessage `json:"conversation"`
	QA           []rawQA                    `json:"qa"`
}

type outConvTurn struct {
	Role       string `json:"role"`
	Content    string `json:"content"`
	EvidenceID string `json:"evidence_id,omitempty"`
	SessionID  string `json:"session_id,omitempty"`
}

type outConv struct {
	Type  string        `json:"type"`
	ID    string        `json:"id"`
	Turns []outConvTurn `json:"turns"`
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
	in := flag.String("in", "bench/locomo/data/locomo/data/locomo10.json", "path to upstream locomo10.json")
	out := flag.String("out", "bench/locomo/data/locomo10.jsonl", "path to write .jsonl dataset")
	limit := flag.Int("limit", 0, "if >0, keep only the first N samples (debug)")
	flag.Parse()

	raw, err := os.ReadFile(*in)
	if err != nil {
		log.Fatalf("read input: %v", err)
	}

	var samples []rawSample
	if err := json.Unmarshal(raw, &samples); err != nil {
		log.Fatalf("parse input: %v", err)
	}
	if *limit > 0 && len(samples) > *limit {
		samples = samples[:*limit]
	}

	f, err := os.Create(*out)
	if err != nil {
		log.Fatalf("create output: %v", err)
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	defer w.Flush()
	enc := json.NewEncoder(w)

	totalConv, totalQ := 0, 0
	for _, s := range samples {
		convID := s.SampleID
		speakerA, speakerB := decodeSpeakers(s.Conversation)
		turns := flattenSessions(s.Conversation, speakerA, speakerB)
		// Make dia_ids unique across samples (LoCoMo restarts D1:1 per sample).
		for i := range turns {
			if turns[i].EvidenceID != "" {
				turns[i].EvidenceID = convID + ":" + turns[i].EvidenceID
			}
		}
		if len(turns) == 0 {
			continue
		}
		if err := enc.Encode(outConv{Type: "conversation", ID: convID, Turns: turns}); err != nil {
			log.Fatalf("encode conv %s: %v", convID, err)
		}
		totalConv++

		for i, q := range s.QA {
			if q.Question == "" || q.Answer == nil {
				continue
			}
			ans := answerToStrings(q.Answer)
			if len(ans) == 0 {
				continue
			}
			tags := []string{fmt.Sprintf("cat%d", q.Category)}
			ev := make([]string, 0, len(q.Evidence))
			for _, e := range q.Evidence {
				if e == "" {
					continue
				}
				ev = append(ev, convID+":"+e)
			}
			out := outQuestion{
				Type:           "question",
				ID:             fmt.Sprintf("%s-q%d", convID, i+1),
				ConversationID: convID,
				Query:          q.Question,
				GoldAnswers:    ans,
				Tags:           tags,
				EvidenceIDs:    ev,
			}
			if err := enc.Encode(out); err != nil {
				log.Fatalf("encode qa: %v", err)
			}
			totalQ++
		}
	}
	fmt.Printf("wrote %s: %d conversations, %d questions\n", *out, totalConv, totalQ)
}

// decodeSpeakers pulls speaker_a / speaker_b names from the conversation map.
func decodeSpeakers(c map[string]json.RawMessage) (string, string) {
	var a, b string
	_ = json.Unmarshal(c["speaker_a"], &a)
	_ = json.Unmarshal(c["speaker_b"], &b)
	return a, b
}

// flattenSessions iterates session_1, session_2, ... in numeric order, mapping
// speaker_a -> user, speaker_b -> assistant, anyone else -> user.
//
// Each turn's Content is prefixed with "[<session date>] <speaker>:" so the
// LTM extractor sees timestamps + speaker identity inline — without this,
// LoCoMo's temporal questions ("When did Caroline …?") are unanswerable.
func flattenSessions(c map[string]json.RawMessage, speakerA, speakerB string) []outConvTurn {
	type sess struct {
		idx      int
		key      string
		dateTime string
	}
	var sessions []sess
	for k := range c {
		if !strings.HasPrefix(k, "session_") || strings.HasSuffix(k, "_date_time") {
			continue
		}
		nStr := strings.TrimPrefix(k, "session_")
		n, err := strconv.Atoi(nStr)
		if err != nil {
			continue
		}
		var dt string
		if raw, ok := c[k+"_date_time"]; ok {
			_ = json.Unmarshal(raw, &dt)
		}
		sessions = append(sessions, sess{idx: n, key: k, dateTime: dt})
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].idx < sessions[j].idx })

	var out []outConvTurn
	for _, s := range sessions {
		var turns []rawTurn
		if err := json.Unmarshal(c[s.key], &turns); err != nil {
			continue
		}
		for _, t := range turns {
			role := "user"
			switch t.Speaker {
			case speakerB:
				role = "assistant"
			case speakerA:
				role = "user"
			}
			text := strings.TrimSpace(t.Text)
			if text == "" {
				continue
			}
			speaker := t.Speaker
			if speaker == "" {
				speaker = role
			}
			content := text
			if s.dateTime != "" {
				content = fmt.Sprintf("[%s] %s: %s", s.dateTime, speaker, text)
			} else {
				content = fmt.Sprintf("%s: %s", speaker, text)
			}
			out = append(out, outConvTurn{Role: role, Content: content, EvidenceID: t.DiaID, SessionID: s.key})
		}
	}
	return out
}

// answerToStrings normalizes the polymorphic answer field into a slice of
// gold-answer strings. Numeric answers are converted to their string form;
// pre-existing slices are returned as-is.
func answerToStrings(v any) []string {
	switch t := v.(type) {
	case string:
		t = strings.TrimSpace(t)
		if t == "" {
			return nil
		}
		return []string{t}
	case float64:
		return []string{strconv.FormatFloat(t, 'f', -1, 64)}
	case bool:
		return []string{strconv.FormatBool(t)}
	case []any:
		var out []string
		for _, x := range t {
			out = append(out, answerToStrings(x)...)
		}
		return out
	}
	return nil
}
