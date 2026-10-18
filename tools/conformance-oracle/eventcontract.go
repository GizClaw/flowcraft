package main

import (
	"encoding/json"
	"io"
	"time"

	"github.com/GizClaw/flowcraft/core/event"
)

// The event contract: the routing keys, the matcher, the producer convention
// and the envelope wire. Same rule as the other halves -- every row is a call
// into core with the outcome computed.
//
// Not in here: the bus. A subscription's delivery, its backpressure policy and
// its close sequence are a runtime, and the port's concurrency model is a
// decision of its own; the contract it has to keep (which envelope reaches
// which subscription, and with what timing guarantee) is what the bus slice
// will pin.
//
// Envelope rows carry the instant as seconds and nanoseconds rather than as a
// string, so the port builds the same instant without needing a parser, and the
// wire column compares the spelling `encoding/json` gives it.
const eventContractNote = "core/event's contract: subjects, patterns, the matcher, " +
	"producer kinds and the envelope wire. Rows carry their input; the port replays them."

type eventContract struct {
	Note      string        `json:"note"`
	Subjects  []subjectRow  `json:"subjects"`
	Patterns  []patternRow  `json:"patterns"`
	Matches   []matchRow    `json:"matches"`
	Producers []producerRow `json:"producers"`
	Envelopes []envelopeRow `json:"envelopes"`
}

type subjectRow struct {
	Subject string `json:"subject"`
	Valid   bool   `json:"valid"`
	Error   string `json:"error,omitempty"`
}

type patternRow struct {
	Pattern string `json:"pattern"`
	Valid   bool   `json:"valid"`
	Error   string `json:"error,omitempty"`
}

type matchRow struct {
	Pattern string `json:"pattern"`
	Subject string `json:"subject"`
	Matches bool   `json:"matches"`
}

type producerRow struct {
	Subject string `json:"subject"`
	Kind    string `json:"kind"`
}

// envelopeRow is one envelope built the way a producer builds it: an id, an
// instant, optional provenance, headers and a payload already in JSON.
type envelopeRow struct {
	Subject   string            `json:"subject"`
	ID        string            `json:"id"`
	TimeSecs  int64             `json:"time_secs"`
	TimeNanos int32             `json:"time_nanos"`
	Source    string            `json:"source,omitempty"`
	TraceID   string            `json:"trace_id,omitempty"`
	SpanID    string            `json:"span_id,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	Payload   string            `json:"payload,omitempty"`

	Wire         string `json:"wire,omitempty"`
	WireError    string `json:"wire_error,omitempty"`
	WireTextFrom string `json:"wire_text_from,omitempty"`
}

func writeEventContract(w io.Writer) error {
	contract := eventContract{
		Note:      eventContractNote,
		Subjects:  make([]subjectRow, 0, len(subjectSamples)),
		Patterns:  make([]patternRow, 0, len(patternSamples)),
		Matches:   make([]matchRow, 0, len(matchSamples)),
		Producers: make([]producerRow, 0, len(producerSamples)),
		Envelopes: make([]envelopeRow, 0, len(envelopeSamples)),
	}
	for _, sample := range subjectSamples {
		row := subjectRow{Subject: sample, Valid: true}
		if err := event.Subject(sample).Validate(); err != nil {
			row.Valid = false
			row.Error = err.Error()
		}
		contract.Subjects = append(contract.Subjects, row)
	}
	for _, sample := range patternSamples {
		row := patternRow{Pattern: sample, Valid: true}
		if err := event.Pattern(sample).Validate(); err != nil {
			row.Valid = false
			row.Error = err.Error()
		}
		contract.Patterns = append(contract.Patterns, row)
	}
	for _, sample := range matchSamples {
		contract.Matches = append(contract.Matches, matchRow{
			Pattern: sample[0],
			Subject: sample[1],
			Matches: event.Pattern(sample[0]).Matches(event.Subject(sample[1])),
		})
	}
	for _, sample := range producerSamples {
		contract.Producers = append(contract.Producers, producerRow{
			Subject: sample,
			Kind:    event.ProducerKind(event.Subject(sample)),
		})
	}
	for _, row := range envelopeSamples {
		contract.Envelopes = append(contract.Envelopes, envelopeOutcome(row))
	}
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	return encoder.Encode(contract)
}

func envelopeOutcome(row envelopeRow) envelopeRow {
	envelope := event.Envelope{
		ID:      row.ID,
		Subject: event.Subject(row.Subject),
		Time:    time.Unix(row.TimeSecs, int64(row.TimeNanos)).UTC(),
		Source:  row.Source,
		TraceID: row.TraceID,
		SpanID:  row.SpanID,
		Headers: row.Headers,
	}
	// A payload is `json.RawMessage` in core -- already encoded -- and an empty
	// column is the nil one.
	if row.Payload != "" {
		envelope.Payload = json.RawMessage(row.Payload)
	}
	wire, err := json.Marshal(envelope)
	if err != nil {
		row.WireError = err.Error()
		row.WireTextFrom = textFrom(err.Error())
		return row
	}
	row.Wire = string(wire)
	return row
}

var subjectSamples = []string{
	"a",
	"graph.run.r1.start",
	"graph.run.r1.node.n1.complete",
	"kanban.board.b1.update",
	"engine.step.actor.agent1.node.n1",
	"a.b.c.d.e.f.g.h",

	// Refusals: the seven ways core refuses a routing key.
	"",
	".a",
	"a.",
	"a..b",
	"a.*",
	"a.>",
	"kanban.*.b1",
}

var patternSamples = []string{
	"a",
	"graph.run.r1.>",
	"graph.run.*.node.*.start",
	"kanban.>",
	"*>",
	"*.*",
	"a.*.b",

	// Refusals: the seven ways core refuses a pattern, plus the two that only
	// patterns can get wrong.
	"",
	".a",
	"a.",
	"a..b",
	"a.>.b",
	"a.b*",
	"*a",
	"a.>>",
}

// matchSamples is pattern, subject -- both well formed, since a bus validates
// before it matches.
var matchSamples = [][2]string{
	{"graph.run.r1.>", "graph.run.r1"},
	{"graph.run.r1.>", "graph.run.r1.node.n1.start"},
	{"graph.run.>", "graph.run.r1"},
	{"graph.run.>", "graph.run"},
	{"graph.run.*.node.*.start", "graph.run.r1.node.n1.start"},
	{"graph.run.*", "graph.run.r1.start"},
	{"kanban.>", "kanban.board.b1.update"},
	{"a.b", "a.b"},
	{"a.b", "a.b.c"},
	{"*", "x"},
	{"*", "x.y"},
	{"a.*.c", "a.b.c"},
	{"a.*.c", "a.b.d"},
}

var producerSamples = []string{
	"engine.run.start",
	"kanban.board.b1.update",
	"graph.run.r1.start",
	"single",
	"a.b",
}

var envelopeSamples = []envelopeRow{
	{
		Subject: "graph.run.r1.start", ID: "d5cf0k0g1h2j3k4m5n6o",
		TimeSecs: 1_760_572_800, TimeNanos: 0,
	},
	{
		Subject: "graph.run.r1.start", ID: "d5cf0k0g1h2j3k4m5n6o",
		TimeSecs: 1_760_572_800, TimeNanos: 123_000_000,
		Source: "engine", TraceID: "4bf92f3577b34da6a3ce929d0e0e4736",
		SpanID: "00f067aa0ba902b7",
		Headers: map[string]string{
			"run_id": "r1", "node_id": "n1", "tenant": "acme",
		},
		Payload: `{"b":1,"a":[true,null]}`,
	},
	{
		Subject: "kanban.board.b1.update", ID: "d5cf0k0g1h2j3k4m5n6p",
		TimeSecs: 1_760_572_800, TimeNanos: 500_000_000,
		Headers: map[string]string{"agent_id": "a1", "parent_run_id": "r0"},
		Payload: `{ "b" : "<1>" }`,
	},
	{
		// The zero instant, which is what an envelope carries before a producer
		// fills it in: core still writes a time.
		Subject: "a.b", ID: "00000000000000000000", TimeSecs: -62_135_596_800,
	},
	{
		// A payload with HTML characters and a U+2028 in it: `json.RawMessage`
		// is compacted and escaped on the way out, not re-encoded.
		Subject: "a.b", ID: "d5cf0k0g1h2j3k4m5n6q", TimeSecs: 1_700_000_000,
		Payload: `{"doc":"a<b>&\u2028c","n":1e100}`,
	},
	{
		// A payload that is not JSON: core's marshal refuses it.
		Subject: "a.b", ID: "d5cf0k0g1h2j3k4m5n6r", TimeSecs: 1_700_000_000,
		Payload: `not json`,
	},
}
