package retrieval

// ListOrderBy controls stable ordering for Index.List.
type ListOrderBy string

const (
	OrderByTimestampDesc ListOrderBy = ""
	OrderByTimestampAsc  ListOrderBy = "ts_asc"
	OrderByIDAsc         ListOrderBy = "id_asc"
)

// ListRequest is a management-style scan (no ranking query).
type ListRequest struct {
	Filter     Filter
	PageSize   int
	PageToken  string
	OrderBy    ListOrderBy
	Project    []string
	WithVector bool
}

// ListResponse is one page of List results.
type ListResponse struct {
	Items         []Doc
	NextPageToken string
	Total         int64
}
