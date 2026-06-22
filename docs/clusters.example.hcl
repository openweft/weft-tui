# Example weft-tui clusters config (place at ~/.config/weft/clusters.hcl).
#
# The TUI iterates the endpoint list at startup + on every health-
# check failure (3-s gRPC timeout, 5-s ticker, 2 consecutive failures
# triggers a swap). Connection loss → transparently fails over to the
# next reachable host, no operator action.

cluster "live" {
  default_ssh_key  = "~/.ssh/id_ed25519"
  default_ssh_user = "admin"
  default_socket   = "/home/admin/.weft/weft.sock"

  endpoint "dc1" { address = "admin@dc1-r1-h1" }
  endpoint "dc2" { address = "admin@dc2-r1-h1" }
  endpoint "dc3" { address = "admin@dc3-r1-h1" }
}

# SRV-record form — operator runs DNS, TUI auto-discovers :
# cluster "prod" {
#   default_ssh_key = "~/.ssh/id_ed25519"
#   default_socket  = "/home/admin/.weft/weft.sock"
#   srv_record = "_weft._tcp.cluster.example.com"
# }
