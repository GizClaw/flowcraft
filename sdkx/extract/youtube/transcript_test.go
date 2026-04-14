package youtube

import (
	"testing"
)

func TestExtractBalancedJSON(t *testing.T) {
	tests := []struct {
		name    string
		html    string
		varName string
		want    string
	}{
		{
			name:    "simple object",
			html:    `var ytInitialPlayerResponse = {"videoDetails":{"title":"test"}};`,
			varName: "ytInitialPlayerResponse",
			want:    `{"videoDetails":{"title":"test"}}`,
		},
		{
			name:    "nested objects",
			html:    `ytInitialPlayerResponse = {"a":{"b":{"c":1}}};`,
			varName: "ytInitialPlayerResponse",
			want:    `{"a":{"b":{"c":1}}}`,
		},
		{
			name:    "braces inside strings",
			html:    `ytInitialPlayerResponse = {"text":"hello {world}"};`,
			varName: "ytInitialPlayerResponse",
			want:    `{"text":"hello {world}"}`,
		},
		{
			name:    "escaped quotes in strings",
			html:    `ytInitialPlayerResponse = {"text":"say \"hi\""};`,
			varName: "ytInitialPlayerResponse",
			want:    `{"text":"say \"hi\""}`,
		},
		{
			name:    "var not found",
			html:    `var something = {};`,
			varName: "ytInitialPlayerResponse",
			want:    "",
		},
		{
			name:    "empty html",
			html:    "",
			varName: "ytInitialPlayerResponse",
			want:    "",
		},
		{
			name:    "unclosed brace returns empty",
			html:    `ytInitialPlayerResponse = {"open":true`,
			varName: "ytInitialPlayerResponse",
			want:    "",
		},
		{
			name:    "with surrounding content",
			html:    `<script>var x=1; ytInitialPlayerResponse = {"id":"abc123"}; var y=2;</script>`,
			varName: "ytInitialPlayerResponse",
			want:    `{"id":"abc123"}`,
		},
		{
			name:    "whitespace between equals and brace",
			html:    `ytInitialPlayerResponse   =   {"ok":true};`,
			varName: "ytInitialPlayerResponse",
			want:    `{"ok":true}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractBalancedJSON(tt.html, tt.varName)
			if got != tt.want {
				t.Errorf("extractBalancedJSON() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseXMLCaption(t *testing.T) {
	tests := []struct {
		name      string
		data      string
		wantLen   int
		wantFirst TranscriptEntry
		wantErr   bool
	}{
		{
			name: "valid xml captions",
			data: `<?xml version="1.0" encoding="utf-8"?>
<transcript>
<text start="0.5" dur="2.1">Hello world</text>
<text start="2.6" dur="1.5">This is a test</text>
<text start="4.1" dur="3.0">Of the caption system</text>
</transcript>`,
			wantLen:   3,
			wantFirst: TranscriptEntry{Start: 0.5, Duration: 2.1, Text: "Hello world"},
		},
		{
			name:    "empty xml",
			data:    `<?xml version="1.0"?><transcript></transcript>`,
			wantErr: true,
		},
		{
			name:    "no text elements",
			data:    `<root><item>not a caption</item></root>`,
			wantErr: true,
		},
		{
			name:      "single entry",
			data:      `<text start="10.0" dur="5.0">Single entry</text>`,
			wantLen:   1,
			wantFirst: TranscriptEntry{Start: 10.0, Duration: 5.0, Text: "Single entry"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			transcript, err := parseXMLCaption([]byte(tt.data))
			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(transcript.Entries) != tt.wantLen {
				t.Fatalf("got %d entries, want %d", len(transcript.Entries), tt.wantLen)
			}
			e := transcript.Entries[0]
			if e.Start != tt.wantFirst.Start || e.Duration != tt.wantFirst.Duration || e.Text != tt.wantFirst.Text {
				t.Errorf("first entry = %+v, want %+v", e, tt.wantFirst)
			}
		})
	}
}

func TestParseJSON3Caption(t *testing.T) {
	tests := []struct {
		name      string
		data      string
		wantLen   int
		wantFirst TranscriptEntry
		wantErr   bool
	}{
		{
			name: "valid json3",
			data: `{"events":[
				{"tStartMs":"1000","dDurationMs":"2000","segs":[{"utf8":"Hello "},{"utf8":"world"}]},
				{"tStartMs":"3000","dDurationMs":"1500","segs":[{"utf8":"Second line"}]}
			]}`,
			wantLen:   2,
			wantFirst: TranscriptEntry{Start: 1.0, Duration: 2.0, Text: "Hello world"},
		},
		{
			name:    "invalid json",
			data:    `{not json}`,
			wantErr: true,
		},
		{
			name:    "empty events",
			data:    `{"events":[]}`,
			wantLen: 0,
		},
		{
			name:      "event with no segs",
			data:      `{"events":[{"tStartMs":"0","dDurationMs":"1000","segs":[]}]}`,
			wantLen:   1,
			wantFirst: TranscriptEntry{Start: 0, Duration: 1.0, Text: ""},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			transcript, err := parseJSON3Caption([]byte(tt.data))
			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(transcript.Entries) != tt.wantLen {
				t.Fatalf("got %d entries, want %d", len(transcript.Entries), tt.wantLen)
			}
			if tt.wantLen > 0 {
				e := transcript.Entries[0]
				if e.Start != tt.wantFirst.Start || e.Duration != tt.wantFirst.Duration || e.Text != tt.wantFirst.Text {
					t.Errorf("first entry = %+v, want %+v", e, tt.wantFirst)
				}
			}
		})
	}
}

func TestExtractDescriptionFromHTML(t *testing.T) {
	tests := []struct {
		name    string
		html    string
		want    string
		wantErr bool
	}{
		{
			name: "valid description",
			html: `<html>var ytInitialPlayerResponse = {"videoDetails":{"shortDescription":"This is a video about testing.","title":"Test"}};</html>`,
			want: "This is a video about testing.",
		},
		{
			name:    "no player response",
			html:    `<html><body>no player data here</body></html>`,
			wantErr: true,
		},
		{
			name:    "no videoDetails",
			html:    `<html>var ytInitialPlayerResponse = {"captions":{}};</html>`,
			wantErr: true,
		},
		{
			name:    "no shortDescription",
			html:    `<html>var ytInitialPlayerResponse = {"videoDetails":{"title":"Test"}};</html>`,
			wantErr: true,
		},
		{
			name: "description with special chars",
			html: `<html>var ytInitialPlayerResponse = {"videoDetails":{"shortDescription":"Line1\nLine2\nLink: https://example.com"}};</html>`,
			want: "Line1\nLine2\nLink: https://example.com",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := extractDescriptionFromHTML([]byte(tt.html))
			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractYtcfg(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		wantKey string
		wantVal string
		wantErr bool
	}{
		{
			name:    "valid ytcfg",
			data:    `ytcfg.set({INNERTUBE_API_KEY: "AIzaSyTest123", VISITOR_DATA: "visitor123"})`,
			wantKey: "INNERTUBE_API_KEY",
			wantVal: "AIzaSyTest123",
		},
		{
			name:    "single quotes",
			data:    `ytcfg.set({INNERTUBE_API_KEY: 'AIzaSyQuoted'})`,
			wantKey: "INNERTUBE_API_KEY",
			wantVal: "AIzaSyQuoted",
		},
		{
			name:    "no ytcfg",
			data:    `<html><body>nothing here</body></html>`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := extractYtcfg([]byte(tt.data))
			if tt.wantErr {
				if err == nil {
					t.Error("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg[tt.wantKey] != tt.wantVal {
				t.Errorf("cfg[%q] = %q, want %q", tt.wantKey, cfg[tt.wantKey], tt.wantVal)
			}
		})
	}
}

func TestExtractTranscriptParams(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{
			name: "params found",
			data: `"getTranscriptEndpoint": {"params": "CgtTb21lVmlkZW9JZA=="}`,
			want: "CgtTb21lVmlkZW9JZA==",
		},
		{
			name: "no params",
			data: `{"otherEndpoint": {"key": "value"}}`,
			want: "",
		},
		{
			name: "empty data",
			data: "",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractTranscriptParams([]byte(tt.data))
			if got != tt.want {
				t.Errorf("extractTranscriptParams() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseInnertubeTranscript(t *testing.T) {
	tests := []struct {
		name    string
		data    map[string]interface{}
		wantLen int
		wantErr bool
	}{
		{
			name:    "no actions",
			data:    map[string]interface{}{},
			wantErr: true,
		},
		{
			name:    "empty actions",
			data:    map[string]interface{}{"actions": []interface{}{}},
			wantErr: true,
		},
		{
			name: "valid transcript",
			data: map[string]interface{}{
				"actions": []interface{}{
					map[string]interface{}{
						"updateEngagementPanelAction": map[string]interface{}{
							"content": map[string]interface{}{
								"sectionListRenderer": map[string]interface{}{
									"contents": []interface{}{
										map[string]interface{}{
											"transcriptRenderer": map[string]interface{}{
												"header": map[string]interface{}{
													"transcriptHeaderRenderer": map[string]interface{}{
														"title": map[string]interface{}{
															"simpleText": "English",
														},
													},
												},
												"content": map[string]interface{}{
													"transcriptSearchPanelRenderer": map[string]interface{}{
														"lines": []interface{}{
															map[string]interface{}{
																"transcriptSegmentRenderer": map[string]interface{}{
																	"startMs": "1000",
																	"endMs":   "3000",
																	"snippet": map[string]interface{}{
																		"simpleText": "Hello world",
																	},
																},
															},
															map[string]interface{}{
																"transcriptSegmentRenderer": map[string]interface{}{
																	"startMs": "3000",
																	"endMs":   "5000",
																	"snippet": map[string]interface{}{
																		"simpleText": "Second line",
																	},
																},
															},
														},
													},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			wantLen: 2,
		},
		{
			name: "action without engagement panel",
			data: map[string]interface{}{
				"actions": []interface{}{
					map[string]interface{}{"someOtherAction": true},
				},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			transcript, err := parseInnertubeTranscript(tt.data)
			if tt.wantErr {
				if err == nil {
					t.Error("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(transcript.Entries) != tt.wantLen {
				t.Fatalf("got %d entries, want %d", len(transcript.Entries), tt.wantLen)
			}
			if tt.wantLen > 0 {
				if transcript.Language != "English" {
					t.Errorf("language = %q, want English", transcript.Language)
				}
				e := transcript.Entries[0]
				if e.Start != 1.0 {
					t.Errorf("start = %v, want 1.0", e.Start)
				}
				if e.Duration != 2.0 {
					t.Errorf("duration = %v, want 2.0", e.Duration)
				}
				if e.Text != "Hello world" {
					t.Errorf("text = %q, want %q", e.Text, "Hello world")
				}
			}
		})
	}
}

func TestSortCaptionTracks(t *testing.T) {
	tracks := []interface{}{
		map[string]interface{}{"languageCode": "fr", "kind": "asr"},
		map[string]interface{}{"languageCode": "en", "kind": "asr"},
		map[string]interface{}{"languageCode": "de", "kind": ""},
		map[string]interface{}{"languageCode": "en", "kind": ""},
	}

	sorted := sortCaptionTracks(tracks)
	if len(sorted) != 4 {
		t.Fatalf("expected 4 tracks, got %d", len(sorted))
	}

	first := sorted[0].(map[string]interface{})
	if first["languageCode"] != "en" || first["kind"] != "" {
		t.Errorf("first track should be en/manual, got lang=%v kind=%v", first["languageCode"], first["kind"])
	}

	second := sorted[1].(map[string]interface{})
	if second["languageCode"] != "en" || second["kind"] != "asr" {
		t.Errorf("second track should be en/asr, got lang=%v kind=%v", second["languageCode"], second["kind"])
	}
}

func TestFormatAsText(t *testing.T) {
	tests := []struct {
		name       string
		transcript *Transcript
		want       string
	}{
		{
			name:       "nil transcript",
			transcript: nil,
			want:       "",
		},
		{
			name: "multiple entries",
			transcript: &Transcript{
				Entries: []TranscriptEntry{
					{Text: "Hello"},
					{Text: "World"},
				},
			},
			want: "Hello\nWorld\n",
		},
		{
			name: "empty entries",
			transcript: &Transcript{
				Entries: []TranscriptEntry{},
			},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormatAsText(tt.transcript)
			if got != tt.want {
				t.Errorf("FormatAsText() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseKeyValuePairs(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want map[string]string
	}{
		{
			name: "double quotes",
			in:   `{API_KEY: "abc123", CONTEXT: "ctx"}`,
			want: map[string]string{"API_KEY": "abc123", "CONTEXT": "ctx"},
		},
		{
			name: "single quotes",
			in:   `{API_KEY: 'abc123'}`,
			want: map[string]string{"API_KEY": "abc123"},
		},
		{
			name: "empty string",
			in:   `{}`,
			want: map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseKeyValuePairs(tt.in)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("key %q = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}
