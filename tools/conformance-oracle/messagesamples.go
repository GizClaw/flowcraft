package main

// The samples the media half of the message contract is dumped from. Every row
// is a call into core's media API or a document handed to its typed wire
// decode; the outcome is computed, never written down here, so a change in
// core shows up as a changed dump rather than as a row nobody re-reads.
//
// "aGk=" is base64 for "hi": a non-empty payload is all the constructors ask
// for, and the wire form echoes it verbatim.
const hiPayload = "aGk="

var mediaRows = []mediaRow{
	// Inline payloads. The media type is required, normalized, and typed.
	{Target: "image", Op: "new_inline", Bytes: hiPayload, MediaType: "image/png"},
	{Target: "audio", Op: "new_inline", Bytes: hiPayload, MediaType: "audio/pcm"},
	{Target: "video", Op: "new_inline", Bytes: hiPayload, MediaType: "video/mp4"},
	{Target: "image", Op: "new_inline", Bytes: hiPayload, MediaType: "IMAGE/PNG"},
	{Target: "image", Op: "new_inline", Bytes: hiPayload, MediaType: "IMAGE/PNG; name=result.png"},
	{Target: "image", Op: "new_inline", Bytes: hiPayload, MediaType: "image/png; c=d; a=b"},
	{Target: "image", Op: "new_inline", Bytes: hiPayload, MediaType: "image/png;charset=utf-8"},
	{Target: "image", Op: "new_inline", Bytes: hiPayload, MediaType: "image/svg+xml"},
	{Target: "audio", Op: "new_inline", Bytes: hiPayload, MediaType: "audio/webm; codecs=opus"},
	{Target: "audio", Op: "new_inline", Bytes: hiPayload, MediaType: "audio/pcm; channels=1"},
	{Target: "video", Op: "new_inline", Bytes: hiPayload, MediaType: `video/webm; codecs="vp9, opus"`},

	// Refusals: another family's type, a type that is not a type, a missing
	// type, a missing payload.
	{Target: "image", Op: "new_inline", Bytes: hiPayload, MediaType: "audio/mpeg"},
	{Target: "audio", Op: "new_inline", Bytes: hiPayload, MediaType: "video/mp4"},
	{Target: "video", Op: "new_inline", Bytes: hiPayload, MediaType: "image/jpeg"},
	{Target: "image", Op: "new_inline", Bytes: hiPayload, MediaType: "image"},
	{Target: "video", Op: "new_inline", Bytes: hiPayload, MediaType: "video"},
	{Target: "image", Op: "new_inline", Bytes: hiPayload, MediaType: ""},
	{Target: "image", Op: "new_inline", Bytes: "", MediaType: "image/png"},
	{Target: "image", Op: "new_inline", Bytes: hiPayload, MediaType: "image/png; ="},
	{Target: "image", Op: "new_inline", Bytes: hiPayload, MediaType: "IMAGE/PNG;"},

	// URL payloads. Absolute request URIs only; the type is optional.
	{Target: "image", Op: "new_url", URL: "https://example.com/i.png", MediaType: "image/png"},
	{Target: "audio", Op: "new_url", URL: "https://example.com/a.mp3", MediaType: "audio/mpeg"},
	{Target: "video", Op: "new_url", URL: "https://example.com/v.mp4", MediaType: "video/mp4"},
	{Target: "image", Op: "new_url", URL: "https://example.com/i.png", MediaType: ""},
	{Target: "image", Op: "new_url", URL: "http://example.com/i.png", MediaType: "image/png"},
	{Target: "image", Op: "new_url", URL: "https://example.com/i", MediaType: "image/png; name=x"},
	{Target: "image", Op: "new_url", URL: "/i.png", MediaType: "image/png"},
	{Target: "image", Op: "new_url", URL: "example.com/i.png", MediaType: "image/png"},
	{Target: "image", Op: "new_url", URL: "https://example.com/i.png#f", MediaType: "image/png"},
	{Target: "image", Op: "new_url", URL: "mailto:x@example.com", MediaType: "image/png"},
	{Target: "audio", Op: "new_url", URL: "https://example.com/a", MediaType: "image/png"},

	// The typed wire decode: payload rules, the type rules, the strict
	// decoder. The relative URL is in here twice for a reason -- core's
	// document path does not repeat the constructors' absoluteness check.
	{Target: "image", Op: "from_wire", Document: `{"kind":"inline","data":"aGk=","media_type":"IMAGE/PNG"}`},
	{Target: "image", Op: "from_wire", Document: `{"kind":"url","url":"https://x/y","media_type":"image/png"}`},
	{Target: "image", Op: "from_wire", Document: `{"kind":"url","url":"https://x/y"}`},
	{Target: "image", Op: "from_wire", Document: `{"kind":"url","url":"/relative"}`},
	{Target: "image", Op: "from_wire", Document: `{"kind":"url","url":"https://x/y","media_type":""}`},
	{Target: "image", Op: "from_wire", Document: `{"kind":"inline","data":"aGk=","media_type":"image/png","url":""}`},
	{Target: "audio", Op: "from_wire", Document: `{"kind":"inline","data":"aGk=","media_type":"image/png"}`},
	{Target: "image", Op: "from_wire", Document: `{"kind":"url"}`},
	{Target: "image", Op: "from_wire", Document: `{"kind":"url","url":"https://x/y","data":"aGk="}`},
	{Target: "image", Op: "from_wire", Document: `{"kind":"inline","data":"aGk="}`},
	{Target: "image", Op: "from_wire", Document: `{"kind":"inline","media_type":"image/png"}`},
	{Target: "image", Op: "from_wire", Document: `{"kind":"stream","media_type":"image/png"}`},
	{Target: "image", Op: "from_wire", Document: `{"kind":"socket"}`},
	{Target: "image", Op: "from_wire", Document: `{}`},

	// encoding/json's own refusals, spelled its way.
	{Target: "image", Op: "from_wire", Document: `{"kind":"url","url":"https://x/y","extra":1}`, TextFrom: "encoding/json"},
	{Target: "image", Op: "from_wire", Document: `{"kind":"inline","data":"!!!","media_type":"image/png"}`, TextFrom: "encoding/json"},
	{Target: "image", Op: "from_wire", Document: `{"kind":"url","url":"https://x/y"} {"kind":"url","url":"https://x/y"}`, TextFrom: "encoding/json"},
}

var formatRows = []formatRow{
	// Raw families need both numbers, and they need them positive.
	{Encoding: "pcm16", SampleRateHz: intPtr(16000), Channels: intPtr(1)},
	{Encoding: "pcm16"},
	{Encoding: "pcm16", SampleRateHz: intPtr(0), Channels: intPtr(1)},
	{Encoding: "pcm16", SampleRateHz: intPtr(16000), Channels: intPtr(0)},
	{Encoding: "pcm16", SampleRateHz: intPtr(16000), Channels: intPtr(-1)},
	{Encoding: "pcm24", SampleRateHz: intPtr(48000), Channels: intPtr(2)},
	{Encoding: "float32", SampleRateHz: intPtr(48000), Channels: intPtr(2)},

	// The compressed families may leave the numbers out; negative ones are
	// refused rather than read as "not declared".
	{Encoding: "mp3"},
	{Encoding: "mp3", SampleRateHz: intPtr(44100), Channels: intPtr(2)},
	{Encoding: "mp3", SampleRateHz: intPtr(0)},
	{Encoding: "mp3", SampleRateHz: intPtr(-1)},
	{Encoding: "opus", Channels: intPtr(2)},
	{Encoding: "opus", Channels: intPtr(0)},
	{Encoding: "opus", Channels: intPtr(-1)},
	{Encoding: "aac"},
	{Encoding: "flac", SampleRateHz: intPtr(-1), Channels: intPtr(2)},
}

func intPtr(value int) *int { return &value }
