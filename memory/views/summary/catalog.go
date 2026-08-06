package summary

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"reflect"
	"strings"
	"time"

	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

const activeCatalogSchemaVersion = 1

type activeCatalogHead struct {
	SchemaVersion     int           `json:"schema_version"`
	RuntimeID         string        `json:"runtime_id"`
	UserID            string        `json:"user_id"`
	AgentID           string        `json:"agent_id,omitempty"`
	ConversationID    string        `json:"conversation_id"`
	BaseID            string        `json:"base_id"`
	BaseDigest        string        `json:"base_digest"`
	HeadSegmentID     string        `json:"head_segment_id,omitempty"`
	HeadSegmentDigest string        `json:"head_segment_digest,omitempty"`
	SegmentCount      int           `json:"segment_count"`
	ActiveDigest      string        `json:"active_digest"`
	GenerationID      string        `json:"generation_id"`
	CoverageRange     CoverageRange `json:"coverage_range"`
	FrontierDigest    string        `json:"frontier_digest"`
	PublishedAtText   string        `json:"published_at"`
}

type activeCatalogBase struct {
	SchemaVersion  int      `json:"schema_version"`
	RuntimeID      string   `json:"runtime_id"`
	UserID         string   `json:"user_id"`
	AgentID        string   `json:"agent_id,omitempty"`
	ConversationID string   `json:"conversation_id"`
	BaseID         string   `json:"base_id"`
	RecordIDs      []string `json:"record_ids"`
	Digest         string   `json:"digest"`
}

type activeCatalogAdd struct {
	ID    string `json:"id"`
	Index int    `json:"index"`
}

type activeCatalogSegment struct {
	SchemaVersion         int                `json:"schema_version"`
	RuntimeID             string             `json:"runtime_id"`
	UserID                string             `json:"user_id"`
	AgentID               string             `json:"agent_id,omitempty"`
	ConversationID        string             `json:"conversation_id"`
	SegmentID             string             `json:"segment_id"`
	BaseID                string             `json:"base_id"`
	BaseDigest            string             `json:"base_digest"`
	PreviousSegmentID     string             `json:"previous_segment_id,omitempty"`
	PreviousSegmentDigest string             `json:"previous_segment_digest,omitempty"`
	Adds                  []activeCatalogAdd `json:"adds,omitempty"`
	RemoveIDs             []string           `json:"remove_ids,omitempty"`
	GenerationID          string             `json:"generation_id"`
	CoverageRange         CoverageRange      `json:"coverage_range"`
	FrontierDigest        string             `json:"frontier_digest"`
	PublishedAtText       string             `json:"published_at"`
	Digest                string             `json:"digest"`
}

type activeCatalogCache struct {
	headDigest string
	manifest   Manifest
}

func (store *WorkspaceStore) publishActiveLocked(ctx context.Context, desired Manifest) error {
	previous, head, found, err := store.materializeActiveLocked(ctx, desired.Scope, desired.ConversationID)
	if err != nil {
		return err
	}
	if found && sameActivePublication(previous, desired) {
		return nil
	}
	if desired.PublishedAt.IsZero() {
		desired.PublishedAt = store.clock()
	}
	desired.PublishedAt = desired.PublishedAt.UTC()
	if err := desired.Validate(); err != nil {
		return err
	}
	adds, removes := activeDelta(previous.RecordIDs, desired.RecordIDs)
	for _, add := range adds {
		record, ok, readErr := store.read(ctx, desired.Scope, desired.ConversationID, add.ID)
		if readErr != nil {
			return readErr
		}
		if !ok {
			return fmt.Errorf("summary view: active record %q not found", add.ID)
		}
		if record.Scope != desired.Scope || record.ConversationID != desired.ConversationID {
			return fmt.Errorf("summary view: active record %q has wrong address", add.ID)
		}
	}

	if !found || head.SegmentCount >= store.activeCompactionThreshold {
		return store.writeActiveBase(ctx, desired)
	}
	segment := activeCatalogSegment{
		SchemaVersion: activeCatalogSchemaVersion,
		RuntimeID:     desired.Scope.RuntimeID, UserID: desired.Scope.UserID, AgentID: desired.Scope.AgentID,
		ConversationID: desired.ConversationID, BaseID: head.BaseID, BaseDigest: head.BaseDigest,
		PreviousSegmentID: head.HeadSegmentID, PreviousSegmentDigest: head.HeadSegmentDigest,
		Adds: adds, RemoveIDs: removes, GenerationID: desired.GenerationID,
		CoverageRange: desired.CoverageRange, FrontierDigest: desired.FrontierDigest,
		PublishedAtText: desired.PublishedAt.UTC().Format(timeLayout),
	}
	segment.Digest = digestCatalog(segment)
	segment.SegmentID = segment.Digest
	segment.Digest = ""
	segment.Digest = digestCatalog(segment)
	data, err := json.Marshal(segment)
	if err != nil {
		return fmt.Errorf("summary view: encode active segment: %w", err)
	}
	if err := workspace.AtomicWrite(ctx, store.ws, store.activeSegmentPath(desired.Scope, desired.ConversationID, segment.SegmentID), data); err != nil {
		return fmt.Errorf("summary view: write active segment: %w", err)
	}
	nextHead := activeCatalogHead{
		SchemaVersion: activeCatalogSchemaVersion,
		RuntimeID:     desired.Scope.RuntimeID, UserID: desired.Scope.UserID, AgentID: desired.Scope.AgentID,
		ConversationID: desired.ConversationID, BaseID: head.BaseID, BaseDigest: head.BaseDigest,
		HeadSegmentID: segment.SegmentID, HeadSegmentDigest: segment.Digest, SegmentCount: head.SegmentCount + 1,
		ActiveDigest: digestManifest(desired), GenerationID: desired.GenerationID,
		CoverageRange: desired.CoverageRange, FrontierDigest: desired.FrontierDigest,
		PublishedAtText: desired.PublishedAt.UTC().Format(timeLayout),
	}
	return store.writeActiveHead(ctx, nextHead, desired)
}

const timeLayout = "2006-01-02T15:04:05.999999999Z07:00"

func (store *WorkspaceStore) writeActiveBase(ctx context.Context, manifest Manifest) error {
	base := activeCatalogBase{
		SchemaVersion: activeCatalogSchemaVersion,
		RuntimeID:     manifest.Scope.RuntimeID, UserID: manifest.Scope.UserID, AgentID: manifest.Scope.AgentID,
		ConversationID: manifest.ConversationID, RecordIDs: append([]string(nil), manifest.RecordIDs...),
	}
	base.Digest = digestCatalog(base)
	base.BaseID = base.Digest
	base.Digest = ""
	base.Digest = digestCatalog(base)
	data, err := json.Marshal(base)
	if err != nil {
		return fmt.Errorf("summary view: encode active base: %w", err)
	}
	if err := workspace.AtomicWrite(ctx, store.ws, store.activeBasePath(manifest.Scope, manifest.ConversationID, base.BaseID), data); err != nil {
		return fmt.Errorf("summary view: write active base: %w", err)
	}
	head := activeCatalogHead{
		SchemaVersion: activeCatalogSchemaVersion,
		RuntimeID:     manifest.Scope.RuntimeID, UserID: manifest.Scope.UserID, AgentID: manifest.Scope.AgentID,
		ConversationID: manifest.ConversationID, BaseID: base.BaseID, BaseDigest: base.Digest,
		ActiveDigest: digestManifest(manifest), GenerationID: manifest.GenerationID,
		CoverageRange: manifest.CoverageRange, FrontierDigest: manifest.FrontierDigest,
		PublishedAtText: manifest.PublishedAt.UTC().Format(timeLayout),
	}
	return store.writeActiveHead(ctx, head, manifest)
}

func (store *WorkspaceStore) writeActiveHead(ctx context.Context, head activeCatalogHead, manifest Manifest) error {
	data, err := json.Marshal(head)
	if err != nil {
		return fmt.Errorf("summary view: encode active manifest: %w", err)
	}
	if err := workspace.AtomicWrite(ctx, store.ws, store.manifestPath(manifest.Scope, manifest.ConversationID), data); err != nil {
		return fmt.Errorf("summary view: publish active manifest: %w", err)
	}
	store.cacheActive(store.catalogKey(manifest.Scope, manifest.ConversationID), digestBytes(data), manifest)
	return nil
}

func (store *WorkspaceStore) materializeActiveLocked(
	ctx context.Context,
	scope sdkmemory.Scope,
	conversationID string,
) (Manifest, activeCatalogHead, bool, error) {
	data, err := store.ws.Read(ctx, store.manifestPath(scope, conversationID))
	if err != nil {
		if errors.Is(err, workspace.ErrNotFound) {
			return Manifest{}, activeCatalogHead{}, false, nil
		}
		return Manifest{}, activeCatalogHead{}, false, fmt.Errorf("summary view: read active manifest: %w", err)
	}
	headDigest := digestBytes(data)
	if cached, ok := store.activeCache[store.catalogKey(scope, conversationID)]; ok && cached.headDigest == headDigest {
		return cached.manifest.Clone(), activeCatalogHeadFromManifestData(data), true, nil
	}
	var head activeCatalogHead
	if err := decodeStrict(data, &head); err != nil {
		return Manifest{}, activeCatalogHead{}, false, fmt.Errorf("summary view: decode active manifest: %w", err)
	}
	if err := validateActiveHead(head, scope, conversationID); err != nil {
		return Manifest{}, activeCatalogHead{}, false, err
	}
	baseData, err := store.ws.Read(ctx, store.activeBasePath(scope, conversationID, head.BaseID))
	if err != nil {
		return Manifest{}, activeCatalogHead{}, false, fmt.Errorf("summary view: read active base: %w", err)
	}
	var base activeCatalogBase
	if err := decodeStrict(baseData, &base); err != nil {
		return Manifest{}, activeCatalogHead{}, false, fmt.Errorf("summary view: decode active base: %w", err)
	}
	if err := validateActiveBase(base, scope, conversationID, head); err != nil {
		return Manifest{}, activeCatalogHead{}, false, err
	}
	segments := make([]activeCatalogSegment, 0, head.SegmentCount)
	nextID, nextDigest := head.HeadSegmentID, head.HeadSegmentDigest
	for len(segments) < head.SegmentCount {
		if nextID == "" || nextDigest == "" {
			return Manifest{}, activeCatalogHead{}, false, errors.New("summary view: active segment chain ended early")
		}
		segmentData, readErr := store.ws.Read(ctx, store.activeSegmentPath(scope, conversationID, nextID))
		if readErr != nil {
			return Manifest{}, activeCatalogHead{}, false, fmt.Errorf("summary view: read active segment: %w", readErr)
		}
		var segment activeCatalogSegment
		if err := decodeStrict(segmentData, &segment); err != nil {
			return Manifest{}, activeCatalogHead{}, false, fmt.Errorf("summary view: decode active segment: %w", err)
		}
		if err := validateActiveSegment(segment, scope, conversationID, head, nextID, nextDigest); err != nil {
			return Manifest{}, activeCatalogHead{}, false, err
		}
		segments = append(segments, segment)
		nextID, nextDigest = segment.PreviousSegmentID, segment.PreviousSegmentDigest
	}
	if nextID != "" || nextDigest != "" {
		return Manifest{}, activeCatalogHead{}, false, errors.New("summary view: active segment chain exceeds declared count")
	}
	ids := append([]string(nil), base.RecordIDs...)
	for index := len(segments) - 1; index >= 0; index-- {
		ids, err = applyActiveDelta(ids, segments[index].Adds, segments[index].RemoveIDs)
		if err != nil {
			return Manifest{}, activeCatalogHead{}, false, err
		}
	}
	publishedAt, err := time.Parse(timeLayout, head.PublishedAtText)
	if err != nil {
		return Manifest{}, activeCatalogHead{}, false, errors.New("summary view: corrupt active manifest published_at")
	}
	manifest := Manifest{
		Scope: scope, ConversationID: conversationID, GenerationID: head.GenerationID,
		RecordIDs: ids, CoverageRange: head.CoverageRange, FrontierDigest: head.FrontierDigest,
		PublishedAt: publishedAt,
	}
	if err := manifest.Validate(); err != nil || digestManifest(manifest) != head.ActiveDigest {
		return Manifest{}, activeCatalogHead{}, false, errors.New("summary view: active catalog digest mismatch")
	}
	store.cacheActive(store.catalogKey(scope, conversationID), headDigest, manifest)
	return manifest.Clone(), head, true, nil
}

func activeCatalogHeadFromManifestData(data []byte) activeCatalogHead {
	var head activeCatalogHead
	_ = json.Unmarshal(data, &head)
	return head
}

func activeDelta(previous, desired []string) ([]activeCatalogAdd, []string) {
	previousSet := make(map[string]struct{}, len(previous))
	desiredSet := make(map[string]struct{}, len(desired))
	for _, id := range previous {
		previousSet[id] = struct{}{}
	}
	for _, id := range desired {
		desiredSet[id] = struct{}{}
	}
	removes := make([]string, 0)
	retained := make([]string, 0, len(previous))
	for _, id := range previous {
		if _, ok := desiredSet[id]; ok {
			retained = append(retained, id)
		} else {
			removes = append(removes, id)
		}
	}
	if !isSubsequence(retained, desired) {
		removes = append([]string(nil), previous...)
		previousSet = map[string]struct{}{}
	}
	adds := make([]activeCatalogAdd, 0)
	for index, id := range desired {
		if _, ok := previousSet[id]; !ok || containsString(removes, id) {
			adds = append(adds, activeCatalogAdd{ID: id, Index: index})
		}
	}
	return adds, removes
}

func applyActiveDelta(ids []string, adds []activeCatalogAdd, removes []string) ([]string, error) {
	removed := make(map[string]struct{}, len(removes))
	for _, id := range removes {
		removed[id] = struct{}{}
	}
	next := make([]string, 0, len(ids)+len(adds))
	for _, id := range ids {
		if _, drop := removed[id]; !drop {
			next = append(next, id)
		}
	}
	for _, add := range adds {
		if add.Index < 0 || add.Index > len(next) || containsString(next, add.ID) {
			return nil, errors.New("summary view: corrupt ordered active delta")
		}
		next = append(next, "")
		copy(next[add.Index+1:], next[add.Index:])
		next[add.Index] = add.ID
	}
	return next, nil
}

func validateActiveHead(head activeCatalogHead, scope sdkmemory.Scope, conversationID string) error {
	if head.SchemaVersion != activeCatalogSchemaVersion || head.RuntimeID != scope.RuntimeID ||
		head.UserID != scope.UserID || head.AgentID != scope.AgentID || head.ConversationID != conversationID ||
		head.BaseID == "" || head.BaseDigest == "" || head.ActiveDigest == "" || head.GenerationID == "" ||
		head.FrontierDigest == "" || head.PublishedAtText == "" || head.SegmentCount < 0 ||
		(head.SegmentCount == 0) != (head.HeadSegmentID == "") ||
		(head.HeadSegmentID == "") != (head.HeadSegmentDigest == "") {
		return errors.New("summary view: corrupt active manifest")
	}
	return nil
}

func validateActiveBase(base activeCatalogBase, scope sdkmemory.Scope, conversationID string, head activeCatalogHead) error {
	digest := base.Digest
	base.Digest = ""
	if base.SchemaVersion != activeCatalogSchemaVersion || base.RuntimeID != scope.RuntimeID ||
		base.UserID != scope.UserID || base.AgentID != scope.AgentID || base.ConversationID != conversationID ||
		base.BaseID != head.BaseID || digest != head.BaseDigest || digest != digestCatalog(base) ||
		!orderedUniqueStrings(base.RecordIDs) {
		return errors.New("summary view: corrupt active base")
	}
	return nil
}

func validateActiveSegment(
	segment activeCatalogSegment,
	scope sdkmemory.Scope,
	conversationID string,
	head activeCatalogHead,
	expectedID string,
	expectedDigest string,
) error {
	digest := segment.Digest
	segment.Digest = ""
	if segment.SchemaVersion != activeCatalogSchemaVersion || segment.RuntimeID != scope.RuntimeID ||
		segment.UserID != scope.UserID || segment.AgentID != scope.AgentID || segment.ConversationID != conversationID ||
		segment.SegmentID != expectedID || digest != expectedDigest || digest != digestCatalog(segment) ||
		segment.BaseID != head.BaseID || segment.BaseDigest != head.BaseDigest ||
		(segment.PreviousSegmentID == "") != (segment.PreviousSegmentDigest == "") {
		return errors.New("summary view: corrupt active segment chain")
	}
	return nil
}

func sameActivePublication(left, right Manifest) bool {
	return left.Scope == right.Scope && left.ConversationID == right.ConversationID &&
		left.GenerationID == right.GenerationID && reflect.DeepEqual(left.RecordIDs, right.RecordIDs) &&
		left.CoverageRange == right.CoverageRange && left.FrontierDigest == right.FrontierDigest
}

func isSubsequence(values, sequence []string) bool {
	index := 0
	for _, value := range sequence {
		if index < len(values) && values[index] == value {
			index++
		}
	}
	return index == len(values)
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func digestCatalog(value any) string {
	data, _ := json.Marshal(value)
	return digestBytes(data)
}

func digestManifest(manifest Manifest) string {
	data, _ := json.Marshal(manifest)
	return digestBytes(data)
}

func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (store *WorkspaceStore) cacheActive(key, headDigest string, manifest Manifest) {
	if _, exists := store.activeCache[key]; !exists {
		store.activeCacheOrder = append(store.activeCacheOrder, key)
	}
	store.activeCache[key] = activeCatalogCache{headDigest: headDigest, manifest: manifest.Clone()}
	for len(store.activeCacheOrder) > store.activeCacheLimit {
		evicted := store.activeCacheOrder[0]
		store.activeCacheOrder = store.activeCacheOrder[1:]
		delete(store.activeCache, evicted)
	}
}

func (store *WorkspaceStore) catalogKey(scope sdkmemory.Scope, conversationID string) string {
	return strings.Join([]string{scope.RuntimeID, scope.UserID, scope.AgentID, conversationID}, "\x00")
}

func (store *WorkspaceStore) activeBasePath(scope sdkmemory.Scope, conversationID, id string) string {
	return path.Join(store.activeCatalogRoot(scope, conversationID), "bases", encode(id)+".json")
}

func (store *WorkspaceStore) activeSegmentPath(scope sdkmemory.Scope, conversationID, id string) string {
	return path.Join(store.activeCatalogRoot(scope, conversationID), "segments", encode(id)+".json")
}

func (store *WorkspaceStore) activeCatalogRoot(scope sdkmemory.Scope, conversationID string) string {
	return path.Dir(store.manifestPath(scope, conversationID))
}
