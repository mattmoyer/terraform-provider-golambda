terraform {
  required_providers {
    golambda = {
      source = "registry.opentofu.org/mattmoyer/golambda"
    }
  }
}

# Private github.com modules are fetched using GITHUB_TOKEN from the
# environment by default.
provider "golambda" {}
