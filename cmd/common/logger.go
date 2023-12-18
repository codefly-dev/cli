package common

import (
	"bytes"
	"fmt"
	"hash/fnv"
	"os"

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

func (logger *CLILogger) ProcessWithSource(msg *wool.Log, source *wool.Identifier) {
	fmt.Println("IDENTIFIER", source, msg)
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

func (cp *ColorPicker) PickStyle(app string, service string) lipgloss.Style {
	hashApp := hashString(app)
	hashService := hashString(service)

	fgColor := cp.foregroundColors[hashApp%uint32(len(cp.foregroundColors))]
	bgColor := cp.backgroundColors[hashService%uint32(len(cp.backgroundColors))]

	return lipgloss.NewStyle().
		Foreground(fgColor).
		Background(bgColor)
}

type ServerFormatter struct {
	buffer bytes.Buffer
	picker *ColorPicker
	debug  bool
	styles map[string]lipgloss.Style
}

func NewServerFormatter() *ServerFormatter {
	return &ServerFormatter{
		picker: NewColorPicker(),
		styles: make(map[string]lipgloss.Style),
	}
}

func (out *ServerFormatter) Write(p []byte) (n int, err error) {
	fmt.Println("got log", string(p))
	n, err = out.buffer.Write(p)
	if err != nil {
		return
	}
	defer out.buffer.Reset()

	// var log LogMessage
	// err = json.Unmarshal(out.buffer.Bytes(), &log)
	// if err != nil {
	// 	fmt.Printf("got error unmarshalling log: %v\n", err)
	// 	return
	// }
	// err = json.Unmarshal([]byte(log.RawMessage), &log.Message)
	// if err != nil {
	// 	log.Message = LogMessageContent{}
	// }

	// message := log.Message.Msg
	// if message == "" {
	// 	return
	// }

	// mgLog := createManagementLog(&log)
	// // Send the management Log to registered callbacks
	// for _, callback := range out.callbacks {
	// 	callback(mgLog)
	// }

	// unique := fmt.Sprintf("%s/%s", log.Message.Application, log.Message.Service)

	// var style lipgloss.Style
	// var ok bool
	// if style, ok = out.styles[unique]; !ok {
	// 	out.styles[unique] = out.picker.PickStyle(log.Message.Application, log.Message.Service)
	// }

	// // debug me bool
	// if log.Message.Level == "debug-me" {
	// 	style = style.Copy().Background(lipgloss.Color("#FFD700")) // gold
	// }
	// sender := fmt.Sprintf("%s/%s", log.Message.Application, log.Message.Service)

	// fmt.Println(style.Render(fmt.Sprintf("[%s] %s", sender, message)))
	return
}
