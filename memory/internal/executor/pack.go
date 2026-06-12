package executor

import (
	"context"
	"maps"
	"strings"
	"sync"

	"github.com/GizClaw/flowcraft/memory/retrieval"
	sourcemessage "github.com/GizClaw/flowcraft/memory/sources/message"
	"github.com/GizClaw/flowcraft/memory/views"
	viewentity "github.com/GizClaw/flowcraft/memory/views/entity"
	"github.com/GizClaw/flowcraft/memory/views/fact"
	viewobservation "github.com/GizClaw/flowcraft/memory/views/observation"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// PackContext loads a recent message window and appends any explicitly
// requested retrieval results in deterministic order.
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
		Items:  make([]ContextItem, 0, len(window.Messages)),
	}

	for i := range window.Messages {
		msg := window.Messages[i]
		text := renderMessageText(msg)
		if strings.TrimSpace(text) == "" {
			continue
		}
		pack.Items = append(pack.Items, ContextItem{
			Kind:    ContextItemRecentMessage,
			Text:    text,
			Message: &msg,
		})
	}

	searches, err := r.runPackContextSearches(ctx, req)
	if err != nil {
		return nil, err
	}
	pack.SummaryHits = searches.SummaryHits
	pack.DocumentHits = searches.DocumentHits
	pack.ObservationHits = searches.ObservationHits
	pack.FactHits = searches.FactHits
	pack.FactGraphHits = searches.FactGraphHits
	pack.EntityProfileHits = searches.EntityProfileHits
	pack.EntityTimelineHits = searches.EntityTimelineHits
	appendPackContextSearchItems(pack)

	if r.contextPacker != nil {
		if err := r.applyContextPacker(ctx, req, pack); err != nil {
			return nil, err
		}
	}

	return pack, nil
}

type packContextSearches struct {
	SummaryHits            []SummaryNodeSearchHit
	DocumentHits           []DocumentChunkSearchHit
	ObservationHits        []ObservationSearchHit
	FactHits               []FactSearchHit
	FactGraphHits          []FactGraphSearchHit
	FactGraphExpansionHits []FactGraphSearchHit
	EntityProfileHits      []EntityProfileSearchHit
	EntityTimelineHits     []EntityTimelineSearchHit
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
	searches.FactGraphHits = mergeFactGraphSearchHits(searches.FactGraphHits, searches.FactGraphExpansionHits)
	searches.FactGraphExpansionHits = nil
	return searches, nil
}

func (r *Executor) packContextSearchTasks(req PackContextRequest) []packContextSearchTask {
	tasks := make([]packContextSearchTask, 0, 8)
	if req.SummarySearch != nil {
		search := clonePackContextSearchRequest(req.SummarySearch)
		namespace := req.SummaryNamespace
		tasks = append(tasks, func(ctx context.Context) packContextSearchResult {
			resp, err := r.SearchSummaryNodes(ctx, search, namespace)
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
	if req.ObservationSearch != nil {
		search := clonePackContextSearchRequest(req.ObservationSearch)
		namespace := req.ObservationNamespace
		tasks = append(tasks, func(ctx context.Context) packContextSearchResult {
			resp, err := r.searchObservations(ctx, search, namespace)
			if err != nil {
				return packContextSearchResult{err: err}
			}
			return packContextSearchResult{apply: func(out *packContextSearches) {
				out.ObservationHits = resp.Hits
			}}
		})
	}
	if req.FactSearch != nil {
		search := clonePackContextSearchRequest(req.FactSearch)
		namespace := req.FactNamespace
		tasks = append(tasks, func(ctx context.Context) packContextSearchResult {
			resp, err := r.searchFacts(ctx, search, namespace)
			if err != nil {
				return packContextSearchResult{err: err}
			}
			return packContextSearchResult{apply: func(out *packContextSearches) {
				out.FactHits = resp.Hits
			}}
		})
	}
	if req.FactGraphSearch != nil {
		search := clonePackContextSearchRequest(req.FactGraphSearch)
		namespace := req.FactGraphNamespace
		tasks = append(tasks, func(ctx context.Context) packContextSearchResult {
			resp, err := r.searchFactGraph(ctx, search, namespace)
			if err != nil {
				return packContextSearchResult{err: err}
			}
			return packContextSearchResult{apply: func(out *packContextSearches) {
				out.FactGraphHits = resp.Hits
			}}
		})
	}
	if req.FactGraphExpansion != nil {
		expansion := cloneFactGraphExpansionRequest(*req.FactGraphExpansion)
		if expansion.Scope.IsZero() {
			expansion.Scope = contextPackScope(req)
		}
		tasks = append(tasks, func(ctx context.Context) packContextSearchResult {
			resp, err := r.expandFactGraph(ctx, expansion)
			if err != nil {
				return packContextSearchResult{err: err}
			}
			return packContextSearchResult{apply: func(out *packContextSearches) {
				out.FactGraphExpansionHits = resp.Hits
			}}
		})
	}
	if req.EntityProfileSearch != nil {
		search := clonePackContextSearchRequest(req.EntityProfileSearch)
		namespace := req.EntityProfileNamespace
		tasks = append(tasks, func(ctx context.Context) packContextSearchResult {
			resp, err := r.searchEntityProfiles(ctx, search, namespace)
			if err != nil {
				return packContextSearchResult{err: err}
			}
			return packContextSearchResult{apply: func(out *packContextSearches) {
				out.EntityProfileHits = resp.Hits
			}}
		})
	}
	if req.EntityTimelineSearch != nil {
		search := clonePackContextSearchRequest(req.EntityTimelineSearch)
		namespace := req.EntityTimelineNamespace
		tasks = append(tasks, func(ctx context.Context) packContextSearchResult {
			resp, err := r.searchEntityTimeline(ctx, search, namespace)
			if err != nil {
				return packContextSearchResult{err: err}
			}
			return packContextSearchResult{apply: func(out *packContextSearches) {
				out.EntityTimelineHits = resp.Hits
			}}
		})
	}
	return tasks
}

func appendPackContextSearchItems(pack *ContextPack) {
	for i := range pack.SummaryHits {
		node := pack.SummaryHits[i].Node
		hit := pack.SummaryHits[i].Retrieval
		text := node.Summary
		if strings.TrimSpace(text) == "" {
			continue
		}
		pack.Items = append(pack.Items, ContextItem{
			Kind:        ContextItemSummaryNode,
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
		pack.Items = append(pack.Items, ContextItem{
			Kind:          ContextItemDocumentChunk,
			Text:          text,
			DocumentChunk: &chunk,
			Retrieval:     &hit,
		})
	}
	for i := range pack.ObservationHits {
		observation := pack.ObservationHits[i].Observation
		hit := pack.ObservationHits[i].Retrieval
		text := renderObservationText(observation)
		if strings.TrimSpace(text) == "" {
			continue
		}
		pack.Items = append(pack.Items, ContextItem{
			Kind:        ContextItemObservation,
			Text:        text,
			Observation: &observation,
			Retrieval:   &hit,
		})
	}
	for i := range pack.FactHits {
		record := pack.FactHits[i].Fact
		hit := pack.FactHits[i].Retrieval
		text := renderFactText(record)
		if strings.TrimSpace(text) == "" {
			continue
		}
		pack.Items = append(pack.Items, ContextItem{
			Kind:      ContextItemFact,
			Text:      text,
			Fact:      &record,
			Retrieval: &hit,
		})
	}
	for i := range pack.FactGraphHits {
		hit := pack.FactGraphHits[i].Retrieval
		if pack.FactGraphHits[i].Node != nil {
			node := *pack.FactGraphHits[i].Node
			text := renderFactGraphNodeText(node)
			if strings.TrimSpace(text) != "" {
				pack.Items = append(pack.Items, ContextItem{
					Kind:          ContextItemFactGraphNode,
					Text:          text,
					FactGraphNode: &node,
					Retrieval:     &hit,
				})
			}
		}
		if pack.FactGraphHits[i].Edge != nil {
			edge := *pack.FactGraphHits[i].Edge
			text := renderFactGraphEdgeText(edge)
			if strings.TrimSpace(text) != "" {
				pack.Items = append(pack.Items, ContextItem{
					Kind:          ContextItemFactGraphEdge,
					Text:          text,
					FactGraphEdge: &edge,
					Retrieval:     &hit,
				})
			}
		}
	}
	for i := range pack.EntityProfileHits {
		profile := pack.EntityProfileHits[i].Profile
		hit := pack.EntityProfileHits[i].Retrieval
		text := renderEntityProfileText(profile)
		if strings.TrimSpace(text) == "" {
			continue
		}
		pack.Items = append(pack.Items, ContextItem{
			Kind:          ContextItemEntityProfile,
			Text:          text,
			EntityProfile: &profile,
			Retrieval:     &hit,
		})
	}
	for i := range pack.EntityTimelineHits {
		event := pack.EntityTimelineHits[i].Event
		hit := pack.EntityTimelineHits[i].Retrieval
		text := renderEntityEventText(event)
		if strings.TrimSpace(text) == "" {
			continue
		}
		pack.Items = append(pack.Items, ContextItem{
			Kind:        ContextItemEntityTimeline,
			Text:        text,
			EntityEvent: &event,
			Retrieval:   &hit,
		})
	}
}

func clonePackContextSearchRequest(in *retrieval.SearchRequest) retrieval.SearchRequest {
	out := *in
	out.QueryVector = append([]float32(nil), in.QueryVector...)
	out.SparseVec = cloneFloat32Map(in.SparseVec)
	out.Filter = cloneRetrievalFilter(in.Filter)
	out.HybridOptions.Weights = cloneSearchSignalWeights(in.HybridOptions.Weights)
	return out
}

func cloneFactGraphExpansionRequest(in FactGraphExpansionRequest) FactGraphExpansionRequest {
	out := in
	out.Search = clonePackContextSearchRequest(&in.Search)
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

func renderObservationText(observation viewobservation.Observation) string {
	return renderTriple(observation.Subject, observation.Predicate, observation.Object)
}

func renderFactText(record fact.Fact) string {
	return renderTriple(record.Subject, record.Predicate, record.Object)
}

func renderFactGraphNodeText(node fact.Node) string {
	return strings.TrimSpace(node.Label)
}

func renderFactGraphEdgeText(edge fact.Edge) string {
	return renderTriple(string(edge.From), edge.Predicate, string(edge.To))
}

func renderEntityProfileText(profile viewentity.ProfileRecord) string {
	parts := []string{strings.TrimSpace(profile.Label)}
	if strings.TrimSpace(profile.Summary) != "" {
		parts = append(parts, strings.TrimSpace(profile.Summary))
	}
	for _, slot := range profile.Slots {
		if strings.TrimSpace(slot.Name) != "" && strings.TrimSpace(slot.Value) != "" {
			parts = append(parts, strings.TrimSpace(slot.Name)+": "+strings.TrimSpace(slot.Value))
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func renderEntityEventText(event viewentity.Event) string {
	return strings.TrimSpace(strings.Join([]string{
		strings.TrimSpace(event.Title),
		strings.TrimSpace(event.Description),
	}, "\n"))
}

func renderTriple(subject, predicate, object string) string {
	parts := []string{
		strings.TrimSpace(subject),
		strings.TrimSpace(predicate),
		strings.TrimSpace(object),
	}
	return strings.TrimSpace(strings.Join(parts, " "))
}

func mergeFactGraphSearchHits(left, right []FactGraphSearchHit) []FactGraphSearchHit {
	if len(left) == 0 {
		return dedupeFactGraphSearchHits(right)
	}
	if len(right) == 0 {
		return dedupeFactGraphSearchHits(left)
	}
	out := make([]FactGraphSearchHit, 0, len(left)+len(right))
	seen := map[string]bool{}
	appendHit := func(hit FactGraphSearchHit) {
		key := factGraphSearchHitKey(hit)
		if key == "" {
			out = append(out, hit)
			return
		}
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, hit)
	}
	for _, hit := range left {
		appendHit(hit)
	}
	for _, hit := range right {
		appendHit(hit)
	}
	return out
}

func dedupeFactGraphSearchHits(in []FactGraphSearchHit) []FactGraphSearchHit {
	if len(in) == 0 {
		return nil
	}
	out := make([]FactGraphSearchHit, 0, len(in))
	seen := map[string]bool{}
	for _, hit := range in {
		key := factGraphSearchHitKey(hit)
		if key != "" {
			if seen[key] {
				continue
			}
			seen[key] = true
		}
		out = append(out, hit)
	}
	return out
}

func factGraphSearchHitKey(hit FactGraphSearchHit) string {
	switch {
	case hit.Node != nil:
		return "node:" + string(hit.Node.ID)
	case hit.Edge != nil:
		return "edge:" + string(hit.Edge.ID)
	case strings.TrimSpace(hit.Retrieval.Doc.ID) != "":
		return "retrieval:" + hit.Retrieval.Doc.ID
	default:
		return ""
	}
}

func factGraphScopeHardMatches(requested, record views.Scope) bool {
	if requested.IsZero() {
		return true
	}
	return strings.TrimSpace(record.RuntimeID) == strings.TrimSpace(requested.RuntimeID) &&
		strings.TrimSpace(record.UserID) == strings.TrimSpace(requested.UserID)
}

func factGraphScopeSoftMatches(requested, record views.Scope) bool {
	if requested.IsZero() {
		return true
	}
	return softScopeFieldMatches(requested.AgentID, record.AgentID) &&
		softScopeFieldMatches(requested.ConversationID, record.ConversationID) &&
		softScopeFieldMatches(requested.DatasetID, record.DatasetID) &&
		softScopeFieldMatches(requested.EntityID, record.EntityID)
}

func factGraphScopeMatches(requested, record views.Scope) bool {
	return factGraphScopeHardMatches(requested, record) && factGraphScopeSoftMatches(requested, record)
}

func softScopeFieldMatches(requested, record string) bool {
	requested = strings.TrimSpace(requested)
	record = strings.TrimSpace(record)
	return requested == "" || record == "" || requested == record
}
