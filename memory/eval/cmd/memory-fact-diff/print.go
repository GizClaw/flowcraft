package main

import (
	"fmt"
	"strings"
)

// print renders the comparison for a reader: what each side wrote, how much of
// the baseline the candidate still says, and then the batches that differ, the
// biggest ones first. A batch's differences are the claims to judge -- a removed
// fact whose closest counterpart is far away went away, one that sits just under
// the threshold only changed wording.
func (reading *reading) print(snippet, cap, limit int) {
	fmt.Printf("scope %s\n", reading.Scope.RuntimeID)
	fmt.Printf("baseline  %d facts over %d batches (p50 %d, p90 %d, p99 %d, max %d)\n",
		reading.Baseline.Facts, reading.Baseline.Batches,
		reading.Baseline.Rank["p50"], reading.Baseline.Rank["p90"],
		reading.Baseline.Rank["p99"], reading.Baseline.Rank["max"])
	fmt.Printf("candidate %d facts over %d batches (p50 %d, p90 %d, p99 %d, max %d)\n",
		reading.Candidate.Facts, reading.Candidate.Batches,
		reading.Candidate.Rank["p50"], reading.Candidate.Rank["p90"],
		reading.Candidate.Rank["p99"], reading.Candidate.Rank["max"])
	paired := reading.Baseline.Facts - reading.Removed
	share := 0.0
	if reading.Baseline.Facts > 0 {
		share = 100 * float64(paired) / float64(reading.Baseline.Facts)
	}
	fmt.Printf("paired %d of %d baseline facts (%.1f%%), removed %d, added %d, threshold %.2f\n",
		paired, reading.Baseline.Facts, share, reading.Removed, reading.Added, reading.Threshold)
	fmt.Printf("%s\n", reading.similarityLine())
	if cap > 0 {
		batches, facts := reading.Baseline.capped(cap)
		share := 0.0
		if reading.Baseline.Facts > 0 {
			share = 100 * float64(facts) / float64(reading.Baseline.Facts)
		}
		fmt.Printf("cap %d on the baseline's batches: %d batches over it, %d facts left out (%.1f%% of the baseline)\n",
			cap, batches, facts, share)
	}
	fmt.Printf("batches that differ %d, batches only in baseline %d, only in candidate %d\n",
		len(reading.Batches), reading.OnlyFirst, reading.OnlySecond)

	printed := 0
	for _, measured := range reading.Batches {
		if limit > 0 && printed >= limit {
			fmt.Printf("\n... %d more batches not printed\n", len(reading.Batches)-printed)
			break
		}
		printed++
		marker := ""
		if cap > 0 && measured.Baseline > cap {
			marker = " capped"
		}
		fmt.Printf("\n[%d -> %d]%s matched %d, removed %d, added %d\n",
			measured.Baseline, measured.Candidate, marker, measured.Matched,
			len(measured.Removed), len(measured.Added))
		fmt.Printf("  %s\n", measured.Key)
		for _, difference := range measured.Removed {
			printDifference("-", difference, snippet)
		}
		for _, difference := range measured.Added {
			printDifference("+", difference, snippet)
		}
	}
}

// similarityLine is the reading's noise floor in one line: how far the removed
// facts' closest counterparts are. Facts past the first bucket went away; the
// ones just under the threshold are the pairs a reader should check by hand.
func (reading *reading) similarityLine() string {
	buckets := make([]int, 5)
	for _, fact := range reading.Batches {
		for _, difference := range fact.Removed {
			switch {
			case difference.Best >= reading.Threshold:
				buckets[0]++
			case difference.Best >= 0.5:
				buckets[1]++
			case difference.Best >= 0.3:
				buckets[2]++
			case difference.Best > 0:
				buckets[3]++
			default:
				buckets[4]++
			}
		}
	}
	return fmt.Sprintf("removed facts by closest counterpart: above threshold %d, 0.50-%.2f %d, 0.30-0.50 %d, 0-0.30 %d, nothing %d",
		buckets[0], reading.Threshold, buckets[1], buckets[2], buckets[3], buckets[4])
}

func printDifference(sign string, difference difference, snippet int) {
	fmt.Printf("  %s (best %.2f) %s\n", sign, difference.Best, quote(shorten(difference.Text, snippet)))
	if strings.TrimSpace(difference.Closest) != "" && difference.Best > 0 {
		fmt.Printf("      closest: %s\n", quote(shorten(difference.Closest, snippet)))
	}
}

func quote(text string) string { return "\"" + text + "\"" }
