package entity

import (
	"context"

	"github.com/GizClaw/flowcraft/memory/views"
	"github.com/GizClaw/flowcraft/memory/views/fact"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

const (
	// DefaultProfileID is the descriptor ID used by NewProfile unless overridden.
	DefaultProfileID views.ID = "entity-profile"

	// DefaultProfileVersion is the descriptor version used by NewProfile unless overridden.
	DefaultProfileVersion = "v1"
)

// ProfileOption configures a Profile.
type ProfileOption interface {
	applyProfile(*Profile)
}

type profileDescriptorOption struct {
	id      views.ID
	version string
}

// WithProfileID overrides the descriptor ID for Profile.
func WithProfileID(id views.ID) ProfileOption {
	return profileDescriptorOption{id: id}
}

// WithProfileVersion overrides the descriptor version for Profile.
func WithProfileVersion(version string) ProfileOption {
	return profileDescriptorOption{version: version}
}

func (o profileDescriptorOption) applyProfile(p *Profile) {
	if o.id != "" {
		p.id = o.id
	}
	if o.version != "" {
		p.version = o.version
	}
}

// Profile is a lightweight facade for the entity profile view contract.
//
// It persists entity summaries and slots derived from fact graph/ledger outputs.
// The records are semantic views with lineage, not retrieval projections.
type Profile struct {
	store   ProfileStore
	id      views.ID
	version string
}

var _ views.View = (*Profile)(nil)

// NewProfile creates an entity profile view backed by store.
func NewProfile(store ProfileStore, opts ...ProfileOption) *Profile {
	profile := &Profile{
		store:   store,
		id:      DefaultProfileID,
		version: DefaultProfileVersion,
	}
	for _, opt := range opts {
		if opt != nil {
			opt.applyProfile(profile)
		}
	}
	return profile
}

// Descriptor declares the Profile view identity.
func (p *Profile) Descriptor() views.Descriptor {
	return views.Descriptor{
		ID:      p.id,
		Kind:    views.KindEntityProfile,
		Version: p.version,
	}
}

// Put stores or replaces an entity profile record.
func (p *Profile) Put(ctx context.Context, record ProfileRecord) (ProfileRecord, error) {
	if p.store == nil {
		return ProfileRecord{}, errdefs.Validationf("%s: store is required", profileErrPrefix)
	}
	record = cloneProfileRecord(record)
	if err := validateProfileRecord(record); err != nil {
		return ProfileRecord{}, err
	}
	stored, err := p.store.Put(ctx, record)
	if err != nil {
		return ProfileRecord{}, err
	}
	return cloneProfileRecord(stored), nil
}

// Get returns one entity profile by id.
func (p *Profile) Get(ctx context.Context, id ProfileID) (ProfileRecord, bool, error) {
	if p.store == nil {
		return ProfileRecord{}, false, errdefs.Validationf("%s: store is required", profileErrPrefix)
	}
	if err := validateProfileID(id); err != nil {
		return ProfileRecord{}, false, err
	}
	record, ok, err := p.store.Get(ctx, id)
	if err != nil {
		return ProfileRecord{}, false, err
	}
	if !ok {
		return ProfileRecord{}, false, nil
	}
	return cloneProfileRecord(record), true, nil
}

// List returns entity profiles matching opts.
func (p *Profile) List(ctx context.Context, opts ProfileListOptions) ([]ProfileRecord, error) {
	if p.store == nil {
		return nil, errdefs.Validationf("%s: store is required", profileErrPrefix)
	}
	records, err := p.store.List(ctx, opts)
	if err != nil {
		return nil, err
	}
	return cloneProfileRecords(records), nil
}

// Delete removes one entity profile by id. It is idempotent at the Store boundary.
func (p *Profile) Delete(ctx context.Context, id ProfileID) error {
	if p.store == nil {
		return errdefs.Validationf("%s: store is required", profileErrPrefix)
	}
	if err := validateProfileID(id); err != nil {
		return err
	}
	return p.store.Delete(ctx, id)
}

// DeleteEntity removes all profile records for one entity. It is idempotent at the Store boundary.
func (p *Profile) DeleteEntity(ctx context.Context, entityID fact.NodeID) error {
	if p.store == nil {
		return errdefs.Validationf("%s: store is required", profileErrPrefix)
	}
	if err := validateEntityID(profileErrPrefix, entityID); err != nil {
		return err
	}
	return p.store.DeleteEntity(ctx, entityID)
}
