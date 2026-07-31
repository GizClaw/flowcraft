package scheduler

import (
	"crypto/rand"
	"encoding/hex"
)

func newID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		panic("scheduler: failed to generate id: " + err.Error())
	}
	return hex.EncodeToString(buf)
}
