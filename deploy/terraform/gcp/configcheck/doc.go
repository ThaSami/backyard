// Package configcheck holds an integration test for the Google Cloud
// Terraform module in deploy/terraform/gcp: it renders backyard.yaml for the
// scenarios in testdata/render.tftest.hcl and for every run of the module's
// own tests/module.tftest.hcl (terraform test with mocked Google providers),
// and loads each result with Backyard's own loader in internal/config, the
// same way the server does at startup (strict YAML, ${ENV} expansion,
// environment overrides, defaults and validation). The testdata scenarios
// also check the loaded values; the module's runs are loaded with the
// environment their Cloud Run container gets (Secret Manager values replaced
// by placeholders).
//
// The test needs a Terraform binary and the module's providers, so it only
// runs when BACKYARD_TEST_TERRAFORM is set ("1" for terraform on PATH, or the
// path of a terraform binary); CI sets it:
//
//	BACKYARD_TEST_TERRAFORM=1 go test ./deploy/terraform/gcp/configcheck/
//
// Provider downloads honour TF_PLUGIN_CACHE_DIR; without it the test reuses
// the module's own .terraform directory when `terraform init` was run there.
package configcheck
