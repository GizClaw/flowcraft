package main

import (
	"sync"
	"time"
)

type latencyAggregator struct {
	mu     sync.Mutex
	totals map[string]time.Duration
	calls  map[string]int
}

func newLatencyAggregator() *latencyAggregator {
	return &latencyAggregator{
		totals: make(map[string]time.Duration),
		calls:  make(map[string]int),
	}
}

func (aggregator *latencyAggregator) record(stage string, duration time.Duration, ok bool) {
	if !ok {
		return
	}
	aggregator.mu.Lock()
	defer aggregator.mu.Unlock()
	aggregator.totals[stage] += duration
	aggregator.calls[stage]++
}

func (aggregator *latencyAggregator) snapshot() map[string]latencySummary {
	aggregator.mu.Lock()
	defer aggregator.mu.Unlock()
	result := make(map[string]latencySummary, len(aggregator.totals))
	for stage, total := range aggregator.totals {
		result[stage] = latencySummary{
			Calls:   aggregator.calls[stage],
			TotalMs: total.Milliseconds(),
		}
	}
	return result
}
