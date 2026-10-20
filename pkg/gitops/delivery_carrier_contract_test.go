package gitops

import (
	"testing"

	"github.com/codefly-dev/cli/pkg/environments"
	"github.com/stretchr/testify/require"
)

// THE CARRIER LABEL'S VALUE IS FROZEN, AND THIS TEST IS THE TRIPWIRE.
//
// A cell turns each namespace's delivery selector into one entry of a CLOSED
// execution-admission set, matched on the exact label set. A render regenerates
// the cell file WHOLE — every namespace at once — while a delivery Job moves
// only when its own module is published. So renaming this value flips every
// namespace's selector in one commit and leaves every not-yet-republished
// module's delivered pod matching no approved entry: the pod is refused by
// admission, that module's delivery sync fails, and the refusal names neither
// the label nor the rename.
//
// That is not hypothetical. #1058 renamed it to "carrier"; obin-staging's
// cell.yaml declared the old value for 10 namespaces and 9 modules' delivered
// Jobs carried it on their pod templates, so the next render would have broken
// delivery for every module not republished in the same change. #1077 put it
// back.
//
// If you are here because this test failed: renaming the value is a migration,
// not an edit. Every module in every environment must be re-published in the
// change that pins the new cell file. Kind-neutrality is already expressed by
// deliveredDocumentLabel, which nothing selects on — put the new distinction
// there instead, where no admission rule reads it.
func TestTheDeliveryCarrierLabelValueIsFrozen(t *testing.T) {
	require.Equal(t, "codefly.dev/delivery", deliveryLabel,
		"the carrier label KEY is part of every delivered tree's pod spec")
	require.Equal(t, "manifest", deliveryCarrierValue,
		"the carrier label VALUE is part of every delivered tree's pod spec and every cell file's selector")
}

// The cell admits exactly the pod the render writes. These are built in
// different files — the selector in cell.go, the pod in delivery_jobs.go — so
// a change to one that misses the other produces a cell that refuses its own
// delivery Jobs. Within one render they must be the same label set.
func TestTheCellSelectorIsTheLabelSetTheCarrierPodCarries(t *testing.T) {
	env := &environments.Environment{Name: "prod", Host: testHost()}
	delivery, err := cellDelivery(env, "example-prod-shop")
	require.NoError(t, err)
	require.Equal(t, deliveryCarrierOf().Labels, delivery.Selector,
		"the cell's carrier selector and the delivery pod's labels are one label set")
	require.Equal(t, deliveryServiceAccount, delivery.ServiceAccount,
		"the cell admits the account the delivery Job runs as")
}
