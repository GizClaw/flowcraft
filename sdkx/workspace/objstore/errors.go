package objstore

import (
	"errors"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

var (
	ErrKeyNotFound   = errdefs.NotFound(errors.New("objstore: key not found"))
	ErrPathTraversal = errdefs.Forbidden(errors.New("objstore: path traversal denied"))
	ErrInvalidKey    = errdefs.Validation(errors.New("objstore: invalid key"))
	ErrDeleteRoot    = errdefs.Validation(errors.New("objstore: refusing to delete root"))
)
