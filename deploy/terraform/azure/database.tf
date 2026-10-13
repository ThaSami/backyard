# Azure Database for PostgreSQL flexible server: Backyard's document store
# (database.type postgres) and cross-replica realtime fan-out (pubsub.type
# postgres, LISTEN/NOTIFY). Skipped when database_url brings an existing server.

locals {
  create_postgres = !try(nonsensitive(var.database_url != null && var.database_url != ""), var.database_url != null && var.database_url != "")
}

resource "random_password" "postgres" {
  count = local.create_postgres ? 1 : 0

  length  = 40
  special = false # keeps the DSN free of URL-escaping concerns; 40 alphanumerics ~ 238 bits
}

resource "azurerm_postgresql_flexible_server" "this" {
  count = local.create_postgres ? 1 : 0

  name                = local.postgres_server_name
  resource_group_name = local.resource_group_name
  location            = var.location
  version             = var.postgres_version
  sku_name            = var.postgres_sku_name
  zone                = var.postgres_zone

  storage_mb        = var.postgres_storage_mb
  auto_grow_enabled = true

  backup_retention_days        = var.postgres_backup_retention_days
  geo_redundant_backup_enabled = var.postgres_geo_redundant_backup

  administrator_login    = var.postgres_admin_login
  administrator_password = random_password.postgres[0].result

  authentication {
    password_auth_enabled         = true
    active_directory_auth_enabled = false
  }

  public_network_access_enabled = !var.vnet_integration
  delegated_subnet_id           = var.vnet_integration ? local.postgres_subnet_id : null
  private_dns_zone_id           = var.vnet_integration ? local.postgres_private_dns_zone_id : null

  dynamic "high_availability" {
    for_each = var.postgres_high_availability ? [1] : []
    content {
      mode = "ZoneRedundant"
    }
  }

  tags = local.tags

  lifecycle {
    # Azure picks (and may fail over) zones; do not fight it.
    ignore_changes = [
      zone,
      high_availability[0].standby_availability_zone,
    ]
  }

  depends_on = [
    azurerm_resource_group.this,
    azurerm_private_dns_zone_virtual_network_link.postgres,
    azurerm_private_dns_zone_virtual_network_link.postgres_apps,
  ]
}

resource "azurerm_postgresql_flexible_server_database" "backyard" {
  count = local.create_postgres ? 1 : 0

  name      = var.postgres_database_name
  server_id = azurerm_postgresql_flexible_server.this[0].id
  charset   = "UTF8"
  collation = "en_US.utf8"
}

# Public mode: 0.0.0.0-0.0.0.0 is Azure's special "allow Azure services" rule.
# It admits every IP Azure allocates, other customers' subscriptions included
# (Container Apps on the Consumption plan has no stable outbound IP, so a
# narrower rule cannot be written); hence the check below.
resource "azurerm_postgresql_flexible_server_firewall_rule" "azure_services" {
  count = local.create_postgres && !var.vnet_integration && var.postgres_allow_azure_services ? 1 : 0

  name             = "AllowAllAzureServicesAndResourcesWithinAzureIps"
  server_id        = azurerm_postgresql_flexible_server.this[0].id
  start_ip_address = "0.0.0.0"
  end_ip_address   = "0.0.0.0"
}

resource "azurerm_postgresql_flexible_server_firewall_rule" "extra" {
  for_each = local.create_postgres && !var.vnet_integration ? var.postgres_allowed_ip_ranges : {}

  name             = each.key
  server_id        = azurerm_postgresql_flexible_server.this[0].id
  start_ip_address = each.value.start
  end_ip_address   = each.value.end
}

# Warning (not an error): with the defaults the admin login is reachable from
# anywhere in Azure, guarded only by TLS and the generated password, so a
# leaked DATABASE_URL or Terraform state is usable from any tenant's VM.
check "postgres_open_to_all_of_azure" {
  assert {
    condition     = !(local.create_postgres && !var.vnet_integration && var.postgres_allow_azure_services)
    error_message = "The Postgres server has a public endpoint with the AllowAllAzureServicesAndResourcesWithinAzureIps firewall rule (postgres_allow_azure_services = true), which admits connections from any Azure IP, including other customers' subscriptions; only TLS and the generated password protect it. For production set vnet_integration = true (private access only), or bring your own server with database_url."
  }
}
