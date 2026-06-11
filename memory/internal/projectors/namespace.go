package projectors

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/GizClaw/flowcraft/memory/internal/views/indexed"
	"github.com/GizClaw/flowcraft/memory/views"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

const (
	projectionNamespaceMaxLen  = 48
	projectionNamespaceHashLen = 8
)

// ScopedNamespace returns the physical projection namespace for the hard
// runtime/user partition represented by scope.
func ScopedNamespace(base string, scope views.Scope) (string, error) {
	base = strings.TrimSpace(base)
	if err := (indexed.Binding{Namespace: base}).Validate(); err != nil {
		return "", errdefs.Validationf("%s: invalid projection namespace base: %w", errPrefix, err)
	}
	if err := scope.Validate(); err != nil {
		return "", errdefs.Validationf("%s: invalid projection scope: %w", errPrefix, err)
	}

	userKind := "g"
	userValue := "global"
	if strings.TrimSpace(scope.UserID) != "" {
		userKind = "u"
		userValue = strings.TrimSpace(scope.UserID)
	}
	suffix := "_rt_" + projectionNamespaceHash(scope.RuntimeID) + "_" + userKind + "_" + projectionNamespaceHash(userValue)
	baseLimit := projectionNamespaceMaxLen - len(suffix)
	if baseLimit <= 0 {
		return "", errdefs.Validationf("%s: projection namespace suffix is too long", errPrefix)
	}
	namespace := shortenProjectionNamespaceBase(base, baseLimit) + suffix
	if err := (indexed.Binding{Namespace: namespace}).Validate(); err != nil {
		return "", errdefs.Validationf("%s: invalid scoped projection namespace: %w", errPrefix, err)
	}
	return namespace, nil
}

func shortenProjectionNamespaceBase(base string, limit int) string {
	if len(base) <= limit {
		return base
	}
	hash := projectionNamespaceHash(base)
	if limit <= len(hash) {
		return hash[:limit]
	}
	prefixLen := limit - len(hash) - 1
	return base[:prefixLen] + "_" + hash
}

func projectionNamespaceHash(value string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(value)))
	return hex.EncodeToString(sum[:])[:projectionNamespaceHashLen]
}
