package monitoring

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	runtimev1 "github.com/codefly-dev/core/proto/v1/go/services/runtime"

	"github.com/codefly-dev/core/shared"
	"github.com/shirou/gopsutil/v3/process"
)

func (ps ProcessState) String() string {
	return [...]string{
		"Unknown",
		"NotFound",
		"Running",
		"Interruptible Sleep",
		"Uninterruptible Sleep",
		"Stopped",
		"Zombie",
		"Dead",
		"Tracing Stop",
		"Idle",
		"Parked",
		"Waking",
	}[ps]
}

type TrackedProcess struct {
	PID    int
	Killed bool
	name   string
	unique string
}

var _ Tracked = (*TrackedProcess)(nil)

func (p *TrackedProcess) Unique() string {
	return p.unique
}

func (p *TrackedProcess) Name() string {
	return p.name
}

func (p *TrackedProcess) Kill() error {
	// TODO implement me
	panic("implement me")
}

func (p *TrackedProcess) GetStatus() (ProcessState, error) {
	logger := shared.NewLogger("TrackedProcess.State<%d>", p.PID)
	// Check for PID
	proc, err := os.FindProcess(p.PID)
	if err != nil {
		return NotFound, nil
	}
	// Sending signal 0 to a pid will return an error if the pid is not running
	// and do nothing if it is.
	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		state, err := findState(p.PID)
		if err != nil {
			return Unknown, logger.Wrapf(err, "cannot check if proc is defunct")
		}
		return state, nil
	}
	return Dead, nil
}

func (p *TrackedProcess) GetUsage() (*Usage, error) {
	logger := shared.NewLogger("TrackedProcess.Usage<%d>", p.PID)
	proc, err := process.NewProcess(int32(p.PID))
	if err != nil {
		return nil, logger.Wrapf(err, "cannot create process")
	}

	// Get CPU percent
	cpuPercent, err := proc.CPUPercent()
	if err != nil {
		return nil, logger.Wrapf(err, "cannot get cpu percent")
	}

	// Get memory info
	memInfo, err := proc.MemoryInfo()
	if err != nil {
		return nil, logger.Wrapf(err, "cannot get memory info")
	}
	return &Usage{
		CPU:    cpuPercent,
		Memory: float64(memInfo.RSS) / 1024.0,
	}, nil
}

func (p *TrackedProcess) Proto() *runtimev1.Tracker {
	return &runtimev1.Tracker{
		Name: p.Name(),
		Tracker: &runtimev1.Tracker_ProcessTracker{
			ProcessTracker: &runtimev1.ProcessTracker{
				PID: int32(p.PID),
			},
		},
	}
}

func parseState(out string) (string, bool) {
	state := strings.TrimSpace(out)
	// Can we the S+ version
	if regular, ok := strings.CutSuffix(state, "+"); ok {
		return regular, false
	}
	return state, true
}

func findState(pid int) (ProcessState, error) {
	logger := shared.NewLogger("TrackedProcess.State<%d>", pid)
	cmd := exec.Command("ps", "-p", fmt.Sprintf("%d", pid), "-o", "state=")
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	if err != nil {
		return Unknown, err
	}
	state, tts := parseState(out.String())
	if tts {
		logger.Debugf("process %d is in TTS", pid)
	}
	switch state {
	case "R":
		return Running, nil
	case "S":
		return InterruptibleSleep, nil
	case "D":
		return UninterruptibleSleep, nil
	case "T":
		return Stopped, nil
	case "Z":
		return Zombie, nil
	case "X":
		return Dead, nil
	case "t":
		return TracingStop, nil
	case "I":
		return Idle, nil
	case "P":
		return Parked, nil
	case "W":
		return Waking, nil
	default:
		return Unknown, fmt.Errorf("unknown state: %s", out.String())
	}
}
