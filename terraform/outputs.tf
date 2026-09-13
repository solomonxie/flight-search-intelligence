# Depends on: fleet.tf, inventory.tf. Downstream: nothing — terminal
# node of the graph, printed for whoever runs `terraform apply` next.
output "control_plane_public_ips" {
  value = aws_instance.control_plane[*].public_ip
}

output "worker_public_ips" {
  value = aws_instance.worker[*].public_ip
}

output "ansible_inventory_path" {
  value = local_file.ansible_inventory.filename
}
