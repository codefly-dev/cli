package composition

import "os"

func validateAuthorityAccess(_ *os.File) error {
	// Linux POSIX ACL named-user/group grants are bounded by the group mode mask,
	// already checked together with the owner on the open handle.
	return nil
}
