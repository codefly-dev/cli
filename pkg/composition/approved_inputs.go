package composition

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	core "github.com/codefly-dev/core/composition"
)

// ApprovedInputs owns private byte copies, not deployment permission. Callers
// must close it and must still fence/verify the actual target before effects.
// It is process-local; there is no resume or automatic retry after a crash.
type ApprovedInputs struct {
	mu        sync.Mutex
	session   *SelectionSession
	parent    *os.Root
	directory *os.Root
	name      string
	files     DeploymentFiles
	approval  DeploymentApproval
	authority string
	closed    bool
}

// PrepareApprovedInputs checks actual source files, copies only Core-admitted
// runtime bytes and declared render outputs, then re-admits the copies. No
// source paths, receipts or qualification statements become trusted by copying.
func (session *SelectionSession) PrepareApprovedInputs(ctx context.Context, files *DeploymentFiles, approval *DeploymentApproval, expectedAuthority string, now time.Time) (*ApprovedInputs, error) {
	started := time.Now()
	if files == nil || approval == nil || expectedAuthority == "" || now.IsZero() {
		return nil, errors.New("approved input preparation requires files, approval, reviewed authority and current time")
	}
	var prepared *ApprovedInputs
	err := session.withResolved(ctx, func(snapshot *selectionSnapshot, resolved *core.ResolvedComposition) (resultErr error) {
		authority, err := session.loadApprovalAuthority()
		if err != nil {
			return err
		}
		if authority.digest != expectedAuthority {
			return errors.New("host approval authority differs from the independently reviewed authority digest")
		}
		verified, err := authority.verify(approval, now.Add(time.Since(started)))
		if err != nil {
			return err
		}
		checked, err := session.recheckResolved(ctx, resolved, files, authority.config.Policy, &approval.Admission, verified.AdmissionIdentity, now.Add(time.Since(started)))
		if err != nil {
			return err
		}
		prepared, err = session.newApprovedInputs(files, approval, authority.digest)
		if err != nil {
			return err
		}
		defer func() {
			if resultErr != nil {
				resultErr = errors.Join(resultErr, prepared.Close())
			}
		}()
		if err = prepared.copyInputs(ctx, &checked.Current.Record); err != nil {
			return err
		}
		if _, err = session.recheckResolved(ctx, resolved, &prepared.files, authority.config.Policy, &prepared.approval.Admission, verified.AdmissionIdentity, now.Add(time.Since(started))); err != nil {
			return err
		}
		if err = errors.Join(ctx.Err(), session.unchanged(snapshot), authority.unchanged()); err != nil {
			return err
		}
		_, err = authority.verify(&prepared.approval, now.Add(time.Since(started)))
		return err
	})
	if err != nil {
		return nil, err
	}
	return prepared, nil
}

func (session *SelectionSession) newApprovedInputs(files *DeploymentFiles, approval *DeploymentApproval, authority string) (*ApprovedInputs, error) {
	// Clone caller-owned maps, receipts and signatures before retaining them.
	data, err := json.Marshal(struct {
		Files    *DeploymentFiles
		Approval *DeploymentApproval
	}{files, approval})
	if err != nil {
		return nil, err
	}
	var cloned struct {
		Files    DeploymentFiles
		Approval DeploymentApproval
	}
	if err = json.Unmarshal(data, &cloned); err != nil {
		return nil, err
	}
	path, _, err := session.approvalStorageDirectory("composition-approved-inputs")
	if err != nil {
		return nil, err
	}
	parent, err := openAuthorityRegistry(filepath.Join(path, "unused"), true)
	if err != nil {
		return nil, err
	}
	name := "inputs-" + rand.Text()
	if err = parent.Mkdir(name, 0o700); err != nil {
		return nil, errors.Join(err, parent.Close())
	}
	directory, err := openAuthorityChild(parent, name, false)
	if err != nil {
		return nil, errors.Join(err, parent.RemoveAll(name), parent.Close())
	}
	if err = validatePrivateInputDirectory(directory); err != nil {
		return nil, errors.Join(err, directory.Close(), parent.RemoveAll(name), parent.Close())
	}
	return &ApprovedInputs{session: session, parent: parent, directory: directory, name: name,
		files: cloned.Files, approval: cloned.Approval, authority: authority}, nil
}

func validatePrivateInputDirectory(directory *os.Root) error {
	file, err := directory.Open(".")
	if err != nil {
		return err
	}
	info, statErr := file.Stat()
	if statErr == nil && info.Mode().Perm()&0o077 != 0 {
		statErr = errors.New("approved input directory must be private to its owner")
	}
	return errors.Join(statErr, validatePrivateInputAccess(file), file.Close())
}

func (prepared *ApprovedInputs) copyInputs(ctx context.Context, record *core.DeploymentRecord) error {
	runtimeDigests := make(map[[2]string]string, len(record.Artifacts))
	for _, artifact := range record.Artifacts {
		runtimeDigests[[2]string{artifact.Target, artifact.Name}] = artifact.Digest
	}
	// Instances may refer to the same runtime bytes. Deduplicate only by Core's
	// authenticated digest, keeping every logical instance/name in the inputs.
	copied := make(map[string]string)
	for i := range prepared.files.Runtime {
		input := &prepared.files.Runtime[i]
		digest := runtimeDigests[[2]string{input.Target, input.Name}]
		if digest == "" {
			return errors.New("runtime input is absent from admitted artifact identities")
		}
		name := copied[digest]
		if name == "" {
			name = fmt.Sprintf("runtime/%04d", i)
			source, err := os.Open(input.Path)
			if err != nil {
				return err
			}
			if err = copyApprovedInput(ctx, source, prepared.directory, name); err != nil {
				return err
			}
			copied[digest] = name
		}
		input.Path = filepath.Join(prepared.directory.Name(), name)
	}
	outputs := make(map[[2]string]core.ArtifactExecutionRecord, len(record.Executions))
	for _, execution := range record.Executions {
		outputs[[2]string{execution.Target, execution.Service}] = execution
	}
	for i := range prepared.files.Executions {
		input := &prepared.files.Executions[i]
		execution, ok := outputs[[2]string{input.Target, input.Service}]
		if !ok {
			return errors.New("render input is absent from admitted execution identities")
		}
		source, err := os.OpenRoot(input.Directory)
		if err != nil {
			return err
		}
		name := fmt.Sprintf("render/%04d", i)
		err = copyApprovedOutputs(ctx, source, prepared.directory, name, &execution)
		if err = errors.Join(err, source.Close()); err != nil {
			return err
		}
		input.Directory = filepath.Join(prepared.directory.Name(), name)
	}
	return ctx.Err()
}

func copyApprovedOutputs(ctx context.Context, source, destination *os.Root, prefix string, execution *core.ArtifactExecutionRecord) error {
	if err := destination.MkdirAll(prefix, 0o700); err != nil {
		return err
	}
	copied := make(map[string]bool)
	for _, output := range execution.Outputs {
		if copied[output.Path] {
			continue
		}
		file, err := source.Open(output.Path)
		if err != nil {
			return err
		}
		if err = copyApprovedInput(ctx, file, destination, filepath.Join(prefix, output.Path)); err != nil {
			return err
		}
		copied[output.Path] = true
	}
	return nil
}

// copyApprovedInput owns and closes the source handle, including on failures.
func copyApprovedInput(ctx context.Context, source *os.File, directory *os.Root, name string) (resultErr error) {
	defer func() { resultErr = errors.Join(resultErr, source.Close()) }()
	info, err := source.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > maxSelectionArtifactBytes {
		return errors.New("approved input must be a bounded regular file")
	}
	if err = directory.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		return err
	}
	file, err := directory.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	n, err := io.Copy(file, &approvedInputReader{ctx: ctx, reader: io.LimitReader(source, maxSelectionArtifactBytes+1)})
	if err != nil {
		return err
	}
	if n > maxSelectionArtifactBytes {
		return errors.New("approved input exceeds artifact size limit")
	}
	return ctx.Err()
}

type approvedInputReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader *approvedInputReader) Read(data []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(data)
}

// OpenRuntime returns a read-only handle into the private copy. Reading is not
// effect authorization. The caller owns the handle and must close it.
func (prepared *ApprovedInputs) OpenRuntime(target, name string) (*os.File, error) {
	prepared.mu.Lock()
	defer prepared.mu.Unlock()
	if prepared.closed {
		return nil, os.ErrClosed
	}
	for _, input := range prepared.files.Runtime {
		if input.Target == target && input.Name == name {
			return prepared.open(input.Path)
		}
	}
	return nil, errors.New("runtime artifact is not part of approved inputs")
}

// OpenRenderOutput addresses a Core-declared named output, never a caller path.
func (prepared *ApprovedInputs) OpenRenderOutput(target, service, name string) (*os.File, error) {
	prepared.mu.Lock()
	defer prepared.mu.Unlock()
	if prepared.closed {
		return nil, os.ErrClosed
	}
	for _, execution := range prepared.approval.Admission.Record.Executions {
		if execution.Target != target || execution.Service != service {
			continue
		}
		for _, output := range execution.Outputs {
			if output.Name != name {
				continue
			}
			for _, input := range prepared.files.Executions {
				if input.Target == target && input.Service == service {
					return prepared.open(filepath.Join(input.Directory, output.Path))
				}
			}
		}
	}
	return nil, errors.New("render output is not part of approved inputs")
}

func (prepared *ApprovedInputs) open(path string) (*os.File, error) {
	name, err := filepath.Rel(prepared.directory.Name(), path)
	if err != nil {
		return nil, err
	}
	return prepared.directory.Open(name)
}

// Reserve re-admits the private copies and consumes the approval. No effect
// guard is relaxed and neither the record nor an open file grants rollout rights.
func (prepared *ApprovedInputs) Reserve(ctx context.Context, now time.Time) (*ApprovalUse, error) {
	prepared.mu.Lock()
	defer prepared.mu.Unlock()
	if prepared.closed {
		return nil, os.ErrClosed
	}
	return prepared.session.ReserveApproval(ctx, &prepared.files, &prepared.approval, prepared.authority, now)
}

// Close removes only this snapshot. Failed cleanup leaves handles for retry and
// never removes/refunds any approval-use record.
func (prepared *ApprovedInputs) Close() error {
	prepared.mu.Lock()
	defer prepared.mu.Unlock()
	if prepared.closed {
		return nil
	}
	if err := prepared.parent.RemoveAll(prepared.name); err != nil {
		return err
	}
	prepared.closed = true
	return errors.Join(prepared.directory.Close(), prepared.parent.Close())
}
