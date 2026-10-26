//go:build darwin

package processgroup

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// TestDarwinBootIDNamesTheBootSessionNotTheClock pins the boot id to the one
// Darwin value that holds still for a whole boot. kern.boottime moves every
// time the clock is set, and a boot id built from it made every live record
// stop matching its own process: the reaper dropped the record as a reused
// group and the group could never be reaped again.
func TestDarwinBootIDNamesTheBootSessionNotTheClock(t *testing.T) {
	session, err := unix.Sysctl("kern.bootsessionuuid")
	if err != nil {
		t.Fatal(err)
	}
	bootID, err := darwinBootID()
	if err != nil {
		t.Fatal(err)
	}
	if bootID != session {
		t.Fatalf("boot id = %q, want the boot session %q", bootID, session)
	}
	bootTime, err := unix.SysctlTimeval("kern.boottime")
	if err != nil {
		t.Fatal(err)
	}
	for _, clockDerived := range []string{
		fmt.Sprintf("%d:%d", bootTime.Sec, bootTime.Usec),
		fmt.Sprintf("%d", bootTime.Sec),
	} {
		if strings.Contains(bootID, clockDerived) {
			t.Fatalf("boot id %q is derived from kern.boottime %q, which the kernel rewrites when the clock is set", bootID, clockDerived)
		}
	}

	identity, err := inspectNativeProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if identity.bootID != session {
		t.Fatalf("inspected boot id = %q, want the boot session %q", identity.bootID, session)
	}
}
