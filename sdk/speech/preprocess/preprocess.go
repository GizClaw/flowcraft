package preprocess

import "github.com/GizClaw/flowcraft/sdk/speech/audio"

type Processor interface {
	Process(audio.Frame) audio.Frame
}

type Func func(audio.Frame) audio.Frame

func (f Func) Process(frame audio.Frame) audio.Frame {
	return f(frame)
}

type Chain struct {
	processors []Processor
}

func NewChain(processors ...Processor) *Chain {
	filtered := make([]Processor, 0, len(processors))
	for _, processor := range processors {
		if processor != nil {
			filtered = append(filtered, processor)
		}
	}
	return &Chain{processors: filtered}
}

func (c *Chain) Process(frame audio.Frame) audio.Frame {
	if c == nil {
		return frame
	}
	for _, processor := range c.processors {
		frame = processor.Process(frame)
	}
	return frame
}

type Noop struct{}

func (Noop) Process(frame audio.Frame) audio.Frame { return frame }
