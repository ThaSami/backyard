// Package configcheck holds an integration test for the AWS Terraform module
// in deploy/terraform/aws: it runs every scenario of the module's
// tests/module.tftest.hcl (terraform test with mocked providers) and loads
// the backyard.yaml each one renders, for the ECS service and for the Lambda
// deploy gateway, with Backyard's own loader in internal/config, in the
// environment the container or function would get (Secrets Manager values
// replaced by placeholders). A rendered key the server no longer knows, or a
// configuration its validation refuses, fails here instead of as an ECS task
// that never starts.
//
// The test needs a Terraform binary and the module's providers, so it only
// runs when BACKYARD_TEST_TERRAFORM is set ("1" for terraform on PATH, or the
// path of a terraform binary); CI sets it:
//
//	BACKYARD_TEST_TERRAFORM=1 go test ./deploy/terraform/aws/configcheck/
//
// Provider downloads honour TF_PLUGIN_CACHE_DIR; without it the test reuses
// the module's own .terraform directory when `terraform init` was run there.
package configcheck
