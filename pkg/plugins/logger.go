package plugins

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"runtime/debug"
	"strings"

	"github.com/codefly-dev/core/shared"
	"github.com/fatih/color"
	"github.com/hashicorp/go-hclog"
	"github.com/pkg/errors"
)

/*
logger used to take the output of the service
*/

type ServiceLogger struct {
	Name      string
	transport hclog.Logger
}

func (l *ServiceLogger) SetLevel(lvl shared.LogLevel) {
	// TODO implement me
	panic("implement me")
}

func NewServiceLogger(name string) *ServiceLogger {
	logger := hclog.New(&hclog.LoggerOptions{
		JSONFormat: true,
	})
	return &ServiceLogger{Name: name, transport: logger}
}

func (l *ServiceLogger) Write(p []byte) (n int, err error) {
	if len(p) == 0 {
		return 0, nil
	}
	entry := LogEntry{
		Msg:    string(p),
		Sender: l.Name,
	}
	data, err := json.Marshal(entry)
	if err != nil {
		// Log the error to a fallback logger or stderr.
		return fmt.Fprintf(os.Stderr, "Could not marshal log entry: %v\n", err)
	}

	writer := l.transport.StandardWriter(&hclog.StandardLoggerOptions{})
	n, err = writer.Write(data)
	if err != nil {
		// Log the error to a fallback logger or stderr.
		return fmt.Fprintf(os.Stderr, "Could not write to StandardWriter: %v\n", err)
	}
	return n, err
}

func (l *ServiceLogger) UnsafeWrite(s string) {
	_, err := l.Write([]byte(s))
	if err != nil {
		panic(err)
	}
}

func (l *ServiceLogger) Info(format string, args ...any) {
	l.UnsafeWrite(fmt.Sprintf(format, args...))
}

func (l *ServiceLogger) Tracef(format string, args ...any) {
	// TODO implement me
	panic("implement me")
}

func (l *ServiceLogger) Debugf(format string, args ...any) {
	// TODO implement me
	panic("implement me")
}

func (l *ServiceLogger) DebugMe(format string, args ...any) {
	l.UnsafeWrite(fmt.Sprintf(format, args...))
}

func (l *ServiceLogger) TODO(format string, args ...any) {
	// TODO implement me
	panic("implement me")
}

func (l *ServiceLogger) Wrapf(err error, format string, args ...any) error {
	// TODO implement me
	panic("implement me")
}

func (l *ServiceLogger) Errorf(format string, args ...any) error {
	// TODO implement me
	panic("implement me")
}

/*
logger used by plugin surrounding
*/

// TODO: Hook std.Out to it

// NewLogger returns a Logger returns JSON with the following fields:
// @message
// @specialization

type PluginLogger struct {
	Name      string
	transport hclog.Logger
	debug     bool
	trace     bool
}

func (l *PluginLogger) SetLevel(lvl shared.LogLevel) {
	if lvl == shared.TraceLevel {
		l.trace = true
		l.debug = true
	} else if lvl == shared.DebugLevel {
		l.debug = true
	}
}

func (l *PluginLogger) SetDebug() {
	l.debug = true
	l.transport.SetLevel(hclog.Debug)
}

func (l *PluginLogger) SetTrace() {
	l.trace = true
	l.transport.SetLevel(hclog.Trace)
}

func (l *PluginLogger) Wrapf(err error, format string, args ...any) error {
	return errors.Wrapf(err, format, args...)
}

func NewPluginLogger(name string) *PluginLogger {
	pluginName := fmt.Sprintf("plugin:%s", name)
	logger := hclog.New(&hclog.LoggerOptions{
		JSONFormat: true,
	})
	return &PluginLogger{Name: pluginName, transport: logger}
}

type LogEntry struct {
	Msg     string
	Sender  string
	DebugMe bool
}

func (l *PluginLogger) WriteEntry(entry LogEntry) (n int, err error) {
	data, err := json.Marshal(entry)
	if err != nil {
		// Log the error to a fallback logger or stderr.
		return fmt.Fprintf(os.Stderr, "Could not marshal log entry: %v\n", err)
	}

	writer := l.transport.StandardWriter(&hclog.StandardLoggerOptions{})
	n, err = writer.Write(data)
	if err != nil {
		// Log the error to a fallback logger or stderr.
		return fmt.Fprintf(os.Stderr, "Could not write to StandardWriter: %v\n", err)
	}
	return n, err
}

func (l *PluginLogger) Write(p []byte) (n int, err error) {
	if len(p) == 0 {
		return 0, nil
	}
	return l.WriteEntry(LogEntry{
		Msg:    string(p),
		Sender: l.Name,
	})
}

func (l *PluginLogger) UnsafeWrite(s string) {
	_, err := l.Write([]byte(s))
	if err != nil {
		panic(err)
	}
}

func (l *PluginLogger) Tracef(format string, args ...any) {
	if !l.trace {
		return
	}
	l.UnsafeWrite(fmt.Sprintf(format, args...))
}

func (l *PluginLogger) Debugf(format string, args ...any) {
	if !l.debug || l.trace {
		return
	}
	l.UnsafeWrite(fmt.Sprintf(format, args...))
}

func (l *PluginLogger) DebugMe(format string, args ...any) {
	if !l.debug {
		return
	}
	_, _ = l.WriteEntry(LogEntry{
		Msg:     fmt.Sprintf(format, args...),
		Sender:  l.Name,
		DebugMe: true,
	})
}

var todos map[string]bool

func init() {
	todos = make(map[string]bool)
}

func (l *PluginLogger) TODO(format string, args ...any) {
	if !l.debug {
		return
	}
	if _, ok := todos[format]; ok {
		return
	}
	todos[format] = true

	_, _ = l.WriteEntry(LogEntry{
		Msg:    fmt.Sprintf(fmt.Sprintf("⚠️TODO %s", format), args...),
		Sender: l.Name,
	})
}

func (l *PluginLogger) Info(format string, args ...any) {
	l.UnsafeWrite(fmt.Sprintf(format, args...))
}

func (l *PluginLogger) Errorf(format string, args ...any) error {
	l.TODO("Implement with gRPC errors properly")
	return fmt.Errorf(format, args...)
}

func (l *PluginLogger) Warn(format string, args ...any) {
	l.UnsafeWrite(fmt.Sprintf(fmt.Sprintf("WARN: %s", format), args...))
}

func (l *PluginLogger) Catch() {
	if r := recover(); r != nil {
		l.Debugf("IN PANIC CATCH")
		l.Warn("PANIC CAUGHT INSIDE THE PLUGIN CODE -- STOPPING EVERYTHING: %v", r)
		l.Warn(string(debug.Stack()))
	}
}

/*
logger used by Codefly server
*/

var logger hclog.Logger

func NewServerLogger(debug bool) hclog.Logger {
	if logger != nil {
		return logger
	}
	logger = hclog.New(&hclog.LoggerOptions{
		JSONFormat: true,
		Output:     NewServerFormatter(debug),
		Level:      hclog.Debug,
	})
	return logger
}

type ColorPicker struct {
	current int
	colors  []color.Attribute
}

func NewColorPicker() *ColorPicker {
	return &ColorPicker{
		colors: []color.Attribute{
			color.FgBlue,
			color.FgGreen,
			color.FgMagenta,
			color.FgCyan,
			color.FgYellow,
			color.FgWhite,
		},
	}
}

func (cp *ColorPicker) Next() *color.Color {
	if cp.current >= len(cp.colors) {
		cp.current = 0
	}

	c := color.New(cp.colors[cp.current])
	cp.current++
	return c
}

type ServerFormatter struct {
	buffer bytes.Buffer
	picker *ColorPicker
	colors map[string]*color.Color
	debug  bool
}

func NewServerFormatter(debug bool) *ServerFormatter {
	return &ServerFormatter{
		picker: NewColorPicker(),
		colors: make(map[string]*color.Color),
		debug:  debug,
	}
}

func (out *ServerFormatter) Write(p []byte) (n int, err error) {
	//fmt.Printf("ServerFormatter: %s\n", string(p))
	n, err = out.buffer.Write(p)
	if err != nil {
		return
	}
	defer out.buffer.Reset()

	var data map[string]any
	err = json.Unmarshal(out.buffer.Bytes(), &data)
	if err != nil {
		return
	}
	format := data["@message"].(string)
	err = json.Unmarshal([]byte(format), &data)
	if err != nil {
		return
	}
	sender := data["Sender"].(string)
	message := strings.TrimSpace(data["Msg"].(string))
	// Don't print empty lines
	if message == "" {
		return
	}
	// Only show plugin messages in debug mode
	if !out.debug && strings.HasPrefix(sender, "plugin:") {
		return
	}

	if _, ok := out.colors[sender]; !ok {
		out.colors[sender] = out.picker.Next()
	}
	c := out.colors[sender]
	// debug me bool
	debugMe := data["DebugMe"]
	if debugMe != nil && debugMe.(bool) {
		// reserved for debug me
		c = color.New(color.FgRed, color.Bold)
	}
	n, err = c.Printf("[%s] %s\n", sender, message)
	if err != nil {
		return
	}
	return
}

func NoLogger() hclog.Logger {
	return hclog.NewNullLogger()
}
