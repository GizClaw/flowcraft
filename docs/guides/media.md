---
layout: default
title: Media & Attachments
---
# Media & Attachments

Attachments travel in the canonical `message` part vocabulary — text, image,
audio, video, file — and the value types that describe them live in
`core/message/media`. There is no separate media pipeline in core: a payload
enters a request through the tool that read it or the caller that built it, and
the budgets that bound it are owned where they are enforced.

| Piece | Package | Job |
| --- | --- | --- |
| Part vocabulary | `core/message` | `Part`, `Content`, marshalling, `NormalizePart`. |
| Media values | `core/message/media` | Sources (URL or bytes), formats, sizes, aspect ratios, duration. |
| Content classification | `core/tool-builtin/files` | decide what a payload is by its bytes, not its name. |
| Provider surfaces | `core/inference-openai` | transcription, image generation and image edit. |

## The vocabulary

A part is one piece of a message, and `Part` is a sealed union: one concrete type
per kind, so a switch over it is exhaustive and a new kind cannot appear without
every table being revisited. `PartKinds()` is the enumeration: `text`, `image`,
`audio`, `video`, `file`, `data`, `tool_call`, `tool_result`, `reasoning`.

```go
src, err := media.NewImageBytes(png, media.ImageFormatPNG.MediaType())
content := message.NewTextContent("what is in this chart?")
content.Parts = append(content.Parts, message.ImagePart{Source: src})
```

| Type | Carries |
| --- | --- |
| `ImageSource`, `AudioSource`, `VideoSource`, `FileSource` | A URL (`NewImageURL`, …) or inline bytes (`NewImageBytes`, …); `DataURL` renders bytes as a data URI. |
| `ImageFormat` | `png`, `jpeg`, `webp`, `gif`, `avif` — each knows its `MediaType()`. |
| `ImageSize`, `AspectRatio` | Dimensions and `width:height`, both validated on construction. |
| `AudioDurationMillis` | The duration of an audio payload in a given `AudioFormat` (container parsing, no decoding). |

`message.NormalizePart` fills defaults before a part reaches a provider, and
`MarshalPart` / `UnmarshalPart` are the wire form — which is what makes an
attachment survive a checkpoint, a store round trip or a protocol hop.

## How a payload reaches a request

**Through a tool.** `read_file` has an image branch: it sniffs the leading bytes
with `http.DetectContentType` and returns an image part instead of text. The rules
are content-first, because a `.png` may hold a JPEG and a JPEG may be named
`.txt`:

| Bound | Value | On exceed |
| --- | --- | --- |
| Bytes per image | 8 MiB (`maxImageBytes`) | Refused, naming the size and the limit. |
| Longest edge | 4096 px (`maxImageEdge`) | Refused with an instruction to downscale. |
| Decodable formats | PNG, JPEG, GIF | WebP is recognised by content type and refused **by name**, because this build has no decoder for it. |

The refusal is the design: core does not resize or re-encode, so silently
shrinking would hide that the model saw something other than the file.

**Through the caller.** A host that already holds bytes builds the part itself
and seeds the message. Nothing in core rewrites it: there is no prepare hook and
no normalization step, so EXIF orientation, alpha flattening and downscaling are
the host's, before seeding. That is deliberate — a pipeline inside the turn loop
would have to re-encode on every retry and every replay.

**Through a provider surface.** Transcription and image generation/edit are
`inference` extensions in `core/inference-openai`; they take their media as
arguments and return it as parts. See [inference.md](inference.md).

## Budgets

| Constant | Value | Bounds |
| --- | --- | --- |
| `files.maxImageBytes` | 8 MiB | one image read by the file tools. |
| `files.maxImageEdge` | 4096 px | one side of an image read by the file tools. |
| `tool/middleware.DefaultResultPartBudget` | 1 MiB | the encoded size of all non-text parts of one tool result; the `result_limit` middleware enforces it, overridable per deployment. |

The last one is why a large image can be readable and still not fit a tool
result: the read cap bounds one attachment, the part budget bounds what a *tool
result* may carry. A model-facing image that exceeds it comes back as a marker
line rather than an oversized part.

## Flattening, when it happens

A provider that does not accept a kind refuses the request; routing is what
should prevent that, not a silent rewrite in the message. If a host wants a turn
to run when the selected model declares no `audio` input, the flattening (an
`[audio file] path` line) belongs to whoever seeds the turn — `AfterTurn` and
`BeforeTurn` hooks are where a summary of what was dropped goes, not the media
layer. See [hooks.md](hooks.md).

## What is not here

| Not in core | Why |
| --- | --- |
| An attachment store, previews, data URLs for UIs | Host concerns; the part vocabulary is all a turn needs. |
| EXIF orientation, alpha flattening, JPEG re-encoding | A turn loop that re-encodes pays on every retry and every replay. |
| A `media.attachments` prepare hook | Removed with the hook system; seeding is the host's. |
| Resizing on read | Refusing with the limit is honest; resizing hides what the model saw. |
