# Depends on: variables.tf (var.ssh_public_key).
# Downstream: fleet.tf (every aws_instance references this key pair).
#
# Imports the admin's own existing public key rather than generating a
# keypair here — no private key material ever enters Terraform state.
resource "aws_key_pair" "admin" {
  key_name   = "${var.project}-admin"
  public_key = var.ssh_public_key
}
