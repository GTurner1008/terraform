terraform {
  required_providers {
    bar = {
      source = "registry.terraform.io/hashicorp/bar"
      version = "1.0.0"
    }
    foo = {
      source = "registry.terraform.io/hashicorp/foo"
    }
    unknown = {
      # TF-UPGRADE-TODO
      #
      # No source detected for this provider. You must add a source address
      # in the following format:
      #
      # source = "your.domain.com/organization/unknown"
      #
      # For more information, see the provider source documentation:
      #
      # https://www.terraform.io/docs/configuration/providers.html#provider-source
      version = "~> 2.0.0"
    }
  }
}
