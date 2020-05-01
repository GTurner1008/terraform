terraform {
  required_providers {
    bar = {
      source = "registry.terraform.io/hashicorp/bar"
      version = "1.0.0"
    }
    baz = {
      source = "registry.terraform.io/terraform-providers/baz"
      version = "~> 2.0.0"
    }
    foo = {
      source = "registry.terraform.io/hashicorp/foo"
    }
  }
}
