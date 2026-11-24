package bytedance

// The video task pipeline is the platform's own dialect, so its wire types live
// here rather than behind a vendor SDK: a create request, a task query, and the
// answer that names the finished file. Only the fields this driver sets are
// declared, which is what keeps the surface honest about what it supports.
//
// The transport is `driver/internal/httpx`; the paths are below.

const (
	// videoTaskPath creates a generation task.
	videoTaskPath = "/contents/generations/tasks"
)

// videoRequest is one task creation.
type videoRequest struct {
	Model                 string          `json:"model"`
	Content               []*videoContent `json:"content"`
	SafetyIdentifier      *string         `json:"safety_identifier,omitempty"`
	CallbackURL           *string         `json:"callback_url,omitempty"`
	ReturnLastFrame       *bool           `json:"return_last_frame,omitempty"`
	ServiceTier           *string         `json:"service_tier,omitempty"`
	ExecutionExpiresAfter *int64          `json:"execution_expires_after,omitempty"`
	Priority              *int32          `json:"priority,omitempty"`
	GenerateAudio         *bool           `json:"generate_audio,omitempty"`
	Draft                 *bool           `json:"draft,omitempty"`
	CameraFixed           *bool           `json:"camera_fixed,omitempty"`
	Watermark             *bool           `json:"watermark,omitempty"`
	Seed                  *int64          `json:"seed,omitempty"`
	Resolution            *string         `json:"resolution,omitempty"`
	Ratio                 *string         `json:"ratio,omitempty"`
	OutputFormat          *string         `json:"output_format,omitempty"`
	OmniReferenceTaskType *string         `json:"omni_reference_task_type,omitempty"`
	Duration              *int64          `json:"duration,omitempty"`
	Frames                *int64          `json:"frames,omitempty"`
	Tools                 []*videoTool    `json:"tools,omitempty"`
}

// videoContent is one item of the multimodal prompt: text, an image, a video or
// an audio reference, with the role it plays.
type videoContent struct {
	Type     string         `json:"type"`
	Text     *string        `json:"text,omitempty"`
	ImageURL *videoMediaURL `json:"image_url,omitempty"`
	AudioURL *videoMediaURL `json:"audio_url,omitempty"`
	VideoURL *videoMediaURL `json:"video_url,omitempty"`
	Role     *string        `json:"role,omitempty"`
}

// videoMediaURL is the reference shape every media item uses.
type videoMediaURL struct {
	URL string `json:"url"`
}

// videoTool is a task-level tool switch (the platform's only one today is the
// web-search tool, named by its type and nothing else).
type videoTool struct {
	Type string `json:"type"`
}

// The content item types the platform reads.
const (
	videoContentText  = "text"
	videoContentImage = "image_url"
	videoContentVideo = "video_url"
	videoContentAudio = "audio_url"
)

// createdTask is the create answer: the id the query path needs.
type createdTask struct {
	ID string `json:"id"`
}

// taskState is one query answer.
type taskState struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Error  *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
	Content struct {
		VideoURL string `json:"video_url"`
	} `json:"content"`
	Usage struct {
		CompletionTokens int64 `json:"completion_tokens"`
		TotalTokens      int64 `json:"total_tokens"`
	} `json:"usage"`
}

// The task states the query reports.
const (
	taskSucceeded = "succeeded"
	taskFailed    = "failed"
	taskCancelled = "cancelled"
)

// The content-item builders: a reference item carries its media URL and the role
// the platform reads it under ("first_frame", "reference_image", ...).

func itemImage(rawURL, role string) *videoContent {
	return &videoContent{Type: videoContentImage, ImageURL: &videoMediaURL{URL: rawURL}, Role: &role}
}

func itemVideo(rawURL, role string) *videoContent {
	return &videoContent{Type: videoContentVideo, VideoURL: &videoMediaURL{URL: rawURL}, Role: &role}
}

func itemAudio(rawURL, role string) *videoContent {
	return &videoContent{Type: videoContentAudio, AudioURL: &videoMediaURL{URL: rawURL}, Role: &role}
}
