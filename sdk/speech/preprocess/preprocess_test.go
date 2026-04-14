package preprocess

import (
	"testing"

	"github.com/GizClaw/flowcraft/sdk/speech/audio"
)

func TestFunc_Process(t *testing.T) {
	f := Func(func(frame audio.Frame) audio.Frame {
		frame.Data = append(frame.Data, 0xFF)
		return frame
	})
	in := audio.Frame{Data: []byte{0x01}}
	out := f.Process(in)
	if len(out.Data) != 2 || out.Data[1] != 0xFF {
		t.Fatal("expected Func to append byte")
	}
}

func TestNoop_Process(t *testing.T) {
	n := Noop{}
	in := audio.Frame{Data: []byte{0x01, 0x02}}
	out := n.Process(in)
	if len(out.Data) != 2 {
		t.Fatal("expected Noop to pass through unchanged")
	}
}

func TestChain_Process(t *testing.T) {
	double := Func(func(f audio.Frame) audio.Frame {
		f.Data = append(f.Data, f.Data...)
		return f
	})
	addByte := Func(func(f audio.Frame) audio.Frame {
		f.Data = append(f.Data, 0xAA)
		return f
	})

	c := NewChain(double, addByte)
	in := audio.Frame{Data: []byte{0x01}}
	out := c.Process(in)

	if len(out.Data) != 3 {
		t.Fatalf("expected 3 bytes (doubled then +1), got %d", len(out.Data))
	}
	if out.Data[2] != 0xAA {
		t.Fatalf("expected last byte 0xAA, got 0x%X", out.Data[2])
	}
}

func TestChain_NilProcessorsFiltered(t *testing.T) {
	n := Noop{}
	c := NewChain(nil, n, nil)
	if len(c.processors) != 1 {
		t.Fatalf("expected 1 processor after nil filtering, got %d", len(c.processors))
	}
}

func TestChain_Empty(t *testing.T) {
	c := NewChain()
	in := audio.Frame{Data: []byte{0x01}}
	out := c.Process(in)
	if len(out.Data) != 1 {
		t.Fatal("expected empty chain to pass through unchanged")
	}
}

func TestChain_NilReceiver(t *testing.T) {
	var c *Chain
	in := audio.Frame{Data: []byte{0x01}}
	out := c.Process(in)
	if len(out.Data) != 1 {
		t.Fatal("expected nil chain to pass through unchanged")
	}
}
