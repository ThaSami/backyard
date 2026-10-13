// Package configcheck holds an integration test for the Azure Terraform
// module in deploy/terraform/azure: it runs every scenario of the module's
// tests/module.tftest.hcl (terraform test with mocked providers) and loads
// the backyard.yaml each one gives the Container App with Backyard's own
// loader in internal/config, in the environment the container would get
// (Container Apps secrets replaced by placeholders). A rendered key the
// server no longer knows, or a configuration its validation refuses, fails
// here instead of as a revision that never becomes ready.
//
// The test needs a Terraform binary and the module's providers, so it only
// runs when BACKYARD_TEST_TERRAFORM is set ("1" for terraform on PATH, or the
// path of a terraform binary); CI sets it:
//
//	BACKYARD_TEST_TERRAFORM=1 go test ./deploy/terraform/azure/configcheck/
//
// Provider downloads honour TF_PLUGIN_CACHE_DIR; without it the test reuses
// the module's own .terraform directory when `terraform init` was run there.
package configcheck
