package main

import (
	"encoding/json"
	"io"
	"path/filepath"

	"github.com/GizClaw/flowcraft/core/utils/pathsafe"
)

// The pathsafe contract: containment, the relative form, the reference guard,
// and the lexical clean behind all three.
//
// pathsafe is small and entirely lexical, which is exactly why a port can be
// plausible and wrong: `path/filepath.Rel` walks up and down between two
// cleaned paths, `Clean` folds inner `..` but keeps a relative leading one, and
// `ResolveUnder` appends even an absolute reference before refusing it. Rows
// carry both paths so the port answers the question rather than comparing a
// printed answer, and every string in a row is a path core accepted as an
// argument.
//
// The filesystem half (`RealWithin`, `RealDir`) is not here: it depends on the
// tree the test builds, so it is tested where the tree is (`pathsafe.rs`, with
// the symlink cases core's own test builds).

const pathsafeContractNote = "core/utils/pathsafe: containment, rel, resolve_under, rel_ref " +
	"and clean. Rows carry their inputs; the port replays them."

type pathsafeContract struct {
	Note    string       `json:"note"`
	Within  []withinRow  `json:"within"`
	Rel     []relRow     `json:"rel"`
	Resolve []resolveRow `json:"resolve"`
	RelRef  []relRefRow  `json:"rel_ref"`
	Clean   []cleanRow   `json:"clean"`
}

type withinRow struct {
	Root       string `json:"root"`
	Path       string `json:"path"`
	Within     bool   `json:"within"`
	WithinFold bool   `json:"within_fold"`
}

type relRow struct {
	Root   string `json:"root"`
	Target string `json:"target"`
	Rel    string `json:"rel,omitempty"`
	OK     bool   `json:"ok"`
}

type resolveRow struct {
	Root      string `json:"root"`
	Reference string `json:"reference"`
	Path      string `json:"path,omitempty"`
	Error     string `json:"error,omitempty"`
}

type relRefRow struct {
	Reference string `json:"reference"`
	OK        bool   `json:"ok"`
}

type cleanRow struct {
	Input   string `json:"input"`
	Cleaned string `json:"cleaned"`
}

func writePathsafeContract(w io.Writer) error {
	contract := pathsafeContract{
		Note:   pathsafeContractNote,
		Within: make([]withinRow, 0, len(pathsafeWithinSamples)),
		Rel:    make([]relRow, 0, len(pathsafeRelSamples)),
		Resolve: make([]resolveRow, 0,
			len(pathsafeResolveSamples)),
		RelRef: make([]relRefRow, 0, len(pathsafeRelRefSamples)),
		Clean:  make([]cleanRow, 0, len(pathsafeCleanSamples)),
	}

	for _, sample := range pathsafeWithinSamples {
		contract.Within = append(contract.Within, withinRow{
			Root:       sample.root,
			Path:       sample.path,
			Within:     pathsafe.Within(sample.root, sample.path),
			WithinFold: pathsafe.WithinFold(sample.root, sample.path),
		})
	}
	for _, sample := range pathsafeRelSamples {
		// pathsafe.Rel, not filepath.Rel: the empty arguments are one of the
		// places the two disagree (the wrapper refuses them), and the wrapper
		// is what the port is a port of.
		rel, ok := pathsafe.Rel(sample.root, sample.target)
		contract.Rel = append(contract.Rel, relRow{
			Root:   sample.root,
			Target: sample.target,
			Rel:    rel,
			OK:     ok,
		})
	}
	for _, sample := range pathsafeResolveSamples {
		row := resolveRow{Root: sample.root, Reference: sample.reference}
		resolved, err := pathsafe.ResolveUnder(sample.root, sample.reference)
		if err != nil {
			row.Error = err.Error()
		} else {
			row.Path = resolved
		}
		contract.Resolve = append(contract.Resolve, row)
	}
	for _, reference := range pathsafeRelRefSamples {
		contract.RelRef = append(contract.RelRef, relRefRow{
			Reference: reference,
			OK:        pathsafe.RelRef(reference),
		})
	}
	for _, input := range pathsafeCleanSamples {
		contract.Clean = append(contract.Clean, cleanRow{
			Input:   input,
			Cleaned: filepath.Clean(input),
		})
	}

	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	return encoder.Encode(contract)
}
