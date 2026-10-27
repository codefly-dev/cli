// Print what a job must compile to prove one conformance row: the row's build
// tag on the first line (empty when it has none), then one package per line.
// The matrix and the tagged sources are the only inputs, so a qualification
// script never keeps its own package list or tag.
package main

import (
	"fmt"
	"os"

	"github.com/codefly-dev/cli/pkg/conformance"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: conformance-packages <row>")
		os.Exit(2)
	}
	row, ok := conformance.Default().Row(os.Args[1])
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown conformance row %q\n", os.Args[1])
		os.Exit(1)
	}
	root, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	packages, err := row.Packages(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(row.BuildTag)
	for _, pkg := range packages {
		fmt.Println(pkg)
	}
}
