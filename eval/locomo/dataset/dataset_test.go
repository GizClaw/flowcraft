package dataset

import "testing"

func TestDecodeDynamicFields(t *testing.T) {
	ds, err := Decode([]byte(syntheticJSON()))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got := len(ds.Samples); got != 1 {
		t.Fatalf("samples len = %d, want 1", got)
	}
	s := ds.Samples[0]
	if s.ID != "conv-a" {
		t.Fatalf("sample id = %q", s.ID)
	}
	if got := len(s.Sessions); got != 2 {
		t.Fatalf("sessions len = %d, want 2", got)
	}
	if s.Sessions[0].DateTime != "2024-01-01 09:00" {
		t.Fatalf("session date = %q", s.Sessions[0].DateTime)
	}
	if got := s.Sessions[0].Turns[1].ImgURL; got != "https://example.test/mug.png" {
		t.Fatalf("image url = %q", got)
	}
	if got := s.Sessions[0].Turns[1].BlipCaption; got != "a red mug on a table" {
		t.Fatalf("caption = %q", got)
	}
	if urls, ok := s.Sessions[0].Turns[1].Metadata["img_urls"].([]string); !ok || len(urls) != 2 || urls[1] != "https://example.test/mug-2.png" {
		t.Fatalf("multi image metadata = %#v, want both URLs", s.Sessions[0].Turns[1].Metadata["img_urls"])
	}
	if got := s.QA[0].Category; got != "multi-hop" {
		t.Fatalf("qa category = %q", got)
	}
	if got := s.QA[0].Evidence[0]; got != "d1" {
		t.Fatalf("qa evidence = %q", got)
	}
	if got := len(s.EventSummaries); got != 1 {
		t.Fatalf("events len = %d, want 1", got)
	}
	if got := len(s.DialogCases); got != 1 {
		t.Fatalf("dialog cases len = %d, want 1", got)
	}
	if c := s.DialogCases[0]; c.SourceTurnDiaID != "d2" || c.TargetDiaID != "d3" || c.Gold != "Here is tea in the red mug." {
		t.Fatalf("dialog case = %+v, want image turn to next text turn", c)
	}
}

func TestDecodeTopLevelShapes(t *testing.T) {
	for name, body := range map[string]string{
		"array":         `[` + sampleObject() + `]`,
		"samples":       `{"samples":[` + sampleObject() + `]}`,
		"data":          `{"data":[` + sampleObject() + `]}`,
		"conversations": `{"conversations":[` + sampleObject() + `]}`,
		"single":        sampleObject(),
	} {
		t.Run(name, func(t *testing.T) {
			ds, err := Decode([]byte(body))
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if got := len(ds.Samples); got != 1 {
				t.Fatalf("samples len = %d, want 1", got)
			}
		})
	}
}

func TestDecodeSkipsEventSummaryDateMetadata(t *testing.T) {
	ds, err := Decode([]byte(`{
  "samples": [
    {
      "sample_id": "conv-event-date",
      "conversation": {
        "session_1_date_time": "8:56 pm on 20 July, 2023",
        "session_1": []
      },
      "event_summary": {
        "events_session_1": {
          "date": "20 July, 2023",
          "Ada": ["Ada visited the museum."]
        }
      }
    }
  ]
}`))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	events := ds.Samples[0].EventSummaries
	if len(events) != 1 {
		t.Fatalf("event summaries len = %d, want only real speaker event: %+v", len(events), events)
	}
	if events[0].Speaker != "Ada" {
		t.Fatalf("event summary speaker = %q, want Ada", events[0].Speaker)
	}
	if events[0].Events[0] != "Ada visited the museum." {
		t.Fatalf("event summary events = %+v, want Ada event", events[0].Events)
	}
}

func sampleObject() string {
	return `{
  "sample_id": "conv-a",
  "conversation": {
    "session_1_date_time": "2024-01-01 09:00",
    "session_1": [{"dia_id": "d1", "speaker": "Ada", "text": "Ada likes tea."}]
  }
}`
}

func syntheticJSON() string {
	return `{
  "samples": [
    {
      "sample_id": "conv-a",
      "conversation": {
        "speaker_1": "Ada",
        "speaker_2": "Ben",
        "session_1_date_time": "2024-01-01 09:00",
        "session_1": [
          {"dia_id": "d1", "speaker": "Ada", "text": "Ada likes tea."},
          {"dia_id": "d2", "speaker": "Ben", "text": "What is in the image?", "img_url": ["https://example.test/mug.png", "https://example.test/mug-2.png"], "blip_caption": "a red mug on a table", "query": "What is shown?"},
          {"dia_id": "d3", "speaker": "Ada", "text": "Here is tea in the red mug."}
        ],
        "session_2_date_time": "2024-01-02 09:00",
        "session_2": [
          {"dia_id": "d4", "speaker": "Ben", "text": "Thanks."}
        ]
      },
      "qa": [
        {"question": "What does Ada like?", "answer": "tea", "category": 1, "evidence": ["d1"], "adversarial_answer": "no information available"}
      ],
      "event_summary": {
        "events_session_1": {"Ada": ["Ada likes tea."]}
      }
    }
  ]
}`
}
