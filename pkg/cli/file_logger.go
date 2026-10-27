package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/codefly-dev/core/agents"
	"github.com/codefly-dev/core/resources"
	"github.com/codefly-dev/core/wool"
)

// FileLogger owns one invocation directory and its dated JSON log segments.
// Rotates when file exceeds maxSize bytes.
type FileLogger struct {
	mu      sync.Mutex
	dir     string
	maxSize int64 // bytes, 0 = no limit
	file    *os.File
	size    int64
	date    string // current date for rotation
	segment uint64
	closed  bool
}

const defaultMaxSize = 50 * 1024 * 1024 // 50MB

// NewFileLogger allocates an exclusive invocation directory under dir.
// Every rotation remains in that directory, including late shutdown records.
func NewFileLogger(dir string) (*FileLogger, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("cannot create log directory: %w", err)
	}
	invocation, err := os.MkdirTemp(dir, "run-"+time.Now().UTC().Format("2006-01-02T150405.000000000Z")+"-")
	if err != nil {
		return nil, fmt.Errorf("cannot create invocation log directory: %w", err)
	}
	fl := &FileLogger{dir: invocation, maxSize: defaultMaxSize}
	if err := fl.rotate(); err != nil {
		_ = os.Remove(invocation) // Only the empty directory allocated above.
		return nil, err
	}
	return fl, nil
}

func (fl *FileLogger) rotate() error {
	today := time.Now().Format("2006-01-02")
	if fl.file != nil && fl.date == today && (fl.maxSize == 0 || fl.size < fl.maxSize) {
		return nil
	}
	name := filepath.Join(fl.dir, fmt.Sprintf("%s-%06d.log", today, fl.segment))
	f, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("cannot open log file: %w", err)
	}
	if fl.file != nil {
		if err := fl.file.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: cannot close log file: %v\n", err)
		}
	}
	fl.file, fl.date, fl.size = f, today, 0
	fl.segment++
	return nil
}

type fileEntry struct {
	Time    string           `json:"time"`
	Level   string           `json:"level"`
	Source  string           `json:"source,omitempty"`
	Header  string           `json:"header,omitempty"`
	Message string           `json:"message"`
	Fields  []*wool.LogField `json:"fields,omitempty"`
}

func (fl *FileLogger) Process(log *wool.Log) {
	fl.ProcessWithSource(nil, log)
}

func (fl *FileLogger) ProcessWithSource(source *wool.Identifier, log *wool.Log) {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	if fl.closed {
		return
	}

	if err := fl.rotate(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: log rotation failed: %v\n", err)
		return
	}

	entry := fileEntry{
		Time:    time.Now().Format("15:04:05.000"),
		Level:   log.Level.String(),
		Header:  log.Header,
		Message: log.Message,
		Fields:  log.Fields,
	}
	if source != nil {
		entry.Source = source.Unique
	}
	data, err := json.Marshal(entry)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: cannot marshal log entry: %v\n", err)
		return
	}
	n, err := fl.file.Write(append(data, '\n'))
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to write log entry: %v\n", err)
		return
	}
	fl.size += int64(n)
}

func (fl *FileLogger) Close() error {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	if fl.closed {
		return nil
	}
	fl.closed = true
	if fl.file != nil {
		return fl.file.Close()
	}
	return nil
}

// --- Global file logger setup ---

var (
	fileLoggerMu     sync.Mutex
	globalFileLogger *FileLogger
)

// EnableFileLogging registers one sink for the CLI invocation, including all
// helper contexts and final teardown. The process owns its descriptor lifetime.
func EnableFileLogging() {
	fileLoggerMu.Lock()
	defer fileLoggerMu.Unlock()
	if globalFileLogger != nil {
		return
	}
	fl, err := NewFileLogger(LogsDir())
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: cannot enable file logging: %v\n", err)
		return
	}
	globalFileLogger = fl
	agents.AddProcessor(fl)
}

// GetFileLogger returns the invocation's sink (nil if not enabled).
func GetFileLogger() *FileLogger {
	fileLoggerMu.Lock()
	defer fileLoggerMu.Unlock()
	return globalFileLogger
}

// Path returns the current segment, retaining the final path after Close so
// a failure report can still name it. An uninitialized logger returns "".
func (fl *FileLogger) Path() string {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	if fl.file == nil {
		return ""
	}
	return fl.file.Name()
}

// LogFilePath returns the active log file path (or "" when file logging is off).
// Used to point users at the detailed log on failure.
func LogFilePath() string {
	fl := GetFileLogger()
	if fl == nil {
		return ""
	}
	return fl.Path()
}

// LogsDir resolves the existing Codefly home policy without creating a logger.
// Read-only log commands must not allocate a new invocation.
func LogsDir() string { return filepath.Join(resources.CodeflyHomeDir(), "logs") }
