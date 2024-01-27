package cli

import (
	"time"

	"github.com/briandowns/spinner"
)

func Spinner() *spinner.Spinner {
	s := spinner.New(spinner.CharSets[11], 100*time.Millisecond) // Use different character sets and duration
	s.Start()
	return s
}
