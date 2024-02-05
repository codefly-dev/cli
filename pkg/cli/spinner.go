package cli

import (
	"time"

	"github.com/briandowns/spinner"
)

// Spinner returns a started spinner
func Spinner() *spinner.Spinner {
	s := spinner.New(spinner.CharSets[11], 100*time.Millisecond) // Use different character sets and duration
	return s
}
