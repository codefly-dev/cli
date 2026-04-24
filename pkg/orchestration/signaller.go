package orchestration

// A signaller is a channel that can be used to signal something

type Signal struct {
}

type CreateSignalFunc func(playbook *Playbook, action Action) *Signal

type Signaller struct {
	// Buffered so signal() doesn't need a goroutine just to enqueue —
	// this was the source of a documented leak: when no consumer was
	// attached, the spawned goroutine blocked on an unbuffered send and
	// stayed resident forever.
	signals chan Signal
	f       CreateSignalFunc
}

func (s *Signaller) signal(playbook *Playbook, action Action) {
	if s.f != nil {
		sig := s.f(playbook, action)
		if sig != nil {
			// Non-blocking send. If the buffer is full (rare — 64 slots),
			// the signal is dropped rather than goroutine-leaked.
			select {
			case s.signals <- *sig:
			default:
			}
		}
	}
}

func (s *Signaller) WithCreateSignal(signaller CreateSignalFunc) {
	s.f = signaller
}

func NewSignaller() *Signaller {
	return &Signaller{
		signals: make(chan Signal, 64),
	}
}
