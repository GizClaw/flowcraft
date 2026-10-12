package skill

import (
	"context"
	"testing"
)

func TestRankAndMention(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "review",
		"name: review\ndescription: review code and docs for quality\n")
	writeSkill(t, root, "plan",
		"name: plan\ndescription: build execution plans\n")
	writeSkill(t, root, "search",
		"name: search\ndescription: search the workspace\n")
	svc := NewService(context.Background(), Options{Roots: []string{root}, TopN: 2})

	ranked := svc.Rank("review the docs", 2, 0)
	if len(ranked) == 0 || ranked[0].Name != "review" {
		t.Fatalf("Rank(review) = %+v, want review first", ranked)
	}
	if len(ranked) > 2 {
		t.Fatalf("Rank topN = %d, want <= 2", len(ranked))
	}
	if got := svc.Rank("zzzzqqqq", 5, 0); len(got) != 0 {
		t.Fatalf("Rank(no match) = %+v, want empty", got)
	}
	if got := svc.Rank("", 5, 0); len(got) != 0 {
		t.Fatalf("Rank(empty) = %+v, want empty", got)
	}
	if got := svc.Rank("plan", 0, 0); len(got) == 0 {
		t.Fatal("Rank(topN 0) must fall back to Options.TopN")
	}

	mentioned := svc.Mentioned("use $plan and $review for this")
	if len(mentioned) != 2 ||
		mentioned[0].Name != "plan" || mentioned[1].Name != "review" {
		t.Fatalf("Mentioned = %+v, want plan and review in mention order", mentioned)
	}
	if got := svc.Mentioned("$plan $plan $nope"); len(got) != 1 {
		t.Fatalf("Mentioned dup = %+v, want one", got)
	}
	// "$50" and a mid-word "$" are not mentions.
	if got := svc.Mentioned("costs $50 and a$plan"); len(got) != 0 {
		t.Fatalf("Mentioned(prices) = %+v, want none", got)
	}
}

func TestRankMinScoreThreshold(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "review", "name: review\ndescription: review code and docs\n")
	svc := NewService(context.Background(), Options{Roots: []string{root}})

	scored := svc.RankScored("review the code", 5, 0)
	if len(scored) == 0 {
		t.Fatal("matching query must rank at least one skill")
	}
	top := scored[0].Score
	if len(svc.Rank("review the code", 5, top)) < 1 {
		t.Fatalf("threshold %v must keep top matches", top)
	}
	if len(svc.Rank("review the code", 5, top+1)) != 0 {
		t.Fatalf("threshold %v must filter everything", top+1)
	}
}

// TestRankCJKQuery pins the kernel's CJK tokenization end to end: a
// Chinese fragment matches a Chinese description without any
// segmentation library.
func TestRankCJKQuery(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, root, "doc-search",
		"name: doc-search\ndescription: 检索本地文档并总结要点\n")
	writeSkill(t, root, "images",
		"name: images\ndescription: generate and edit images\n")
	svc := NewService(context.Background(), Options{Roots: []string{root}})

	hits := svc.Rank("本地文档", 5, 0)
	if len(hits) == 0 || hits[0].Name != "doc-search" {
		t.Fatalf("Rank(本地文档) = %+v, want doc-search", hits)
	}
}

// TestDuplicateNamesBothRankable keeps the duplicate-name contract
// honest: both same-named paths are indexed under distinct IDs (their
// paths), so neither crowds the other out of the index.
func TestDuplicateNamesBothRankable(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	writeSkill(t, rootA, "dup", "name: dup\ndescription: duplicate in root A\n")
	writeSkill(t, rootB, "dup", "name: dup\ndescription: duplicate in root B\n")
	svc := NewService(context.Background(), Options{Roots: []string{rootA, rootB}})

	paths := map[string]bool{}
	for _, sc := range svc.RankScored("duplicate root", 5, 0) {
		if sc.Skill.Name == "dup" {
			paths[sc.Skill.Path] = true
		}
	}
	if len(paths) != 2 {
		t.Fatalf("ranked dup paths = %v, want both copies", paths)
	}
}
