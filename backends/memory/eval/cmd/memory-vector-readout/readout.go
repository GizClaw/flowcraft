package main

import (
	"fmt"
	"math"
	"sort"
	"time"

	flowmemory "github.com/GizClaw/flowcraft/backends/memory"
	"github.com/GizClaw/flowcraft/backends/memory/eval"
	"github.com/GizClaw/flowcraft/backends/memory/projection/vector"
)

// reading is the archived form of one run: what was read, under which
// deployment, and the numbers themselves. Durations serialize as nanoseconds.
type reading struct {
	Tool             string            `json:"tool"`
	StartedAt        time.Time         `json:"started_at"`
	Revision         string            `json:"revision"`
	VCSModified      bool              `json:"vcs_modified"`
	Deploy           string            `json:"deploy"`
	Library          eval.LibraryState `json:"library"`
	PolicyDigest     string            `json:"policy_digest"`
	RetrievalSummary string            `json:"retrieval_summary"`
	RecentMaxItems   int               `json:"recent_max_items"`
	RecentMaxTokens  int               `json:"recent_max_tokens"`
	MaxItems         int               `json:"max_items"`
	MaxTokens        int               `json:"max_tokens"`
	Scopes           int               `json:"scopes"`
	Workers          int               `json:"workers"`
	Cold             []coldReading     `json:"cold,omitempty"`
	Hot              *hotReading       `json:"hot,omitempty"`
	// Resident is what the lane's store kept decoded when the run ended: the
	// tables, the bytes they are reckoned at, and what the reads did with
	// them. It is absolute -- what the pass left -- because the reading of a
	// budget is the state it ends in.
	Resident        residentReading `json:"resident"`
	HeapBeforeBytes uint64          `json:"heap_before_bytes"`
	HeapAfterBytes  uint64          `json:"heap_after_bytes"`
	RSSBeforeBytes  uint64          `json:"rss_before_bytes"`
	RSSAfterBytes   uint64          `json:"rss_after_bytes"`
}

// residentReading is what the lane's store kept decoded, copied out of the
// lane's own type so the archive names the fields it publishes.
type residentReading struct {
	Bases     int   `json:"bases"`
	Bytes     int64 `json:"bytes"`
	Hits      int64 `json:"hits"`
	Misses    int64 `json:"misses"`
	Evictions int64 `json:"evictions"`
	Uncached  int64 `json:"uncached"`
}

func residentOf(stats vector.Stats) residentReading {
	return residentReading{
		Bases:     stats.Resident.Bases,
		Bytes:     stats.Resident.Bytes,
		Hits:      stats.Resident.Hits,
		Misses:    stats.Resident.Misses,
		Evictions: stats.Resident.Evictions,
		Uncached:  stats.Resident.Uncached,
	}
}

// coldReading is one conversation's first query: the query that pays to read
// the shard it names out of storage. ShardLoads is how many tables the query
// materialized and Decodes how many of them the store had to decode rather
// than finding decoded; BytesAdded is the payload those tables added to what
// the store keeps. Scans and Embeds say whether the question was then served
// by the lane or only read its table, which is what a row whose counts are
// below one has to be read as.
type coldReading struct {
	ConversationID string        `json:"conversation_id"`
	Latency        time.Duration `json:"latency_ns"`
	ShardLoad      time.Duration `json:"shard_load_ns"`
	ShardLoads     int64         `json:"shard_loads"`
	Decodes        int64         `json:"decodes"`
	BytesAdded     int64         `json:"bytes_added"`
	HeapDeltaBytes int64         `json:"heap_delta_bytes"`
	Scans          int64         `json:"scans"`
	Embeds         int64         `json:"embeds"`
}

// hotReading is the concurrent pass over every question, measured as the delta
// between two readings taken around it.
type hotReading struct {
	Questions       int                     `json:"questions"`
	Workers         int                     `json:"workers"`
	Wall            time.Duration           `json:"wall_ns"`
	Latency         latencyReading          `json:"latency"`
	Stages          map[string]stageReading `json:"stages"`
	Vector          vectorPhase             `json:"vector"`
	Shares          shareReading            `json:"shares"`
	MeanEmbed       time.Duration           `json:"mean_embed_ns"`
	PerEntryScan    time.Duration           `json:"per_entry_scan_ns"`
	ScannedPerQuery float64                 `json:"scanned_per_query"`
	ScoredPerQuery  float64                 `json:"scored_per_query"`
	Projection      []projectionPoint       `json:"projection"`
	Certification   certification           `json:"certification"`
}

type latencyReading struct {
	Mean   time.Duration `json:"mean_ns"`
	P50    time.Duration `json:"p50_ns"`
	P90    time.Duration `json:"p90_ns"`
	P99    time.Duration `json:"p99_ns"`
	Max    time.Duration `json:"max_ns"`
	Over5s int           `json:"over_5s"`
}

type stageReading struct {
	Total time.Duration `json:"total_ns"`
	Count int           `json:"count"`
}

// vectorPhase is the delta of two lane readings: what one pass did. ShardLoads
// and ShardLoad are the tables the pass materialized and what reading them
// cost; Hits, Decodes, Evictions and Uncached are the store's account of those
// reads -- found decoded, decoded off storage, dropped to stay within the
// budget, and read-and-dropped because one table alone exceeded it.
type vectorPhase struct {
	Queries        int64         `json:"queries"`
	ShardLoads     int64         `json:"shard_loads"`
	ShardErrors    int64         `json:"shard_errors"`
	ShardLoad      time.Duration `json:"shard_load_ns"`
	Hits           int64         `json:"hits"`
	Decodes        int64         `json:"decodes"`
	Evictions      int64         `json:"evictions"`
	Uncached       int64         `json:"uncached"`
	Embeds         int64         `json:"embeds"`
	EmbedTotal     time.Duration `json:"embed_ns"`
	Scans          int64         `json:"scans"`
	ScanTotal      time.Duration `json:"scan_ns"`
	ScoreTotal     time.Duration `json:"score_ns"`
	SortTotal      time.Duration `json:"sort_ns"`
	EntriesScanned int64         `json:"entries_scanned"`
	EntriesScored  int64         `json:"entries_scored"`
}

// walk is the part of the scan spent on the filter match of every entry the
// table holds, as opposed to the cosine of the entries it admitted.
func (phase vectorPhase) walk() time.Duration {
	if phase.ScanTotal < phase.ScoreTotal {
		return 0
	}
	return phase.ScanTotal - phase.ScoreTotal
}

// shareReading is the same time expressed as fractions, because the gate is a
// question about share: which term dominates the lane.
type shareReading struct {
	ScanOfLane    float64 `json:"scan_of_lane"`
	SortOfLane    float64 `json:"sort_of_lane"`
	EmbedOfLane   float64 `json:"embed_of_lane"`
	LoadOfLane    float64 `json:"load_of_lane"`
	ScanOfSearch  float64 `json:"scan_of_search"`
	ScanOfContext float64 `json:"scan_of_context"`
}

// projectionPoint answers what the exact scan would cost at another table
// size, at the per-entry slope this run measured. The embed column it is read
// against is the mean query embed of this run: the scan mattering is a
// question of what it is worth next to the network call that feeds it.
type projectionPoint struct {
	EntriesPerShard int           `json:"entries_per_shard"`
	Scan            time.Duration `json:"scan_ns"`
	Sort            time.Duration `json:"sort_ns"`
}

// phaseDelta subtracts two lane readings. The resident state is not part of the
// phase: it is an absolute, reported once at the end.
func phaseDelta(before, after vector.Stats) vectorPhase {
	return vectorPhase{
		Queries:        after.Queries - before.Queries,
		ShardLoads:     after.ShardLoads - before.ShardLoads,
		ShardErrors:    after.ShardErrors - before.ShardErrors,
		ShardLoad:      after.ShardLoadTotal - before.ShardLoadTotal,
		Hits:           after.Resident.Hits - before.Resident.Hits,
		Decodes:        after.Resident.Misses - before.Resident.Misses,
		Evictions:      after.Resident.Evictions - before.Resident.Evictions,
		Uncached:       after.Resident.Uncached - before.Resident.Uncached,
		Embeds:         after.Embeds - before.Embeds,
		EmbedTotal:     after.EmbedTotal - before.EmbedTotal,
		Scans:          after.Scans - before.Scans,
		ScanTotal:      after.ScanTotal - before.ScanTotal,
		ScoreTotal:     after.ScoreTotal - before.ScoreTotal,
		SortTotal:      after.SortTotal - before.SortTotal,
		EntriesScanned: after.EntriesScanned - before.EntriesScanned,
		EntriesScored:  after.EntriesScored - before.EntriesScored,
	}
}

// certification is what a pass states about its own numbers: a reading a
// report may quote is one where every question reached the lane. A pass where
// the lane served nothing -- it could not read its table, or the scope holds
// none -- measures the other lanes and the materialization it paid for, not
// what one retrieval of this lane costs, and the numbers below it say so
// instead of reading like a fast lane.
type certification struct {
	Certified bool     `json:"certified"`
	Reasons   []string `json:"reasons,omitempty"`
}

// certify compares what the pass asked for against what the lane reported.
// The counters are the lane's own, recorded where the lane serves, so a pass
// whose numbers disagree with the questions did not measure what it claims.
func certify(phase vectorPhase, questions int) certification {
	reasons := make([]string, 0, 4)
	if phase.Queries != int64(questions) {
		reasons = append(reasons, fmt.Sprintf("queries %d != questions %d", phase.Queries, questions))
	}
	if phase.Scans != int64(questions) {
		reasons = append(reasons, fmt.Sprintf("scans %d != questions %d", phase.Scans, questions))
	}
	if phase.Embeds != phase.Queries {
		reasons = append(reasons, fmt.Sprintf("embeds %d != queries %d", phase.Embeds, phase.Queries))
	}
	if phase.ShardErrors != 0 {
		reasons = append(reasons, fmt.Sprintf("shard errors %d", phase.ShardErrors))
	}
	return certification{Certified: len(reasons) == 0, Reasons: reasons}
}

// stageDeltas subtracts two provider stage readings.
func stageDeltas(before, after map[string]flowmemory.StageStat) map[string]stageReading {
	out := make(map[string]stageReading, len(after))
	for stage, stat := range after {
		prior := before[stage]
		out[stage] = stageReading{Total: stat.Total - prior.Total, Count: stat.Count - prior.Count}
	}
	return out
}

// distribution summarizes per-question latencies; the over-5s count is the
// reading the eval report quotes alongside p90 and max.
func distribution(values []time.Duration) latencyReading {
	if len(values) == 0 {
		return latencyReading{}
	}
	sorted := append([]time.Duration(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	var total time.Duration
	over5s := 0
	for _, value := range sorted {
		total += value
		if value > 5*time.Second {
			over5s++
		}
	}
	return latencyReading{
		Mean:   total / time.Duration(len(sorted)),
		P50:    nearestRank(sorted, 0.50),
		P90:    nearestRank(sorted, 0.90),
		P99:    nearestRank(sorted, 0.99),
		Max:    sorted[len(sorted)-1],
		Over5s: over5s,
	}
}

// nearestRank returns the value at the given percentile of an ascending
// series, the convention the eval harness's scoring uses.
func nearestRank(sorted []time.Duration, fraction float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	index := int(math.Ceil(fraction*float64(len(sorted)))) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}

func share(part, whole time.Duration) float64 {
	if whole <= 0 {
		return 0
	}
	return float64(part) / float64(whole)
}

// computeShares expresses the lane's phases as fractions of the lane, of the
// provider's search stage, and of the sum of all request times.
func computeShares(phase vectorPhase, stages map[string]stageReading, contextSum time.Duration) shareReading {
	lane := stages["lane:vector"].Total
	search := stages["search"].Total
	return shareReading{
		ScanOfLane:    share(phase.ScanTotal, lane),
		SortOfLane:    share(phase.SortTotal, lane),
		EmbedOfLane:   share(phase.EmbedTotal, lane),
		LoadOfLane:    share(phase.ShardLoad, lane),
		ScanOfSearch:  share(phase.ScanTotal, search),
		ScanOfContext: share(phase.ScanTotal, contextSum),
	}
}

// projections scale the measured per-entry scan cost to table sizes the run
// did not have: the scan is linear in the table, and the ordering is the work
// of the limit the scan asked for -- the offer of one score per admitted entry
// is part of the per-entry slope -- so it does not grow with the table.
func projections(phase vectorPhase, entriesPerShard int) []projectionPoint {
	points := make([]projectionPoint, 0, 4)
	if phase.EntriesScanned <= 0 || entriesPerShard <= 0 {
		return points
	}
	perEntry := time.Duration(float64(phase.ScanTotal) / float64(phase.EntriesScanned))
	scans := phase.Scans
	if scans <= 0 {
		scans = 1
	}
	perScanSort := float64(phase.SortTotal) / float64(scans)
	for _, size := range []int{entriesPerShard, 10000, 40000, 100000} {
		if size <= 0 {
			continue
		}
		point := projectionPoint{
			EntriesPerShard: size,
			Scan:            perEntry * time.Duration(size),
			Sort:            time.Duration(perScanSort),
		}
		if len(points) > 0 && points[len(points)-1].EntriesPerShard == size {
			continue
		}
		points = append(points, point)
	}
	return points
}
