package dashscope

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/utils"
)

// referenceDoc is the config skill's resource reference, which carries the
// example deployment this driver documents: users copy its model blocks, so
// the example has to load exactly the way a deployment does. The file lives
// outside this module, so a checkout that holds the module alone skips the
// guard instead of failing on a missing path; the repository lanes, where the
// file is present, are where the promise is kept.
const referenceDoc = "../../skills/flowcraft-config/references/resources.md"

// referenceHeading opens the section of referenceDoc that documents the
// DashScope native surfaces.
const referenceHeading = "### DashScope native surfaces"

// referenceExample returns the yaml fence under the DashScope heading.
func referenceExample(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(referenceDoc)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			t.Skipf("the config skill's reference is not in this checkout (%s)", referenceDoc)
		}
		t.Fatalf("read %s: %v", referenceDoc, err)
	}
	lines := strings.Split(string(raw), "\n")
	start := slices.Index(lines, referenceHeading)
	if start < 0 {
		t.Fatalf("no %q heading in %s", referenceHeading, referenceDoc)
	}
	open := slices.Index(lines[start:], "```yaml")
	if open < 0 {
		t.Fatalf("no yaml block under %q", referenceHeading)
	}
	open += start
	close := slices.Index(lines[open+1:], "```")
	if close < 0 {
		t.Fatalf("unterminated yaml block under %q", referenceHeading)
	}
	return strings.Join(lines[open+1:open+1+close], "\n")
}

// TestReferenceExampleLoads pins the DashScope example in the config skill's
// resource reference against the driver's own load path: the document decodes
// as a provider spec, every model block passes the family contract, and the
// example still covers every kind this driver serves. A reference example that
// drifts away from the driver is a deployment users cannot load.
func TestReferenceExampleLoads(t *testing.T) {
	raw, err := utils.ToJSON([]byte(referenceExample(t)))
	if err != nil {
		t.Fatalf("the reference example is not a loadable document: %v", err)
	}
	var doc struct {
		Provider struct {
			Kind     string `json:"kind"`
			Impl     string `json:"impl"`
			Settings struct {
				ID   string          `json:"id"`
				Spec json.RawMessage `json:"spec"`
			} `json:"settings"`
		} `json:"provider"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode the reference example: %v", err)
	}
	if doc.Provider.Kind != "inference.Provider" || doc.Provider.Impl != "dashscope" {
		t.Fatalf(
			"the reference example is not a dashscope provider (kind %q, impl %q)",
			doc.Provider.Kind, doc.Provider.Impl,
		)
	}
	spec, err := decodeSpec(context.Background(), doc.Provider.Settings.Spec)
	if err != nil {
		t.Fatalf("the reference example does not load: %v", err)
	}
	if len(spec.Models) == 0 {
		t.Fatal("the reference example declares no models")
	}
	kinds := make(map[string]struct{}, len(spec.Models))
	for _, declared := range spec.Models {
		if err := validateModel(declared); err != nil {
			t.Errorf("model %q: %v", declared.Name, err)
		}
		id := model.ModelID{Provider: doc.Provider.Settings.ID, Name: declared.Name}
		if err := declared.Lifecycle.ValidateFor(id); err != nil {
			t.Errorf("model %q: %v", declared.Name, err)
		}
		kinds[declared.Kind] = struct{}{}
	}
	// The example is the one place a user sees the whole line-up, so it has
	// to keep showing a model of every kind the driver serves.
	for _, kind := range []string{"image", "video", "generate", "embed", "tts", "transcribe"} {
		if _, ok := kinds[kind]; !ok {
			t.Errorf("the reference example declares no %q model", kind)
		}
	}
}
