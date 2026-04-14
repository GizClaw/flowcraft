package vad

import (
	"encoding/binary"
	"math"
	"testing"
)

// ---------------------------------------------------------------------------
// PCM16RMS (shared helper)
// ---------------------------------------------------------------------------

func TestPCM16RMS_Zeros(t *testing.T) {
	data := make([]byte, 8)
	if got := PCM16RMS(data, 2); got != 0 {
		t.Errorf("PCM16RMS(zeros) = %v, want 0", got)
	}
}

func TestPCM16RMS_KnownAmplitude(t *testing.T) {
	data := make([]byte, 2)
	binary.LittleEndian.PutUint16(data, 32767)
	if got := PCM16RMS(data, 2); math.Abs(got-1.0) > 1e-4 {
		t.Errorf("PCM16RMS([32767]) = %v, want 1.0", got)
	}
}

func TestPCM16RMS_EmptyChunk(t *testing.T) {
	if got := PCM16RMS(nil, 2); got != 0 {
		t.Errorf("PCM16RMS(nil) = %v, want 0", got)
	}
}

func TestPCM16RMS_NegativeAmplitude(t *testing.T) {
	data := make([]byte, 2)
	binary.LittleEndian.PutUint16(data, 32768)
	if got := PCM16RMS(data, 2); math.Abs(got-1.0) > 1e-4 {
		t.Errorf("PCM16RMS([-32768]) = %v, want 1.0", got)
	}
}

func TestPCM16RMS_MaxSample(t *testing.T) {
	data := make([]byte, 2)
	binary.LittleEndian.PutUint16(data, 32767)
	if got := PCM16RMS(data, 2); math.Abs(got-1.0) > 1e-4 {
		t.Errorf("PCM16RMS(max) = %v, want 1.0", got)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func makePCM16(samples ...int16) []byte {
	b := make([]byte, len(samples)*2)
	for i, s := range samples {
		binary.LittleEndian.PutUint16(b[i*2:], uint16(s))
	}
	return b
}

// makeSineWave generates a PCM16 sine wave at the given frequency and
// amplitude (0-32767), useful for testing spectral features.
func makeSineWave(freq float64, amplitude int16, sampleRate, numSamples int) []byte {
	b := make([]byte, numSamples*2)
	for i := range numSamples {
		val := float64(amplitude) * math.Sin(2*math.Pi*freq*float64(i)/float64(sampleRate))
		s := int16(val)
		binary.LittleEndian.PutUint16(b[i*2:], uint16(s))
	}
	return b
}

// makeWhiteNoise generates pseudo-random noise using a simple LCG,
// deterministic for reproducible tests.
func makeWhiteNoise(amplitude int16, numSamples int) []byte {
	b := make([]byte, numSamples*2)
	state := uint32(12345)
	for i := range numSamples {
		state = state*1103515245 + 12345
		val := int16(float64(int32(state>>16)-16384) * float64(amplitude) / 16384.0)
		binary.LittleEndian.PutUint16(b[i*2:], uint16(val))
	}
	return b
}

// newTestVAD creates an EnergyVAD with spectral disabled and tiny sample
// counts suitable for unit testing of segmentation logic.
func newTestVAD(opts ...VADOption) *EnergyVAD {
	defaults := []VADOption{
		WithVADThreshold(0.001),
		WithVADSampleRate(8000),
		WithSilenceSamples(4),
		WithFinalSamples(8),
		WithSpeechOnset(1),
		WithSpectral(false),
	}
	return NewEnergyVAD(append(defaults, opts...)...)
}

// ---------------------------------------------------------------------------
// Basic behaviour (energy-only mode, onset=1 for backward compat)
// ---------------------------------------------------------------------------

func TestEnergyVAD_SilenceOnly(t *testing.T) {
	v := newTestVAD()
	seg, isFinal := v.Feed(makePCM16(0, 0, 0, 0, 0))
	if seg != nil || isFinal {
		t.Errorf("Feed(silence): segment=%v, isFinal=%v; want nil, false", seg, isFinal)
	}
}

func TestEnergyVAD_SpeechThenSilence(t *testing.T) {
	v := newTestVAD()
	seg, _ := v.Feed(makePCM16(100, 100, 100, 100))
	if seg != nil {
		t.Errorf("Feed(speech): unexpected segment")
	}
	seg, isFinal := v.Feed(makePCM16(0, 0, 0, 0))
	if seg == nil {
		t.Fatal("Feed(silence): expected segment after speech")
	}
	if isFinal {
		t.Error("expected non-final after silenceSamples, got final")
	}
}

func TestEnergyVAD_FinalSegment(t *testing.T) {
	v := newTestVAD(WithSilenceSamples(2), WithFinalSamples(4))
	v.Feed(makePCM16(100, 100))
	seg, isFinal := v.Feed(makePCM16(0, 0, 0, 0, 0, 0))
	if seg == nil {
		t.Fatal("expected final segment")
	}
	if !isFinal {
		t.Error("expected isFinal=true after finalSamples of silence")
	}
}

func TestEnergyVAD_CustomSampleThresholds(t *testing.T) {
	v := newTestVAD(WithSilenceSamples(2), WithFinalSamples(6))
	v.Feed(makePCM16(100, 100))
	segMid, _ := v.Feed(makePCM16(0, 0))
	if segMid == nil {
		t.Fatal("expected intermediate segment after silenceSamples")
	}
	seg, isFinal := v.Feed(makePCM16(0, 0, 0, 0))
	if seg == nil {
		t.Fatal("expected final segment")
	}
	if !isFinal {
		t.Errorf("expected isFinal=true with finalSamples=6")
	}
}

func TestEnergyVAD_Flush(t *testing.T) {
	v := newTestVAD()
	v.Feed(makePCM16(100, 100, 100))
	rest := v.Flush()
	if rest == nil {
		t.Fatal("Flush: expected buffered data")
	}
	if len(rest) != 6 {
		t.Errorf("Flush: got %d bytes, want 6", len(rest))
	}
	if v.Flush() != nil {
		t.Error("second Flush: expected nil")
	}
}

func TestEnergyVAD_FlushEmpty(t *testing.T) {
	v := NewEnergyVAD()
	if v.Flush() != nil {
		t.Error("Flush on empty VAD: expected nil")
	}
}

func TestEnergyVAD_Reset(t *testing.T) {
	v := newTestVAD()
	v.Feed(makePCM16(100, 100))
	v.Reset()
	if v.Flush() != nil {
		t.Error("Flush after Reset: expected nil")
	}
}

func TestEnergyVAD_Options(t *testing.T) {
	v := NewEnergyVAD(
		WithVADThreshold(0.05),
		WithVADSampleRate(48000),
		WithSilenceSamples(100),
		WithFinalSamples(200),
		WithSpeechOnset(5),
		WithNoiseFloorMultiplier(4.0),
		WithNoiseFloorAlpha(0.1),
		WithSpectral(false),
	)
	if v.fixedThreshold != 0.05 {
		t.Errorf("fixedThreshold = %v, want 0.05", v.fixedThreshold)
	}
	if v.sampleRate != 48000 {
		t.Errorf("sampleRate = %v, want 48000", v.sampleRate)
	}
	if v.silenceSamples != 100 {
		t.Errorf("silenceSamples = %v, want 100", v.silenceSamples)
	}
	if v.finalSamples != 200 {
		t.Errorf("finalSamples = %v, want 200", v.finalSamples)
	}
	if v.speechOnset != 5 {
		t.Errorf("speechOnset = %v, want 5", v.speechOnset)
	}
	if v.noiseMultiplier != 4.0 {
		t.Errorf("noiseMultiplier = %v, want 4.0", v.noiseMultiplier)
	}
	if v.noiseAlpha != 0.1 {
		t.Errorf("noiseAlpha = %v, want 0.1", v.noiseAlpha)
	}
	if v.spectral {
		t.Error("spectral should be false")
	}
}

func TestEnergyVAD_DefaultsSpectralOn(t *testing.T) {
	v := NewEnergyVAD()
	if !v.spectral {
		t.Error("spectral should be true by default")
	}
}

func TestEnergyVAD_DefaultSampleThresholds(t *testing.T) {
	v := NewEnergyVAD()
	if v.silenceSamples == 0 {
		t.Error("silenceSamples: expected default, got 0")
	}
	if v.finalSamples == 0 {
		t.Error("finalSamples: expected default, got 0")
	}
}

// ---------------------------------------------------------------------------
// Speech onset confirmation
// ---------------------------------------------------------------------------

func TestEnergyVAD_OnsetConfirmation(t *testing.T) {
	v := newTestVAD(WithSpeechOnset(3))
	speech := makePCM16(100, 100)

	seg, _ := v.Feed(speech)
	if seg != nil {
		t.Fatal("onset should not confirm after 1 frame")
	}
	seg, _ = v.Feed(speech)
	if seg != nil {
		t.Fatal("onset should not confirm after 2 frames")
	}
	if v.speaking {
		t.Fatal("speaking should be false before onset confirmation")
	}

	seg, _ = v.Feed(speech)
	if seg != nil {
		t.Fatal("no segment expected immediately at onset")
	}
	if !v.speaking {
		t.Fatal("speaking should be true after onset confirmation")
	}
}

func TestEnergyVAD_OnsetResetByQuietFrame(t *testing.T) {
	v := newTestVAD(WithSpeechOnset(3))
	v.Feed(makePCM16(100, 100))
	v.Feed(makePCM16(100, 100))
	v.Feed(makePCM16(0, 0))
	v.Feed(makePCM16(100, 100))
	if v.speaking {
		t.Fatal("quiet frame should have reset onset counter")
	}
}

func TestEnergyVAD_FlushIncludesPrefixBuf(t *testing.T) {
	v := newTestVAD(WithSpeechOnset(5))
	v.Feed(makePCM16(100, 100))
	rest := v.Flush()
	if rest == nil {
		t.Fatal("Flush should return prefix buffer")
	}
	if len(rest) != 4 {
		t.Errorf("Flush: got %d bytes, want 4", len(rest))
	}
}

// ---------------------------------------------------------------------------
// Adaptive noise floor (energy-only mode)
// ---------------------------------------------------------------------------

func TestEnergyVAD_AdaptiveThresholdEnergyMode(t *testing.T) {
	v := NewEnergyVAD(
		WithVADSampleRate(8000),
		WithSilenceSamples(4),
		WithFinalSamples(8),
		WithSpeechOnset(1),
		WithNoiseFloorMultiplier(3.0),
		WithSpectral(false),
	)

	if v.threshold() != 0.01 {
		t.Errorf("initial threshold = %v, want 0.01", v.threshold())
	}

	quiet := makePCM16(200, 200, 200, 200)
	for range 20 {
		v.Feed(quiet)
	}

	if !v.noiseReady {
		t.Fatal("noiseReady should be true after feeding quiet frames")
	}
	if v.noiseFloor <= 0 {
		t.Fatal("noiseFloor should be positive after calibration")
	}

	expected := v.noiseFloor * 3.0
	if expected < 0.005 {
		t.Fatalf("test setup error: expected adaptive threshold %v < minThreshold", expected)
	}
	if got := v.threshold(); math.Abs(got-expected) > 1e-6 {
		t.Errorf("adaptive threshold = %v, want %v", got, expected)
	}
}

func TestEnergyVAD_AdaptiveMinThresholdEnergyMode(t *testing.T) {
	v := NewEnergyVAD(WithNoiseFloorMultiplier(0.1), WithSpectral(false))
	v.noiseFloor = 0.001
	v.noiseReady = true
	if got := v.threshold(); got != 0.005 {
		t.Errorf("threshold = %v, want 0.005 (clamped to min)", got)
	}
}

func TestEnergyVAD_AdaptiveMinThresholdSpectralMode(t *testing.T) {
	v := NewEnergyVAD(WithNoiseFloorMultiplier(0.1))
	v.noiseFloor = 0.01
	v.noiseReady = true
	if got := v.threshold(); got != 0.20 {
		t.Errorf("threshold = %v, want 0.20 (spectral min)", got)
	}
}

func TestEnergyVAD_ResetPreservesNoiseFloor(t *testing.T) {
	v := NewEnergyVAD(WithSpeechOnset(1), WithSpectral(false))
	for range 10 {
		v.Feed(makePCM16(3, 3, 3, 3))
	}
	floor := v.noiseFloor
	v.Reset()
	if v.noiseFloor != floor {
		t.Errorf("Reset changed noiseFloor from %v to %v", floor, v.noiseFloor)
	}
	if !v.noiseReady {
		t.Error("Reset cleared noiseReady")
	}
}

func TestEnergyVAD_FixedThresholdDisablesAdaptive(t *testing.T) {
	v := NewEnergyVAD(WithVADThreshold(0.05), WithSpectral(false))
	for range 10 {
		v.Feed(makePCM16(3, 3, 3, 3))
	}
	if got := v.threshold(); got != 0.05 {
		t.Errorf("threshold = %v, want fixed 0.05", got)
	}
}

// ---------------------------------------------------------------------------
// EnergyVAD.SetSampleRate (from resample_test.go)
// ---------------------------------------------------------------------------

func TestEnergyVAD_SetSampleRate(t *testing.T) {
	v := NewEnergyVAD(WithVADSampleRate(24000))
	if v.sampleRate != 24000 {
		t.Fatalf("initial sampleRate = %d", v.sampleRate)
	}
	v.SetSampleRate(16000)
	if v.sampleRate != 16000 {
		t.Errorf("after SetSampleRate: sampleRate = %d, want 16000", v.sampleRate)
	}
	wantSilence := 16000 * 300 / 1000
	if v.silenceSamples != wantSilence {
		t.Errorf("silenceSamples = %d, want %d", v.silenceSamples, wantSilence)
	}
	wantFinal := 16000 * 800 / 1000
	if v.finalSamples != wantFinal {
		t.Errorf("finalSamples = %d, want %d", v.finalSamples, wantFinal)
	}
}

func TestEnergyVAD_SetSampleRate_PreservesExplicit(t *testing.T) {
	v := NewEnergyVAD(
		WithVADSampleRate(24000),
		WithSilenceSamples(500),
		WithFinalSamples(1000),
	)
	v.SetSampleRate(16000)
	if v.silenceSamples != 500 {
		t.Errorf("silenceSamples = %d, want 500 (user-explicit)", v.silenceSamples)
	}
	if v.finalSamples != 1000 {
		t.Errorf("finalSamples = %d, want 1000 (user-explicit)", v.finalSamples)
	}
	if v.sampleRate != 16000 {
		t.Errorf("sampleRate = %d, want 16000", v.sampleRate)
	}
}

func TestEnergyVAD_SetSampleRate_RecalcsDefaults(t *testing.T) {
	v := NewEnergyVAD(WithVADSampleRate(24000))
	v.SetSampleRate(16000)
	wantSilence := 16000 * 300 / 1000
	if v.silenceSamples != wantSilence {
		t.Errorf("silenceSamples = %d, want %d (recalculated)", v.silenceSamples, wantSilence)
	}
	wantFinal := 16000 * 800 / 1000
	if v.finalSamples != wantFinal {
		t.Errorf("finalSamples = %d, want %d (recalculated)", v.finalSamples, wantFinal)
	}
}

func TestEnergyVAD_SetSampleRate_Zero(t *testing.T) {
	v := NewEnergyVAD(WithVADSampleRate(24000))
	v.SetSampleRate(0)
	if v.sampleRate != 24000 {
		t.Errorf("SetSampleRate(0) should be no-op, got %d", v.sampleRate)
	}
}

// ---------------------------------------------------------------------------
// EnergyVAD.Classify (from resample_test.go)
// ---------------------------------------------------------------------------

func TestEnergyVAD_Classify_Speech(t *testing.T) {
	v := NewEnergyVAD(WithVADSampleRate(16000), WithVADThreshold(0.40))
	sine := makePCM16SineForVADTest(200, 16000, 512, 1)
	score, isSpeech := v.Classify(sine)
	if !isSpeech {
		t.Errorf("200Hz sine: isSpeech=false, score=%v", score)
	}
	if score < 0.4 {
		t.Errorf("200Hz sine: score=%v, want >= 0.4", score)
	}
}

func TestEnergyVAD_Classify_Silence(t *testing.T) {
	v := NewEnergyVAD(WithVADSampleRate(16000), WithVADThreshold(0.40))
	silence := make([]byte, 1024)
	score, isSpeech := v.Classify(silence)
	if isSpeech {
		t.Errorf("silence: isSpeech=true, score=%v", score)
	}
}

func TestEnergyVAD_Classify_AdaptiveThreshold(t *testing.T) {
	v := NewEnergyVAD(WithVADSampleRate(16000), WithSpectral(false))
	quiet := make([]byte, 320)
	for range 10 {
		v.Classify(quiet)
	}
	// After calibration, silence should still not trigger
	_, isSpeech := v.Classify(quiet)
	if isSpeech {
		t.Error("quiet frames should not trigger after adaptive calibration")
	}
}

func makePCM16SineForVADTest(freq float64, sampleRate, numSamples, channels int) []byte {
	data := make([]byte, numSamples*channels*2)
	for i := range numSamples {
		val := int16(16000 * math.Sin(2*math.Pi*freq*float64(i)/float64(sampleRate)))
		for ch := range channels {
			binary.LittleEndian.PutUint16(data[(i*channels+ch)*2:], uint16(val))
		}
	}
	return data
}

// ---------------------------------------------------------------------------
// Spectral classification tests
// ---------------------------------------------------------------------------

func TestClassify_SilenceScoresLow(t *testing.T) {
	v := NewEnergyVAD(WithVADSampleRate(16000))
	silence := makePCM16(make([]int16, 512)...)
	score := v.classify(silence)
	if score > 0.1 {
		t.Errorf("silence score = %v, want < 0.1", score)
	}
}

func TestClassify_SpeechSineScoresHigh(t *testing.T) {
	v := NewEnergyVAD(WithVADSampleRate(16000))
	sine := makeSineWave(200, 8000, 16000, 512)
	score := v.classify(sine)
	if score < 0.4 {
		t.Errorf("200Hz sine score = %v, want >= 0.4", score)
	}
}

func TestClassify_HighFreqSineScoresLower(t *testing.T) {
	v := NewEnergyVAD(WithVADSampleRate(16000))
	speech := makeSineWave(200, 8000, 16000, 512)
	highFreq := makeSineWave(6000, 8000, 16000, 512)

	speechScore := v.classify(speech)
	noiseScore := v.classify(highFreq)

	if noiseScore >= speechScore {
		t.Errorf("6kHz sine score (%v) should be lower than 200Hz (%v)", noiseScore, speechScore)
	}
}

func TestClassify_WhiteNoiseVsSpeech(t *testing.T) {
	v := NewEnergyVAD(WithVADSampleRate(16000))
	sine := makeSineWave(150, 6000, 16000, 512)
	noise := makeWhiteNoise(6000, 512)

	speechS := v.classify(sine)
	noiseS := v.classify(noise)
	if noiseS >= speechS {
		t.Errorf("white noise score (%v) should be lower than speech-like sine (%v)", noiseS, speechS)
	}
}

func TestClassify_EnergyOnlyFallback(t *testing.T) {
	v := NewEnergyVAD(WithVADSampleRate(16000), WithSpectral(false))
	sine := makeSineWave(200, 8000, 16000, 512)
	score := v.classify(sine)
	rms := PCM16RMS(sine, 2)
	if math.Abs(score-rms) > 1e-10 {
		t.Errorf("energy-only classify = %v, want RMS = %v", score, rms)
	}
}

// ---------------------------------------------------------------------------
// Spectral mode segmentation (integration)
// ---------------------------------------------------------------------------

func TestEnergyVAD_SpectralDetectsSpeech(t *testing.T) {
	v := NewEnergyVAD(
		WithVADSampleRate(16000),
		WithSilenceSamples(256),
		WithFinalSamples(512),
		WithSpeechOnset(1),
		WithVADThreshold(0.40),
	)

	// Feed speech-like sine at 200Hz
	speech := makeSineWave(200, 8000, 16000, 512)
	v.Feed(speech)
	if !v.speaking {
		t.Fatal("spectral VAD should detect 200Hz sine as speech")
	}

	// Feed silence to trigger segment
	silence := makePCM16(make([]int16, 512)...)
	var gotSegment bool
	for range 5 {
		seg, _ := v.Feed(silence)
		if seg != nil {
			gotSegment = true
			break
		}
	}
	if !gotSegment {
		t.Fatal("expected segment after silence following speech")
	}
}

func TestEnergyVAD_SpectralRejectsNoise(t *testing.T) {
	// A 7kHz sine at moderate amplitude should score meaningfully lower
	// than a 200Hz speech-like sine at the same amplitude.
	v := NewEnergyVAD(WithVADSampleRate(16000))
	speech := makeSineWave(200, 4000, 16000, 512)
	noise := makeSineWave(7000, 4000, 16000, 512)

	speechS := v.classify(speech)
	noiseS := v.classify(noise)
	if noiseS >= speechS {
		t.Errorf("7kHz noise score (%v) should be lower than 200Hz speech (%v)", noiseS, speechS)
	}

	// With a threshold set between the two scores, only speech triggers
	mid := (speechS + noiseS) / 2
	v2 := NewEnergyVAD(
		WithVADSampleRate(16000),
		WithSilenceSamples(256),
		WithFinalSamples(512),
		WithSpeechOnset(2),
		WithVADThreshold(mid),
	)

	for range 5 {
		v2.Feed(noise)
	}
	if v2.speaking {
		t.Error("spectral VAD should reject 7kHz sine at threshold midpoint")
	}

	for range 5 {
		v2.Feed(speech)
	}
	if !v2.speaking {
		t.Error("spectral VAD should accept 200Hz sine at threshold midpoint")
	}
}

// ---------------------------------------------------------------------------
// DSP helper unit tests
// ---------------------------------------------------------------------------

func TestZeroCrossingRate_Silence(t *testing.T) {
	samples := make([]float64, 100)
	zcr := zeroCrossingRate(samples)
	if zcr != 0 {
		t.Errorf("ZCR(silence) = %v, want 0", zcr)
	}
}

func TestZeroCrossingRate_MaxCrossing(t *testing.T) {
	samples := make([]float64, 100)
	for i := range samples {
		if i%2 == 0 {
			samples[i] = 1
		} else {
			samples[i] = -1
		}
	}
	zcr := zeroCrossingRate(samples)
	if math.Abs(zcr-1.0) > 1e-10 {
		t.Errorf("ZCR(alternating) = %v, want 1.0", zcr)
	}
}

func TestZeroCrossingRate_LowFreqSine(t *testing.T) {
	n := 1000
	samples := make([]float64, n)
	for i := range n {
		samples[i] = math.Sin(2 * math.Pi * 100 * float64(i) / 16000)
	}
	zcr := zeroCrossingRate(samples)
	// 100Hz at 16kHz → ~12.5 crossings per 1000 samples
	if zcr < 0.01 || zcr > 0.05 {
		t.Errorf("ZCR(100Hz sine) = %v, want ~0.01-0.03", zcr)
	}
}

func TestSubbandEnergyRatio_SpeechBand(t *testing.T) {
	n := 512
	sr := 16000
	samples := make([]float64, n)
	for i := range n {
		samples[i] = math.Sin(2 * math.Pi * 500 * float64(i) / float64(sr))
	}
	ratio := subbandEnergyRatio(samples, sr)
	if ratio < 0.8 {
		t.Errorf("500Hz sine band ratio = %v, want > 0.8", ratio)
	}
}

func TestSubbandEnergyRatio_OutOfBand(t *testing.T) {
	n := 512
	sr := 16000
	samples := make([]float64, n)
	for i := range n {
		samples[i] = math.Sin(2 * math.Pi * 6000 * float64(i) / float64(sr))
	}
	ratio := subbandEnergyRatio(samples, sr)
	if ratio > 0.2 {
		t.Errorf("6kHz sine band ratio = %v, want < 0.2", ratio)
	}
}

func TestSubbandEnergyRatio_EmptySamples(t *testing.T) {
	ratio := subbandEnergyRatio(nil, 16000)
	if ratio != 0 {
		t.Errorf("empty samples band ratio = %v, want 0", ratio)
	}
}

func TestPitchConfidence_PureTone(t *testing.T) {
	n := 1024
	sr := 16000
	samples := make([]float64, n)
	for i := range n {
		samples[i] = math.Sin(2 * math.Pi * 200 * float64(i) / float64(sr))
	}
	conf := pitchConfidence(samples, sr)
	if conf < 0.7 {
		t.Errorf("200Hz tone pitch confidence = %v, want > 0.7", conf)
	}
}

func TestPitchConfidence_Noise(t *testing.T) {
	n := 1024
	samples := make([]float64, n)
	state := uint32(99999)
	for i := range n {
		state = state*1103515245 + 12345
		samples[i] = float64(int32(state>>16)-16384) / 16384.0
	}
	conf := pitchConfidence(samples, 16000)
	if conf > 0.5 {
		t.Errorf("noise pitch confidence = %v, want < 0.5", conf)
	}
}

func TestPitchConfidence_Silence(t *testing.T) {
	samples := make([]float64, 512)
	conf := pitchConfidence(samples, 16000)
	if conf != 0 {
		t.Errorf("silence pitch confidence = %v, want 0", conf)
	}
}

func TestSpeechScore_Ranges(t *testing.T) {
	// Silence: low everything
	s := speechScore(0, 0, 0, 0)
	if s > 0.15 {
		t.Errorf("silence speechScore = %v, want < 0.15", s)
	}

	// Typical speech: moderate RMS, low ZCR, high band ratio, high pitch
	s = speechScore(0.05, 0.08, 0.8, 0.8)
	if s < 0.5 {
		t.Errorf("speech-like speechScore = %v, want > 0.5", s)
	}

	// High-energy noise: high RMS, high ZCR, low band ratio, no pitch
	s = speechScore(0.1, 0.5, 0.2, 0.1)
	if s > 0.6 {
		t.Errorf("noise-like speechScore = %v, want < 0.6", s)
	}
}

func TestFFT_KnownSignal(t *testing.T) {
	n := 64
	samples := make([]float64, n)
	for i := range n {
		samples[i] = math.Cos(2 * math.Pi * float64(i) / float64(n))
	}
	spectrum := realFFT(samples)
	// DC should be ~0, bin 1 should have the peak
	dc := cmplxMag(spectrum[0])
	peak := cmplxMag(spectrum[1])
	if dc > 0.1 {
		t.Errorf("DC magnitude = %v, want ~0", dc)
	}
	if peak < float64(n)/2-1 {
		t.Errorf("bin 1 magnitude = %v, want ~%v", peak, float64(n)/2)
	}
}

func TestFFT_AllZeros(t *testing.T) {
	spectrum := realFFT(make([]float64, 32))
	for i, c := range spectrum {
		if cmplxMag(c) > 1e-10 {
			t.Errorf("FFT of zeros: bin %d = %v, want 0", i, c)
		}
	}
}

func cmplxMag(c complex128) float64 {
	return math.Sqrt(real(c)*real(c) + imag(c)*imag(c))
}

// ---------------------------------------------------------------------------
// Benchmarks
// ---------------------------------------------------------------------------

func BenchmarkEnergyVAD_Feed_EnergyOnly(b *testing.B) {
	for _, frameMs := range []int{20, 30, 100} {
		b.Run(frameMsName(frameMs), func(b *testing.B) {
			sr := 16000
			n := sr * frameMs / 1000
			chunk := makeSineWave(200, 8000, sr, n)
			v := NewEnergyVAD(
				WithVADSampleRate(sr),
				WithVADThreshold(0.01),
				WithSpectral(false),
			)
			b.ResetTimer()
			b.ReportAllocs()
			for range b.N {
				v.Feed(chunk)
			}
		})
	}
}

func BenchmarkEnergyVAD_Feed_Spectral(b *testing.B) {
	for _, frameMs := range []int{20, 30, 100} {
		b.Run(frameMsName(frameMs), func(b *testing.B) {
			sr := 16000
			n := sr * frameMs / 1000
			chunk := makeSineWave(200, 8000, sr, n)
			v := NewEnergyVAD(
				WithVADSampleRate(sr),
				WithVADThreshold(0.45),
				WithSpectral(true),
			)
			b.ResetTimer()
			b.ReportAllocs()
			for range b.N {
				v.Feed(chunk)
			}
		})
	}
}

func BenchmarkClassify_EnergyOnly(b *testing.B) {
	sr := 16000
	chunk := makeSineWave(200, 8000, sr, 512)
	v := NewEnergyVAD(WithVADSampleRate(sr), WithSpectral(false))
	b.ResetTimer()
	b.ReportAllocs()
	for range b.N {
		v.classify(chunk)
	}
}

func BenchmarkClassify_Spectral(b *testing.B) {
	sr := 16000
	chunk := makeSineWave(200, 8000, sr, 512)
	v := NewEnergyVAD(WithVADSampleRate(sr), WithSpectral(true))
	b.ResetTimer()
	b.ReportAllocs()
	for range b.N {
		v.classify(chunk)
	}
}

func BenchmarkFFT_512(b *testing.B) {
	samples := make([]float64, 512)
	for i := range samples {
		samples[i] = math.Sin(2 * math.Pi * 200 * float64(i) / 16000)
	}
	b.ResetTimer()
	b.ReportAllocs()
	for range b.N {
		realFFT(samples)
	}
}

func BenchmarkPitchConfidence_512(b *testing.B) {
	samples := make([]float64, 512)
	for i := range samples {
		samples[i] = math.Sin(2 * math.Pi * 200 * float64(i) / 16000)
	}
	b.ResetTimer()
	b.ReportAllocs()
	for range b.N {
		pitchConfidence(samples, 16000)
	}
}

func frameMsName(ms int) string {
	return string(rune('0'+ms/100)) + string(rune('0'+(ms/10)%10)) + string(rune('0'+ms%10)) + "ms"
}
