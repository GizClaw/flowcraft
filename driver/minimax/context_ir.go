package minimax

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
)

// H3-Context-IR runs on the same v2 async task pipeline as video
// generation, but its output is a text artifact: the task deeply
// understands the multimodal context and returns a structured, richer
// video prompt (task_type=h3_context_ir, modality=text). It never creates
// a video. The content input, duration, ratio, and callback_url semantics
// are identical to the v2 video API, so the compiler shares the content
// role mapping (compileV2Inputs) and the transport shares the poll loop
// (pollV2Task); only the create endpoint and the succeeded content field
// differ.
//
// The canonical surface is a TextIntent — the enhanced prompt is the
// requested output. The target video's duration and ratio have no
// canonical intent home, so they ride the ContextIROptions extension;
// both default like the video API does (6s; 16:9 for text-only tasks,
// adaptive otherwise).

type contextIRWire struct {
	model  string
	prompt string
	v2Content
	duration    int // seconds; default 6
	ratio       string
	callbackURL string
}

type contextIRRaw struct {
	prompt       string
	inputTokens  int64
	outputTokens int64
	totalTokens  int64
	requestID    string
}

func compileContextIR(
	endpoint string,
	entry catalogEntry,
) inference.GenerateCompiler[contextIRWire] {
	return func(
		_ context.Context,
		model inference.ModelRef,
		request inference.GenerateRequest,
		shape inference.GenerateExecutionShape,
	) (inference.Compiled[contextIRWire], error) {
		ledger := newLedger(
			inference.OperationGenerate,
			request.ActiveFieldsFor(shape),
		)
		wire := contextIRWire{model: endpoint}
		if shape == inference.GenerateExecutionStream {
			ledger.reject(
				inference.FieldGenerateExecutionStream,
				"context-IR is a unary task on this provider",
			)
		}

		prompt, images, videos, audios := collectTaskParts(request, ledger)
		wire.prompt = strings.Join(prompt, "\n")
		compileV2Inputs(&wire.v2Content, images, videos, audios, ledger)
		rejectPromptLength(
			request,
			ledger,
			len(wire.prompt),
			7000,
			fmt.Sprintf("MiniMax-H3 prompts are at most 7000 characters, not %d", len(wire.prompt)),
		)
		if wire.prompt == "" {
			reason := fmt.Sprintf("%s requires a non-empty text prompt", endpoint)
			if len(prompt) > 0 {
				ledger.reject(inference.FieldGenerateInputText, reason)
			} else if intent := request.Input.Content.Intent; intent.Text != nil {
				ledger.reject(inference.FieldGenerateIntentText, reason)
			} else {
				ledger.reject(inference.FieldGenerateInputRole, reason)
			}
		}

		intent := request.Input.Content.Intent
		if text := intent.Text; text != nil {
			// Context-IR has no tool, sampling, token-cap, or response
			// shaping controls: the output is one fixed enhanced prompt.
			rejectTextControls(text, ledger,
				"context-IR tasks do not call tools",
				"context-IR has no sampling controls",
				"context-IR has no thinking control",
			)
			if text.MaxOutputTokens != nil {
				ledger.reject(
					inference.FieldGenerateIntentTextMaxOutputTokens,
					"context-IR returns a fixed enhanced prompt; no token cap applies",
				)
			}
			if text.Response != nil {
				ledger.reject(
					inference.FieldGenerateIntentTextResponse,
					"context-IR returns a plain enhanced prompt",
				)
			}
		}
		if intent.Image != nil {
			ledger.reject(
				inference.FieldGenerateIntentImage,
				"context-IR returns an enhanced prompt, not an image",
			)
		}
		if intent.Audio != nil {
			ledger.reject(
				inference.FieldGenerateIntentAudio,
				"context-IR returns an enhanced prompt, not audio",
			)
		}
		if intent.Video != nil {
			ledger.reject(
				inference.FieldGenerateIntentVideo,
				"context-IR returns an enhanced prompt, not a video; use the Hailuo/H3 video models to generate",
			)
		}
		if intent.Text == nil {
			ledger.reject(
				inference.FieldGenerateInputRole,
				"context-IR requires the text output intent; it returns an enhanced prompt",
			)
		}

		options, other := operationExtensions[ContextIROptions](request.Extensions)
		compileContextIROptions(&wire, options, endpoint, ledger)
		rejectOtherExtensions("context-IR", other, ledger)

		report := ledger.report()
		if len(ledger.order) > 0 {
			return inference.Compiled[contextIRWire]{Report: report}, ledger.err()
		}
		return inference.Compiled[contextIRWire]{Wire: wire, Report: report}, nil
	}
}

// compileContextIROptions lowers ContextIROptions onto the wire and
// enforces the target-video constraints shared with the v2 video API.
func compileContextIROptions(
	wire *contextIRWire,
	options ContextIROptions,
	endpoint string,
	ledger *ledger,
) {
	field := func(name string) inference.FieldID {
		return inference.ExtensionField(name).Qualify(options)
	}
	wire.callbackURL = options.CallbackURL
	// The schema requires duration; the driver defaults to the same 6s
	// the v2 video API uses when the caller leaves it open.
	wire.duration = 6
	if options.DurationMillis != nil {
		millis := *options.DurationMillis
		if millis%1000 != 0 {
			ledger.reject(
				field("duration_millis"),
				fmt.Sprintf("minimax durations are whole seconds, not %dms", millis),
			)
		} else {
			seconds := int(millis / 1000)
			if seconds < 4 || seconds > 15 {
				ledger.reject(
					field("duration_millis"),
					fmt.Sprintf("MiniMax-H3 durations are 4-15s, not %ds", seconds),
				)
			} else {
				wire.duration = seconds
			}
		}
	}
	if options.Ratio != "" {
		if !v2RatioValues[options.Ratio] {
			ledger.reject(
				field("ratio"),
				fmt.Sprintf(
					"MiniMax-H3 ratios are adaptive/21:9/16:9/4:3/1:1/3:4/9:16, not %q",
					options.Ratio,
				),
			)
		} else {
			wire.ratio = options.Ratio
		}
	} else if v2TextOnly(&wire.v2Content) {
		// Text-only tasks require an explicit non-adaptive ratio; 16:9 is
		// the driver default, mirroring the video compiler.
		wire.ratio = "16:9"
	}
	if wire.ratio == "adaptive" && v2TextOnly(&wire.v2Content) {
		ledger.reject(
			field("ratio"),
			"text-only context-IR requires an explicit ratio; adaptive is not allowed",
		)
	}
}

func transportContextIR(
	client *mediaClient,
	pollInterval time.Duration,
) inference.Transport[contextIRWire, contextIRRaw] {
	return func(ctx context.Context, wire contextIRWire) (contextIRRaw, error) {
		request := map[string]any{
			"model":    wire.model,
			"content":  v2ContentItems(wire.prompt, wire.v2Content),
			"duration": wire.duration,
		}
		if wire.ratio != "" {
			request["ratio"] = wire.ratio
		}
		if wire.callbackURL != "" {
			request["callback_url"] = wire.callbackURL
		}

		var created videoV2CreateResponse
		if err := client.postJSON(ctx, "/v2/h3_context_ir", request, &created); err != nil {
			return contextIRRaw{}, err
		}
		if created.TaskID == "" {
			return contextIRRaw{}, fmt.Errorf(
				"minimax: context-IR task creation returned no task_id",
			)
		}
		task, err := pollV2Task(ctx, client, created.TaskID, pollInterval)
		if err != nil {
			return contextIRRaw{}, err
		}
		if task.prompt == "" {
			return contextIRRaw{}, fmt.Errorf(
				"minimax: succeeded context-IR task %q carries no prompt",
				created.TaskID,
			)
		}
		return contextIRRaw{
			prompt:       task.prompt,
			inputTokens:  task.usage.inputTokens,
			outputTokens: task.usage.outputTokens,
			totalTokens:  task.usage.totalTokens,
			requestID:    task.id,
		}, nil
	}
}

func decodeContextIR(
	_ context.Context,
	raw contextIRRaw,
) (inference.GenerateResponse, error) {
	if raw.prompt == "" {
		return inference.GenerateResponse{}, fmt.Errorf(
			"minimax: context-IR task returned no prompt",
		)
	}
	return inference.GenerateResponse{
		Message: message.Message{
			Role: message.RoleAssistant,
			Content: message.Content{
				Parts: []message.Part{message.TextPart{Text: raw.prompt}},
			},
		},
		FinishReason: inference.FinishCompleted,
		Usage: inference.Usage{
			InputTokens:  raw.inputTokens,
			OutputTokens: raw.outputTokens,
			TotalTokens:  raw.totalTokens,
		},
		Metadata: inference.Metadata{RequestID: raw.requestID},
	}, nil
}

func openContextIR(
	cls *clients,
	spec Spec,
	entry catalogEntry,
	id inference.ModelID,
) (inference.GenerateOperations, error) {
	unary, err := inference.BindGenerate(
		compileContextIR(wireModel(id.Name, entry), entry),
		transportContextIR(cls.media, spec.videoPollInterval()),
		decodeContextIR,
	)
	if err != nil {
		return inference.GenerateOperations{}, err
	}
	return inference.GenerateOperations{Unary: unary}, nil
}
