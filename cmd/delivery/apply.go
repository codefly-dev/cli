package delivery

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/codefly-dev/cli/cmd/common"
	"github.com/codefly-dev/cli/pkg/gitops"
	"github.com/codefly-dev/cli/pkg/orchestration"

	compositionv1 "github.com/codefly-dev/authority/libraries/authority-client/go/gen/host/composition/v1"
)

// applyOptions are the flags, named so the Job that runs this reads as the
// thing it does.
var applyOptions struct {
	documents, address, identityFile string
	secure                           bool
	budget, request, maxPause        time.Duration
}

// ApplyCmd sends the prepared paired delivery documents a render wrote to the
// host's composition interface. It is what a cell's delivery Job runs: the
// documents are mounted, the credential is projected, and neither is baked
// into anything.
//
// NOT exposed over MCP, deliberately. Every input it needs exists only inside
// the pod that runs it — a mounted document set, a projected credential and a
// cluster-internal address — so an assistant calling it from outside has
// nothing to pass, and the one thing it could pass is a path to a credential.
// The verbs that produce what this sends (render and publish) are the ones
// worth driving.
var ApplyCmd = &cobra.Command{
	Use: "apply", Short: "Send prepared paired delivery documents to the host's composition interface",
	Long: `Send the prepared delivery documents under --documents to the host's composition
interface (host.composition.v1.CompositionService/ApplyManifest) at --address.

Each document is one ApplyManifestRequest carrying both signed halves — core's
module manifest in its DSSE carrier, and the authority declaration signed
against that carrier's exact bytes: method policies, scope ceilings, roles,
owner-awareness, the installation selector and its person-context edges. Only
this interface takes the pair. The host's legacy HTTP manifest route takes the
Core half alone, so a module delivered that way reaches the host with no roles.

A prepared document carries NO credential: a delivery credential is a
projected ServiceAccount token with a lifetime of minutes, and one written into
a rendered manifest would be a published secret and expired by the time the
manifest synced. The credential is read from --identity-file at execution,
once per request, so a token rotated between rounds is the one the next round
sends. It is sent RAW, in the request's credential field, because the host adds
the bearer scheme itself. It is never printed and never passed as an argument.

Documents are sent in ROUNDS: every pending one once per round, then a pause
that doubles up to --max-pause, until each is answered or --budget is spent, so
a document the host keeps failing never keeps a later one — a withdrawal among
them — from its first attempt. A refusal the host means (an invalid document, a
refused carrier, a stale or rewritten generation, a withdrawal it keeps
terminal) refuses that document and is not retried; only "not serving yet"
(unavailable, out of time, out of capacity) is. The command fails when any
document was not accepted, so a delivery that did not land fails the Job that
ran it.

The address is the host's delivery endpoint. A Connect endpoint is one: a
Connect server answers the gRPC protocol on the same listener (h2c in
plaintext), so --address is <host>:<port> of the endpoint the environment's
host.delivery names, and --secure only when that endpoint serves TLS itself.

A GitOps publish toward a host whose delivery endpoint declares
codefly.dev/composition writes these documents into the delivery overlay, one
per manifest, and renders the manifest's delivery Job to run this command.`,
	Example: `  codefly delivery apply --documents /etc/codefly/delivery --address accounts.platform.svc.cluster.local:8081 --identity-file /var/run/secrets/codefly/delivery-token
  codefly delivery apply --documents ./prepared --address localhost:8081 --identity-file ./token --budget 2m`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx, cancel := common.NewContext()
		defer cancel()
		if applyOptions.documents == "" {
			return errors.New("--documents is the directory holding the prepared delivery documents")
		}
		if applyOptions.address == "" {
			return errors.New("--address is <host>:<port> of the endpoint the environment's host.delivery names")
		}
		if applyOptions.identityFile == "" {
			return errors.New("--identity-file is the projected delivery credential, read at execution")
		}
		connection, err := orchestration.GRPCTarget{Address: applyOptions.address, Secure: applyOptions.secure}.Dial()
		if err != nil {
			return fmt.Errorf("dial the host's composition interface at %s: %w", applyOptions.address, err)
		}
		defer func() { _ = connection.Close() }()
		sender := &gitops.PairedApply{
			Client:     compositionv1.NewCompositionServiceClient(connection),
			Credential: func() (string, error) { return readCredential(applyOptions.identityFile) },
			Budget:     applyOptions.budget, Request: applyOptions.request, MaxPause: applyOptions.maxPause,
			Report: func(line string) { fmt.Fprintln(cmd.OutOrStdout(), line) },
		}
		_, err = sender.Apply(ctx, applyOptions.documents)
		return err
	},
}

// readCredential reads the projected token and returns it as the host takes
// it: raw, with no bearer scheme and no surrounding whitespace, which a
// projection's trailing newline would otherwise carry into the request. The
// value is returned and nothing more — it is not logged, and the error names
// the path rather than the content.
func readCredential(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read the projected delivery credential at %s: %w", path, err)
	}
	return strings.TrimSpace(string(data)), nil
}

func init() {
	ApplyCmd.Flags().StringVar(&applyOptions.documents, "documents", "", "Directory holding the prepared delivery documents (*.json)")
	ApplyCmd.Flags().StringVar(&applyOptions.address, "address", "", "<host>:<port> of the host's delivery endpoint")
	ApplyCmd.Flags().StringVar(&applyOptions.identityFile, "identity-file", "", "Path of the projected delivery credential, read at execution")
	ApplyCmd.Flags().BoolVar(&applyOptions.secure, "secure", false, "Dial with TLS, for a delivery endpoint that serves it itself")
	ApplyCmd.Flags().DurationVar(&applyOptions.budget, "budget", 25*time.Minute, "Bound on the whole run")
	ApplyCmd.Flags().DurationVar(&applyOptions.request, "request-timeout", 30*time.Second, "Bound on one call")
	ApplyCmd.Flags().DurationVar(&applyOptions.maxPause, "max-pause", time.Minute, "Longest pause between rounds")
}
