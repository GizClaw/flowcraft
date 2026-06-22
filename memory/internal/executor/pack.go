package executor

import (
	"context"
	"maps"
	"strings"
	"sync"

	"github.com/GizClaw/flowcraft/memory/derive"
	"github.com/GizClaw/flowcraft/memory/retrieval"
	sourcemessage "github.com/GizClaw/flowcraft/memory/sources/message"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// PackContext loads a recent message window and appends explicitly requested
// retrieval results in deterministic order.
func (r *Executor) PackContext(ctx context.Context, req PackContextRequest) (*ContextPack, error) {
	if r.recentWindow == nil {
		return nil, errdefs.NotAvailablef("%s: recent window is not configured", errPrefix)
	}

	window, err := r.recentWindow.Load(ctx, req.Window)
	if err != nil {
		return nil, err
	}

	pack := &ContextPack{
		Window: window,
		Items:  make([]derive.ContextItem, 0, len(window.Messages)),
	}

	recentItems := make([]derive.ContextItem, 0, len(window.Messages))
	for i := range window.Messages {
		msg := window.Messages[i]
		text := renderMessageText(msg)
		if strings.TrimSpace(text) == "" {
			continue
		}
		recentItems = append(recentItems, derive.ContextItem{
			Kind:    derive.ContextItemRecentMessage,
			Text:    text,
			Message: &msg,
		})
	}

	searches, err := r.runPackContextSearches(ctx, req)
	if err != nil {
		return nil, err
	}
	pack.MessageHits = searches.MessageHits
	pack.SummaryHits = searches.SummaryHits
	pack.DocumentHits = searches.DocumentHits
	pack.EntityHits = searches.EntityHits
	appendPackContextSearchItems(pack)
	appendRecentContextItems(pack, recentItems)

	if r.contextPacker != nil {
		if err := r.applyContextPacker(ctx, req, pack); err != nil {
			return nil, err
		}
	}

	return pack, nil
}

type packContextSearches struct {
	MessageHits  []derive.SourceMessageSearchHit
	SummaryHits  []derive.SummaryNodeSearchHit
	DocumentHits []derive.DocumentChunkSearchHit
	EntityHits   []derive.EntityFactSearchHit
}

type packContextSearchTask func(context.Context) packContextSearchResult

type packContextSearchResult struct {
	apply func(*packContextSearches)
	err   error
}

func (r *Executor) runPackContextSearches(ctx context.Context, req PackContextRequest) (packContextSearches, error) {
	tasks := r.packContextSearchTasks(req)
	if len(tasks) == 0 {
		return packContextSearches{}, nil
	}

	searchCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	results := make(chan packContextSearchResult, len(tasks))
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	recordErr := func(err error) {
		if err == nil {
			return
		}
		shouldCancel := false
		mu.Lock()
		if firstErr == nil {
			firstErr = err
			shouldCancel = true
		}
		mu.Unlock()
		if shouldCancel {
			cancel()
		}
	}

	for _, task := range tasks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result := task(searchCtx)
			recordErr(result.err)
			results <- result
		}()
	}

	wg.Wait()
	close(results)

	mu.Lock()
	err := firstErr
	mu.Unlock()
	if err != nil {
		return packContextSearches{}, err
	}

	var searches packContextSearches
	for result := range results {
		if result.apply != nil {
			result.apply(&searches)
		}
	}
	return searches, nil
}

func (r *Executor) packContextSearchTasks(req PackContextRequest) []packContextSearchTask {
	tasks := make([]packContextSearchTask, 0, 3)
	if req.MessageSearch != nil {
		search := clonePackContextSearchRequest(req.MessageSearch)
		namespace := req.MessageNamespace
		tasks = append(tasks, func(ctx context.Context) packContextSearchResult {
			resp, err := r.SearchSourceMessages(ctx, search, namespace)
			if err != nil {
				return packContextSearchResult{err: err}
			}
			return packContextSearchResult{apply: func(out *packContextSearches) {
				out.MessageHits = resp.Hits
			}}
		})
	}
	if req.SummarySearch != nil {
		search := clonePackContextSearchRequest(req.SummarySearch)
		namespace := req.SummaryNamespace
		tasks = append(tasks, func(ctx context.Context) packContextSearchResult {
			resp, err := r.SearchSummaryNodes(ctx, search, namespace, req.SummaryRetrieval)
			if err != nil {
				return packContextSearchResult{err: err}
			}
			return packContextSearchResult{apply: func(out *packContextSearches) {
				out.SummaryHits = resp.Hits
			}}
		})
	}
	if req.DocumentSearch != nil {
		search := clonePackContextSearchRequest(req.DocumentSearch)
		namespace := req.DocumentNamespace
		tasks = append(tasks, func(ctx context.Context) packContextSearchResult {
			resp, err := r.SearchDocumentChunks(ctx, search, namespace)
			if err != nil {
				return packContextSearchResult{err: err}
			}
			return packContextSearchResult{apply: func(out *packContextSearches) {
				out.DocumentHits = resp.Hits
			}}
		})
	}
	if req.EntityFactSearch != nil {
		search := clonePackContextSearchRequest(req.EntityFactSearch)
		namespace := req.EntityFactNamespace
		tasks = append(tasks, func(ctx context.Context) packContextSearchResult {
			resp, err := r.SearchEntityFacts(ctx, search, namespace)
			if err != nil {
				return packContextSearchResult{err: err}
			}
			return packContextSearchResult{apply: func(out *packContextSearches) {
				out.EntityHits = resp.Hits
			}}
		})
	}
	return tasks
}

func appendPackContextSearchItems(pack *ContextPack) {
	for i := range pack.MessageHits {
		msg := pack.MessageHits[i].Message
		hit := pack.MessageHits[i].Retrieval
		text := renderMessageText(msg)
		if strings.TrimSpace(text) == "" {
			continue
		}
		pack.Items = append(pack.Items, derive.ContextItem{
			Kind:      derive.ContextItemRecentMessage,
			Text:      text,
			Message:   &msg,
			Retrieval: &hit,
		})
	}
	for i := range pack.SummaryHits {
		node := pack.SummaryHits[i].Node
		hit := pack.SummaryHits[i].Retrieval
		text := node.Summary
		if strings.TrimSpace(text) == "" {
			continue
		}
		pack.Items = append(pack.Items, derive.ContextItem{
			Kind:        derive.ContextItemSummaryNode,
			Text:        text,
			SummaryNode: &node,
			Retrieval:   &hit,
		})
	}
	for i := range pack.DocumentHits {
		chunk := pack.DocumentHits[i].Chunk
		hit := pack.DocumentHits[i].Retrieval
		text := chunk.Text
		if strings.TrimSpace(text) == "" {
			continue
		}
		pack.Items = append(pack.Items, derive.ContextItem{
			Kind:          derive.ContextItemDocumentChunk,
			Text:          text,
			DocumentChunk: &chunk,
			Retrieval:     &hit,
		})
	}
	for i := range pack.EntityHits {
		fact := pack.EntityHits[i].Fact
		hit := pack.EntityHits[i].Retrieval
		text := fact.FactText
		if strings.TrimSpace(text) == "" {
			continue
		}
		pack.Items = append(pack.Items, derive.ContextItem{
			Kind:       derive.ContextItemEntityFact,
			Text:       text,
			EntityFact: &fact,
			Retrieval:  &hit,
		})
	}
}

func appendRecentContextItems(pack *ContextPack, recentItems []derive.ContextItem) {
	seen := make(map[string]struct{}, len(pack.Items))
	for _, item := range pack.Items {
		if key := contextItemDedupeKey(item); key != "" {
			seen[key] = struct{}{}
		}
	}
	for _, item := range recentItems {
		if key := contextItemDedupeKey(item); key != "" {
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
		}
		pack.Items = append(pack.Items, item)
	}
}

func contextItemDedupeKey(item derive.ContextItem) string {
	if item.Message != nil {
		if item.Message.ConversationID != "" && item.Message.ID != "" {
			return "message:" + item.Message.ConversationID + ":" + item.Message.ID
		}
	}
	if item.Retrieval != nil && strings.TrimSpace(item.Retrieval.Doc.ID) != "" {
		return "retrieval:" + strings.TrimSpace(item.Retrieval.Doc.ID)
	}
	if strings.TrimSpace(item.Text) != "" {
		return "text:" + string(item.Kind) + ":" + strings.TrimSpace(item.Text)
	}
	return ""
}

func clonePackContextSearchRequest(in *retrieval.SearchRequest) retrieval.SearchRequest {
	out := *in
	out.QueryVector = append([]float32(nil), in.QueryVector...)
	out.SparseVec = cloneFloat32Map(in.SparseVec)
	out.Filter = cloneRetrievalFilter(in.Filter)
	out.HybridOptions.Weights = cloneSearchSignalWeights(in.HybridOptions.Weights)
	return out
}

func cloneRetrievalFilter(in retrieval.Filter) retrieval.Filter {
	out := in
	out.And = cloneRetrievalFilters(in.And)
	out.Or = cloneRetrievalFilters(in.Or)
	if in.Not != nil {
		not := cloneRetrievalFilter(*in.Not)
		out.Not = &not
	}
	out.Eq = cloneAnyMap(in.Eq)
	out.Neq = cloneAnyMap(in.Neq)
	out.In = cloneStringAnyListMap(in.In)
	out.NotIn = cloneStringAnyListMap(in.NotIn)
	out.Range = cloneRangeMap(in.Range)
	out.Exists = append([]string(nil), in.Exists...)
	out.Missing = append([]string(nil), in.Missing...)
	out.Match = cloneStringMap(in.Match)
	out.Contains = cloneAnyMap(in.Contains)
	out.IContains = cloneAnyMap(in.IContains)
	out.ContainsAny = cloneStringAnyListMap(in.ContainsAny)
	out.ContainsAll = cloneStringAnyListMap(in.ContainsAll)
	return out
}

func cloneRetrievalFilters(in []retrieval.Filter) []retrieval.Filter {
	if in == nil {
		return nil
	}
	out := make([]retrieval.Filter, len(in))
	for i, filter := range in {
		out[i] = cloneRetrievalFilter(filter)
	}
	return out
}

func cloneFloat32Map(in map[string]float32) map[string]float32 {
	if in == nil {
		return nil
	}
	out := make(map[string]float32, len(in))
	maps.Copy(out, in)
	return out
}

func cloneSearchSignalWeights(in map[retrieval.SearchSignal]float64) map[retrieval.SearchSignal]float64 {
	if in == nil {
		return nil
	}
	out := make(map[retrieval.SearchSignal]float64, len(in))
	maps.Copy(out, in)
	return out
}

func cloneStringAnyListMap(in map[string][]any) map[string][]any {
	if in == nil {
		return nil
	}
	out := make(map[string][]any, len(in))
	for key, values := range in {
		out[key] = make([]any, len(values))
		for i, value := range values {
			out[key][i] = cloneAny(value)
		}
	}
	return out
}

func cloneRangeMap(in map[string]retrieval.Range) map[string]retrieval.Range {
	if in == nil {
		return nil
	}
	out := make(map[string]retrieval.Range, len(in))
	maps.Copy(out, in)
	return out
}

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	maps.Copy(out, in)
	return out
}

func renderMessageText(msg sourcemessage.Message) string {
	content := msg.Content()
	if strings.TrimSpace(content) == "" {
		return ""
	}
	return string(msg.Role) + ": " + content
}
