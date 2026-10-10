package hook

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/utils"
)

// urlSource decodes one URL-kind media source. media's exported
// constructors take real URLs, so a source carrying a local path is
// built the way the wire carries it: through JSON.
func urlSource[T any](t *testing.T, rawURL, mediaType string) T {
	t.Helper()
	var source T
	if err := json.Unmarshal(
		[]byte(`{"kind":"url","url":`+strconv.Quote(rawURL)+`,"media_type":"`+mediaType+`"}`),
		&source,
	); err != nil {
		t.Fatalf("build media source: %v", err)
	}
	return source
}

func mustResolve(t *testing.T, settings Settings) resolvedSettings {
	t.Helper()
	config, err := settings.resolve()
	if err != nil {
		t.Fatalf("resolve settings: %v", err)
	}
	return config
}

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestNormalizePartsInlinesLocalMedia(t *testing.T) {
	dir := t.TempDir()
	png := writeFile(t, dir, "photo.png", "png-bytes")
	remote, err := media.NewImageURL("https://example.com/a.png", "image/png")
	if err != nil {
		t.Fatal(err)
	}
	recording, err := media.NewAudioBytes([]byte("audio-bytes"), "audio/webm")
	if err != nil {
		t.Fatal(err)
	}
	// The budget below is exactly the file size: an attachment on the
	// budget is inlined, one byte over it is flattened.
	config := mustResolve(t, Settings{
		WorkDir:          dir,
		PassthroughKinds: []string{"file", "audio"},
		MaxInlineBytes:   int64(len("png-bytes")),
	})
	parts, changed, err := normalizeParts(context.Background(), []message.Part{
		message.TextPart{Text: "look"},
		message.ImagePart{Source: urlSource[media.ImageSource](t, png, "image/png")},
		message.ImagePart{Source: remote},
		message.AudioPart{Source: recording},
		message.FilePart{URI: png, Name: "photo.png", MediaType: "image/png"},
	}, config)
	if err != nil {
		t.Fatalf("normalizeParts: %v", err)
	}
	if !changed {
		t.Fatal("normalizeParts reported no change")
	}
	local := parts[1].(message.ImagePart)
	if local.Source.Kind() != media.SourceInline {
		t.Fatalf("local image source = %s, want inline", local.Source.Kind())
	}
	if string(local.Source.Bytes()) != "png-bytes" {
		t.Errorf("inline bytes = %q, want source bytes", local.Source.Bytes())
	}
	if got := parts[2].(message.ImagePart); got.Source.Kind() != media.SourceURL {
		t.Errorf("remote image was inlined: %s", got.Source.Kind())
	}
	if got := parts[3].(message.AudioPart); got.Source.Kind() != media.SourceInline {
		t.Errorf("inline audio was rewritten: %s", got.Source.Kind())
	}
	if _, ok := parts[4].(message.FilePart); !ok {
		t.Errorf("file part changed type: %T", parts[4])
	}
}

func TestNormalizePartsNoChange(t *testing.T) {
	remote, err := media.NewImageURL("https://example.com/a.png", "image/png")
	if err != nil {
		t.Fatal(err)
	}
	parts, changed, err := normalizeParts(context.Background(), []message.Part{
		message.TextPart{Text: "hi"},
		message.ImagePart{Source: remote},
	}, mustResolve(t, Settings{}))
	if err != nil {
		t.Fatalf("normalizeParts: %v", err)
	}
	if changed || len(parts) != 2 {
		t.Errorf("changed=%v parts=%d, want no change", changed, len(parts))
	}
}

func TestNormalizePartsFlattensNonPassthroughKinds(t *testing.T) {
	const workDir = "/ws/proj"
	const storedAudio = workDir + "/sessions/s-abc/media/1-a.mp3"
	const storedVideo = workDir + "/sessions/s-abc/media/2-b.mp4"
	const storedFile = workDir + "/sessions/s-abc/files/3-notes.txt"
	config := mustResolve(t, Settings{WorkDir: workDir})
	audioSource := urlSource[media.AudioSource](t, storedAudio, "audio/mpeg")
	videoSource := urlSource[media.VideoSource](t, storedVideo, "video/mp4")
	parts, changed, err := normalizeParts(context.Background(), []message.Part{
		message.TextPart{Text: "look"},
		message.FilePart{URI: storedFile, Name: "notes.txt"},
		message.AudioPart{Source: audioSource},
		message.VideoPart{Source: videoSource},
	}, config)
	if err != nil {
		t.Fatalf("normalizeParts: %v", err)
	}
	if !changed || len(parts) != 4 {
		t.Fatalf("flatten = %v parts, changed=%v", len(parts), changed)
	}
	if got := parts[1].(message.TextPart).Text; got != "[file] sessions/s-abc/files/3-notes.txt" {
		t.Errorf("file line = %q", got)
	}
	if got := parts[2].(message.TextPart).Text; got != "[audio file] sessions/s-abc/media/1-a.mp3" {
		t.Errorf("audio line = %q", got)
	}
	if got := parts[3].(message.TextPart).Text; got != "[video file] sessions/s-abc/media/2-b.mp4" {
		t.Errorf("video line = %q", got)
	}

	// Paths outside the workspace stay absolute.
	outside := urlSource[media.AudioSource](t, "/tmp/out.mp3", "audio/mpeg")
	parts, changed, err = normalizeParts(context.Background(), []message.Part{
		message.AudioPart{Source: outside},
	}, config)
	if err != nil {
		t.Fatalf("normalizeParts: %v", err)
	}
	if !changed || len(parts) != 1 {
		t.Fatalf("outside flatten = %v parts, changed=%v", len(parts), changed)
	}
	if got := parts[0].(message.TextPart).Text; got != "[audio file] /tmp/out.mp3" {
		t.Errorf("outside line = %q", got)
	}

	// A kind the deployment passes through keeps its part: the driver
	// lowers it to the wire, and flattening it would hide the
	// attachment behind a path the model cannot open. This path names no
	// local file, so nothing is inlined either.
	videoConfig := mustResolve(t, Settings{
		WorkDir:          workDir,
		PassthroughKinds: []string{"video"},
	})
	parts, changed, err = normalizeParts(context.Background(), []message.Part{
		message.TextPart{Text: "look"},
		message.VideoPart{Source: videoSource},
	}, videoConfig)
	if err != nil {
		t.Fatalf("normalizeParts: %v", err)
	}
	if changed || len(parts) != 2 {
		t.Fatalf("video passthrough = %v parts, changed=%v", len(parts), changed)
	}
	if _, ok := parts[1].(message.VideoPart); !ok {
		t.Fatalf("part = %T, want the video to survive", parts[1])
	}

	// A custom marker replaces the default for its kind; the localized
	// label here is what a desktop deployment configures.
	marked := mustResolve(t, Settings{WorkDir: workDir, AudioMarker: "[录音] "})
	parts, changed, err = normalizeParts(context.Background(), []message.Part{
		message.AudioPart{Source: audioSource},
	}, marked)
	if err != nil {
		t.Fatalf("normalizeParts: %v", err)
	}
	if !changed || len(parts) != 1 {
		t.Fatalf("marked flatten = %v parts, changed=%v", len(parts), changed)
	}
	if got := parts[0].(message.TextPart).Text; got != "[录音] sessions/s-abc/media/1-a.mp3" {
		t.Errorf("marked audio line = %q", got)
	}
}

func TestNormalizePartsDegradesOversizedAttachments(t *testing.T) {
	dir := t.TempDir()
	photo := writeFile(t, dir, "big.png", "0123456789")
	config := mustResolve(t, Settings{WorkDir: dir, MaxInlineBytes: 4})
	parts, changed, err := normalizeParts(context.Background(), []message.Part{
		message.ImagePart{Source: urlSource[media.ImageSource](t, photo, "image/png")},
	}, config)
	if err != nil {
		t.Fatalf("normalizeParts: %v", err)
	}
	if !changed || len(parts) != 1 {
		t.Fatalf("parts = %d, changed=%v: an oversized attachment must not fail the turn",
			len(parts), changed)
	}
	if got := parts[0].(message.TextPart).Text; got != "[image file] big.png" {
		t.Errorf("degraded image line = %q", got)
	}

	// A passthrough kind over the budget is degraded too: the bytes
	// cannot travel, so the part would reach the provider unfetchable.
	clip := writeFile(t, dir, "clip.mp4", "0123456789")
	videoConfig := mustResolve(t, Settings{
		WorkDir:          dir,
		MaxInlineBytes:   4,
		PassthroughKinds: []string{"video"},
	})
	parts, changed, err = normalizeParts(context.Background(), []message.Part{
		message.VideoPart{Source: urlSource[media.VideoSource](t, clip, "video/mp4")},
	}, videoConfig)
	if err != nil {
		t.Fatalf("normalizeParts: %v", err)
	}
	if !changed || len(parts) != 1 {
		t.Fatalf("parts = %d, changed=%v", len(parts), changed)
	}
	if got := parts[0].(message.TextPart).Text; got != "[video file] clip.mp4" {
		t.Errorf("degraded video line = %q", got)
	}
}

func TestNormalizePartsReadsUnderAnInt64CeilingBudget(t *testing.T) {
	// A budget at the int64 ceiling means "no practical bound". The read
	// runs one byte past the budget to spot an oversized file, and that
	// increment has no room left here: wrapping it would read nothing,
	// pass the budget check with an empty payload, and fail the turn
	// inside a media constructor — naming neither the setting nor the
	// file. The attachment must inline with its bytes intact.
	dir := t.TempDir()
	recording := writeFile(t, dir, "note.webm", "audio-bytes")
	config := mustResolve(t, Settings{
		WorkDir:          dir,
		PassthroughKinds: []string{"audio"},
		MaxInlineBytes:   math.MaxInt64,
	})
	parts, changed, err := normalizeParts(context.Background(), []message.Part{
		message.AudioPart{
			Source: urlSource[media.AudioSource](t, recording, "audio/webm"),
		},
	}, config)
	if err != nil {
		t.Fatalf("normalizeParts: %v", err)
	}
	if !changed || len(parts) != 1 {
		t.Fatalf("parts = %d, changed=%v", len(parts), changed)
	}
	audio, ok := parts[0].(message.AudioPart)
	if !ok {
		t.Fatalf("part = %T, want the audio part inlined", parts[0])
	}
	if audio.Source.Kind() != media.SourceInline {
		t.Fatalf("audio source = %s, want inline", audio.Source.Kind())
	}
	if got := string(audio.Source.Bytes()); got != "audio-bytes" {
		t.Errorf("inline bytes = %q, want the file's bytes", got)
	}
}

func TestNormalizePartsDescribesPathlessAttachments(t *testing.T) {
	recording, err := media.NewAudioBytes([]byte("audio-bytes"), "audio/webm")
	if err != nil {
		t.Fatal(err)
	}
	clip, err := media.NewVideoStream(message.NewPartPipe(1), "video/mp4")
	if err != nil {
		t.Fatal(err)
	}
	parts, changed, err := normalizeParts(context.Background(), []message.Part{
		message.AudioPart{Source: recording},
		message.VideoPart{Source: clip},
		message.FilePart{Name: "notes.txt"},
	}, mustResolve(t, Settings{}))
	if err != nil {
		t.Fatalf("normalizeParts: %v", err)
	}
	if !changed || len(parts) != 3 {
		t.Fatalf("parts = %d, changed=%v", len(parts), changed)
	}
	// An attachment with no path to name is described instead of
	// vanishing from the prompt: the model knows what it did not get.
	for index, want := range []string{
		"[audio file] (inline audio/webm)",
		"[video file] (stream video/mp4)",
		"[file] (file text/plain)",
	} {
		if got := parts[index].(message.TextPart).Text; got != want {
			t.Errorf("line %d = %q, want %q", index, got, want)
		}
	}
}

func TestNormalizePartsUsesForwardSlashes(t *testing.T) {
	// The rendered line is prompt text, and prompt text crosses hosts:
	// separators are slashed on every platform, including the one this
	// test runs on when it is Windows.
	workDir := filepath.FromSlash("/ws/proj")
	config := mustResolve(t, Settings{WorkDir: workDir})
	stored := filepath.Join(workDir, "sessions", "s-abc", "files", "notes.txt")
	parts, changed, err := normalizeParts(context.Background(), []message.Part{
		message.FilePart{URI: stored, Name: "notes.txt"},
	}, config)
	if err != nil {
		t.Fatalf("normalizeParts: %v", err)
	}
	if !changed || len(parts) != 1 {
		t.Fatalf("parts = %d, changed=%v", len(parts), changed)
	}
	line := parts[0].(message.TextPart).Text
	if want := "[file] sessions/s-abc/files/notes.txt"; line != want {
		t.Errorf("line = %q, want %q", line, want)
	}
	if strings.ContainsRune(line, '\\') {
		t.Errorf("line carries a platform separator: %q", line)
	}
}

func TestNormalizePartsHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := normalizeParts(ctx, []message.Part{
		message.TextPart{Text: "hi"},
	}, mustResolve(t, Settings{})); !errors.Is(err, context.Canceled) {
		t.Fatalf("normalizeParts error = %v, want context.Canceled", err)
	}

	value, err := prepareFactory{}.New(context.Background(), resource.Input{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	board := agent.NewBoard()
	board.AppendChannelMessage(agent.MainChannel, message.Message{
		Role:    message.RoleUser,
		Content: message.Content{Parts: []message.Part{message.TextPart{Text: "hi"}}},
	})
	if _, err := value.(agent.Preparer).Before(ctx,
		agent.Identity{RunID: "r1"}, &agent.Request{TaskID: "t1"}, board); err == nil {
		t.Fatal("Before ignored a cancelled context")
	}
}

func TestNormalizePartsRejectsMalformedParts(t *testing.T) {
	_, _, err := normalizeParts(context.Background(), []message.Part{nil},
		mustResolve(t, Settings{}))
	if err == nil {
		t.Fatal("normalizeParts accepted a nil part")
	}
}

func TestSettingsResolve(t *testing.T) {
	config := mustResolve(t, Settings{})
	if config.workDir != "" || len(config.passthrough) != 0 {
		t.Errorf("default config = %+v, want empty", config)
	}
	if config.maxInline != DefaultMaxInlineBytes {
		t.Errorf("default inline budget = %d, want %d",
			config.maxInline, DefaultMaxInlineBytes)
	}
	if config.audioMarker != DefaultAudioMarker ||
		config.videoMarker != DefaultVideoMarker ||
		config.imageMarker != DefaultImageMarker ||
		config.fileMarker != DefaultFileMarker {
		t.Errorf("default markers = %q, %q, %q, %q",
			config.audioMarker, config.videoMarker,
			config.imageMarker, config.fileMarker)
	}

	config = mustResolve(t, Settings{
		WorkDir:          "/ws",
		PassthroughKinds: []string{"video", " file "},
		AudioMarker:      "A ",
		MaxInlineBytes:   2048,
	})
	if !config.passthrough[message.PartVideo] ||
		!config.passthrough[message.PartFile] {
		t.Errorf("passthrough = %v, want video and file", config.passthrough)
	}
	if config.passthrough[message.PartAudio] ||
		config.passthrough[message.PartImage] {
		t.Errorf("passthrough = %v, want no audio or image", config.passthrough)
	}
	if config.audioMarker != "A " || config.videoMarker != DefaultVideoMarker {
		t.Errorf("markers = %q, %q", config.audioMarker, config.videoMarker)
	}
	if config.maxInline != 2048 {
		t.Errorf("inline budget = %d, want 2048", config.maxInline)
	}

	for _, kind := range []string{"image", "text", "", "audio, video"} {
		if _, err := (Settings{PassthroughKinds: []string{kind}}).resolve(); err == nil {
			t.Errorf("passthrough kind %q was accepted", kind)
		}
	}
	if _, err := (Settings{MaxInlineBytes: -1}).resolve(); err == nil {
		t.Error("a negative inline budget was accepted")
	}
}

func TestPrepareFactorySpec(t *testing.T) {
	spec := prepareFactory{}.Spec()
	if spec.Kind != resource.Kind("hook.prepare") || spec.Impl != AttachmentsType {
		t.Fatalf("spec = %+v, want hook.prepare/%s", spec, AttachmentsType)
	}
}

func TestRegisterAddsTheFactory(t *testing.T) {
	registry := resource.NewRegistry()
	if err := Register(registry); err != nil {
		t.Fatal(err)
	}
	if _, ok := registry.Lookup(resource.Kind("hook.prepare"), AttachmentsType); !ok {
		t.Fatal("media.attachments factory is not registered")
	}
}

func TestPrepareFactoryNew(t *testing.T) {
	value, err := prepareFactory{}.New(context.Background(), resource.Input{
		Settings: settingsNode(t, `
work_dir: /ws
passthrough_kinds: [video]
audio_marker: "A "
image_marker: "I "
max_inline_bytes: 4096
`),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	preparer, ok := value.(agent.Preparer)
	if !ok {
		t.Fatalf("New returned %T, want an agent.Preparer", value)
	}
	if _, err := preparer.Before(context.Background(), agent.Identity{RunID: "r1"},
		&agent.Request{TaskID: "t1"}, nil); err == nil {
		t.Fatal("Before accepted a nil board")
	}

	for name, settings := range map[string]string{
		"unknown passthrough kind": "passthrough_kinds: [text]\n",
		"negative inline budget":   "max_inline_bytes: -1\n",
		"unknown field":            `{"nope": 1}`,
		"wrong field type":         `{"work_dir": 3}`,
	} {
		if _, err := (prepareFactory{}).New(context.Background(), resource.Input{
			Settings: settingsNode(t, settings),
		}); err == nil {
			t.Errorf("%s: New accepted the settings", name)
		}
	}
}

func TestPreparerNormalizesTheBoard(t *testing.T) {
	dir := t.TempDir()
	png := writeFile(t, dir, "photo.png", "png-bytes")
	settings, err := json.Marshal(map[string]string{"work_dir": dir})
	if err != nil {
		t.Fatal(err)
	}
	value, err := prepareFactory{}.New(context.Background(), resource.Input{
		Settings: settings,
	})
	if err != nil {
		t.Fatal(err)
	}
	preparer := value.(agent.Preparer)

	turn := message.Message{Role: message.RoleUser, Content: message.Content{
		Parts: []message.Part{
			message.TextPart{Text: "look"},
			message.ImagePart{Source: urlSource[media.ImageSource](t, png, "image/png")},
			message.FilePart{URI: filepath.Join(dir, "notes.txt"), Name: "notes.txt"},
		},
	}}
	board := agent.NewBoard()
	board.AppendChannelMessage(agent.MainChannel, turn)
	before := board.Clone()

	next, err := preparer.Before(context.Background(),
		agent.Identity{RunID: "r1"}, &agent.Request{TaskID: "t1"}, board)
	if err != nil {
		t.Fatalf("Before: %v", err)
	}
	if next == board {
		t.Fatal("Before returned the input board; each call must allocate a fresh one")
	}
	if !reflect.DeepEqual(board, before) {
		t.Fatal("Before mutated the previous board")
	}

	channel := next.Channel(agent.MainChannel)
	if len(channel) != 1 {
		t.Fatalf("channel = %d messages, want 1", len(channel))
	}
	parts := channel[0].Content.Parts
	if len(parts) != 3 {
		t.Fatalf("parts = %d, want 3", len(parts))
	}
	if got := parts[0].(message.TextPart).Text; got != "look" {
		t.Errorf("text part = %q", got)
	}
	image := parts[1].(message.ImagePart)
	if image.Source.Kind() != media.SourceInline {
		t.Errorf("image source = %s, want inline", image.Source.Kind())
	}
	if got := string(image.Source.Bytes()); got != "png-bytes" {
		t.Errorf("image bytes = %q", got)
	}
	if got := parts[2].(message.TextPart).Text; got != "[file] notes.txt" {
		t.Errorf("file line = %q", got)
	}

	// A first message that is not the user's request is left alone.
	other := agent.NewBoard()
	other.AppendChannelMessage(agent.MainChannel, message.Message{
		Role: message.RoleAssistant,
		Content: message.Content{Parts: []message.Part{
			message.FilePart{URI: filepath.Join(dir, "notes.txt"), Name: "notes.txt"},
		}},
	})
	next, err = preparer.Before(context.Background(),
		agent.Identity{RunID: "r1"}, &agent.Request{TaskID: "t1"}, other)
	if err != nil {
		t.Fatalf("Before: %v", err)
	}
	if _, ok := next.Channel(agent.MainChannel)[0].Content.Parts[0].(message.FilePart); !ok {
		t.Error("a non-user first message was rewritten")
	}
}

func settingsNode(t *testing.T, source string) json.RawMessage {
	t.Helper()
	jsonData, err := utils.ToJSON([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	var out json.RawMessage
	if err := json.Unmarshal(jsonData, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
