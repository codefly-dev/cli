package orchestration

// The external test passes promotion's output through the GitOps render guard.
// GitOps imports orchestration, so that check must live outside this package.
var PromotableDeploymentConfigurationsForTest = promotableDeploymentConfigurations
