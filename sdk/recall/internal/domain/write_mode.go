package domain

// WriteMode controls SaveRequest semantics. Zero value = synchronous
// (preserves current behaviour). WriteModeAsyncSemantic stores raw
// episodes synchronously and enqueues semantic extraction.
type WriteMode int

const (
	WriteModeSync WriteMode = iota
	WriteModeAsyncSemantic
)

func (m WriteMode) String() string {
	switch m {
	case WriteModeSync:
		return "sync"
	case WriteModeAsyncSemantic:
		return "async_semantic"
	}
	return "unknown"
}
