package dataset

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var sessionKeyRE = regexp.MustCompile(`^session_(\d+)$`)

// Load reads a LoCoMo JSON file and normalizes dynamic session fields.
func Load(path string) (*Dataset, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	ds, err := Decode(data)
	if err != nil {
		return nil, err
	}
	ds.Name = filepath.Base(path)
	return ds, nil
}

// Decode accepts an upstream array, a single sample object, or an object
// with samples/data/conversations as its sample array.
func Decode(data []byte) (*Dataset, error) {
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("locomo dataset: decode json: %w", err)
	}
	ds := &Dataset{Name: "locomo"}
	switch v := raw.(type) {
	case []any:
		for i, item := range v {
			sample, err := parseSample(item, i)
			if err != nil {
				return nil, err
			}
			ds.Samples = append(ds.Samples, sample)
		}
	case map[string]any:
		if arr, ok := firstArray(v, "samples", "data", "conversations"); ok {
			if name := stringField(v, "name", "dataset"); name != "" {
				ds.Name = name
			}
			for i, item := range arr {
				sample, err := parseSample(item, i)
				if err != nil {
					return nil, err
				}
				ds.Samples = append(ds.Samples, sample)
			}
		} else {
			sample, err := parseSample(v, 0)
			if err != nil {
				return nil, err
			}
			ds.Samples = append(ds.Samples, sample)
		}
	default:
		return nil, fmt.Errorf("locomo dataset: top-level JSON must be object or array")
	}
	return ds, nil
}

func parseSample(raw any, idx int) (Sample, error) {
	obj, ok := raw.(map[string]any)
	if !ok {
		return Sample{}, fmt.Errorf("locomo dataset: sample %d is %T, want object", idx, raw)
	}
	sample := Sample{
		ID:       stringField(obj, "sample_id", "id", "conversation_id", "uid"),
		Speakers: map[string]string{},
	}
	if sample.ID == "" {
		sample.ID = fmt.Sprintf("sample_%04d", idx+1)
	}

	conv, _ := objectField(obj, "conversation", "dialog", "sessions")
	if conv == nil {
		conv = obj
	}
	maps.Copy(sample.Speakers, collectSpeakers(obj, conv))
	sessions, err := parseSessions(conv)
	if err != nil {
		return Sample{}, fmt.Errorf("locomo dataset: sample %s: %w", sample.ID, err)
	}
	sample.Sessions = sessions
	sample.QA = parseQAItems(anyField(obj, "qa", "questions"))
	sample.EventSummaries = parseEventSummaries(anyField(obj, "event_summary", "event_summaries", "events"))
	sample.DialogCases = buildDialogCases(sample.Sessions)
	return sample, nil
}

func parseSessions(conv map[string]any) ([]Session, error) {
	var sessions []Session
	for key, raw := range conv {
		m := sessionKeyRE.FindStringSubmatch(key)
		if m == nil {
			continue
		}
		idx, _ := strconv.Atoi(m[1])
		turnsRaw, ok := raw.([]any)
		if !ok {
			continue
		}
		session := Session{
			Index:    idx,
			DateTime: stringField(conv, fmt.Sprintf("session_%d_date_time", idx), fmt.Sprintf("session_%d_datetime", idx), fmt.Sprintf("session_%d_date", idx)),
		}
		for i, item := range turnsRaw {
			turn, err := parseTurn(item, idx, i)
			if err != nil {
				return nil, err
			}
			session.Turns = append(session.Turns, turn)
		}
		sessions = append(sessions, session)
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].Index < sessions[j].Index })
	return sessions, nil
}

func parseTurn(raw any, sessionIdx, turnIdx int) (Turn, error) {
	obj, ok := raw.(map[string]any)
	if !ok {
		if s, ok := raw.(string); ok {
			return Turn{SessionIndex: sessionIdx, DiaID: fmt.Sprintf("s%d_t%d", sessionIdx, turnIdx+1), Text: s}, nil
		}
		return Turn{}, fmt.Errorf("session_%d turn %d is %T, want object", sessionIdx, turnIdx, raw)
	}
	imageURLs := stringSliceField(obj, "img_url", "image_url", "image")
	turn := Turn{
		SessionIndex: sessionIdx,
		DiaID:        stringField(obj, "dia_id", "dialog_id", "id", "turn_id"),
		Speaker:      stringField(obj, "speaker", "role", "speaker_id"),
		Text:         stringField(obj, "text", "content", "utterance", "message"),
		ImgURL:       firstString(imageURLs),
		BlipCaption:  stringField(obj, "blip_caption", "caption"),
		Query:        stringField(obj, "query"),
		Metadata:     map[string]any{},
	}
	if turn.DiaID == "" {
		turn.DiaID = fmt.Sprintf("s%d_t%d", sessionIdx, turnIdx+1)
	}
	for k, v := range obj {
		switch k {
		case "dia_id", "dialog_id", "id", "turn_id", "speaker", "role", "speaker_id", "text", "content", "utterance", "message", "img_url", "image_url", "image", "blip_caption", "caption", "query":
			continue
		default:
			turn.Metadata[k] = v
		}
	}
	if len(imageURLs) > 1 {
		turn.Metadata["img_urls"] = imageURLs
	}
	if len(turn.Metadata) == 0 {
		turn.Metadata = nil
	}
	return turn, nil
}

func parseQAItems(raw any) []QAItem {
	arr, ok := raw.([]any)
	if !ok {
		return nil
	}
	items := make([]QAItem, 0, len(arr))
	for i, item := range arr {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		catID := intField(obj, "category", "category_id", "type")
		cat := qaCategoryNames[catID]
		if cat == "" {
			cat = stringField(obj, "category", "category_name")
		}
		q := QAItem{
			ID:                stringField(obj, "id", "qid", "question_id"),
			Question:          stringField(obj, "question", "query"),
			Answer:            stringField(obj, "answer", "target"),
			CategoryID:        catID,
			Category:          cat,
			Evidence:          stringSliceField(obj, "evidence", "evidences"),
			AdversarialAnswer: stringField(obj, "adversarial_answer", "adversarial"),
		}
		if q.ID == "" {
			q.ID = fmt.Sprintf("qa_%04d", i+1)
		}
		items = append(items, q)
	}
	return items
}

func parseEventSummaries(raw any) []EventSummary {
	obj, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	var out []EventSummary
	for key, val := range obj {
		if !strings.HasPrefix(key, "events_session_") {
			continue
		}
		idx, _ := strconv.Atoi(strings.TrimPrefix(key, "events_session_"))
		switch typed := val.(type) {
		case map[string]any:
			for speaker, eventsRaw := range typed {
				if isEventSummaryMetadataKey(speaker) {
					continue
				}
				events := toStringSlice(eventsRaw)
				if len(events) == 0 {
					continue
				}
				out = append(out, EventSummary{SessionIndex: idx, Speaker: speaker, Events: events})
			}
		default:
			events := toStringSlice(typed)
			if len(events) > 0 {
				out = append(out, EventSummary{SessionIndex: idx, Events: events})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SessionIndex == out[j].SessionIndex {
			return out[i].Speaker < out[j].Speaker
		}
		return out[i].SessionIndex < out[j].SessionIndex
	})
	return out
}

func isEventSummaryMetadataKey(key string) bool {
	return strings.EqualFold(strings.TrimSpace(key), "date")
}

func buildDialogCases(sessions []Session) []DialogCase {
	var cases []DialogCase
	for _, session := range sessions {
		for i, turn := range session.Turns {
			if strings.TrimSpace(turn.BlipCaption) == "" && strings.TrimSpace(turn.ImgURL) == "" {
				continue
			}
			var target Turn
			for j := i + 1; j < len(session.Turns); j++ {
				if strings.TrimSpace(session.Turns[j].Text) != "" {
					target = session.Turns[j]
					break
				}
			}
			if target.Text == "" {
				continue
			}
			cases = append(cases, DialogCase{
				ID:              fmt.Sprintf("dialog_s%d_%s", session.Index, turn.DiaID),
				SessionIndex:    session.Index,
				SourceTurnDiaID: turn.DiaID,
				TargetDiaID:     target.DiaID,
				ImageURL:        turn.ImgURL,
				Caption:         turn.BlipCaption,
				Query:           turn.Query,
				Gold:            target.Text,
			})
		}
	}
	return cases
}

func collectSpeakers(objects ...map[string]any) map[string]string {
	out := map[string]string{}
	for _, obj := range objects {
		for key, val := range obj {
			lower := strings.ToLower(key)
			if !strings.HasPrefix(lower, "speaker") {
				continue
			}
			if s, ok := val.(string); ok {
				out[key] = s
			}
		}
	}
	return out
}

func anyField(obj map[string]any, keys ...string) any {
	for _, key := range keys {
		if val, ok := obj[key]; ok {
			return val
		}
	}
	return nil
}

func firstArray(obj map[string]any, keys ...string) ([]any, bool) {
	for _, key := range keys {
		if arr, ok := obj[key].([]any); ok {
			return arr, true
		}
	}
	return nil, false
}

func objectField(obj map[string]any, keys ...string) (map[string]any, bool) {
	for _, key := range keys {
		if val, ok := obj[key].(map[string]any); ok {
			return val, true
		}
	}
	return nil, false
}

func stringField(obj map[string]any, keys ...string) string {
	for _, key := range keys {
		if val, ok := obj[key]; ok {
			if s := toString(val); s != "" {
				return s
			}
		}
	}
	return ""
}

func intField(obj map[string]any, keys ...string) int {
	for _, key := range keys {
		val, ok := obj[key]
		if !ok {
			continue
		}
		switch typed := val.(type) {
		case float64:
			return int(typed)
		case string:
			n, _ := strconv.Atoi(strings.TrimSpace(typed))
			return n
		}
	}
	return 0
}

func stringSliceField(obj map[string]any, keys ...string) []string {
	for _, key := range keys {
		if val, ok := obj[key]; ok {
			return toStringSlice(val)
		}
	}
	return nil
}

func toStringSlice(val any) []string {
	switch typed := val.(type) {
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if s := toString(item); s != "" {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return append([]string(nil), typed...)
	default:
		if s := toString(typed); s != "" {
			return []string{s}
		}
	}
	return nil
}

func firstString(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func toString(val any) string {
	switch typed := val.(type) {
	case string:
		return strings.TrimSpace(typed)
	case float64:
		if typed == float64(int64(typed)) {
			return strconv.FormatInt(int64(typed), 10)
		}
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case bool:
		if typed {
			return "true"
		}
		return "false"
	default:
		return ""
	}
}
