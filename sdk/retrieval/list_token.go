package retrieval

import (
	"encoding/base64"
	"encoding/json"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// listPageCursor is an opaque page token ( List PageToken).
type listPageCursor struct {
	Offset int `json:"o"`
}

// EncodeListPageToken encodes an offset cursor as base64(JSON). Adapter implementations
// may use this helper or define their own opaque format.
func EncodeListPageToken(offset int) (string, error) {
	b, err := json.Marshal(listPageCursor{Offset: offset})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// DecodeListPageToken parses a token produced by EncodeListPageToken.
// Empty token decodes to offset=0.
func DecodeListPageToken(tok string) (offset int, err error) {
	if tok == "" {
		return 0, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil {
		return 0, err
	}
	var c listPageCursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return 0, err
	}
	if c.Offset < 0 {
		return 0, errdefs.Validationf("retrieval: invalid page token")
	}
	return c.Offset, nil
}
