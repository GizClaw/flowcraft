package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCreateAutoWorkspacesCreatesAndKeepsBothConfigsUnderRunDir(t *testing.T) {
	runDir := filepath.Join(t.TempDir(), ".out", "run")
	raid, persona, err := createAutoWorkspaces(runDir, "chat", "girl_7_Momo")
	if err != nil {
		t.Fatalf("createAutoWorkspaces: %v", err)
	}
	if raid != filepath.Join(runDir, "raid") {
		t.Fatalf("raid workspace = %s, want under run dir", raid)
	}
	if persona != filepath.Join(runDir, "persona") {
		t.Fatalf("persona workspace = %s, want under run dir", persona)
	}
	for _, dir := range []string{raid, persona} {
		path := filepath.Join(dir, configFileName)
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("missing %s: %v", path, err)
		}
	}
	if _, err := os.Stat(runDir); err != nil {
		t.Fatalf("auto workspace root should be retained: %v", err)
	}
}

func TestAppendAutoTurnDoesNotWrapAgentText(t *testing.T) {
	var transcript strings.Builder
	appendAutoTurn(&transcript, 1, autoSpeech{Actor: "persona", Text: "你好呀"}, autoSpeech{Actor: "raid", Text: "你好，Momo"})
	got := transcript.String()
	if strings.Contains(got, "用户刚才说") || strings.Contains(got, "请自然回复") || strings.Contains(got, "对方刚才回复") {
		t.Fatalf("transcript contains prompt wrapper: %s", got)
	}
	if strings.Contains(got, "input:") || strings.Contains(got, "output:") {
		t.Fatalf("transcript should not expose internal input/output: %s", got)
	}
	if !strings.Contains(got, "=== Turn 1 ===") {
		t.Fatalf("transcript does not include turn heading: %s", got)
	}
	if !strings.Contains(got, "--- persona ---\n你好呀") || !strings.Contains(got, "--- raid ---\n你好，Momo") {
		t.Fatalf("transcript does not preserve visible dialogue: %s", got)
	}
}

func TestAppendAutoTurnPreservesSpeechOrder(t *testing.T) {
	var transcript strings.Builder
	appendAutoTurn(&transcript, 1, autoSpeech{Actor: "raid", Text: "先讲故事"}, autoSpeech{Actor: "persona", Text: "我听着呢"})
	got := transcript.String()
	raidIdx := strings.Index(got, "--- raid ---")
	personaIdx := strings.Index(got, "--- persona ---")
	if raidIdx < 0 || personaIdx < 0 {
		t.Fatalf("missing actor sections: %s", got)
	}
	if raidIdx > personaIdx {
		t.Fatalf("speech order = %s, want raid before persona", got)
	}
}

func TestAppendAutoMessageUsesNodeLogWhenPresent(t *testing.T) {
	var transcript strings.Builder
	appendAutoMessage(&transcript, "persona", "那后来呢？", "--- answer ---\n那后来呢？")
	got := transcript.String()
	if !strings.Contains(got, "--- answer ---\n那后来呢？") {
		t.Fatalf("transcript missing node log: %s", got)
	}
	if strings.Contains(got, "--- persona ---") {
		t.Fatalf("transcript should not include actor label when node log exists: %s", got)
	}
}

func TestSelfStartingRaidOpeningKeepsTurnsPersonaThenRaid(t *testing.T) {
	var transcript strings.Builder
	appendAutoMessage(&transcript, "raid", "先讲一段")
	appendAutoTurn(&transcript, 1, autoSpeech{Actor: "persona", Text: "我听到了"}, autoSpeech{Actor: "raid", Text: "继续讲"})
	got := transcript.String()
	openingIdx := strings.Index(got, "--- raid ---")
	turnIdx := strings.Index(got, "=== Turn 1 ===")
	personaIdx := strings.LastIndex(got, "--- persona ---")
	raidIdx := strings.LastIndex(got, "--- raid ---")
	if openingIdx < 0 || turnIdx < 0 || personaIdx < 0 || raidIdx < 0 {
		t.Fatalf("missing transcript sections: %s", got)
	}
	if !(openingIdx < turnIdx && turnIdx < personaIdx && personaIdx < raidIdx) {
		t.Fatalf("unexpected self-starting transcript order: %s", got)
	}
}

func TestSanitizeAutoTextRemovesEmojiLikeRunes(t *testing.T) {
	got := sanitizeAutoText("小兔子🐇 很开心😉\ufe0f")
	if got != "小兔子 很开心" {
		t.Fatalf("sanitizeAutoText = %q", got)
	}
}

func TestTestAutoCmdRejectsNonPositiveTimeout(t *testing.T) {
	err := testAutoCmd([]string{
		"--raid", "chat",
		"--persona", "girl_7_Momo",
		"--timeout", "0s",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "--timeout > 0") {
		t.Fatalf("error = %v, want timeout validation", err)
	}
}

func TestPrepareAutoRunOutputUsesDefaultOutDirectory(t *testing.T) {
	cwd := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(cwd); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	defer func() { _ = os.Chdir(old) }()

	output, err := prepareAutoRunOutput("journey", "boy_14_Tom", "")
	if err != nil {
		t.Fatalf("prepareAutoRunOutput: %v", err)
	}
	wantPrefix := filepath.Join(".out", "journey_boy_14_Tom_")
	if !strings.HasPrefix(output.Dir, wantPrefix) {
		t.Fatalf("Dir = %q, want prefix %q", output.Dir, wantPrefix)
	}
	for _, path := range []string{
		filepath.Join(output.Dir, "chat_log.txt"),
		filepath.Join(output.Dir, "stats.txt"),
		filepath.Join(output.Dir, "stdout.txt"),
		filepath.Join(output.Dir, "stderr.txt"),
	} {
		if path != output.ChatLogPath && path != output.StatsPath && path != output.StdoutPath && path != output.StderrPath {
			t.Fatalf("missing output path %s in %+v", path, output)
		}
	}
	if _, err := os.Stat(output.Dir); err != nil {
		t.Fatalf("output dir not created: %v", err)
	}
}

func TestPrepareAutoRunOutputDefaultNamesRunDirectory(t *testing.T) {
	cwd := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(cwd); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	defer func() { _ = os.Chdir(old) }()

	output, err := prepareAutoRunOutput("examples/raids/journey.yaml", "personas/boy_14_Tom.yaml", "")
	if err != nil {
		t.Fatalf("prepareAutoRunOutput: %v", err)
	}
	if !strings.HasPrefix(output.Dir, filepath.Join(".out", "journey_boy_14_Tom_")) {
		t.Fatalf("Dir = %q, want .out/journey_boy_14_Tom_<time>", output.Dir)
	}
	if output.ChatLogPath != filepath.Join(output.Dir, "chat_log.txt") {
		t.Fatalf("ChatLogPath = %q", output.ChatLogPath)
	}
	if output.StatsPath != filepath.Join(output.Dir, "stats.txt") {
		t.Fatalf("StatsPath = %q", output.StatsPath)
	}
}

func TestWriteAutoStatsWritesInspectionSections(t *testing.T) {
	runDir := filepath.Join(t.TempDir(), ".out", "run")
	raid, persona, err := createAutoWorkspaces(runDir, "chat", "girl_7_Momo")
	if err != nil {
		t.Fatalf("createAutoWorkspaces: %v", err)
	}
	setExampleEnv(t)

	out := filepath.Join(t.TempDir(), "stats.txt")
	metrics := autoMetrics{
		StartedAt:        time.Unix(1, 0),
		FinishedAt:       time.Unix(2, 0),
		Elapsed:          time.Second,
		TurnsCompleted:   0,
		Timeout:          time.Nanosecond,
		StoppedReason:    "timeout before turn 1",
		RaidWorkspace:    raid,
		PersonaWorkspace: persona,
		ContextID:        "ctx",
		Starts:           "persona",
		InitialPrompt:    "hello",
	}
	if err := writeAutoStats(out, metrics, raid, persona); err != nil {
		t.Fatalf("writeAutoStats: %v", err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	got := string(raw)
	if !strings.Contains(got, "--- runtime inspect ---") || !strings.Contains(got, "--- raid inspect ---") || !strings.Contains(got, "--- persona inspect ---") {
		t.Fatalf("missing inspect sections: %s", got)
	}
	if strings.Contains(got, "turns_requested:") {
		t.Fatalf("runtime metrics should not include turns_requested: %s", got)
	}
	if strings.Contains(got, "--- persona ---\nhello") {
		t.Fatalf("stats should not contain chat transcript: %s", got)
	}
	if !strings.Contains(got, "timeout: 1ns") || !strings.Contains(got, "stopped_reason: timeout before turn 1") {
		t.Fatalf("missing runtime metrics: %s", got)
	}
	if !strings.Contains(got, "starts: persona") || !strings.Contains(got, "initial_prompt: hello") {
		t.Fatalf("missing run metadata: %s", got)
	}
	if !strings.Contains(got, "badger:") || !strings.Contains(got, "bleve:") || !strings.Contains(got, "hnsw:") {
		t.Fatalf("missing storage metrics: %s", got)
	}
}
