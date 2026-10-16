// Command oracle dumps the Go-side decode behavior for configuration
// documents so the Rust port can be diffed against it.
//
// It lives outside the repository's module graph on purpose: it builds with
// GOWORK=off against a local replace of core, so the gate compiles against
// the same code the release does, and it is part of no release itself.
// It reports, per file:
//
//	format        — the Kubernetes sniffing rule (core/utils.FormatOf)
//	error/json    — the strict YAML/JSON -> JSON conversion result
//	deploy_*      — decoding the JSON form into the typed deploy.Document
//	key_collisions — JSON keys that two distinct YAML keys in the same mapping
//	                 collapse onto (see jsonKey); deterministic evidence of a
//	                 document whose conversion result cannot be defined
//
// With -error-contract it prints no per-file records and instead dumps the
// errdefs error contract -- the classification tables and the classifier
// samples the port is held to (see errorcontract.go). With -message-contract
// it dumps the other half of the contract the port is held to: the media DTOs
// of core/message, with an outcome per sample (see messagecontract.go).
//
// Usage: oracle [-error-contract | -message-contract] <dir> [dir...]
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/utils"
	yamlv2 "gopkg.in/yaml.v2"
)

type record struct {
	File          string   `json:"file"`
	Format        string   `json:"format"`
	Error         string   `json:"error,omitempty"`
	JSON          string   `json:"json,omitempty"`
	DeployOK      bool     `json:"deploy_ok"`
	DeployError   string   `json:"deploy_error,omitempty"`
	KeyCollisions []string `json:"key_collisions,omitempty"`
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "-error-contract" {
		if err := writeErrorContract(os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "error contract:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "-message-contract" {
		if err := writeMessageContract(os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "message contract:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: oracle [-error-contract | -message-contract] <dir> [dir...]")
		os.Exit(2)
	}
	var files []string
	for _, root := range os.Args[1:] {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			switch strings.ToLower(filepath.Ext(path)) {
			case ".yaml", ".yml", ".json":
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, "walk:", err)
			os.Exit(1)
		}
	}
	sort.Strings(files)

	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	enc := json.NewEncoder(out)
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "read:", err)
			os.Exit(1)
		}
		rec := record{File: path, Format: utils.FormatOf(data).String()}
		if jsonData, err := utils.ToJSON(data); err != nil {
			rec.Error = err.Error()
		} else {
			rec.JSON = string(jsonData)
		}
		rec.KeyCollisions = keyCollisions(data)
		if _, err := utils.Decode[deploy.Document](data); err != nil {
			rec.DeployError = err.Error()
		} else {
			rec.DeployOK = true
		}
		if err := enc.Encode(rec); err != nil {
			fmt.Fprintln(os.Stderr, "encode:", err)
			os.Exit(1)
		}
	}
}

// keyCollisions lists the JSON keys that two distinct YAML keys of the same
// mapping collapse onto. Such a document has no defined conversion result: the
// collapse happens while iterating a Go map, so which value survives is decided
// by map iteration order, i.e. per process. Measured over 400 runs of the
// integer/string case: {"1":"a"} 47 times, {"1":"b"} 353 times.
//
// This is reported as a fact about the file, not as a difference between the
// implementations, so it is visible from a single run -- a differential gate
// that has to catch a coin flip by sampling is only as good as its luck.
func keyCollisions(data []byte) []string {
	if utils.FormatOf(data) != utils.FormatYAML {
		return nil // the JSON path is returned verbatim; nothing is collapsed
	}
	var value any
	if err := yamlv2.Unmarshal(data, &value); err != nil {
		return nil // no decoded value, so no collapse: the error path is stable
	}
	found := map[string]struct{}{}
	collectCollisions(value, found)
	if len(found) == 0 {
		return nil
	}
	out := make([]string, 0, len(found))
	for key := range found {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func collectCollisions(value any, found map[string]struct{}) {
	switch typed := value.(type) {
	case map[interface{}]interface{}:
		seen := make(map[string]bool, len(typed))
		for key, entry := range typed {
			if jsonKey, ok := jsonKeyOf(key); ok {
				if seen[jsonKey] {
					found[jsonKey] = struct{}{}
				}
				seen[jsonKey] = true
			}
			collectCollisions(entry, found)
		}
	case []interface{}:
		for _, entry := range typed {
			collectCollisions(entry, found)
		}
	}
}

// jsonKeyOf reproduces sigs.k8s.io/yaml v1.4.0's convertToJSONableObject key
// spelling: keys may be string, int, int64, float64 or bool, and anything else
// is refused there. The float case is worth reading twice -- it formats at
// float32 precision, so `1.0000000001` and `1` both become "1".
//
// main_test.go pins this copy against the library, one key form at a time, so
// the two cannot drift apart unnoticed.
func jsonKeyOf(key any) (string, bool) {
	switch typed := key.(type) {
	case string:
		return typed, true
	case int:
		return strconv.Itoa(typed), true
	case int64:
		return strconv.FormatInt(typed, 10), true
	case float64:
		s := strconv.FormatFloat(typed, 'g', -1, 32)
		switch s {
		case "+Inf":
			s = ".inf"
		case "-Inf":
			s = "-.inf"
		case "NaN":
			s = ".nan"
		}
		return s, true
	case bool:
		if typed {
			return "true", true
		}
		return "false", true
	default:
		return "", false
	}
}
