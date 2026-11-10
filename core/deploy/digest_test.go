package deploy

import "testing"

func TestDeploymentDigestCoversTheDeployment(t *testing.T) {
	base := deploymentDigest("sha256:rt", "a2a", "https://a", "")
	if base == "" {
		t.Fatal("empty digest")
	}
	same := deploymentDigest("sha256:rt", "a2a", "https://a", "")
	if base != same {
		t.Error("the same deployment produced two digests")
	}
	cases := map[string]string{
		"runtime":  string(deploymentDigest("sha256:other", "a2a", "https://a", "")),
		"peer":     string(deploymentDigest("sha256:rt", "a2a", "https://b", "")),
		"engine":   string(deploymentDigest("sha256:rt", "react", "", "")),
		"protocol": string(deploymentDigest("sha256:rt", "a2a", "https://a", "a2a")),
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
	first := deploymentDigest("sha256:rt", "a2a", "https://a", "a2a")
	second := deploymentDigest("sha256:rt", "a2a", "https://a", "a2a")
	if first != second {
		t.Error("a card edit invalidated the deployment identity")
	}
}
