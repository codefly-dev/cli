package control

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	codecore "github.com/codefly-dev/core/code"
)

// This file lifts the SourceEditor group. Per the "CLI does generic ops
// directly, plugins only for special behavior" principle, file CRUD / list /
// search / plain literal edits run DIRECTLY on the local filesystem — the
// control plane is co-located with the workspace on the execution machine.
// They reuse core/code (the same FileOperation abstraction the Gateway uses),
// which resolves every path under the workspace root with symlink-safe escape
// guards. Fix (language-aware repair) is NOT here — it is plugin behavior and is
// lifted with the Code-plugin group.

// fileOps builds a workspace-rooted FileOperation (fresh per call, like the
// Gateway) and returns the resolved workspace root for callers that also need
// an absolute path (e.g. stat).
func (p *planeImpl) fileOps(ctx context.Context) (codecore.FileOperation, string, error) {
	ws, err := p.workspace(ctx)
	if err != nil {
		return nil, "", err
	}
	root := ws.Dir()
	return codecore.NewFileOps(codecore.LocalVFS{}, root), root, nil
}

func (p *planeImpl) ReadFile(ctx context.Context, path string) ([]byte, error) {
	ops, _, err := p.fileOps(ctx)
	if err != nil {
		return nil, err
	}
	return ops.ReadFile(ctx, path)
}

func (p *planeImpl) WriteFile(ctx context.Context, path string, content []byte) error {
	ops, _, err := p.fileOps(ctx)
	if err != nil {
		return err
	}
	return ops.WriteFile(ctx, path, content)
}

// CreateFile refuses to overwrite an existing file (probe-then-write, matching
// the Gateway's CreateFile guard).
func (p *planeImpl) CreateFile(ctx context.Context, path string, content []byte) error {
	ops, _, err := p.fileOps(ctx)
	if err != nil {
		return err
	}
	if _, err := ops.ReadFile(ctx, path); err == nil {
		return fmt.Errorf("file already exists: %s", path)
	} else if !os.IsNotExist(err) {
		return err
	}
	return ops.WriteFile(ctx, path, content)
}

func (p *planeImpl) DeleteFile(ctx context.Context, path string) error {
	ops, _, err := p.fileOps(ctx)
	if err != nil {
		return err
	}
	return ops.DeleteFile(ctx, path)
}

func (p *planeImpl) MoveFile(ctx context.Context, from, to string) error {
	ops, _, err := p.fileOps(ctx)
	if err != nil {
		return err
	}
	return ops.MoveFile(ctx, from, to)
}

// ListFiles lists the immediate entries under dir (non-recursive). It stats each
// entry best-effort to fill IsDir/Size; a stat failure just leaves them zero.
func (p *planeImpl) ListFiles(ctx context.Context, dir string) ([]FileInfo, error) {
	ops, root, err := p.fileOps(ctx)
	if err != nil {
		return nil, err
	}
	names, err := ops.ListFiles(ctx, dir, false, nil)
	if err != nil {
		return nil, err
	}
	infos := make([]FileInfo, 0, len(names))
	for _, name := range names {
		info := FileInfo{Path: name}
		if st, err := os.Stat(filepath.Join(root, name)); err == nil {
			info.IsDir = st.IsDir()
			info.Size = st.Size()
		}
		infos = append(infos, info)
	}
	return infos, nil
}

// Search runs core/code's VFS regex/literal search (the same walker the Gateway
// uses — not ripgrep). req.Regex=false means a literal pattern.
func (p *planeImpl) Search(ctx context.Context, req SearchRequest) ([]SearchHit, error) {
	ops, _, err := p.fileOps(ctx)
	if err != nil {
		return nil, err
	}
	result, err := ops.Search(ctx, codecore.SearchOpts{
		Pattern: req.Query,
		Literal: !req.Regex,
		Path:    req.Path,
	})
	if err != nil {
		return nil, err
	}
	hits := make([]SearchHit, 0, len(result.Matches))
	for _, m := range result.Matches {
		hits = append(hits, SearchHit{Path: m.File, Line: m.Line, Text: m.Text})
	}
	return hits, nil
}

// ApplyEdit applies one plain literal edit on disk. Whole replaces the file with
// NewText; otherwise OldText → NewText (all occurrences). A non-whole edit whose
// OldText is absent is an error, so a no-op edit never passes silently. This is
// the generic, plugin-free edit path; language-aware edits are a plugin concern.
func (p *planeImpl) ApplyEdit(ctx context.Context, edit Edit) error {
	ops, _, err := p.fileOps(ctx)
	if err != nil {
		return err
	}
	return applyEditWith(ctx, ops, edit)
}

func applyEditWith(ctx context.Context, ops codecore.FileOperation, edit Edit) error {
	if edit.Whole {
		return ops.WriteFile(ctx, edit.Path, []byte(edit.NewText))
	}
	changed, err := ops.ReplaceInFile(ctx, edit.Path, edit.OldText, edit.NewText)
	if err != nil {
		return err
	}
	if !changed {
		return fmt.Errorf("edit did not match in %s: old text not found", edit.Path)
	}
	return nil
}

// BatchApplyEdits applies edits transactionally: it snapshots every touched
// file first, applies in order, and on any failure restores all snapshots so a
// partial batch never lands.
func (p *planeImpl) BatchApplyEdits(ctx context.Context, edits []Edit) error {
	ops, _, err := p.fileOps(ctx)
	if err != nil {
		return err
	}
	originals := make(map[string][]byte, len(edits))
	rollback := func() {
		for path, data := range originals {
			_ = ops.WriteFile(ctx, path, data)
		}
	}
	for _, edit := range edits {
		if _, seen := originals[edit.Path]; !seen {
			data, err := ops.ReadFile(ctx, edit.Path)
			if err != nil {
				rollback()
				return fmt.Errorf("snapshot %s: %w", edit.Path, err)
			}
			originals[edit.Path] = data
		}
		if err := applyEditWith(ctx, ops, edit); err != nil {
			rollback()
			return err
		}
	}
	return nil
}
