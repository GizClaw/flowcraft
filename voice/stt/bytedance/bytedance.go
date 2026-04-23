package bytedance

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"

	"github.com/GizClaw/flowcraft/voice/audio"
	"github.com/GizClaw/flowcraft/voice/stt"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
)

const (
	defaultHost  = "openspeech.bytedance.com"
	defaultModel = "bigmodel"
)

func init() {
	stt.RegisterSTT("bytedance", func(apiKey, baseURL string, opts ...stt.STTProviderOption) (stt.STT, error) {
		var sttOpts []STTOption
		if apiKey != "" {
			sttOpts = append(sttOpts, WithToken(apiKey))
		}
		if baseURL != "" {
			sttOpts = append(sttOpts, WithHost(baseURL))
		}
		for _, o := range opts {
			if opt, ok := o.(STTOption); ok {
				sttOpts = append(sttOpts, opt)
			}
		}
		return New(sttOpts...)
	})
}

// STTOption configures a ByteDance STT instance.
type STTOption func(*STT)

func (o STTOption) ApplySTTProvider(target any) {
	if s, ok := target.(*STT); ok {
		o(s)
	}
}

func WithAppID(id string) STTOption  { return func(s *STT) { s.appID = id } }
func WithToken(tok string) STTOption { return func(s *STT) { s.token = tok } }
func WithHost(host string) STTOption { return func(s *STT) { s.host = host } }
func WithModel(m string) STTOption   { return func(s *STT) { s.model = m } }
func WithUID(uid string) STTOption   { return func(s *STT) { s.uid = uid } }

// EnableNonstream turns on two-pass recognition: streaming partial results
// followed by a non-streaming re-recognition per sentence for higher accuracy.
func EnableNonstream() STTOption { return func(s *STT) { s.nonstream = true } }

// WithEndWindow sets the default silence duration (ms) that triggers a sentence boundary.
// Default 800, minimum 200. For per-call control, use EndWindow() instead.
func WithEndWindow(ms int) STTOption { return func(s *STT) { s.endWindow = ms } }

// ---------------------------------------------------------------------------
// Per-call STTOptions (via Extra map)
// ---------------------------------------------------------------------------

const (
	KeyEndWindow  = "bytedance.end_window"
	KeyNonstream  = "bytedance.nonstream"
	KeyEnableITN  = "bytedance.enable_itn"
	KeyEnablePUNC = "bytedance.enable_punc"
	KeyResultType = "bytedance.result_type"
)

// EndWindow returns a per-call STTOption that overrides the instance-level end window (ms).
func EndWindow(ms int) stt.STTOption { return stt.WithExtra(KeyEndWindow, ms) }

// Nonstream returns a per-call STTOption that enables/disables two-pass recognition.
func Nonstream(enable bool) stt.STTOption { return stt.WithExtra(KeyNonstream, enable) }

// EnableITN returns a per-call STTOption that enables/disables inverse text normalization.
func EnableITN(enable bool) stt.STTOption { return stt.WithExtra(KeyEnableITN, enable) }

// EnablePUNC returns a per-call STTOption that enables/disables punctuation.
func EnablePUNC(enable bool) stt.STTOption { return stt.WithExtra(KeyEnablePUNC, enable) }

// ResultType returns a per-call STTOption that sets the result type (e.g. "full", "single").
func ResultType(t string) stt.STTOption { return stt.WithExtra(KeyResultType, t) }

// STT implements voice/stt.STT and voice/stt.StreamSTT for ByteDance Volcano ASR.
type STT struct {
	appID     string
	token     string
	host      string
	model     string
	uid       string
	nonstream bool
	endWindow int
}

func New(opts ...STTOption) (*STT, error) {
	s := &STT{
		host:      defaultHost,
		model:     defaultModel,
		endWindow: 800,
	}
	for _, o := range opts {
		o(s)
	}
	if s.token == "" {
		return nil, fmt.Errorf("bytedance stt: token is required")
	}
	if s.appID == "" {
		return nil, fmt.Errorf("bytedance stt: app id is required")
	}
	return s, nil
}

// Recognize performs one-shot recognition by opening a WebSocket session,
// sending all audio, and collecting the final transcript.
func (s *STT) Recognize(ctx context.Context, input audio.Frame, opts ...stt.STTOption) (stt.STTResult, error) {
	o := stt.ApplySTTOptions(opts...)
	sess, err := dialASR(ctx, s.appID, s.token, s.host, false)
	if err != nil {
		return stt.STTResult{}, err
	}
	defer func() { _ = sess.close() }()

	if err := sess.sendFullRequest(ctx, s.buildASRRequest(o, input.Format)); err != nil {
		return stt.STTResult{}, fmt.Errorf("bytedance stt: send request: %w", err)
	}

	audioData := input.Data
	const chunkSize = 3200 // 100ms at 16kHz 16bit mono
	for i := 0; i < len(audioData); i += chunkSize {
		select {
		case <-ctx.Done():
			return stt.STTResult{}, ctx.Err()
		default:
		}
		end := min(i+chunkSize, len(audioData))
		if err := sess.sendAudio(ctx, audioData[i:end]); err != nil {
			return stt.STTResult{}, fmt.Errorf("bytedance stt: send audio: %w", err)
		}
	}

	if err := sess.sendFinish(ctx); err != nil {
		return stt.STTResult{}, fmt.Errorf("bytedance stt: send finish: %w", err)
	}

	// Collect results; only keep the last non-empty text to avoid duplication
	// from intermediate partial results.
	var lastText string
	for {
		resp, err := sess.read(ctx)
		if err != nil {
			return stt.STTResult{}, fmt.Errorf("bytedance stt: read: %w", err)
		}
		if resp.code != 0 {
			errMsg := fmt.Sprintf("code %d", resp.code)
			if resp.payload != nil && resp.payload.Error != "" {
				errMsg = resp.payload.Error
			}
			return stt.STTResult{}, fmt.Errorf("bytedance stt: server error: %s", errMsg)
		}
		if resp.payload != nil && resp.payload.Result.Text != "" {
			lastText = resp.payload.Result.Text
		}
		if resp.isLast {
			break
		}
	}

	return stt.STTResult{
		Text:    lastText,
		IsFinal: true,
		Lang:    o.Language,
		Audio:   input,
	}, nil
}

// RecognizeStream implements stt.StreamSTT. It opens a WebSocket session and
// streams audio chunks as they arrive, emitting partial and final results.
func (s *STT) RecognizeStream(
	ctx context.Context,
	input audio.Stream[audio.Frame],
	opts ...stt.STTOption,
) (audio.Stream[stt.STTResult], error) {
	o := stt.ApplySTTOptions(opts...)

	// Read first frame to get format for ASR request
	firstFrame, firstErr := input.Read()
	if firstErr != nil && firstErr != io.EOF {
		return nil, fmt.Errorf("bytedance stt: read first frame: %w", firstErr)
	}
	format := firstFrame.Format
	if format.SampleRate <= 0 {
		format.SampleRate = 16000
	}
	if format.Channels <= 0 {
		format.Channels = 1
	}

	sess, err := dialASR(ctx, s.appID, s.token, s.host, true)
	if err != nil {
		return nil, err
	}

	if err := sess.sendFullRequest(ctx, s.buildASRRequest(o, format)); err != nil {
		_ = sess.close()
		return nil, fmt.Errorf("bytedance stt: send request: %w", err)
	}

	out := audio.NewPipe[stt.STTResult](8)

	go func() {
		defer out.Close()

		innerCtx, innerCancel := context.WithCancel(ctx)
		defer innerCancel()

		writerDone := make(chan error, 1)
		go func() {
			// Send first frame if we got one
			if firstErr != io.EOF && len(firstFrame.Data) > 0 {
				if err := sess.sendAudio(innerCtx, firstFrame.Data); err != nil {
					writerDone <- err
					return
				}
			}
			for {
				select {
				case <-innerCtx.Done():
					writerDone <- innerCtx.Err()
					return
				default:
				}
				frame, readErr := input.Read()
				if readErr == io.EOF {
					writerDone <- sess.sendFinish(innerCtx)
					return
				}
				if readErr != nil {
					writerDone <- readErr
					return
				}
				if len(frame.Data) > 0 {
					if err := sess.sendAudio(innerCtx, frame.Data); err != nil {
						writerDone <- err
						return
					}
				}
			}
		}()

		defer func() { _ = sess.close() }()
		for {
			select {
			case <-innerCtx.Done():
				return
			case err := <-writerDone:
				if err != nil && err != io.EOF && innerCtx.Err() == nil {
					return
				}
				writerDone = nil
			default:
			}

			resp, err := sess.read(innerCtx)
			if err != nil {
				return
			}
			if resp.code != 0 {
				errMsg := fmt.Sprintf("bytedance stt: server error: code %d", resp.code)
				if resp.payload != nil && resp.payload.Error != "" {
					errMsg = fmt.Sprintf("bytedance stt: server error: %s", resp.payload.Error)
				}
				telemetry.Error(innerCtx, errMsg)
				out.Interrupt()
				return
			}
			if resp.payload != nil {
				s.emitResults(innerCtx, resp, o.Language, out)
			}
			if resp.isLast {
				return
			}
		}
	}()

	return out, nil
}

func (s *STT) emitResults(ctx context.Context, resp *asrResponse, lang string, out *audio.Pipe[stt.STTResult]) {
	if len(resp.payload.Result.Utterances) > 0 {
		for _, u := range resp.payload.Result.Utterances {
			if u.Text == "" {
				continue
			}
			select {
			case <-ctx.Done():
				return
			default:
				if !out.Send(stt.STTResult{Text: u.Text, IsFinal: u.Definite, Lang: lang}) {
					return
				}
			}
		}
		return
	}
	if resp.payload.Result.Text != "" {
		select {
		case <-ctx.Done():
			return
		default:
			out.Send(stt.STTResult{Text: resp.payload.Result.Text, IsFinal: resp.isLast, Lang: lang})
		}
	}
}

func formatToBytedance(c audio.Codec) string {
	switch c {
	case audio.CodecPCM:
		return "pcm"
	case audio.CodecOpus:
		return "opus"
	case audio.CodecMP3:
		return "mp3"
	case audio.CodecWAV:
		return "wav"
	default:
		return ""
	}
}

func (s *STT) buildASRRequest(o *stt.STTOptions, fmt audio.Format) asrRequestPayload {
	format := formatToBytedance(fmt.Codec)
	if format == "" {
		format = "pcm"
	}

	rate := fmt.SampleRate
	if rate <= 0 {
		rate = 16000
	}

	channels := fmt.Channels
	if channels <= 0 {
		channels = 1
	}

	bits := fmt.BitDepth
	if bits <= 0 {
		bits = 16
	}

	req := asrRequestPayload{
		User: asrUserMeta{UID: s.uid},
		Audio: asrAudioMeta{
			Format:   format,
			Rate:     rate,
			Bits:     bits,
			Channel:  channels,
			Language: o.Language,
		},
		Request: asrRequestMeta{
			ModelName:       s.model,
			EnableITN:       o.ExtraBool(KeyEnableITN, true),
			EnablePUNC:      o.ExtraBool(KeyEnablePUNC, true),
			ShowUtterances:  true,
			EnableNonstream: o.ExtraBool(KeyNonstream, s.nonstream),
			EndWindowSize:   o.ExtraInt(KeyEndWindow, s.endWindow),
			ResultType:      o.ExtraString(KeyResultType, "full"),
		},
	}
	return req
}

func generateReqID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "0000000000000000"
	}
	return hex.EncodeToString(b[:])
}
