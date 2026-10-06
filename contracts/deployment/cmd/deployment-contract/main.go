// deployment-contract is the single offline executable used by Go and Python
// consumers. It reads one request from stdin; it never fetches input itself.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	deployment "github.com/codefly-dev/cli/contracts/deployment"
)

type response struct {
	Valid     bool                  `json:"valid"`
	Canonical *string               `json:"canonical"`
	Digest    *string               `json:"digest"`
	Rows      []deployment.Row      `json:"rows"`
	Violation *deployment.Violation `json:"violation"`
}

func main() { os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)) }
func run(args []string, in io.Reader, out, diagnostic io.Writer) int {
	if len(args) == 1 && (args[0] == "schema" || args[0] == "context-schema") {
		_, err := out.Write(deployment.SchemaJSON(args[0] == "context-schema"))
		if err != nil {
			return 2
		}
		return 0
	}
	if len(args) != 1 || args[0] != "validate" {
		fmt.Fprintln(diagnostic, "usage: deployment-contract validate | schema | context-schema\nvalidate reads {inventory: object, context: object} from stdin; offline consistency only, not platform authorization")
		return 2
	}
	raw, err := io.ReadAll(io.LimitReader(in, 2*deployment.MaxInputBytes+1))
	if err != nil {
		return failure(out, &deployment.Violation{Rule: "INPUT_IO", Path: "$", Message: "cannot read request"}, 2)
	}
	// The request has its own 16 MiB budget, including base64 retained bytes.
	if _, err = deployment.CanonicalJSON(raw); err != nil {
		return failure(out, err, 1)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fields); err != nil || len(fields) != 2 || fields["inventory"] == nil || fields["context"] == nil {
		return failure(out, &deployment.Violation{Rule: "REQUEST_SHAPE", Path: "$", Message: "exactly inventory and context are required"}, 1)
	}
	result, err := deployment.Validate(fields["inventory"], fields["context"])
	if err != nil {
		return failure(out, err, 1)
	}
	canonical, digest := string(result.Canonical()), result.Digest()
	if err = json.NewEncoder(out).Encode(response{true, &canonical, &digest, result.Rows(), nil}); err != nil {
		return 2
	}
	return 0
}
func failure(out io.Writer, err error, code int) int {
	var v *deployment.Violation
	if !errors.As(err, &v) {
		v = &deployment.Violation{Rule: "INTERNAL", Path: "$", Message: "internal validation error"}
		code = 2
	}
	if json.NewEncoder(out).Encode(response{Violation: v}) != nil {
		return 2
	}
	return code
}
