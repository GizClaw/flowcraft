package hook

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
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

func TestInlineLocalMedia(t *testing.T) {
	dir := t.TempDir()
	png := filepath.Join(dir, "photo.png")
	if err := os.WriteFile(png, []byte("png-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	localSource := urlSource[media.ImageSource](t, png, "image/png")
	remoteSource, err := media.NewImageURL("https://example.com/a.png", "image/png")
	if err != nil {
		t.Fatal(err)
	}
	parts, changed, err := inlineLocalMedia([]message.Part{
		message.TextPart{Text: "look"},
		message.ImagePart{Source: localSource},
		message.ImagePart{Source: remoteSource},
		message.FilePart{URI: png, Name: "photo.png", MediaType: "image/png"},
	})
	if err != nil {
		t.Fatalf("inlineLocalMedia: %v", err)
	}
	if !changed {
		t.Fatal("inlineLocalMedia reported no change")
	}
	local := parts[1].(message.ImagePart)
	if local.Source.Kind() != media.SourceInline {
		t.Fatalf("local image source = %s, want inline", local.Source.Kind())
	}
	if string(local.Source.Bytes()) != "png-bytes" {
		t.Errorf("inline bytes = %q, want source bytes", local.Source.Bytes())
	}
	remote := parts[2].(message.ImagePart)
	if remote.Source.Kind() != media.SourceURL {
		t.Errorf("remote image was inlined: %s", remote.Source.Kind())
	}
	if _, ok := parts[3].(message.FilePart); !ok {
		t.Errorf("file part changed type: %T", parts[3])
	}
}

func TestInlineLocalMediaNoChange(t *testing.T) {
	remote, err := media.NewImageURL("https://example.com/a.png", "image/png")
	if err != nil {
		t.Fatal(err)
	}
	parts, changed, err := inlineLocalMedia([]message.Part{
		message.TextPart{Text: "hi"},
		message.ImagePart{Source: remote},
	})
	if err != nil {
		t.Fatalf("inlineLocalMedia: %v", err)
	}
	if changed || len(parts) != 2 {
		t.Errorf("changed=%v parts=%d, want no change", changed, len(parts))
	}
}

func TestFlattenNonImageMedia(t *testing.T) {
	const workDir = "/ws/proj"
	const storedAudio = workDir + "/sessions/s-abc/media/1-a.mp3"
	const storedVideo = workDir + "/sessions/s-abc/media/2-b.mp4"
	const storedFile = workDir + "/sessions/s-abc/files/3-notes.txt"
	config := mustResolve(t, Settings{WorkDir: workDir})
	audioSource := urlSource[media.AudioSource](t, storedAudio, "audio/mpeg")
	videoSource := urlSource[media.VideoSource](t, storedVideo, "video/mp4")
	parts, changed := flattenNonImageMedia([]message.Part{
		message.TextPart{Text: "look"},
		message.FilePart{URI: storedFile, Name: "notes.txt"},
		message.AudioPart{Source: audioSource},
		message.VideoPart{Source: videoSource},
	}, config)
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
	parts, changed = flattenNonImageMedia([]message.Part{
		message.AudioPart{Source: outside},
	}, config)
	if !changed || len(parts) != 1 {
		t.Fatalf("outside flatten = %v parts, changed=%v", len(parts), changed)
	}
	if got := parts[0].(message.TextPart).Text; got != "[audio file] /tmp/out.mp3" {
		t.Errorf("outside line = %q", got)
	}

	// A kind the deployment passes through keeps its part: the driver
	// lowers it to the wire, and flattening it would hide the
	// attachment behind a path the model cannot open.
	videoConfig := mustResolve(t, Settings{
		WorkDir:          workDir,
		PassthroughKinds: []string{"video"},
	})
	parts, changed = flattenNonImageMedia([]message.Part{
		message.TextPart{Text: "look"},
		message.VideoPart{Source: videoSource},
	}, videoConfig)
	if changed || len(parts) != 2 {
		t.Fatalf("video passthrough = %v parts, changed=%v", len(parts), changed)
	}
	if _, ok := parts[1].(message.VideoPart); !ok {
		t.Fatalf("part = %T, want the video to survive", parts[1])
	}

	// Images survive stripping.
	remote, err := media.NewImageURL("https://example.com/a.png", "image/png")
	if err != nil {
		t.Fatal(err)
	}
	parts, changed = flattenNonImageMedia([]message.Part{
		message.TextPart{Text: "hi"},
		message.ImagePart{Source: remote},
	}, config)
	if changed || len(parts) != 2 {
		t.Errorf("flatten = %v parts, changed=%v, want unchanged", len(parts), changed)
	}

	// A custom marker replaces the default for its kind; the localized
	// label here is what a desktop deployment configures.
	marked := mustResolve(t, Settings{WorkDir: workDir, AudioMarker: "[录音] "})
	parts, changed = flattenNonImageMedia([]message.Part{
		message.AudioPart{Source: audioSource},
	}, marked)
	if !changed || len(parts) != 1 {
		t.Fatalf("marked flatten = %v parts, changed=%v", len(parts), changed)
	}
	if got := parts[0].(message.TextPart).Text; got != "[录音] sessions/s-abc/media/1-a.mp3" {
		t.Errorf("marked audio line = %q", got)
	}
}

func TestSettingsResolve(t *testing.T) {
	config := mustResolve(t, Settings{})
	if config.workDir != "" || len(config.passthrough) != 0 {
		t.Errorf("default config = %+v, want empty", config)
	}
	if config.audioMarker != DefaultAudioMarker ||
		config.videoMarker != DefaultVideoMarker ||
		config.fileMarker != DefaultFileMarker {
		t.Errorf("default markers = %q, %q, %q",
			config.audioMarker, config.videoMarker, config.fileMarker)
	}

	config = mustResolve(t, Settings{
		WorkDir:          "/ws",
		PassthroughKinds: []string{"video", " file "},
		AudioMarker:      "A ",
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

	for _, kind := range []string{"image", "text", "", "audio, video"} {
		if _, err := (Settings{PassthroughKinds: []string{kind}}).resolve(); err == nil {
			t.Errorf("passthrough kind %q was accepted", kind)
		}
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
`),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := value.(agent.Preparer); !ok {
		t.Fatalf("New returned %T, want an agent.Preparer", value)
	}

	for name, settings := range map[string]string{
		"unknown passthrough kind": "passthrough_kinds: [text]\n",
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
	png := filepath.Join(dir, "photo.png")
	if err := os.WriteFile(png, []byte("png-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
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
