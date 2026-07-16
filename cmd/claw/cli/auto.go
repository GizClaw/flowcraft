package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/memory/retrieval/bbh"
	sdkworkspace "github.com/GizClaw/flowcraft/sdk/workspace"
	"github.com/GizClaw/flowcraft/sdkx/claw"
)

func testAutoCmd(args []string) error {
	flags := flag.NewFlagSet("test-auto", flag.ExitOnError)
	raidConfig := flags.String("raid", "", "raid config template path or embedded example name")
	personaConfig := flags.String("persona", "", "persona config template path or embedded example name")
	timeout := flags.Duration("timeout", 5*time.Minute, "maximum auto dialogue duration")
	closeTimeout := flags.Duration("close-timeout", 15*time.Second, "maximum time to wait for each workspace close")
	flags.Parse(args)

	if *raidConfig == "" || *personaConfig == "" {
		return fmt.Errorf("test-auto requires --raid and --persona\n\n%s", usage())
	}
	if *timeout <= 0 {
		return fmt.Errorf("test-auto requires --timeout > 0")
	}
	output, err := prepareAutoRunOutput(*raidConfig, *personaConfig, "")
	if err != nil {
		return err
	}
	restoreOutput, err := captureAutoRunOutput(output)
	if err != nil {
		return err
	}
	defer restoreOutput()

	raidWorkspace, personaWorkspace, err := createAutoWorkspaces(output.Dir, *raidConfig, *personaConfig)
	if err != nil {
		return err
	}

	metrics, err := runAutoDialogue(raidWorkspace, personaWorkspace, output.ChatLogPath, autoOptions{
		Timeout:      *timeout,
		CloseTimeout: *closeTimeout,
	})
	if err != nil {
		return err
	}
	if err := writeAutoStats(output.StatsPath, metrics, raidWorkspace, personaWorkspace); err != nil {
		return err
	}
	restoreOutput()
	restoreOutput = func() {}
	fmt.Printf("wrote test-auto %s\n", output.Dir)
	return nil
}

type autoRunOutput struct {
	Dir         string
	ChatLogPath string
	StatsPath   string
	StdoutPath  string
	StderrPath  string
}

func prepareAutoRunOutput(raidConfig, personaConfig, outDir string) (autoRunOutput, error) {
	runName := fmt.Sprintf("%s_%s_%s", autoRunNamePart(raidConfig), autoRunNamePart(personaConfig), time.Now().Format("20060102_150405"))
	if strings.TrimSpace(outDir) == "" {
		outDir = ".out"
	}
	runDir := filepath.Join(outDir, runName)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return autoRunOutput{}, fmt.Errorf("create auto output directory: %w", err)
	}
	return autoRunOutput{
		Dir:         runDir,
		ChatLogPath: filepath.Join(runDir, "chat_log.txt"),
		StatsPath:   filepath.Join(runDir, "stats.txt"),
		StdoutPath:  filepath.Join(runDir, "stdout.txt"),
		StderrPath:  filepath.Join(runDir, "stderr.txt"),
	}, nil
}

func captureAutoRunOutput(output autoRunOutput) (func(), error) {
	stdoutFile, err := os.OpenFile(output.StdoutPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open auto stdout: %w", err)
	}
	stderrFile, err := os.OpenFile(output.StderrPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		_ = stdoutFile.Close()
		return nil, fmt.Errorf("open auto stderr: %w", err)
	}
	oldStdout := os.Stdout
	oldStderr := os.Stderr
	os.Stdout = stdoutFile
	os.Stderr = stderrFile
	return func() {
		os.Stdout = oldStdout
		os.Stderr = oldStderr
		_ = stdoutFile.Close()
		_ = stderrFile.Close()
	}, nil
}

func autoRunNamePart(name string) string {
	name = strings.TrimSuffix(filepath.Base(strings.TrimSpace(name)), filepath.Ext(name))
	if name == "" || name == "." || name == string(filepath.Separator) {
		return "auto"
	}
	var b strings.Builder
	lastUnderscore := false
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastUnderscore = false
		case r == '-' || r == '_':
			if !lastUnderscore {
				b.WriteByte('_')
				lastUnderscore = true
			}
		default:
			if !lastUnderscore {
				b.WriteByte('_')
				lastUnderscore = true
			}
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return "auto"
	}
	return out
}

func createAutoWorkspaces(runDir, raidConfig, personaConfig string) (string, string, error) {
	if strings.TrimSpace(runDir) == "" {
		return "", "", fmt.Errorf("create auto workspaces: run directory is required")
	}
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return "", "", fmt.Errorf("create auto workspace root: %w", err)
	}
	raidWorkspace := filepath.Join(runDir, "raid")
	personaWorkspace := filepath.Join(runDir, "persona")
	if err := WriteConfig(templateFS, raidConfig, raidWorkspace); err != nil {
		return "", "", fmt.Errorf("create raid workspace: %w", err)
	}
	if err := WriteConfig(templateFS, personaConfig, personaWorkspace); err != nil {
		return "", "", fmt.Errorf("create persona workspace: %w", err)
	}
	fmt.Printf("created raid workspace %s from config %s\n", raidWorkspace, raidConfig)
	fmt.Printf("created persona workspace %s from config %s\n", personaWorkspace, personaConfig)
	return raidWorkspace, personaWorkspace, nil
}

type autoOptions struct {
	Timeout      time.Duration
	CloseTimeout time.Duration
}

type autoMetrics struct {
	StartedAt        time.Time
	FinishedAt       time.Time
	Elapsed          time.Duration
	TurnsCompleted   int
	Timeout          time.Duration
	StoppedReason    string
	CloseTimeout     time.Duration
	RaidWorkspace    string
	PersonaWorkspace string
	ContextID        string
	Starts           string
	InitialPrompt    string
	Turns            []autoTurnMetric
	Closes           []autoCloseMetric
}

type autoTurnMetric struct {
	Turn              int
	Actor             string
	StartedAt         time.Time
	FirstTokenAt      time.Time
	FinishedAt        time.Time
	Elapsed           time.Duration
	FirstTokenLatency time.Duration
	TokenEvents       int
	OutputChars       int
}

type autoCloseMetric struct {
	Name    string
	Status  string
	Elapsed time.Duration
	Error   string
}

func runAutoDialogue(raidWorkspace, personaWorkspace, outputPath string, opts autoOptions) (autoMetrics, error) {
	if opts.CloseTimeout <= 0 {
		opts.CloseTimeout = 15 * time.Second
	}
	metrics := autoMetrics{
		StartedAt:        time.Now(),
		Timeout:          opts.Timeout,
		CloseTimeout:     opts.CloseTimeout,
		RaidWorkspace:    raidWorkspace,
		PersonaWorkspace: personaWorkspace,
	}
	raid, err := openApp(raidWorkspace)
	if err != nil {
		return metrics, fmt.Errorf("open raid workspace: %w", err)
	}
	attachSimulatedToolHandler(raid)
	persona, err := openApp(personaWorkspace)
	if err != nil {
		_ = raid.Close()
		return metrics, fmt.Errorf("open persona workspace: %w", err)
	}
	attachSimulatedToolHandler(persona)
	metrics.ContextID = raid.Config().Conversation.ContextID

	transcript, err := os.OpenFile(outputPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return metrics, fmt.Errorf("open transcript: %w", err)
	}
	defer func() { _ = transcript.Close() }()

	writeTranscript := func(text string) error {
		if _, err := transcript.WriteString(text); err != nil {
			return fmt.Errorf("write transcript: %w", err)
		}
		if err := transcript.Sync(); err != nil {
			return fmt.Errorf("sync transcript: %w", err)
		}
		return nil
	}

	nextPersonaInput := "你正在进入一个互动故事副本，对话会像语音聊天一样一来一回。请以你的角色用一句自然中文开场，只输出会被直接朗读出来的话，不要写占位符、舞台说明或内心想法。"
	raidStarts := strings.EqualFold(strings.TrimSpace(raid.Config().Conversation.Starts), "self")
	if raidStarts {
		metrics.Starts = "raid"
	} else {
		metrics.Starts = "persona"
		metrics.InitialPrompt = nextPersonaInput
	}
	started := time.Now()
	if raidStarts {
		raidTurn, err := runAutoTurn(raid, "", 0, "raid")
		if err != nil {
			return metrics, fmt.Errorf("raid opening: %w", err)
		}
		metrics.Turns = append(metrics.Turns, raidTurn.Metric)
		var opening strings.Builder
		appendAutoMessage(&opening, "raid", raidTurn.Text, raidTurn.EventLog)
		if err := writeTranscript(opening.String()); err != nil {
			return metrics, err
		}
		nextPersonaInput = raidTurn.Text
	}
	for i := 1; ; i++ {
		if opts.Timeout > 0 && time.Since(started) >= opts.Timeout {
			metrics.StoppedReason = fmt.Sprintf("timeout before turn %d", i)
			break
		}
		var turn strings.Builder
		personaTurn, err := runAutoTurn(persona, nextPersonaInput, i, "persona")
		if err != nil {
			return metrics, fmt.Errorf("persona turn %d: %w", i, err)
		}
		metrics.Turns = append(metrics.Turns, personaTurn.Metric)
		raidTurn, err := runAutoTurn(raid, personaTurn.Text, i, "raid")
		if err != nil {
			return metrics, fmt.Errorf("raid turn %d: %w", i, err)
		}
		metrics.Turns = append(metrics.Turns, raidTurn.Metric)
		appendAutoTurn(&turn, i,
			autoSpeech{Actor: "persona", Text: personaTurn.Text, EventLog: personaTurn.EventLog},
			autoSpeech{Actor: "raid", Text: raidTurn.Text, EventLog: raidTurn.EventLog},
		)
		if err := writeTranscript(turn.String()); err != nil {
			return metrics, err
		}

		nextPersonaInput = raidTurn.Text
		metrics.TurnsCompleted = i
	}
	if metrics.StoppedReason == "" {
		metrics.StoppedReason = "completed"
	}
	metrics.FinishedAt = time.Now()
	metrics.Elapsed = metrics.FinishedAt.Sub(metrics.StartedAt)
	metrics.Closes = append(metrics.Closes, closeAutoApp("persona", persona, opts.CloseTimeout))
	metrics.Closes = append(metrics.Closes, closeAutoApp("raid", raid, opts.CloseTimeout))

	fmt.Printf("wrote transcript %s\n", outputPath)
	return metrics, nil
}

func closeAutoApp(name string, app *claw.Claw, timeout time.Duration) autoCloseMetric {
	started := time.Now()
	out := autoCloseMetric{Name: name}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	err := app.CloseContext(ctx)
	out.Elapsed = time.Since(started)
	if err != nil {
		out.Status = "error"
		out.Error = err.Error()
		if ctx.Err() != nil {
			out.Status = "timeout"
			out.Error = fmt.Sprintf("close exceeded %s: %v", timeout, err)
		}
	} else {
		out.Status = "closed"
	}
	return out
}

func writeAutoStats(outputPath string, metrics autoMetrics, raidWorkspace, personaWorkspace string) error {
	var out strings.Builder
	writeAutoRuntimeInspect(&out, metrics)
	raidMetadataInspect := inspectMemoryMetadataSection(context.Background(), "raid", raidWorkspace)
	raidInspect := inspectBBHWorkspaceSection(context.Background(), "raid", raidWorkspace)
	personaMetadataInspect := inspectMemoryMetadataSection(context.Background(), "persona", personaWorkspace)
	personaInspect := inspectBBHWorkspaceSection(context.Background(), "persona", personaWorkspace)
	fmt.Fprintf(&out, "\n%s", strings.TrimRight(raidMetadataInspect, "\n"))
	fmt.Fprintf(&out, "\n%s", strings.TrimRight(raidInspect, "\n"))
	fmt.Fprintf(&out, "\n\n%s", strings.TrimRight(personaMetadataInspect, "\n"))
	fmt.Fprintf(&out, "\n\n%s\n", strings.TrimRight(personaInspect, "\n"))
	if err := os.WriteFile(outputPath, []byte(strings.TrimLeft(out.String(), "\n")), 0o644); err != nil {
		return err
	}
	return nil
}

func inspectBBHWorkspaceSection(ctx context.Context, label, workspaceDir string) string {
	inspection, err := inspectBBHWorkspace(ctx, workspaceDir)
	if err != nil {
		fallback, fallbackErr := inspectBBHWorkspaceFiles(ctx, workspaceDir)
		if fallbackErr != nil {
			return fmt.Sprintf("--- %s inspect ---\nerror: %v\nfallback_error: %v\n", label, err, fallbackErr)
		}
		return fmt.Sprintf("--- %s inspect ---\nmode: files_only\nerror: %v\n%s", label, err, strings.TrimLeft(fallback, "\n"))
	}
	return fmt.Sprintf("--- %s inspect ---\n%s", label, strings.TrimRight(inspection, "\n"))
}

func inspectMemoryMetadataSection(ctx context.Context, label, workspaceDir string) string {
	text, err := inspectMemoryMetadata(ctx, workspaceDir)
	if err != nil {
		return fmt.Sprintf("--- %s metadata inspect ---\nerror: %v\n", label, err)
	}
	return fmt.Sprintf("--- %s metadata inspect ---\n%s", label, strings.TrimRight(text, "\n"))
}

type metadataStateInspection struct {
	Version     int                        `json:"version"`
	Facts       []json.RawMessage          `json:"facts"`
	Evidence    []json.RawMessage          `json:"evidence"`
	SideEffects []metadataStatusRecord     `json:"side_effects"`
	Async       []metadataStatusRecord     `json:"async_semantic"`
	Counters    map[string]json.RawMessage `json:"counters"`
}

type metadataStatusRecord struct {
	Status string `json:"status"`
	Job    struct {
		Scope metadataScope `json:"scope"`
	} `json:"job"`
	Failure metadataFailure `json:"failure"`
}

type metadataFailure struct {
	ErrClass     string `json:"ErrClass,omitempty"`
	Err          string `json:"Err,omitempty"`
	RetryAt      string `json:"RetryAt,omitempty"`
	ErrClassJSON string `json:"err_class,omitempty"`
	ErrJSON      string `json:"err,omitempty"`
	RetryAtJSON  string `json:"retry_at,omitempty"`
}

type metadataScope struct {
	RuntimeID     string `json:"RuntimeID,omitempty"`
	UserID        string `json:"UserID,omitempty"`
	AgentID       string `json:"AgentID,omitempty"`
	RuntimeIDJSON string `json:"runtime_id,omitempty"`
	UserIDJSON    string `json:"user_id,omitempty"`
	AgentIDJSON   string `json:"agent_id,omitempty"`
}

func (s metadataScope) label() string {
	runtimeID := firstNonEmpty(s.RuntimeID, s.RuntimeIDJSON)
	userID := firstNonEmpty(s.UserID, s.UserIDJSON)
	agentID := firstNonEmpty(s.AgentID, s.AgentIDJSON)
	if runtimeID == "" && userID == "" && agentID == "" {
		return "unknown"
	}
	return fmt.Sprintf("runtime=%s user=%s agent=%s", runtimeID, userID, agentID)
}

func inspectMemoryMetadata(ctx context.Context, workspaceDir string) (string, error) {
	ws, err := sdkworkspace.NewLocalWorkspace(workspaceDir)
	if err != nil {
		return "", fmt.Errorf("open workspace: %w", err)
	}
	memoryRoot, err := inspectMemoryRoot(ctx, ws)
	if err != nil {
		return "", err
	}
	statePath := filepath.Join(workspaceDir, memoryRoot, "metadata", "state.json")
	exists, size := localPathSize(statePath)
	var out strings.Builder
	fmt.Fprintf(&out, "path: %s\n", statePath)
	fmt.Fprintf(&out, "exists: %t\n", exists)
	fmt.Fprintf(&out, "size_bytes: %d\n", size)
	if !exists {
		return out.String(), nil
	}
	raw, err := os.ReadFile(statePath)
	if err != nil {
		return "", fmt.Errorf("read metadata state: %w", err)
	}
	var st metadataStateInspection
	if err := json.Unmarshal(raw, &st); err != nil {
		return "", fmt.Errorf("decode metadata state: %w", err)
	}
	fmt.Fprintf(&out, "version: %d\n", st.Version)
	fmt.Fprintf(&out, "facts: %d\n", len(st.Facts))
	fmt.Fprintf(&out, "evidence: %d\n", len(st.Evidence))
	writeStatusCounts(&out, "side_effects", st.SideEffects)
	writeScopeCounts(&out, "side_effect_scopes", st.SideEffects)
	writeFailureSummary(&out, "side_effect_failures", st.SideEffects)
	writeStatusCounts(&out, "async_semantic", st.Async)
	writeScopeCounts(&out, "async_semantic_scopes", st.Async)
	writeFailureSummary(&out, "async_semantic_failures", st.Async)
	fmt.Fprintf(&out, "counter_partitions: %d\n", len(st.Counters))
	return out.String(), nil
}

func inspectBBHWorkspace(ctx context.Context, workspaceDir string) (string, error) {
	ws, err := sdkworkspace.NewLocalWorkspace(workspaceDir)
	if err != nil {
		return "", fmt.Errorf("open workspace: %w", err)
	}
	memoryRoot, err := inspectMemoryRoot(ctx, ws)
	if err != nil {
		return "", err
	}
	memoryWS, err := ws.Sub(memoryRoot)
	if err != nil {
		return "", fmt.Errorf("open memory workspace: %w", err)
	}
	retrievalWS, err := memoryWS.Sub("retrieval")
	if err != nil {
		return "", fmt.Errorf("open retrieval workspace: %w", err)
	}
	inspector, err := bbh.NewInspector(retrievalWS)
	if err != nil {
		return "", err
	}
	defer func() { _ = inspector.Close() }()
	inspection, err := inspector.Inspect(ctx)
	if err != nil {
		return "", err
	}
	var out strings.Builder
	writeBBHInspection(&out, inspection)
	return out.String(), nil
}

func inspectBBHWorkspaceFiles(ctx context.Context, workspaceDir string) (string, error) {
	ws, err := sdkworkspace.NewLocalWorkspace(workspaceDir)
	if err != nil {
		return "", fmt.Errorf("open workspace: %w", err)
	}
	memoryRoot, err := inspectMemoryRoot(ctx, ws)
	if err != nil {
		return "", err
	}
	root := filepath.Join(workspaceDir, memoryRoot, "retrieval")
	badgerPath := filepath.Join(root, "badger")
	blevePath := filepath.Join(root, "bleve")
	hnswPath := filepath.Join(root, "hnsw")
	badgerExists, badgerSize := localPathSize(badgerPath)
	bleveExists, bleveSize := localPathSize(blevePath)
	hnswExists, hnswSize := localPathSize(hnswPath)
	namespaces, err := inspectBBHNamespaceFiles(ctx, blevePath, hnswPath)
	if err != nil {
		return "", err
	}
	var out strings.Builder
	fmt.Fprintf(&out, "root: %s\n", root)
	fmt.Fprintf(&out, "total_size_bytes: %d\n", badgerSize+bleveSize+hnswSize)
	fmt.Fprintf(&out, "physical_namespace_count: %d\n", len(namespaces))
	fmt.Fprintf(&out, "doc_namespace_count: unavailable\n")
	fmt.Fprintf(&out, "total_docs: unavailable\n")
	fmt.Fprintf(&out, "total_vector_docs: unavailable\n")
	fmt.Fprintf(&out, "badger:\n")
	fmt.Fprintf(&out, "  path: %s\n", badgerPath)
	fmt.Fprintf(&out, "  exists: %t\n", badgerExists)
	fmt.Fprintf(&out, "  size_bytes: %d\n", badgerSize)
	fmt.Fprintf(&out, "bleve:\n")
	fmt.Fprintf(&out, "  path: %s\n", blevePath)
	fmt.Fprintf(&out, "  exists: %t\n", bleveExists)
	fmt.Fprintf(&out, "  size_bytes: %d\n", bleveSize)
	fmt.Fprintf(&out, "  index_count: %d\n", countFileNamespaces(namespaces, "bleve"))
	fmt.Fprintf(&out, "hnsw:\n")
	fmt.Fprintf(&out, "  path: %s\n", hnswPath)
	fmt.Fprintf(&out, "  exists: %t\n", hnswExists)
	fmt.Fprintf(&out, "  size_bytes: %d\n", hnswSize)
	fmt.Fprintf(&out, "  graph_file_count: %d\n", countFileNamespaces(namespaces, "hnsw"))
	fmt.Fprintf(&out, "  graph_count: unavailable\n")
	if len(namespaces) > 0 {
		fmt.Fprintf(&out, "namespaces:\n")
		for _, ns := range namespaces {
			fmt.Fprintf(&out, "  - namespace: %s\n", ns.namespace)
			fmt.Fprintf(&out, "    token: %s\n", ns.token)
			if ns.decodeError != "" {
				fmt.Fprintf(&out, "    decode_error: %s\n", ns.decodeError)
			}
			fmt.Fprintf(&out, "    docs: unavailable\n")
			fmt.Fprintf(&out, "    vector_docs: unavailable\n")
			fmt.Fprintf(&out, "    bleve_bytes: %d\n", ns.bleveBytes)
			fmt.Fprintf(&out, "    hnsw_bytes: %d\n", ns.hnswBytes)
			fmt.Fprintf(&out, "    source_bleve: %t\n", ns.sourceBleve)
			fmt.Fprintf(&out, "    source_hnsw: %t\n", ns.sourceHNSW)
		}
	}
	return out.String(), nil
}

type fileNamespaceInspect struct {
	namespace   string
	token       string
	decodeError string
	bleveBytes  int64
	hnswBytes   int64
	sourceBleve bool
	sourceHNSW  bool
}

func inspectBBHNamespaceFiles(ctx context.Context, blevePath, hnswPath string) ([]fileNamespaceInspect, error) {
	byToken := map[string]*fileNamespaceInspect{}
	if err := inspectBleveNamespaceFiles(ctx, blevePath, byToken); err != nil {
		return nil, err
	}
	if err := inspectHNSWNamespaceFiles(ctx, hnswPath, byToken); err != nil {
		return nil, err
	}
	out := make([]fileNamespaceInspect, 0, len(byToken))
	for _, ns := range byToken {
		out = append(out, *ns)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].namespace == out[j].namespace {
			return out[i].token < out[j].token
		}
		return out[i].namespace < out[j].namespace
	})
	return out, nil
}

func inspectBleveNamespaceFiles(ctx context.Context, blevePath string, byToken map[string]*fileNamespaceInspect) error {
	entries, err := os.ReadDir(blevePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read bleve dir: %w", err)
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !entry.IsDir() {
			continue
		}
		ns := ensureFileNamespace(byToken, entry.Name())
		ns.sourceBleve = true
		_, ns.bleveBytes = localPathSize(filepath.Join(blevePath, entry.Name()))
	}
	return nil
}

func inspectHNSWNamespaceFiles(ctx context.Context, hnswPath string, byToken map[string]*fileNamespaceInspect) error {
	entries, err := os.ReadDir(hnswPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read hnsw dir: %w", err)
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".graph") {
			continue
		}
		token := strings.TrimSuffix(entry.Name(), ".graph")
		ns := ensureFileNamespace(byToken, token)
		ns.sourceHNSW = true
		_, ns.hnswBytes = localPathSize(filepath.Join(hnswPath, entry.Name()))
	}
	return nil
}

func ensureFileNamespace(byToken map[string]*fileNamespaceInspect, token string) *fileNamespaceInspect {
	if ns, ok := byToken[token]; ok {
		return ns
	}
	ns := &fileNamespaceInspect{token: token, namespace: token}
	if raw, err := base64.RawURLEncoding.DecodeString(token); err == nil {
		ns.namespace = string(raw)
	} else {
		ns.decodeError = err.Error()
	}
	byToken[token] = ns
	return ns
}

func countFileNamespaces(namespaces []fileNamespaceInspect, source string) int {
	count := 0
	for _, ns := range namespaces {
		switch source {
		case "bleve":
			if ns.sourceBleve {
				count++
			}
		case "hnsw":
			if ns.sourceHNSW {
				count++
			}
		}
	}
	return count
}

func writeStatusCounts(w io.Writer, label string, records []metadataStatusRecord) {
	counts := map[string]int{}
	for _, record := range records {
		status := strings.TrimSpace(record.Status)
		if status == "" {
			status = "unknown"
		}
		counts[status]++
	}
	fmt.Fprintf(w, "%s: %d", label, len(records))
	if len(counts) == 0 {
		fmt.Fprintln(w)
		return
	}
	statuses := make([]string, 0, len(counts))
	for status := range counts {
		statuses = append(statuses, status)
	}
	sort.Strings(statuses)
	for _, status := range statuses {
		fmt.Fprintf(w, " %s=%d", status, counts[status])
	}
	fmt.Fprintln(w)
}

func writeScopeCounts(w io.Writer, label string, records []metadataStatusRecord) {
	counts := map[string]int{}
	for _, record := range records {
		counts[record.Job.Scope.label()]++
	}
	if len(counts) == 0 {
		fmt.Fprintf(w, "%s: 0\n", label)
		return
	}
	scopes := make([]string, 0, len(counts))
	for scope := range counts {
		scopes = append(scopes, scope)
	}
	sort.Strings(scopes)
	fmt.Fprintf(w, "%s:\n", label)
	for _, scope := range scopes {
		fmt.Fprintf(w, "  - %s count=%d\n", scope, counts[scope])
	}
}

func writeFailureSummary(w io.Writer, label string, records []metadataStatusRecord) {
	var first metadataFailure
	var count int
	for _, record := range records {
		if record.Failure.err() == "" {
			continue
		}
		count++
		if first.err() == "" {
			first = record.Failure
		}
	}
	fmt.Fprintf(w, "%s: %d\n", label, count)
	if first.err() == "" {
		return
	}
	fmt.Fprintf(w, "%s_first_class: %s\n", label, first.errClass())
	fmt.Fprintf(w, "%s_first_error: %s\n", label, first.err())
	if retryAt := first.retryAt(); retryAt != "" {
		fmt.Fprintf(w, "%s_first_retry_at: %s\n", label, retryAt)
	}
}

func (f metadataFailure) errClass() string {
	return firstNonEmpty(f.ErrClass, f.ErrClassJSON)
}

func (f metadataFailure) err() string {
	return firstNonEmpty(f.Err, f.ErrJSON)
}

func (f metadataFailure) retryAt() string {
	return firstNonEmpty(f.RetryAt, f.RetryAtJSON)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func localPathSize(path string) (bool, int64) {
	var total int64
	err := filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || os.IsNotExist(err) {
			return false, 0
		}
		return true, total
	}
	return true, total
}

func inspectMemoryRoot(ctx context.Context, ws sdkworkspace.Workspace) (string, error) {
	raw, err := ws.Read(ctx, configFileName)
	if err != nil {
		return "", fmt.Errorf("read workspace config: %w", err)
	}
	var cfg struct {
		Workspace struct {
			MemoryRoot string `json:"memory_root,omitempty"`
		} `json:"workspace,omitempty"`
	}
	decoded, err := decodeConfigFile(raw)
	if err != nil {
		return "", fmt.Errorf("decode workspace config: %w", err)
	}
	cfg.Workspace.MemoryRoot = decoded.Workspace.MemoryRoot
	if strings.TrimSpace(cfg.Workspace.MemoryRoot) == "" {
		return claw.DefaultConfig().Workspace.MemoryRoot, nil
	}
	return cfg.Workspace.MemoryRoot, nil
}

func writeAutoRuntimeInspect(w io.Writer, metrics autoMetrics) {
	fmt.Fprintln(w, "\n--- runtime inspect ---")
	fmt.Fprintf(w, "raid_workspace: %s\n", metrics.RaidWorkspace)
	fmt.Fprintf(w, "persona_workspace: %s\n", metrics.PersonaWorkspace)
	fmt.Fprintf(w, "context: %s\n", metrics.ContextID)
	fmt.Fprintf(w, "starts: %s\n", metrics.Starts)
	if metrics.InitialPrompt != "" {
		fmt.Fprintf(w, "initial_prompt: %s\n", metrics.InitialPrompt)
	}
	fmt.Fprintf(w, "started_at: %s\n", metrics.StartedAt.Format(time.RFC3339))
	fmt.Fprintf(w, "finished_at: %s\n", metrics.FinishedAt.Format(time.RFC3339))
	fmt.Fprintf(w, "elapsed: %s\n", metrics.Elapsed.Round(time.Millisecond))
	fmt.Fprintf(w, "turns_completed: %d\n", metrics.TurnsCompleted)
	fmt.Fprintf(w, "timeout: %s\n", metrics.Timeout)
	fmt.Fprintf(w, "stopped_reason: %s\n", metrics.StoppedReason)
	fmt.Fprintf(w, "close_timeout: %s\n", metrics.CloseTimeout)
	for _, turn := range metrics.Turns {
		if turn.Turn == 0 {
			fmt.Fprintf(w, "\n--- opening %s ---\n", turn.Actor)
		} else {
			fmt.Fprintf(w, "\n--- turn %d %s ---\n", turn.Turn, turn.Actor)
		}
		fmt.Fprintf(w, "started_at: %s\n", turn.StartedAt.Format(time.RFC3339))
		fmt.Fprintf(w, "finished_at: %s\n", turn.FinishedAt.Format(time.RFC3339))
		fmt.Fprintf(w, "elapsed: %s\n", turn.Elapsed.Round(time.Millisecond))
		if turn.FirstTokenAt.IsZero() {
			fmt.Fprintln(w, "first_token_at: none")
			fmt.Fprintln(w, "first_token_latency: none")
		} else {
			fmt.Fprintf(w, "first_token_at: %s\n", turn.FirstTokenAt.Format(time.RFC3339Nano))
			fmt.Fprintf(w, "first_token_latency: %s\n", turn.FirstTokenLatency.Round(time.Millisecond))
		}
		fmt.Fprintf(w, "token_events: %d\n", turn.TokenEvents)
		fmt.Fprintf(w, "output_chars: %d\n", turn.OutputChars)
	}
	for _, close := range metrics.Closes {
		fmt.Fprintf(w, "%s_close_status: %s\n", close.Name, close.Status)
		fmt.Fprintf(w, "%s_close_elapsed: %s\n", close.Name, close.Elapsed.Round(time.Millisecond))
		if close.Error != "" {
			fmt.Fprintf(w, "%s_close_error: %s\n", close.Name, close.Error)
		}
	}
}

func writeBBHInspection(w io.Writer, inspection *bbh.Inspection) {
	totalSize := inspection.BadgerSizeBytes + inspection.BleveSizeBytes + inspection.HNSWSizeBytes
	fmt.Fprintf(w, "root: %s\n", inspection.Root)
	fmt.Fprintf(w, "total_size_bytes: %d\n", totalSize)
	fmt.Fprintf(w, "physical_namespace_count: %d\n", inspection.PhysicalNamespaceCount)
	fmt.Fprintf(w, "doc_namespace_count: %d\n", inspection.DocNamespaceCount)
	fmt.Fprintf(w, "total_docs: %d\n", inspection.TotalDocs)
	fmt.Fprintf(w, "total_vector_docs: %d\n", inspection.TotalVectorDocs)
	fmt.Fprintf(w, "badger:\n")
	fmt.Fprintf(w, "  path: %s\n", inspection.BadgerPath)
	fmt.Fprintf(w, "  exists: %t\n", inspection.BadgerExists)
	fmt.Fprintf(w, "  size_bytes: %d\n", inspection.BadgerSizeBytes)
	fmt.Fprintf(w, "bleve:\n")
	fmt.Fprintf(w, "  path: %s\n", inspection.BlevePath)
	fmt.Fprintf(w, "  exists: %t\n", inspection.BleveExists)
	fmt.Fprintf(w, "  size_bytes: %d\n", inspection.BleveSizeBytes)
	fmt.Fprintf(w, "  index_count: %d\n", countBleveIndexes(inspection.Namespaces))
	fmt.Fprintf(w, "hnsw:\n")
	fmt.Fprintf(w, "  path: %s\n", inspection.HNSWPath)
	fmt.Fprintf(w, "  exists: %t\n", inspection.HNSWExists)
	fmt.Fprintf(w, "  size_bytes: %d\n", inspection.HNSWSizeBytes)
	fmt.Fprintf(w, "  graph_file_count: %d\n", countHNSWGraphFiles(inspection.Namespaces))
	fmt.Fprintf(w, "  graph_count: %d\n", countHNSWGraphs(inspection.Namespaces))
	if len(inspection.Namespaces) == 0 {
		return
	}
	fmt.Fprintf(w, "namespaces:\n")
	for _, ns := range inspection.Namespaces {
		fmt.Fprintf(w, "  - namespace: %s\n", ns.Namespace)
		fmt.Fprintf(w, "    token: %s\n", ns.Token)
		fmt.Fprintf(w, "    docs: %d\n", ns.DocCount)
		fmt.Fprintf(w, "    vector_docs: %d\n", ns.VectorDocCount)
		fmt.Fprintf(w, "    badger_bytes: %d\n", ns.BadgerBytes)
		fmt.Fprintf(w, "    bleve_bytes: %d\n", ns.BleveSizeBytes)
		fmt.Fprintf(w, "    hnsw_bytes: %d\n", ns.HNSWSizeBytes)
		fmt.Fprintf(w, "    empty: %t\n", ns.Empty)
	}
}

func countBleveIndexes(namespaces []bbh.NamespaceInspection) int {
	count := 0
	for _, ns := range namespaces {
		if ns.BleveExists {
			count++
		}
	}
	return count
}

func countHNSWGraphFiles(namespaces []bbh.NamespaceInspection) int {
	count := 0
	for _, ns := range namespaces {
		if ns.HNSWExists {
			count++
		}
	}
	return count
}

func countHNSWGraphs(namespaces []bbh.NamespaceInspection) int {
	count := 0
	for _, ns := range namespaces {
		if ns.HNSWExists && ns.HNSWSizeBytes > 0 {
			count++
		}
	}
	return count
}

func appendFile(path, text string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open transcript for append: %w", err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(text); err != nil {
		return fmt.Errorf("append transcript: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync transcript: %w", err)
	}
	return nil
}

type autoTurnResult struct {
	Text     string
	EventLog string
	Metric   autoTurnMetric
}

func runAutoTurn(app *claw.Claw, text string, turn int, actor string) (autoTurnResult, error) {
	metric := autoTurnMetric{
		Turn:      turn,
		Actor:     actor,
		StartedAt: time.Now(),
	}
	resp, err := app.RoundTrip(claw.Request{
		Context: context.Background(),
		Text:    text,
	})
	if err != nil {
		metric.FinishedAt = time.Now()
		metric.Elapsed = metric.FinishedAt.Sub(metric.StartedAt)
		return autoTurnResult{}, err
	}
	var out strings.Builder
	eventLog := newAutoEventLog()
	for {
		ev, err := resp.Next()
		if errors.Is(err, io.EOF) {
			metric.FinishedAt = time.Now()
			metric.Elapsed = metric.FinishedAt.Sub(metric.StartedAt)
			metric.OutputChars = len(out.String())
			return autoTurnResult{
				Text:     sanitizeAutoText(out.String()),
				EventLog: eventLog.String(),
				Metric:   metric,
			}, nil
		}
		if err != nil {
			metric.FinishedAt = time.Now()
			metric.Elapsed = metric.FinishedAt.Sub(metric.StartedAt)
			return autoTurnResult{}, err
		}
		eventLog.Record(ev)
		switch ev.Type {
		case claw.EventToken:
			if metric.FirstTokenAt.IsZero() {
				metric.FirstTokenAt = time.Now()
				metric.FirstTokenLatency = metric.FirstTokenAt.Sub(metric.StartedAt)
			}
			metric.TokenEvents++
			out.WriteString(ev.Content)
		case claw.EventError:
			metric.FinishedAt = time.Now()
			metric.Elapsed = metric.FinishedAt.Sub(metric.StartedAt)
			return autoTurnResult{}, fmt.Errorf("%s", ev.Err)
		}
	}
}

type autoSpeech struct {
	Actor    string
	Text     string
	EventLog string
}

func appendAutoTurn(dst *strings.Builder, turn int, speeches ...autoSpeech) {
	if turn > 1 {
		dst.WriteByte('\n')
	}
	fmt.Fprintf(dst, "=== Turn %d ===\n", turn)
	for _, speech := range speeches {
		appendAutoMessage(dst, speech.Actor, speech.Text, speech.EventLog)
	}
}

func appendAutoMessage(dst *strings.Builder, actor, text string, eventLog ...string) {
	for _, log := range eventLog {
		if strings.TrimSpace(log) == "" {
			continue
		}
		dst.WriteString(strings.TrimRight(log, "\n"))
		dst.WriteString("\n\n")
		return
	}
	fmt.Fprintf(dst, "--- %s ---\n%s\n\n", actor, sanitizeAutoText(text))
}

type autoEventLog struct {
	order []string
	seen  map[string]bool
	nodes map[string]*strings.Builder
}

func newAutoEventLog() *autoEventLog {
	return &autoEventLog{
		seen:  make(map[string]bool),
		nodes: make(map[string]*strings.Builder),
	}
}

func (l *autoEventLog) Record(ev claw.Event) {
	if l == nil {
		return
	}
	switch ev.Type {
	case "parallel_branch_accept", "parallel_branch_cancel":
		// Branch terminal events are control-plane data. Keep them out of
		// chat_log.txt; the node sections are meant to show generated text.
		return
	}

	nodeID := strings.TrimSpace(ev.NodeID)
	if nodeID == "" {
		if ev.Type != claw.EventResult {
			nodeID = "run"
		} else {
			return
		}
	}
	dst := l.node(nodeID)
	switch ev.Type {
	case claw.EventToken:
		dst.WriteString(ev.Content)
	case claw.EventToolCall:
		appendAutoEventLine(dst, fmt.Sprintf("tool_call: %s arguments=%v", ev.Name, ev.Arguments))
	case claw.EventToolResult:
		appendAutoEventLine(dst, fmt.Sprintf("tool_result: %s content=%s", ev.Name, ev.Content))
	case claw.EventError:
		fmt.Fprintf(dst, "\n[%s] %s\n", ev.Type, ev.Err)
	case claw.EventResult:
		// Result is a run-level terminal marker; the node sections above
		// already show the useful stream output.
	default:
		fmt.Fprintf(dst, "\n[%s]\n", ev.Type)
	}
}

func appendAutoEventLine(dst *strings.Builder, line string) {
	if dst.Len() > 0 {
		dst.WriteByte('\n')
	}
	dst.WriteString(line)
	dst.WriteByte('\n')
}

func (l *autoEventLog) node(nodeID string) *strings.Builder {
	if !l.seen[nodeID] {
		l.seen[nodeID] = true
		l.order = append(l.order, nodeID)
		l.nodes[nodeID] = &strings.Builder{}
	}
	return l.nodes[nodeID]
}

func (l *autoEventLog) String() string {
	if l == nil {
		return ""
	}
	var out strings.Builder
	for _, nodeID := range l.order {
		body := strings.TrimRight(l.nodes[nodeID].String(), "\n")
		fmt.Fprintf(&out, "--- %s ---\n%s\n\n", nodeID, body)
	}
	return strings.TrimRight(out.String(), "\n")
}

func sanitizeAutoText(text string) string {
	var out strings.Builder
	for _, r := range strings.TrimSpace(text) {
		if isEmojiLikeRune(r) {
			continue
		}
		out.WriteRune(r)
	}
	return strings.TrimSpace(out.String())
}

func isEmojiLikeRune(r rune) bool {
	switch {
	case r == 0xFE0F:
		return true
	case r >= 0x1F000 && r <= 0x1FAFF:
		return true
	case r >= 0x2600 && r <= 0x27BF:
		return true
	default:
		return false
	}
}
