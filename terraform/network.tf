# Depends on: variables.tf (CIDRs, AZ, admin CIDR).
# Downstream: fleet.tf (subnet id + security group id), outputs.tf.
#
#   aws_vpc.this
#     ├─ aws_internet_gateway.this
#     ├─ aws_subnet.public ── aws_route_table.public ── aws_route_table_association.public
#     └─ aws_security_group.fleet (SSH/k3s-API from ssh_admin_cidr; flannel
#         VXLAN + kubelet + NodePort range + HTTP/S intra-fleet and public
#         where a real deploy needs it; all egress open)
#
# Single public subnet, no NAT gateway: every node gets a public IP
# directly. Keeps this stack small and inspectable; a private-subnet +
# NAT/bastion layout is a hardening item, not done here — see
# terraform/README.md.
resource "aws_vpc" "this" {
  cidr_block           = var.vpc_cidr
  enable_dns_support   = true
  enable_dns_hostnames = true
  tags                 = { Name = "${var.project}-vpc" }
}

resource "aws_internet_gateway" "this" {
  vpc_id = aws_vpc.this.id
  tags   = { Name = "${var.project}-igw" }
}

resource "aws_subnet" "public" {
  vpc_id                  = aws_vpc.this.id
  cidr_block              = var.public_subnet_cidr
  availability_zone       = var.availability_zone
  map_public_ip_on_launch = true
  tags                    = { Name = "${var.project}-public" }
}

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.this.id

  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.this.id
  }

  tags = { Name = "${var.project}-public-rt" }
}

resource "aws_route_table_association" "public" {
  subnet_id      = aws_subnet.public.id
  route_table_id = aws_route_table.public.id
}

resource "aws_security_group" "fleet" {
  name        = "${var.project}-fleet"
  description = "k3s control-plane + agent nodes"
  vpc_id      = aws_vpc.this.id
  tags        = { Name = "${var.project}-fleet-sg" }

  # Admin access
  ingress {
    description = "SSH from admin"
    from_port   = 22
    to_port     = 22
    protocol    = "tcp"
    cidr_blocks = [var.ssh_admin_cidr]
  }
  ingress {
    description = "k3s/Kubernetes API from admin (kubectl/helm)"
    from_port   = 6443
    to_port     = 6443
    protocol    = "tcp"
    cidr_blocks = [var.ssh_admin_cidr]
  }

  # Intra-fleet: k3s server<->agent, flannel VXLAN, kubelet metrics.
  # self = true scopes each rule to this security group's own members
  # only, not the whole VPC.
  ingress {
    description = "k3s API, intra-fleet"
    from_port   = 6443
    to_port     = 6443
    protocol    = "tcp"
    self        = true
  }
  ingress {
    description = "flannel VXLAN, intra-fleet"
    from_port   = 8472
    to_port     = 8472
    protocol    = "udp"
    self        = true
  }
  ingress {
    description = "kubelet metrics, intra-fleet"
    from_port   = 10250
    to_port     = 10250
    protocol    = "tcp"
    self        = true
  }
  ingress {
    description = "NodePort range, intra-fleet"
    from_port   = 30000
    to_port     = 32767
    protocol    = "tcp"
    self        = true
  }

  # Public ingress once a real HTTP(S) service (search-api, an ingress
  # controller) is fronted by this fleet.
  ingress {
    description = "HTTP"
    from_port   = 80
    to_port     = 80
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }
  ingress {
    description = "HTTPS"
    from_port   = 443
    to_port     = 443
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }

  egress {
    description = "all outbound"
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}
