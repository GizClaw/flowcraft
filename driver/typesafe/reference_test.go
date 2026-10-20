package typesafe

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
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/utils/decode"
)

// referenceDoc is the config skill's resource reference, which carries the
// example deployment this driver documents: users copy its model block, so the
// example has to load exactly the way a deployment does. The file lives outside
// this module, so a checkout that holds the module alone skips the guard
// instead of failing on a missing path; the repository lanes, where the file is
// present, are where the promise is kept.
const referenceDoc = "../../skills/flowcraft-config/references/resources.md"

// referenceHeading opens the section of referenceDoc that documents the System
// One decision surface.
const referenceHeading = "### TypeSafe System One"

// referenceExample returns the yaml fence under the System One heading.
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

// TestReferenceExampleLoads pins the TypeSafe example in the config skill's
// resource reference against the driver's own load path: the document decodes
// as a provider settings document, the declared model passes the family
// contract, and the example declares the one kind this driver serves. A
// reference example that drifts away from the driver is a deployment users
// cannot load.
func TestReferenceExampleLoads(t *testing.T) {
	raw, err := decode.ToJSON([]byte(referenceExample(t)))
	if err != nil {
		t.Fatalf("the reference example is not a loadable document: %v", err)
	}
	var doc struct {
		Provider struct {
			Kind     string          `json:"kind"`
			Impl     string          `json:"impl"`
			Settings json.RawMessage `json:"settings"`
		} `json:"provider"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode the reference example: %v", err)
	}
	if doc.Provider.Kind != ResourceKind || doc.Provider.Impl != providerID {
		t.Fatalf(
			"the reference example is not a typesafe provider (kind %q, impl %q)",
			doc.Provider.Kind, doc.Provider.Impl,
		)
	}
	// The deployment path, not the spec decoder: an example that builds a
	// provider definition is an example a deployment can load.
	value, err := Factory().New(context.Background(), resource.Input{
		Settings: doc.Provider.Settings,
	})
	if err != nil {
		t.Fatalf("the reference example does not load: %v", err)
	}
	provider := value
	if len(provider.Models) == 0 {
		t.Fatal("the reference example declares no models")
	}
	for _, implementation := range provider.Models {
		capabilities := implementation.Descriptor.Capabilities
		if len(capabilities.Outputs) != 0 {
			t.Errorf("model %q declares outputs: a decision model produces no content kind",
				implementation.Descriptor.ID.Name)
		}
		if slices.ContainsFunc(capabilities.Inputs,
			func(kind message.PartKind) bool { return kind != message.PartText }) {
			t.Errorf("model %q declares non-text input: the wire carries one text state value",
				implementation.Descriptor.ID.Name)
		}
		if implementation.Openers.Decision == nil {
			t.Errorf("model %q opens no decision driver", implementation.Descriptor.ID.Name)
		}
	}
	if len(provider.Profiles) == 0 {
		t.Fatal("the reference example declares no profile")
	}
	for _, profile := range provider.Profiles {
		if !slices.Contains(profile.Operations, model.OperationDecision) {
			t.Errorf("profile %q does not declare the decision operation", profile.ID)
		}
	}
}
