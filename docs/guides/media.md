---
layout: default
title: Media & Attachments
---
# Media & Attachments

Attachments travel in the canonical `message` part vocabulary — image,
audio, video, file — and three packages decide what a payload is, how
large it may be, and how it survives the trip into a request:

| Piece | Package | Job |
| --- | --- | --- |
| Classification | `core/utils/filetype` | decide a file's type by its content, not its name |
| Normalization | `core/media/imageutil` | EXIF orientation, alpha flattening, bounded JPEG re-encoding |
| Board adaptation | `core/media/hook` | the `media.attachments` prepare hook |

`core/media` is the payload layer: `core/message/media` keeps the
vocabulary (sources, parts, streams), and `core/media` holds the
handling around it. Nothing in core wires the hook or the normalization
entry points yet — they are the contract hosts consume, and the desktop
application registers `media.attachments` in place of its own hook.

## The `media.attachments` prepare hook

The hook runs as an `agent.Preparer` and normalizes the seeded user
message before the engine reads the board:

- **audio, video and file parts are flattened** into a text line naming
  the attachment path, so a turn whose routed model declares no such
  input still runs, and the file tools can still reach the attachment
  by path;
- **URL-sourced image, audio and video parts that point at a local
  file are inlined** as byte parts: no provider can fetch a host path,
  and inline bytes are what the wire carries;
- **images and text otherwise pass through untouched**: a vision model
  takes the image part directly, and the declaration checks route the
  turn to a target that accepts it;
- **an attachment whose bytes cannot travel is flattened the same way**
  rather than failing the turn: a local file over the inline budget, and
  an inline or stream source with no path to name, are described in the
  line (`[image file] sessions/s-abc/media/2-b.png`,
  `[audio file] (inline audio/webm)`) so the model knows what it did not
  get. How large an attachment is never decides whether a turn runs.

The hook never rewrites the request itself; flattening and inlining
live on the board the engine reads, so a session archive that keeps the
request keeps the original typed attachments.

Flattened lines use forward slashes on every platform, because prompt
text crosses hosts.

Part URLs on the main channel are host-attested: the hook inlines
whatever local file they name, wherever it lives. An attachment
legitimately sits outside the workspace root (`work_dir` is for
readable paths, not confinement — a host keeps session media in its own
data directory), so a host that maps untrusted input into parts must
sanitize before seeding the board.

```yaml
agents:
  assistant:
    prepare:
      - type: media.attachments        # hook.prepare attachment normalizer
        settings:
          work_dir: /workspace           # attachment paths under it render relative
          passthrough_kinds: [video]     # keep these kinds as parts: audio / video / file
          max_inline_bytes: 10485760     # inline budget per attachment (10 MiB default)
          audio_marker: "[audio file] "  # per-kind label (defaults shown)
          video_marker: "[video file] "
          image_marker: "[image file] "
          file_marker: "[file] "
```

Settings:

| Field | Meaning |
| --- | --- |
| `work_dir` | Workspace root; attachment paths under it render relative in the flattened text, paths outside it stay absolute. Optional. |
| `passthrough_kinds` | The non-image kinds (`audio`, `video`, `file`) whose parts survive as parts. The default flattens every non-image attachment: the hook runs before routing, so the declared inputs of the eventual target are not known here. A deployment lists a kind only for turns it routes to a model that declares that input. Images are flattened only when their bytes cannot travel. |
| `max_inline_bytes` | Inline budget for one attachment. Optional; 10 MiB (`hook.DefaultMaxInlineBytes`) when unset, matching `imageutil.MaxInlineImageBytes` so an attachment that survived persistence also survives the trip into a request. A file over it is flattened to a path line, never a failed turn. |
| `audio_marker`, `video_marker`, `image_marker`, `file_marker` | Prefix of the flattened line for that kind. An empty marker falls back to `[audio file] `, `[video file] `, `[image file] ` and `[file] `. |

## Content classification

`core/utils/filetype` answers two questions at once — the media type to
report and whether the payload is text:

```go
kind := filetype.OfPath("report.ts") // or OfData(name, payload)
kind.MediaType                       // the type to report; never empty
kind.Text                            // render as source, read as lines
```

The rule is content-first, in order:

1. a media signature in the leading 512 bytes (image, video, audio,
   PDF) wins when the payload is binary or the name agrees on the
   family — this keeps documents that merely start with `BM` or `ID3`
   as text;
2. a payload without a NUL byte is text, whatever the name says: the
   platform table maps `.ts` to `video/mp2t`, and the TypeScript source
   beside the recording must still open as source;
3. everything else is binary and takes the name's type when the
   platform table has one, and the sniffed type otherwise.

`IsText`, `NameType` and `Family` expose the individual rules.

## Image normalization

`core/media/imageutil` turns an arbitrary attachment image into
predictable prompt bytes:

- `NormalizeToJPEG(r)` decodes JPEG / PNG / GIF / TIFF / BMP, applies
  the EXIF orientation, flattens transparency onto white (JPEG cannot
  carry alpha), and re-encodes at quality 90. Formats the decoder does
  not know (WebP, AVIF, …) return an error so the caller can fall back
  to the original bytes;
- `NormalizeFileToJPEG(path)` is the file entry point: it checks the
  pixel budget from the header first, so a decompression bomb is
  rejected before a full-size allocation;
- `DownscaleToJPEG(r, maxEdge, maxBytes)` fits both bounds: quality is
  stepped down first, and the image is scaled further only when the
  lowest quality still does not fit (`maxBytes <= 0` applies no byte
  budget). The dimensions it returns always describe the bytes it
  returns, including when the budget stays out of reach even at the
  smallest size the ladder produces — then the smallest encoding is
  returned rather than an error;
- `JPEGUpright(path)` reports whether a JPEG's EXIF orientation needs
  no transform, so an upright file can be persisted byte-for-byte
  instead of being re-encoded. It answers what imaging's own EXIF
  reader answers (first APP1 segment only; the orientation is the first
  two bytes of the tag's value field, whatever type and count the entry
  declares), and where it cannot follow a malformed stream — truncated
  EXIF data, marker fill bytes — it reports "needs a transform" rather
  than guessing.

Budgets:

| Constant | Value | Bounds |
| --- | --- | --- |
| `hook.DefaultMaxInlineBytes` | 10 MiB | one inlined attachment of any kind; `max_inline_bytes` overrides it |
| `MaxInlineImageBytes` | 10 MiB | one image: persisted as an attachment, previewed as a data URL |
| `MaxDecodePixels` | 40,000,000 | pixels of any image that is fully decoded |
| `DefaultPromptImageEdge` | 1568 px | longest edge of a prompt-side downscale |
| `DefaultPromptImageBytes` | 786,000 | raw JPEG size whose marshalled part stays under the default 1 MiB non-text part budget (`middleware.DefaultResultPartBudget`), pinned by a test |

Decoding runs on `github.com/disintegration/imaging` (pure Go, no cgo);
TIFF and BMP decoding comes from `golang.org/x/image`, which imaging
imports for registration (a test in the package pins that dependency).
