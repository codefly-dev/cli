package composition

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Walk from the filesystem root using handles, checking both each entry and the
// opened directory. A trusted sticky ancestor protects trusted-owned children;
// a non-sticky writable ancestor permits their replacement regardless of mode.
func openProtectedAuthorityDirectory(path string) (*os.Root, error) {
	directory, _, err := openAuthorityDirectory(path, -1)
	return directory, err
}

func canonicalAuthorityHome(path string) (string, error) {
	directory, canonical, err := openAuthorityDirectory(path, 40)
	if err != nil {
		return "", err
	}
	return canonical, directory.Close()
}

func openAuthorityDirectory(path string, links int) (*os.Root, string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, "", errors.New("approval authority directory must be absolute and canonical")
	}
	directory, err := os.OpenRoot(string(filepath.Separator))
	if err != nil {
		return nil, "", err
	}
	info, err := directory.Stat(".")
	if err == nil {
		err = validateAuthorityDirectory(info, path != string(filepath.Separator))
	}
	if err == nil {
		err = validateAuthorityDirectoryAccess(directory)
	}
	if err != nil {
		_ = directory.Close()
		return nil, "", err
	}
	parts := strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator))
	prefix := string(filepath.Separator)
	for index, part := range parts {
		if part == "" {
			continue
		}
		if links >= 0 {
			target, linkErr := authorityLinkTarget(directory, part, prefix)
			if linkErr != nil || target != "" {
				_ = directory.Close()
				if linkErr != nil {
					return nil, "", linkErr
				}
				if links == 0 {
					return nil, "", errors.New("too many approval authority home symlinks")
				}
				return openAuthorityDirectory(filepath.Join(append([]string{target}, parts[index+1:]...)...), links-1)
			}
		}
		next, openErr := openAuthorityChild(directory, part, index < len(parts)-1)
		_ = directory.Close()
		if openErr != nil {
			return nil, "", fmt.Errorf("approval authority ancestry %q: %w", part, openErr)
		}
		directory = next
		prefix = filepath.Join(prefix, part)
	}
	return directory, path, nil
}

func authorityLinkTarget(parent *os.Root, name, prefix string) (string, error) {
	info, err := parent.Lstat(name)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return "", nil
	}
	if err = trustedAuthorityOwner(info); err != nil {
		return "", err
	}
	target, err := parent.Readlink(name)
	if err != nil {
		return "", err
	}
	// Do not lexically erase traversal through another symlink (x/../y).
	// Operators can select the canonical home directly for such aliases.
	if filepath.Clean(target) != target || slices.Contains(strings.Split(target, string(filepath.Separator)), "..") {
		return "", errors.New("approval authority home symlink target must be canonical without parent traversal")
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(prefix, target)
	}
	return target, nil
}

func validateAuthorityDirectory(info os.FileInfo, ancestor bool) error {
	if !info.IsDir() {
		return errors.New("approval authority storage must be a directory, not a symlink")
	}
	if err := trustedAuthorityOwner(info); err != nil {
		return err
	}
	if info.Mode().Perm()&0o022 != 0 && (!ancestor || info.Mode()&os.ModeSticky == 0) {
		return errors.New("approval authority directory must not be group/world writable unless it is a trusted sticky ancestor")
	}
	return nil
}

func openAuthorityChild(parent *os.Root, name string, ancestor bool) (*os.Root, error) {
	before, err := parent.Lstat(name)
	if err != nil {
		return nil, err
	}
	if err = validateAuthorityDirectory(before, ancestor); err != nil {
		return nil, err
	}
	child, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	after, err := child.Stat(".")
	if err == nil {
		err = validateAuthorityDirectory(after, ancestor)
		if err == nil && !os.SameFile(before, after) {
			err = errors.New("approval authority directory changed while opening")
		}
	}
	if err == nil {
		err = validateAuthorityDirectoryAccess(child)
	}
	if err != nil {
		_ = child.Close()
		return nil, err
	}
	return child, nil
}

func validateAuthorityDirectoryAccess(directory *os.Root) error {
	file, err := directory.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	return validateAuthorityAccess(file)
}

func openAuthorityRegistry(path string, create bool) (*os.Root, error) {
	home, err := openProtectedAuthorityDirectory(filepath.Dir(filepath.Dir(path)))
	if err != nil {
		return nil, err
	}
	defer func() { _ = home.Close() }()
	name := filepath.Base(filepath.Dir(path))
	if create {
		if err = home.Mkdir(name, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		// A concurrent creator may not yet have synced the registry entry.
		if err = syncAuthorityDirectory(home); err != nil {
			return nil, err
		}
	}
	return openAuthorityChild(home, name, false)
}

func validateAuthorityFile(info os.FileInfo) error {
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 || info.Size() > 1<<20 {
		return errors.New("approval authority must be a bounded regular file, not group/world writable")
	}
	return trustedAuthorityOwner(info)
}

func readAuthorityDocumentAt(directory *os.Root, name string) ([]byte, error) {
	before, err := directory.Lstat(name)
	if err != nil {
		return nil, err
	}
	if err = validateAuthorityFile(before); err != nil {
		return nil, err
	}
	file, err := directory.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	after, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if err = validateAuthorityFile(after); err != nil {
		return nil, err
	}
	if !os.SameFile(before, after) {
		return nil, errors.New("approval authority changed while opening")
	}
	if err = validateAuthorityAccess(file); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 1<<20 {
		return nil, errors.New("approval authority exceeds size limit")
	}
	return data, nil
}

func writeAuthorityDocumentAt(ctx context.Context, directory *os.Root, name string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	temporary := ".authority-" + rand.Text()
	file, err := directory.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = directory.Remove(temporary) }()
	if err = validateAuthorityAccess(file); err != nil {
		return errors.Join(err, file.Close())
	}
	_, writeErr := file.Write(data)
	if err = errors.Join(writeErr, file.Sync(), file.Close()); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = directory.Rename(temporary, name); err != nil {
		return err
	}
	return syncAuthorityDirectory(directory)
}

func syncAuthorityDirectory(directory *os.Root) error {
	parent, err := directory.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(parent.Sync(), parent.Close())
}
