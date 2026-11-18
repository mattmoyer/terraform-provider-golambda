terraform {
  required_providers {
    golambda = {
      source = "spacelift.io/moyer/golambda"
    }
  }
}

# Private github.com modules are fetched using GITHUB_TOKEN from the
# environment by default.
provider "golambda" {}
