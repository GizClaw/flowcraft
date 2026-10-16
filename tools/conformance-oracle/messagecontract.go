package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"

	"github.com/GizClaw/flowcraft/core/message/media"
)

// The message half of the contract, starting with the media DTOs.
//
// Every row carries its own input, the same rule the error contract follows:
// the Rust side replays the call and compares outcomes
// (crates/flowcraft-contract/tests/message_contract.rs), so a row that stops
// being true fails on the replay rather than being a snapshot of what core
// happened to print. The sentences are core's own -- the ones a constructor or
// a validator writes -- because those are what callers match on; the two rows
// whose text comes from encoding/json say so.
//
// What this dump does not cover yet, and why: the stream kind (a live handle
// has no wire form to dump), the `unknown audio encoding` refusal (it needs
// the decode path, which lands with the codec that reads messages), and
// AudioFormat's own JSON decode (core decodes it as a plain struct, so its
// unknown-field behavior belongs to whichever decoder reads the part that
// holds it).
const messageContractNote = "The media DTOs of core/message, dumped for the port. " +
	"Rows carry their input and the outcome core produced; the port replays them."

type messageContract struct {
	Note         string      `json:"note"`
	MediaSources []mediaRow  `json:"media_sources"`
	AudioFormats []formatRow `json:"audio_formats"`
}

// mediaRow is one call into the media constructors or one document handed to
// the typed wire decode, with the outcome core produced.
type mediaRow struct {
	Target    string `json:"target"` // image | audio | video
	Op        string `json:"op"`     // new_inline | new_url | from_wire
	Bytes     string `json:"bytes,omitempty"`     // base64, for new_inline
	URL       string `json:"url,omitempty"`       // for new_url
	MediaType string `json:"media_type,omitempty"` // for the constructors
	Document  string `json:"document,omitempty"`  // for from_wire

	Outcome    string `json:"outcome"` // ok | error
	Error      string `json:"error,omitempty"`
	// TextFrom names the writer of Error when it is not core's own sentence:
	// the strict decoder is encoding/json, and its refusals are spelled the
	// way that package spells them.
	TextFrom   string `json:"text_from,omitempty"`
	MediaTypeOut string `json:"media_type_out,omitempty"`
	BaseMediaType string `json:"base_media_type,omitempty"`
	Wire       string `json:"wire,omitempty"`
}

// formatRow is one AudioFormat, built the way core's callers build it (a
// literal), with its validation outcome and the media type its encoding maps
// to. Absent numbers are absent here too: core reads an unset int as zero, and
// the port reads an absent one the same way.
type formatRow struct {
	Encoding     string `json:"encoding"`
	SampleRateHz *int   `json:"sample_rate_hz,omitempty"`
	Channels     *int   `json:"channels,omitempty"`

	Outcome   string `json:"outcome"` // ok | error
	Error     string `json:"error,omitempty"`
	MediaType string `json:"media_type"`
}

// mediaView is the shape every typed source shares: the validators, the
// normalized and base media types, and the wire codec.
type mediaView interface {
	Validate() error
	MediaType() string
	BaseMediaType() string
}

func writeMessageContract(w io.Writer) error {
	contract := messageContract{
		Note:         messageContractNote,
		MediaSources: make([]mediaRow, 0, len(mediaRows)),
		AudioFormats: make([]formatRow, 0, len(formatRows)),
	}
	for _, row := range mediaRows {
		contract.MediaSources = append(contract.MediaSources, mediaOutcome(row))
	}
	for _, row := range formatRows {
		contract.AudioFormats = append(contract.AudioFormats, formatOutcome(row))
	}
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	return encoder.Encode(contract)
}

func mediaOutcome(row mediaRow) mediaRow {
	view, err := buildMedia(row)
	if err != nil {
		row.Outcome = "error"
		row.Error = err.Error()
		return row
	}
	if err := view.Validate(); err != nil {
		row.Outcome = "error"
		row.Error = err.Error()
		return row
	}
	wire, err := json.Marshal(view)
	if err != nil {
		row.Outcome = "error"
		row.Error = err.Error()
		return row
	}
	row.Outcome = "ok"
	row.MediaTypeOut = view.MediaType()
	row.BaseMediaType = view.BaseMediaType()
	row.Wire = string(wire)
	return row
}

func buildMedia(row mediaRow) (mediaView, error) {
	switch row.Op {
	case "new_inline":
		data, err := base64.StdEncoding.DecodeString(row.Bytes)
		if err != nil {
			return nil, err
		}
		switch row.Target {
		case "image":
			value, err := media.NewImageBytes(data, row.MediaType)
			if err != nil {
				return nil, err
			}
			return value, nil
		case "audio":
			value, err := media.NewAudioBytes(data, row.MediaType)
			if err != nil {
				return nil, err
			}
			return value, nil
		case "video":
			value, err := media.NewVideoBytes(data, row.MediaType)
			if err != nil {
				return nil, err
			}
			return value, nil
		}
	case "new_url":
		switch row.Target {
		case "image":
			value, err := media.NewImageURL(row.URL, row.MediaType)
			if err != nil {
				return nil, err
			}
			return value, nil
		case "audio":
			value, err := media.NewAudioURL(row.URL, row.MediaType)
			if err != nil {
				return nil, err
			}
			return value, nil
		case "video":
			value, err := media.NewVideoURL(row.URL, row.MediaType)
			if err != nil {
				return nil, err
			}
			return value, nil
		}
	case "from_wire":
		document := []byte(row.Document)
		switch row.Target {
		case "image":
			var value media.ImageSource
			if err := json.Unmarshal(document, &value); err != nil {
				return nil, err
			}
			return value, nil
		case "audio":
			var value media.AudioSource
			if err := json.Unmarshal(document, &value); err != nil {
				return nil, err
			}
			return value, nil
		case "video":
			var value media.VideoSource
			if err := json.Unmarshal(document, &value); err != nil {
				return nil, err
			}
			return value, nil
		}
	}
	return nil, fmt.Errorf("unknown media sample target %q op %q", row.Target, row.Op)
}

func formatOutcome(row formatRow) formatRow {
	format := media.AudioFormat{
		Encoding: media.AudioEncoding(row.Encoding),
		SampleRateHz: intOf(row.SampleRateHz),
		Channels:     intOf(row.Channels),
	}
	row.MediaType = format.Encoding.MediaType()
	if err := format.Validate(); err != nil {
		row.Outcome = "error"
		row.Error = err.Error()
		return row
	}
	row.Outcome = "ok"
	return row
}

func intOf(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}
