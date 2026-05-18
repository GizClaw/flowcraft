package namespace

import (
	"context"

	"github.com/GizClaw/flowcraft/sdk/retrieval"
)

// CopyOptions configures CopyNamespace.
type CopyOptions struct {
	// BatchSize controls scan and upsert batches. Values <= 0 use 256.
	BatchSize int
	// DropSource removes the source namespace after all documents are copied.
	// Prefer leaving this false for the first deploy, then dropping legacy data
	// after verification.
	DropSource bool
}

// CopyResult reports the outcome of a namespace compatibility migration.
type CopyResult struct {
	From   string
	To     string
	Copied int64
}

// CopyNamespace copies every document from one namespace to another using the
// generic retrieval.Index surface.
//
// It is intentionally conservative: by default the source namespace remains in
// place so operators can verify the destination before deleting legacy data.
func CopyNamespace(ctx context.Context, idx retrieval.Index, from, to string, opts CopyOptions) (CopyResult, error) {
	if opts.BatchSize <= 0 {
		opts.BatchSize = 256
	}
	res := CopyResult{From: from, To: to}
	if idx == nil || from == "" || to == "" || from == to {
		return res, nil
	}
	var copiedIDs []string
	flush := func(docs []retrieval.Doc) error {
		if len(docs) == 0 {
			return nil
		}
		if err := idx.Upsert(ctx, to, docs); err != nil {
			return err
		}
		res.Copied += int64(len(docs))
		if opts.DropSource {
			for _, d := range docs {
				copiedIDs = append(copiedIDs, d.ID)
			}
		}
		return nil
	}
	if it, ok := idx.(retrieval.Iterable); ok {
		cursor := ""
		for {
			docs, next, err := it.Iterate(ctx, from, cursor, opts.BatchSize)
			if err != nil {
				return res, err
			}
			if err := flush(docs); err != nil {
				return res, err
			}
			if next == "" || next == cursor {
				break
			}
			cursor = next
		}
	} else {
		token := ""
		for {
			page, err := idx.List(ctx, from, retrieval.ListRequest{
				PageSize:   opts.BatchSize,
				PageToken:  token,
				WithVector: true,
			})
			if err != nil {
				return res, err
			}
			if page == nil {
				break
			}
			if err := flush(page.Items); err != nil {
				return res, err
			}
			if page.NextPageToken == "" {
				break
			}
			token = page.NextPageToken
		}
	}
	if !opts.DropSource || res.Copied == 0 {
		return res, nil
	}
	if d, ok := idx.(retrieval.Droppable); ok {
		return res, d.Drop(ctx, from)
	}
	for len(copiedIDs) > 0 {
		n := opts.BatchSize
		if n > len(copiedIDs) {
			n = len(copiedIDs)
		}
		if err := idx.Delete(ctx, from, copiedIDs[:n]); err != nil {
			return res, err
		}
		copiedIDs = copiedIDs[n:]
	}
	return res, nil
}
