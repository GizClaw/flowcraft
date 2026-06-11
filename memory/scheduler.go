package memory

import (
	"context"
	"strconv"
	"sync"

	"github.com/GizClaw/flowcraft/memory/views/recent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// Scheduler is the async write-stage queue used by the root memory facade.
type Scheduler interface {
	Enqueue(context.Context, Job) (JobHandle, error)
	RunOnce(context.Context) (JobResult, error)
	Drain(context.Context) error
	Shutdown(context.Context) error
	Stats(context.Context) (QueueStats, error)
}

// Job describes one queued memory control-plane unit. Runnable closures are
// intentionally kept unexported so public metadata stays serializable.
type Job struct {
	ID     string
	Kind   string
	Scope  Scope
	Window recent.WindowRequest
	Stages []PlannedStage

	run func(context.Context) error
}

// JobHandle is returned after enqueueing async memory work.
type JobHandle struct {
	ID string
}

// JobResult reports the result of one scheduler execution attempt.
type JobResult struct {
	JobID     string
	Completed bool
	Error     string
}

// QueueStats summarizes in-memory scheduler state.
type QueueStats struct {
	Pending   int
	Running   int
	Completed int
	Failed    int
}

type jobEnvelope struct {
	job Job
	run func(context.Context) error
}

// MemoryScheduler is a small FIFO scheduler for local execution and tests.
type MemoryScheduler struct {
	mu        sync.Mutex
	nextID    uint64
	pending   []jobEnvelope
	running   int
	completed int
	failed    int
	shutdown  bool
}

// NewMemoryScheduler returns a local FIFO scheduler for async memory jobs.
func NewMemoryScheduler() *MemoryScheduler {
	return &MemoryScheduler{}
}

// Enqueue appends a runnable job to the local FIFO queue.
func (s *MemoryScheduler) Enqueue(_ context.Context, job Job) (JobHandle, error) {
	if s == nil {
		return JobHandle{}, errdefs.NotAvailablef("memory: scheduler is not configured")
	}
	if job.run == nil {
		return JobHandle{}, errdefs.Validationf("memory: job %q has no runner", job.ID)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.shutdown {
		return JobHandle{}, errdefs.NotAvailablef("memory: scheduler is shut down")
	}
	if job.ID == "" {
		s.nextID++
		job.ID = "memory-job-" + strconv.FormatUint(s.nextID, 10)
	}
	envelope := jobEnvelope{
		job: cloneJobMetadata(job),
		run: job.run,
	}
	s.pending = append(s.pending, envelope)
	return JobHandle{ID: job.ID}, nil
}

// RunOnce runs the next pending job, if any.
func (s *MemoryScheduler) RunOnce(ctx context.Context) (JobResult, error) {
	if s == nil {
		err := errdefs.NotAvailablef("memory: scheduler is not configured")
		return JobResult{Error: err.Error()}, err
	}

	envelope, ok := s.pop()
	if !ok {
		return JobResult{}, nil
	}

	err := envelope.run(ctx)
	s.finish(err)
	if err != nil {
		return JobResult{JobID: envelope.job.ID, Error: err.Error()}, err
	}
	return JobResult{JobID: envelope.job.ID, Completed: true}, nil
}

// Drain runs pending jobs until the queue is empty or a job fails.
func (s *MemoryScheduler) Drain(ctx context.Context) error {
	if s == nil {
		return errdefs.NotAvailablef("memory: scheduler is not configured")
	}
	for {
		result, err := s.RunOnce(ctx)
		if err != nil {
			return err
		}
		if result.JobID == "" {
			return nil
		}
	}
}

// Shutdown prevents future enqueueing. Pending jobs remain visible in Stats.
func (s *MemoryScheduler) Shutdown(_ context.Context) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.shutdown = true
	return nil
}

// Stats returns a snapshot of queue counters.
func (s *MemoryScheduler) Stats(_ context.Context) (QueueStats, error) {
	if s == nil {
		return QueueStats{}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return QueueStats{
		Pending:   len(s.pending),
		Running:   s.running,
		Completed: s.completed,
		Failed:    s.failed,
	}, nil
}

func (s *MemoryScheduler) pop() (jobEnvelope, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) == 0 {
		return jobEnvelope{}, false
	}
	envelope := s.pending[0]
	copy(s.pending, s.pending[1:])
	s.pending[len(s.pending)-1] = jobEnvelope{}
	s.pending = s.pending[:len(s.pending)-1]
	s.running++
	return envelope, true
}

func (s *MemoryScheduler) finish(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running > 0 {
		s.running--
	}
	if err != nil {
		s.failed++
		return
	}
	s.completed++
}

func cloneJobMetadata(job Job) Job {
	return Job{
		ID:     job.ID,
		Kind:   job.Kind,
		Scope:  job.Scope,
		Window: job.Window,
		Stages: clonePlannedStages(job.Stages),
	}
}
