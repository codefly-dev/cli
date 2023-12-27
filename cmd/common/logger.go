package common

import (
	"fmt"
	"hash/fnv"
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/agents"
	"github.com/codefly-dev/core/wool"
)

type CLILogger struct {
}

var cliLogger *CLILogger

func init() {
	cliLogger = &CLILogger{}
	agents.AddProcessor(cliLogger)
}

func CLI() *CLILogger {
	return cliLogger
}

func (logger *CLILogger) Process(log *wool.Log) {
	if log.Level < wool.GlobalLogLevel() {
		return
	}
	switch log.Level {
	case wool.TRACE:
		cli.Trace(fmt.Sprintf("%s", log))
	case wool.DEBUG:
		cli.Debug(fmt.Sprintf("%s", log))
	case wool.INFO:
		cli.Info(fmt.Sprintf("%s", log))
	case wool.WARN:
		cli.Warning(fmt.Sprintf("%s", log))
	case wool.ERROR:
		cli.Error(fmt.Sprintf("%s", log))
	case wool.FOCUS:
		cli.Focus(fmt.Sprintf("%s", log))
	default:
		fmt.Printf("%s\n", log)
	}
}

func (logger *CLILogger) ProcessWithSource(log *wool.Log, source *wool.Identifier) {
	if log.Level < wool.GlobalLogLevel() {
		return
	}
	if source.IsSystem() {
		logger.Process(log)
		return
	}
	Log(source, log)
}

func Log(identifier *wool.Identifier, log *wool.Log) {
	sep := "|"
	if log.Level == wool.FORWARD {
		sep = ">>"
	}
	style := GetBaseStyle(identifier.Unique)
	if log.Level == wool.FOCUS {
		style = style.Copy()
		style.Border(lipgloss.RoundedBorder())
	}
	fmt.Println(style.Render(fmt.Sprintf("%s %s %s", identifier.Unique, sep, log)))
}

func (logger *CLILogger) Oops(format string, args ...any) {
	cli.Error(format, args...)
	os.Exit(1)
}

type ColorPicker struct {
	foregroundColors []lipgloss.Color
	backgroundColors []lipgloss.Color
}

func generateForegroundColors() []lipgloss.Color {
	return []lipgloss.Color{
		lipgloss.Color("#ADD8E6"), // Light Blue
		lipgloss.Color("#90EE90"), // Soft Green
		lipgloss.Color("#FFC0CB"), // Pale Pink
		lipgloss.Color("#E6E6FA"), // Lavender
		lipgloss.Color("#F08080"), // Light Coral
		lipgloss.Color("#F5DEB3"), // Wheat
		lipgloss.Color("#00FF00"), // Bright Green
		lipgloss.Color("#00FFFF"), // Cyan
		lipgloss.Color("#FF1493"), // Neon Pink
		lipgloss.Color("#7DF9FF"), // Electric Blue
		lipgloss.Color("#FF69B4"), // Hot Pink
		lipgloss.Color("#C0C0C0"), // Silver
	}
}

func NewColorPicker() *ColorPicker {
	backgroundColors := []lipgloss.Color{
		lipgloss.Color("#333333"), lipgloss.Color("#444444"), // ... add more colors
	}
	return &ColorPicker{backgroundColors: backgroundColors, foregroundColors: generateForegroundColors()}
}

func hashString(s string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return h.Sum32()
}

func (cp *ColorPicker) PickStyle(unique string) lipgloss.Style {
	// Split in "/"
	parts := strings.Split(unique, "/")
	if len(parts) != 2 {
		return lipgloss.NewStyle()
	}
	hashApp := hashString(parts[0])
	hashService := hashString(parts[1])

	fgColor := cp.foregroundColors[hashService%uint32(len(cp.foregroundColors))]
	bgColor := cp.backgroundColors[hashApp%uint32(len(cp.backgroundColors))]

	return lipgloss.NewStyle().
		Foreground(fgColor).
		Background(bgColor)
}

var styles map[string]lipgloss.Style

func init() {
	styles = map[string]lipgloss.Style{}
}

func GetBaseStyle(unique string) lipgloss.Style {
	if style, ok := styles[unique]; ok {
		return style
	}
	style := NewColorPicker().PickStyle(unique)
	styles[unique] = style
	return style
}
