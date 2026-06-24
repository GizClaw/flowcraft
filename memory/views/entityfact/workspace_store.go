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

const (
	factIndexEntities  = "entities"
	factIndexSubjects  = "subjects"
	factIndexObjects   = "objects"
	factIndexRelations = "relations"
	factIndexTimes     = "times"
	factIndexSources   = "sources"
)

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

	oldFact, oldOK, err := s.readFact(ctx, fact.Scope, fact.ID)
	if err != nil {
		return Fact{}, err
	}
	if oldOK {
		if err := s.removeFactIndexEntries(ctx, oldFact); err != nil {
			return Fact{}, err
		}
	}
	if err := s.writeJSON(ctx, s.factPath(fact.Scope, fact.ID), fact); err != nil {
		return Fact{}, err
	}
	if err := s.addFactIndexEntries(ctx, fact); err != nil {
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

	return s.listFactsUnlocked(ctx, scope, opts)
}

func (s *WorkspaceStore) ListFactsByEntity(ctx context.Context, scope views.Scope, id EntityID, opts ListOptions) ([]Fact, error) {
	return s.listFactsByIndexedIDs(ctx, scope, s.factEntityIndexPath(scope, id), opts, func(fact Fact) bool {
		return factHasEntity(fact, id)
	})
}

func (s *WorkspaceStore) ListFactsBySubject(ctx context.Context, scope views.Scope, id EntityID, opts ListOptions) ([]Fact, error) {
	return s.listFactsByIndexedIDs(ctx, scope, s.factSubjectIndexPath(scope, id), opts, func(fact Fact) bool {
		return fact.SubjectEntityID == id
	})
}

func (s *WorkspaceStore) ListFactsByObject(ctx context.Context, scope views.Scope, id EntityID, opts ListOptions) ([]Fact, error) {
	return s.listFactsByIndexedIDs(ctx, scope, s.factObjectIndexPath(scope, id), opts, func(fact Fact) bool {
		return factHasObject(fact, id)
	})
}

func (s *WorkspaceStore) ListFactsByRelation(ctx context.Context, scope views.Scope, relation RelationType, opts ListOptions) ([]Fact, error) {
	return s.listFactsByIndexedIDs(ctx, scope, s.factRelationIndexPath(scope, relation), opts, func(fact Fact) bool {
		return fact.RelationType == relation
	})
}

func (s *WorkspaceStore) ListFactsByTime(ctx context.Context, scope views.Scope, timeText string, opts ListOptions) ([]Fact, error) {
	timeKey := NormalizeTimeKey(timeText)
	if timeKey == "" {
		return nil, nil
	}
	return s.listFactsByIndexedIDs(ctx, scope, s.factTimeIndexPath(scope, timeKey), opts, func(fact Fact) bool {
		return NormalizeTimeKey(fact.TimeText) == timeKey
	})
}

func (s *WorkspaceStore) ListFactsBySourceMessage(ctx context.Context, scope views.Scope, conversationID, messageID string, opts ListOptions) ([]Fact, error) {
	if conversationID == "" {
		conversationID = scope.ConversationID
	}
	if messageID == "" {
		return nil, nil
	}
	return s.listFactsByIndexedIDs(ctx, scope, s.factSourceIndexPath(scope, conversationID, messageID), opts, func(fact Fact) bool {
		return factHasSourceMessage(fact, conversationID, messageID)
	})
}

func (s *WorkspaceStore) listFactsUnlocked(ctx context.Context, scope views.Scope, opts ListOptions) ([]Fact, error) {
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

func (s *WorkspaceStore) listFactsByIndexedIDs(ctx context.Context, scope views.Scope, indexPath string, opts ListOptions, match func(Fact) bool) ([]Fact, error) {
	if s.ws == nil {
		return nil, errdefs.Validationf("%s: workspace is required", errPrefix)
	}
	if scope.ConversationID == "" || indexPath == "" {
		return nil, nil
	}
	if err := validateScope(scope); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	ids, ok, err := s.readFactIndexIDs(ctx, indexPath)
	if err != nil {
		return nil, err
	}
	if !ok {
		facts, err := s.listFactsUnlocked(ctx, scope, opts)
		if err != nil {
			return nil, err
		}
		return filterFacts(facts, opts, match), nil
	}
	out := make([]Fact, 0, len(ids))
	for _, id := range ids {
		if opts.AfterID != "" && string(id) <= opts.AfterID {
			continue
		}
		fact, ok, err := s.readFact(ctx, scope, id)
		if err != nil {
			return nil, err
		}
		if !ok || !match(fact) {
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

func (s *WorkspaceStore) addFactIndexEntries(ctx context.Context, fact Fact) error {
	for _, entry := range s.factIndexEntries(fact) {
		if err := s.addFactIndexID(ctx, entry, fact.ID); err != nil {
			return err
		}
	}
	return nil
}

func (s *WorkspaceStore) removeFactIndexEntries(ctx context.Context, fact Fact) error {
	for _, entry := range s.factIndexEntries(fact) {
		if err := s.removeFactIndexID(ctx, entry, fact.ID); err != nil {
			return err
		}
	}
	return nil
}

func (s *WorkspaceStore) factIndexEntries(fact Fact) []string {
	seen := map[string]bool{}
	add := func(out *[]string, p string) {
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		*out = append(*out, p)
	}
	var out []string
	add(&out, s.factEntityIndexPath(fact.Scope, fact.SubjectEntityID))
	add(&out, s.factSubjectIndexPath(fact.Scope, fact.SubjectEntityID))
	for _, id := range fact.ObjectEntityIDs {
		add(&out, s.factEntityIndexPath(fact.Scope, id))
		add(&out, s.factObjectIndexPath(fact.Scope, id))
	}
	add(&out, s.factRelationIndexPath(fact.Scope, fact.RelationType))
	if timeKey := NormalizeTimeKey(fact.TimeText); timeKey != "" {
		add(&out, s.factTimeIndexPath(fact.Scope, timeKey))
	}
	for _, ref := range fact.SourceRefs {
		if ref.Kind != views.SourceMessage || ref.Message == nil || ref.Message.MessageID == "" {
			continue
		}
		conversationID := ref.Message.ConversationID
		if conversationID == "" {
			conversationID = fact.Scope.ConversationID
		}
		add(&out, s.factSourceIndexPath(fact.Scope, conversationID, ref.Message.MessageID))
	}
	return out
}

func (s *WorkspaceStore) addFactIndexID(ctx context.Context, indexPath string, id FactID) error {
	ids, _, err := s.readFactIndexIDs(ctx, indexPath)
	if err != nil {
		return err
	}
	if !containsFactID(ids, id) {
		ids = append(ids, id)
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	}
	return s.writeJSON(ctx, indexPath, ids)
}

func (s *WorkspaceStore) removeFactIndexID(ctx context.Context, indexPath string, id FactID) error {
	ids, ok, err := s.readFactIndexIDs(ctx, indexPath)
	if err != nil || !ok {
		return err
	}
	out := ids[:0]
	for _, existing := range ids {
		if existing != id {
			out = append(out, existing)
		}
	}
	if len(out) == 0 {
		if err := s.ws.Delete(ctx, indexPath); err != nil && !errdefs.IsNotFound(err) {
			return err
		}
		return nil
	}
	return s.writeJSON(ctx, indexPath, out)
}

func (s *WorkspaceStore) readFactIndexIDs(ctx context.Context, indexPath string) ([]FactID, bool, error) {
	var ids []FactID
	ok, err := s.readJSON(ctx, indexPath, &ids)
	if err != nil || !ok {
		return nil, ok, err
	}
	return ids, true, nil
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

func (s *WorkspaceStore) factIndexDir(scope views.Scope) string {
	return path.Join(s.conversationDir(scope), "fact_index")
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

func (s *WorkspaceStore) factEntityIndexPath(scope views.Scope, id EntityID) string {
	if id == "" {
		return ""
	}
	return path.Join(s.factIndexDir(scope), factIndexEntities, s.pathSegment(string(id))+".json")
}

func (s *WorkspaceStore) factSubjectIndexPath(scope views.Scope, id EntityID) string {
	if id == "" {
		return ""
	}
	return path.Join(s.factIndexDir(scope), factIndexSubjects, s.pathSegment(string(id))+".json")
}

func (s *WorkspaceStore) factObjectIndexPath(scope views.Scope, id EntityID) string {
	if id == "" {
		return ""
	}
	return path.Join(s.factIndexDir(scope), factIndexObjects, s.pathSegment(string(id))+".json")
}

func (s *WorkspaceStore) factRelationIndexPath(scope views.Scope, relation RelationType) string {
	if relation == "" {
		return ""
	}
	return path.Join(s.factIndexDir(scope), factIndexRelations, s.pathSegment(string(relation))+".json")
}

func (s *WorkspaceStore) factTimeIndexPath(scope views.Scope, timeKey string) string {
	if timeKey == "" {
		return ""
	}
	return path.Join(s.factIndexDir(scope), factIndexTimes, s.pathSegment(timeKey)+".json")
}

func (s *WorkspaceStore) factSourceIndexPath(scope views.Scope, conversationID, messageID string) string {
	if conversationID == "" || messageID == "" {
		return ""
	}
	return path.Join(s.factIndexDir(scope), factIndexSources, s.pathSegment(conversationID), s.pathSegment(messageID)+".json")
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

func containsFactID(ids []FactID, id FactID) bool {
	for _, existing := range ids {
		if existing == id {
			return true
		}
	}
	return false
}

func factHasEntity(fact Fact, id EntityID) bool {
	return fact.SubjectEntityID == id || factHasObject(fact, id)
}

func factHasObject(fact Fact, id EntityID) bool {
	for _, objectID := range fact.ObjectEntityIDs {
		if objectID == id {
			return true
		}
	}
	return false
}

func factHasSourceMessage(fact Fact, conversationID, messageID string) bool {
	for _, ref := range fact.SourceRefs {
		if ref.Kind != views.SourceMessage || ref.Message == nil {
			continue
		}
		refConversationID := ref.Message.ConversationID
		if refConversationID == "" {
			refConversationID = fact.Scope.ConversationID
		}
		if refConversationID == conversationID && ref.Message.MessageID == messageID {
			return true
		}
	}
	return false
}

func filterFacts(facts []Fact, opts ListOptions, match func(Fact) bool) []Fact {
	out := make([]Fact, 0, len(facts))
	for _, fact := range facts {
		if opts.AfterID != "" && string(fact.ID) <= opts.AfterID {
			continue
		}
		if !match(fact) {
			continue
		}
		out = append(out, CloneFact(fact))
		if opts.Limit > 0 && len(out) >= opts.Limit {
			break
		}
	}
	return out
}
