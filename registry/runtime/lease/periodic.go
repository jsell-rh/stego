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
	// Interval is the job's declared interval: the time from the end of one run to the start of the next.
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
}

// Periodic is the periodic job component of the controller mode.
type Periodic struct {
	lease func() *Held
	jobs  []Job
	clock Clock

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
// interval has passed since the end of its last run. A run starts only while Lease gives a lease that has not
// stopped, and its context ends when that lease stops; a job that is due while no lease is held is skipped, not run
// late. A run that fails, or panics, is counted, makes Health report the job, and does not stop later runs. It is
// the run function of the controller mode's component PeriodicName, and it returns only when ctx ends.
func (p *Periodic) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	for _, j := range p.jobs {
		wg.Add(1)
		go func(j Job) {
			defer wg.Done()
			for {
				p.runOnce(ctx, j)
				select {
				case <-ctx.Done():
					return
				case <-p.clock.After(j.Interval):
				}
			}
		}(j)
	}
	wg.Wait()
	return ctx.Err()
}

// runOnce runs j once when the lease is held, and counts the outcome.
func (p *Periodic) runOnce(ctx context.Context, j Job) {
	if ctx.Err() != nil {
		return
	}
	held := p.lease()
	if held == nil || held.Err() != nil {
		p.count(j.Name, func(s *JobStats) { s.Skipped++ })
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	stop := make(chan struct{})
	go func() {
		select {
		case <-held.Done():
			cancel()
		case <-stop:
		}
	}()
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
