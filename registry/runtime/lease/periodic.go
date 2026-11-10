package lease

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// PeriodicName is the name of the periodic job component, which the generated code registers for the controller
// mode: modes.Component{Name: lease.PeriodicName, Run: periodic.Run}.
const PeriodicName = "periodic-jobs"

// Job is one periodic job of the controller mode (ST-SUP-RP09-LC13), such as the audit anchor job at the declared
// anchor_period, 1h by default, or the audit maintenance at its own declared interval.
type Job struct {
	// Name names the job in its counts and its health.
	Name string
	// Interval is the job's declared interval: the time from the start of one run to the start of the next.
	Interval time.Duration
	// Run runs the job once under held, the lease that the controller holds. Its context ends when the lease
	// stops. An error, or a panic, is a failed run.
	Run func(ctx context.Context, held *Held) error
}

// JobStats are the counts of one job.
type JobStats struct {
	// Runs counts the runs under the lease, and Failures the runs among them that failed.
	Runs, Failures int64
	// Skipped counts the times the job was due while the controller held no lease.
	Skipped int64
	// LastFailed is true while the last run failed.
	LastFailed bool
}

// PeriodicConfig is what the periodic job component needs.
type PeriodicConfig struct {
	// Lease returns the controller lease that this process holds now, or nil when it holds none.
	Lease func() *Held
	// Jobs are the jobs to run, each at its own interval.
	Jobs []Job
	// Clock is the process clock of the intervals; nil is the system clock.
	Clock Clock
	// Retry is the wait before the next try after a due run found no lease held: the lease's retry period, zero
	// for DefaultRetry, so a job runs soon after the controller acquires the lease.
	Retry time.Duration
}

// Periodic is the periodic job component of the controller mode.
type Periodic struct {
	lease func() *Held
	jobs  []Job
	clock Clock
	retry time.Duration

	mu    sync.Mutex
	stats map[string]*JobStats
}

// NewPeriodic checks cfg and returns the component. It refuses a configuration with no lease function or no job, a
// job with no name, no run function or an interval that is not positive, and two jobs of one name.
func NewPeriodic(cfg PeriodicConfig) (*Periodic, error) {
	if cfg.Lease == nil || len(cfg.Jobs) == 0 {
		return nil, errors.New("lease: the periodic jobs need the lease and at least one job")
	}
	p := &Periodic{lease: cfg.Lease, jobs: append([]Job(nil), cfg.Jobs...), clock: cfg.Clock, stats: map[string]*JobStats{}}
	if p.clock == nil {
		p.clock = systemClock{}
	}
	p.retry = cfg.Retry
	if p.retry == 0 {
		p.retry = DefaultRetry
	}
	if p.retry < 0 {
		return nil, errors.New("lease: the retry period of the periodic jobs is negative")
	}
	for _, j := range p.jobs {
		switch {
		case j.Name == "" || j.Run == nil:
			return nil, errors.New("lease: a periodic job needs a name and a run function")
		case j.Interval <= 0:
			return nil, fmt.Errorf("lease: the periodic job %s has an interval that is not positive", j.Name)
		case p.stats[j.Name] != nil:
			return nil, fmt.Errorf("lease: two periodic jobs are named %s", j.Name)
		}
		p.stats[j.Name] = &JobStats{}
	}
	return p, nil
}

// Run runs each job until ctx ends (ST-SUP-RP09-LC13): the first run at start, and then one run each time the job's
// interval has passed since the start of its last run. A run starts only while Lease gives a lease that has not
// stopped; its context ends when that lease stops, or when the lease's expiry passes on the lease's clock, read
// again after each renew, so no run outlives the lease and a renewed lease keeps its run. A job that is due while no lease is held is skipped, counted, and tried again after the retry period, so
// it runs soon after the lease is acquired. A run that fails, or panics, is counted, makes Health report the job,
// and does not stop later runs. It is the run function of the controller mode's component PeriodicName, and it
// returns only when ctx ends.
func (p *Periodic) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	for _, j := range p.jobs {
		wg.Add(1)
		go func(j Job) {
			defer wg.Done()
			for {
				start := p.clock.Now()
				wait := p.retry
				if p.runOnce(ctx, j) {
					wait = max(j.Interval-p.clock.Now().Sub(start), 0)
				}
				select {
				case <-ctx.Done():
					return
				case <-p.clock.After(wait):
				}
			}
		}(j)
	}
	wg.Wait()
	return ctx.Err()
}

// runOnce runs j once when the lease is held, counts the outcome, and reports whether it ran.
func (p *Periodic) runOnce(ctx context.Context, j Job) bool {
	if ctx.Err() != nil {
		return false
	}
	held := p.lease()
	if held == nil || held.Err() != nil {
		p.count(j.Name, func(s *JobStats) { s.Skipped++ })
		return false
	}
	runCtx, cancel := context.WithCancel(ctx)
	stop := make(chan struct{})
	go held.watch(stop, cancel)
	err := runJob(runCtx, j, held)
	close(stop)
	cancel()
	p.count(j.Name, func(s *JobStats) {
		s.Runs++
		s.LastFailed = err != nil
		if err != nil {
			s.Failures++
		}
	})
	return true
}

// watch calls cancel when h stops or when its expiry passes on the lease's process clock, until stop closes. It
// reads the expiry again each time it wakes, so a renew that moved the expiry keeps the run going.
func (h *Held) watch(stop <-chan struct{}, cancel context.CancelFunc) {
	clock := h.client.clock
	for {
		left := h.Expires().Sub(clock.Now())
		if left <= 0 {
			cancel()
			return
		}
		select {
		case <-h.Done():
			cancel()
			return
		case <-stop:
			return
		case <-clock.After(left):
		}
	}
}

// runJob calls the job's run function and turns a panic into an error that holds no part of the panic value.
func runJob(ctx context.Context, j Job, held *Held) (err error) {
	defer func() {
		if recover() != nil {
			err = fmt.Errorf("lease: the periodic job %s panicked", j.Name)
		}
	}()
	return j.Run(ctx, held)
}

func (p *Periodic) count(name string, change func(*JobStats)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	change(p.stats[name])
}

// Stats returns the counts of the job name, and false for a name of no job.
func (p *Periodic) Stats(name string) (JobStats, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.stats[name]
	if !ok {
		return JobStats{}, false
	}
	return *s, true
}

// Health is the health signal of the jobs: false, with the names of the jobs, while the last run of any job failed.
func (p *Periodic) Health() (bool, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var failed []string
	for name, s := range p.stats {
		if s.LastFailed {
			failed = append(failed, name)
		}
	}
	if len(failed) == 0 {
		return true, ""
	}
	sort.Strings(failed)
	return false, "the last run of the periodic jobs " + strings.Join(failed, ", ") + " failed"
}
