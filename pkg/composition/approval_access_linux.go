package composition

import "os"

func validateAuthorityAccess(_ *os.File) error {
	// Linux POSIX ACL named-user/group grants are bounded by the group mode mask,
	// already checked together with the owner on the open handle.
	return nil
}

func validatePrivateInputAccess(_ *os.File) error {
	// A private directory's zero group/other bits also bound named ACL grants.
	return nil
}
