// Package service implements the language-neutral out-of-process
// plugin channel: a process supervisor, a JSON-RPC 2.0 client, and the
// capability handshake. It is transport-agnostic — stdio (JSONL) and
// HTTP are supported — and binds no concrete resource contract; the
// contract adapters live one layer up.
//
// Protocol v1: the host is the client and the plugin process is the
// server. Startup is lazy: the process is spawned and the handshake
// runs on first use. The host sends the protocol versions it supports
// (a contiguous set); the plugin replies with the highest common
// version, and a version mismatch fails the handshake with
// errdefs.NotAvailable. Capabilities are authoritative from the
// handshake result; the host degrades missing capabilities instead of
// rejecting the plugin.
//
// stdio calls are serialized over one response stream, so a per-call
// timeout abandons a reader on that stream and the process is torn
// down; the next use starts a fresh process. Plugin processes receive
// a minimal environment (PATH, TMPDIR plus Spec.Env) and never
// inherit host secrets.
package service
