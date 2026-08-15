//go:build linux

package processgroup

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

var errProcessNotFound = errors.New("process not found")

func inspectNativeProcess(pid int) (nativeProcessIdentity, error) {
	identity, err := readLinuxProcessStat(pid)
	if err != nil {
		return nativeProcessIdentity{}, err
	}
	executable, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
	if err != nil {
		if os.IsNotExist(err) {
			return nativeProcessIdentity{}, errProcessNotFound
		}
		return nativeProcessIdentity{}, err
	}
	bootID, err := linuxBootID()
	if err != nil {
		return nativeProcessIdentity{}, err
	}
	identity.bootID = bootID
	identity.executable = executable
	return identity, nil
}

func readLinuxProcessStat(pid int) (nativeProcessIdentity, error) {
	data, err := fs.ReadFile(os.DirFS("/proc"), strconv.Itoa(pid)+"/stat")
	if err != nil {
		if os.IsNotExist(err) {
			return nativeProcessIdentity{}, errProcessNotFound
		}
		return nativeProcessIdentity{}, err
	}
	closing := strings.LastIndex(string(data), ") ")
	if closing < 0 {
		return nativeProcessIdentity{}, errors.New("invalid process stat")
	}
	fields := strings.Fields(string(data[closing+2:]))
	if len(fields) < 20 {
		return nativeProcessIdentity{}, errors.New("incomplete process stat")
	}
	parent, err := strconv.Atoi(fields[1])
	if err != nil {
		return nativeProcessIdentity{}, fmt.Errorf("parse process parent: %w", err)
	}
	pgid, err := strconv.Atoi(fields[2])
	if err != nil {
		return nativeProcessIdentity{}, fmt.Errorf("parse process group: %w", err)
	}
	startID, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return nativeProcessIdentity{}, fmt.Errorf("parse process start identity: %w", err)
	}
	if startID == 0 {
		return nativeProcessIdentity{}, errors.New("process start identity is zero")
	}
	return nativeProcessIdentity{pid: pid, pgid: pgid, parent: parent, startID: startID}, nil
}

func linuxBootID() (string, error) {
	data, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	bootID := strings.TrimSpace(string(data))
	if bootID == "" {
		return "", errors.New("boot identity is empty")
	}
	return bootID, nil
}

func readProcessGroupAuthentication(pid int) (string, error) {
	file, err := os.Open(filepath.Join("/proc", strconv.Itoa(pid), "environ"))
	if err != nil {
		if os.IsNotExist(err) {
			return "", errProcessNotFound
		}
		return "", err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 4<<20))
	if err != nil {
		return "", err
	}
	prefix := []byte(groupAuthEnv + "=")
	for entry := range bytes.SplitSeq(data, []byte{0}) {
		if value, ok := bytes.CutPrefix(entry, prefix); ok {
			return string(value), nil
		}
	}
	return "", nil
}

type linuxProcessSignalHandle struct {
	fd int
}

func openProcessSignalHandle(expected *processIdentity) (processSignalHandle, error) {
	fd, err := unix.PidfdOpen(expected.pid, 0)
	if errors.Is(err, syscall.ESRCH) {
		return nil, errProcessNotFound
	}
	if err != nil {
		return nil, err
	}
	current, err := inspectProcessIdentity(expected.pid)
	if err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	if !current.same(expected) {
		_ = unix.Close(fd)
		return nil, errProcessGroupIdentityChanged
	}
	return &linuxProcessSignalHandle{fd: fd}, nil
}

func (handle *linuxProcessSignalHandle) Signal(signal syscall.Signal) error {
	return unix.PidfdSendSignal(handle.fd, unix.Signal(signal), nil, 0)
}

func (handle *linuxProcessSignalHandle) Close() error {
	return unix.Close(handle.fd)
}
