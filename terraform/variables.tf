# Depends on: nothing (leaf inputs). Downstream: every other .tf file
# reads from var.* declared here.

variable "aws_region" {
  description = "AWS region for the whole fleet"
  type        = string
  default     = "us-west-2"
}

variable "project" {
  description = "Tag value identifying this stack in Cost Explorer"
  type        = string
  default     = "flight-search-intelligence"
}

variable "environment" {
  description = "sandbox | dev | staging | prod"
  type        = string
  default     = "prod"
}

variable "owner" {
  description = "Person/team accountable for this stack's cost"
  type        = string
  default     = "flight-search-intelligence"
}

variable "vpc_cidr" {
  description = "CIDR for the fleet's VPC"
  type        = string
  default     = "10.42.0.0/16"
}

variable "public_subnet_cidr" {
  description = "CIDR for the single public subnet the fleet lives in"
  type        = string
  default     = "10.42.1.0/24"
}

variable "availability_zone" {
  description = "Single AZ for the fleet's subnet — no cross-AZ HA yet, see terraform/README.md"
  type        = string
  default     = "us-west-2a"
}

variable "instance_type" {
  description = "arm64/Graviton per DESIGN.md 'Open decisions' — same arch as local M1 builds"
  type        = string
  default     = "t4g.medium"
}

variable "control_plane_count" {
  description = "k3s server nodes (DESIGN.md picked k3s over kubeadm/RKE2) — 1 = no HA control plane yet"
  type        = number
  default     = 1
}

variable "worker_count" {
  description = "k3s agent nodes"
  type        = number
  default     = 2
}

variable "root_volume_gb" {
  description = "Root EBS volume size per instance"
  type        = number
  default     = 40
}

variable "ssh_public_key" {
  description = "Admin's own SSH public key (e.g. contents of ~/.ssh/id_ed25519.pub) — no private key material is generated or stored by this stack"
  type        = string
}

variable "ssh_admin_cidr" {
  description = "CIDR allowed to reach SSH (22) and the k3s API (6443) from outside the VPC — set to your own IP/32, never left as 0.0.0.0/0"
  type        = string
}
