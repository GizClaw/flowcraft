package memorytest

import (
	"strconv"
	"time"
)

// defaultTestTimeout is the per-test context timeout. Generous
// enough for any reasonable in-process or file-backed impl.
const defaultTestTimeout = 5 * time.Second

// itoa is a tiny strconv.Itoa shim. Tests use it to keep
// record IDs deterministic.
func itoa(i int) string { return strconv.Itoa(i) }
