package retrieval

import "testing"

func TestListPageTokenBoundToFilterAndOrder(t *testing.T) {
	req := ListRequest{
		Filter:  Filter{Eq: map[string]any{"tenant": "a"}},
		OrderBy: OrderByIDAsc,
	}
	tok, err := EncodeListPageTokenFor(10, req)
	if err != nil {
		t.Fatal(err)
	}
	if off, err := DecodeListPageTokenFor(tok, req); err != nil || off != 10 {
		t.Fatalf("DecodeListPageTokenFor same request off=%d err=%v", off, err)
	}
	if _, err := DecodeListPageTokenFor(tok, ListRequest{
		Filter:  Filter{Eq: map[string]any{"tenant": "b"}},
		OrderBy: OrderByIDAsc,
	}); err == nil {
		t.Fatal("expected token/filter mismatch error")
	}
	if _, err := DecodeListPageTokenFor(tok, ListRequest{
		Filter:  Filter{Eq: map[string]any{"tenant": "a"}},
		OrderBy: OrderByTimestampAsc,
	}); err == nil {
		t.Fatal("expected token/order mismatch error")
	}
}

func TestListPageTokenLegacyOffsetStillAccepted(t *testing.T) {
	tok, err := EncodeListPageToken(7)
	if err != nil {
		t.Fatal(err)
	}
	if off, err := DecodeListPageTokenFor(tok, ListRequest{
		Filter: Filter{Eq: map[string]any{"tenant": "changed"}},
	}); err != nil || off != 7 {
		t.Fatalf("legacy token off=%d err=%v", off, err)
	}
}
