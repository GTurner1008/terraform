terraform {
  required_providers {
    bar = {
      source = "registry.terraform.io/hashicorp/bar"
    }
    baz = {
      source = "registry.terraform.io/terraform-providers/baz"
    }
    foo = {
      source = "registry.terraform.io/hashicorp/foo"
    }
  }
}
