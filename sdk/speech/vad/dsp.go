package vad

import (
	"encoding/binary"
	"math"
	"math/cmplx"
)

// pcm16ToFloat32 decodes PCM16LE bytes into normalized [-1, 1] float64 samples.
func pcm16ToFloat32(data []byte) []float64 {
	n := len(data) / 2
	out := make([]float64, n)
	for i := range n {
		out[i] = float64(int16(binary.LittleEndian.Uint16(data[i*2:]))) / 32768.0
	}
	return out
}

// zeroCrossingRate returns the fraction of adjacent sample pairs that cross
// zero. Speech typically has ZCR in [0.02, 0.20]; unvoiced fricatives and
// white noise tend to be higher.
func zeroCrossingRate(samples []float64) float64 {
	if len(samples) < 2 {
		return 0
	}
	var crossings int
	for i := 1; i < len(samples); i++ {
		if (samples[i] >= 0) != (samples[i-1] >= 0) {
			crossings++
		}
	}
	return float64(crossings) / float64(len(samples)-1)
}

// subbandEnergyRatio computes the fraction of total spectral energy that
// falls within the speech band (roughly 300-3400 Hz). A high ratio indicates
// the signal is likely voiced speech rather than broadband noise.
func subbandEnergyRatio(samples []float64, sampleRate int) float64 {
	spectrum := realFFT(samples)
	n := len(spectrum)
	if n == 0 {
		return 0
	}

	binHz := float64(sampleRate) / float64(len(samples))
	lowBin := int(math.Round(300.0 / binHz))
	highBin := int(math.Round(3400.0 / binHz))
	if lowBin < 0 {
		lowBin = 0
	}
	if highBin > n {
		highBin = n
	}

	var total, band float64
	for i, c := range spectrum {
		mag2 := real(c)*real(c) + imag(c)*imag(c)
		total += mag2
		if i >= lowBin && i < highBin {
			band += mag2
		}
	}
	if total == 0 {
		return 0
	}
	return band / total
}

// pitchConfidence estimates the likelihood that the frame contains a periodic
// (voiced) signal by computing the normalized autocorrelation peak in the
// plausible fundamental frequency range (85-400 Hz). Returns a value in [0,1].
//
// Uses FFT-based autocorrelation: R(τ) = IFFT(|FFT(x)|²), which reduces
// complexity from O(n·maxLag) to O(n·log n).
func pitchConfidence(samples []float64, sampleRate int) float64 {
	if len(samples) < 2 {
		return 0
	}

	minLag := sampleRate / 400
	maxLag := sampleRate / 85
	if maxLag >= len(samples) {
		maxLag = len(samples) - 1
	}
	if minLag >= maxLag {
		return 0
	}

	n := nextPow2(len(samples) * 2)
	buf := make([]complex128, n)
	for i, s := range samples {
		buf[i] = complex(s, 0)
	}

	fft(buf)

	// Power spectrum → autocorrelation via inverse FFT
	for i, c := range buf {
		buf[i] = complex(real(c)*real(c)+imag(c)*imag(c), 0)
	}
	ifft(buf)

	r0 := real(buf[0])
	if r0 == 0 {
		return 0
	}

	var bestR float64
	for lag := minLag; lag <= maxLag; lag++ {
		r := real(buf[lag])
		if r > bestR {
			bestR = r
		}
	}

	conf := bestR / r0
	if conf < 0 {
		return 0
	}
	if conf > 1 {
		return 1
	}
	return conf
}

// speechScore combines multiple features into a single [0, 1] score
// indicating the probability that a frame contains voiced speech.
//
// Weights are tuned for typical voice-pipeline conditions (close-talk
// microphone, 16-24 kHz sample rate).
func speechScore(rms float64, zcr float64, bandRatio float64, pitch float64) float64 {
	// RMS: high energy is a strong speech indicator. Map through a sigmoid
	// centered around a "typical speech" level.
	rmsScore := sigmoid(rms, 0.02, 200)

	// ZCR: voiced speech usually has moderate ZCR (0.02-0.20). Very high
	// ZCR suggests unvoiced noise; very low suggests silence.
	var zcrScore float64
	switch {
	case zcr < 0.01:
		zcrScore = 0.1
	case zcr <= 0.25:
		zcrScore = 1.0 - math.Abs(zcr-0.10)/0.25
	default:
		zcrScore = 0.2
	}
	if zcrScore < 0 {
		zcrScore = 0
	}

	// Band ratio and pitch are direct indicators.
	bandScore := bandRatio
	pitchScore := pitch

	const (
		wRMS   = 0.30
		wZCR   = 0.15
		wBand  = 0.25
		wPitch = 0.30
	)
	return wRMS*rmsScore + wZCR*zcrScore + wBand*bandScore + wPitch*pitchScore
}

func sigmoid(x, center, steepness float64) float64 {
	return 1.0 / (1.0 + math.Exp(-steepness*(x-center)))
}

// --------------------------------------------------------------------
// Minimal radix-2 FFT (pure Go, no external dependencies)
// --------------------------------------------------------------------

// realFFT computes the FFT of real-valued samples and returns the positive-
// frequency half of the spectrum (N/2+1 bins). The input is zero-padded to
// the next power of two if necessary.
func realFFT(samples []float64) []complex128 {
	n := nextPow2(len(samples))
	buf := make([]complex128, n)
	for i, s := range samples {
		buf[i] = complex(s, 0)
	}
	fft(buf)
	return buf[:n/2+1]
}

// ifft computes the inverse FFT in-place by conjugating, running the
// forward FFT, conjugating again, and scaling by 1/N.
func ifft(a []complex128) {
	n := len(a)
	for i := range a {
		a[i] = complex(real(a[i]), -imag(a[i]))
	}
	fft(a)
	invN := 1.0 / float64(n)
	for i := range a {
		a[i] = complex(real(a[i])*invN, -imag(a[i])*invN)
	}
}

func fft(a []complex128) {
	n := len(a)
	if n <= 1 {
		return
	}
	// bit-reversal permutation
	for i, j := 1, 0; i < n; i++ {
		bit := n >> 1
		for ; j&bit != 0; bit >>= 1 {
			j ^= bit
		}
		j ^= bit
		if i < j {
			a[i], a[j] = a[j], a[i]
		}
	}
	// Cooley-Tukey iterative
	for size := 2; size <= n; size <<= 1 {
		half := size >> 1
		wn := cmplx.Exp(complex(0, -2*math.Pi/float64(size)))
		for start := 0; start < n; start += size {
			w := complex(1, 0)
			for k := 0; k < half; k++ {
				u := a[start+k]
				t := w * a[start+k+half]
				a[start+k] = u + t
				a[start+k+half] = u - t
				w *= wn
			}
		}
	}
}

func nextPow2(n int) int {
	p := 1
	for p < n {
		p <<= 1
	}
	return p
}
