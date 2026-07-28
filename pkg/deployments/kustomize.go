package deployments

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"github.com/codefly-dev/core/resources"
)

func KustomizeDir(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service) string {
	return path.Join(workspace.Dir(), "deployments", "modules", module.Name, "services", service.Name)
}

func KustomizeDirForEnv(ctx context.Context, workspace *resources.Workspace, module *resources.Module, service *resources.Service, env *resources.Environment) string {
	return path.Join(KustomizeDir(ctx, workspace, module, service), "overlays", env.Name)
}

func renderKustomize(ctx context.Context, tree, treeDigest, dir string) (string, []string, error) {
	cmd := exec.CommandContext(ctx, "kustomize", "build", dir)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		return "", nil, fmt.Errorf("cannot run kustomize build: %w: %s", err, stderr.String())
	}
	currentDigest, err := RenderedTreeDigest(tree)
	if err != nil {
		return "", nil, fmt.Errorf("cannot verify rendered deployment tree: %w", err)
	}
	if currentDigest != treeDigest {
		return "", nil, fmt.Errorf("rendered deployment tree changed after validation; refusing direct apply")
	}
	manifests := stdout.String()
	return manifests, strings.Split(manifests, "---"), nil
}

func RenderedTreeDigest(root string) (string, error) {
	digest := sha256.New()
	err := filepath.WalkDir(root, func(filePath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, filePath)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		var kind byte
		var content []byte
		switch {
		case entry.Type().IsRegular():
			kind = 1
			content, err = os.ReadFile(filePath)
			if err != nil {
				return err
			}
		case entry.Type()&os.ModeSymlink != 0:
			target, err := os.Readlink(filePath)
			if err != nil {
				return err
			}
			kind = 2
			content = []byte(target)
		default:
			return fmt.Errorf("unsupported file type in rendered deployment tree: %s", filePath)
		}
		if _, err := digest.Write([]byte{kind}); err != nil {
			return err
		}
		if err := binary.Write(digest, binary.BigEndian, uint64(len(relative))); err != nil {
			return err
		}
		if _, err := digest.Write([]byte(relative)); err != nil {
			return err
		}
		if err := binary.Write(digest, binary.BigEndian, uint64(len(content))); err != nil {
			return err
		}
		_, err = digest.Write(content)
		return err
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("sha256:%x", digest.Sum(nil)), nil
}
