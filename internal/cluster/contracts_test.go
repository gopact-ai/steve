package cluster

import (
	"github.com/gopact-ai/steve/internal/coordination"
	"github.com/gopact-ai/steve/internal/sshconnect"
)

// The only production implementations of capabilities coordination and
// sshconnect probe for.
var (
	_ coordination.CheckpointApplication = application{}
	_ sshconnect.AliasBackend            = peerSSHBackend{}
	_ sshconnect.Linker                  = peerSSHBackend{}
	_ sshconnect.RegistrationAbandoner   = peerSSHBackend{}
	_ sshconnect.RegistrationRecovery    = peerSSHBackend{}
	_ sshconnect.RegistrationVerifier    = peerSSHBackend{}
	_ sshconnect.UpgradeBackend          = peerSSHBackend{}
)
