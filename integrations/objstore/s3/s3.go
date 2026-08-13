// Package s3 provides an ObjectStore implementation for any S3-compatible
// service (AWS S3, MinIO, Cloudflare R2, Alibaba OSS S3-compatible mode).
//
// Usage with AWS SDK v2:
//
//	cfg, _ := config.LoadDefaultConfig(ctx)
//	client := s3sdk.NewFromConfig(cfg)
//	store := s3.New(client, "my-bucket")
//	ws := objstore.NewWorkspace(store, objstore.WithPrefix("workspace/prod"))
package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/GizClaw/flowcraft/integrations/objstore"
)

// Client is the minimal S3 client interface. It is satisfied by the real
// AWS SDK v2 s3.Client without importing it here.
//
// Each method mirrors the corresponding S3 API:
//   - GetObject, PutObject, DeleteObject, HeadObject,
//     ListObjectsV2, DeleteObjects
type Client interface {
	GetObject(ctx context.Context, input *GetObjectInput) (*GetObjectOutput, error)
	PutObject(ctx context.Context, input *PutObjectInput) error
	DeleteObject(ctx context.Context, input *DeleteObjectInput) error
	HeadObject(ctx context.Context, input *HeadObjectInput) (*HeadObjectOutput, error)
	ListObjectsV2(ctx context.Context, input *ListObjectsV2Input) (*ListObjectsV2Output, error)
	DeleteObjects(ctx context.Context, input *DeleteObjectsInput) error
}

// --- S3-like request/response types (minimal subset) ---

type GetObjectInput struct {
	Bucket string
	Key    string
}

type GetObjectOutput struct {
	Body io.ReadCloser
}

type PutObjectInput struct {
	Bucket string
	Key    string
	Body   io.Reader
}

type DeleteObjectInput struct {
	Bucket string
	Key    string
}

type HeadObjectInput struct {
	Bucket string
	Key    string
}

type HeadObjectOutput struct {
	objstore.ObjectInfo
}

type ListObjectsV2Input struct {
	Bucket            string
	Prefix            string
	Delimiter         string
	ContinuationToken *string
}

type ListObjectsV2Output struct {
	Contents       []objstore.ObjectInfo
	CommonPrefixes []string
	// NextContinuationToken is set when more keys are available. A nil or
	// empty value means the listing is complete.
	NextContinuationToken *string
}

type DeleteObjectsInput struct {
	Bucket string
	Keys   []string
}

// Store implements objstore.ObjectStore backed by an S3-compatible service.
type Store struct {
	client Client
	bucket string
}

// New creates an S3-backed ObjectStore.
func New(client Client, bucket string) *Store {
	return &Store{client: client, bucket: bucket}
}

func (s *Store) Get(ctx context.Context, key string) ([]byte, error) {
	out, err := s.client.GetObject(ctx, &GetObjectInput{
		Bucket: s.bucket,
		Key:    key,
	})
	if err != nil {
		if isNotFoundError(err) {
			return nil, fmt.Errorf("%w: %s", objstore.ErrKeyNotFound, key)
		}
		return nil, fmt.Errorf("s3: get %q: %w", key, err)
	}
	defer func() { _ = out.Body.Close() }()
	return io.ReadAll(out.Body)
}

func (s *Store) Put(ctx context.Context, key string, data []byte) error {
	return s.client.PutObject(ctx, &PutObjectInput{
		Bucket: s.bucket,
		Key:    key,
		Body:   bytes.NewReader(data),
	})
}

func (s *Store) Del(ctx context.Context, key string) error {
	return s.client.DeleteObject(ctx, &DeleteObjectInput{
		Bucket: s.bucket,
		Key:    key,
	})
}

func (s *Store) Head(ctx context.Context, key string) (objstore.ObjectInfo, error) {
	out, err := s.client.HeadObject(ctx, &HeadObjectInput{
		Bucket: s.bucket,
		Key:    key,
	})
	if err != nil {
		if isNotFoundError(err) {
			return objstore.ObjectInfo{}, fmt.Errorf("%w: %s", objstore.ErrKeyNotFound, key)
		}
		return objstore.ObjectInfo{}, fmt.Errorf("s3: head %q: %w", key, err)
	}
	return out.ObjectInfo, nil
}

func (s *Store) ListPrefix(ctx context.Context, prefix, delimiter string) (*objstore.ListResult, error) {
	out, err := s.client.ListObjectsV2(ctx, &ListObjectsV2Input{
		Bucket:    s.bucket,
		Prefix:    prefix,
		Delimiter: delimiter,
	})
	if err != nil {
		return nil, fmt.Errorf("s3: list prefix %q: %w", prefix, err)
	}
	return &objstore.ListResult{
		Objects:        out.Contents,
		CommonPrefixes: out.CommonPrefixes,
	}, nil
}

func (s *Store) DelPrefix(ctx context.Context, prefix string) error {
	// Collect all keys with this prefix across every page, then batch
	// delete. S3 caps a single listing page at 1000 keys, so a prefix with
	// more objects would silently retain the tail without pagination.
	var token *string
	var allKeys []string
	for {
		out, err := s.client.ListObjectsV2(ctx, &ListObjectsV2Input{
			Bucket:            s.bucket,
			Prefix:            prefix,
			ContinuationToken: token,
		})
		if err != nil {
			return fmt.Errorf("s3: list for delete %q: %w", prefix, err)
		}
		for _, obj := range out.Contents {
			allKeys = append(allKeys, obj.Key)
		}
		if out.NextContinuationToken == nil || *out.NextContinuationToken == "" {
			break
		}
		token = out.NextContinuationToken
	}
	if len(allKeys) == 0 {
		return nil
	}

	// S3 DeleteObjects supports up to 1000 keys per request.
	for i := 0; i < len(allKeys); i += 1000 {
		end := i + 1000
		if end > len(allKeys) {
			end = len(allKeys)
		}
		if err := s.client.DeleteObjects(ctx, &DeleteObjectsInput{
			Bucket: s.bucket,
			Keys:   allKeys[i:end],
		}); err != nil {
			return fmt.Errorf("s3: batch delete: %w", err)
		}
	}
	return nil
}

// isNotFoundError reports whether an S3 client error means the key does
// not exist. Only these shapes map to objstore.ErrKeyNotFound; transient
// transport, auth, and permission failures must surface as themselves so
// callers do not mistake an outage for a missing object.
func isNotFoundError(err error) bool {
	if errors.Is(err, objstore.ErrKeyNotFound) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "nosuchkey") ||
		strings.Contains(msg, "notfound") ||
		strings.Contains(msg, "no such key")
}

var _ objstore.ObjectStore = (*Store)(nil)

// --- Adapter helper for real AWS SDK v2 ---
//
// To bridge the real AWS SDK v2 s3.Client to this package's Client
// interface, create a thin adapter. Example:
//
//	type awsAdapter struct{ client *s3sdk.Client }
//
//	func (a *awsAdapter) GetObject(ctx context.Context, in *s3.GetObjectInput) (*s3.GetObjectOutput, error) {
//	    out, err := a.client.GetObject(ctx, &s3sdk.GetObjectInput{
//	        Bucket: &in.Bucket,
//	        Key:    &in.Key,
//	    })
//	    if err != nil { return nil, err }
//	    return &s3.GetObjectOutput{Body: out.Body}, nil
//	}
//	// ... same pattern for Put, Delete, Head, List, DeleteObjects ...
