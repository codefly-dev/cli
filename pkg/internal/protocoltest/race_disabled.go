//go:build !race

package protocoltest

// raceEnabled reports whether this test binary was built with the race
// detector, so the peer it builds matches it.
const raceEnabled = false
