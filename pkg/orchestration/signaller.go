package orchestration

// A signaller is a channel that can be used to signal something

type Signal struct {
}

type CreateSignalFunc func(playbook *Playbook, action Action) *Signal

type Signaller struct {
	signals chan Signal
	f       CreateSignalFunc
}

func (s *Signaller) signal(playbook *Playbook, action Action) {
	if s.f != nil {
		sig := s.f(playbook, action)
		if sig != nil {
			go func() {
				s.signals <- *sig
			}()
		}
	}
}

func (s *Signaller) WithCreateSignal(signaller CreateSignalFunc) {
	s.f = signaller
}

func NewSignaller() *Signaller {
	return &Signaller{
		signals: make(chan Signal),
	}
}
