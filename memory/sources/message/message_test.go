package message

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/storage"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	sdkmessage "github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

var (
	messageScopeA = sdkmemory.Scope{RuntimeID: "runtime", UserID: "alice", AgentID: "agent-a"}
	messageScopeB = sdkmemory.Scope{RuntimeID: "runtime", UserID: "bob", AgentID: "agent-b"}
)

func TestMessageStoreAppendGetListAndRetry(t *testing.T) {
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

func TestMessageStoreCommitIdentityAndListingAreStable(t *testing.T) {
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

func TestMessageStoreHardPartitionAndConversationIsolation(t *testing.T) {
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

func TestMessageStoreAgentPartitionIsolation(t *testing.T) {
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

func TestMessageStorePersistsAndPaginatesStably(t *testing.T) {
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

func TestMessageStoreCommitOrderComesFromSeqNotIdempotencyKey(t *testing.T) {
	ctx := context.Background()
	store := newMessageStore(t, workspace.NewMemWorkspace())
	for _, turn := range []struct{ key, text string }{
		{"z-last-key", "first turn"},
		{"a-first-key", "second turn"},
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

func TestMessageStoreConcurrentAppend(t *testing.T) {
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

func TestMessageStoreOwnsInputsAndResults(t *testing.T) {
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

func TestMessageStoreEncodesMaliciousIDs(t *testing.T) {
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
	stream, err := storage.StreamName(scope, conversation)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stream, conversation) || strings.Contains(stream, "..") ||
		strings.HasPrefix(stream, "/") {
		t.Fatalf("unsafe stream name %q", stream)
	}
	commits, err := store.ListCommits(ctx, scope, conversation, ListCommitOptions{})
	if err != nil || len(commits) != 1 || commits[0].Records[0].Message.Content.Text() != "safe" {
		t.Fatalf("ListCommits = %#v, %v", commits, err)
	}
}

func TestMessageStoreValidationAndNilDependencies(t *testing.T) {
	logStore, err := storage.NewWorkspaceLog(workspace.NewMemWorkspace())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewMessageStore(nil); err == nil {
		t.Fatal("nil log accepted")
	}
	if _, err := NewMessageStore(nonCommitLog{Log: logStore}); err == nil {
		t.Fatal("log without commit metadata accepted")
	}
	store, err := NewMessageStore(logStore)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Append(context.Background(), AppendRequest{
		Scope: messageScopeA, ConversationID: "conv", IdempotencyKey: "key",
		Messages: []sdkmessage.Message{{}},
	})
	if err == nil {
		t.Fatal("invalid canonical message accepted")
	}
	if _, err := store.Append(context.Background(), AppendRequest{
		Scope: messageScopeA, ConversationID: "conv",
		Messages: []sdkmessage.Message{sdkmessage.NewTextMessage(sdkmessage.RoleUser, "x")},
	}); err == nil {
		t.Fatal("empty idempotency key accepted")
	}
}

func TestMessageStoreConversationOrderingIsSorted(t *testing.T) {
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

func newMessageStore(t *testing.T, ws workspace.Workspace, options ...Option) *MessageStore {
	t.Helper()
	logStore, err := storage.NewWorkspaceLog(ws)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewMessageStore(logStore, options...)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

type nonCommitLog struct {
	storage.Log
}
