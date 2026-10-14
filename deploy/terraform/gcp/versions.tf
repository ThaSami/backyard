terraform {
  # optional() object attributes with defaults need >= 1.3; 1.6 is the oldest
  # release this module is tested against.
  required_version = ">= 1.6.0"

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 8.0"
    }
    # Used only for google_project_service_identity (the IAP service agent),
    # which is not available in the GA provider.
    google-beta = {
      source  = "hashicorp/google-beta"
      version = "~> 8.0"
    }
    random = {
      source  = "hashicorp/random"
      version = ">= 3.6, < 4.0"
    }
  }
}
