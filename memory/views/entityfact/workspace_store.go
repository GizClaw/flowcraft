package entityfact

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GizClaw/flowcraft/memory/views"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

const defaultPathSegmentPrefix = "ef_"

type WorkspaceStore struct {
	ws                workspace.Workspace
	pathSegmentPrefix string
	tmpCounter        atomic.Uint64
	mu                sync.RWMutex
}

var _ Store = (*WorkspaceStore)(nil)

type WorkspaceStoreOption interface {
	applyEntityFactWorkspaceStore(*WorkspaceStore)
}

type pathSegmentPrefixOption string

func WithPathSegmentPrefix(prefix string) WorkspaceStoreOption {
	return pathSegmentPrefixOption(prefix)
}

func (o pathSegmentPrefixOption) applyEntityFactWorkspaceStore(s *WorkspaceStore) {
	if o != "" {
		s.pathSegmentPrefix = string(o)
	}
}

func NewWorkspaceStore(ws workspace.Workspace, opts ...WorkspaceStoreOption) *WorkspaceStore {
	s := &WorkspaceStore{
		ws:                ws,
		pathSegmentPrefix: defaultPathSegmentPrefix,
	}
	for _, opt := range opts {
		if opt != nil {
			opt.applyEntityFactWorkspaceStore(s)
		}
	}
	return s
}

func (s *WorkspaceStore) PutEntity(ctx context.Context, entity Entity) (Entity, error) {
	if s.ws == nil {
		return Entity{}, errdefs.Validationf("%s: workspace is required", errPrefix)
	}
	if err := ValidateEntity(entity); err != nil {
		return Entity{}, err
	}
	entity = CloneEntity(entity)
	now := time.Now().UTC()
	if entity.CreatedAt.IsZero() {
		entity.CreatedAt = now
	}
	if entity.UpdatedAt.IsZero() {
		entity.UpdatedAt = now
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.writeJSON(ctx, s.entityPath(entity.Scope, entity.ID), entity); err != nil {
		return Entity{}, err
	}
	if err := s.addAliasEntries(ctx, entity); err != nil {
		return Entity{}, err
	}
	return CloneEntity(entity), nil
}

func (s *WorkspaceStore) PutFact(ctx context.Context, fact Fact) (Fact, error) {
	if s.ws == nil {
		return Fact{}, errdefs.Validationf("%s: workspace is required", errPrefix)
	}
	if err := ValidateFact(fact); err != nil {
		return Fact{}, err
	}
	fact = CloneFact(fact)
	now := time.Now().UTC()
	if fact.CreatedAt.IsZero() {
		fact.CreatedAt = now
	}
	if fact.UpdatedAt.IsZero() {
		fact.UpdatedAt = now
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.writeJSON(ctx, s.factPath(fact.Scope, fact.ID), fact); err != nil {
		return Fact{}, err
	}
	return CloneFact(fact), nil
}

func (s *WorkspaceStore) GetEntity(ctx context.Context, scope views.Scope, id EntityID) (Entity, bool, error) {
	if s.ws == nil {
		return Entity{}, false, errdefs.Validationf("%s: workspace is required", errPrefix)
	}
	if scope.ConversationID == "" || id == "" {
		return Entity{}, false, nil
	}
	if err := validateScope(scope); err != nil {
		return Entity{}, false, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	entity, ok, err := s.readEntity(ctx, scope, id)
	if err != nil || !ok {
		return Entity{}, ok, err
	}
	return CloneEntity(entity), true, nil
}

func (s *WorkspaceStore) GetFact(ctx context.Context, scope views.Scope, id FactID) (Fact, bool, error) {
	if s.ws == nil {
		return Fact{}, false, errdefs.Validationf("%s: workspace is required", errPrefix)
	}
	if scope.ConversationID == "" || id == "" {
		return Fact{}, false, nil
	}
	if err := validateScope(scope); err != nil {
		return Fact{}, false, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	fact, ok, err := s.readFact(ctx, scope, id)
	if err != nil || !ok {
		return Fact{}, ok, err
	}
	return CloneFact(fact), true, nil
}

func (s *WorkspaceStore) ListEntities(ctx context.Context, scope views.Scope, opts ListOptions) ([]Entity, error) {
	if s.ws == nil {
		return nil, errdefs.Validationf("%s: workspace is required", errPrefix)
	}
	if scope.ConversationID == "" {
		return nil, nil
	}
	if err := validateScope(scope); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	ids, err := s.listIDs(ctx, s.entitiesDir(scope), opts.AfterID)
	if err != nil {
		return nil, err
	}
	out := make([]Entity, 0, len(ids))
	for _, id := range ids {
		entity, ok, err := s.readEntity(ctx, scope, EntityID(id))
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		out = append(out, CloneEntity(entity))
		if opts.Limit > 0 && len(out) >= opts.Limit {
			break
		}
	}
	return out, nil
}

func (s *WorkspaceStore) ListFacts(ctx context.Context, scope views.Scope, opts ListOptions) ([]Fact, error) {
	if s.ws == nil {
		return nil, errdefs.Validationf("%s: workspace is required", errPrefix)
	}
	if scope.ConversationID == "" {
		return nil, nil
	}
	if err := validateScope(scope); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	ids, err := s.listIDs(ctx, s.factsDir(scope), opts.AfterID)
	if err != nil {
		return nil, err
	}
	out := make([]Fact, 0, len(ids))
	for _, id := range ids {
		fact, ok, err := s.readFact(ctx, scope, FactID(id))
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		out = append(out, CloneFact(fact))
		if opts.Limit > 0 && len(out) >= opts.Limit {
			break
		}
	}
	return out, nil
}

func (s *WorkspaceStore) LookupAlias(ctx context.Context, scope views.Scope, alias string) ([]EntityID, error) {
	if s.ws == nil {
		return nil, errdefs.Validationf("%s: workspace is required", errPrefix)
	}
	key := NormalizeAlias(alias)
	if scope.ConversationID == "" || key == "" {
		return nil, nil
	}
	if err := validateScope(scope); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	ids, err := s.readAliasIDs(ctx, scope, key)
	if err != nil {
		return nil, err
	}
	return append([]EntityID(nil), ids...), nil
}

func (s *WorkspaceStore) DeleteScope(ctx context.Context, scope views.Scope) error {
	if s.ws == nil {
		return errdefs.Validationf("%s: workspace is required", errPrefix)
	}
	if scope.ConversationID == "" {
		return nil
	}
	if err := validateScope(scope); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.ws.RemoveAll(ctx, s.conversationDir(scope)); err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("%s: delete scope %q/%q/%q/%q: %w", errPrefix, scope.RuntimeID, scope.UserID, scope.AgentID, scope.ConversationID, err)
	}
	return nil
}

func (s *WorkspaceStore) readEntity(ctx context.Context, scope views.Scope, id EntityID) (Entity, bool, error) {
	var entity Entity
	ok, err := s.readJSON(ctx, s.entityPath(scope, id), &entity)
	if err != nil || !ok || !sameScope(entity.Scope, scope) {
		return Entity{}, ok && err == nil, err
	}
	return entity, true, nil
}

func (s *WorkspaceStore) readFact(ctx context.Context, scope views.Scope, id FactID) (Fact, bool, error) {
	var fact Fact
	ok, err := s.readJSON(ctx, s.factPath(scope, id), &fact)
	if err != nil || !ok || !sameScope(fact.Scope, scope) {
		return Fact{}, ok && err == nil, err
	}
	return fact, true, nil
}

func (s *WorkspaceStore) readJSON(ctx context.Context, file string, out any) (bool, error) {
	data, err := s.ws.Read(ctx, file)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	if err := json.Unmarshal(data, out); err != nil {
		return false, err
	}
	return true, nil
}

func (s *WorkspaceStore) writeJSON(ctx context.Context, livePath string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	tmpPath := s.tmpPath(livePath)
	if err := s.ws.Write(ctx, tmpPath, data); err != nil {
		return err
	}
	if err := s.ws.Rename(ctx, tmpPath, livePath); err != nil {
		_ = s.ws.Delete(ctx, tmpPath)
		return err
	}
	return nil
}

func (s *WorkspaceStore) addAliasEntries(ctx context.Context, entity Entity) error {
	for _, key := range entity.AliasKeys() {
		ids, err := s.readAliasIDs(ctx, entity.Scope, key)
		if err != nil {
			return err
		}
		if !containsEntityID(ids, entity.ID) {
			ids = append(ids, entity.ID)
			sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		}
		if err := s.writeJSON(ctx, s.aliasPath(entity.Scope, key), ids); err != nil {
			return err
		}
	}
	return nil
}

func (s *WorkspaceStore) readAliasIDs(ctx context.Context, scope views.Scope, key string) ([]EntityID, error) {
	var ids []EntityID
	ok, err := s.readJSON(ctx, s.aliasPath(scope, key), &ids)
	if err != nil || !ok {
		return nil, err
	}
	return ids, nil
}

func (s *WorkspaceStore) listIDs(ctx context.Context, dir, afterID string) ([]string, error) {
	entries, err := s.ws.List(ctx, dir)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		segment := strings.TrimSuffix(entry.Name(), ".json")
		if !strings.HasPrefix(segment, s.pathSegmentPrefix) {
			continue
		}
		id, err := s.rawPathSegment(segment)
		if err != nil {
			return nil, err
		}
		if id > afterID {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

func validateScope(scope views.Scope) error {
	if err := scope.Validate(); err != nil {
		return errdefs.Validationf("%s: invalid scope: %w", errPrefix, err)
	}
	return nil
}

func (s *WorkspaceStore) runtimeDir(scope views.Scope) string {
	return path.Join("runtimes", s.pathSegment(scope.RuntimeID))
}

func (s *WorkspaceStore) userDir(scope views.Scope) string {
	return path.Join(s.runtimeDir(scope), "users", s.pathSegment(scope.UserID))
}

func (s *WorkspaceStore) agentDir(scope views.Scope) string {
	return path.Join(s.userDir(scope), "agents", s.pathSegment(scope.AgentID))
}

func (s *WorkspaceStore) conversationDir(scope views.Scope) string {
	return path.Join(s.agentDir(scope), "conversations", s.pathSegment(scope.ConversationID))
}

func (s *WorkspaceStore) entitiesDir(scope views.Scope) string {
	return path.Join(s.conversationDir(scope), "entities")
}

func (s *WorkspaceStore) factsDir(scope views.Scope) string {
	return path.Join(s.conversationDir(scope), "facts")
}

func (s *WorkspaceStore) aliasesDir(scope views.Scope) string {
	return path.Join(s.conversationDir(scope), "aliases")
}

func (s *WorkspaceStore) entityPath(scope views.Scope, id EntityID) string {
	return path.Join(s.entitiesDir(scope), s.pathSegment(string(id))+".json")
}

func (s *WorkspaceStore) factPath(scope views.Scope, id FactID) string {
	return path.Join(s.factsDir(scope), s.pathSegment(string(id))+".json")
}

func (s *WorkspaceStore) aliasPath(scope views.Scope, key string) string {
	return path.Join(s.aliasesDir(scope), s.pathSegment(key)+".json")
}

func (s *WorkspaceStore) tmpPath(livePath string) string {
	return fmt.Sprintf("%s.tmp.%d.%d.%d", livePath, os.Getpid(), time.Now().UnixNano(), s.tmpCounter.Add(1))
}

func (s *WorkspaceStore) pathSegment(id string) string {
	return s.pathSegmentPrefix + base64.RawURLEncoding.EncodeToString([]byte(id))
}

func (s *WorkspaceStore) rawPathSegment(segment string) (string, error) {
	if !strings.HasPrefix(segment, s.pathSegmentPrefix) {
		return "", fmt.Errorf("missing %q prefix", s.pathSegmentPrefix)
	}
	data, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(segment, s.pathSegmentPrefix))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func sameScope(left, right views.Scope) bool {
	return left.RuntimeID == right.RuntimeID &&
		left.UserID == right.UserID &&
		left.AgentID == right.AgentID &&
		left.ConversationID == right.ConversationID
}

func containsEntityID(ids []EntityID, id EntityID) bool {
	for _, existing := range ids {
		if existing == id {
			return true
		}
	}
	return false
}
