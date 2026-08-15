//go:build darwin

package processgroup

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

var errProcessNotFound = errors.New("process not found")

func inspectNativeProcess(pid int) (nativeProcessIdentity, error) {
	info, err := readDarwinProcessInfo(pid)
	if err != nil {
		return nativeProcessIdentity{}, err
	}
	bootID, err := darwinBootID()
	if err != nil {
		return nativeProcessIdentity{}, err
	}
	executable := darwinProcessCommand(info)
	if executable == "" {
		return nativeProcessIdentity{}, errors.New("process executable identity is empty")
	}
	return nativeProcessIdentity{
		pid:    pid,
		pgid:   int(info.Eproc.Pgid),
		parent: int(info.Eproc.Ppid),
		bootID: bootID,
		// #nosec G115 -- readDarwinProcessInfo rejects non-positive and negative start-time components.
		startID:    uint64(info.Proc.P_starttime.Sec)*1_000_000 + uint64(info.Proc.P_starttime.Usec),
		executable: executable,
	}, nil
}

func readDarwinProcessInfo(pid int) (*unix.KinfoProc, error) {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		if errors.Is(err, syscall.ESRCH) || errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.EIO) {
			return nil, errProcessNotFound
		}
		return nil, err
	}
	// #nosec G115 -- kernel process identifiers are signed 32-bit values.
	if info.Proc.P_pid != int32(pid) {
		return nil, errProcessNotFound
	}
	if info.Proc.P_starttime.Sec <= 0 || info.Proc.P_starttime.Usec < 0 {
		return nil, errors.New("invalid process start identity")
	}
	return info, nil
}

func darwinProcessCommand(info *unix.KinfoProc) string {
	if info == nil {
		return ""
	}
	command := make([]byte, 0, len(info.Proc.P_comm))
	for _, character := range info.Proc.P_comm {
		if character == 0 {
			break
		}
		command = append(command, byte(character))
	}
	return string(command)
}

func darwinBootID() (string, error) {
	bootTime, err := unix.SysctlTimeval("kern.boottime")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d:%d", bootTime.Sec, bootTime.Usec), nil
}

func readProcessGroupAuthentication(pid int) (string, error) {
	data, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		if errors.Is(err, syscall.ESRCH) || errors.Is(err, syscall.ENOENT) {
			return "", errProcessNotFound
		}
		return "", err
	}
	if len(data) < 4 {
		return "", errors.New("process arguments are incomplete")
	}
	argc := int(binary.NativeEndian.Uint32(data[:4]))
	data = data[4:]
	executableEnd := bytes.IndexByte(data, 0)
	if executableEnd < 0 {
		return "", errors.New("process executable is unterminated")
	}
	data = bytes.TrimLeft(data[executableEnd+1:], "\x00")
	for range argc {
		argumentEnd := bytes.IndexByte(data, 0)
		if argumentEnd < 0 {
			return "", errors.New("process arguments are unterminated")
		}
		data = data[argumentEnd+1:]
	}
	prefix := []byte(groupAuthEnv + "=")
	for entry := range bytes.SplitSeq(data, []byte{0}) {
		if value, ok := bytes.CutPrefix(entry, prefix); ok {
			return string(value), nil
		}
	}
	return "", nil
}

type darwinProcessSignalHandle struct {
	token darwinAuditToken
}

func openProcessSignalHandle(expected *processIdentity) (processSignalHandle, error) {
	unique, err := readDarwinProcessUniqueInfo(expected.pid)
	if err != nil {
		return nil, err
	}
	current, err := inspectProcessIdentity(expected.pid)
	if err != nil {
		return nil, err
	}
	if !current.same(expected) {
		return nil, errProcessGroupIdentityChanged
	}
	token := darwinAuditToken{}
	// #nosec G115 -- process IDs and validated positive PID versions fit the audit-token fields.
	token.Value[5] = uint32(expected.pid)
	// #nosec G115 -- process IDs and validated positive PID versions fit the audit-token fields.
	token.Value[7] = uint32(unique.PIDVersion)
	return &darwinProcessSignalHandle{token: token}, nil
}

func (handle *darwinProcessSignalHandle) Signal(signal syscall.Signal) error {
	const procInfoCallSignalAuditToken = 0x11
	_, _, errno := syscall.Syscall6(
		syscall.SYS_PROC_INFO,
		procInfoCallSignalAuditToken,
		0,
		uintptr(signal),
		0,
		uintptr(unsafe.Pointer(&handle.token)),
		unsafe.Sizeof(handle.token),
	)
	runtime.KeepAlive(handle)
	if errno != 0 {
		return errno
	}
	return nil
}

func (handle *darwinProcessSignalHandle) Close() error {
	return nil
}

type darwinAuditToken struct {
	Value [8]uint32
}

type darwinProcessUniqueInfo struct {
	ExecutableUUID           [16]byte
	UniqueID                 uint64
	ParentUniqueID           uint64
	PIDVersion               int32
	OriginalParentPIDVersion int32
	Reserved                 [2]uint64
}

func readDarwinProcessUniqueInfo(pid int) (darwinProcessUniqueInfo, error) {
	const (
		procInfoCallPIDInfo             = 0x2
		procPIDUniqueIdentifierInfo     = 17
		procPIDUniqueIdentifierInfoSize = 56
	)
	var info darwinProcessUniqueInfo
	written, _, errno := syscall.Syscall6(
		syscall.SYS_PROC_INFO,
		procInfoCallPIDInfo,
		uintptr(pid),
		procPIDUniqueIdentifierInfo,
		0,
		uintptr(unsafe.Pointer(&info)),
		unsafe.Sizeof(info),
	)
	runtime.KeepAlive(&info)
	if errno != 0 {
		if errors.Is(errno, syscall.ESRCH) {
			return darwinProcessUniqueInfo{}, errProcessNotFound
		}
		return darwinProcessUniqueInfo{}, errno
	}
	if written != procPIDUniqueIdentifierInfoSize || info.PIDVersion <= 0 {
		return darwinProcessUniqueInfo{}, errors.New("process unique identity is incomplete")
	}
	return info, nil
}
