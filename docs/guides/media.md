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
| Normalization | `core/message/media/imageutil` | EXIF orientation, alpha flattening, bounded JPEG re-encoding |
| Board adaptation | `core/message/media/hook` | the `media.attachments` prepare hook |

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
  turn to a target that accepts it.

The hook never rewrites the request itself; flattening and inlining
live on the board the engine reads, so a session archive that keeps the
request keeps the original typed attachments.

```yaml
agents:
  assistant:
    prepare:
      - type: media.attachments        # hook.prepare attachment normalizer
        settings:
          work_dir: /workspace         # attachment paths under it render relative
          passthrough_kinds: [video]   # keep these kinds as parts: audio / video / file
```

Settings:

| Field | Meaning |
| --- | --- |
| `work_dir` | Workspace root; attachment paths under it render relative in the flattened text, paths outside it stay absolute. Optional. |
| `passthrough_kinds` | The non-image kinds (`audio`, `video`, `file`) whose parts survive as parts. The default flattens every non-image attachment: the hook runs before routing, so the declared inputs of the eventual target are not known here. A deployment lists a kind only for turns it routes to a model that declares that input. Images are never flattened. |
| `audio_marker`, `video_marker`, `file_marker` | Prefix of the flattened line for that kind. An empty marker falls back to `[audio file] `, `[video file] ` and `[file] `. |

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

`core/message/media/imageutil` turns an arbitrary attachment image into
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
  lowest quality still does not fit. It returns the dimensions that
  were actually encoded;
- `JPEGUpright(path)` reports whether a JPEG's EXIF orientation needs
  no transform, so an upright file can be persisted byte-for-byte
  instead of being re-encoded.

Budgets:

| Constant | Value | Bounds |
| --- | --- | --- |
| `MaxInlineImageBytes` | 10 MiB | one inlined attachment, including the hook's local-file inlining |
| `MaxDecodePixels` | 40,000,000 | pixels of any image that is fully decoded |
| `DefaultPromptImageEdge` | 1568 px | longest edge of a prompt-side downscale |
| `DefaultPromptImageBytes` | 786,000 | raw JPEG size whose marshalled part stays under the default 1 MiB non-text part budget (`middleware.DefaultResultPartBudget`), pinned by a test |

Decoding runs on `github.com/disintegration/imaging` (pure Go, no cgo);
TIFF and BMP decoding comes from `golang.org/x/image`.
