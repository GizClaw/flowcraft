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
	{Target: "image", Op: "new_url", URL: "HTTPS://EXAMPLE.COM/i.png", MediaType: "image/png"},
	{Target: "image", Op: "new_url", URL: "https://example.com/i.png?q=1", MediaType: "image/png"},
	{Target: "image", Op: "new_url", URL: "https://exa mple.com/i.png", MediaType: "image/png"},
	{Target: "image", Op: "new_url", URL: "https://example.com/a b.png", MediaType: "image/png"},
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

	// An unknown field is encoding/json's sentence, and a small enough one that
	// the port reproduces it verbatim; the two below are its syntax
	// diagnostics, which the port does not.
	{Target: "image", Op: "from_wire", Document: `{"kind":"url","url":"https://x/y","extra":1}`},
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

// The part samples: every kind in its valid form, the format corners, and the
// refusals -- core's codec's own, and the strict decoder's. One case is
// deliberately absent: a format whose `encoding` is outside the table. `core`
// decodes it (the field is a string) and refuses it from `Validate`; the port
// refuses it at decode, because its encoding is a type. Same sentence, earlier
// stage -- see the replay's module docs.
var partSamples = []string{
	// Every kind, in its valid form.
	`{"type":"text","text":"hi"}`,
	`{"type":"text","text":"<a>&\u2028 café"}`,
	`{"type":"image","source":{"kind":"inline","data":"aGk=","media_type":"IMAGE/PNG"}}`,
	`{"type":"image","source":{"kind":"url","url":"https://x/y.png","media_type":"image/png; name=a.png"}}`,
	`{"type":"audio","source":{"kind":"url","url":"https://x/a","media_type":"audio/mpeg"},"format":{"encoding":"mp3","sample_rate_hz":44100,"channels":2}}`,
	`{"type":"audio","source":{"kind":"inline","data":"aGk=","media_type":"audio/pcm"},"format":{"encoding":"pcm16","sample_rate_hz":24000,"channels":1},"duration_millis":1500}`,
	`{"type":"video","source":{"kind":"url","url":"https://x/v.mp4","media_type":"video/mp4"}}`,
	`{"type":"file","uri":"file:///tmp/a.txt","media_type":"text/plain","name":"a.txt"}`,
	`{"type":"file","uri":"s3://bucket/key"}`,
	`{"type":"data","media_type":"application/json","value":{"b":1,"a":[true,null]}}`,
	`{"type":"data","value": { "a" : 1 } }`,
	`{"type":"data","value":{"a":"<b>&"}}`,
	`{"type":"tool_call","call":{"id":"c1","name":"search","arguments":{"q":"x"}}}`,
	`{"type":"tool_result","result":{"call_id":"c1","content":{"parts":[{"type":"text","text":"ok"}]},"is_error":true}}`,
	`{"type":"reasoning","text":"hmm","signature":"sig","id":"r1","source":"openai/gpt"}`,
	`{"type":"reasoning","signature":"sig"}`,

	// Audio format corners: a compressed format reads a zero as "not declared"
	// and drops it from the wire, a raw one needs both numbers, a duration of
	// zero survives because it is a pointer, and a source whose media type
	// disagrees with its format is refused.
	`{"type":"audio","source":{"kind":"url","url":"https://x/a"},"format":{"encoding":"mp3","sample_rate_hz":0}}`,
	`{"type":"audio","source":{"kind":"url","url":"https://x/a"},"format":{"encoding":"pcm16"}}`,
	`{"type":"audio","source":{"kind":"url","url":"https://x/a"},"format":{"encoding":"mp3","sample_rate_hz":-1}}`,
	`{"type":"audio","source":{"kind":"url","url":"https://x/a"},"duration_millis":0}`,
	`{"type":"audio","source":{"kind":"url","url":"https://x/a"},"duration_millis":-1}`,
	`{"type":"audio","source":{"kind":"inline","data":"aGk=","media_type":"audio/webm"},"format":{"encoding":"mp3"}}`,

	// The codec's own refusals.
	`{"type":"blob"}`,
	`{}`,
	`{"type":null}`,
	`{"type":"text"}`,
	`{"type":"text","text":null}`,
	`{"type":"reasoning","text":"","signature":""}`,
	`{"type":"file"}`,
	`{"type":"data"}`,
	`{"type":"data","value":null}`,
	`{"type":"data","value":[1]}`,
	`{"type":"tool_call"}`,
	`{"type":"tool_call","call":{"id":"c1","name":"x"}}`,
	`{"type":"tool_result"}`,
	`{"type":"tool_result","result":{"content":{"parts":[{"type":"text","text":"x"}]}}}`,

	// Strictness: the part level, the source level, and the JSON text itself.
	`{"type":"text","text":"hi","zzz":1}`,
	`{"type":"file","uri":"u","nope":1}`,
	`{"type":"text","text":5}`,
	`{"type":"image","source":null}`,
	`{"type":"image","source":{}}`,
	`{"type":"image","source":{"kind":"inline","data":"aGk=","media_type":"audio/mpeg"}}`,
	`{"type":"image","source":{"kind":"stream"}}`,
	`{"type":"image","source":{"kind":"url","url":"/relative"}}`,
	`{"type":"image","source":{"kind":"url","url":"https://x/y","extra":1}}`,
	`"hi"`,
	`{"type":"text","text":"hi"} {"type":"text"}`,
}

// The message samples: the permissive message level over the strict content
// level, the role rules, and the codec's refusals. An unknown or absent role is
// deliberately absent -- core decodes it and refuses it from `Validate`, the
// port refuses it at decode (its role is a type), same sentence, earlier stage.
var messageSamples = []string{
	`{"role":"user","content":{"parts":[{"type":"text","text":"hi"}]}}`,
	`{"role":"system","content":{"parts":[{"type":"text","text":"s"}]},"extra":1}`,
	`{"role":"assistant","content":{"parts":[{"type":"text","text":"a"},{"type":"reasoning","text":"because"}]}}`,
	`{"role":"assistant","content":{"parts":[{"type":"tool_call","call":{"id":"c1","name":"search","arguments":{"q":"x"}}}]}}`,
	`{"role":"tool","content":{"parts":[{"type":"tool_result","result":{"call_id":"c1","content":{"parts":[{"type":"text","text":"ok"}]}}}]}}`,
	`{"role":"tool","content":{"parts":[{"type":"text","text":"no result"}]}}`,
	`{"role":"user","content":{"parts":[{"type":"tool_call","call":{"id":"c1","name":"x","arguments":{}}}]}}`,
	`{"role":"assistant","content":{"parts":[{"type":"tool_result","result":{"call_id":"c1","content":{"parts":[{"type":"text","text":"r"}]}}}]}}`,
	`{"role":"user"}`,
	`{"role":"user","content":null}`,
	`{"role":"user","content":{"parts":[]}}`,
	`{"role":"user","content":{"parts":[{"type":"text","text":"hi"}],"intent":"x"}}`,
	`{"role":"user","content":{"parts":[{"type":"text"}]}}`,
	`{"role":"user","content":{"parts":[{"type":"blob"}]}}`,
	`{"role":"user","content":{"parts":[{"type":"text","text":"hi"}]}} x`,
	`[1]`,
	`{"role":5,"content":{"parts":[{"type":"text","text":"hi"}]}}`,
}

// The tool DTO samples. There is no decode here: callers build these values, so
// the rules are `Validate` and the wire.
var toolSamples = []toolRow{
	{Op: "definition", Name: "search", Description: "Find things.", InputSchema: `{"type":"object","properties":{"q":{"type":"string"}}}`},
	{Op: "definition", Name: "search", InputSchema: `{"type":"object"}`},
	{Op: "definition", InputSchema: `{"type":"object"}`},
	{Op: "definition", Name: "x", InputSchema: `[]`},
	{Op: "definition", Name: "x"},

	{Op: "call", ID: "c1", Name: "search", Arguments: `{"q":"x"}`},
	{Op: "call", ID: "c1", Name: "search", Arguments: `{ "q" : "x" }`},
	{Op: "call", Name: "search", Arguments: `{}`},
	{Op: "call", ID: "c1", Name: "search", Arguments: `[1]`},
	{Op: "call", ID: "c1", Name: "search"},

	{Op: "result", CallID: "c1", Text: "ok"},
	{Op: "result", CallID: "c1", Text: "<failed>", IsError: true},
	{Op: "result", Text: "ok"},
	{Op: "result", CallID: "c1", Text: ""},
}

// The schema builder samples. `items`, `properties` and `default` are JSON text
// because core's helpers take a map, an `any` and a `...string`.
var schemaSamples = []schemaRow{
	{Name: "search", Description: "Find things.", Props: []propSample{
		{Kind: "property", Name: "query", Type: "string", Description: "What to look for."},
		{Kind: "property", Name: "limit", Type: "integer", Description: "How many."},
	}, Required: []string{"query"}, Closed: true},
	{Name: "noProps", Description: "Nothing to pass."},
	{Name: "objects", Description: "Nested.", Props: []propSample{
		{Kind: "object", Name: "filter", Description: "A filter.",
			Properties: `{"field":{"type":"string"},"value":{"type":"string"}}`},
	}},
	{Name: "arrays", Description: "Arrays.", Props: []propSample{
		{Kind: "array", Name: "tags", Description: "Tags.", Items: `{"type":"string"}`},
		{Kind: "string_map", Name: "labels", Description: "Labels."},
	}, Required: []string{"tags", "tags", "tags"}},
	{Name: "defaults", Description: "Defaults.", Props: []propSample{
		{Kind: "default", Name: "verbose", Type: "boolean", Description: "Say more.", Default: `true`},
		{Kind: "enum", Name: "mode", Type: "string", Description: "How.", Values: []string{"fast", "thorough"}},
	}},
	{Name: "duplicates", Description: "Two props with one name.", Props: []propSample{
		{Kind: "property", Name: "x", Type: "string", Description: "first"},
		{Kind: "property", Name: "x", Type: "integer", Description: "second"},
	}},
	{Name: "", Description: "No name."},
}
