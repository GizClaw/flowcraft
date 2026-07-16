package bbh

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/dgraph-io/badger/v4"

	"github.com/GizClaw/flowcraft/memory/retrieval"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	sdkworkspace "github.com/GizClaw/flowcraft/sdk/workspace"
)

// Inspector provides read-only visibility into a BBH workspace.
type Inspector struct {
	root   string
	db     *badger.DB
	closed atomic.Bool
}

// Inspection is a point-in-time summary of the BBH workspace layout.
type Inspection struct {
	Root                   string                `json:"root"`
	BadgerPath             string                `json:"badger_path"`
	BlevePath              string                `json:"bleve_path"`
	HNSWPath               string                `json:"hnsw_path"`
	BadgerExists           bool                  `json:"badger_exists"`
	BleveExists            bool                  `json:"bleve_exists"`
	HNSWExists             bool                  `json:"hnsw_exists"`
	BadgerSizeBytes        int64                 `json:"badger_size_bytes"`
	BleveSizeBytes         int64                 `json:"bleve_size_bytes"`
	HNSWSizeBytes          int64                 `json:"hnsw_size_bytes"`
	PhysicalNamespaceCount int                   `json:"physical_namespace_count"`
	DocNamespaceCount      int                   `json:"doc_namespace_count"`
	TotalDocs              int64                 `json:"total_docs"`
	TotalVectorDocs        int64                 `json:"total_vector_docs"`
	Namespaces             []NamespaceInspection `json:"namespaces"`
}

// NamespaceInspection describes the physical index artifacts for one
// namespace. Namespace is decoded from the BBH safe token when possible.
type NamespaceInspection struct {
	Namespace      string `json:"namespace"`
	Token          string `json:"token"`
	DecodeError    string `json:"decode_error,omitempty"`
	DocCount       int64  `json:"doc_count"`
	VectorDocCount int64  `json:"vector_doc_count"`
	BadgerBytes    int64  `json:"badger_bytes"`
	BlevePath      string `json:"bleve_path,omitempty"`
	BleveExists    bool   `json:"bleve_exists"`
	BleveSizeBytes int64  `json:"bleve_size_bytes"`
	HNSWPath       string `json:"hnsw_path,omitempty"`
	HNSWExists     bool   `json:"hnsw_exists"`
	HNSWSizeBytes  int64  `json:"hnsw_size_bytes"`
	Empty          bool   `json:"empty"`
	SourceBadger   bool   `json:"source_badger"`
	SourceBleve    bool   `json:"source_bleve"`
	SourceHNSW     bool   `json:"source_hnsw"`
}

// NewInspector opens an offline, read-only inspector rooted at a BBH workspace.
// It opens Badger with a read-only lock, so it cannot inspect a workspace while
// a writable Index is already open on the same path.
func NewInspector(ws *sdkworkspace.LocalWorkspace) (*Inspector, error) {
	if ws == nil {
		return nil, errdefs.Validationf("retrieval/bbh: workspace is nil")
	}
	root := ws.Root()
	in := &Inspector{root: root}
	dbPath := filepath.Join(root, badgerDir)
	info, err := os.Stat(dbPath)
	switch {
	case err == nil && !info.IsDir():
		return nil, fmt.Errorf("retrieval/bbh: badger path is not a directory: %s", dbPath)
	case err == nil:
		db, err := badger.Open(badger.DefaultOptions(dbPath).WithLogger(nil).WithReadOnly(true))
		if err != nil {
			return nil, fmt.Errorf("retrieval/bbh: open badger inspector: %w", err)
		}
		in.db = db
	case os.IsNotExist(err):
	default:
		return nil, fmt.Errorf("retrieval/bbh: stat badger path: %w", err)
	}
	return in, nil
}

// Close releases read-only resources held by the inspector.
func (in *Inspector) Close() error {
	if in == nil || !in.closed.CompareAndSwap(false, true) {
		return nil
	}
	if in.db != nil {
		return in.db.Close()
	}
	return nil
}

// Inspect returns a point-in-time summary of all discovered namespaces.
func (in *Inspector) Inspect(ctx context.Context) (*Inspection, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := in.ensureOpen(); err != nil {
		return nil, err
	}
	out := &Inspection{
		Root:       in.root,
		BadgerPath: filepath.Join(in.root, badgerDir),
		BlevePath:  filepath.Join(in.root, bleveDir),
		HNSWPath:   filepath.Join(in.root, hnswDir),
	}
	var err error
	out.BadgerExists, out.BadgerSizeBytes, err = pathSize(ctx, out.BadgerPath)
	if err != nil {
		return nil, err
	}
	out.BleveExists, out.BleveSizeBytes, err = pathSize(ctx, out.BlevePath)
	if err != nil {
		return nil, err
	}
	out.HNSWExists, out.HNSWSizeBytes, err = pathSize(ctx, out.HNSWPath)
	if err != nil {
		return nil, err
	}

	namespaces := map[string]*NamespaceInspection{}
	if err := in.inspectBadger(ctx, namespaces); err != nil {
		return nil, err
	}
	if err := inspectBleveDirs(ctx, in.root, namespaces); err != nil {
		return nil, err
	}
	if err := inspectHNSWFiles(ctx, in.root, namespaces); err != nil {
		return nil, err
	}

	out.Namespaces = make([]NamespaceInspection, 0, len(namespaces))
	for _, ns := range namespaces {
		ns.Empty = ns.DocCount == 0 && ns.VectorDocCount == 0
		out.TotalDocs += ns.DocCount
		out.TotalVectorDocs += ns.VectorDocCount
		if ns.DocCount > 0 {
			out.DocNamespaceCount++
		}
		out.Namespaces = append(out.Namespaces, *ns)
	}
	out.PhysicalNamespaceCount = len(out.Namespaces)
	sort.Slice(out.Namespaces, func(i, j int) bool {
		if out.Namespaces[i].Namespace == out.Namespaces[j].Namespace {
			return out.Namespaces[i].Token < out.Namespaces[j].Token
		}
		return out.Namespaces[i].Namespace < out.Namespaces[j].Namespace
	})
	return out, nil
}

// InspectNamespace returns inspection data for one namespace.
func (in *Inspector) InspectNamespace(ctx context.Context, namespace string) (NamespaceInspection, error) {
	if strings.TrimSpace(namespace) == "" {
		return NamespaceInspection{}, errdefs.Validationf("retrieval/bbh: namespace is required")
	}
	if err := ctx.Err(); err != nil {
		return NamespaceInspection{}, err
	}
	if err := in.ensureOpen(); err != nil {
		return NamespaceInspection{}, err
	}
	token := safeToken(namespace)
	ns := namespaceFromToken(token)
	if err := in.inspectBadgerNamespace(ctx, token, &ns); err != nil {
		return NamespaceInspection{}, err
	}
	if err := inspectBleveNamespace(ctx, in.root, token, &ns); err != nil {
		return NamespaceInspection{}, err
	}
	if err := inspectHNSWNamespace(ctx, in.root, token, &ns); err != nil {
		return NamespaceInspection{}, err
	}
	ns.Empty = ns.DocCount == 0 && ns.VectorDocCount == 0
	return ns, nil
}

func (in *Inspector) ensureOpen() error {
	if in == nil || in.closed.Load() {
		return errdefs.NotAvailablef("retrieval/bbh: inspector is closed")
	}
	return nil
}

func (in *Inspector) inspectBadgerNamespace(ctx context.Context, token string, ns *NamespaceInspection) error {
	if in.db == nil {
		return nil
	}
	return in.db.View(func(txn *badger.Txn) error {
		it := txn.NewIterator(badger.IteratorOptions{PrefetchValues: true})
		defer it.Close()
		prefix := []byte(badgerDocPrefix + token + "/")
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}
			key := string(it.Item().Key())
			ns.SourceBadger = true
			ns.DocCount++
			ns.BadgerBytes += int64(len(key))
			if err := it.Item().Value(func(v []byte) error {
				ns.BadgerBytes += int64(len(v))
				var d retrieval.Doc
				if err := json.Unmarshal(v, &d); err != nil {
					return err
				}
				if len(d.Vector) > 0 {
					ns.VectorDocCount++
				}
				return nil
			}); err != nil {
				return err
			}
		}
		return nil
	})
}

func (in *Inspector) inspectBadger(ctx context.Context, namespaces map[string]*NamespaceInspection) error {
	if in.db == nil {
		return nil
	}
	return in.db.View(func(txn *badger.Txn) error {
		it := txn.NewIterator(badger.IteratorOptions{PrefetchValues: true})
		defer it.Close()
		prefix := []byte(badgerDocPrefix)
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}
			key := string(it.Item().Key())
			token, ok := namespaceTokenFromDocKey(key)
			if !ok {
				continue
			}
			ns := ensureNamespaceInspection(namespaces, token)
			ns.SourceBadger = true
			ns.DocCount++
			ns.BadgerBytes += int64(len(key))
			if err := it.Item().Value(func(v []byte) error {
				ns.BadgerBytes += int64(len(v))
				var d retrieval.Doc
				if err := json.Unmarshal(v, &d); err != nil {
					return err
				}
				if len(d.Vector) > 0 {
					ns.VectorDocCount++
				}
				return nil
			}); err != nil {
				return err
			}
		}
		return nil
	})
}

func inspectBleveDirs(ctx context.Context, root string, namespaces map[string]*NamespaceInspection) error {
	dir := filepath.Join(root, bleveDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("retrieval/bbh: read bleve dir: %w", err)
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !entry.IsDir() {
			continue
		}
		token := entry.Name()
		ns := ensureNamespaceInspection(namespaces, token)
		ns.SourceBleve = true
		ns.BleveExists = true
		ns.BlevePath = filepath.Join(dir, token)
		var err error
		_, ns.BleveSizeBytes, err = pathSize(ctx, ns.BlevePath)
		if err != nil {
			return err
		}
	}
	return nil
}

func inspectBleveNamespace(ctx context.Context, root, token string, ns *NamespaceInspection) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path := filepath.Join(root, bleveDir, token)
	exists, size, err := pathSize(ctx, path)
	if err != nil {
		return err
	}
	if exists {
		ns.SourceBleve = true
		ns.BleveExists = true
		ns.BlevePath = path
		ns.BleveSizeBytes = size
	}
	return nil
}

func inspectHNSWFiles(ctx context.Context, root string, namespaces map[string]*NamespaceInspection) error {
	dir := filepath.Join(root, hnswDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("retrieval/bbh: read hnsw dir: %w", err)
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), hnswGraphExt) {
			continue
		}
		token := strings.TrimSuffix(entry.Name(), hnswGraphExt)
		ns := ensureNamespaceInspection(namespaces, token)
		ns.SourceHNSW = true
		ns.HNSWExists = true
		ns.HNSWPath = filepath.Join(dir, entry.Name())
		var err error
		_, ns.HNSWSizeBytes, err = pathSize(ctx, ns.HNSWPath)
		if err != nil {
			return err
		}
	}
	return nil
}

func inspectHNSWNamespace(ctx context.Context, root, token string, ns *NamespaceInspection) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path := filepath.Join(root, hnswDir, token+hnswGraphExt)
	exists, size, err := pathSize(ctx, path)
	if err != nil {
		return err
	}
	if exists {
		ns.SourceHNSW = true
		ns.HNSWExists = true
		ns.HNSWPath = path
		ns.HNSWSizeBytes = size
	}
	return nil
}

func ensureNamespaceInspection(namespaces map[string]*NamespaceInspection, token string) *NamespaceInspection {
	if ns, ok := namespaces[token]; ok {
		return ns
	}
	ns := namespaceFromToken(token)
	namespaces[token] = &ns
	return &ns
}

func namespaceFromToken(token string) NamespaceInspection {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return NamespaceInspection{
			Namespace:   token,
			Token:       token,
			DecodeError: err.Error(),
		}
	}
	return NamespaceInspection{
		Namespace: string(raw),
		Token:     token,
	}
}

func namespaceTokenFromDocKey(key string) (string, bool) {
	if !strings.HasPrefix(key, badgerDocPrefix) {
		return "", false
	}
	rest := strings.TrimPrefix(key, badgerDocPrefix)
	token, _, ok := strings.Cut(rest, "/")
	return token, ok && token != ""
}

func pathSize(ctx context.Context, path string) (bool, int64, error) {
	var total int64
	err := filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || os.IsNotExist(err) {
			return false, 0, nil
		}
		return true, total, err
	}
	return true, total, nil
}
