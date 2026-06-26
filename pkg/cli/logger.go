package cli

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codefly-dev/core/agents"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/tui"
	"github.com/codefly-dev/core/wool"
)

// Log-line markers form a small fixed taxonomy so a streamed run stays
// scannable: a single glance at the leading marker tells you who is speaking.
//
//	>>  an aggregate lifecycle milestone for a named service, e.g.
//	    ">> mind/mind: Running". Emitted both here (headless) and by the
//	    core/tui aggregate layer (interactive), so the two modes share one form.
//	>   codefly narrating about one service (forwarded wool logs).
//	|   the service's own output (its stdout / wool logs).
const (
	MarkerMilestone = ">>"
	MarkerNarration = ">"
	MarkerService   = "|"
)

// Legend returns a one-line key to the log-line markers, suitable for printing
// once at the top of a run so users can decode the stream.
func Legend() string {
	return fmt.Sprintf("markers: %s lifecycle milestone   %s codefly narration   %s service output",
		MarkerMilestone, MarkerNarration, MarkerService)
}

type Logger struct {
	suppressed bool
}

var cliLogger *Logger

// timestamps controls whether streamed log lines are prefixed with a
// wall-clock HH:MM:SS timestamp. On by default so output correlates with
// other tools' logs out of the box; toggled by the --timestamps flag.
var timestamps atomic.Bool

func init() {
	cliLogger = &Logger{}
	agents.AddProcessor(cliLogger)
	timestamps.Store(true)
}

// SetTimestamps enables or disables the wall-clock timestamp prefix on
// streamed log lines.
func SetTimestamps(on bool) {
	timestamps.Store(on)
}

func GetLogger() *Logger {
	return cliLogger
}

// SuppressOutput stops the CLI logger from printing to stdout.
// Use when a TUI is active and consuming logs via a channel instead.
func SuppressOutput() {
	cliLogger.suppressed = true
}

// RestoreOutput re-enables CLI logger stdout after SuppressOutput, e.g. once a
// TUI has exited and the command needs to print a final report.
func RestoreOutput() {
	cliLogger.suppressed = false
}

var maxUnique int

func RegisterLoggingResource(unique string) {
	maxUnique = max(maxUnique, len(unique))
}

func (logger *Logger) Process(log *wool.Log) {
	if log.Level < wool.GlobalLogLevel() {
		return
	}
	// log is a fmt.Stringer; call String() directly instead of routing every
	// line through fmt.Sprintf("%s", ...)'s reflection path.
	s := log.String()
	if logger.suppressed {
		emitToSink(log.Level, s)
		return
	}
	switch log.Level {
	case wool.TRACE:
		Trace("%s", s)
	case wool.DEBUG:
		Debug("%s", s)
	case wool.INFO:
		Info("%s", s)
	case wool.WARN:
		Warning("%s", s)
	case wool.ERROR:
		Error("%s", s)
	case wool.FOCUS:
		Focus("%s", s)
	default:
		fmt.Println(s)
	}
}

func (logger *Logger) ProcessWithSource(source *wool.Identifier, log *wool.Log) {
	if logger.suppressed || log.Level < wool.GlobalLogLevel() {
		return
	}
	if source == nil {
		logger.Process(log)
		return
	}
	if source.IsSystem() {
		logger.Process(log)
		return
	}
	Log(source, log)
}

func padRight(str string, length int) string {
	return fmt.Sprintf("%-*s", length, str)
}

var (
	silentMu sync.RWMutex
	silent   []string
)

func Log(identifier *wool.Identifier, log *wool.Log) {
	if identifier == nil || log == nil {
		return
	}
	sep := MarkerService
	if log.Level == wool.FORWARD {
		silentMu.RLock()
		for _, s := range silent {
			if strings.HasPrefix(identifier.Unique, s) {
				silentMu.RUnlock()
				return
			}
		}
		silentMu.RUnlock()
		sep = MarkerNarration
	}
	for _, text := range formattedLogLines(identifier.Unique, sep, log) {
		if log.Level == wool.FOCUS {
			fmt.Println(tui.ServiceFocusRenderer(identifier.Unique)(text))
		} else {
			fmt.Println(tui.ServiceRenderer(identifier.Unique)(text))
		}
	}
}

func formattedLogLines(source string, separator string, log *wool.Log) []string {
	message := strings.ReplaceAll(log.String(), "\r\n", "\n")
	message = strings.TrimRight(message, "\r\n")
	lines := strings.Split(message, "\n")
	prefix := fmt.Sprintf("%s %s ", padRight(source, maxUnique), separator)
	if timestamps.Load() {
		prefix = time.Now().Format("15:04:05") + " " + prefix
	}
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimRight(line, " \t\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		out = append(out, prefix+line)
	}
	return out
}

func (logger *Logger) Oops(format string, args ...any) {
	Error(format, args...)
	Done()
	os.Exit(1)
}

func WithSilence(services []*resources.ServiceWithModule) {
	silentMu.Lock()
	defer silentMu.Unlock()
	for _, s := range services {
		silent = append(silent, s.Unique())
	}
}
