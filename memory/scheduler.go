package memory

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/memory/views/recent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// LifecycleJobStore is the durable queue boundary for serializable memory jobs.
// Implementations own persistence, leases, and terminal status transitions.
type LifecycleJobStore interface {
	Enqueue(context.Context, LifecycleJob) (LifecycleJobID, error)
	Claim(context.Context, string, time.Duration) (LifecycleJob, bool, error)
	Heartbeat(context.Context, LifecycleJobID, string, time.Duration) error
	Complete(context.Context, LifecycleJobID, string, LifecycleJobResult) error
	Fail(context.Context, LifecycleJobID, string, error, map[string]any) error
	Cancel(context.Context, LifecycleJobID, string) error
	Shutdown(context.Context) error
	Stats(context.Context) (QueueStats, error)
}

// LifecycleJobKind names the serializable work payload carried by the job store.
type LifecycleJobKind string

const (
	LifecycleJobKindWriteChain     LifecycleJobKind = "write_chain"
	LifecycleJobKindRebuild        LifecycleJobKind = LifecycleJobKind(LifecycleActionRebuild)
	LifecycleJobKindReconcile      LifecycleJobKind = LifecycleJobKind(LifecycleActionReconcile)
	LifecycleJobKindReload         LifecycleJobKind = LifecycleJobKind(LifecycleActionReload)
	LifecycleJobKindFreshnessCheck LifecycleJobKind = LifecycleJobKind(LifecycleActionFreshnessCheck)
)

// LifecycleJobStatus is the durable status of a queued memory job.
type LifecycleJobStatus string

const (
	LifecycleJobStatusPending   LifecycleJobStatus = "pending"
	LifecycleJobStatusRunning   LifecycleJobStatus = "running"
	LifecycleJobStatusCompleted LifecycleJobStatus = "completed"
	LifecycleJobStatusFailed    LifecycleJobStatus = "failed"
	LifecycleJobStatusCancelled LifecycleJobStatus = "cancelled"
)

// LifecycleJob is the serializable job payload used by the memory control plane.
type LifecycleJob struct {
	ID             LifecycleJobID
	TraceID        TraceID
	OperationID    OperationID
	Kind           LifecycleJobKind
	Scope          Scope
	Capabilities   []Capability
	Documents      []DocumentTarget
	Reason         string
	Window         recent.WindowRequest
	Stages         []PlannedStage
	Status         LifecycleJobStatus
	Attempt        int
	MaxAttempts    int
	LeaseOwner     string
	LeaseExpiresAt time.Time
	Checkpoint     map[string]any
	CreatedAt      time.Time
	UpdatedAt      time.Time
	Error          string
}

// LifecycleJobResult reports the result of one worker execution attempt.
type LifecycleJobResult struct {
	JobID       LifecycleJobID
	OperationID OperationID
	Kind        LifecycleJobKind
	Completed   bool
	Error       string
	Checkpoint  map[string]any
}

// QueueStats summarizes durable job state.
type QueueStats struct {
	Pending         int
	Running         int
	Completed       int
	Failed          int
	Cancelled       int
	Attempts        int
	QueuedByKind    map[LifecycleJobKind]int
	AttemptsByKind  map[LifecycleJobKind]int
	CompletedByKind map[LifecycleJobKind]int
	FailedByKind    map[LifecycleJobKind]int
	CancelledByKind map[LifecycleJobKind]int
}

// MemoryJobStore is an in-memory reference implementation of LifecycleJobStore.
// It is intended for tests and local runs; production deployments should provide
// a persistent implementation with the same lease semantics.
type MemoryJobStore struct {
	mu       sync.Mutex
	nextID   uint64
	order    []LifecycleJobID
	jobs     map[LifecycleJobID]LifecycleJob
	shutdown bool
}

// NewMemoryJobStore returns a local durable-job reference store.
func NewMemoryJobStore() *MemoryJobStore {
	return &MemoryJobStore{
		jobs: make(map[LifecycleJobID]LifecycleJob),
	}
}

// Enqueue persists a serializable job in pending state.
func (s *MemoryJobStore) Enqueue(_ context.Context, job LifecycleJob) (LifecycleJobID, error) {
	if s == nil {
		return "", errdefs.NotAvailablef("memory: job store is not configured")
	}
	if job.Kind == "" {
		return "", errdefs.Validationf("memory: job kind is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.shutdown {
		return "", errdefs.NotAvailablef("memory: job store is shut down")
	}
	if s.jobs == nil {
		s.jobs = make(map[LifecycleJobID]LifecycleJob)
	}
	if job.ID == "" {
		s.nextID++
		job.ID = LifecycleJobID("memory-job-" + strconv.FormatUint(s.nextID, 10))
	} else if _, exists := s.jobs[job.ID]; exists {
		return "", errdefs.Validationf("memory: job %q already exists", job.ID)
	}
	if job.Status == "" {
		job.Status = LifecycleJobStatusPending
	}
	if job.Status != LifecycleJobStatusPending {
		return "", errdefs.Validationf("memory: job %q must be enqueued as pending", job.ID)
	}
	if job.MaxAttempts == 0 {
		job.MaxAttempts = 1
	}
	now := time.Now().UTC()
	if job.CreatedAt.IsZero() {
		job.CreatedAt = now
	}
	job.UpdatedAt = now
	job = cloneLifecycleJob(job)
	s.jobs[job.ID] = job
	s.order = append(s.order, job.ID)
	return job.ID, nil
}

// Get returns a snapshot of one queued job when available.
func (s *MemoryJobStore) Get(_ context.Context, jobID LifecycleJobID) (LifecycleJob, bool, error) {
	if s == nil {
		return LifecycleJob{}, false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[jobID]
	if !ok {
		return LifecycleJob{}, false, nil
	}
	return cloneLifecycleJob(job), true, nil
}

// Claim leases the next pending or expired running job for one worker.
func (s *MemoryJobStore) Claim(_ context.Context, workerID string, lease time.Duration) (LifecycleJob, bool, error) {
	if s == nil {
		return LifecycleJob{}, false, errdefs.NotAvailablef("memory: job store is not configured")
	}
	if workerID == "" {
		return LifecycleJob{}, false, errdefs.Validationf("memory: worker id is required")
	}
	if lease <= 0 {
		return LifecycleJob{}, false, errdefs.Validationf("memory: lease must be positive")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	for _, id := range s.order {
		job := s.jobs[id]
		claimable := job.Status == LifecycleJobStatusPending ||
			(job.Status == LifecycleJobStatusRunning && !job.LeaseExpiresAt.IsZero() && !job.LeaseExpiresAt.After(now))
		if !claimable {
			continue
		}
		job.Status = LifecycleJobStatusRunning
		job.Attempt++
		job.LeaseOwner = workerID
		job.LeaseExpiresAt = now.Add(lease)
		job.UpdatedAt = now
		job.Error = ""
		s.jobs[id] = job
		return cloneLifecycleJob(job), true, nil
	}
	return LifecycleJob{}, false, nil
}

// Heartbeat extends a running job lease for the current owner.
func (s *MemoryJobStore) Heartbeat(_ context.Context, jobID LifecycleJobID, workerID string, lease time.Duration) error {
	if s == nil {
		return errdefs.NotAvailablef("memory: job store is not configured")
	}
	if lease <= 0 {
		return errdefs.Validationf("memory: lease must be positive")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	job, err := s.requireOwnedRunning(jobID, workerID)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	job.LeaseExpiresAt = now.Add(lease)
	job.UpdatedAt = now
	s.jobs[jobID] = job
	return nil
}

// Complete marks a leased job as completed.
func (s *MemoryJobStore) Complete(_ context.Context, jobID LifecycleJobID, workerID string, result LifecycleJobResult) error {
	if s == nil {
		return errdefs.NotAvailablef("memory: job store is not configured")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	job, err := s.requireOwnedRunning(jobID, workerID)
	if err != nil {
		return err
	}
	job.Status = LifecycleJobStatusCompleted
	job.LeaseOwner = ""
	job.LeaseExpiresAt = time.Time{}
	job.Error = ""
	if result.Checkpoint != nil {
		job.Checkpoint = cloneCheckpoint(result.Checkpoint)
	}
	job.UpdatedAt = time.Now().UTC()
	s.jobs[jobID] = job
	return nil
}

// Fail records an attempt failure and either requeues or terminally fails the job.
func (s *MemoryJobStore) Fail(_ context.Context, jobID LifecycleJobID, workerID string, jobErr error, checkpoint map[string]any) error {
	if s == nil {
		return errdefs.NotAvailablef("memory: job store is not configured")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	job, err := s.requireOwnedRunning(jobID, workerID)
	if err != nil {
		return err
	}
	if job.MaxAttempts == 0 {
		job.MaxAttempts = 1
	}
	if job.Attempt < job.MaxAttempts {
		job.Status = LifecycleJobStatusPending
	} else {
		job.Status = LifecycleJobStatusFailed
	}
	job.LeaseOwner = ""
	job.LeaseExpiresAt = time.Time{}
	if jobErr != nil {
		job.Error = jobErr.Error()
	}
	if checkpoint != nil {
		job.Checkpoint = cloneCheckpoint(checkpoint)
	}
	job.UpdatedAt = time.Now().UTC()
	s.jobs[jobID] = job
	return nil
}

// Cancel marks a non-terminal job as cancelled.
func (s *MemoryJobStore) Cancel(_ context.Context, jobID LifecycleJobID, reason string) error {
	if s == nil {
		return errdefs.NotAvailablef("memory: job store is not configured")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	job, ok := s.jobs[jobID]
	if !ok {
		return errdefs.NotFoundf("memory: job %q not found", jobID)
	}
	if isTerminalLifecycleJobStatus(job.Status) {
		return nil
	}
	job.Status = LifecycleJobStatusCancelled
	job.LeaseOwner = ""
	job.LeaseExpiresAt = time.Time{}
	job.Error = reason
	job.UpdatedAt = time.Now().UTC()
	s.jobs[jobID] = job
	return nil
}

// Shutdown prevents future enqueueing. Existing jobs remain visible in Stats.
func (s *MemoryJobStore) Shutdown(_ context.Context) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.shutdown = true
	return nil
}

// Stats returns a snapshot of queue counters.
func (s *MemoryJobStore) Stats(_ context.Context) (QueueStats, error) {
	if s == nil {
		return QueueStats{}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	stats := QueueStats{
		QueuedByKind:    make(map[LifecycleJobKind]int),
		AttemptsByKind:  make(map[LifecycleJobKind]int),
		CompletedByKind: make(map[LifecycleJobKind]int),
		FailedByKind:    make(map[LifecycleJobKind]int),
		CancelledByKind: make(map[LifecycleJobKind]int),
	}
	for _, id := range s.order {
		job := s.jobs[id]
		stats.QueuedByKind[job.Kind]++
		stats.Attempts += job.Attempt
		stats.AttemptsByKind[job.Kind] += job.Attempt
		switch job.Status {
		case LifecycleJobStatusPending:
			stats.Pending++
		case LifecycleJobStatusRunning:
			stats.Running++
		case LifecycleJobStatusCompleted:
			stats.Completed++
			stats.CompletedByKind[job.Kind]++
		case LifecycleJobStatusFailed:
			stats.Failed++
			stats.FailedByKind[job.Kind]++
		case LifecycleJobStatusCancelled:
			stats.Cancelled++
			stats.CancelledByKind[job.Kind]++
		}
	}
	if len(stats.QueuedByKind) == 0 {
		stats.QueuedByKind = nil
	}
	if len(stats.AttemptsByKind) == 0 {
		stats.AttemptsByKind = nil
	}
	if len(stats.CompletedByKind) == 0 {
		stats.CompletedByKind = nil
	}
	if len(stats.FailedByKind) == 0 {
		stats.FailedByKind = nil
	}
	if len(stats.CancelledByKind) == 0 {
		stats.CancelledByKind = nil
	}
	return stats, nil
}

func (s *MemoryJobStore) requireOwnedRunning(jobID LifecycleJobID, workerID string) (LifecycleJob, error) {
	job, ok := s.jobs[jobID]
	if !ok {
		return LifecycleJob{}, errdefs.NotFoundf("memory: job %q not found", jobID)
	}
	if job.Status != LifecycleJobStatusRunning {
		return LifecycleJob{}, errdefs.Validationf("memory: job %q is not running", jobID)
	}
	if job.LeaseOwner != workerID {
		return LifecycleJob{}, errdefs.Validationf("memory: job %q is leased by another worker", jobID)
	}
	return job, nil
}

func isTerminalLifecycleJobStatus(status LifecycleJobStatus) bool {
	switch status {
	case LifecycleJobStatusCompleted, LifecycleJobStatusFailed, LifecycleJobStatusCancelled:
		return true
	default:
		return false
	}
}

func cloneLifecycleJob(job LifecycleJob) LifecycleJob {
	job.Capabilities = cloneCapabilities(job.Capabilities)
	job.Documents = cloneDocumentTargets(job.Documents)
	job.Stages = clonePlannedStages(job.Stages)
	job.Checkpoint = cloneCheckpoint(job.Checkpoint)
	return job
}

func cloneCheckpoint(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
