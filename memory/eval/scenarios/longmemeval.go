package scenarios

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/memory/eval/dataset"
	"github.com/GizClaw/flowcraft/sdk/message"
)

type longmemevalScenario struct{}

func (longmemevalScenario) Name() string      { return "longmemeval" }
func (longmemevalScenario) RuntimeID() string { return "eval-lme" }

// Convert maps upstream xiaowu0162/longmemeval-cleaned JSON into the eval
// JSONL schema.
//
// Verified against longmemeval_oracle.json (August 2026):
//   - haystack_sessions / haystack_session_ids / haystack_dates are parallel
//     arrays but are NOT in chronological order; sessions are reordered by
//     haystack_dates before ingestion so temporal questions have a coherent
//     history.
//   - every turn carries has_answer; turns with has_answer=true are the
//     turn-level gold evidence. answer_session_ids is only a fallback for
//     variants that predate the flag.
func (longmemevalScenario) Convert(raw []byte) (dataset.Dataset, Stats, error) {
	var instances []lmeRawInstance
	if err := json.Unmarshal(raw, &instances); err != nil {
		return dataset.Dataset{}, Stats{}, fmt.Errorf("parse longmemeval JSON: %w", err)
	}
	converted := dataset.Dataset{Name: "longmemeval"}
	var stats Stats
	for _, instance := range instances {
		conversationID := instance.QuestionID
		if conversationID == "" || len(instance.HaystackSessions) == 0 {
			continue
		}
		turns, hasAnswerIDs := lmeFlatten(instance, conversationID)
		if len(turns) == 0 {
			continue
		}
		stats.Conversations++
		stats.Turns += len(turns)
		converted.Conversations = append(converted.Conversations, dataset.Conversation{
			ID:    conversationID,
			Turns: turns,
		})
		evidence := hasAnswerIDs
		if len(evidence) == 0 {
			evidence = lmeFallbackEvidence(instance, conversationID)
		}
		gold := lmeAnswer(instance.Answer)
		if gold == "" {
			gold = "(no answer)"
		}
		tags := []string{"qtype:" + instance.QuestionType}
		if strings.HasSuffix(conversationID, "_abs") {
			tags = append(tags, "abs")
		}
		converted.Questions = append(converted.Questions, dataset.Question{
			ID:             conversationID,
			ConversationID: conversationID,
			Query:          instance.Question,
			GoldAnswers:    []string{gold},
			Tags:           tags,
			EvidenceIDs:    evidence,
			AskedAt:        strings.TrimSpace(instance.QuestionDate),
		})
		stats.Questions++
	}
	if err := converted.Validate(); err != nil {
		return dataset.Dataset{}, Stats{}, err
	}
	return converted, stats, nil
}

func (longmemevalScenario) Score(prediction string, question dataset.Question, _ float64, _ bool) (float64, float64, *float64) {
	em, f1 := scoreEMF1(prediction, question.GoldAnswers)
	if !isAbstentionQuestion(question) {
		return em, f1, nil
	}
	abstained := hasAbstained(prediction)
	return em, f1, boolFloatPtr(abstained)
}

func (longmemevalScenario) Aggregate(scores []QuestionScore) ScoreAggregate {
	aggregate := aggregateScores(scores)
	aggregate.Abstain = abstainMean(scores)
	return aggregate
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

func lmeFlatten(instance lmeRawInstance, conversationID string) ([]dataset.Turn, []string) {
	var (
		turns        []dataset.Turn
		hasAnswerIDs []string
	)
	for _, sessionIndex := range lmeSessionOrder(instance) {
		session := instance.HaystackSessions[sessionIndex]
		sessionID := ""
		if sessionIndex < len(instance.HaystackSessionIDs) {
			sessionID = strings.TrimSpace(instance.HaystackSessionIDs[sessionIndex])
		}
		scopedSessionID := conversationID
		if sessionID != "" {
			scopedSessionID = conversationID + ":" + sessionID
		}
		date := ""
		if sessionIndex < len(instance.HaystackDates) {
			date = strings.TrimSpace(instance.HaystackDates[sessionIndex])
		}
		for turnIndex, raw := range session {
			content := strings.TrimSpace(raw.Content)
			if content == "" {
				continue
			}
			role := strings.ToLower(strings.TrimSpace(raw.Role))
			if role != "user" && role != "assistant" {
				role = "user"
			}
			body := content
			if date != "" {
				body = fmt.Sprintf("[%s] %s: %s", date, role, content)
			} else {
				body = fmt.Sprintf("%s: %s", role, content)
			}
			evidenceID := lmeTurnEvidenceID(conversationID, sessionID, turnIndex)
			parts := []message.Part{
				message.TextPart{Text: body},
				dataset.TurnDataPart(role, sessionID, date, evidenceID),
			}
			turns = append(turns, dataset.Turn{
				Message: message.Message{
					Role:    message.Role(role),
					Content: message.Content{Parts: parts},
				},
				EvidenceID: evidenceID,
				SessionID:  scopedSessionID,
			})
			if raw.HasAnswer {
				hasAnswerIDs = append(hasAnswerIDs, evidenceID)
			}
		}
	}
	return turns, hasAnswerIDs
}

// lmeSessionOrder returns session indices in chronological order by
// haystack_dates. The upstream arrays are parallel but not sorted. If any
// date is empty or unparsable the original order is preserved so malformed
// variants fail loudly in review rather than being silently reordered.
func lmeSessionOrder(instance lmeRawInstance) []int {
	order := make([]int, len(instance.HaystackSessions))
	for index := range order {
		order[index] = index
	}
	if len(instance.HaystackDates) < len(instance.HaystackSessions) {
		return order
	}
	parsed := make([]time.Time, len(order))
	for index := range order {
		value, err := time.Parse("2006/01/02 (Mon) 15:04", strings.TrimSpace(instance.HaystackDates[index]))
		if err != nil {
			return order
		}
		parsed[index] = value
	}
	sort.SliceStable(order, func(a, b int) bool {
		return parsed[order[a]].Before(parsed[order[b]])
	})
	return order
}

func lmeTurnEvidenceID(conversationID, sessionID string, turnIndex int) string {
	if sessionID == "" {
		return fmt.Sprintf("%s:t%d", conversationID, turnIndex)
	}
	return fmt.Sprintf("%s:%s:t%d", conversationID, sessionID, turnIndex)
}

func lmeFallbackEvidence(instance lmeRawInstance, conversationID string) []string {
	wanted := make(map[string]struct{}, len(instance.AnswerSessionIDs))
	for _, id := range instance.AnswerSessionIDs {
		if id = strings.TrimSpace(id); id != "" {
			wanted[id] = struct{}{}
		}
	}
	if len(wanted) == 0 {
		return nil
	}
	seen := make(map[string]struct{})
	var evidence []string
	for _, sessionIndex := range lmeSessionOrder(instance) {
		sessionID := ""
		if sessionIndex < len(instance.HaystackSessionIDs) {
			sessionID = strings.TrimSpace(instance.HaystackSessionIDs[sessionIndex])
		}
		if _, ok := wanted[sessionID]; !ok {
			continue
		}
		for turnIndex, turn := range instance.HaystackSessions[sessionIndex] {
			if strings.TrimSpace(turn.Content) == "" {
				continue
			}
			evidenceID := lmeTurnEvidenceID(conversationID, sessionID, turnIndex)
			if _, duplicate := seen[evidenceID]; duplicate {
				continue
			}
			seen[evidenceID] = struct{}{}
			evidence = append(evidence, evidenceID)
		}
	}
	return evidence
}

func lmeAnswer(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return strings.TrimSpace(text)
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err == nil {
		return number.String()
	}
	return strings.TrimSpace(string(raw))
}

func boolFloatPtr(value bool) *float64 {
	number := 0.0
	if value {
		number = 1
	}
	return &number
}
