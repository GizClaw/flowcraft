package namespace

import (
	"fmt"
	"strconv"
	"strings"
)

const (
	globalToken     = "__global"
	userMarker      = "__u"
	suffixSeparator = "__"
)

// UserScope returns the V2 per-user namespace:
//
//	<prefix>_<Sanitize(runtimeID)>__u<len>_<Sanitize(userID)>
func (p *Prefix) UserScope(runtimeID, userID string) string {
	rt := Sanitize(runtimeID)
	user := Sanitize(userID)
	var b strings.Builder
	b.Grow(len(p.name) + 1 + len(rt) + len(userMarker) + 5 + 1 + len(user))
	b.WriteString(p.name)
	b.WriteByte('_')
	b.WriteString(rt)
	b.WriteString(userMarker)
	b.WriteString(strconv.Itoa(len(user)))
	b.WriteByte('_')
	b.WriteString(user)
	return b.String()
}

// GlobalScope returns the global namespace for runtimeID.
func (p *Prefix) GlobalScope(runtimeID string) string {
	rt := Sanitize(runtimeID)
	var b strings.Builder
	b.Grow(len(p.name) + 1 + len(rt) + len(globalToken))
	b.WriteString(p.name)
	b.WriteByte('_')
	b.WriteString(rt)
	b.WriteString(globalToken)
	return b.String()
}

// SuffixedScope appends a subsystem-owned suffix to a runtime scope.
func (p *Prefix) SuffixedScope(runtimeID, userID, suffix string) string {
	if !isSaneToken(suffix) {
		panic(fmt.Sprintf("retrieval/namespace: invalid suffix %q", suffix))
	}
	base := p.GlobalScope(runtimeID)
	if userID != "" {
		base = p.UserScope(runtimeID, userID)
	}
	return base + suffixSeparator + suffix
}

// DatasetScope returns the dataset-keyed namespace used by knowledge-like
// systems:
//
//	<prefix>_<Sanitize(datasetID)>__<suffix>
func (p *Prefix) DatasetScope(datasetID, suffix string) string {
	if !isSaneToken(suffix) {
		panic(fmt.Sprintf("retrieval/namespace: invalid suffix %q", suffix))
	}
	return p.name + "_" + Sanitize(datasetID) + suffixSeparator + suffix
}

// LegacyUserScopeV1 returns the pre-V2 recall-style user namespace:
//
//	<prefix>_<Sanitize(runtimeID)>__u_<Sanitize(userID)>
//
// Deprecated: V1 user-scope namespaces are kept only for migration tooling
// and will be removed in v0.5.0. New writes must use UserScope.
func (p *Prefix) LegacyUserScopeV1(runtimeID, userID string) string {
	return p.name + "_" + Sanitize(runtimeID) + "__u_" + Sanitize(userID)
}
