package locomo

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/GizClaw/flowcraft/eval/dataset"
	"github.com/GizClaw/flowcraft/eval/locomo/runners/flowcraft"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/recall"
)

// addLocomoIngest wires `eval locomo ingest` — runs Runner.Save in a
// loop, reporting per-conversation latency. Useful for warming a
// persistent Index before a real run or for isolating extractor
// regressions independently of recall + answer cost.
func addLocomoIngest(parent *cobra.Command) {
	var (
		runnerName  string
		datasetFlag string
	)

	cmd := &cobra.Command{
		Use:   "ingest",
		Short: "Pipe a dataset through Runner.Save (no QA) and report latencies",
		RunE: func(c *cobra.Command, _ []string) error {
			if runnerName != "flowcraft" {
				return fmt.Errorf("unknown runner: %s", runnerName)
			}
			var ds *dataset.Dataset
			var err error
			if datasetFlag == "synthetic" {
				ds = dataset.Synthetic()
			} else {
				ds, err = dataset.LoadJSONL(datasetFlag)
				if err != nil {
					return err
				}
			}
			r, err := flowcraft.New(flowcraft.Options{Name: runnerName})
			if err != nil {
				return err
			}
			defer r.Close()

			scope := recall.Scope{RuntimeID: "ingest", UserID: "u-bench", AgentID: "agent-bench"}
			// SaveRaw is an optional interface the runner may expose
			// to skip the extractor and bench the raw-storage path.
			type rawSaver interface {
				SaveRaw(ctx context.Context, scope recall.Scope, msgs []llm.Message) (int, time.Duration, error)
			}
			for _, conv := range ds.Conversations {
				t0 := time.Now()
				msgs := ingestConvo(conv)
				var n int
				if rs, ok := any(r).(rawSaver); ok {
					n, _, _ = rs.SaveRaw(c.Context(), scope, msgs)
				} else {
					n, _, _ = r.Save(c.Context(), scope, msgs)
				}
				fmt.Printf("ingest %s in %s, %d facts\n", conv.ID, time.Since(t0), n)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&runnerName, "runner", "flowcraft", "runner name")
	cmd.Flags().StringVar(&datasetFlag, "dataset", "synthetic", "dataset (synthetic) or .jsonl path")

	parent.AddCommand(cmd)
}

func ingestConvo(c dataset.Conversation) []llm.Message {
	out := make([]llm.Message, 0, len(c.Turns))
	for _, t := range c.Turns {
		out = append(out, llm.Message{Role: model.Role(t.Role), Parts: []model.Part{{Type: model.PartText, Text: t.Content}}})
	}
	return out
}
