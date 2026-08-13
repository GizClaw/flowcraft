package media

import "fmt"

// VideoFrame is a timestamped image frame for live multimodal input. Complete
// video-generation request and response types are intentionally out of scope.
type VideoFrame struct {
	Source          ImageSource `json:"source"`
	TimestampMillis int64       `json:"timestamp_millis,omitempty"`
}

func (f VideoFrame) Clone() VideoFrame {
	f.Source = f.Source.Clone()
	return f
}

func (f VideoFrame) Validate() error {
	if err := f.Source.Validate(); err != nil {
		return err
	}
	if f.TimestampMillis < 0 {
		return fmt.Errorf("video frame timestamp must not be negative")
	}
	return nil
}
