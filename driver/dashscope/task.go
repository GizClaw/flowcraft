package dashscope

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// The async task pipeline is the platform's default shape for media
// generation: the create call answers with a task id, the caller polls
// GET {base}/api/v1/tasks/{task_id} until the task reaches a terminal state,
// and the generated links live in the terminal query response. Task data is
// retained for 24 hours; afterwards the query answers UNKNOWN.
const (
	taskPathPrefix = "/api/v1/tasks/"

	taskStatusPending   = "PENDING"
	taskStatusRunning   = "RUNNING"
	taskStatusSucceeded = "SUCCEEDED"
	taskStatusFailed    = "FAILED"
	taskStatusCanceled  = "CANCELED"
	taskStatusUnknown   = "UNKNOWN"
)

// taskCreate is the envelope every task surface returns from its create
// call: output.task_id plus the initial status. Request-level failures
// arrive as a top-level code/message pair on an HTTP 200 instead, because
// the platform reports them in-band.
type taskCreate struct {
	Output struct {
		TaskID     string `json:"task_id"`
		TaskStatus string `json:"task_status"`
	} `json:"output"`
	RequestID string `json:"request_id"`
	Code      string `json:"code"`
	Message   string `json:"message"`
}

// taskResultItem is one entry of an image task's result list: the link, or
// the failure that kept the entry from producing one.
type taskResultItem struct {
	URL     string `json:"url"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// taskQuery is the envelope the task endpoint returns. Image tasks carry
// their links in output.results (one entry per image, each able to fail on
// its own), video tasks carry output.video_url, and a terminal failure
// carries output.code/output.message.
type taskQuery struct {
	Output struct {
		TaskID     string           `json:"task_id"`
		TaskStatus string           `json:"task_status"`
		Results    []taskResultItem `json:"results"`
		VideoURL   string           `json:"video_url"`
		Code       string           `json:"code"`
		Message    string           `json:"message"`
	} `json:"output"`
	Usage struct {
		ImageCount int64 `json:"image_count"`
		VideoCount int64 `json:"video_count"`
	} `json:"usage"`
	RequestID string `json:"request_id"`
	Code      string `json:"code"`
	Message   string `json:"message"`
}

// createTask posts one task-creating request and returns the task id and the
// create call's request id. The async header is mandatory: the platform
// answers a synchronous call on these endpoints with "current user api does
// not support synchronous calls".
func createTask(
	ctx context.Context,
	client *mediaClient,
	path string,
	body any,
	operation string,
) (taskID, requestID string, err error) {
	var created taskCreate
	if err := client.postJSON(ctx, path, true, body, &created); err != nil {
		return "", "", err
	}
	if created.Code != "" || created.Message != "" {
		return "", "", errorBody{
			Code:      created.Code,
			Message:   created.Message,
			RequestID: created.RequestID,
		}.failure(operation, 0)
	}
	if created.Output.TaskID == "" {
		return "", "", fmt.Errorf(
			"dashscope: %s returned no task_id", operation)
	}
	return created.Output.TaskID, created.RequestID, nil
}

// awaitTask polls the task endpoint until the task reaches a terminal state.
// The wait is the operation, not a retry: a media task takes minutes, and
// the loop ends on the caller's context deadline, on a terminal state, or on
// a wire error the client already classified.
func awaitTask(
	ctx context.Context,
	client *mediaClient,
	interval time.Duration,
	taskID, operation string,
) (taskQuery, error) {
	queryPath := taskPathPrefix + url.PathEscape(taskID)
	for {
		var query taskQuery
		if err := client.getJSON(ctx, queryPath, &query); err != nil {
			return taskQuery{}, err
		}
		if query.Code != "" || query.Message != "" {
			return taskQuery{}, errorBody{
				Code:      query.Code,
				Message:   query.Message,
				RequestID: query.RequestID,
			}.failure(operation+" task query", 0)
		}
		switch strings.ToUpper(query.Output.TaskStatus) {
		case taskStatusSucceeded:
			return query, nil
		case taskStatusFailed, taskStatusCanceled:
			return taskQuery{}, errorBody{
				Code:      query.Output.Code,
				Message:   query.Output.Message,
				RequestID: query.RequestID,
			}.failure(operation, 0)
		case taskStatusUnknown:
			return taskQuery{}, errdefs.NotFound(fmt.Errorf(
				"dashscope: %s task %q is unknown; results are retained for 24 hours",
				operation, taskID,
			))
		case taskStatusPending, taskStatusRunning:
		default:
			// An unrecognized status is a state this driver has not seen
			// before; polling on is the only safe reading, and the
			// caller's deadline bounds it.
		}
		select {
		case <-ctx.Done():
			return taskQuery{}, ctx.Err()
		case <-time.After(interval):
		}
	}
}

// imageURLs reads the generated links out of a succeeded image task. An
// entry may carry its own failure code while its siblings succeed, so the
// successful links are returned and a wholly failed task reports the first
// per-entry error rather than an empty result.
func imageURLs(query taskQuery) ([]string, error) {
	urls := make([]string, 0, len(query.Output.Results))
	var firstFailure errorBody
	for _, result := range query.Output.Results {
		if result.URL != "" {
			urls = append(urls, result.URL)
			continue
		}
		if firstFailure.Code == "" && firstFailure.Message == "" {
			firstFailure = errorBody{Code: result.Code, Message: result.Message}
		}
	}
	if len(urls) == 0 {
		if firstFailure.Code != "" || firstFailure.Message != "" {
			return nil, firstFailure.failure("image generation", 0)
		}
		return nil, fmt.Errorf("dashscope: image generation returned no images")
	}
	return urls, nil
}
