package cli

import "context"

// CloseLeaseSSHSessions ends every multiplexed SSH master Crabbox holds for the
// lease that target addresses. A provider whose checkpoint captures live
// sockets calls it before the capture: CRIU cannot dump an established TCP
// connection, so a master left open into the lease makes the dump fail or hang.
// On a platform without control masters the platform implementation is a no-op.
func CloseLeaseSSHSessions(ctx context.Context, target SSHTarget) error {
	leaseDir := sshControlLeaseDirectory(target)
	if leaseDir == "" {
		return nil
	}
	return closeLeaseSSHControlMasters(ctx, leaseDir)
}
