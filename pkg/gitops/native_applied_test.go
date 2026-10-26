package gitops

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// A withdrawal is complete once the host's applied record of the binding is
// exactly it — binding, generation, digest, release and removed — as the
// composition interface answers it.
func TestWithdrawalCompletionRequiresExactAppliedWithdrawal(t *testing.T) {
	module, services := nativeModule(t, "crm", false)
	live := renderNative(t, module, services, "api")
	live.Generation = 3
	withdrawal, err := WithdrawalOf(live)
	require.NoError(t, err)
	digest, err := withdrawal.Digest()
	require.NoError(t, err)
	for _, tc := range []struct {
		name    string
		missing bool
		refused error
		change  func(*AppliedManifest)
		want    bool
		wantErr bool
	}{
		{name: "exact applied", want: true},
		{name: "no applied state", missing: true},
		{name: "previous generation", change: func(v *AppliedManifest) { v.Generation = withdrawal.Generation - 1; v.Removed = false }},
		{name: "wrong binding", change: func(v *AppliedManifest) { v.Binding = "other" }, wantErr: true},
		{name: "wrong digest", change: func(v *AppliedManifest) { v.Digest = "other" }, wantErr: true},
		{name: "wrong release", change: func(v *AppliedManifest) { v.Release = "other" }, wantErr: true},
		{name: "not removed", change: func(v *AppliedManifest) { v.Removed = false }, wantErr: true},
		{name: "later generation", change: func(v *AppliedManifest) { v.Generation = withdrawal.Generation + 1 }, wantErr: true},
		{name: "observation refused", refused: errors.New("refused"), wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &NativeDeliverer{Applied: func(_ context.Context, binding string) (*AppliedManifest, bool, error) {
				require.Equal(t, withdrawal.Binding, binding)
				if tc.refused != nil || tc.missing {
					return nil, false, tc.refused
				}
				applied := &AppliedManifest{Binding: binding, Generation: withdrawal.Generation, Digest: digest, Release: withdrawal.Release.Identity(), Removed: true}
				if tc.change != nil {
					tc.change(applied)
				}
				return applied, true, nil
			}}
			got, err := d.withdrawalApplied(t.Context(), withdrawal, digest)
			require.Equal(t, tc.want, got)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			if tc.want {
				require.NoError(t, d.WaitForWithdrawal(t.Context(), withdrawal))
			}
		})
	}
	applied := func(context.Context, string) (*AppliedManifest, bool, error) { return nil, false, nil }
	require.Error(t, (&NativeDeliverer{Applied: applied}).WaitForWithdrawal(t.Context(), live), "a live manifest is no withdrawal to wait for")
	require.Error(t, (&NativeDeliverer{}).WaitForWithdrawal(t.Context(), withdrawal), "no applied-state source is no completion")
}
