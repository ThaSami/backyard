# Optional Entra ID (Azure AD) app registration for Backyard's built-in OIDC
# login (auth_mode "oidc" without oidc_issuer). Backyard uses the
# authorization-code flow with PKCE against
# https://login.microsoftonline.com/<tenant>/v2.0 and reads the email, name
# and groups claims from the ID token.

locals {
  entra_display_name = var.entra_application_display_name != null ? var.entra_application_display_name : "${var.display_name} (${local.base_domain})"
  entra_groups_claim = !contains(var.entra_group_membership_claims, "None")
  graph_scopes       = ["openid", "profile", "email", "offline_access", "User.Read"]
}

data "azuread_client_config" "current" {
  count = local.create_entra ? 1 : 0
}

data "azuread_application_published_app_ids" "well_known" {
  count = local.create_entra ? 1 : 0
}

data "azuread_service_principal" "msgraph" {
  count = local.create_entra ? 1 : 0

  client_id = data.azuread_application_published_app_ids.well_known[0].result["MicrosoftGraph"]
}

resource "azuread_application" "backyard" {
  count = local.create_entra ? 1 : 0

  display_name     = local.entra_display_name
  description      = "Backyard internal sites platform (${local.public_url}). Managed by Terraform."
  sign_in_audience = var.entra_sign_in_audience
  owners           = distinct(concat([data.azuread_client_config.current[0].object_id], var.entra_owners))

  group_membership_claims = var.entra_group_membership_claims

  # Device-code login for the CLI (`backyard login --device`).
  fallback_public_client_enabled = var.entra_enable_device_flow

  web {
    homepage_url  = local.public_url
    redirect_uris = [local.callback_url]

    implicit_grant {
      access_token_issuance_enabled = false
      id_token_issuance_enabled     = false
    }
  }

  optional_claims {
    id_token {
      name = "email"
    }

    dynamic "id_token" {
      for_each = local.entra_groups_claim ? [1] : []
      content {
        name                  = "groups"
        additional_properties = var.entra_group_claim_names ? ["cloud_displayname"] : []
      }
    }
  }

  required_resource_access {
    resource_app_id = data.azuread_application_published_app_ids.well_known[0].result["MicrosoftGraph"]

    dynamic "resource_access" {
      for_each = local.graph_scopes
      content {
        id   = data.azuread_service_principal.msgraph[0].oauth2_permission_scope_ids[resource_access.value]
        type = "Scope"
      }
    }
  }

  lifecycle {
    precondition {
      condition     = !var.entra_group_claim_names || contains(var.entra_group_membership_claims, "ApplicationGroup")
      error_message = "entra_group_claim_names (cloud_displayname) only works for groups assigned to the application: set entra_group_membership_claims = [\"ApplicationGroup\"]."
    }
  }
}

resource "azuread_service_principal" "backyard" {
  count = local.create_entra ? 1 : 0

  client_id                    = azuread_application.backyard[0].client_id
  app_role_assignment_required = var.entra_assignment_required
  owners                       = distinct(concat([data.azuread_client_config.current[0].object_id], var.entra_owners))
  login_url                    = "${local.public_url}/__backyard/auth/login"
  notes                        = "Backyard (${local.public_url}). Managed by Terraform."

  feature_tags {
    enterprise = true
  }
}

resource "azuread_application_password" "backyard" {
  count = local.create_entra ? 1 : 0

  application_id = azuread_application.backyard[0].id
  display_name   = "backyard-${var.name}"
  end_date       = var.entra_client_secret_end_date

  rotate_when_changed = {
    rotation = var.entra_client_secret_rotation
  }
}

# Default-access assignments for users/groups (with entra_assignment_required).
resource "azuread_app_role_assignment" "backyard" {
  for_each = local.create_entra ? toset(var.entra_assigned_principal_object_ids) : toset([])

  app_role_id         = "00000000-0000-0000-0000-000000000000"
  principal_object_id = each.value
  resource_object_id  = azuread_service_principal.backyard[0].object_id
}

resource "azuread_service_principal_delegated_permission_grant" "graph" {
  count = local.create_entra && var.entra_grant_admin_consent ? 1 : 0

  service_principal_object_id          = azuread_service_principal.backyard[0].object_id
  resource_service_principal_object_id = data.azuread_service_principal.msgraph[0].object_id
  claim_values                         = local.graph_scopes
}
