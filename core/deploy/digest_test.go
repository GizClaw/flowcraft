package deploy

import "testing"

func TestDeploymentDigestCoversTheDeployment(t *testing.T) {
	base := deploymentDigest("sha256:rt", EngineSpec{Kind: "a2a", Peer: "https://a"}, nil)
	if base == "" {
		t.Fatal("empty digest")
	}
	same := deploymentDigest("sha256:rt", EngineSpec{Kind: "a2a", Peer: "https://a"}, nil)
	if base != same {
		t.Error("the same deployment produced two digests")
	}
	cases := map[string]string{
		"runtime":  string(deploymentDigest("sha256:other", EngineSpec{Kind: "a2a", Peer: "https://a"}, nil)),
		"peer":     string(deploymentDigest("sha256:rt", EngineSpec{Kind: "a2a", Peer: "https://b"}, nil)),
		"engine":   string(deploymentDigest("sha256:rt", EngineSpec{Kind: "react"}, nil)),
		"protocol": string(deploymentDigest("sha256:rt", EngineSpec{Kind: "a2a", Peer: "https://a"}, &ProtocolSpec{Kind: "a2a"})),
	}
	for name, digest := range cases {
		if digest == string(base) {
			t.Errorf("changing %s did not change the digest", name)
		}
	}
}

// An advertisement is not part of the deployment's identity: the card is not an
// input here, which is the point.
func TestDeploymentDigestIgnoresTheCard(t *testing.T) {
	first := deploymentDigest("sha256:rt", EngineSpec{Kind: "a2a", Peer: "https://a"}, &ProtocolSpec{
		Kind: "a2a", Settings: rawJSON(map[string]any{"card": map[string]any{"description": "one"}}),
	})
	second := deploymentDigest("sha256:rt", EngineSpec{Kind: "a2a", Peer: "https://a"}, &ProtocolSpec{
		Kind: "a2a", Settings: rawJSON(map[string]any{"card": map[string]any{"description": "two"}}),
	})
	if first != second {
		t.Error("a card edit invalidated the deployment identity")
	}
}
