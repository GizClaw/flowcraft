package graph

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/GizClaw/flowcraft/core/agent"
)

// Errors that explain why a value stays off the event stream. Neither fails a run:
// a report is for observers, and a step's writes are already durable.
var (
	// ErrInvalidStateJSON is a json key that does not hold a document.
	ErrInvalidStateJSON = errors.New("graph: state key holds invalid JSON")
	// ErrUnreportableState is a value kind a surface has nothing to render from.
	ErrUnreportableState = errors.New("graph: state value is not reported on the stream")
)

// StateName is the payload name the graph puts on the event stream.
//
// It is not the checkpoint's record kind (that one is "graph.state", one record per
// written key): an event is a live report of a step, and the store's records are the
// durable form of a key. Same fact, different carrier, so different name.
const StateName = "graph.step"

// GraphState is what one step changed.
//
// It is emitted only when the document sets `stream.state`, because building one
// costs a JSON projection of every value the step wrote.
//
// It is a delta, not a snapshot: the keys this step wrote, in the encoding the
// graph promises - text is a JSON string, json is its document, bool is a bool, and
// messages is the run the step appended, not the whole transcript. A consumer that
// keeps a projection appends the messages delta; one that replaces its copy loses
// the history. Media values are not reported: a blob reference is not a fact a
// surface renders a decision from, and a task's artifacts already carry them.
type GraphState struct {
	// Step is the run's step ordinal, 1-based like Status.Reason and the graph.step
	// span attribute, so a consumer can notice a missed event.
	Step int
	// Values are the changed keys, in the state's own JSON domain.
	Values map[string]json.RawMessage
}

// stateEvent renders one step's writes for the live stream, when the document
// asked for them, or reports that there is nothing to send.
//
// The projection costs a JSON encoding per written value, which is the whole
// reason it is opt-in: a run nobody is watching should not pay for it.
func (r *Run) stateEvent(updates []StateUpdate) (agent.Event, bool) {
	if !r.g.stateEvents {
		return nil, false
	}
	return graphStateEvent(r.step+1, updates)
}

// graphStateEvent renders one step's writes as the extension event the host sees,
// or reports that the step changed nothing.
func graphStateEvent(step int, updates []StateUpdate) (agent.Event, bool) {
	if len(updates) == 0 {
		return nil, false
	}
	values := make(map[string]json.RawMessage, len(updates))
	for _, update := range updates {
		encoded, err := plainJSON(update.Value)
		if err != nil {
			continue
		}
		values[update.Key] = encoded
	}
	if len(values) == 0 {
		return nil, false
	}
	return agent.EventExtension[GraphState]{
		Name:  StateName,
		Value: GraphState{Step: step, Values: values},
	}, true
}

// plainJSON renders one state value in the domain a host reads: what the graph's
// condition layers and the script bridge already project.
func plainJSON(value Value) (json.RawMessage, error) {
	switch value.Kind() {
	case KindText:
		text, err := value.Text()
		if err != nil {
			return nil, err
		}
		return json.Marshal(text)
	case KindBool:
		flag, err := value.Bool()
		if err != nil {
			return nil, err
		}
		return json.Marshal(flag)
	case KindJSON:
		raw, err := value.JSON()
		if err != nil {
			return nil, err
		}
		if !json.Valid(raw) {
			return nil, ErrInvalidStateJSON
		}
		return raw, nil
	case KindMessages:
		history, err := value.Messages()
		if err != nil {
			return nil, err
		}
		items := history.Slice()
		rows := make([]map[string]string, 0, len(items))
		for _, item := range items {
			rows = append(rows, map[string]string{
				"role": string(item.Role),
				"text": item.Content.Text(),
			})
		}
		return json.Marshal(rows)
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnreportableState, value.Kind())
	}
}
