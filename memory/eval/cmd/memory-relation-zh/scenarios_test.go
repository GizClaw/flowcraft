package main

import "testing"

// TestCorpusIsWellFormed pins what the reading assumes about its own corpus: an
// update conversation states two turns and names the relation the later one
// changes, a repeatable conversation states two turns and names none, and no two
// conversations share an id. A corpus that drifts stops being the comparison the
// reading is, and the drift is silent -- a scenario with one turn is simply
// skipped by the scoring loop.
func TestCorpusIsWellFormed(t *testing.T) {
	seen := make(map[string]struct{}, len(zhScenarios))
	for _, scenario := range zhScenarios {
		if scenario.ID == "" {
			t.Error("a scenario has no id")
		}
		if _, duplicate := seen[scenario.ID]; duplicate {
			t.Errorf("scenario %q appears twice", scenario.ID)
		}
		seen[scenario.ID] = struct{}{}
		if len(scenario.Turns) != 2 {
			t.Errorf("scenario %q has %d turns, want 2", scenario.ID, len(scenario.Turns))
			continue
		}
		for index, turn := range scenario.Turns {
			if turn.At == "" || turn.Says == "" {
				t.Errorf("scenario %q turn %d is incomplete", scenario.ID, index)
			}
		}
		first, second := scenario.Turns[0], scenario.Turns[1]
		update := first.Want != ""
		if update != (second.Want != "") {
			t.Errorf("scenario %q labels one turn but not the other", scenario.ID)
		}
		if update && first.Want != second.Want {
			t.Errorf("scenario %q changes %q to %q, which is not the same relation",
				scenario.ID, first.Want, second.Want)
		}
	}

	classes := make(map[string]struct{})
	for _, entry := range zhRelationInventory {
		if entry.Predicate == "" {
			t.Error("an inventory entry has no predicate")
		}
		if entry.Control {
			continue
		}
		if entry.Relation != "" {
			classes[entry.Relation] = struct{}{}
		}
	}
	// The corpus is the reading's fixed half: every class the pass ships has to
	// be confronted with Chinese phrasing, or the miss count flatters the table.
	for _, scenario := range zhScenarios {
		for _, turn := range scenario.Turns {
			if turn.Want == "" {
				continue
			}
			if _, ok := classes[turn.Want]; !ok {
				t.Errorf("scenario %q states %q, which the inventory does not cover",
					scenario.ID, turn.Want)
			}
		}
	}
}
