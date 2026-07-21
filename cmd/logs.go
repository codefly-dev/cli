package cmd

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/cli"
	"github.com/codefly-dev/core/tui"
	"github.com/codefly-dev/core/wool"
	"github.com/spf13/cobra"
)

var (
	logsFollow bool
	logsTail   int
	logsRaw    bool
	logsPath   bool
	logsFile   string
)

// LogsCmd shows the codefly CLI session logs (~/.codefly/logs/<date>.log) — the
// same file the failure hint ("↳ full logs: …") points at. This is the fast
// path from "something broke" to "here's what happened" without hunting for the
// file or remembering --debug.
var LogsCmd = &cobra.Command{
	Use:   "logs",
	Short: "Read or follow logs from the current Codefly session",
	Long: `Show codefly's detailed session logs (~/.codefly/logs/<date>.log).

By default prints the last 100 lines of today's log, pretty-printed. This is the
log the failure hint points at, so the usual flow after a command fails is:

  codefly logs            # what just happened
  codefly logs -f         # follow live (e.g. in a second terminal during a run)
  codefly logs -n 500     # more history
  codefly logs --raw      # raw JSON lines (for jq / grep)
  codefly logs --path     # just print the file path (for scripts)`,
	RunE: func(cmd *cobra.Command, args []string) error {
		path := logsFile
		if path == "" {
			var err error
			path, err = resolveLogFile()
			if err != nil {
				return err
			}
		}

		if logsPath {
			fmt.Println(path)
			return nil
		}

		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("cannot open log file %s: %w", path, err)
		}
		defer f.Close()

		// Print the resolved path to stderr so stdout stays the log content.
		fmt.Fprintln(os.Stderr, tui.RenderInfo(fmt.Sprintf("logs: %s", path)))

		lines := tailLines(path, logsTail)
		for _, line := range lines {
			printLogLine(line)
		}

		if !logsFollow {
			return nil
		}

		// Follow: seek to end, poll for new lines. Ctrl-C exits.
		ctx, stop := common.SignalContext(cmd.Context())
		defer stop()
		if _, err := f.Seek(0, 2); err != nil {
			return err
		}
		reader := bufio.NewReader(f)
		var retryCount int
		for {
			select {
			case <-ctx.Done():
				return nil
			default:
			}
			line, err := reader.ReadString('\n')
			if len(line) > 0 {
				printLogLine(strings.TrimRight(line, "\n"))
				retryCount = 0 // reset on any successful read
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					// Expected when following a live file — wait a beat, then
					// only resume reading once the file has actually grown.
					// Avoids a tight poll loop when the file ends without a
					// newline and stops growing.
					time.Sleep(200 * time.Millisecond)
					pos, errSeek := f.Seek(0, io.SeekCurrent)
					if errSeek != nil {
						fmt.Fprintf(os.Stderr, "error reading log: %v\n", errSeek)
						return errSeek
					}
					if stat, errStat := f.Stat(); errStat == nil && stat.Size() > pos {
						reader = bufio.NewReader(f)
					}
				} else {
					// Real error (permission denied, I/O error, etc.). Retry a
					// few times to ride out transient blips, then give up loud.
					retryCount++
					if retryCount > 5 {
						fmt.Fprintf(os.Stderr, "error reading log: %v\n", err)
						return err
					}
					time.Sleep(200 * time.Millisecond)
				}
			}
		}
	},
}

// resolveLogFile returns today's log file, falling back to the most recent
// *.log in the logs dir.
func resolveLogFile() (string, error) {
	dir := cli.LogsDir()
	if dir == "" {
		return "", fmt.Errorf("cannot resolve logs directory")
	}
	today := filepath.Join(dir, time.Now().Format("2006-01-02")+".log")
	if _, err := os.Stat(today); err == nil {
		return today, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("no logs found in %s (run a codefly command first)", dir)
	}
	var logs []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".log") {
			logs = append(logs, e.Name())
		}
	}
	if len(logs) == 0 {
		return "", fmt.Errorf("no logs found in %s (run a codefly command first)", dir)
	}
	sort.Strings(logs) // date-prefixed names sort chronologically
	return filepath.Join(dir, logs[len(logs)-1]), nil
}

// tailLines returns the last n lines of a file (all lines when n <= 0).
func tailLines(path string, n int) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var all []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		all = append(all, sc.Text())
	}
	if n <= 0 || n >= len(all) {
		return all
	}
	return all[len(all)-n:]
}

func printLogLine(line string) {
	if logsRaw || line == "" {
		fmt.Println(line)
		return
	}
	var e struct {
		Time    string           `json:"time"`
		Level   string           `json:"level"`
		Source  string           `json:"source,omitempty"`
		Header  string           `json:"header,omitempty"`
		Message string           `json:"message"`
		Fields  []*wool.LogField `json:"fields,omitempty"`
	}
	if json.Unmarshal([]byte(line), &e) != nil {
		fmt.Println(line) // not our JSON shape — print verbatim
		return
	}
	level := logLevelLabel(e.Level)
	var b strings.Builder
	b.WriteString(e.Time)
	b.WriteString("  ")
	b.WriteString(fmt.Sprintf("%-5s", level))
	if e.Header != "" {
		b.WriteString("  [")
		b.WriteString(e.Header)
		b.WriteString("]")
	}
	b.WriteString("  ")
	b.WriteString(e.Message)
	for _, fld := range e.Fields {
		if fld != nil {
			b.WriteString(fmt.Sprintf("  %s", fld))
		}
	}
	fmt.Println(b.String())
}

// logLevelLabel maps the numeric wool.Loglevel stored in the log file to a name.
func logLevelLabel(raw string) string {
	n, err := strconv.Atoi(raw)
	if err != nil {
		return raw
	}
	return wool.Loglevel(n).String()
}

func init() {
	LogsCmd.Flags().BoolVarP(&logsFollow, "follow", "f", false, "Follow the log live")
	LogsCmd.Flags().IntVarP(&logsTail, "tail", "n", 100, "Show the last N lines (0 = all)")
	LogsCmd.Flags().BoolVar(&logsRaw, "raw", false, "Print raw JSON lines (for jq/grep)")
	LogsCmd.Flags().BoolVar(&logsPath, "path", false, "Print only the log file path")
	LogsCmd.Flags().StringVar(&logsFile, "file", "", "Read a specific log file instead of today's")
}
