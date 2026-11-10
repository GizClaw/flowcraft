package deploy

import "testing"

func TestDeploymentFingerprintCoversTheDeployment(t *testing.T) {
	base := deploymentFingerprint("sha256:rt", "a2a", "https://a", "")
	if base == "" {
		t.Fatal("empty fingerprint")
	}
	same := deploymentFingerprint("sha256:rt", "a2a", "https://a", "")
	if base != same {
		t.Error("the same deployment produced two fingerprints")
	}
	cases := map[string]string{
		"runtime":  string(deploymentFingerprint("sha256:other", "a2a", "https://a", "")),
		"peer":     string(deploymentFingerprint("sha256:rt", "a2a", "https://b", "")),
		"engine":   string(deploymentFingerprint("sha256:rt", "react", "", "")),
		"protocol": string(deploymentFingerprint("sha256:rt", "a2a", "https://a", "a2a")),
	}
	for name, fingerprint := range cases {
		if fingerprint == string(base) {
			t.Errorf("changing %s did not change the fingerprint", name)
		}
	}
}

// An advertisement is not part of the deployment's identity: the card is not an
// input here, which is the point.
func TestDeploymentFingerprintIgnoresTheCard(t *testing.T) {
	first := deploymentFingerprint("sha256:rt", "a2a", "https://a", "a2a")
	second := deploymentFingerprint("sha256:rt", "a2a", "https://a", "a2a")
	if first != second {
		t.Error("a card edit invalidated the deployment identity")
	}
}
