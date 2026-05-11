package locomo

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// addLocomoConvert wires `eval locomo convert` — the upstream
// snap-research/locomo `locomo10.json` → eval JSONL converter.
//
// Mapping rules (unchanged from the legacy cmd/convert-locomo binary):
//   - One LoCoMo `sample` → one Conversation; turns flattened across
//     sessions in chronological order with "[<datetime>] <speaker>:"
//     content prefixes so the LTM extractor sees timestamps + names
//     inline (without those, LoCoMo's temporal questions are
//     unanswerable).
//   - speaker_a → "user", speaker_b → "assistant".
//   - Each `qa` entry → Question; `evidence` → evidence_ids;
//     `category` → "catN" tag.
func addLocomoConvert(parent *cobra.Command) {
	var (
		in    string
		out   string
		limit int
	)

	cmd := &cobra.Command{
		Use:   "convert",
		Short: "Convert upstream locomo10.json → eval JSONL",
		RunE: func(c *cobra.Command, _ []string) error {
			raw, err := os.ReadFile(in)
			if err != nil {
				return fmt.Errorf("read input: %w", err)
			}
			var samples []convRawSample
			if err := json.Unmarshal(raw, &samples); err != nil {
				return fmt.Errorf("parse input: %w", err)
			}
			if limit > 0 && len(samples) > limit {
				samples = samples[:limit]
			}

			f, err := os.Create(out)
			if err != nil {
				return fmt.Errorf("create output: %w", err)
			}
			defer f.Close()
			w := bufio.NewWriter(f)
			defer w.Flush()
			enc := json.NewEncoder(w)

			totalConv, totalQ := 0, 0
			for _, s := range samples {
				convID := s.SampleID
				speakerA, speakerB := convDecodeSpeakers(s.Conversation)
				turns := convFlattenSessions(s.Conversation, speakerA, speakerB)
				for i := range turns {
					if turns[i].EvidenceID != "" {
						turns[i].EvidenceID = convID + ":" + turns[i].EvidenceID
					}
				}
				if len(turns) == 0 {
					continue
				}
				if err := enc.Encode(convOutConv{Type: "conversation", ID: convID, Turns: turns}); err != nil {
					return fmt.Errorf("encode conv %s: %w", convID, err)
				}
				totalConv++

				for i, q := range s.QA {
					if q.Question == "" || q.Answer == nil {
						continue
					}
					ans := convAnswerToStrings(q.Answer)
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
					row := convOutQuestion{
						Type:           "question",
						ID:             fmt.Sprintf("%s-q%d", convID, i+1),
						ConversationID: convID,
						Query:          q.Question,
						GoldAnswers:    ans,
						Tags:           tags,
						EvidenceIDs:    ev,
					}
					if err := enc.Encode(row); err != nil {
						return fmt.Errorf("encode qa: %w", err)
					}
					totalQ++
				}
			}
			fmt.Printf("wrote %s: %d conversations, %d questions\n", out, totalConv, totalQ)
			return nil
		},
	}

	f := cmd.Flags()
	f.StringVar(&in, "in", "eval/locomo/data/locomo/data/locomo10.json", "path to upstream locomo10.json")
	f.StringVar(&out, "out", "eval/locomo/data/locomo10.jsonl", "path to write .jsonl dataset")
	f.IntVar(&limit, "limit", 0, "if >0, keep only the first N samples (debug)")

	parent.AddCommand(cmd)
}

type convRawTurn struct {
	Speaker string `json:"speaker"`
	DiaID   string `json:"dia_id"`
	Text    string `json:"text"`
}

type convRawQA struct {
	Question  string   `json:"question"`
	Answer    any      `json:"answer"`
	AdvAnswer any      `json:"adversarial_answer,omitempty"`
	Evidence  []string `json:"evidence,omitempty"`
	Category  int      `json:"category"`
}

type convRawSample struct {
	SampleID     string                     `json:"sample_id"`
	Conversation map[string]json.RawMessage `json:"conversation"`
	QA           []convRawQA                `json:"qa"`
}

type convOutConvTurn struct {
	Role       string `json:"role"`
	Content    string `json:"content"`
	EvidenceID string `json:"evidence_id,omitempty"`
	SessionID  string `json:"session_id,omitempty"`
}

type convOutConv struct {
	Type  string            `json:"type"`
	ID    string            `json:"id"`
	Turns []convOutConvTurn `json:"turns"`
}

type convOutQuestion struct {
	Type           string   `json:"type"`
	ID             string   `json:"id"`
	ConversationID string   `json:"conversation_id"`
	Query          string   `json:"query"`
	GoldAnswers    []string `json:"gold_answers"`
	Tags           []string `json:"tags,omitempty"`
	EvidenceIDs    []string `json:"evidence_ids,omitempty"`
}

func convDecodeSpeakers(c map[string]json.RawMessage) (string, string) {
	var a, b string
	_ = json.Unmarshal(c["speaker_a"], &a)
	_ = json.Unmarshal(c["speaker_b"], &b)
	return a, b
}

func convFlattenSessions(c map[string]json.RawMessage, speakerA, speakerB string) []convOutConvTurn {
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

	var out []convOutConvTurn
	for _, s := range sessions {
		var turns []convRawTurn
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
			out = append(out, convOutConvTurn{Role: role, Content: content, EvidenceID: t.DiaID, SessionID: s.key})
		}
	}
	return out
}

func convAnswerToStrings(v any) []string {
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
			out = append(out, convAnswerToStrings(x)...)
		}
		return out
	}
	return nil
}
