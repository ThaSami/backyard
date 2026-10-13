# Optional virtual network integration (vnet_integration = true): the
# Container Apps environment gets an infrastructure subnet and the Postgres
# flexible server is reachable only through a delegated subnet + private DNS.
# Existing subnets can be supplied instead of creating a virtual network.

locals {
  create_vnet = var.vnet_integration && var.existing_container_apps_subnet_id == null

  container_apps_subnet_id = (
    !var.vnet_integration ? null
    : local.create_vnet ? azurerm_subnet.apps[0].id
    : var.existing_container_apps_subnet_id
  )
  postgres_subnet_id = (
    !var.vnet_integration || !local.create_postgres ? null
    : local.create_vnet ? azurerm_subnet.postgres[0].id
    : var.existing_postgres_subnet_id
  )
  # Virtual networks holding the subnets (for private DNS links).
  postgres_vnet_id = local.postgres_subnet_id == null ? null : replace(local.postgres_subnet_id, "/(?i)/subnets/[^/]+$/", "")
  # Existing subnets may live in different (peered) virtual networks; the
  # private DNS zone must then be linked to the Container Apps one as well.
  link_apps_vnet = (
    local.create_vnet || var.existing_container_apps_subnet_id == null || var.existing_postgres_subnet_id == null ? false
    : lower(replace(var.existing_container_apps_subnet_id, "/(?i)/subnets/[^/]+$/", "")) != lower(replace(var.existing_postgres_subnet_id, "/(?i)/subnets/[^/]+$/", ""))
  )

  create_postgres_dns_zone = var.vnet_integration && local.create_postgres && var.existing_postgres_private_dns_zone_id == null
  postgres_private_dns_zone_id = (
    !var.vnet_integration || !local.create_postgres ? null
    : local.create_postgres_dns_zone ? azurerm_private_dns_zone.postgres[0].id
    : var.existing_postgres_private_dns_zone_id
  )
}

resource "azurerm_virtual_network" "this" {
  count = local.create_vnet ? 1 : 0

  name                = "${var.name}-vnet"
  location            = var.location
  resource_group_name = local.resource_group_name
  address_space       = var.vnet_address_space
  tags                = local.tags

  depends_on = [azurerm_resource_group.this]
}

resource "azurerm_subnet" "apps" {
  count = local.create_vnet ? 1 : 0

  name                 = "container-apps"
  resource_group_name  = local.resource_group_name
  virtual_network_name = azurerm_virtual_network.this[0].name
  address_prefixes     = [var.container_apps_subnet_cidr]

  delegation {
    name = "container-apps"
    service_delegation {
      name    = "Microsoft.App/environments"
      actions = ["Microsoft.Network/virtualNetworks/subnets/join/action"]
    }
  }
}

resource "azurerm_subnet" "postgres" {
  count = local.create_vnet && local.create_postgres ? 1 : 0

  name                 = "postgres"
  resource_group_name  = local.resource_group_name
  virtual_network_name = azurerm_virtual_network.this[0].name
  address_prefixes     = [var.postgres_subnet_cidr]

  delegation {
    name = "postgres-flexible-server"
    service_delegation {
      name    = "Microsoft.DBforPostgreSQL/flexibleServers"
      actions = ["Microsoft.Network/virtualNetworks/subnets/join/action"]
    }
  }
}

resource "azurerm_private_dns_zone" "postgres" {
  count = local.create_postgres_dns_zone ? 1 : 0

  # Must end in .postgres.database.azure.com for flexible server VNet integration.
  name                = "${var.name}-${local.suffix}.private.postgres.database.azure.com"
  resource_group_name = local.resource_group_name
  tags                = local.tags

  depends_on = [azurerm_resource_group.this]
}

resource "azurerm_private_dns_zone_virtual_network_link" "postgres" {
  count = local.create_postgres_dns_zone ? 1 : 0

  name                 = "${var.name}-postgres"
  private_dns_zone_id  = azurerm_private_dns_zone.postgres[0].id
  virtual_network_id   = local.postgres_vnet_id
  registration_enabled = false
  tags                 = local.tags
}

resource "azurerm_private_dns_zone_virtual_network_link" "postgres_apps" {
  count = local.create_postgres_dns_zone && local.link_apps_vnet ? 1 : 0

  name                 = "${var.name}-container-apps"
  private_dns_zone_id  = azurerm_private_dns_zone.postgres[0].id
  virtual_network_id   = replace(var.existing_container_apps_subnet_id, "/(?i)/subnets/[^/]+$/", "")
  registration_enabled = false
  tags                 = local.tags
}
