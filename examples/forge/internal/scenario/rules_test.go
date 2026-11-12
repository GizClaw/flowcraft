package scenario_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent-graph"
	"github.com/GizClaw/flowcraft/core/agent-graph/script"
	"github.com/GizClaw/flowcraft/core/agent-graph/script/starlark"
)

// The rules are scripts under the scenario, so these tests are what pins them: a
// rule is a pure function of state, and a fixed seed makes a whole hand
// reproducible.
const werewolf = "../../scenarios/raids/werewolf"

// ruleKind builds the script node over the scenario's rules directory.
func ruleKind(t *testing.T) graph.NodeKind {
	t.Helper()
	kind, err := script.New(
		script.WithBackends(starlark.New()),
		script.WithSource(&script.FileSource{BaseDir: werewolf}),
	)
	if err != nil {
		t.Fatalf("script.New: %v", err)
	}
	return kind
}

// runRule invokes one rule file and returns the writes it produced.
func runRule(t *testing.T, path, config string, values map[string]graph.Value) map[string]graph.Value {
	t.Helper()
	kind := ruleKind(t)
	in := graph.Invoke{
		Node:     path,
		Config:   json.RawMessage(config),
		State:    func(key string) graph.Value { return values[key] },
		Snapshot: func() graph.Snapshot { return graph.NewSnapshot(values) },
	}
	stream, err := kind.Invoke(context.Background(), in)
	if err != nil {
		t.Fatalf("invoke %s: %v", path, err)
	}
	for {
		if _, err := stream.Next(context.Background()); err != nil {
			if !errors.Is(err, io.EOF) {
				t.Fatalf("%s: %v", path, err)
			}
			break
		}
	}
	result, err := stream.Result()
	if err != nil {
		t.Fatalf("%s result: %v", path, err)
	}
	writes := make(map[string]graph.Value, len(result.State))
	for _, update := range result.State {
		writes[update.Key] = update.Value
	}
	return writes
}

const seedConfig = `{"backend":"starlark","path":"rules/seed.star","reads":["seed","game"],"writes":["game","prompt"]}`

// deal runs the deal rule for one seed.
func deal(t *testing.T, seed string) (gameJSON []byte, writes map[string]graph.Value) {
	t.Helper()
	writes = runRule(t, "rules/seed.star", seedConfig, map[string]graph.Value{
		"seed":   graph.Text(seed),
		"game":   graph.JSON([]byte(`{}`)),
		"prompt": graph.Text(""),
	})
	game := writes["game"]
	if game.Kind() != graph.KindJSON {
		t.Fatalf("seed wrote a %s game", game.Kind())
	}
	raw, err := game.JSON()
	if err != nil {
		t.Fatalf("game: %v", err)
	}
	return raw, writes
}

type table struct {
	Phase string `json:"phase"`
	Alive []int  `json:"alive"`
	Seats []struct {
		ID   int    `json:"id"`
		Role string `json:"role"`
	} `json:"seats"`
	PlayerSeat int      `json:"player_seat"`
	PlayerRole string   `json:"player_role"`
	PublicLog  []string `json:"public_log"`
}

func decodeTable(t *testing.T, raw []byte) table {
	t.Helper()
	var dealt table
	if err := json.Unmarshal(raw, &dealt); err != nil {
		t.Fatalf("decode game: %v", err)
	}
	return dealt
}

func TestSeedDealsACompleteTable(t *testing.T) {
	raw, writes := deal(t, "fixed-seed")
	table := decodeTable(t, raw)

	if len(table.Seats) != 8 || len(table.Alive) != 8 {
		t.Fatalf("seats = %d alive = %d, want 8 and 8", len(table.Seats), len(table.Alive))
	}
	counts := map[string]int{}
	for _, seat := range table.Seats {
		counts[seat.Role]++
	}
	want := map[string]int{"werewolf": 3, "seer": 1, "witch": 1, "hunter": 1, "villager": 2}
	for role, n := range want {
		if counts[role] != n {
			t.Fatalf("roles = %v, want %v", counts, want)
		}
	}
	if table.Phase != "night_wolf" || table.PlayerSeat != 3 {
		t.Fatalf("phase = %q player = %d", table.Phase, table.PlayerSeat)
	}
	if table.PlayerRole != table.Seats[2].Role {
		t.Fatalf("player_role %q does not match seat 3 (%q)", table.PlayerRole, table.Seats[2].Role)
	}
	if len(table.PublicLog) != 1 || table.PublicLog[0] == "" {
		t.Fatalf("public_log = %v", table.PublicLog)
	}
	// The human's own role is announced to them, and only to them.
	prompt, _ := writes["prompt"].Text()
	if prompt == "" {
		t.Fatal("the deal must tell the player their seat and role")
	}
}

func TestSeedIsReproducible(t *testing.T) {
	first, _ := deal(t, "same-seed")
	second, _ := deal(t, "same-seed")
	if string(first) != string(second) {
		t.Fatal("the same seed dealt two different tables")
	}
	other, _ := deal(t, "another-seed")
	if string(other) == string(first) {
		t.Fatal("two seeds dealt the same table; the shuffle is not using the seed")
	}
}
