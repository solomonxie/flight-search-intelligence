# Depends on: nothing but the provider (region comes from providers.tf).
# Downstream: fleet.tf (every aws_instance's ami).
#
# Ubuntu 22.04 LTS, arm64 — matches var.instance_type's Graviton family
# and local M1 dev builds (DESIGN.md "Open decisions": same architecture
# on purpose, removes a whole class of arch-mismatch bugs).
data "aws_ami" "ubuntu_arm64" {
  most_recent = true
  owners      = ["099720109477"] # Canonical

  filter {
    name   = "name"
    values = ["ubuntu/images/hvm-ssd/ubuntu-jammy-22.04-arm64-server-*"]
  }
  filter {
    name   = "virtualization-type"
    values = ["hvm"]
  }
  filter {
    name   = "architecture"
    values = ["arm64"]
  }
}
