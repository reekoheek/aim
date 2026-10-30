package lane

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/reekoheek/aim/internal/logger"
)

var (
	ErrClosed      = errors.New("supervisor is shutting down")
	ErrStartFailed = errors.New("failed to start process")
)

type Supervisor struct {
	idleTimeout   time.Duration
	logger        *logger.Logger
	mu            sync.Mutex
	lanes         map[string]*Lane
	lifetime      context.Context
	closeLifetime context.CancelFunc
}

func NewSupervisor(idleTimeout time.Duration, l *logger.Logger) *Supervisor {
	lifetime, closeLifetime := context.WithCancel(context.Background())
	return &Supervisor{
		idleTimeout:   idleTimeout,
		logger:        l,
		lanes:         make(map[string]*Lane),
		lifetime:      lifetime,
		closeLifetime: closeLifetime,
	}
}

type Lease[T Process] struct {
	Process T
	once    sync.Once
	release func()
}

func (l *Lease[T]) Release() {
	l.once.Do(l.release)
}

func Run[T Process](ctx context.Context, s *Supervisor, laneName string, model string, build func() (T, error)) (*Lease[T], error) {
	lane := s.laneNamed(laneName)
	r, err := lane.run(ctx, s.lifetime, model, func() (Process, error) {
		return build()
	})
	if err != nil {
		return nil, err
	}
	return &Lease[T]{
		Process: r.process.(T),
		release: func() { lane.release(r) },
	}, nil
}

func (s *Supervisor) Lane(name string) *Lane {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.lanes[name]
}

func (s *Supervisor) laneNamed(name string) *Lane {
	s.mu.Lock()
	defer s.mu.Unlock()

	lane, ok := s.lanes[name]
	if !ok {
		lane = newLane(name, s.idleTimeout, s.logger)
		s.lanes[name] = lane
	}
	return lane
}

func (s *Supervisor) Close(ctx context.Context) {
	s.closeLifetime()

	s.mu.Lock()
	lanes := make([]*Lane, 0, len(s.lanes))
	for _, lane := range s.lanes {
		lanes = append(lanes, lane)
	}
	s.mu.Unlock()

	var closing sync.WaitGroup
	for _, lane := range lanes {
		closing.Add(1)
		go func() {
			defer closing.Done()
			lane.close(ctx)
		}()
	}
	closing.Wait()
}
