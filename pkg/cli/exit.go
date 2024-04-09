package cli

type Cleanup func()

var cleanups []Cleanup

func RegisterCleanup(cleanup Cleanup) {
	cleanups = append(cleanups, cleanup)
}

func Done() {
	for _, cleanup := range cleanups {
		cleanup()
	}
}
