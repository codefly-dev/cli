package processgroup

// These tests start processes and read back whose run they belong to from the
// environment those processes inherited; a run surrounding the test must not be
// that run.
import _ "github.com/codefly-dev/cli/internal/testenv"
