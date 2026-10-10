// Package media is the payload layer for attachments: what a payload
// is, how large it may be, and how it survives the trip into a request.
// Three pieces share the job, and this package documents where each
// lives:
//
//   - classification is core/utils/filetype: a payload's type is decided
//     by its bytes, not by its name;
//   - normalization is core/media/imageutil: EXIF orientation, alpha
//     flattening, bounded JPEG re-encoding;
//   - board adaptation is core/media/hook: the media.attachments prepare
//     hook, which flattens the attachments a routed model cannot take and
//     inlines the local files it can.
//
// The vocabulary itself stays in core/message/media (sources, parts,
// streams): that package is the wire contract, this one the handling
// around it. A payload's path into a request therefore reads
// message.Part → media/hook → media/imageutil, with filetype answering
// what the bytes are along the way.
//
// # Budgets
//
// Three budgets bound a payload, and each is owned where it is enforced:
//
//   - hook.DefaultMaxInlineBytes (10 MiB) is one attachment's inline
//     budget: the hook reads a local file into the board below it and
//     flattens anything larger to a path line;
//   - imageutil.MaxInlineImageBytes (10 MiB) is the same bound for the
//     image paths hosts own: persisting an attachment, previewing one;
//   - imageutil.DefaultPromptImageBytes (786 KB) is far smaller, because
//     an inline part is replayed in every later turn's context. It
//     bounds what a host normalizes before seeding, not what the hook
//     carries: core/media/hook moves bytes verbatim.
//
// # Consumers
//
// Nothing in core wires the hook or the normalization entry points
// yet: they are the contract hosts consume. The desktop application
// registers media.attachments in place of its own hook and calls
// imageutil when it persists attachments, and the model-facing tools
// that read and render images (view_image, generate_image) follow in
// their own change. Until then the packages are pinned by their own
// tests — including the cross-package wiring tests in
// imageutil/budget_test.go — rather than by in-repo callers.
package media
