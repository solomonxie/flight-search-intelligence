# Depends on: ami.tf (image), keypair.tf (key), network.tf (subnet +
# security group), variables.tf (counts/sizing).
# Downstream: inventory.tf (renders Ansible groups from these IPs),
# outputs.tf.
#
#   data.aws_ami.ubuntu_arm64 ─┐
#   aws_key_pair.admin ────────┼─▶ aws_instance.control_plane (count = control_plane_count)
#   aws_subnet.public ─────────┼─▶ aws_instance.worker         (count = worker_count)
#   aws_security_group.fleet ──┘
resource "aws_instance" "control_plane" {
  count = var.control_plane_count

  ami                         = data.aws_ami.ubuntu_arm64.id
  instance_type               = var.instance_type
  subnet_id                   = aws_subnet.public.id
  vpc_security_group_ids      = [aws_security_group.fleet.id]
  key_name                    = aws_key_pair.admin.key_name
  associate_public_ip_address = true

  root_block_device {
    volume_size = var.root_volume_gb
    volume_type = "gp3"
  }

  tags = { Name = "${var.project}-k3s-control-plane-${count.index}" }
}

resource "aws_instance" "worker" {
  count = var.worker_count

  ami                         = data.aws_ami.ubuntu_arm64.id
  instance_type               = var.instance_type
  subnet_id                   = aws_subnet.public.id
  vpc_security_group_ids      = [aws_security_group.fleet.id]
  key_name                    = aws_key_pair.admin.key_name
  associate_public_ip_address = true

  root_block_device {
    volume_size = var.root_volume_gb
    volume_type = "gp3"
  }

  tags = { Name = "${var.project}-k3s-worker-${count.index}" }
}
