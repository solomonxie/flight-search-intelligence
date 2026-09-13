# Depends on: fleet.tf (instance public IPs).
# Downstream: nothing in Terraform — this is the bridge to the next
# tool in the pipeline (ansible-playbook, see terraform/README.md).
#
# Writes an Ansible inventory ini from inventory.tpl so
# `ansible/roles/k8s_fleet` has real host IPs to configure, instead of
# a human copy-pasting them out of `terraform output`.
resource "local_file" "ansible_inventory" {
  filename = "${path.module}/../ansible/inventories/aws/hosts.ini"
  content = templatefile("${path.module}/inventory.tpl", {
    control_plane_ips = aws_instance.control_plane[*].public_ip
    worker_ips        = aws_instance.worker[*].public_ip
  })
}
