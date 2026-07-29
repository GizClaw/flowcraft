// Package config loads versioned inference deployment configuration and
// assembles inference.Runtime instances from it.
//
// The package enforces a SecretRef-only boundary: documents never contain
// plaintext credentials, and every credential is resolved through an explicit
// SecretResolver catalog at build time. Provider packages own their Spec
// decoding and model rules through the Factory contract; this package owns
// envelope validation, secret resolution, and Store-backed persistence.
//
// Reloader supports hot reload: stores implementing Notifier push advisory
// change signals for immediate reloads (Reloader.Watch), with a slow fallback
// poll as safety net; other stores use plain interval polling (Reloader.Run).
// The last-good runtime keeps serving across failed reloads.
package config
