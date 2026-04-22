// Command ingest pipes a dataset through a Runner.Save loop and reports per-
// conversation latencies. Useful for warming up a persistent Index before
// running eval, or for observing extractor regression independently.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"time"

	"github.com/GizClaw/flowcraft/bench/locomo/dataset"
	"github.com/GizClaw/flowcraft/bench/locomo/runners/flowcraft"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/memory/ltm"
	"github.com/GizClaw/flowcraft/sdk/model"
)

func main() {
	runner := flag.String("runner", "flowcraft", "runner name")
	datasetFlag := flag.String("dataset", "synthetic", "dataset (synthetic) or .jsonl path")
	flag.Parse()
	if *runner != "flowcraft" {
		log.Fatalf("unknown runner: %s", *runner)
	}
	var ds *dataset.Dataset
	var err error
	if *datasetFlag == "synthetic" {
		ds = dataset.Synthetic()
	} else {
		ds, err = dataset.LoadJSONL(*datasetFlag)
		if err != nil {
			log.Fatal(err)
		}
	}
	r, err := flowcraft.New(flowcraft.Options{Name: *runner})
	if err != nil {
		log.Fatal(err)
	}
	defer r.Close()
	scope := ltm.MemoryScope{RuntimeID: "ingest", UserID: "u-bench", AgentID: "agent-bench"}
	type rawSaver interface {
		SaveRaw(ctx context.Context, scope ltm.MemoryScope, msgs []llm.Message) (int, time.Duration, error)
	}
	for _, c := range ds.Conversations {
		t0 := time.Now()
		msgs := convo(c)
		var n int
		if rs, ok := any(r).(rawSaver); ok {
			n, _, _ = rs.SaveRaw(context.Background(), scope, msgs)
		} else {
			n, _, _ = r.Save(context.Background(), scope, msgs)
		}
		fmt.Printf("ingest %s in %s, %d facts\n", c.ID, time.Since(t0), n)
	}
}

func convo(c dataset.Conversation) []llm.Message {
	out := make([]llm.Message, 0, len(c.Turns))
	for _, t := range c.Turns {
		out = append(out, llm.Message{Role: model.Role(t.Role), Parts: []model.Part{{Type: model.PartText, Text: t.Content}}})
	}
	return out
}
