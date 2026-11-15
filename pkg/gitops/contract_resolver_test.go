package gitops

import "github.com/codefly-dev/core/solutionhost"

// uncomposed resolves a contract with no composition values for class: what a
// test of a contract whose slots need none reads its declaration through.
func uncomposed(class string) ContractResolver { return solutionhost.Resolver(nil, class, nil) }
