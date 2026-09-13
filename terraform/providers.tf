# Depends on: versions.tf (provider version constraint).
# Downstream: every resource/data source in this folder implicitly uses
# this provider. default_tags means every resource below inherits
# Project/Environment/ManagedBy/Owner without repeating them — see the
# `terraform` skill's tagging strategy.
provider "aws" {
  region = var.aws_region

  default_tags {
    tags = {
      Project     = var.project
      Environment = var.environment
      ManagedBy   = "terraform"
      Owner       = var.owner
    }
  }
}
