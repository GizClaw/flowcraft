// Package document provides the canonical source of normalized document
// content and provenance. Current values live in a storage.Store and every
// immutable revision is appended to a storage.Log stream per hard scope.
// Documents are hard-partitioned by memory scope and isolated by dataset.
package document
