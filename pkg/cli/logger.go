package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/codefly-dev/core/agents"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/tui"
	"github.com/codefly-dev/core/wool"
)

type Logger struct {
	suppressed bool
}

var cliLogger *Logger

func init() {
	cliLogger = &Logger{}
	agents.AddProcessor(cliLogger)
}

func GetLogger() *Logger {
	return cliLogger
}

// SuppressOutput stops the CLI logger from printing to stdout.
// Use when a TUI is active and consuming logs via a channel instead.
func SuppressOutput() {
	cliLogger.suppressed = true
}

var maxUnique int

func RegisterLoggingResource(unique string) {
	maxUnique = max(maxUnique, len(unique))
}

func (logger *Logger) Process(log *wool.Log) {
	if logger.suppressed || log.Level < wool.GlobalLogLevel() {
		return
	}
	switch log.Level {
	case wool.TRACE:
		Trace(fmt.Sprintf("%s", log))
	case wool.DEBUG:
		Debug(fmt.Sprintf("%s", log))
	case wool.INFO:
		Info(fmt.Sprintf("%s", log))
	case wool.WARN:
		Warning(fmt.Sprintf("%s", log))
	case wool.ERROR:
		Error(fmt.Sprintf("%s", log))
	case wool.FOCUS:
		Focus(fmt.Sprintf("%s", log))
	default:
		fmt.Printf("%s\n", log)
	}
}

func (logger *Logger) ProcessWithSource(source *wool.Identifier, log *wool.Log) {
	if logger.suppressed || log.Level < wool.GlobalLogLevel() {
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

var silent []string

func Log(identifier *wool.Identifier, log *wool.Log) {
	sep := "||"
	if log.Level == wool.FORWARD {
		for _, s := range silent {
			if strings.HasPrefix(identifier.Unique, s) {
				return
			}
		}
		sep = ">>"
	}
	text := fmt.Sprintf("%s %s %s", padRight(identifier.Unique, maxUnique), sep, log)
	if log.Level == wool.FOCUS {
		fmt.Println(tui.ServiceFocusRenderer(identifier.Unique)(text))
	} else {
		fmt.Println(tui.ServiceRenderer(identifier.Unique)(text))
	}
}

func (logger *Logger) Oops(format string, args ...any) {
	Error(format, args...)
	os.Exit(1)
}

func WithSilence(services []*resources.ServiceWithModule) {
	for _, s := range services {
		silent = append(silent, s.Unique())
	}
}
