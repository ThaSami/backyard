# Offline tests: `terraform init -backend=false && terraform test` in
# deploy/terraform/gcp. Google providers are mocked; nothing is created.

mock_provider "google" {
  mock_data "google_project" {
    defaults = {
      number = "123456789012"
    }
  }

  mock_resource "google_compute_backend_service" {
    defaults = {
      generated_id = 4242424242
    }
  }

  mock_resource "google_compute_global_address" {
    defaults = {
      address = "203.0.113.10"
    }
  }

  mock_resource "google_sql_database_instance" {
    defaults = {
      connection_name = "acme-backyard:us-central1:backyard-pg-abc123"
    }
  }

  mock_resource "google_service_account" {
    defaults = {
      name   = "projects/acme-backyard/serviceAccounts/backyard-runtime@acme-backyard.iam.gserviceaccount.com"
      email  = "backyard-runtime@acme-backyard.iam.gserviceaccount.com"
      member = "serviceAccount:backyard-runtime@acme-backyard.iam.gserviceaccount.com"
    }
  }

  mock_resource "google_secret_manager_secret_version" {
    defaults = {
      version = "1"
    }
  }

  mock_resource "google_certificate_manager_dns_authorization" {
    defaults = {
      dns_resource_record = [{
        name = "_acme-challenge.backyard.example.com."
        type = "CNAME"
        data = "0123abcd.1.authorize.certificatemanager.goog."
      }]
    }
  }

  mock_resource "google_certificate_manager_certificate_map" {
    defaults = {
      id = "projects/acme-backyard/locations/global/certificateMaps/backyard-certs"
    }
  }
}

mock_provider "google-beta" {
  mock_resource "google_project_service_identity" {
    defaults = {
      email = "service-123456789012@gcp-sa-iap.iam.gserviceaccount.com"
    }
  }
}

variables {
  project_id  = "acme-backyard"
  domain      = "backyard.example.com"
  iap_members = ["domain:example.com"]
  admins      = ["alice@example.com"]
}

run "iap_defaults" {
  assert {
    condition     = strcontains(output.backyard_config_yaml, "mode: iap") && strcontains(output.backyard_config_yaml, "audience: \"/projects/123456789012/global/backendServices/4242424242\"")
    error_message = "IAP mode must render auth.iap.audience from the project number and the app backend service id."
  }

  assert {
    condition     = output.iap_audience == "/projects/123456789012/global/backendServices/4242424242"
    error_message = "iap_audience output is wrong."
  }

  assert {
    condition     = length(google_compute_backend_service.app.iap) == 1 && google_compute_backend_service.app.iap[0].enabled && length(google_compute_backend_service.deploy) == 1 && length(google_compute_backend_service.deploy[0].iap) == 0
    error_message = "The app backend must have IAP and the deploy backend must not."
  }

  assert {
    condition     = length(google_compute_url_map.https.path_matcher[0].route_rules) == 5 && google_compute_url_map.https.path_matcher[0].route_rules[3].match_rules[0].header_matches[0].header_name == "Authorization"
    error_message = "Deploy routes must send bearer-token gateway calls to the deploy backend."
  }

  assert {
    # `backyard token create` (POST /__backyard/api/tokens with a Backyard
    # bearer token) cannot pass IAP; browser calls without Authorization stay
    # behind it.
    condition = anytrue([
      for r in google_compute_url_map.https.path_matcher[0].route_rules :
      r.service == google_compute_backend_service.deploy[0].id &&
      r.match_rules[0].full_path_match == "/__backyard/api/tokens" &&
      length(r.match_rules[0].header_matches) == 1 &&
      r.match_rules[0].header_matches[0].header_name == "Authorization" &&
      r.match_rules[0].header_matches[0].present_match &&
      r.header_action[0].request_headers_to_remove == tolist(["X-Goog-IAP-JWT-Assertion", "X-Goog-Authenticated-User-Email", "X-Goog-Authenticated-User-Id"])
    ])
    error_message = "POST /__backyard/api/tokens with an Authorization header must reach the deploy backend (no IAP) with the IAP headers stripped."
  }

  assert {
    condition = alltrue([
      for r in google_compute_url_map.https.path_matcher[0].route_rules :
      length(r.match_rules[0].header_matches) == 1 if r.match_rules[0].full_path_match == "/__backyard/api/tokens" || r.match_rules[0].prefix_match == "/__backyard/api/deploy/"
    ])
    error_message = "Only requests with an Authorization header may bypass IAP on the token API and the gateway."
  }

  assert {
    condition     = google_cloud_run_v2_service.backyard.ingress == "INGRESS_TRAFFIC_INTERNAL_LOAD_BALANCER" && google_cloud_run_v2_service.backyard.template[0].timeout == "3600s" && google_cloud_run_v2_service.backyard.template[0].session_affinity
    error_message = "Cloud Run must only accept load balancer traffic, allow hour-long websockets and use session affinity."
  }

  assert {
    condition     = google_cloud_run_v2_service.backyard.template[0].containers[0].resources[0].cpu_idle == false
    error_message = "CPU must be always allocated."
  }

  assert {
    condition     = output.image == "us-central1-docker.pkg.dev/acme-backyard/backyard-mirror/thasami/backyard:latest" && google_artifact_registry_repository.mirror[0].remote_repository_config[0].docker_repository[0].custom_repository[0].uri == "https://ghcr.io"
    error_message = "ghcr.io images must be pulled through the Artifact Registry remote repository."
  }

  assert {
    condition     = toset(keys(google_secret_manager_secret.managed)) == toset(["BACKYARD_SECRET", "DATABASE_URL"])
    error_message = "Default deployment manages exactly the server secret and the database DSN."
  }

  assert {
    condition     = startswith(nonsensitive(google_secret_manager_secret_version.managed["DATABASE_URL"].secret_data), "host=/cloudsql/acme-backyard:us-central1:backyard-pg-abc123 dbname=backyard user=backyard password=")
    error_message = "The Cloud SQL DSN must use the connector's Unix socket."
  }

  assert {
    condition     = length(output.dns_records) == 3 && output.dns_records[2].type == "CNAME" && !output.dns_records_managed
    error_message = "Host mode needs apex + wildcard A records and the certificate CNAME."
  }

  assert {
    condition     = toset(google_certificate_manager_certificate.backyard[0].managed[0].domains) == toset(["backyard.example.com", "*.backyard.example.com"])
    error_message = "The certificate must cover the hub and the wildcard."
  }

  assert {
    condition     = strcontains(output.backyard_config_yaml, "\"smart\":\n      provider: \"claude\"\n      model: \"claude-opus-5-5\"") && strcontains(output.backyard_config_yaml, "platform: vertex")
    error_message = "Vertex AI must configure Claude on Vertex as the smart alias."
  }

  assert {
    condition     = google_iap_web_backend_service_iam_binding.access[0].members == toset(["domain:example.com"])
    error_message = "IAP access must be granted to iap_members."
  }

  assert {
    condition     = !strcontains(output.backyard_config_yaml, "secret:") && !strcontains(output.backyard_config_yaml, "password")
    error_message = "The rendered config must not contain secrets."
  }

  assert {
    condition     = google_cloud_run_v2_service.backyard.default_uri_disabled && google_cloud_run_v2_service.backyard.invoker_iam_disabled
    error_message = "The run.app URL must be disabled so the load balancer is the only way in."
  }

  # Structural checks on the parsed document (Terraform's YAML decoder does
  # not expand $${ENV}; configcheck/ loads it with Backyard's own loader).
  assert {
    condition = (
      yamldecode(output.backyard_config_yaml).auth.iap.audience == "/projects/123456789012/global/backendServices/4242424242" &&
      yamldecode(output.backyard_config_yaml).storage.gcs.signer_email == "backyard-runtime@acme-backyard.iam.gserviceaccount.com" &&
      yamldecode(output.backyard_config_yaml).database.type == "postgres" &&
      yamldecode(output.backyard_config_yaml).database.postgres.max_conns == 4 &&
      yamldecode(output.backyard_config_yaml).pubsub.type == "postgres" &&
      yamldecode(output.backyard_config_yaml).server.shutdown_timeout == "8s"
    )
    error_message = "The rendered config must parse as YAML with the expected structure."
  }

  assert {
    condition     = yamldecode(output.backyard_config_yaml).files.max_upload_bytes == 32505856 && yamldecode(output.backyard_config_yaml).sites.max_file_bytes == 32505856
    error_message = "Uploads and site files must be capped below Cloud Run's 32 MiB HTTP/1 limit."
  }

  assert {
    condition     = tolist(yamldecode(output.backyard_config_yaml).deploy.methods) == tolist(["api"]) && yamldecode(output.backyard_config_yaml).deploy.expose_storage == false
    error_message = "Without bucket writers only the api deploy method is advertised and the bucket stays private."
  }
}

run "hardening" {
  assert {
    condition     = google_sql_database_instance.backyard[0].settings[0].connector_enforcement == "REQUIRED" && length(google_sql_database_instance.backyard[0].settings[0].ip_configuration[0].authorized_networks) == 0
    error_message = "Cloud SQL must only accept Cloud SQL connector connections."
  }

  assert {
    condition     = strcontains(google_project_iam_member.runtime_cloudsql_client[0].condition[0].expression, "/instances/${google_sql_database_instance.backyard[0].name}")
    error_message = "cloudsql.client must be limited to the Backyard instance."
  }

  assert {
    condition     = google_storage_bucket.sites.public_access_prevention == "enforced" && google_storage_bucket.sites.uniform_bucket_level_access && google_storage_bucket.sites.cors[0].origin == tolist(["https://backyard.example.com"])
    error_message = "The bucket must be private with CORS for the hub origin only."
  }

  assert {
    condition = (
      contains(google_storage_bucket.sites.cors[0].method, "PUT") &&
      alltrue([for h in ["content-type", "x-goog-content-length-range"] : contains([for r in google_storage_bucket.sites.cors[0].response_header : lower(r)], h)])
    )
    error_message = "The bucket CORS rule must allow the headers of a signed upload (Content-Type, x-goog-content-length-range), or browser uploads fail their preflight."
  }

  assert {
    condition     = google_compute_url_map.https.path_matcher[0].route_rules[0].header_action[0].request_headers_to_remove == tolist(["X-Goog-IAP-JWT-Assertion", "X-Goog-Authenticated-User-Email", "X-Goog-Authenticated-User-Id"])
    error_message = "Routes that bypass IAP must strip client-supplied IAP headers."
  }

  assert {
    condition     = google_cloud_run_v2_service_iam_member.iap_invoker[0].member == "serviceAccount:service-123456789012@gcp-sa-iap.iam.gserviceaccount.com"
    error_message = "The IAP service agent must be able to invoke the service."
  }

  assert {
    condition     = google_compute_ssl_policy.modern.min_tls_version == "TLS_1_2" && google_compute_target_https_proxy.https.certificate_map == "//certificatemanager.googleapis.com/projects/acme-backyard/locations/global/certificateMaps/backyard-certs"
    error_message = "HTTPS must use the certificate map and a TLS 1.2+ policy."
  }
}

run "oidc_external_postgres" {
  variables {
    auth_mode = "oidc"
    oidc = {
      issuer           = "https://acme.okta.com/oauth2/default"
      client_id        = "0oa123"
      device_client_id = "0oa456"
    }
    oidc_client_secret           = "s3cr3t"
    database                     = "external-postgres"
    external_database_url_secret = "projects/shared-db/secrets/backyard-dsn"
    vertex_ai                    = false
    anthropic_api_key            = "sk-ant-test"
    iap_members                  = []
  }

  assert {
    condition     = length(google_compute_backend_service.deploy) == 0 && length(google_compute_backend_service.app.iap) == 0 && length(google_compute_url_map.https.path_matcher) == 0
    error_message = "OIDC mode must not configure IAP or the bypass backend."
  }

  assert {
    condition     = length(google_sql_database_instance.backyard) == 0 && length(google_project_iam_member.runtime_cloudsql_client) == 0
    error_message = "external-postgres must not create Cloud SQL."
  }

  assert {
    condition     = toset(keys(google_secret_manager_secret.managed)) == toset(["BACKYARD_SECRET", "OIDC_CLIENT_SECRET", "ANTHROPIC_API_KEY"])
    error_message = "Secrets must be created for the server secret, OIDC client secret and Anthropic key only."
  }

  assert {
    condition     = google_secret_manager_secret_iam_member.external["DATABASE_URL"].project == "shared-db" && google_secret_manager_secret_iam_member.external["DATABASE_URL"].secret_id == "backyard-dsn"
    error_message = "The runtime must be able to read the external DSN secret."
  }

  assert {
    condition     = strcontains(output.backyard_config_yaml, "client_secret: \"$${OIDC_CLIENT_SECRET}\"") && strcontains(output.backyard_config_yaml, "api_key: \"$${ANTHROPIC_API_KEY}\"") && strcontains(output.backyard_config_yaml, "device_client_id: \"0oa456\"")
    error_message = "Secrets must be referenced as $${ENV} in the config."
  }

  assert {
    condition     = output.iap_audience == null && length(google_cloud_run_v2_service.backyard.template[0].volumes) == 0
    error_message = "No IAP audience and no Cloud SQL volume outside IAP / Cloud SQL mode."
  }
}

run "path_mode_without_ai" {
  # auth_mode = "none" without Cloud Armor is flagged.
  expect_failures = [check.public_without_auth]

  variables {
    auth_mode     = "none"
    url_mode      = "path"
    vertex_ai     = false
    image         = "us-docker.pkg.dev/acme-images/backyard/backyard:0.1.0"
    enable_ipv6   = true
    iap_members   = []
    http_redirect = false
  }

  assert {
    condition     = output.image == "us-docker.pkg.dev/acme-images/backyard/backyard:0.1.0" && length(google_artifact_registry_repository.mirror) == 0
    error_message = "Artifact Registry images must be used as-is."
  }

  assert {
    condition     = !strcontains(output.backyard_config_yaml, "\nai:") && strcontains(output.backyard_config_yaml, "url_mode: path")
    error_message = "No AI section without providers; path mode rendered."
  }

  assert {
    condition     = toset(google_certificate_manager_certificate.backyard[0].managed[0].domains) == toset(["backyard.example.com"]) && length(google_certificate_manager_certificate_map_entry.hosts) == 1
    error_message = "Path mode needs no wildcard certificate."
  }

  assert {
    condition     = length(google_compute_global_forwarding_rule.https) == 2 && length(google_compute_global_forwarding_rule.http_redirect) == 0 && length(output.dns_records) == 3
    error_message = "IPv6 adds a forwarding rule and an AAAA record; http_redirect = false removes port 80."
  }
}

run "docker_hub_image" {
  variables {
    image = "nginx:1.27"
  }

  assert {
    condition     = output.image == "us-central1-docker.pkg.dev/acme-backyard/backyard-mirror/library/nginx:1.27" && google_artifact_registry_repository.mirror[0].remote_repository_config[0].docker_repository[0].public_repository == "DOCKER_HUB"
    error_message = "Docker Hub images must go through a DOCKER_HUB remote repository under library/."
  }
}

run "bigquery_and_dns" {
  variables {
    bigquery_warehouse = true
    bigquery_datasets  = ["analytics", "other-project.events"]
    warehouse_queries = {
      weekly_active_users = {
        sql         = "SELECT day, users FROM analytics.wau WHERE team = @team"
        description = "WAU per team"
        params      = ["team"]
      }
    }
    dns_zone_name         = "example-com"
    bucket_writer_members = ["group:eng@example.com"]
    extra_config_yaml     = "identity:\n  directory:\n    type: csv\n    path: /etc/backyard/people.csv\nsites:\n  ownership: owner\n  max_file_bytes: 10485760\n"
  }

  assert {
    condition     = google_bigquery_dataset_iam_member.runtime_data_viewer["other-project.events"].project == "other-project" && google_bigquery_dataset_iam_member.runtime_data_viewer["analytics"].project == "acme-backyard" && length(google_project_iam_member.runtime_bigquery_data_viewer) == 0
    error_message = "Dataset grants must honour project.dataset references."
  }

  assert {
    condition     = strcontains(output.backyard_config_yaml, "type: bigquery") && strcontains(output.backyard_config_yaml, "\"weekly_active_users\":") && strcontains(output.backyard_config_yaml, "max_bytes_billed: 10737418240")
    error_message = "The warehouse section must be rendered."
  }

  assert {
    condition     = length(google_dns_record_set.address) == 2 && length(google_dns_record_set.certificate_authorization) == 1 && output.dns_records_managed
    error_message = "With a Cloud DNS zone the module creates the records."
  }

  assert {
    condition     = strcontains(output.backyard_config_yaml, "methods: [\"api\",\"gcs\"]") && strcontains(output.backyard_config_yaml, "expose_storage: true") && strcontains(output.backyard_config_yaml, "ownership: owner")
    error_message = "Bucket writers enable the gcs deploy method; extra_config_yaml is appended."
  }

  assert {
    # Direct-sync deployers write sites/ only: _meta/ (site owners, used by
    # sites.ownership = owner) and uploads/ stay with the server.
    condition = (
      google_storage_bucket_iam_member.writers["group:eng@example.com"].role == "roles/storage.objectUser" &&
      length(google_storage_bucket_iam_member.writers["group:eng@example.com"].condition) == 1 &&
      google_storage_bucket_iam_member.writers["group:eng@example.com"].condition[0].expression == "resource.name.startsWith(\"projects/_/buckets/${google_storage_bucket.sites.name}/objects/sites/\")"
    )
    error_message = "Bucket writers must be limited to objects below sites/ by an IAM condition."
  }

  assert {
    condition = (
      google_storage_bucket_iam_member.writers_list["group:eng@example.com"].role == "roles/storage.objectViewer" &&
      google_storage_bucket_iam_member.writers_list["group:eng@example.com"].condition[0].expression == "resource.type == \"storage.googleapis.com/Bucket\"" &&
      length([for b in google_storage_bucket_iam_member.writers_list : b if length(b.condition) == 0]) == 0 &&
      length(google_storage_bucket_iam_member.writers) == 1 && length(google_storage_bucket_iam_member.writers_list) == 1
    )
    error_message = "Bucket writers may list the bucket (rsync), but never read objects outside sites/."
  }

  assert {
    condition     = yamldecode(output.backyard_config_yaml).sites.max_file_bytes == 10485760 && yamldecode(output.backyard_config_yaml).files.max_upload_bytes == 32505856
    error_message = "A sites section in extra_config_yaml replaces the module's; files keeps the module cap."
  }

  assert {
    condition     = yamldecode(output.backyard_config_yaml).warehouse.allow_raw_sql == false && yamldecode(output.backyard_config_yaml).warehouse.sources.bigquery.description == "BigQuery (acme-backyard)"
    error_message = "Raw SQL is off by default (as in Backyard); the source is described."
  }
}

run "escapes_literal_dollar_braces" {
  variables {
    display_name      = "Acme $${HOME} \"Yard\""
    anthropic_api_key = "sk-ant-test"
    vertex_ai         = false
    ai_models = {
      deep = { provider = "anthropic", model = "claude-opus-5-5", description = "costs $${PRICE}", effort = "high", max_tokens = 32000 }
    }
    bigquery_warehouse = true
    warehouse_queries = {
      tagged = { sql = "SELECT '$${x}' AS t" }
    }
    extra_config_yaml = "identity:\n  directory:\n    type: csv\n    path: $${DIRECTORY_PATH}\n"
    # What the reference above expands to (configcheck loads the config with
    # the container's environment).
    extra_env = { DIRECTORY_PATH = "/etc/backyard/people.csv" }
  }

  # Backyard reads "$$${" as a literal "$${"; the intended references stay.
  assert {
    condition = (
      strcontains(output.backyard_config_yaml, "name: \"Acme $$${HOME} \\\"Yard\\\"\"") &&
      strcontains(output.backyard_config_yaml, "description: \"costs $$${PRICE}\"") &&
      strcontains(output.backyard_config_yaml, "sql: \"SELECT '$$${x}' AS t\"") &&
      strcontains(output.backyard_config_yaml, "api_key: \"$${ANTHROPIC_API_KEY}\"") &&
      strcontains(output.backyard_config_yaml, "path: $${DIRECTORY_PATH}") &&
      !strcontains(output.backyard_config_yaml, "\u0001")
    )
    error_message = "Only the module's own and extra_config_yaml's $${ENV} references may stay expandable."
  }

  assert {
    condition     = yamldecode(output.backyard_config_yaml).ai.models.deep.effort == "high" && yamldecode(output.backyard_config_yaml).ai.models.deep.max_tokens == 32000
    error_message = "ai_models effort and max_tokens must be rendered."
  }
}

run "iap_access_managed_elsewhere" {
  expect_failures = [check.iap_access]

  variables {
    iap_members = []
  }

  assert {
    condition     = length(google_iap_web_backend_service_iam_binding.access) == 0
    error_message = "An empty iap_members must not create an authoritative (empty) binding."
  }
}

run "existing_certificate_map" {
  variables {
    certificate_map = "projects/acme-backyard/locations/global/certificateMaps/shared"
  }

  assert {
    condition     = length(google_certificate_manager_certificate.backyard) == 0 && google_compute_target_https_proxy.https.certificate_map == "//certificatemanager.googleapis.com/projects/acme-backyard/locations/global/certificateMaps/shared"
    error_message = "An existing certificate map is attached as-is."
  }

  assert {
    condition     = length(output.dns_records) == 2 && alltrue([for r in output.dns_records : r.type == "A"])
    error_message = "Without managed certificates no DNS authorization record is needed."
  }
}

run "database_connection_budget" {
  expect_failures = [check.database_connections]

  variables {
    max_instances = 20
  }
}

run "database_connection_budget_raised" {
  variables {
    max_instances            = 20
    database_max_connections = 400
  }

  assert {
    condition     = one(google_sql_database_instance.backyard[0].settings[0].database_flags).name == "max_connections" && one(google_sql_database_instance.backyard[0].settings[0].database_flags).value == "400"
    error_message = "database_max_connections sets the Cloud SQL flag."
  }
}

run "extra_files_section_must_cap_uploads" {
  expect_failures = [check.cloud_run_body_limits]

  variables {
    extra_config_yaml = "files:\n  max_upload_bytes: 104857600\n"
  }

  assert {
    condition     = length(regexall("(?m)^files:", output.backyard_config_yaml)) == 1
    error_message = "The module must not render a second files section."
  }
}

run "warehouse_without_queries_or_raw_sql" {
  expect_failures = [check.warehouse_usable]

  variables {
    bigquery_warehouse = true
  }
}

run "rejects_oidc_without_settings" {
  command = plan

  variables {
    auth_mode = "oidc"
  }

  expect_failures = [terraform_data.validation]
}

run "rejects_ambiguous_external_database" {
  command = plan

  variables {
    database                     = "external-postgres"
    external_database_url        = "postgres://u:p@db:5432/backyard"
    external_database_url_secret = "backyard-dsn"
  }

  expect_failures = [terraform_data.validation]
}

run "rejects_unknown_ai_provider" {
  command = plan

  variables {
    ai_models = {
      gpt = { provider = "openai", model = "gpt-5" }
    }
  }

  expect_failures = [terraform_data.validation]
}

run "rejects_generated_sections_in_extra_yaml" {
  command = plan

  variables {
    extra_config_yaml = "auth:\n  mode: none\n"
  }

  expect_failures = [terraform_data.validation]
}

run "rejects_bad_domain" {
  command = plan

  variables {
    domain = "https://Backyard.Example.com/"
  }

  expect_failures = [var.domain]
}

run "rejects_secret_breaking_yaml" {
  command = plan

  variables {
    auth_mode = "oidc"
    oidc = {
      issuer    = "https://acme.okta.com/oauth2/default"
      client_id = "0oa123"
    }
    oidc_client_secret = "abc\"def"
  }

  expect_failures = [var.oidc_client_secret]
}

run "rejects_certificate_map_url" {
  command = plan

  variables {
    certificate_map = "//certificatemanager.googleapis.com/projects/p/locations/global/certificateMaps/m"
  }

  expect_failures = [var.certificate_map]
}

run "bucket_writers_with_storage_prefix" {
  command = plan

  variables {
    storage_prefix        = "/team-a/"
    bucket_writer_members = ["serviceAccount:ci@acme-backyard.iam.gserviceaccount.com", "group:eng@example.com"]
  }

  assert {
    condition = alltrue([
      for b in google_storage_bucket_iam_member.writers :
      b.condition[0].expression == "resource.name.startsWith(\"projects/_/buckets/${google_storage_bucket.sites.name}/objects/team-a/sites/\")"
    ]) && length(google_storage_bucket_iam_member.writers) == 2
    error_message = "The writers' condition must use the normalised storage prefix (team-a/sites/), like Backyard's object keys."
  }
}

run "rejects_storage_prefix_traversal" {
  command = plan

  variables {
    storage_prefix = "a/../b"
  }

  expect_failures = [var.storage_prefix]
}

run "rejects_datasets_without_warehouse" {
  command = plan

  variables {
    bigquery_datasets = ["analytics"]
  }

  expect_failures = [terraform_data.validation]
}

run "rejects_unknown_extra_section" {
  command = plan

  variables {
    extra_config_yaml = "ratelimits:\n  ai_per_minute: 5\n"
  }

  expect_failures = [terraform_data.validation]
}

run "rejects_indented_extra_yaml" {
  command = plan

  variables {
    extra_config_yaml = "  sites:\n    ownership: owner\n    max_file_bytes: 1048576\n"
  }

  expect_failures = [terraform_data.validation]
}
