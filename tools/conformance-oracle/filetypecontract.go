package main

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/GizClaw/flowcraft/core/utils/filetype"
)

// The filetype contract has two halves, and they are both functions of core
// alone:
//
//   - `sniffs` is `net/http.DetectContentType`, the WHATWG algorithm core's
//     classifier relies on for its content half. Every signature, every mask,
//     the tag-terminating rule, the mp4 box walk and the 512-byte cutoff are in
//     the corpus, because an off-by-one in any of them is a silent
//     misclassification rather than an error.
//   - `classify` is `filetype.OfData` with a name that has no extension, so the
//     ordered rule is pinned without involving the host's MIME database: a text
//     payload is text whatever the sniffer says, a binary payload takes the
//     sniffer's answer, and the empty name contributes nothing.
//
// The name half (`NameType`) is deliberately absent: it resolves through the
// host's MIME database, so its answers differ between machines while both
// sides read the same files (see the port's filetype module docs). The port
// tests its parsers against fixture text instead.

const filetypeContractNote = "core/utils/filetype: the WHATWG sniffer, and the ordered rule " +
	"with a nameless payload. Name-derived answers are host-dependent and stay out."

// sniffRow carries the bytes, not a description of them: the port rebuilds the
// sample and runs its own sniffer over it.
type sniffRow struct {
	Data []byte `json:"data"`
	Type string `json:"type"`
}

type classifyRow struct {
	Data      []byte `json:"data"`
	MediaType string `json:"media_type"`
	Text      bool   `json:"text"`
}

type filetypeContract struct {
	Note     string        `json:"note"`
	Sniffs   []sniffRow    `json:"sniffs"`
	Classify []classifyRow `json:"classify"`
}

func writeFiletypeContract(w io.Writer) error {
	corpus := filetypeCorpus()
	contract := filetypeContract{
		Note:     filetypeContractNote,
		Sniffs:   make([]sniffRow, 0, len(corpus)),
		Classify: make([]classifyRow, 0, len(corpus)),
	}
	for _, sample := range corpus {
		// A nil slice encodes as JSON null; the empty sample is a sample too,
		// and the corpus has one (an empty file reads as text).
		if sample == nil {
			sample = []byte{}
		}
		contract.Sniffs = append(contract.Sniffs, sniffRow{
			Data: sample,
			Type: http.DetectContentType(sample),
		})
		// A name with no extension: the table contributes "" on every host.
		kind := filetype.OfData("blob", sample)
		contract.Classify = append(contract.Classify, classifyRow{
			Data:      sample,
			MediaType: kind.MediaType,
			Text:      kind.Text,
		})
	}

	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	return encoder.Encode(contract)
}
