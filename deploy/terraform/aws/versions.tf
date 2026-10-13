terraform {
  required_version = ">= 1.6.0"

  required_providers {
    aws = {
      source = "hashicorp/aws"
      # 6.28 added aws_lambda_permission.invoked_via_function_url, needed for
      # public function URLs created since October 2025.
      version = ">= 6.28, < 7.0"
    }
    random = {
      source  = "hashicorp/random"
      version = ">= 3.6, < 4.0"
    }
    http = {
      source  = "hashicorp/http"
      version = ">= 3.4, < 4.0"
    }
  }
}
