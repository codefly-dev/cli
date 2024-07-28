package cli

import (
	"fmt"
	"hash/fnv"
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/codefly-dev/core/agents"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
)

type Logger struct {
}

var cliLogger *Logger

func init() {
	cliLogger = &Logger{}
	agents.AddProcessor(cliLogger)

}

func GetLogger() *Logger {
	return cliLogger
}

var maxUnique int

func RegisterLoggingResource(unique string) {
	maxUnique = max(maxUnique, len(unique))
}

func (logger *Logger) Process(log *wool.Log) {
	if log.Level < wool.GlobalLogLevel() {
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
	if log.Level < wool.GlobalLogLevel() {
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
	style := GetBaseStyle(identifier.Unique)
	if log.Level == wool.FOCUS {
		style = style.Copy()
		style.Border(lipgloss.RoundedBorder())
	}
	fmt.Println(style.Render(fmt.Sprintf("%s %s %s", padRight(identifier.Unique, maxUnique), sep, log)))
}

func (logger *Logger) Oops(format string, args ...any) {
	Error(format, args...)
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
		lipgloss.Color("#00FF00"), // Bright Green
		lipgloss.Color("#00FFFF"), // Cyan
		lipgloss.Color("#FF1493"), // Neon Pink
		lipgloss.Color("#7DF9FF"), // Electric Blue
		lipgloss.Color("#FF69B4"), // Hot Pink
		lipgloss.Color("#C0C0C0"), // Silver
		lipgloss.Color("#FFD700"), // Gold
		lipgloss.Color("#FF4500"), // Orange Red
		lipgloss.Color("#9370DB"), // Medium Purple
		lipgloss.Color("#3CB371"), // Medium Sea Green
		lipgloss.Color("#20B2AA"), // Light Sea Green
		lipgloss.Color("#DDA0DD"), // Plum
		lipgloss.Color("#B0E0E6"), // Powder Blue
		lipgloss.Color("#FF6347"), // Tomato
		lipgloss.Color("#4682B4"), // Steel Blue
		lipgloss.Color("#D2691E"), // Chocolate
		lipgloss.Color("#FFDAB9"), // Peach Puff
		lipgloss.Color("#7B68EE"), // Medium Slate Blue
		lipgloss.Color("#BA55D3"), // Medium Orchid
		lipgloss.Color("#F0E68C"), // Khaki
		lipgloss.Color("#48D1CC"), // Medium Turquoise
		lipgloss.Color("#FFB6C1"), // Light Pink
		lipgloss.Color("#DEB887"), // Burlywood
		lipgloss.Color("#AFEEEE"), // Pale Turquoise
		lipgloss.Color("#98FB98"), // Pale Green
		lipgloss.Color("#FFA07A"), // Light Salmon
		lipgloss.Color("#E0FFFF"), // Light Cyan
		lipgloss.Color("#D8BFD8"), // Thistle
		lipgloss.Color("#FFDAB9"), // Peach Puff
		lipgloss.Color("#CD853F"), // Peru
		lipgloss.Color("#FFA500"), // Orange
		lipgloss.Color("#F0FFF0"), // Honeydew
		lipgloss.Color("#F5DEB3"), // Wheat
		lipgloss.Color("#FAFAD2"), // Light Goldenrod Yellow
		lipgloss.Color("#B0C4DE"), // Light Steel Blue
		lipgloss.Color("#FF00FF"), // Magenta
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
	hash := hashString(unique)

	fgColor := cp.foregroundColors[hash%uint32(len(cp.foregroundColors))]

	return lipgloss.NewStyle().
		Foreground(fgColor)
}

var styles map[string]lipgloss.Style
var silent []string

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

func WithSilence(services []*resources.ServiceWithModule) {
	for _, s := range services {
		silent = append(silent, s.Unique())
	}
}
