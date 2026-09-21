//go:build darwin && cgo

package composition

/*
#include <sys/acl.h>
#include <errno.h>

// Darwin extended ACLs can grant mutation independently of POSIX mode bits.
// Reject mutation grants; read-only grants and ordinary deny-delete ACLs remain
// valid. Inspect the open handle, not a path that could resolve differently.
static int approval_acl_check(int fd) {
    acl_t acl = acl_get_fd_np(fd, ACL_TYPE_EXTENDED);
    // On an already-open descriptor Darwin reports ENOENT for no extended ACL.
    // Other errors (including unsupported retrieval) are not absence evidence.
    if (acl == NULL) return errno == ENOENT ? 0 : errno;
    if (acl_valid(acl) != 0) {
        int invalid = errno;
        acl_free(acl);
        return invalid;
    }
    acl_entry_t entry;
    int next = ACL_FIRST_ENTRY;
    int result = 0;
    while (acl_get_entry(acl, next, &entry) == 0) {
        next = ACL_NEXT_ENTRY;
        acl_tag_t tag;
        acl_permset_t perms;
        if (acl_get_tag_type(entry, &tag) != 0 || acl_get_permset(entry, &perms) != 0) {
            result = errno;
            break;
        }
        if (tag != ACL_EXTENDED_ALLOW) continue;
        acl_perm_t writes[] = {ACL_WRITE_DATA, ACL_APPEND_DATA, ACL_DELETE,
            ACL_DELETE_CHILD, ACL_WRITE_ATTRIBUTES, ACL_WRITE_EXTATTRIBUTES,
            ACL_WRITE_SECURITY, ACL_CHANGE_OWNER};
        for (unsigned int i = 0; i < sizeof(writes) / sizeof(writes[0]); i++) {
            int granted = acl_get_perm_np(perms, writes[i]);
            if (granted != 0) {
                result = granted < 0 ? errno : EPERM;
                break;
            }
        }
        if (result != 0) break;
    }
    acl_free(acl);
    return result;
}
*/
import "C"

import (
	"fmt"
	"os"
	"runtime"
	"syscall"
)

func validateAuthorityAccess(file *os.File) error {
	code := C.approval_acl_check(C.int(file.Fd()))
	runtime.KeepAlive(file)
	if code != 0 {
		return fmt.Errorf("approval authority ACL must not grant mutation: %w", syscall.Errno(code))
	}
	return nil
}
