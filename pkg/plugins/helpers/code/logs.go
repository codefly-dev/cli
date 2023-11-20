package code

import (
	"bufio"
	"io"
)

func ProcessLogs(r io.ReadCloser, w io.Writer) {
	// TODO: Logging error strategy
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		_, _ = w.Write([]byte(scanner.Text()))
	}

	if err := scanner.Err(); err != nil {
		_, _ = w.Write([]byte(err.Error()))
	}

	_ = r.Close()
}
