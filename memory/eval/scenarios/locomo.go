package scenarios

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/GizClaw/flowcraft/memory/eval/dataset"
	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/message/media"
)

type locomoScenario struct{}

func (locomoScenario) Name() string      { return "locomo" }
func (locomoScenario) RuntimeID() string { return "eval-locomo" }

// Convert maps upstream snap-research/locomo locomo10.json into the eval
// JSONL schema.
//
// Verified against the upstream file (August 2026, ref main):
//   - dia_id is already globally unique per sample ("D1:3"); evidence
//     references the same identifiers, so no prefixing is applied.
//   - one evidence field contains a semicolon-joined multi-reference
//     ("D8:6; D9:17") and one contains an invalid token ("D"); both are
//     normalized by splitting on ";" and dropping references that do not
//     match a turn dia_id in the same sample.
//   - answers are string or number; 444 adversarial rows have null answers
//     and are skipped (no gold to score against).
func (locomoScenario) Convert(raw []byte) (dataset.Dataset, Stats, error) {
	var samples []loCoMoRawSample
	if err := json.Unmarshal(raw, &samples); err != nil {
		return dataset.Dataset{}, Stats{}, fmt.Errorf("parse locomo10.json: %w", err)
	}
	converted := dataset.Dataset{Name: "locomo"}
	var stats Stats
	for _, sample := range samples {
		if sample.SampleID == "" {
			continue
		}
		speakerA, speakerB := loCoMoSpeakers(sample.Conversation)
		diaIDs, turns := loCoMoFlatten(sample.Conversation, speakerA, speakerB)
		if len(turns) == 0 {
			continue
		}
		stats.Conversations++
		stats.Turns += len(turns)
		converted.Conversations = append(converted.Conversations, dataset.Conversation{
			ID:    sample.SampleID,
			Turns: turns,
		})
		for index, qa := range sample.QA {
			if strings.TrimSpace(qa.Question) == "" {
				continue
			}
			answers := loCoMoAnswers(qa.Answer)
			if len(answers) == 0 {
				stats.SkippedNullAnswer++
				continue
			}
			evidence, dropped := loCoMoEvidence(qa.Evidence, diaIDs)
			stats.DroppedEvidence += dropped
			tags := []string{fmt.Sprintf("cat%d", qa.Category)}
			if name := loCoMoCategoryName(qa.Category); name != "" {
				tags = append(tags, name)
			}
			converted.Questions = append(converted.Questions, dataset.Question{
				ID:             fmt.Sprintf("%s-q%d", sample.SampleID, index+1),
				ConversationID: sample.SampleID,
				Query:          qa.Question,
				GoldAnswers:    answers,
				Tags:           tags,
				EvidenceIDs:    evidence,
			})
			stats.Questions++
		}
	}
	if err := converted.Validate(); err != nil {
		return dataset.Dataset{}, Stats{}, err
	}
	return converted, stats, nil
}

func (locomoScenario) Score(prediction string, question dataset.Question, _ float64, _ bool) (float64, float64, float64, *float64) {
	em, f1 := scoreEMF1(prediction, question.GoldAnswers)
	return em, f1, scoreItemRecall(prediction, question.GoldAnswers), nil
}

func (locomoScenario) Aggregate(scores []QuestionScore) ScoreAggregate {
	return aggregateScores(scores)
}

type loCoMoRawTurn struct {
	Speaker     string   `json:"speaker"`
	DiaID       string   `json:"dia_id"`
	Text        string   `json:"text"`
	Query       string   `json:"query,omitempty"`
	BlipCaption string   `json:"blip_caption,omitempty"`
	ImgURL      []string `json:"img_url,omitempty"`
}

type loCoMoRawQA struct {
	Question string   `json:"question"`
	Answer   any      `json:"answer"`
	Evidence []string `json:"evidence,omitempty"`
	Category int      `json:"category"`
}

type loCoMoRawSample struct {
	SampleID     string                     `json:"sample_id"`
	Conversation map[string]json.RawMessage `json:"conversation"`
	QA           []loCoMoRawQA              `json:"qa"`
}

func loCoMoSpeakers(conversation map[string]json.RawMessage) (string, string) {
	var speakerA, speakerB string
	_ = json.Unmarshal(conversation["speaker_a"], &speakerA)
	_ = json.Unmarshal(conversation["speaker_b"], &speakerB)
	return speakerA, speakerB
}

// loCoMoFlatten returns the conversation's turns in session order together
// with the set of dia_ids present in that sample.
func loCoMoFlatten(
	conversation map[string]json.RawMessage,
	speakerA, speakerB string,
) (map[string]struct{}, []dataset.Turn) {
	type session struct {
		index    int
		key      string
		dateTime string
	}
	var sessions []session
	for key := range conversation {
		if !strings.HasPrefix(key, "session_") || strings.HasSuffix(key, "_date_time") {
			continue
		}
		number, err := strconv.Atoi(strings.TrimPrefix(key, "session_"))
		if err != nil {
			continue
		}
		var dateTime string
		_ = json.Unmarshal(conversation[key+"_date_time"], &dateTime)
		sessions = append(sessions, session{index: number, key: key, dateTime: dateTime})
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].index < sessions[j].index })

	diaIDs := make(map[string]struct{})
	var turns []dataset.Turn
	for _, current := range sessions {
		var rawTurns []loCoMoRawTurn
		if err := json.Unmarshal(conversation[current.key], &rawTurns); err != nil {
			continue
		}
		for _, raw := range rawTurns {
			role := "user"
			switch raw.Speaker {
			case speakerB:
				role = "assistant"
			case speakerA:
				role = "user"
			}
			text := strings.TrimSpace(raw.Text)
			annotation := loCoMoImageAnnotation(raw)
			if text == "" && annotation == "" {
				continue
			}
			body := text
			if annotation != "" {
				if body == "" {
					body = annotation
				} else {
					body += " " + annotation
				}
			}
			speaker := raw.Speaker
			if speaker == "" {
				speaker = role
			}
			content := body
			if current.dateTime != "" {
				content = fmt.Sprintf("[%s] %s: %s", current.dateTime, speaker, body)
			} else {
				content = fmt.Sprintf("%s: %s", speaker, body)
			}
			parts := []message.Part{message.TextPart{Text: content}}
			for _, imageURL := range raw.ImgURL {
				source, sourceErr := media.NewImageURL(imageURL, "")
				if sourceErr == nil {
					parts = append(parts, message.ImagePart{Source: source})
				}
			}
			parts = append(parts, dataset.TurnDataPart(speaker, current.key, current.dateTime, raw.DiaID))
			if raw.DiaID != "" {
				diaIDs[raw.DiaID] = struct{}{}
			}
			turns = append(turns, dataset.Turn{
				Message: message.Message{
					Role:    message.Role(role),
					Content: message.Content{Parts: parts},
				},
				EvidenceID: raw.DiaID,
				SessionID:  current.key,
			})
		}
	}
	return diaIDs, turns
}

func loCoMoImageAnnotation(turn loCoMoRawTurn) string {
	if len(turn.ImgURL) == 0 {
		return ""
	}
	hint := strings.TrimSpace(turn.Query)
	if hint == "" {
		hint = strings.TrimSpace(turn.BlipCaption)
	}
	if hint == "" {
		return ""
	}
	return "[shared image: " + hint + "]"
}

func loCoMoEvidence(evidence []string, diaIDs map[string]struct{}) ([]string, int) {
	seen := make(map[string]struct{})
	var (
		result  []string
		dropped int
	)
	for _, raw := range evidence {
		for _, token := range strings.Split(raw, ";") {
			token = strings.TrimSpace(token)
			if token == "" {
				continue
			}
			if _, ok := diaIDs[token]; !ok {
				dropped++
				continue
			}
			if _, duplicate := seen[token]; duplicate {
				continue
			}
			seen[token] = struct{}{}
			result = append(result, token)
		}
	}
	return result, dropped
}

func loCoMoAnswers(value any) []string {
	switch typed := value.(type) {
	case string:
		if trimmed := strings.TrimSpace(typed); trimmed != "" {
			return []string{trimmed}
		}
	case float64:
		return []string{strconv.FormatFloat(typed, 'f', -1, 64)}
	case []string:
		var answers []string
		for _, item := range typed {
			if trimmed := strings.TrimSpace(item); trimmed != "" {
				answers = append(answers, trimmed)
			}
		}
		return answers
	case []any:
		var answers []string
		for _, item := range typed {
			switch element := item.(type) {
			case string:
				if trimmed := strings.TrimSpace(element); trimmed != "" {
					answers = append(answers, trimmed)
				}
			case float64:
				answers = append(answers, strconv.FormatFloat(element, 'f', -1, 64))
			}
		}
		return answers
	}
	return nil
}

func loCoMoCategoryName(category int) string {
	switch category {
	case 1:
		return "single-hop"
	case 2:
		return "temporal"
	case 3:
		return "multi-hop"
	case 4:
		return "open-domain"
	case 5:
		return "adversarial"
	default:
		return ""
	}
}
