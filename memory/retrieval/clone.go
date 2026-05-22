package retrieval

import "maps"

// CloneDoc returns a copy of d whose mutable slices/maps are not shared.
func CloneDoc(d Doc) Doc {
	out := d
	if d.Vector != nil {
		out.Vector = append([]float32(nil), d.Vector...)
	}
	if d.Metadata != nil {
		out.Metadata = maps.Clone(d.Metadata)
	}
	if d.SparseVector != nil {
		out.SparseVector = maps.Clone(d.SparseVector)
	}
	return out
}

// CloneHit returns a copy of h whose mutable maps/slices are not shared.
func CloneHit(h Hit) Hit {
	out := h
	out.Doc = CloneDoc(h.Doc)
	if h.Scores != nil {
		out.Scores = maps.Clone(h.Scores)
	}
	return out
}

// CloneHits clones a hit slice and every hit's mutable maps/slices.
func CloneHits(in []Hit) []Hit {
	out := make([]Hit, len(in))
	for i := range in {
		out[i] = CloneHit(in[i])
	}
	return out
}
