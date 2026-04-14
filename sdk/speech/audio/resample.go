package audio

import (
	"encoding/binary"
	"io"
)

// ResamplePCM16 converts PCM16LE audio data from one sample rate to another
// using linear interpolation. channels specifies the number of interleaved
// channels (typically 1 for mono).
//
// If fromRate == toRate the original slice is returned without copying.
func ResamplePCM16(data []byte, fromRate, toRate, channels int) []byte {
	if fromRate == toRate || fromRate <= 0 || toRate <= 0 || channels <= 0 {
		return data
	}

	bytesPerSample := 2
	frameSize := bytesPerSample * channels
	srcFrames := len(data) / frameSize
	if srcFrames < 1 {
		return data
	}

	dstFrames := int(int64(srcFrames) * int64(toRate) / int64(fromRate))
	if dstFrames < 1 {
		return nil
	}

	out := make([]byte, dstFrames*frameSize)
	ratio := float64(srcFrames-1) / float64(dstFrames-1)
	if dstFrames == 1 {
		ratio = 0
	}

	for i := range dstFrames {
		srcPos := ratio * float64(i)
		idx := int(srcPos)
		frac := srcPos - float64(idx)

		nextIdx := idx + 1
		if nextIdx >= srcFrames {
			nextIdx = srcFrames - 1
		}

		for ch := range channels {
			s0 := int16(binary.LittleEndian.Uint16(data[(idx*channels+ch)*bytesPerSample:]))
			s1 := int16(binary.LittleEndian.Uint16(data[(nextIdx*channels+ch)*bytesPerSample:]))
			val := float64(s0) + frac*(float64(s1)-float64(s0))

			if val > 32767 {
				val = 32767
			} else if val < -32768 {
				val = -32768
			}
			binary.LittleEndian.PutUint16(out[(i*channels+ch)*bytesPerSample:], uint16(int16(val)))
		}
	}
	return out
}

// ResampleStream wraps a Stream[Frame] and resamples every PCM16 frame to
// targetRate on the fly. Non-PCM frames or frames already at the target
// rate are passed through unchanged.
func ResampleStream(input Stream[Frame], targetRate int) Stream[Frame] {
	if targetRate <= 0 {
		return input
	}
	p := NewPipe[Frame](16)
	go func() {
		defer p.Close()
		for {
			f, err := input.Read()
			if err != nil {
				if err != io.EOF {
					p.Interrupt()
				}
				return
			}
			if f.Format.Codec == CodecPCM && f.Format.SampleRate > 0 && f.Format.SampleRate != targetRate {
				channels := f.Format.Channels
				if channels <= 0 {
					channels = 1
				}
				f.Data = ResamplePCM16(f.Data, f.Format.SampleRate, targetRate, channels)
				f.Format.SampleRate = targetRate
			}
			if !p.Send(f) {
				return
			}
		}
	}()
	return p
}
