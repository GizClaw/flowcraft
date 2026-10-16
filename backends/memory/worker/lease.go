package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/backends/memory/storage"
	corememory "github.com/GizClaw/flowcraft/core/memory"
)

const leaseRoot = "worker/v1/leases"

// leaseKV is the KV surface a lease needs: the absent-key write that takes a
// scope nobody holds, and the compare-and-swap write that takes one whose claim
// expired. Both are storage extensions, so a substrate without them cannot
// hand out leases.
type leaseKV interface {
	storage.Store
	storage.CASStore
	storage.PutIfAbsentStore
}

// ScopeLease is one runner's claim on a scope's derivation. The claim is the
// only one its scope has at a time; a claim that is not renewed past
// ExpiresAt stops being one, which is what bounds a runner that died mid-pass.
type ScopeLease struct {
	Scope corememory.Scope `json:"scope"`
	// Holder identifies the runner, not the process: two runners in one
	// process are two holders. It is what a renewal and a release have to
	// match, and what a host reads to see who is deriving.
	Holder string `json:"holder"`
	// AcquiredAt is when this holder first took the scope; a renewal keeps it,
	// so it ages with the claim rather than with the last write.
	AcquiredAt time.Time `json:"acquired_at"`
	// ExpiresAt is when the claim stops being one unless it is renewed.
	ExpiresAt time.Time `json:"expires_at"`
	// Released marks a claim its holder gave back, written where the claim
	// was: the scope is free, and the next acquirer takes it with one
	// compare-and-swap instead of finding a deleted key.
	Released bool `json:"released,omitempty"`
}

// Held reports whether the claim is one right now.
func (lease ScopeLease) Held() bool {
	return !lease.Released && lease.Holder != ""
}

// Expired reports whether the claim has stopped being one at the given time.
func (lease ScopeLease) Expired(now time.Time) bool {
	return lease.Released || !now.Before(lease.ExpiresAt)
}

// LeaseStore hands out one scope's lease. It is shared state: every runner
// over the same workspace opens it on the same KV area, so exactly one of them
// holds a scope at a time.
//
// The lease is cooperative and scoped to one scope's derivation: a runner
// takes it before it derives that scope, renews it while it works, and gives it
// back when it stops. It does not make derivation exclusive by itself -- the
// derivation is idempotent either way -- it makes the *work* exclusive, so two
// runners sharing a workspace do not both hunt the same conversations.
type LeaseStore struct {
	kv    leaseKV
	clock func() time.Time
}

// LeaseOption configures the lease store.
type LeaseOption func(*LeaseStore)

// WithLeaseClock replaces the clock the lease is measured against. The clock
// has to move forward for a claim to expire, and every runner comparing claims
// has to read the same one for a lease to mean anything across processes.
func WithLeaseClock(clock func() time.Time) LeaseOption {
	return func(store *LeaseStore) {
		if clock != nil {
			store.clock = clock
		}
	}
}

// NewLeaseStore constructs a lease store over a KV substrate that supports
// absent-key writes and compare-and-swap.
func NewLeaseStore(kv storage.Store, options ...LeaseOption) (*LeaseStore, error) {
	if nilInterface(kv) {
		return nil, errors.New("memory worker: lease kv store is required")
	}
	store, ok := kv.(leaseKV)
	if !ok {
		return nil, fmt.Errorf(
			"memory worker: lease requires a kv store with compare-and-swap and absent-key writes, %T has neither",
			kv)
	}
	result := &LeaseStore{kv: store, clock: time.Now}
	for _, option := range options {
		if option != nil {
			option(result)
		}
	}
	return result, nil
}

// Acquire takes one scope for holder until ttl from now. It reports whether
// this call took the scope: a scope another holder has is returned unheld,
// together with the claim that kept it, so a caller can report who is working.
//
// A holder re-acquiring its own claim gets it back: the claim is already the
// holder's, and a runner that asks twice is asking for what it has.
func (store *LeaseStore) Acquire(
	ctx context.Context,
	scope corememory.Scope,
	holder string,
	ttl time.Duration,
) (ScopeLease, bool, error) {
	if store == nil || nilInterface(store.kv) {
		return ScopeLease{}, false, errors.New("memory worker: lease store is incomplete")
	}
	if ctx == nil {
		return ScopeLease{}, false, errors.New("memory worker: context is required")
	}
	if err := scope.Validate(); err != nil {
		return ScopeLease{}, false, err
	}
	if err := validateHolder(holder); err != nil {
		return ScopeLease{}, false, err
	}
	if ttl <= 0 {
		return ScopeLease{}, false, errors.New("memory worker: lease ttl must be positive")
	}
	key, err := leaseKey(scope)
	if err != nil {
		return ScopeLease{}, false, err
	}
	now := store.clock().UTC()
	claim := ScopeLease{Scope: scope, Holder: holder, AcquiredAt: now, ExpiresAt: now.Add(ttl)}
	encoded, err := encodeLease(claim)
	if err != nil {
		return ScopeLease{}, false, err
	}
	stored, raw, found, err := store.read(ctx, key)
	if err != nil {
		return ScopeLease{}, false, err
	}
	if found && !stored.Expired(now) {
		if stored.Holder != holder {
			return stored, false, nil
		}
		return stored, true, nil
	}
	if !found {
		taken, err := store.kv.PutIfAbsent(ctx, key, encoded)
		if err != nil {
			return ScopeLease{}, false, fmt.Errorf("memory worker: take lease: %w", err)
		}
		if taken {
			return claim, true, nil
		}
		// Someone wrote the key between the read and the write. Read it back:
		// it is either a fresh claim of theirs or one this holder still has.
		stored, _, found, err = store.read(ctx, key)
		if err != nil {
			return ScopeLease{}, false, err
		}
		if !found {
			return ScopeLease{}, false, nil
		}
		if stored.Holder == holder && !stored.Expired(now) {
			return stored, true, nil
		}
		return stored, false, nil
	}
	// The claim on the key expired. Taking it is one compare-and-swap from the
	// exact bytes that were read, so a runner that renewed in between is not
	// overwritten by a claim measured against the state before its renewal.
	taken, err := store.kv.CompareAndSwap(ctx, key, raw, encoded)
	if err != nil {
		return ScopeLease{}, false, fmt.Errorf("memory worker: take expired lease: %w", err)
	}
	if !taken {
		stored, _, _, err := store.read(ctx, key)
		if err != nil {
			return ScopeLease{}, false, err
		}
		return stored, false, nil
	}
	return claim, true, nil
}

// Renew extends the claim the holder already has. It reports whether the scope
// is still the holder's: a claim another runner took, or one that was released,
// is not renewed, and that is the signal for a runner to stop working.
//
// A claim of this holder that expired but was not taken is renewed as well.
// Nobody else wanted the scope, and the alternative -- dropping a pass over
// work no one else is doing -- buys nothing; a runner that lost the scope to
// another holder still sees the renewal fail.
func (store *LeaseStore) Renew(
	ctx context.Context,
	lease ScopeLease,
	ttl time.Duration,
) (ScopeLease, bool, error) {
	if store == nil || nilInterface(store.kv) {
		return ScopeLease{}, false, errors.New("memory worker: lease store is incomplete")
	}
	if ctx == nil {
		return ScopeLease{}, false, errors.New("memory worker: context is required")
	}
	if err := lease.Scope.Validate(); err != nil {
		return ScopeLease{}, false, err
	}
	if err := validateHolder(lease.Holder); err != nil {
		return ScopeLease{}, false, err
	}
	if ttl <= 0 {
		return ScopeLease{}, false, errors.New("memory worker: lease ttl must be positive")
	}
	key, err := leaseKey(lease.Scope)
	if err != nil {
		return ScopeLease{}, false, err
	}
	stored, raw, found, err := store.read(ctx, key)
	if err != nil {
		return ScopeLease{}, false, err
	}
	if !found || stored.Holder != lease.Holder || stored.Released {
		return stored, false, nil
	}
	now := store.clock().UTC()
	acquiredAt := stored.AcquiredAt
	if acquiredAt.IsZero() {
		acquiredAt = now
	}
	renewed := ScopeLease{
		Scope: lease.Scope, Holder: lease.Holder, AcquiredAt: acquiredAt, ExpiresAt: now.Add(ttl),
	}
	encoded, err := encodeLease(renewed)
	if err != nil {
		return ScopeLease{}, false, err
	}
	swapped, err := store.kv.CompareAndSwap(ctx, key, raw, encoded)
	if err != nil {
		return ScopeLease{}, false, fmt.Errorf("memory worker: renew lease: %w", err)
	}
	if !swapped {
		stored, _, _, err := store.read(ctx, key)
		if err != nil {
			return ScopeLease{}, false, err
		}
		return stored, false, nil
	}
	return renewed, true, nil
}

// Release gives the scope back before the claim would expire. It reports
// whether the claim was still this holder's: a claim already taken over is not
// released, because the runner releasing it does not own it any more.
//
// The claim is written back expired rather than deleted, so the next acquirer
// takes the scope with the same compare-and-swap it would use on an expired
// claim, and no release can delete a claim that arrived after it.
func (store *LeaseStore) Release(ctx context.Context, lease ScopeLease) (bool, error) {
	if store == nil || nilInterface(store.kv) {
		return false, errors.New("memory worker: lease store is incomplete")
	}
	if ctx == nil {
		return false, errors.New("memory worker: context is required")
	}
	if err := lease.Scope.Validate(); err != nil {
		return false, err
	}
	if err := validateHolder(lease.Holder); err != nil {
		return false, err
	}
	key, err := leaseKey(lease.Scope)
	if err != nil {
		return false, err
	}
	stored, raw, found, err := store.read(ctx, key)
	if err != nil {
		return false, err
	}
	if !found || stored.Holder != lease.Holder || stored.Released {
		return false, nil
	}
	now := store.clock().UTC()
	released := ScopeLease{
		Scope: lease.Scope, Holder: lease.Holder, AcquiredAt: stored.AcquiredAt,
		ExpiresAt: now, Released: true,
	}
	encoded, err := encodeLease(released)
	if err != nil {
		return false, err
	}
	swapped, err := store.kv.CompareAndSwap(ctx, key, raw, encoded)
	if err != nil {
		return false, fmt.Errorf("memory worker: release lease: %w", err)
	}
	return swapped, nil
}

// Current returns the claim stored for one scope, whether or not it is still
// held. It is the read behind a host's diagnostics: it does not take, renew or
// release anything.
func (store *LeaseStore) Current(
	ctx context.Context,
	scope corememory.Scope,
) (ScopeLease, bool, error) {
	if store == nil || nilInterface(store.kv) {
		return ScopeLease{}, false, errors.New("memory worker: lease store is incomplete")
	}
	if ctx == nil {
		return ScopeLease{}, false, errors.New("memory worker: context is required")
	}
	key, err := leaseKey(scope)
	if err != nil {
		return ScopeLease{}, false, err
	}
	lease, _, found, err := store.read(ctx, key)
	return lease, found, err
}

// read returns the claim stored at key together with the exact bytes it was
// read from: the compare-and-swap writes have to be measured against the state
// the claim was decided on, not against a re-encoded form of the decoded
// struct.
func (store *LeaseStore) read(
	ctx context.Context,
	key string,
) (ScopeLease, []byte, bool, error) {
	data, err := store.kv.Get(ctx, key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return ScopeLease{}, nil, false, nil
		}
		return ScopeLease{}, nil, false, fmt.Errorf("memory worker: read lease: %w", err)
	}
	var lease ScopeLease
	if err := decodeStrict(data, &lease); err != nil {
		return ScopeLease{}, nil, false, fmt.Errorf("memory worker: decode lease: %w", err)
	}
	if err := validateLease(lease, key); err != nil {
		return ScopeLease{}, nil, false, err
	}
	return lease, data, true, nil
}

func encodeLease(lease ScopeLease) ([]byte, error) {
	data, err := json.Marshal(lease)
	if err != nil {
		return nil, fmt.Errorf("memory worker: encode lease: %w", err)
	}
	return data, nil
}

func leaseKey(scope corememory.Scope) (string, error) {
	partition, err := storage.ScopePartition(scope)
	if err != nil {
		return "", err
	}
	return leaseRoot + "/" + partition, nil
}

func validateHolder(holder string) error {
	switch {
	case strings.TrimSpace(holder) == "":
		return errors.New("memory worker: lease holder is required")
	case holder != strings.TrimSpace(holder):
		return errors.New("memory worker: lease holder must not carry surrounding whitespace")
	}
	return nil
}

func validateLease(lease ScopeLease, key string) error {
	if err := lease.Scope.Validate(); err != nil {
		return fmt.Errorf("memory worker: lease %q: %w", key, err)
	}
	if err := validateHolder(lease.Holder); err != nil {
		return fmt.Errorf("memory worker: lease %q: %w", key, err)
	}
	if lease.ExpiresAt.IsZero() {
		return fmt.Errorf("memory worker: lease %q has no expiry", key)
	}
	return nil
}
