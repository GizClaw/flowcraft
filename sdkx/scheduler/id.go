package scheduler

import (
	"crypto/rand"
	"encoding/hex"
)

func newID() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		panic("scheduler: failed to generate id: " + err.Error())
	}
	return hex.EncodeToString(buf[:])
}
