package message

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	sdkmessage "github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

var (
	messageScopeA = sdkmemory.Scope{RuntimeID: "runtime", UserID: "alice", AgentID: "agent-a"}
	messageScopeB = sdkmemory.Scope{RuntimeID: "runtime", UserID: "bob", AgentID: "agent-b"}
)

func TestWorkspaceStoreAppendGetListAndRetry(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	store := newMessageStore(t, workspace.NewMemWorkspace(), WithClock(func() time.Time {
		value := now
		now = now.Add(time.Second)
		return value
	}))
	input := []sdkmessage.Message{
		sdkmessage.NewTextMessage(sdkmessage.RoleUser, "hello"),
		sdkmessage.NewTextMessage(sdkmessage.RoleAssistant, "hi"),
	}
	metadata := sdkmemory.Metadata{"source": "test"}
	first, err := store.Append(ctx, AppendRequest{
		Scope: messageScopeA, ConversationID: "conv", IdempotencyKey: "turn-1",
		Messages: input, Metadata: metadata,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || first[0].Seq != 1 || first[1].Seq != 2 {
		t.Fatalf("append sequence = %#v", first)
	}
	if first[0].ID == "" || first[0].ID == first[1].ID || first[0].CreatedAt.IsZero() {
		t.Fatalf("authority fields not assigned: %#v", first)
	}

	retry, err := store.Append(ctx, AppendRequest{
		Scope: messageScopeA, ConversationID: "conv", IdempotencyKey: "turn-1",
		Messages: []sdkmessage.Message{sdkmessage.NewTextMessage(sdkmessage.RoleUser, "changed")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, retry) {
		t.Fatalf("retry = %#v, want first result %#v", retry, first)
	}
	listed, err := store.List(ctx, messageScopeA, "conv", ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 {
		t.Fatalf("retry duplicated records: got %d", len(listed))
	}
	got, ok, err := store.Get(ctx, messageScopeA, "conv", first[1].ID)
	if err != nil || !ok || got.Message.Content.Text() != "hi" {
		t.Fatalf("Get = %#v, %v, %v", got, ok, err)
	}
}

func TestWorkspaceStoreCommitIdentityAndListingAreStable(t *testing.T) {
	ctx := context.Background()
	ws := workspace.NewMemWorkspace()
	store := newMessageStore(t, ws)
	first, err := store.Commit(ctx, AppendRequest{
		Scope: messageScopeA, ConversationID: "conv", IdempotencyKey: "turn-1",
		Messages: []sdkmessage.Message{sdkmessage.NewTextMessage(sdkmessage.RoleUser, "one")},
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Commit(ctx, AppendRequest{
		Scope: messageScopeA, ConversationID: "conv", IdempotencyKey: "turn-2",
		Messages: []sdkmessage.Message{sdkmessage.NewTextMessage(sdkmessage.RoleUser, "two")},
	})
	if err != nil {
		t.Fatal(err)
	}
	retry, err := newMessageStore(t, ws).Commit(ctx, AppendRequest{
		Scope: messageScopeA, ConversationID: "conv", IdempotencyKey: "turn-1",
		Messages: []sdkmessage.Message{sdkmessage.NewTextMessage(sdkmessage.RoleUser, "ignored")},
	})
	if err != nil || retry.ID != first.ID || retry.Version != first.Version {
		t.Fatalf("retry = %#v, err=%v", retry, err)
	}
	listed, err := store.ListCommits(ctx, messageScopeA, "conv", ListCommitOptions{AfterVersion: first.Version})
	if err != nil || len(listed) != 1 || listed[0].ID != second.ID || second.Version <= first.Version {
		t.Fatalf("commits = %#v, err=%v", listed, err)
	}
}

func TestCommitIDRemainsByteCompatibleWithLegacyScopeEncoding(t *testing.T) {
	scope := sdkmemory.Scope{RuntimeID: "runtime", UserID: "user", AgentID: "agent"}
	legacyInput := scope.RuntimeID + "\x00" + scope.UserID + "\x00" + scope.AgentID + "\x00conv\x00turn"
	sum := sha256.Sum256([]byte(legacyInput))
	want := "commit-" + hex.EncodeToString(sum[:])

	if got := commitID(scope, "conv", "turn"); got != want {
		t.Fatalf("commitID() = %q, want %q", got, want)
	}
}

func TestWorkspaceStoreHardPartitionAndConversationIsolation(t *testing.T) {
	ctx := context.Background()
	store := newMessageStore(t, workspace.NewMemWorkspace())
	for _, item := range []struct {
		scope sdkmemory.Scope
		conv  string
		text  string
	}{
		{messageScopeA, "same", "alice"},
		{messageScopeB, "same", "bob"},
		{messageScopeA, "other", "other"},
	} {
		_, err := store.Append(ctx, AppendRequest{
			Scope: item.scope, ConversationID: item.conv, IdempotencyKey: item.text,
			Messages: []sdkmessage.Message{sdkmessage.NewTextMessage(sdkmessage.RoleUser, item.text)},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	alice, _ := store.List(ctx, messageScopeA, "same", ListOptions{})
	bob, _ := store.List(ctx, messageScopeB, "same", ListOptions{})
	if len(alice) != 1 || alice[0].Message.Content.Text() != "alice" ||
		len(bob) != 1 || bob[0].Message.Content.Text() != "bob" {
		t.Fatalf("partition leak: alice=%#v bob=%#v", alice, bob)
	}
	conversations, err := store.ListConversations(ctx, messageScopeA)
	if err != nil || !reflect.DeepEqual(conversations, []string{"other", "same"}) {
		t.Fatalf("conversations = %v, %v", conversations, err)
	}
}

func TestWorkspaceStoreAgentPartitionIsolation(t *testing.T) {
	ctx := context.Background()
	store := newMessageStore(t, workspace.NewMemWorkspace())
	scopeA := sdkmemory.Scope{RuntimeID: "runtime", UserID: "same-user", AgentID: "agent-a"}
	scopeB := sdkmemory.Scope{RuntimeID: "runtime", UserID: "same-user", AgentID: "agent-b"}
	global := sdkmemory.Scope{RuntimeID: "runtime", UserID: "same-user"}
	for _, item := range []struct {
		scope sdkmemory.Scope
		text  string
	}{
		{scopeA, "a"}, {scopeB, "b"}, {global, "global"},
	} {
		if _, err := store.Commit(ctx, AppendRequest{
			Scope: item.scope, ConversationID: "conversation", IdempotencyKey: item.text,
			Messages: []sdkmessage.Message{sdkmessage.NewTextMessage(sdkmessage.RoleUser, item.text)},
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, item := range []struct {
		scope sdkmemory.Scope
		text  string
	}{
		{scopeA, "a"}, {scopeB, "b"}, {global, "global"},
	} {
		commits, err := store.ListCommits(ctx, item.scope, "conversation", ListCommitOptions{})
		if err != nil || len(commits) != 1 || commits[0].Scope != item.scope ||
			commits[0].Records[0].Message.Content.Text() != item.text {
			t.Fatalf("ListCommits(%v) = %#v, %v", item.scope, commits, err)
		}
	}
}

func TestWorkspaceStorePersistsAndPaginatesStably(t *testing.T) {
	ctx := context.Background()
	ws := workspace.NewMemWorkspace()
	for index := 0; index < 5; index++ {
		store := newMessageStore(t, ws)
		_, err := store.Append(ctx, AppendRequest{
			Scope: messageScopeA, ConversationID: "conv", IdempotencyKey: fmt.Sprint(index),
			Messages: []sdkmessage.Message{sdkmessage.NewTextMessage(sdkmessage.RoleUser, fmt.Sprint(index))},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	reopened := newMessageStore(t, ws)
	page, err := reopened.List(ctx, messageScopeA, "conv", ListOptions{AfterSeq: 2, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got := []uint64{page[0].Seq, page[1].Seq}; !reflect.DeepEqual(got, []uint64{3, 4}) {
		t.Fatalf("page seqs = %v", got)
	}
	rest, err := reopened.List(ctx, messageScopeA, "conv", ListOptions{AfterSeq: 4, Limit: -1})
	if err != nil || len(rest) != 1 || rest[0].Seq != 5 {
		t.Fatalf("rest = %#v, %v", rest, err)
	}
}

func TestWorkspaceStoreCommitOrderComesFromSeqNotFilename(t *testing.T) {
	ctx := context.Background()
	store := newMessageStore(t, workspace.NewMemWorkspace())
	for _, turn := range []struct{ key, text string }{
		{"z-last-filename", "first turn"},
		{"a-first-filename", "second turn"},
	} {
		if _, err := store.Append(ctx, AppendRequest{
			Scope: messageScopeA, ConversationID: "conv", IdempotencyKey: turn.key,
			Messages: []sdkmessage.Message{sdkmessage.NewTextMessage(sdkmessage.RoleUser, turn.text)},
		}); err != nil {
			t.Fatal(err)
		}
	}
	records, err := store.List(ctx, messageScopeA, "conv", ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{records[0].Message.Content.Text(), records[1].Message.Content.Text()}; !reflect.DeepEqual(got, []string{"first turn", "second turn"}) {
		t.Fatalf("record order = %v", got)
	}
}

func TestWorkspaceStoreAppendPublishesOnlyNewCommit(t *testing.T) {
	ctx := context.Background()
	counting := newCountingWorkspace(workspace.NewMemWorkspace())
	store := newMessageStore(t, counting)
	request := func(key string) AppendRequest {
		return AppendRequest{
			Scope: messageScopeA, ConversationID: "conv", IdempotencyKey: key,
			Messages: []sdkmessage.Message{sdkmessage.NewTextMessage(sdkmessage.RoleUser, key)},
		}
	}
	if _, err := store.Append(ctx, request("turn-1")); err != nil {
		t.Fatal(err)
	}
	firstPath := store.commitPath(messageScopeA, "conv", "turn-1")
	if _, err := store.Append(ctx, request("turn-2")); err != nil {
		t.Fatal(err)
	}
	secondPath := store.commitPath(messageScopeA, "conv", "turn-2")
	if counting.renameCount(firstPath) != 1 || counting.renameCount(secondPath) != 1 {
		t.Fatalf("publish counts: first=%d second=%d", counting.renameCount(firstPath), counting.renameCount(secondPath))
	}
}

func TestWorkspaceStoreConcurrentAppend(t *testing.T) {
	ctx := context.Background()
	store := newMessageStore(t, workspace.NewMemWorkspace())
	const count = 40
	var wait sync.WaitGroup
	errs := make(chan error, count)
	for index := 0; index < count; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			_, err := store.Append(ctx, AppendRequest{
				Scope: messageScopeA, ConversationID: "conv", IdempotencyKey: fmt.Sprint(index),
				Messages: []sdkmessage.Message{sdkmessage.NewTextMessage(sdkmessage.RoleUser, fmt.Sprint(index))},
			})
			errs <- err
		}(index)
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	records, err := store.List(ctx, messageScopeA, "conv", ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != count {
		t.Fatalf("records = %d, want %d", len(records), count)
	}
	for index, record := range records {
		if record.Seq != uint64(index+1) {
			t.Fatalf("record %d seq = %d", index, record.Seq)
		}
	}
}

func TestWorkspaceStoreOwnsInputsAndResults(t *testing.T) {
	ctx := context.Background()
	store := newMessageStore(t, workspace.NewMemWorkspace())
	input := sdkmessage.NewTextMessage(sdkmessage.RoleUser, "original")
	metadata := sdkmemory.Metadata{"key": "original"}
	result, err := store.Append(ctx, AppendRequest{
		Scope: messageScopeA, ConversationID: "conv", IdempotencyKey: "key",
		Messages: []sdkmessage.Message{input}, Metadata: metadata,
	})
	if err != nil {
		t.Fatal(err)
	}
	input.Content.Parts[0] = sdkmessage.TextPart{Text: "input mutation"}
	metadata["key"] = "input mutation"
	result[0].Message.Content.Parts[0] = sdkmessage.TextPart{Text: "result mutation"}
	result[0].Metadata["key"] = "result mutation"
	got, _, err := store.Get(ctx, messageScopeA, "conv", result[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Message.Content.Text() != "original" || got.Metadata["key"] != "original" {
		t.Fatalf("stored value was aliased: %#v", got)
	}
	got.Message.Content.Parts[0] = sdkmessage.TextPart{Text: "get mutation"}
	again, _, _ := store.Get(ctx, messageScopeA, "conv", result[0].ID)
	if again.Message.Content.Text() != "original" {
		t.Fatal("Get result aliases stored content")
	}
}

func TestWorkspaceStoreEncodesMaliciousIDs(t *testing.T) {
	ctx := context.Background()
	ws := workspace.NewMemWorkspace()
	store := newMessageStore(t, ws)
	scope := sdkmemory.Scope{RuntimeID: "../runtime", UserID: "/../../user"}
	conversation := "../../escape/./conversation"
	records, err := store.Append(ctx, AppendRequest{
		Scope: scope, ConversationID: conversation, IdempotencyKey: "../key",
		Messages: []sdkmessage.Message{sdkmessage.NewTextMessage(sdkmessage.RoleUser, "safe")},
	})
	if err != nil || len(records) != 1 {
		t.Fatalf("Append = %#v, %v", records, err)
	}
	commitPath := store.commitPath(scope, conversation, "../key")
	if strings.Contains(commitPath, "..") || strings.Contains(commitPath, conversation) || strings.HasPrefix(commitPath, "/") {
		t.Fatalf("unsafe commit path %q", commitPath)
	}
	if exists, err := ws.Exists(ctx, commitPath); err != nil || !exists {
		t.Fatalf("encoded commit missing: exists=%v err=%v", exists, err)
	}
}

func TestWorkspaceStoreRejectsCorruptionAndUnknownSchema(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name string
		data []byte
	}{
		{"malformed", []byte(`{"schema_version":`)},
		{"unknown schema", []byte(`{"schema_version":99,"runtime_id":"runtime","user_id":"alice","conversation_id":"conv","records":[],"idempotency":{}}`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			ws := workspace.NewMemWorkspace()
			store := newMessageStore(t, ws)
			ws.MustWrite(store.headPath(messageScopeA, "conv"), test.data)
			if _, err := store.List(ctx, messageScopeA, "conv", ListOptions{}); err == nil {
				t.Fatal("List error = nil")
			}
		})
	}

	ws := workspace.NewMemWorkspace()
	store := newMessageStore(t, ws)
	_, err := store.Append(ctx, AppendRequest{
		Scope: messageScopeA, ConversationID: "conv", IdempotencyKey: "key",
		Messages: []sdkmessage.Message{sdkmessage.NewTextMessage(sdkmessage.RoleUser, "ok")},
	})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := ws.Read(ctx, store.recordPath(messageScopeA, "conv", 1))
	var state map[string]any
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	state["seq"] = float64(7)
	corrupt, _ := json.Marshal(state)
	ws.MustWrite(store.recordPath(messageScopeA, "conv", 1), corrupt)
	if _, err := store.List(ctx, messageScopeA, "conv", ListOptions{}); err == nil {
		t.Fatal("invalid sequence was accepted")
	}
}

func TestWorkspaceStoreIgnoresForeignFilesButRejectsDataLikeNames(t *testing.T) {
	ctx := context.Background()
	ws := workspace.NewMemWorkspace()
	store := newMessageStore(t, ws)
	if _, err := store.Append(ctx, AppendRequest{
		Scope: messageScopeA, ConversationID: "conv", IdempotencyKey: "key",
		Messages: []sdkmessage.Message{sdkmessage.NewTextMessage(sdkmessage.RoleUser, "ok")},
	}); err != nil {
		t.Fatal(err)
	}
	ws.MustWrite(store.commitsDir(messageScopeA, "conv")+"/README.txt", []byte("ignored"))
	ws.MustWrite(store.commitsDir(messageScopeA, "conv")+"/.commit.json.tmp", []byte("ignored"))
	if records, err := store.List(ctx, messageScopeA, "conv", ListOptions{}); err != nil || len(records) != 1 {
		t.Fatalf("foreign files affected scan: records=%d err=%v", len(records), err)
	}
	ws.MustWrite(store.commitLocatorsDir(messageScopeA, "conv")+"/k_!!!.json", []byte("{}"))
	if _, err := store.ListCommits(ctx, messageScopeA, "conv", ListCommitOptions{}); err == nil {
		t.Fatal("data-like invalid filename was ignored")
	}
}

func TestWorkspaceStoreValidationAndNilWorkspace(t *testing.T) {
	if _, err := NewWorkspaceStore(nil); err == nil {
		t.Fatal("nil workspace accepted")
	}
	var typedNil *workspace.MemWorkspace
	if _, err := NewWorkspaceStore(typedNil); err == nil {
		t.Fatal("typed nil workspace accepted")
	}
	store := newMessageStore(t, workspace.NewMemWorkspace())
	_, err := store.Append(context.Background(), AppendRequest{
		Scope: messageScopeA, ConversationID: "conv", IdempotencyKey: "key",
		Messages: []sdkmessage.Message{{}},
	})
	if err == nil {
		t.Fatal("invalid canonical message accepted")
	}
}

func TestWorkspaceStoreConversationOrderingIgnoresWorkspaceOrder(t *testing.T) {
	ctx := context.Background()
	store := newMessageStore(t, workspace.NewMemWorkspace())
	input := []string{"z", "a", "m"}
	for _, conversation := range input {
		_, err := store.Append(ctx, AppendRequest{
			Scope: messageScopeA, ConversationID: conversation, IdempotencyKey: conversation,
			Messages: []sdkmessage.Message{sdkmessage.NewTextMessage(sdkmessage.RoleUser, conversation)},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	sort.Strings(input)
	got, err := store.ListConversations(ctx, messageScopeA)
	if err != nil || !reflect.DeepEqual(got, input) {
		t.Fatalf("ListConversations = %v, %v", got, err)
	}
}

func TestWorkspaceStoreHotPathsReadOnlyDelta(t *testing.T) {
	ctx := context.Background()
	counting := newCountingWorkspace(workspace.NewMemWorkspace())
	store := newMessageStore(t, counting)
	for index := 0; index < 300; index++ {
		if _, err := store.Commit(ctx, AppendRequest{
			Scope: messageScopeA, ConversationID: "conv", IdempotencyKey: fmt.Sprint(index),
			Messages: []sdkmessage.Message{sdkmessage.NewTextMessage(sdkmessage.RoleUser, fmt.Sprint(index))},
		}); err != nil {
			t.Fatal(err)
		}
	}

	counting.resetCounts()
	if _, err := store.Commit(ctx, AppendRequest{
		Scope: messageScopeA, ConversationID: "conv", IdempotencyKey: "last",
		Messages: []sdkmessage.Message{sdkmessage.NewTextMessage(sdkmessage.RoleUser, "last")},
	}); err != nil {
		t.Fatal(err)
	}
	if reads := counting.totalReads(); reads > 8 {
		t.Fatalf("incremental Commit reads=%d, want bounded", reads)
	}
	if lists := counting.totalLists(); lists != 0 {
		t.Fatalf("incremental Commit lists=%d, want 0", lists)
	}

	counting.resetCounts()
	latest, err := store.Latest(ctx, messageScopeA, "conv", LatestOptions{Limit: 3})
	if err != nil || len(latest) != 3 {
		t.Fatalf("Latest=%#v err=%v", latest, err)
	}
	if reads := counting.totalReads(); reads > 6 {
		t.Fatalf("Latest reads=%d, want O(limit)", reads)
	}
	if lists := counting.totalLists(); lists != 0 {
		t.Fatalf("Latest lists=%d, want 0", lists)
	}

	counting.resetCounts()
	if _, ok, err := store.Get(ctx, messageScopeA, "conv", latest[0].ID); err != nil || !ok {
		t.Fatalf("Get ok=%v err=%v", ok, err)
	}
	if reads := counting.totalReads(); reads > 3 {
		t.Fatalf("Get reads=%d, want O(1)", reads)
	}

	counting.resetCounts()
	commits, err := store.ListCommits(ctx, messageScopeA, "conv", ListCommitOptions{AfterVersion: 297, Limit: 2})
	if err != nil || len(commits) != 2 {
		t.Fatalf("ListCommits=%#v err=%v", commits, err)
	}
	if reads := counting.totalReads(); reads > 6 {
		t.Fatalf("ListCommits reads=%d, want O(limit)", reads)
	}
}

func TestWorkspaceStoreRepairsCommitPublishedBeforeHead(t *testing.T) {
	ctx := context.Background()
	base := workspace.NewMemWorkspace()
	faults := &failRenameWorkspace{Workspace: base}
	store := newMessageStore(t, faults)
	faults.destination = store.headPath(messageScopeA, "conv")
	request := AppendRequest{
		Scope: messageScopeA, ConversationID: "conv", IdempotencyKey: "turn",
		Messages: []sdkmessage.Message{sdkmessage.NewTextMessage(sdkmessage.RoleUser, "durable")},
	}
	if _, err := store.Commit(ctx, request); err == nil {
		t.Fatal("head publish failure was not surfaced")
	}

	reopened := newMessageStore(t, base)
	repaired, err := reopened.Commit(ctx, request)
	if err != nil || repaired.Version != 1 {
		t.Fatalf("retry repair=%#v err=%v", repaired, err)
	}
	commits, err := reopened.ListCommits(ctx, messageScopeA, "conv", ListCommitOptions{})
	if err != nil || len(commits) != 1 || commits[0].Version != 1 {
		t.Fatalf("repaired commits=%#v err=%v", commits, err)
	}
}

func TestWorkspaceStoreRepairsFailureAfterHeadPublication(t *testing.T) {
	ctx := context.Background()
	base := workspace.NewMemWorkspace()
	faults := &failDeleteWorkspace{Workspace: base}
	store := newMessageStore(t, faults)
	faults.target = store.pendingPath(messageScopeA, "conv")
	if _, err := store.Commit(ctx, AppendRequest{
		Scope: messageScopeA, ConversationID: "conv", IdempotencyKey: "turn",
		Messages: []sdkmessage.Message{sdkmessage.NewTextMessage(sdkmessage.RoleUser, "durable")},
	}); err == nil {
		t.Fatal("post-head cleanup failure was not surfaced")
	}

	reopened := newMessageStore(t, base)
	latest, err := reopened.Latest(ctx, messageScopeA, "conv", LatestOptions{Limit: 1})
	if err != nil || len(latest) != 1 || latest[0].Seq != 1 {
		t.Fatalf("repair after head=%#v err=%v", latest, err)
	}
}

func newMessageStore(t *testing.T, ws workspace.Workspace, options ...Option) *WorkspaceStore {
	t.Helper()
	store, err := NewWorkspaceStore(ws, options...)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

type countingWorkspace struct {
	workspace.Workspace
	mu      sync.Mutex
	renames map[string]int
	reads   map[string]int
	lists   map[string]int
}

type failRenameWorkspace struct {
	workspace.Workspace
	mu          sync.Mutex
	destination string
	failed      bool
}

type failDeleteWorkspace struct {
	workspace.Workspace
	mu     sync.Mutex
	target string
	failed bool
}

func (ws *failDeleteWorkspace) Delete(ctx context.Context, name string) error {
	ws.mu.Lock()
	if name == ws.target && !ws.failed {
		ws.failed = true
		ws.mu.Unlock()
		return errors.New("injected delete failure")
	}
	ws.mu.Unlock()
	return ws.Workspace.Delete(ctx, name)
}

func (ws *failRenameWorkspace) Rename(ctx context.Context, source, destination string) error {
	ws.mu.Lock()
	if destination == ws.destination && !ws.failed {
		ws.failed = true
		ws.mu.Unlock()
		return errors.New("injected rename failure")
	}
	ws.mu.Unlock()
	return ws.Workspace.Rename(ctx, source, destination)
}

func newCountingWorkspace(ws workspace.Workspace) *countingWorkspace {
	return &countingWorkspace{
		Workspace: ws,
		renames:   make(map[string]int),
		reads:     make(map[string]int),
		lists:     make(map[string]int),
	}
}

func (ws *countingWorkspace) Read(ctx context.Context, name string) ([]byte, error) {
	ws.mu.Lock()
	ws.reads[name]++
	ws.mu.Unlock()
	return ws.Workspace.Read(ctx, name)
}

func (ws *countingWorkspace) List(ctx context.Context, name string) ([]fs.DirEntry, error) {
	ws.mu.Lock()
	ws.lists[name]++
	ws.mu.Unlock()
	return ws.Workspace.List(ctx, name)
}

func (ws *countingWorkspace) Rename(ctx context.Context, source, destination string) error {
	if err := ws.Workspace.Rename(ctx, source, destination); err != nil {
		return err
	}
	ws.mu.Lock()
	ws.renames[destination]++
	ws.mu.Unlock()
	return nil
}

func (ws *countingWorkspace) renameCount(path string) int {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	return ws.renames[path]
}

func (ws *countingWorkspace) resetCounts() {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	ws.reads = make(map[string]int)
	ws.lists = make(map[string]int)
}

func (ws *countingWorkspace) totalReads() int {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	total := 0
	for _, count := range ws.reads {
		total += count
	}
	return total
}

func (ws *countingWorkspace) totalLists() int {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	total := 0
	for _, count := range ws.lists {
		total += count
	}
	return total
}
