package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/codefly-dev/core/wool"
)

// FileLogger writes JSON log entries to date-stamped files in a directory.
// Rotates when file exceeds maxSize bytes.
type FileLogger struct {
	mu      sync.Mutex
	dir     string
	maxSize int64 // bytes, 0 = no limit
	file    *os.File
	size    int64
	date    string // current date for rotation
}

const defaultMaxSize = 50 * 1024 * 1024 // 50MB

// NewFileLogger creates a file logger writing to dir/<date>.log.
// Rotates daily and when file exceeds maxSize.
func NewFileLogger(dir string) (*FileLogger, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("cannot create log directory: %w", err)
	}
	fl := &FileLogger{dir: dir, maxSize: defaultMaxSize}
	if err := fl.rotate(); err != nil {
		return nil, err
	}
	return fl, nil
}

func (fl *FileLogger) rotate() error {
	today := time.Now().Format("2006-01-02")
	if fl.file != nil && fl.date == today && (fl.maxSize == 0 || fl.size < fl.maxSize) {
		return nil
	}
	if fl.file != nil {
		if err := fl.file.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "warning: cannot close log file: %v\n", err)
		}
	}
	fl.date = today
	name := filepath.Join(fl.dir, today+".log")

	// If file is too big, add a suffix
	info, err := os.Stat(name)
	if err == nil && fl.maxSize > 0 && info.Size() >= fl.maxSize {
		name = filepath.Join(fl.dir, fmt.Sprintf("%s-%d.log", today, time.Now().Unix()))
	}

	f, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("cannot open log file: %w", err)
	}
	info, _ = f.Stat()
	if info != nil {
		fl.size = info.Size()
	}
	fl.file = f
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

	if err := fl.rotate(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: log rotation failed: %v\n", err)
		return
	}

	entry := fileEntry{
		Time:    time.Now().Format("15:04:05.000"),
		Level:   fmt.Sprintf("%d", log.Level),
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
	if fl.file != nil {
		return fl.file.Close()
	}
	return nil
}

// --- Global file logger setup ---

var globalFileLogger *FileLogger

// EnableFileLogging starts writing logs to ~/.codefly/logs/<date>.log
func EnableFileLogging() {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	dir := filepath.Join(home, ".codefly", "logs")
	fl, err := NewFileLogger(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: cannot enable file logging: %v\n", err)
		return
	}
	globalFileLogger = fl
	// Register as an agent log processor too
	// (agents.AddProcessor is called in logger.go init)
}

// GetFileLogger returns the global file logger (nil if not enabled).
func GetFileLogger() *FileLogger {
	return globalFileLogger
}

// Path returns the absolute path of the file currently being written, or ""
// if the logger isn't open.
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
	if globalFileLogger == nil {
		return ""
	}
	return globalFileLogger.Path()
}

// LogsDir returns the directory codefly writes its logs to (~/.codefly/logs),
// or "" if the home directory can't be resolved. Standalone — does not enable
// file logging, so the `codefly logs` command can read existing logs without
// creating a new file.
func LogsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codefly", "logs")
}
