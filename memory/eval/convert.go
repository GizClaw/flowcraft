package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// conversionStats reports how much of the upstream corpus survived
// normalization. Everything dropped is intentional and visible.
type conversionStats struct {
	Conversations     int `json:"conversations"`
	Questions         int `json:"questions"`
	Turns             int `json:"turns"`
	DroppedEvidence   int `json:"dropped_evidence"`
	SkippedNullAnswer int `json:"skipped_null_answer"`
}

// convertLoCoMo maps upstream snap-research/locomo locomo10.json into the
// eval JSONL schema.
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
func convertLoCoMo(raw []byte) (Dataset, conversionStats, error) {
	var samples []loCoMoRawSample
	if err := json.Unmarshal(raw, &samples); err != nil {
		return Dataset{}, conversionStats{}, fmt.Errorf("parse locomo10.json: %w", err)
	}
	dataset := Dataset{Name: "locomo"}
	var stats conversionStats
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
		dataset.Conversations = append(dataset.Conversations, Conversation{
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
			dataset.Questions = append(dataset.Questions, Question{
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
	if err := dataset.Validate(); err != nil {
		return Dataset{}, conversionStats{}, err
	}
	return dataset, stats, nil
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
) (map[string]struct{}, []Turn) {
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
	var turns []Turn
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
			if raw.DiaID != "" {
				diaIDs[raw.DiaID] = struct{}{}
			}
			turns = append(turns, Turn{
				Role:       role,
				Content:    content,
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
	case json.Number:
		return []string{typed.String()}
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

// convertLongMemEval maps upstream xiaowu0162/longmemeval-cleaned JSON into
// the eval JSONL schema.
//
// Verified against longmemeval_oracle.json (August 2026):
//   - haystack_sessions / haystack_session_ids / haystack_dates are parallel
//     arrays but are NOT in chronological order; sessions are reordered by
//     haystack_dates before ingestion so temporal questions have a coherent
//     history.
//   - every turn carries has_answer; turns with has_answer=true are the
//     turn-level gold evidence. answer_session_ids is only a fallback for
//     variants that predate the flag.
func convertLongMemEval(raw []byte) (Dataset, conversionStats, error) {
	var instances []lmeRawInstance
	if err := json.Unmarshal(raw, &instances); err != nil {
		return Dataset{}, conversionStats{}, fmt.Errorf("parse longmemeval JSON: %w", err)
	}
	dataset := Dataset{Name: "longmemeval"}
	var stats conversionStats
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
		dataset.Conversations = append(dataset.Conversations, Conversation{
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
		dataset.Questions = append(dataset.Questions, Question{
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
	if err := dataset.Validate(); err != nil {
		return Dataset{}, conversionStats{}, err
	}
	return dataset, stats, nil
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

func lmeFlatten(instance lmeRawInstance, conversationID string) ([]Turn, []string) {
	var (
		turns        []Turn
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
			turns = append(turns, Turn{
				Role:       role,
				Content:    body,
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

func printConversionStats(suite string, stats conversionStats) {
	fmt.Fprintf(os.Stderr,
		"%s conversion: %d conversations, %d turns, %d questions; "+
			"dropped %d evidence refs, skipped %d null-answer questions\n",
		suite, stats.Conversations, stats.Turns, stats.Questions,
		stats.DroppedEvidence, stats.SkippedNullAnswer,
	)
}

// applyConversationLimit keeps the first N conversations and only the questions
// that belong to them, so a truncated dataset stays internally consistent.
func applyConversationLimit(dataset *Dataset, limit int) {
	if limit <= 0 || len(dataset.Conversations) <= limit {
		return
	}
	dataset.Conversations = dataset.Conversations[:limit]
	kept := make(map[string]struct{}, len(dataset.Conversations))
	for _, conversation := range dataset.Conversations {
		kept[conversation.ID] = struct{}{}
	}
	questions := dataset.Questions[:0]
	for _, question := range dataset.Questions {
		if _, ok := kept[question.ConversationID]; ok {
			questions = append(questions, question)
		}
	}
	dataset.Questions = questions
}
