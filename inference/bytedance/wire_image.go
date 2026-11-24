package bytedance

// The image surface is the platform's own dialect: one create call carrying the
// prompt, up to a few hundred kilobytes of base64 references, and the switches a
// Seedream generation honours. These are the fields this driver sets, with the
// platform's own names — the vendor SDK described the same JSON.

const (
	// imageRequestIDHeader is where the platform stamps the request identity it
	// also puts in an error body.
	imageRequestIDHeader = "X-Client-Request-Id"
	// The sequential-generation switch: "auto" lets the model return a related
	// set, "disabled" keeps one image per call.
	imageSequentialAuto     = "auto"
	imageSequentialDisabled = "disabled"
)

// imageWire is one generation call.
type imageWire struct {
	Model                            string                  `json:"model"`
	Prompt                           string                  `json:"prompt"`
	Image                            any                     `json:"image,omitempty"`
	ResponseFormat                   *string                 `json:"response_format,omitempty"`
	Seed                             *int64                  `json:"seed,omitempty"`
	GuidanceScale                    *float64                `json:"guidance_scale,omitempty"`
	Size                             *string                 `json:"size,omitempty"`
	Watermark                        *bool                   `json:"watermark,omitempty"`
	OptimizePrompt                   *bool                   `json:"optimize_prompt,omitempty"`
	OptimizePromptOptions            *imageOptimizeOptions   `json:"optimize_prompt_options,omitempty"`
	SequentialImageGeneration        *string                 `json:"sequential_image_generation,omitempty"`
	SequentialImageGenerationOptions *imageSequentialOptions `json:"sequential_image_generation_options,omitempty"`
	Tools                            []*videoTool            `json:"tools,omitempty"`
	OutputFormat                     *string                 `json:"output_format,omitempty"`
	LayerDecomposition               *bool                   `json:"layer_decomposition,omitempty"`
	Background                       string                  `json:"background,omitempty"`
}

// imageOptimizeOptions steers the prompt rewrite the platform may perform.
type imageOptimizeOptions struct {
	Mode     string `json:"mode,omitempty"`
	Thinking string `json:"thinking,omitempty"`
}

// imageSequentialOptions bounds a grouped generation.
type imageSequentialOptions struct {
	MaxImages *int `json:"max_images,omitempty"`
}

// imagesResponse is the create answer: the images, and the failure envelope the
// platform may carry on an HTTP 200.
type imagesResponse struct {
	Data []struct {
		URL     string `json:"url"`
		B64JSON string `json:"b64_json"`
		Size    string `json:"size"`
	} `json:"data"`
	Usage *struct {
		OutputTokens int64 `json:"output_tokens"`
		TotalTokens  int64 `json:"total_tokens"`
	} `json:"usage,omitempty"`
	Error *imageFailure `json:"error,omitempty"`
}

// imageFailure is the platform's error body.
type imageFailure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// derefString reads an optional string as "" when unset. Both media surfaces
// carry optional strings the platform answers with or without.
func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
