package common

import (
	"fmt"
	"os"

	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/wool"
)

type CLILogger struct {
}

var cliLogger = &CLILogger{}

func CLI() *CLILogger {
	return cliLogger
}

func (logger *CLILogger) Process(msg *wool.Log) {
	switch msg.Level {
	case wool.TRACE:
		cli.Debug(fmt.Sprintf("%s: %s", msg.Header, msg.Message))
	case wool.WARN:
		cli.Warning(msg.Message)
	default:
		fmt.Println(msg.Message)
	}
}

func (logger *CLILogger) Oops(format string, args ...any) {
	cli.Error(format, args...)
	os.Exit(1)
}
