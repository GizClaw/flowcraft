package a2a

import (
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/message/media"
	a2aprotocol "github.com/a2aproject/a2a-go/v2/a2a"
)

// a2aPartFromMessagePart converts one FlowCraft message part into an A2A
// part. The second return value reports whether the part was representable;
// unsupported kinds (tool calls, tool results, reasoning traces) are skipped
// rather than mangled, matching the A2A "tolerate unknown parts" rule.
func a2aPartFromMessagePart(part message.Part) (out *a2aprotocol.Part, handled bool, err error) {
	normalized, err := message.NormalizePart(part)
	if err != nil {
		return nil, false, err
	}
	switch v := normalized.(type) {
	case message.TextPart:
		return a2aprotocol.NewTextPart(v.Text), true, nil
	case message.DataPart:
		var data any
		if err := json.Unmarshal(v.Value, &data); err != nil {
			return nil, false, err
		}
		return a2aprotocol.NewDataPart(data), true, nil
	case message.FilePart:
		out := a2aprotocol.NewFileURLPart(a2aprotocol.URL(v.URI), v.MediaType)
		out.Filename = v.Name
		return out, true, nil
	case message.ImagePart:
		return a2aPartFromMediaSource(v.Source, "")
	case message.AudioPart:
		return a2aPartFromMediaSource(v.Source, "")
	case message.VideoPart:
		return a2aPartFromMediaSource(v.Source, "")
	default:
		return nil, false, nil
	}
}

// a2aPartFromMediaSource converts an inline/URL media source into an A2A
// part: URLs become file parts with a URL, inline bytes become raw parts.
func a2aPartFromMediaSource(src interface {
	Kind() media.SourceKind
	URL() string
	Bytes() []byte
	MediaType() string
}, name string) (*a2aprotocol.Part, bool, error) {
	switch src.Kind() {
	case media.SourceURL:
		out := a2aprotocol.NewFileURLPart(a2aprotocol.URL(src.URL()), src.MediaType())
		out.Filename = name
		return out, true, nil
	default:
		out := a2aprotocol.NewRawPart(src.Bytes())
		out.MediaType = src.MediaType()
		out.Filename = name
		return out, true, nil
	}
}

// messagePartsFromA2A converts A2A parts into FlowCraft message parts.
// Text maps to TextPart, URL/file parts to FilePart (URLs verbatim, raw
// bytes re-encoded as data: URIs so they round-trip through the URI-based
// FilePart), and data parts to DataPart. Unknown content kinds are skipped
// with forward compatibility.
func messagePartsFromA2A(parts []*a2aprotocol.Part) []message.Part {
	var out []message.Part
	for _, p := range parts {
		if p == nil || p.Content == nil {
			continue
		}
		switch {
		case p.Text() != "":
			out = append(out, message.TextPart{Text: p.Text()})
		case p.URL() != "":
			out = append(out, message.FilePart{
				URI:       string(p.URL()),
				MediaType: p.MediaType,
				Name:      p.Filename,
			})
		case p.Raw() != nil:
			out = append(out, message.FilePart{
				URI:       dataURI(p.MediaType, p.Raw()),
				MediaType: p.MediaType,
				Name:      p.Filename,
			})
		case p.Data() != nil:
			raw, err := json.Marshal(p.Data())
			if err != nil {
				continue
			}
			out = append(out, message.DataPart{MediaType: p.MediaType, Value: raw})
		}
	}
	return out
}

// messageText concatenates the text parts of an A2A message. It is used to
// build human-readable errors and prompts from remote status messages.
func messageText(m *a2aprotocol.Message) string {
	if m == nil {
		return ""
	}
	var b strings.Builder
	for _, p := range m.Parts {
		if p == nil || p.Text() == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(p.Text())
	}
	return b.String()
}

// dataURI encodes bytes as a data: URI so raw A2A parts can ride in the
// URI-based message.FilePart without inventing a transport.
func dataURI(mediaType string, data []byte) string {
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	return "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(data)
}
