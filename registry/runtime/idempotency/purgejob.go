package idempotency

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultPurgeInterval is the interval of the purge job when the service declares none (ST-SUP-RP07-LC09).
const DefaultPurgeInterval = time.Minute

// JobClock is the process clock of the purge job. A lease.Clock satisfies it.
type JobClock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time                         { return time.Now() }
func (systemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// PurgeJobConfig is what the purge job needs. A zero field takes its default.
type PurgeJobConfig struct {
	// Interval is the declared interval of the job; zero is DefaultPurgeInterval.
	Interval time.Duration
	// Batch and Pause set each Purge, as PurgeOptions does.
	Batch int
	Pause time.Duration
	// Clock is the clock of the interval and of the expiry time; nil is the system clock.
	Clock JobClock
}

// PurgeJob runs Purge on the declared interval (ST-SUP-RP07-LC09). The generated service only builds it and
// registers Run as a component.
type PurgeJob struct {
	db       Querier
	interval time.Duration
	opts     PurgeOptions
	clock    JobClock

	running  sync.Mutex
	runs     atomic.Int64
	failures atomic.Int64
	deleted  atomic.Int64
	failed   atomic.Bool
}

// NewPurgeJob returns the purge job on db. It refuses a nil db and a negative interval.
func NewPurgeJob(db Querier, cfg PurgeJobConfig) (*PurgeJob, error) {
	if db == nil {
		return nil, errors.New("idempotency: the purge job needs the database")
	}
	if cfg.Interval < 0 || cfg.Batch < 0 || cfg.Pause < 0 {
		return nil, errors.New("idempotency: the purge job takes no negative interval, batch or pause")
	}
	if cfg.Batch > MaxPurgeBatch {
		return nil, errors.New("idempotency: the purge job takes no batch above MaxPurgeBatch")
	}
	j := &PurgeJob{db: db, interval: cfg.Interval, clock: cfg.Clock}
	if j.interval == 0 {
		j.interval = DefaultPurgeInterval
	}
	if j.clock == nil {
		j.clock = systemClock{}
	}
	j.opts = PurgeOptions{Batch: cfg.Batch, Pause: cfg.Pause, Clock: j.clock.Now, Sleep: j.sleep}
	return j, nil
}

// Run runs one Purge at start and then each time the interval passes, until ctx ends, and then returns ctx.Err().
// Each wait, also the pause between two batches of a purge, ends when ctx ends. It never runs two purges at once,
// also when Run is called twice: a tick that comes while a purge runs waits for it. A purge that fails is counted
// (Failures) and reported by Health, and the next tick runs again; a purge that ctx ended is not counted as a run.
func (j *PurgeJob) Run(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		j.purge(ctx)
		if err := j.sleep(ctx, j.interval); err != nil {
			return err
		}
	}
}

// sleep waits d on the job's clock, or until ctx ends.
func (j *PurgeJob) sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-j.clock.After(d):
		return nil
	}
}

func (j *PurgeJob) purge(ctx context.Context) {
	j.running.Lock()
	defer j.running.Unlock()
	n, err := Purge(ctx, j.db, j.opts)
	j.deleted.Add(n)
	if ctx.Err() != nil {
		return
	}
	j.runs.Add(1)
	if err != nil {
		j.failures.Add(1)
		j.failed.Store(true)
		return
	}
	j.failed.Store(false)
}

// Runs counts the purges that ran, Failures those that failed, and Deleted the rows they removed.
func (j *PurgeJob) Runs() int64     { return j.runs.Load() }
func (j *PurgeJob) Failures() int64 { return j.failures.Load() }
func (j *PurgeJob) Deleted() int64  { return j.deleted.Load() }

// Health is false while the last purge failed.
func (j *PurgeJob) Health() (bool, string) {
	if j.failed.Load() {
		return false, "the last purge of the idempotency keys failed"
	}
	return true, ""
}
