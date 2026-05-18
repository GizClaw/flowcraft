package main

import "testing"

func TestPlanNamespacesExplicit(t *testing.T) {
	from, to, err := planNamespaces(config{mode: "explicit", from: "old", to: "new"})
	if err != nil {
		t.Fatal(err)
	}
	if from != "old" || to != "new" {
		t.Fatalf("plan = %q -> %q", from, to)
	}
}

func TestPlanNamespacesRecallUserV1(t *testing.T) {
	from, to, err := planNamespaces(config{
		mode:      "recall-user-v1",
		prefix:    "ltmtest",
		runtimeID: "rt1",
		userID:    "conv-26",
	})
	if err != nil {
		t.Fatal(err)
	}
	if from != "ltmtest_rt1__u_conv_26" {
		t.Fatalf("from = %q", from)
	}
	if to != "ltmtest_rt1__u7_conv_26" {
		t.Fatalf("to = %q", to)
	}
}

func TestPlanNamespacesRecallEntitiesV1(t *testing.T) {
	from, to, err := planNamespaces(config{
		mode:      "recall-entities-v1",
		prefix:    "ltmenttest",
		runtimeID: "rt1",
		userID:    "conv-26",
	})
	if err != nil {
		t.Fatal(err)
	}
	if from != "ltmenttest_rt1__u_conv_26__entities" {
		t.Fatalf("from = %q", from)
	}
	if to != "ltmenttest_rt1__u7_conv_26__entities" {
		t.Fatalf("to = %q", to)
	}
}
