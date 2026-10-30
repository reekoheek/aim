package lane

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/reekoheek/aim/internal/logger"
)

type Process interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context)
	Exited() bool
}

type resident struct {
	model          string
	process        Process
	leases         int
	stopping       bool
	drained        chan struct{}
	idleTimer      *time.Timer
	idleGeneration uint64
}

func (r *resident) disarmIdleTimer() {
	r.idleGeneration++
	if r.idleTimer != nil {
		r.idleTimer.Stop()
		r.idleTimer = nil
	}
}

func (r *resident) drainedSignal() <-chan struct{} {
	if r.drained == nil {
		r.drained = make(chan struct{})
		if r.leases == 0 {
			close(r.drained)
		}
	}
	return r.drained
}

type Lane struct {
	name        string
	idleTimeout time.Duration
	logger      *logger.Logger
	turn        chan struct{}
	mu          sync.Mutex
	resident    *resident
}

func newLane(name string, idleTimeout time.Duration, l *logger.Logger) *Lane {
	return &Lane{
		name:        name,
		idleTimeout: idleTimeout,
		logger:      l,
		turn:        make(chan struct{}, 1),
	}
}

func (l *Lane) Process() Process {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.resident == nil {
		return nil
	}
	return l.resident.process
}

func (l *Lane) Leases() int {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.resident == nil {
		return 0
	}
	return l.resident.leases
}

func (l *Lane) run(ctx context.Context, lifetime context.Context, model string, build func() (Process, error)) (*resident, error) {
	select {
	case l.turn <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer l.endTurn()

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if r := l.resident; r != nil && r.model == model && !r.process.Exited() && l.lease(r) {
		return r, nil
	}

	if lifetime.Err() != nil {
		return nil, ErrClosed
	}

	process, err := build()
	if err != nil {
		return nil, err
	}

	l.evict(lifetime, model)

	if lifetime.Err() != nil {
		return nil, ErrClosed
	}

	if err := process.Start(lifetime); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrStartFailed, err)
	}

	r := &resident{model: model, process: process, leases: 1}
	l.mu.Lock()
	l.resident = r
	l.mu.Unlock()
	return r, nil
}

func (l *Lane) close(ctx context.Context) {
	l.turn <- struct{}{}
	defer l.endTurn()

	l.evict(ctx, "")
}

func (l *Lane) endTurn() {
	<-l.turn
}

func (l *Lane) lease(r *resident) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	if r.stopping {
		return false
	}
	r.disarmIdleTimer()
	r.leases++
	return true
}

func (l *Lane) release(r *resident) {
	l.mu.Lock()
	defer l.mu.Unlock()

	r.leases--
	if r.leases > 0 {
		return
	}
	if r.drained != nil {
		close(r.drained)
	}
	if r.stopping || l.idleTimeout <= 0 {
		return
	}
	l.armIdleTimer(r)
}

func (l *Lane) armIdleTimer(r *resident) {
	r.disarmIdleTimer()
	generation := r.idleGeneration
	r.idleTimer = time.AfterFunc(l.idleTimeout, func() {
		l.stopIfIdle(r, generation)
	})
}

func (l *Lane) stopIfIdle(r *resident, generation uint64) {
	l.mu.Lock()
	if generation != r.idleGeneration || r.leases > 0 || r.stopping {
		l.mu.Unlock()
		return
	}
	r.stopping = true
	r.idleTimer = nil
	l.mu.Unlock()

	l.logger.Info("stopping idle process",
		slog.String("lane", l.name),
		slog.String("model", r.model),
		logger.Seconds("timeout", l.idleTimeout),
	)

	l.turn <- struct{}{}
	defer l.endTurn()

	if l.resident != r {
		return
	}
	r.process.Stop(context.Background())
	l.vacate()
}

func (l *Lane) evict(ctx context.Context, nextModel string) {
	r := l.resident
	if r == nil {
		return
	}
	if nextModel != "" && nextModel != r.model {
		l.logger.Info("swapping process",
			slog.String("lane", l.name),
			slog.String("from", r.model),
			slog.String("to", nextModel),
		)
	}

	l.mu.Lock()
	r.stopping = true
	r.disarmIdleTimer()
	drained := r.drainedSignal()
	l.mu.Unlock()

	select {
	case <-drained:
	case <-ctx.Done():
	}
	r.process.Stop(ctx)
	l.vacate()
}

func (l *Lane) vacate() {
	l.mu.Lock()
	l.resident = nil
	l.mu.Unlock()
}
