# Terraform

Provisions the EC2 fleet + networking for the self-managed Kubernetes
(k3s) cluster DESIGN.md's "Infra" section describes. File order below
doesn't matter — reference order does; see each file's own header
comment for its exact place in the graph.

**Not applied.** This is reviewable IaC, not a running deployment —
running `terraform apply` here provisions real, billable AWS resources.
See root DESIGN.md "Open decisions" for why: k3s (not kubeadm/RKE2),
arm64/Graviton, single public subnet (no NAT/private-subnet layer yet).

```
terraform apply
  ├─ versions.tf    → provider/version constraints (aws, local)
  ├─ variables.tf   → resolve inputs (sizing, CIDRs, admin SSH key/CIDR)
  ├─ providers.tf   → aws provider + default_tags (Project/Environment/
  │                    ManagedBy/Owner on every resource below)
  ├─ network.tf     → VPC, public subnet, IGW, route table, one
  │                    security group (SSH/k3s-API from admin CIDR;
  │                    flannel/kubelet/NodePort intra-fleet; HTTP/S public)
  ├─ keypair.tf     → imports var.ssh_public_key (no private key ever
  │                    touches Terraform state)
  ├─ ami.tf         → looks up the latest Ubuntu 22.04 arm64 AMI
  ├─ fleet.tf       → EC2 instances: control_plane_count k3s servers +
  │                    worker_count k3s agents, off of the network/
  │                    keypair/ami resources above
  ├─ inventory.tf   → renders ansible/inventories/aws/hosts.ini from
  │                    the instances' public IPs (inventory.tpl)
  └─ outputs.tf     → prints both IP lists + the inventory path
        │
        ▼
ansible-playbook -i ansible/inventories/aws/hosts.ini \
  ansible/playbooks/k8s_fleet.yml
  (installs/joins k3s on every node — ansible/roles/k8s_fleet)
        │
        ▼
helm install ...   (helm/ charts, against the resulting cluster)
```

## Required inputs

`terraform.tfvars` (gitignored) or `-var` flags:

- `ssh_public_key` — your own public key, e.g. `$(cat ~/.ssh/id_ed25519.pub)`
- `ssh_admin_cidr` — your own IP, e.g. `1.2.3.4/32`; never `0.0.0.0/0`

Everything else has a default (see `variables.tf`).

## Known gaps (accepted for this project's scale, not oversights)

- No NAT gateway/private subnet — every node gets a public IP directly.
- No multi-AZ — single `availability_zone` var.
- No cost/self-termination auto-cleanup — this is meant to be a
  standing fleet, not a throwaway sandbox.
- Activate `Project`/`Environment` as Cost Explorer cost-allocation
  tags after the first real `apply` — untracked tags don't backfill.
