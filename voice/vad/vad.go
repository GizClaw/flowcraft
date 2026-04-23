package vad

import (
	"encoding/binary"
	"math"
)

// VAD detects speech segment boundaries in an audio stream.
// Not safe for concurrent use from multiple goroutines.
type VAD interface {
	Feed(chunk []byte) (segment []byte, isFinal bool)
	Reset()
	Flush() []byte
}

// Classifier scores a single audio chunk for speech likelihood.
// Implementations return a score (higher means more likely speech)
// and a boolean indicating whether the score exceeds the current
// threshold. Session uses this for speech onset and barge-in
// detection instead of raw RMS energy.
type Classifier interface {
	Classify(chunk []byte) (score float64, isSpeech bool)
}

// SampleRateAware is an optional interface that a VAD can implement to
// allow its sample rate to be aligned with the actual audio stream at
// runtime, before any audio is fed.
type SampleRateAware interface {
	SetSampleRate(rate int)
}

// VADOption configures an EnergyVAD.
type VADOption func(*EnergyVAD)

// WithVADThreshold sets a fixed speech-score threshold, bypassing adaptive
// calibration. In spectral mode the score is in [0, 1] (try 0.45); in
// energy-only mode it is the raw RMS value (try 0.01).
func WithVADThreshold(t float64) VADOption { return func(v *EnergyVAD) { v.fixedThreshold = t } }
func WithVADSampleRate(rate int) VADOption { return func(v *EnergyVAD) { v.sampleRate = rate } }
func WithSilenceSamples(n int) VADOption {
	return func(v *EnergyVAD) { v.silenceSamples = n; v.silenceExplicit = true }
}
func WithFinalSamples(n int) VADOption {
	return func(v *EnergyVAD) { v.finalSamples = n; v.finalExplicit = true }
}

// WithSpeechOnset sets how many consecutive loud frames are required to
// confirm speech onset (default 3). Prevents short transient noise from
// triggering speech detection.
func WithSpeechOnset(n int) VADOption { return func(v *EnergyVAD) { v.speechOnset = n } }

// WithNoiseFloorMultiplier sets the adaptive threshold as a multiple of the
// estimated noise-floor score (default 3.0). Ignored when a fixed threshold
// is set.
func WithNoiseFloorMultiplier(m float64) VADOption {
	return func(v *EnergyVAD) { v.noiseMultiplier = m }
}

// WithNoiseFloorAlpha sets the EMA smoothing factor for noise floor
// estimation (default 0.05). Smaller values track slower but are more stable.
func WithNoiseFloorAlpha(a float64) VADOption {
	return func(v *EnergyVAD) { v.noiseAlpha = a }
}

// WithSpectral enables or disables spectral analysis (ZCR, sub-band energy,
// pitch detection). When disabled, falls back to pure RMS energy detection.
// Enabled by default.
func WithSpectral(on bool) VADOption { return func(v *EnergyVAD) { v.spectral = on } }

// EnergyVAD is a PCM16 voice-activity detector that combines RMS energy
// with spectral features (zero-crossing rate, sub-band energy ratio, and
// autocorrelation pitch detection) for accurate speech/noise discrimination.
//
// When spectral mode is disabled (WithSpectral(false)), it falls back to
// pure RMS energy detection — useful for ultra-low-latency or constrained
// environments.
type EnergyVAD struct {
	// config – set via options, immutable after construction
	fixedThreshold  float64 // 0 means adaptive mode
	noiseMultiplier float64
	noiseAlpha      float64
	sampleRate      int
	bytesPerSample  int
	silenceSamples  int
	finalSamples    int
	speechOnset     int
	spectral        bool
	silenceExplicit bool // true when set via WithSilenceSamples
	finalExplicit   bool // true when set via WithFinalSamples

	// runtime state
	buf         []byte
	prefixBuf   []byte // frames buffered during onset confirmation
	silentCount int
	loudCount   int  // consecutive loud frames for onset confirmation
	speaking    bool // currently inside a speech segment
	hasSpeech   bool // at least one segment has been emitted or is in progress
	noiseFloor  float64
	noiseReady  bool
}

func NewEnergyVAD(opts ...VADOption) *EnergyVAD {
	v := &EnergyVAD{
		noiseMultiplier: 3.0,
		noiseAlpha:      0.05,
		sampleRate:      24000,
		bytesPerSample:  2,
		speechOnset:     3,
		spectral:        true,
	}
	for _, o := range opts {
		o(v)
	}
	if v.silenceSamples == 0 {
		v.silenceSamples = v.sampleRate * 300 / 1000
	}
	if v.finalSamples == 0 {
		v.finalSamples = v.sampleRate * 800 / 1000
	}
	if v.speechOnset < 1 {
		v.speechOnset = 1
	}
	return v
}

// Classify implements the Classifier interface. It returns the speech
// likelihood score and whether it exceeds the current threshold.
func (v *EnergyVAD) Classify(chunk []byte) (float64, bool) {
	score := v.classify(chunk)
	return score, score >= v.threshold()
}

// classify returns a score indicating speech likelihood. In spectral mode
// it returns a combined [0, 1] score from RMS, ZCR, sub-band energy and
// pitch; in energy-only mode it returns the raw RMS value.
func (v *EnergyVAD) classify(chunk []byte) float64 {
	rms := PCM16RMS(chunk, v.bytesPerSample)
	if !v.spectral {
		return rms
	}
	samples := pcm16ToFloat32(chunk)
	zcr := zeroCrossingRate(samples)
	band := subbandEnergyRatio(samples, v.sampleRate)
	pitch := pitchConfidence(samples, v.sampleRate)
	return speechScore(rms, zcr, band, pitch)
}

func (v *EnergyVAD) threshold() float64 {
	if v.fixedThreshold > 0 {
		return v.fixedThreshold
	}
	if !v.noiseReady {
		if v.spectral {
			return 0.45
		}
		return 0.01
	}
	t := v.noiseFloor * v.noiseMultiplier
	minT := 0.005
	if v.spectral {
		minT = 0.20
	}
	if t < minT {
		return minT
	}
	return t
}

// updateNoiseFloor updates the noise floor estimate via EMA. Only called
// when the frame is considered non-speech.
func (v *EnergyVAD) updateNoiseFloor(score float64) {
	if !v.noiseReady {
		v.noiseFloor = score
		v.noiseReady = true
		return
	}
	v.noiseFloor = v.noiseAlpha*score + (1-v.noiseAlpha)*v.noiseFloor
}

func (v *EnergyVAD) Feed(chunk []byte) (segment []byte, isFinal bool) {
	score := v.classify(chunk)
	chunkSamples := len(chunk) / v.bytesPerSample
	loud := score >= v.threshold()

	// -- Idle: no speech detected yet --
	if !v.speaking && !v.hasSpeech {
		if loud {
			v.loudCount++
			v.prefixBuf = append(v.prefixBuf, chunk...)
			if v.loudCount >= v.speechOnset {
				v.buf = append(v.buf, v.prefixBuf...)
				v.prefixBuf = v.prefixBuf[:0]
				v.speaking = true
				v.hasSpeech = true
				v.silentCount = 0
			}
			return nil, false
		}
		v.loudCount = 0
		v.prefixBuf = v.prefixBuf[:0]
		v.updateNoiseFloor(score)
		return nil, false
	}

	// -- Waiting for final after an intermediate segment (speaking=false,
	//    hasSpeech=true). New speech resumes the turn; continued silence
	//    accumulates toward finalSamples. --
	if !v.speaking && v.hasSpeech {
		if loud {
			v.buf = append(v.buf, chunk...)
			v.speaking = true
			v.silentCount = 0
			return nil, false
		}
		v.buf = append(v.buf, chunk...)
		v.silentCount += chunkSamples
		if v.silentCount >= v.finalSamples {
			segment = v.drainBuf()
			v.hasSpeech = false
			v.silentCount = 0
			v.loudCount = 0
			return segment, true
		}
		return nil, false
	}

	// -- Active speech (speaking=true) --
	if loud {
		v.buf = append(v.buf, chunk...)
		v.silentCount = 0
		return nil, false
	}

	v.buf = append(v.buf, chunk...)
	v.silentCount += chunkSamples

	if v.silentCount >= v.finalSamples {
		segment = v.drainBuf()
		v.speaking = false
		v.hasSpeech = false
		v.silentCount = 0
		v.loudCount = 0
		return segment, true
	}

	if v.silentCount >= v.silenceSamples {
		segment = v.drainBuf()
		v.speaking = false
		return segment, false
	}

	return nil, false
}

func (v *EnergyVAD) drainBuf() []byte {
	out := make([]byte, len(v.buf))
	copy(out, v.buf)
	v.buf = v.buf[:0]
	return out
}

func (v *EnergyVAD) Reset() {
	v.buf = v.buf[:0]
	v.prefixBuf = v.prefixBuf[:0]
	v.silentCount = 0
	v.loudCount = 0
	v.speaking = false
	v.hasSpeech = false
	// Preserve noiseFloor across resets so calibration is not lost.
}

// SetSampleRate updates the sample rate and recalculates derived thresholds.
// Must be called before any audio is fed. Implements SampleRateAware.
func (v *EnergyVAD) SetSampleRate(rate int) {
	if rate <= 0 {
		return
	}
	v.sampleRate = rate
	if !v.silenceExplicit {
		v.silenceSamples = rate * 300 / 1000
	}
	if !v.finalExplicit {
		v.finalSamples = rate * 800 / 1000
	}
}

func (v *EnergyVAD) Flush() []byte {
	total := len(v.prefixBuf) + len(v.buf)
	if total == 0 {
		return nil
	}
	out := make([]byte, 0, total)
	out = append(out, v.prefixBuf...)
	out = append(out, v.buf...)
	v.prefixBuf = v.prefixBuf[:0]
	v.buf = v.buf[:0]
	return out
}

// PCM16RMS computes the root-mean-square of PCM16LE audio data.
func PCM16RMS(data []byte, bytesPerSample int) float64 {
	if len(data) < bytesPerSample {
		return 0
	}
	samples := len(data) / bytesPerSample
	var sumSq float64
	for i := range samples {
		sample := int16(binary.LittleEndian.Uint16(data[i*bytesPerSample:]))
		normalized := float64(sample) / 32768.0
		sumSq += normalized * normalized
	}
	return math.Sqrt(sumSq / float64(samples))
}
