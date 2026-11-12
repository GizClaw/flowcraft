package scenario_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent-graph"
	"github.com/GizClaw/flowcraft/core/agent-graph/script"
	"github.com/GizClaw/flowcraft/core/agent-graph/script/starlark"
	"github.com/GizClaw/flowcraft/core/message"
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

// gameFrom re-encodes the dealt game so a test can pose a position without going
// through the phases that would produce it. It starts from the rule's own JSON, so
// every field the rules read stays present.
func gameFrom(t *testing.T, raw []byte, extra map[string]any) graph.Value {
	t.Helper()
	var game map[string]any
	if err := json.Unmarshal(raw, &game); err != nil {
		t.Fatalf("decode game: %v", err)
	}
	for key, value := range extra {
		game[key] = value
	}
	return jsonValue(t, game)
}

// wolvesAsVillagers rewrites a table so no wolf is left.
func wolvesAsVillagers(t *testing.T, raw []byte) graph.Value {
	t.Helper()
	var game map[string]any
	if err := json.Unmarshal(raw, &game); err != nil {
		t.Fatalf("decode game: %v", err)
	}
	seats, _ := game["seats"].([]any)
	for _, item := range seats {
		seat, ok := item.(map[string]any)
		if ok && seat["role"] == "werewolf" {
			seat["role"] = "villager"
		}
	}
	return jsonValue(t, game)
}

func jsonValue(t *testing.T, value any) graph.Value {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return graph.JSON(raw)
}

// speechValue builds a channel holding the human's last line.
func speechValue(text string) graph.Value {
	return graph.MessagesValue(graph.MessagesFromSlice([]message.Message{
		message.NewTextMessage(message.RoleUser, text),
	}))
}

const nightConfig = `{"backend":"starlark","path":"rules/night.star","reads":["game","speech"],"writes":["game","prompt","brief"]}`
const dawnConfig = `{"backend":"starlark","path":"rules/dawn.star","reads":["game"],"writes":["game","prompt"]}`

func TestNightAsksTheWolfForATarget(t *testing.T) {
	raw, _ := deal(t, "fixed-seed")
	if decodeTable(t, raw).PlayerRole != "werewolf" {
		t.Fatalf("this seed makes the player a villager; the test needs a wolf")
	}
	values := map[string]graph.Value{
		"game": gameFrom(t, raw, map[string]any{"night": map[string]any{
			"started": true, "wolf_slot": 1, "wolf_proposals": []any{},
			"wolf_discussion": []any{}, "wolf_decided": false, "wolf_target": 0,
			"witch_save": 0, "witch_poison": 0, "witch_decided": false,
			"seer_target": 0, "seer_decided": false,
		}}),
		"speech": graph.MessagesValue(graph.MessagesFromSlice(nil)),
		"prompt": graph.Text(""),
		"brief":  graph.Text(""),
	}
	first := runRule(t, "rules/night.star", nightConfig, values)
	prompt, _ := first["prompt"].Text()
	if !strings.Contains(prompt, "刀5号") {
		t.Fatalf("prompt = %q, want the target question", prompt)
	}
	game := decodeGame(t, first["game"])
	if game["waiting_for"] != "wolf_target" {
		t.Fatalf("waiting_for = %v", game["waiting_for"])
	}

	// The human answers: the proposal is recorded and the slot advances.
	values["game"] = first["game"]
	values["speech"] = speechValue("刀5号")
	game = decodeGame(t, runRule(t, "rules/night.star", nightConfig, values)["game"])
	night, _ := game["night"].(map[string]any)
	proposals, _ := night["wolf_proposals"].([]any)
	if len(proposals) != 1 {
		t.Fatalf("proposals = %v", night["wolf_proposals"])
	}
	// The human was on slot 1; answering advances the round.
	if night["wolf_slot"] != float64(2) {
		t.Fatalf("wolf_slot = %v, want 2", night["wolf_slot"])
	}
}

func TestNightRejectsAnImpossibleTarget(t *testing.T) {
	raw, _ := deal(t, "fixed-seed")
	values := map[string]graph.Value{
		"game": gameFrom(t, raw, map[string]any{"night": map[string]any{
			"started": true, "wolf_slot": 1, "wolf_proposals": []any{},
			"wolf_discussion": []any{}, "wolf_decided": false, "wolf_target": 0,
			"witch_save": 0, "witch_poison": 0, "witch_decided": false,
			"seer_target": 0, "seer_decided": false,
		}}),
		"speech": graph.MessagesValue(graph.MessagesFromSlice(nil)),
		"prompt": graph.Text(""),
		"brief":  graph.Text(""),
	}
	first := runRule(t, "rules/night.star", nightConfig, values)
	values["game"] = first["game"]
	values["speech"] = speechValue("刀9号")
	game := decodeGame(t, runRule(t, "rules/night.star", nightConfig, values)["game"])
	if game["waiting_for"] != "wolf_target" {
		t.Fatalf("waiting_for = %v, an out-of-range seat must not be accepted", game["waiting_for"])
	}
	night, _ := game["night"].(map[string]any)
	proposals, _ := night["wolf_proposals"].([]any)
	if len(proposals) != 0 {
		t.Fatalf("proposals = %v, want none", night["wolf_proposals"])
	}
}

func TestDawnKillsAndAdvances(t *testing.T) {
	raw, _ := deal(t, "fixed-seed")
	game := decodeGame(t, runRule(t, "rules/dawn.star", dawnConfig, map[string]graph.Value{
		"game": gameFrom(t, raw, map[string]any{"night": map[string]any{
			"wolf_target": 5, "witch_save": 0, "witch_poison": 0,
		}}),
		"prompt": graph.Text(""),
	})["game"])
	alive, _ := game["alive"].([]any)
	for _, id := range alive {
		if id == float64(5) {
			t.Fatal("seat 5 was knifed and must not be alive")
		}
	}
	if game["phase"] != "day" {
		t.Fatalf("phase = %v, want day", game["phase"])
	}
}

func TestDawnHonoursTheWitchSave(t *testing.T) {
	raw, _ := deal(t, "fixed-seed")
	game := decodeGame(t, runRule(t, "rules/dawn.star", dawnConfig, map[string]graph.Value{
		"game": gameFrom(t, raw, map[string]any{"night": map[string]any{
			"wolf_target": 5, "witch_save": 5, "witch_poison": 0,
		}}),
		"prompt": graph.Text(""),
	})["game"])
	alive, _ := game["alive"].([]any)
	if len(alive) != 8 {
		t.Fatalf("alive = %v, a saved victim must not die", alive)
	}
}

func TestDawnEndsTheGameWhenTheWolvesAreGone(t *testing.T) {
	raw, _ := deal(t, "fixed-seed")
	game := decodeGame(t, runRule(t, "rules/dawn.star", dawnConfig, map[string]graph.Value{
		"game":   gameFrom(t, raw, nil),
		"prompt": graph.Text(""),
	})["game"])
	if game["winner"] != "" {
		t.Fatalf("winner = %v, a fresh deal has wolves", game["winner"])
	}

	game = decodeGame(t, runRule(t, "rules/dawn.star", dawnConfig, map[string]graph.Value{
		"game":   wolvesAsVillagers(t, raw),
		"prompt": graph.Text(""),
	})["game"])
	if game["winner"] != "good" || game["phase"] != "ended" {
		t.Fatalf("winner = %v phase = %v, want good and ended", game["winner"], game["phase"])
	}
}

func decodeGame(t *testing.T, value graph.Value) map[string]any {
	t.Helper()
	raw, err := value.JSON()
	if err != nil {
		t.Fatalf("game: %v", err)
	}
	var game map[string]any
	if err := json.Unmarshal(raw, &game); err != nil {
		t.Fatalf("decode game: %v", err)
	}
	return game
}

const exileConfig = `{"backend":"starlark","path":"rules/exile.star","reads":["game"],"writes":["game","prompt"]}`

// voteConfig and dayConfig declare all eight seat channels: a rule writes a brief
// into exactly one of them, and a script node declares its writes statically.
const voteConfig = `{"backend":"starlark","path":"rules/vote.star","reads":["game","speech"],"writes":["game","prompt","brief"]}`

const dayConfig = `{"backend":"starlark","path":"rules/day.star","reads":["game","speech"],"writes":["game","prompt","brief"]}`

// seatChannels is the empty channel set a rule invocation expects.
func seatChannels(overrides map[string]graph.Value) map[string]graph.Value {
	values := map[string]graph.Value{"prompt": graph.Text(""), "brief": graph.Text("")}
	for id := 1; id <= 8; id++ {
		key := "seat_" + strconv.Itoa(id)
		values[key] = graph.MessagesValue(graph.MessagesFromSlice(nil))
	}
	for key, value := range overrides {
		values[key] = value
	}
	return values
}

func TestDayMovesToTheVoteWhenEveryoneSpoke(t *testing.T) {
	raw, _ := deal(t, "fixed-seed")
	values := seatChannels(map[string]graph.Value{
		"game": gameFrom(t, raw, map[string]any{
			"phase": "day", "speaker_order": []any{1, 2}, "speech_index": 2, "day": 1,
		}),
		"speech": graph.MessagesValue(graph.MessagesFromSlice(nil)),
	})
	game := decodeGame(t, runRule(t, "rules/day.star", dayConfig, values)["game"])
	if game["phase"] != "vote" {
		t.Fatalf("phase = %v, want vote", game["phase"])
	}
	pending, _ := game["vote_pending"].([]any)
	if len(pending) != len(game["alive"].([]any)) {
		t.Fatalf("vote_pending = %v, want every living seat", pending)
	}
}

func TestDayAsksTheHumanInTurn(t *testing.T) {
	raw, _ := deal(t, "fixed-seed")
	values := seatChannels(map[string]graph.Value{
		"game": gameFrom(t, raw, map[string]any{
			"phase": "day", "speaker_order": []any{2, 3}, "speech_index": 1, "day": 1,
		}),
		"speech": graph.MessagesValue(graph.MessagesFromSlice(nil)),
	})
	writes := runRule(t, "rules/day.star", dayConfig, values)
	prompt, _ := writes["prompt"].Text()
	if !strings.Contains(prompt, "轮到你") {
		t.Fatalf("prompt = %q, want the human's turn", prompt)
	}
	if game := decodeGame(t, writes["game"]); game["waiting_for"] != "day_speech" {
		t.Fatalf("waiting_for = %v", game["waiting_for"])
	}
}

func TestVoteTallyExilesTheMajority(t *testing.T) {
	raw, _ := deal(t, "fixed-seed")
	values := seatChannels(map[string]graph.Value{
		"game": gameFrom(t, raw, map[string]any{
			"phase": "tally", "vote_index": 3, "vote_pending": []any{1, 2, 4},
			"current_votes": []any{
				map[string]any{"voter": 1, "target": 5},
				map[string]any{"voter": 2, "target": 5},
				map[string]any{"voter": 4, "target": 7},
			},
			"pk": map[string]any{"mode": "", "candidates": []any{}, "round": 0},
		}),
		"speech": graph.MessagesValue(graph.MessagesFromSlice(nil)),
	})
	game := decodeGame(t, runRule(t, "rules/vote.star", voteConfig, values)["game"])
	if game["exile_target"] != float64(5) || game["phase"] != "exile" {
		t.Fatalf("exile_target = %v phase = %v, want 5 and exile", game["exile_target"], game["phase"])
	}
}

func TestVoteTallyGoesToPKOnATie(t *testing.T) {
	raw, _ := deal(t, "fixed-seed")
	values := seatChannels(map[string]graph.Value{
		"game": gameFrom(t, raw, map[string]any{
			"phase": "tally", "vote_index": 2, "vote_pending": []any{1, 2},
			"current_votes": []any{
				map[string]any{"voter": 1, "target": 5},
				map[string]any{"voter": 2, "target": 7},
			},
			"pk": map[string]any{"mode": "", "candidates": []any{}, "round": 0},
		}),
		"speech": graph.MessagesValue(graph.MessagesFromSlice(nil)),
	})
	game := decodeGame(t, runRule(t, "rules/vote.star", voteConfig, values)["game"])
	if game["phase"] != "pk" {
		t.Fatalf("phase = %v, want pk", game["phase"])
	}
	pk, _ := game["pk"].(map[string]any)
	candidates, _ := pk["candidates"].([]any)
	if len(candidates) != 2 {
		t.Fatalf("candidates = %v", candidates)
	}
}

func TestExileKillsAndStartsTheNextNight(t *testing.T) {
	raw, _ := deal(t, "fixed-seed")
	game := decodeGame(t, runRule(t, "rules/exile.star", exileConfig, map[string]graph.Value{
		"game":   gameFrom(t, raw, map[string]any{"day": 1, "exile_target": 5}),
		"prompt": graph.Text(""),
	})["game"])
	alive, _ := game["alive"].([]any)
	for _, id := range alive {
		if id == float64(5) {
			t.Fatal("the exiled seat must not be alive")
		}
	}
	if game["phase"] != "night_wolf" || game["day"] != float64(2) {
		t.Fatalf("phase = %v day = %v, want the next night", game["phase"], game["day"])
	}
}

func TestExileEndsTheGameWhenTheLastWolfLeaves(t *testing.T) {
	raw, _ := deal(t, "fixed-seed")
	// Keep exactly one wolf (seat 1) and exile it.
	var game map[string]any
	if err := json.Unmarshal(raw, &game); err != nil {
		t.Fatalf("decode: %v", err)
	}
	seats, _ := game["seats"].([]any)
	wolf := 0
	for _, item := range seats {
		seat, _ := item.(map[string]any)
		if seat["role"] == "werewolf" {
			if wolf == 0 {
				wolf = int(seat["id"].(float64))
				continue
			}
			seat["role"] = "villager"
		}
	}
	game["day"] = 1
	game["exile_target"] = wolf
	state := decodeGame(t, runRule(t, "rules/exile.star", exileConfig, map[string]graph.Value{
		"game":   jsonValue(t, game),
		"prompt": graph.Text(""),
	})["game"])
	if state["winner"] != "good" || state["phase"] != "ended" {
		t.Fatalf("winner = %v phase = %v, want good and ended", state["winner"], state["phase"])
	}
}

// votePosition poses a table at the moment the vote is resolved. The phases that
// would produce it are covered elsewhere; a test that needs a tie should not have
// to play seven seats to get one.
func votePosition(t *testing.T, raw []byte, extra map[string]any) graph.Value {
	t.Helper()
	base := map[string]any{
		"phase":         "vote",
		"day":           1,
		"waiting_for":   "",
		"vote_index":    0,
		"vote_pending":  []any{},
		"current_votes": []any{},
		"pk":            map[string]any{"candidates": []any{}, "mode": "", "round": 0},
		"speech_index":  0,
	}
	for key, value := range extra {
		base[key] = value
	}
	return gameFrom(t, raw, base)
}

// TestTiedVoteOpensThePK: a tie is not an exile. The table gets the candidates'
// second speeches and votes again, between them only.
func TestTiedVoteOpensThePK(t *testing.T) {
	raw, _ := deal(t, "fixed-seed")
	alive := decodeTable(t, raw).Alive
	first, second := alive[0], alive[1]
	writes := runRule(t, "rules/vote.star", voteConfig, map[string]graph.Value{
		"game": votePosition(t, raw, map[string]any{
			"phase":        "tally",
			"vote_pending": alive,
			"current_votes": []any{
				map[string]any{"voter": alive[2], "target": first},
				map[string]any{"voter": alive[3], "target": second},
			},
		}),
		"speech": graph.MessagesValue(graph.MessagesFromSlice(nil)),
		"prompt": graph.Text(""),
		"brief":  graph.Text(""),
	})
	game := decodeGame(t, writes["game"])
	if game["phase"] != "pk" {
		t.Fatalf("phase = %v, want pk", game["phase"])
	}
	pk, _ := game["pk"].(map[string]any)
	if pk["mode"] != "speech" {
		t.Fatalf("pk = %v", pk)
	}
	candidates, _ := pk["candidates"].([]any)
	if len(candidates) != 2 || candidates[0] != float64(first) || candidates[1] != float64(second) {
		t.Fatalf("candidates = %v, want %v", candidates, []int{first, second})
	}
}

// TestPKSpeechesReopenTheVote: both candidates speak again, then the table votes
// once more, and that round may only name a candidate.
func TestPKSpeechesReopenTheVote(t *testing.T) {
	raw, _ := deal(t, "fixed-seed")
	alive := decodeTable(t, raw).Alive
	first, second := alive[0], alive[1]
	// Two candidates after the tie, both non-human, so neither branch asks for input.
	pk := map[string]any{"candidates": []any{first, second}, "mode": "speech", "round": 1}
	base := map[string]any{
		"phase":         "pk",
		"speaker_order": []any{first, second},
		"speech_index":  0,
		"pk":            pk,
	}
	values := map[string]graph.Value{
		"game":   votePosition(t, raw, base),
		"speech": graph.MessagesValue(graph.MessagesFromSlice(nil)),
		"prompt": graph.Text(""),
		"brief":  graph.Text(""),
	}
	firstTurn := runRule(t, "rules/vote.star", voteConfig, values)
	game := decodeGame(t, firstTurn["game"])
	if game["brief_seat"] != float64(first) || game["brief_kind"] != "speech" {
		t.Fatalf("brief_seat = %v kind = %v", game["brief_seat"], game["brief_kind"])
	}
	brief, _ := firstTurn["brief"].Text()
	if !strings.Contains(brief, "PK") {
		t.Fatalf("brief = %q, want the PK speech brief", brief)
	}

	// The second candidate's speech, then the table votes again.
	values["game"] = gameFrom(t, raw, map[string]any{
		"phase":         "pk",
		"speaker_order": []any{first, second},
		"speech_index":  2,
		"pk":            pk,
	})
	voteRound := runRule(t, "rules/vote.star", voteConfig, values)
	game = decodeGame(t, voteRound["game"])
	pending, _ := game["vote_pending"].([]any)
	if game["phase"] != "pk" {
		t.Fatalf("phase = %v", game["phase"])
	}
	for _, id := range pending {
		if id == float64(first) || id == float64(second) {
			t.Fatalf("vote_pending = %v, a candidate must not vote in the PK round", pending)
		}
	}
}

// TestPKTieExilesBySeatOrder: a second tie cannot reopen the PK forever, so the
// table settles it deterministically.
func TestPKTieExilesBySeatOrder(t *testing.T) {
	raw, _ := deal(t, "fixed-seed")
	alive := decodeTable(t, raw).Alive
	first, second := alive[0], alive[1]
	game := decodeGame(t, runRule(t, "rules/vote.star", voteConfig, map[string]graph.Value{
		"game": votePosition(t, raw, map[string]any{
			"phase": "tally",
			"pk":    map[string]any{"candidates": []any{first, second}, "mode": "vote", "round": 1},
			"current_votes": []any{
				map[string]any{"voter": alive[2], "target": first},
				map[string]any{"voter": alive[3], "target": second},
			},
		}),
		"speech": graph.MessagesValue(graph.MessagesFromSlice(nil)),
		"prompt": graph.Text(""),
		"brief":  graph.Text(""),
	})["game"])
	if game["phase"] != "exile" {
		t.Fatalf("phase = %v, want exile", game["phase"])
	}
	if game["exile_target"] != float64(first) {
		t.Fatalf("exile_target = %v, want the earlier seat in order (%d)", game["exile_target"], first)
	}
}

// TestHumanVoteRejectsAnUnparseableLine: the human's turn only advances on a line
// the rules can read.
func TestHumanVoteRejectsAnUnparseableLine(t *testing.T) {
	raw, _ := deal(t, "fixed-seed")
	player := decodeTable(t, raw).PlayerSeat
	writes := runRule(t, "rules/vote.star", voteConfig, map[string]graph.Value{
		"game": votePosition(t, raw, map[string]any{
			"waiting_for":  "vote",
			"vote_pending": []any{player, 4},
		}),
		"speech": speechValue("我再想想，先不急着定"),
		"prompt": graph.Text(""),
		"brief":  graph.Text(""),
	})
	game := decodeGame(t, writes["game"])
	if game["waiting_for"] != "vote" {
		t.Fatalf("waiting_for = %v, want the vote to stay open", game["waiting_for"])
	}
	prompt, _ := writes["prompt"].Text()
	if !strings.Contains(prompt, "请明确投票座号") {
		t.Fatalf("prompt = %q", prompt)
	}
	votes, _ := game["current_votes"].([]any)
	if len(votes) != 0 {
		t.Fatalf("current_votes = %v, want none", votes)
	}
}

// TestHumanVoteReadsHowPeopleWrite: "我投六号" and "我这票投2号" are the same
// instruction, and a parser that only reads one of them loses the vote.
func TestHumanVoteReadsHowPeopleWrite(t *testing.T) {
	raw, _ := deal(t, "fixed-seed")
	player := decodeTable(t, raw).PlayerSeat
	for _, line := range []string{"我投六号。", "我这票投2号，先听解释。"} {
		game := decodeGame(t, runRule(t, "rules/vote.star", voteConfig, map[string]graph.Value{
			"game": votePosition(t, raw, map[string]any{
				"waiting_for":  "vote",
				"vote_pending": []any{player, 4},
			}),
			"speech": speechValue(line),
			"prompt": graph.Text(""),
			"brief":  graph.Text(""),
		})["game"])
		votes, _ := game["current_votes"].([]any)
		if len(votes) != 1 {
			t.Fatalf("%q recorded %d votes (waiting=%v)", line, len(votes), game["waiting_for"])
		}
		vote, _ := votes[0].(map[string]any)
		want := float64(6)
		if strings.Contains(line, "2号") {
			want = 2
		}
		if vote["target"] != want {
			t.Fatalf("%q = target %v, want %v", line, vote["target"], want)
		}
	}
}
