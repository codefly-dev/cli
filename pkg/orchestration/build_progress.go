package orchestration

import (
	"fmt"
	"io"
	"sync"
	"time"
)

// BuildProgress is what the operator sees of a build phase while it runs.
//
// A render's build phase is where its wall clock goes, and since the builds
// run several at a time (build_concurrency.go) each one's own progress stream
// is captured rather than shown (buildTranscript): a 25-minute phase building
// ten images printed nothing to the terminal at all, and the only sign that
// anything was happening was the engine's CPU. This is the one line per event
// that replaces that silence — a build starting, a build finishing, a push
// starting and finishing — and, once per phase, how many images were kept
// because no input of them changed.
//
// It is written to the operator's stdout, which is the render command's to
// pass (Flow.WithBuildProgress); a flow with none reports nothing, so `run`
// and every other command that builds print exactly what they printed before.
// The log lines each build already writes stay as they are: this is a
// summary for a person watching, not a replacement for the log.
//
// The index a line carries is assigned when the build is actually started,
// not when its action was dispatched, because a dispatched action can still
// find its image up to date and build nothing; the total is the number of
// images the phase has to account for, so the indices reached and the kept
// count add up to it.
type BuildProgress struct {
	out io.Writer

	mu      sync.Mutex
	total   int
	started int
	kept    int
	pushes  int
}

// NewBuildProgress reports a build phase's progress to out. A nil out reports
// nothing.
func NewBuildProgress(out io.Writer) *BuildProgress {
	if out == nil {
		return nil
	}
	return &BuildProgress{out: out}
}

// beginPhase opens a phase of total images and returns what closes it, which
// reports the images the phase kept. A nil progress is the no-op one.
func (p *BuildProgress) beginPhase(total int) func() {
	if p == nil {
		return func() {}
	}
	p.mu.Lock()
	p.total, p.started, p.kept, p.pushes = total, 0, 0, 0
	p.mu.Unlock()
	return p.endPhase
}

func (p *BuildProgress) endPhase() {
	p.mu.Lock()
	kept := p.kept
	p.kept = 0
	p.mu.Unlock()
	switch kept {
	case 0:
	case 1:
		p.printf("1 image unchanged, kept")
	default:
		p.printf("%d images unchanged, kept", kept)
	}
}

// keptImage records that service's image was found up to date and not built.
func (p *BuildProgress) keptImage() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.kept++
	p.mu.Unlock()
}

// building reports that service's recipe starts building now and returns the
// index the matching built line carries.
func (p *BuildProgress) building(service, recipe string) int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	p.started++
	index, total := p.started, p.total
	p.mu.Unlock()
	p.printf("building %d/%d %s recipe=%s", index, total, service, recipe)
	return index
}

// built reports that the build started under index finished, how long it
// took, and the digest it was pushed at when it was pushed.
func (p *BuildProgress) built(index int, service string, took time.Duration, digest string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	total := p.total
	p.mu.Unlock()
	line := fmt.Sprintf("built %d/%d %s %s", index, total, service, formatBuildDuration(took))
	if digest != "" {
		line += " digest=" + digest
	}
	p.printf("%s", line)
}

// pushing reports that service's image starts its push now and returns the
// index the matching pushed line carries.
func (p *BuildProgress) pushing(service, image string) int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	p.pushes++
	index, total := p.pushes, p.total
	p.mu.Unlock()
	p.printf("pushing %d/%d %s %s", index, total, service, image)
	return index
}

// pushed reports that the push started under index finished.
func (p *BuildProgress) pushed(index int, service, image string, took time.Duration) {
	if p == nil {
		return
	}
	p.mu.Lock()
	total := p.total
	p.mu.Unlock()
	p.printf("pushed %d/%d %s %s %s", index, total, service, image, formatBuildDuration(took))
}

// printf writes one line, under the lock, so lines from builds finishing at
// the same time never interleave.
func (p *BuildProgress) printf(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fmt.Fprintf(p.out, format+"\n", args...)
}

// formatBuildDuration renders a build's wall clock to the second: what a
// person compares builds by, without the noise of a nanosecond figure.
func formatBuildDuration(took time.Duration) string {
	return took.Round(time.Second).String()
}

// buildProgress is this world's progress reporter, nil when nobody is
// watching; every method of a nil *BuildProgress is a no-op.
func (world *World) buildProgress() *BuildProgress {
	if world == nil {
		return nil
	}
	return world.BuildProgress
}
