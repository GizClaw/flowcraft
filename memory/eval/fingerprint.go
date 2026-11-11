package eval

import (
	"crypto/sha256"
	"encoding/hex"
	"runtime/debug"
	"sort"
	"strconv"
)

// Fingerprint names the exact configuration a run was measured under: the code
// revision, the derivation policy, the dataset, and the flags that shape the
// graded set. A report carries it so results from different configurations can
// never be mixed silently -- which is what resuming into a stale report would
// otherwise do.
type Fingerprint struct {
	Revision     string            `json:"revision,omitempty"`
	Dataset      string            `json:"dataset,omitempty"`
	Questions    int               `json:"questions,omitempty"`
	PolicyDigest string            `json:"policy_digest,omitempty"`
	Values       map[string]string `json:"values,omitempty"`
}

// BuildRevision reports the VCS revision the binary was built from, with a
// "+dirty" suffix when the working tree had uncommitted changes. It is empty
// when the binary carries no VCS stamp.
func BuildRevision() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	revision, dirty := "", false
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			dirty = setting.Value == "true"
		}
	}
	if revision == "" {
		return ""
	}
	if dirty {
		return revision + "+dirty"
	}
	return revision
}

// DatasetDigest summarizes the dataset bytes, so a rerun over a changed
// benchmark file is detectable even when the question count matches.
func DatasetDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:12]
}

// Equal reports whether two fingerprints describe the same run configuration.
func (fingerprint Fingerprint) Equal(other Fingerprint) bool {
	if fingerprint.Revision != other.Revision ||
		fingerprint.Dataset != other.Dataset ||
		fingerprint.Questions != other.Questions ||
		fingerprint.PolicyDigest != other.PolicyDigest ||
		len(fingerprint.Values) != len(other.Values) {
		return false
	}
	for key, value := range fingerprint.Values {
		if other.Values[key] != value {
			return false
		}
	}
	return true
}

// Empty reports whether the fingerprint carries no information at all, which
// is the case for reports written before fingerprints were recorded.
func (fingerprint Fingerprint) Empty() bool {
	return fingerprint.Revision == "" && fingerprint.Dataset == "" &&
		fingerprint.Questions == 0 && fingerprint.PolicyDigest == "" &&
		len(fingerprint.Values) == 0
}

// Clone returns a deep copy, so callers can adjust one fingerprint's Values
// without touching another's.
func (fingerprint Fingerprint) Clone() Fingerprint {
	clone := fingerprint
	if fingerprint.Values != nil {
		clone.Values = make(map[string]string, len(fingerprint.Values))
		for key, value := range fingerprint.Values {
			clone.Values[key] = value
		}
	}
	return clone
}

// String renders the fingerprint as a stable, sorted key=value summary for
// logs and error messages.
func (fingerprint Fingerprint) String() string {
	keys := make([]string, 0, len(fingerprint.Values))
	for key := range fingerprint.Values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	summary := "revision=" + fingerprint.Revision +
		" dataset=" + fingerprint.Dataset +
		" questions=" + strconv.Itoa(fingerprint.Questions) +
		" policy_digest=" + short(fingerprint.PolicyDigest)
	for _, key := range keys {
		summary += " " + key + "=" + fingerprint.Values[key]
	}
	return summary
}

func short(value string) string {
	if len(value) <= 12 {
		return value
	}
	return value[:12]
}
