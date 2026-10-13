// Package configcheck holds an integration test for the Google Cloud
// Terraform module in deploy/terraform/gcp: it renders backyard.yaml for
// several module configurations (terraform test with mocked Google
// providers) and loads every result with Backyard's own loader in
// internal/config, the same way the server does at startup (strict YAML,
// ${ENV} expansion, environment overrides, defaults and validation).
//
// The test needs a Terraform binary and the module's providers, so it only
// runs when BACKYARD_TEST_TERRAFORM is set ("1" for terraform on PATH, or the
// path of a terraform binary):
//
//	BACKYARD_TEST_TERRAFORM=1 go test ./deploy/terraform/gcp/configcheck/
//
// Provider downloads honour TF_PLUGIN_CACHE_DIR; without it the test reuses
// the module's own .terraform directory when `terraform init` was run there.
package configcheck
