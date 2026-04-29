package retrieval

import "strings"

// Reserved metadata keys that are part of the public retrieval / recall
// protocol. Stages, writers and external integrations MUST use these
// constants rather than hard-coded string literals so a typo is caught
// at compile time and a future rename only touches one place.
//
// The recall package defines additional reserved keys (subject,
// predicate, superseded_by, tombstone, …) that are documented in
// sdk/recall/metadata.go; they live there because they are semantically
// owned by the recall package, not the generic retrieval substrate.
const (
	// MetaSlotKey is the canonical metadata key under which a writer
	// stores the slot supersede tuple ("subject|predicate"). The
	// SlotCollapse retrieval stage reads it; the recall save path
	// writes it. Kept on the retrieval side so both packages can
	// reference one source of truth without an import cycle.
	MetaSlotKey = "slot_key"
)

// SlotKeyOf returns the trimmed slot_key metadata value, or "" when
// absent / non-string. Centralised so every reader (retrieval stages,
// recall package helpers, future tooling) agrees on the lookup
// contract — including the trim-on-read rule that tolerates writers
// that forgot to canonicalise on write.
func SlotKeyOf(d Doc) string {
	if d.Metadata == nil {
		return ""
	}
	v, ok := d.Metadata[MetaSlotKey].(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(v)
}
