// Package charttest holds tests for the Helm chart in deploy/helm/backyard.
// They render the chart with `helm template` and check the guards in
// templates/_helpers.tpl (for example that auth.mode header cannot trust
// identity headers from the whole pod network) and the manifests the values
// produce, such as the NetworkPolicy.
//
// The tests need helm on PATH and are skipped without it, unless
// BACKYARD_TEST_HELM is set (CI does), which turns a missing helm into a
// failure:
//
//	go test ./deploy/helm/...
package charttest
