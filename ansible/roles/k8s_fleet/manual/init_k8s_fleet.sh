#!/usr/bin/env bash
# Manual equivalent of ansible/roles/k8s_fleet — for reference, not execution.

# tasks/control_plane.yml: deploy k3s server config
mkdir -p /etc/rancher/k3s
cat > /etc/rancher/k3s/config.yaml <<EOF
tls-san:
  - <control-plane-private-ip>
  - <control-plane-public-ip>
node-ip: <control-plane-private-ip>
write-kubeconfig-mode: "0644"
EOF

# tasks/control_plane.yml: install k3s server
curl -sfL https://get.k3s.io | INSTALL_K3S_CHANNEL=stable sh -s - server

# tasks/control_plane.yml: read the join token the workers need
cat /var/lib/rancher/k3s/server/node-token

# tasks/control_plane.yml: install Helm
curl -sfL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash

# tasks/worker.yml: on each worker, deploy k3s agent config
cat > /etc/rancher/k3s/config.yaml <<EOF
server: https://<control-plane-private-ip>:6443
token: <node-token-from-above>
node-ip: <worker-private-ip>
EOF

# tasks/worker.yml: install k3s agent
curl -sfL https://get.k3s.io | INSTALL_K3S_CHANNEL=stable sh -s - agent
