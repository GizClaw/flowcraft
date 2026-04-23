package recall

// Partition names a logical bucket considered during a List/Search call.
// Partitions are stored on [Scope]; an entry matches a scope if it
// satisfies any listed partition.
type Partition string

const (
	// PartitionUser matches rows whose Scope.UserID equals the query
	// scope's UserID.
	PartitionUser Partition = "user"
	// PartitionGlobal matches runtime-global rows (empty UserID on the entry).
	PartitionGlobal Partition = "global"
)

// NormalizePartitions returns a deduplicated, ordered slice. An empty
// input returns a nil slice so callers can distinguish "auto" from
// "explicitly empty".
func NormalizePartitions(parts []Partition) []Partition {
	if len(parts) == 0 {
		return nil
	}
	seen := make(map[Partition]struct{}, len(parts))
	out := make([]Partition, 0, len(parts))
	for _, p := range parts {
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
