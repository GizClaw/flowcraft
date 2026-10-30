package maintain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/backends/memory/storage"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

const (
	// verdictRoot holds the arbiter's answers to the pairs earlier passes paid
	// for. It is a cache, not a judgment: the overlay is what a read path
	// applies, and losing these documents costs another call, never a wrong
	// score.
	verdictRoot = "maintain/v2/verdicts"
	// shardWidth is how many hex characters of a digest name its shard. Two
	// characters split a scope's answers into 256 shards, so a pass writes
	// only the ones it added to: the name is the digest's own prefix, and a
	// shard holds every answer that hashes into it, from every conversation
	// of the scope.
	shardWidth = 2
	// verdictEntriesPerShard bounds one shard. A shard is capped rather than
	// the scope so the eviction is local: a write never has to read the whole
	// scope to decide what to forget, and 256 shards put the scope's ceiling
	// at 256x this (204,800 answers), which is more than a corpus of a
	// hundred thousand facts proposes.
	verdictEntriesPerShard = 800
)

// verdictEntry is one remembered answer. Seen is when the answering pass
// judged it, and is what eviction reads once a shard is full: the answers a
// growing corpus keeps re-deriving are the ones worth keeping, and the ones
// about facts a rebuild dropped are the ones worth forgetting.
type verdictEntry struct {
	Verdict    Verdict `json:"verdict"`
	Confidence float64 `json:"confidence,omitempty"`
	Seen       int64   `json:"seen,omitempty"`
}

// verdictDocument is one shard: the arbiter the answers belong to, and the
// answers themselves, keyed by the digest of the question they answered.
//
// The identity is the shard's whole validity condition. A judgment made by one
// model, one question wording, or one threshold is not another's: the answer
// "replaced" is only a finding above the threshold that asked for it, so a
// shard stamped with a different identity is ignored rather than reinterpreted
// under the new one.
type verdictDocument struct {
	Identity string                  `json:"identity"`
	Entries  map[string]verdictEntry `json:"entries,omitempty"`
}

// Verdicts is one pass's view of the answers the arbiter already gave for a
// scope: the ones an earlier pass remembered, plus the ones this pass is about
// to write. A nil *Verdicts answers nothing and remembers nothing, which is
// what a deployment without an arbiter holds.
type Verdicts struct {
	scope    corememory.Scope
	identity string
	// entries is every answer readable under identity, keyed by verdictDigest.
	entries map[string]verdictEntry
	// pending is what this pass asked and must write.
	pending map[string]verdictEntry
	now     int64
}

// verdictScopePrefix is the key prefix one scope's shards live under.
func verdictScopePrefix(scope corememory.Scope) (string, error) {
	partition, err := storage.ScopePartition(scope)
	if err != nil {
		return "", err
	}
	return verdictRoot + "/" + partition, nil
}

// verdictDigest names the question a pair asks, not the pair: the two
// statements and the time expressions the arbiter reads beside them, in the
// order the question states them. Two facts that state the same thing under a
// rebuilt id are the same question and get the same answer; a fact whose text
// moved is a new question and is asked again.
func verdictDigest(pair Pair) string {
	hash := sha256.New()
	for _, statement := range []string{
		pair.Older.Text, pair.Older.TemporalDetail,
		pair.Newer.Text, pair.Newer.TemporalDetail,
	} {
		// The separator keeps "ab"+"c" from digesting like "a"+"bc": the
		// statements are the question's whole content.
		hash.Write([]byte(strings.TrimSpace(statement)))
		hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// Verdicts reads the answers this scope already holds for an arbiter. Shards
// written under another identity are skipped, not reinterpreted: they answer a
// different question. A shard that cannot be decoded fails the read rather than
// counting as a miss -- the caller would pay to ask again and overwrite it, so
// a silent miss would turn a corrupt cache into a quiet cost rather than a
// fault worth seeing.
func (store *Store) Verdicts(ctx context.Context, scope corememory.Scope, identity string) (*Verdicts, error) {
	if store == nil || store.kv == nil {
		return nil, errors.New("maintain: store is incomplete")
	}
	if strings.TrimSpace(identity) == "" {
		return nil, errors.New("maintain: verdicts require an arbiter identity")
	}
	prefix, err := verdictScopePrefix(scope)
	if err != nil {
		return nil, err
	}
	stored, err := store.kv.List(ctx, prefix)
	if err != nil {
		return nil, fmt.Errorf("maintain: load verdicts: %w", err)
	}
	view := &Verdicts{
		scope:    scope,
		identity: identity,
		entries:  make(map[string]verdictEntry, verdictEntriesPerShard),
		pending:  make(map[string]verdictEntry),
		now:      store.clock().UTC().Unix(),
	}
	for _, shard := range stored {
		var document verdictDocument
		if err := json.Unmarshal(shard.Value, &document); err != nil {
			return nil, fmt.Errorf("maintain: decode verdicts %q: %w", shard.Key, err)
		}
		if document.Identity != identity {
			continue
		}
		for digest, entry := range document.Entries {
			view.entries[digest] = entry
		}
	}
	return view, nil
}

// Lookup reports the answer this scope already paid for, if any.
func (verdicts *Verdicts) Lookup(pair Pair) (Judgment, bool) {
	if verdicts == nil {
		return Judgment{}, false
	}
	entry, found := verdicts.entries[verdictDigest(pair)]
	if !found || entry.Verdict == "" {
		return Judgment{}, false
	}
	return Judgment{Verdict: entry.Verdict, Confidence: entry.Confidence}, true
}

// Remember keeps an answer this pass was given, so the next pass over the same
// statements does not pay for it again. Every verdict is kept, including the
// undecided one: a pair the model declined, or answered below the threshold,
// was answered at the cost of a call, and re-asking it on every pass is the
// spend this document exists to stop. A threshold or model change is a new
// identity, which is what reopens the question.
func (verdicts *Verdicts) Remember(pair Pair, judgment Judgment) {
	if verdicts == nil || judgment.Verdict == "" {
		return
	}
	digest := verdictDigest(pair)
	entry := verdictEntry{Verdict: judgment.Verdict, Confidence: judgment.Confidence, Seen: verdicts.now}
	verdicts.entries[digest] = entry
	verdicts.pending[digest] = entry
}

// Pending reports how many answers this pass has to write.
func (verdicts *Verdicts) Pending() int {
	if verdicts == nil {
		return 0
	}
	return len(verdicts.pending)
}

// SaveVerdicts merges this pass's answers into the shards they belong to. It
// writes only the shards this pass added to, and keeps the answers already in
// them: a pass that judged one conversation must not drop what another pass
// paid for.
func (store *Store) SaveVerdicts(ctx context.Context, verdicts *Verdicts) error {
	if store == nil || store.kv == nil {
		return errors.New("maintain: store is incomplete")
	}
	if verdicts == nil || len(verdicts.pending) == 0 {
		return nil
	}
	prefix, err := verdictScopePrefix(verdicts.scope)
	if err != nil {
		return err
	}
	shards := make(map[string]map[string]verdictEntry)
	for digest, entry := range verdicts.pending {
		name := digest[:shardWidth]
		if shards[name] == nil {
			shards[name] = make(map[string]verdictEntry)
		}
		shards[name][digest] = entry
	}
	names := make([]string, 0, len(shards))
	for name := range shards {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		key := prefix + "/" + name
		document := verdictDocument{Identity: verdicts.identity, Entries: make(map[string]verdictEntry)}
		data, err := store.kv.Get(ctx, key)
		switch {
		case err == nil:
			var stored verdictDocument
			if err := json.Unmarshal(data, &stored); err != nil {
				return fmt.Errorf("maintain: decode verdicts %q: %w", key, err)
			}
			if stored.Identity == verdicts.identity {
				for digest, entry := range stored.Entries {
					document.Entries[digest] = entry
				}
			}
		case errors.Is(err, storage.ErrNotFound):
			// First write to this shard.
		default:
			return fmt.Errorf("maintain: load verdicts %q: %w", key, err)
		}
		for digest, entry := range shards[name] {
			document.Entries[digest] = entry
		}
		evictVerdicts(document.Entries, verdictEntriesPerShard)
		if len(document.Entries) == 0 {
			continue
		}
		encoded, err := json.Marshal(document)
		if err != nil {
			return fmt.Errorf("maintain: encode verdicts %q: %w", key, err)
		}
		if err := store.kv.Put(ctx, key, encoded); err != nil {
			return fmt.Errorf("maintain: save verdicts %q: %w", key, err)
		}
	}
	return nil
}

// evictVerdicts bounds a shard by forgetting its oldest answers first. The
// tie-break on the digest keeps a save's output independent of map order, so
// two passes that wrote the same answers store the same document.
func evictVerdicts(entries map[string]verdictEntry, limit int) {
	if limit <= 0 || len(entries) <= limit {
		return
	}
	digests := make([]string, 0, len(entries))
	for digest := range entries {
		digests = append(digests, digest)
	}
	sort.Slice(digests, func(i, j int) bool {
		left, right := entries[digests[i]], entries[digests[j]]
		if left.Seen != right.Seen {
			return left.Seen < right.Seen
		}
		return digests[i] < digests[j]
	})
	for _, digest := range digests[:len(digests)-limit] {
		delete(entries, digest)
	}
}
