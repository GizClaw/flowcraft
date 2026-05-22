package retrieval

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// listPageCursor is an opaque page token ( List PageToken).
type listPageCursor struct {
	Offset int    `json:"o"`
	Hash   string `json:"h,omitempty"`
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

// EncodeListPageTokenFor encodes a cursor bound to the request's filter/order.
func EncodeListPageTokenFor(offset int, req ListRequest) (string, error) {
	b, err := json.Marshal(listPageCursor{Offset: offset, Hash: listRequestHash(req)})
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

// DecodeListPageTokenFor parses a token produced by EncodeListPageTokenFor.
// Legacy offset-only tokens are accepted, but new tokens are rejected if the
// request's filter/order differs from the request that produced the token.
func DecodeListPageTokenFor(tok string, req ListRequest) (offset int, err error) {
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
	if c.Hash != "" && c.Hash != listRequestHash(req) {
		return 0, errdefs.Validationf("retrieval: page token does not match list request")
	}
	return c.Offset, nil
}

func listRequestHash(req ListRequest) string {
	key := struct {
		Filter  Filter      `json:"filter"`
		OrderBy ListOrderBy `json:"order_by"`
	}{
		Filter:  req.Filter,
		OrderBy: req.OrderBy,
	}
	raw, _ := json.Marshal(key)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:16])
}
