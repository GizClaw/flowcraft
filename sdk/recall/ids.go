package recall

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/GizClaw/flowcraft/sdk/llm"
)

// ulidEntropy is a single monotonic entropy source shared across the package
// so concurrent ID generators stay lexicographically monotonic within the
// same millisecond.
var (
	ulidMu      sync.Mutex
	ulidEntropy = ulid.Monotonic(rand.Reader, 0)
)

// NewULID returns a 26-char Crockford-Base32 ULID. Concurrent callers see
// strictly increasing IDs within the same millisecond.
func NewULID() string {
	ulidMu.Lock()
	defer ulidMu.Unlock()
	return ulid.MustNew(ulid.Timestamp(time.Now()), ulidEntropy).String()
}

// NewJobID returns a JobID-typed ULID for SaveAsync (§7.1).
func NewJobID() JobID {
	return JobID(NewULID())
}

// deterministicEntryID derives a stable doc ID for a fact — the cornerstone of
// the at-least-once Save guarantee.
//
// id = "ltm_<sha256(scope|messages|index|content)[:16]>"
//
// Unlike ULIDs, this ID must be content-addressed so a job replayed twice
// upserts the same row.
func deterministicEntryID(scope MemoryScope, msgs []llm.Message, index int, content string) string {
	h := sha256.New()
	fmt.Fprintf(h, "rt=%s\n", scope.RuntimeID)
	fmt.Fprintf(h, "agent=%s\n", scope.AgentID)
	fmt.Fprintf(h, "user=%s\n", scope.UserID)
	fmt.Fprintf(h, "session=%s\n", scope.SessionID)
	for _, m := range msgs {
		fmt.Fprintf(h, "%s|%s\n", string(m.Role), m.Content())
	}
	fmt.Fprintf(h, "i=%d|c=%s", index, content)
	sum := h.Sum(nil)
	return "ltm_" + hex.EncodeToString(sum[:16])
}
