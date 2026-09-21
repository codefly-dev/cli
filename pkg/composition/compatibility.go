package composition

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"time"

	core "github.com/codefly-dev/core/composition"
	updatev0 "github.com/codefly-dev/core/generated/go/codefly/update/v0"
	"github.com/codefly-dev/core/moduleupdate"
	"google.golang.org/protobuf/encoding/protojson"
)

type CompatibilityInspection struct {
	SelectionIdentity string          `json:"selectionIdentity"`
	Target            string          `json:"target"`
	Result            json.RawMessage `json:"result"`
}

// CheckCompatibility evaluates authenticated snapshots, not version strings.
// A changed contract without source-supported evidence remains UNDETERMINED.
func (session *SelectionSession) CheckCompatibility(ctx context.Context, target, artifact string, usage core.SignedConsumerUsage, authority core.ConsumerUsageAuthority, now time.Time, client *http.Client) (*CompatibilityInspection, error) {
	var inspection *CompatibilityInspection
	err := session.withResolved(ctx, func(snapshot *selectionSnapshot, resolved *core.ResolvedComposition) error {
		var result *updatev0.UpdateResult
		pin, err := core.VerifyConsumerUsage(usage, authority, target, resolved.Identity(), now)
		if err != nil {
			result = unknownCompatibility(err)
		} else {
			result, err = session.evaluateSelection(ctx, snapshot, resolved, target, artifact, pin, client)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				result = unknownCompatibility(err)
			}
		}
		if localErr := resolved.CheckLocalInputs(); localErr != nil {
			return localErr
		}
		if changedErr := session.unchanged(ctx, snapshot); changedErr != nil {
			return changedErr
		}
		data, err := protojson.Marshal(result)
		if err != nil {
			return err
		}
		inspection = &CompatibilityInspection{SelectionIdentity: resolved.Identity(), Target: target, Result: data}
		return nil
	})
	return inspection, err
}

func unknownCompatibility(err error) *updatev0.UpdateResult {
	return &updatev0.UpdateResult{Verdict: updatev0.Verdict_VERDICT_UNDETERMINED,
		Undetermined: []*updatev0.AffectedItem{{Reason: err.Error()}}}
}

func (session *SelectionSession) evaluateSelection(ctx context.Context, snapshot *selectionSnapshot, resolved *core.ResolvedComposition, target, artifact string, pin *updatev0.ConsumerPin, client *http.Client) (*updatev0.UpdateResult, error) {
	if len(snapshot.local) > 0 {
		return nil, errors.New("released contract snapshots do not establish compatibility of local content")
	}
	index := slices.IndexFunc(snapshot.descriptor.Replacements, func(replacement core.Replacement) bool { return replacement.Target == target })
	if index < 0 {
		return nil, errors.New("compatibility target must identify a selected replacement")
	}
	baselineDescriptor := *snapshot.descriptor
	baselineDescriptor.Replacements = slices.Delete(slices.Clone(baselineDescriptor.Replacements), index, index+1)
	baseline, err := session.Engine.ResolveComposition(ctx, &baselineDescriptor, snapshot.inputs.Root, session.options(snapshot))
	if err != nil {
		return nil, err
	}
	before, err := session.contractSnapshot(ctx, baseline, target, artifact, client)
	if err != nil {
		return nil, err
	}
	after, err := session.contractSnapshot(ctx, resolved, target, artifact, client)
	if err != nil {
		return nil, err
	}
	diff, err := moduleupdate.BuildReleaseDiff(before, after)
	if err != nil {
		return nil, err
	}
	return moduleupdate.Evaluate(diff, pin), nil
}

func (session *SelectionSession) contractSnapshot(ctx context.Context, resolved *core.ResolvedComposition, target, name string, client *http.Client) (*updatev0.ContractSnapshot, error) {
	acquisitions := resolved.Record().Acquisitions
	for i := range acquisitions {
		acquisition := &acquisitions[i]
		if acquisition.Target != target || acquisition.Artifact.Name != name || acquisition.Artifact.Purpose != core.ArtifactContracts {
			continue
		}
		path, err := session.acquireArtifact(ctx, client, acquisition.Artifact)
		if err != nil {
			return nil, err
		}
		data, err := readContractArtifact(ctx, path)
		if err != nil {
			return nil, err
		}
		// Recheck after opening: a cache path is not an authentication token.
		if contentDigest(data) != acquisition.Artifact.Digest {
			return nil, core.ErrDigestMismatch
		}
		var snapshot updatev0.ContractSnapshot
		if err := protojson.Unmarshal(data, &snapshot); err != nil {
			return nil, err
		}
		if snapshot.Module != acquisition.Owner.ID || snapshot.Version != acquisition.Owner.Version {
			return nil, errors.New("contract snapshot does not identify the selected owner release")
		}
		return &snapshot, nil
	}
	return nil, fmt.Errorf("%s: contracts artifact %s is not in the selected requirements", target, name)
}

func readContractArtifact(ctx context.Context, path string) ([]byte, error) {
	file, err := openInputFile(ctx, nil, path)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(&approvedInputReader{ctx: ctx, reader: io.LimitReader(file, maxSelectionArtifactBytes+1)})
	if err = errors.Join(readErr, file.Close(), ctx.Err()); err != nil {
		return nil, err
	}
	if int64(len(data)) > maxSelectionArtifactBytes {
		return nil, errors.New("contract snapshot exceeds artifact size limit")
	}
	return data, nil
}
