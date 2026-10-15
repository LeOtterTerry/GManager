package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

// ErrJobFinished means Cancel was asked to stop a job that already ended.
var ErrJobFinished = errors.New("job finished")

// DefaultRetention is how long a finished job or an idempotency record stays
// in memory. Running jobs are kept until they finish.
const DefaultRetention = 15 * time.Minute

// JobState is the lifecycle of an accepted write.
type JobState string

const (
	// JobQueued means the work has been accepted and has not started.
	JobQueued JobState = "queued"
	// JobRunning means the work is in progress.
	JobRunning JobState = "running"
	// JobSucceeded means the work finished without error.
	JobSucceeded JobState = "succeeded"
	// JobFailed means the work finished with an error.
	JobFailed JobState = "failed"
)

// Job is one accepted write. Clients poll GET /api/v1/jobs/{id}.
type Job struct {
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	InstanceID string          `json:"instanceId,omitempty"`
	State      JobState        `json:"state"`
	Error      string          `json:"error,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
	CreatedAt  time.Time       `json:"createdAt"`
	FinishedAt *time.Time      `json:"finishedAt,omitempty"`
}

// JobFunc is the body of an async job. The context is cancelled when the
// client cancels the job.
type JobFunc func(ctx context.Context) (any, error)

// JobRunner stores jobs in memory. Persistence is a later slice.
// Finished jobs older than DefaultRetention are dropped.
type JobRunner struct {
	mu      sync.Mutex
	jobs    map[string]*Job
	inline  bool
	ttl     time.Duration
	now     func() time.Time
	cancels map[string]context.CancelFunc
}

// NewJobRunner returns an in-memory runner.
// Inline runs the job before Start returns, which tests use for determinism.
func NewJobRunner(inline bool) *JobRunner {
	return &JobRunner{
		jobs:    map[string]*Job{},
		inline:  inline,
		ttl:     DefaultRetention,
		now:     time.Now,
		cancels: map[string]context.CancelFunc{},
	}
}

// Start accepts work and returns the job immediately when not inline.
func (r *JobRunner) Start(typ, instanceID string, fn JobFunc) *Job {
	ctx, cancel := context.WithCancel(context.Background())
	job := r.new(typ, instanceID)
	r.mu.Lock()
	r.cancels[job.ID] = cancel
	r.mu.Unlock()
	r.launch(job, ctx, fn)
	return r.copy(job.ID)
}

// Cancel asks a running job to stop. A missing job returns an error that
// wraps os.ErrNotExist. A finished job returns ErrJobFinished.
func (r *JobRunner) Cancel(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	job, ok := r.jobs[id]
	if !ok {
		return os.ErrNotExist
	}
	if job.State == JobSucceeded || job.State == JobFailed {
		return ErrJobFinished
	}
	if cancel := r.cancels[id]; cancel != nil {
		cancel()
	}
	return nil
}

// Record stores a job that already finished. Config saves use this so a
// conflict can be refused before a job exists, while an accepted save still
// returns a job id.
func (r *JobRunner) Record(typ, instanceID string, result any, err error) *Job {
	job := r.new(typ, instanceID)
	r.finish(job, result, err)
	return r.copy(job.ID)
}

// Get returns a copy of the job. Expired finished jobs are removed.
func (r *JobRunner) Get(id string) (Job, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sweepLocked()
	job, ok := r.jobs[id]
	if !ok {
		return Job{}, false
	}
	return *job, true
}

func (r *JobRunner) new(typ, instanceID string) *Job {
	job := &Job{
		ID:         newID(),
		Type:       typ,
		InstanceID: instanceID,
		State:      JobQueued,
		CreatedAt:  r.now().UTC(),
	}
	r.mu.Lock()
	r.sweepLocked()
	r.jobs[job.ID] = job
	r.mu.Unlock()
	return job
}

func (r *JobRunner) launch(job *Job, ctx context.Context, fn JobFunc) {
	run := func() {
		r.setState(job, JobRunning)
		defer func() {
			r.mu.Lock()
			delete(r.cancels, job.ID)
			r.mu.Unlock()
		}()
		defer func() {
			if rec := recover(); rec != nil {
				r.finish(job, nil, fmt.Errorf("panic: %v", rec))
			}
		}()
		result, err := fn(ctx)
		r.finish(job, result, err)
	}
	if r.inline {
		run()
		return
	}
	go run()
}

func (r *JobRunner) setState(job *Job, state JobState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if job.State == JobSucceeded || job.State == JobFailed {
		return
	}
	job.State = state
}

func (r *JobRunner) finish(job *Job, result any, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if job.State == JobSucceeded || job.State == JobFailed {
		return
	}
	now := r.now().UTC()
	job.FinishedAt = &now
	if err != nil {
		job.State = JobFailed
		job.Error = err.Error()
		return
	}
	job.State = JobSucceeded
	if result != nil {
		raw, marshalErr := json.Marshal(result)
		if marshalErr != nil {
			job.State = JobFailed
			job.Error = marshalErr.Error()
			return
		}
		job.Result = raw
	}
}

func (r *JobRunner) sweepLocked() {
	if r.ttl <= 0 {
		return
	}
	now := r.now()
	for id, job := range r.jobs {
		if job.FinishedAt != nil && now.Sub(*job.FinishedAt) > r.ttl {
			delete(r.jobs, id)
			delete(r.cancels, id)
		}
	}
}

func (r *JobRunner) copy(id string) *Job {
	job, ok := r.Get(id)
	if !ok {
		return nil
	}
	return &job
}

func newID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("job-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf[:])
}
