//go:build linux || windows

package journal

import "sort"

// containsAll checks the ops a path was reported with as a set: the
// order the kernel chose is not what these tests are about.
func containsAll(haystack []string, needles ...string) bool {
	sorted := append([]string(nil), haystack...)
	sort.Strings(sorted)
	for _, needle := range needles {
		idx := sort.SearchStrings(sorted, needle)
		if idx >= len(sorted) || sorted[idx] != needle {
			return false
		}
	}
	return true
}
