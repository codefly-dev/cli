package mutationauthority

import "fmt"

type PreparedPermit struct {
	prepared bool
}

func NewPreparedPermit() PreparedPermit {
	return PreparedPermit{prepared: true}
}

func (permit PreparedPermit) Validate() error {
	if !permit.prepared {
		return fmt.Errorf("mutation requires prepared authority")
	}
	return nil
}
