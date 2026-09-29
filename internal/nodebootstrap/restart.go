package nodebootstrap

// RestartSpec describes restarting an installed peer on the program it
// already has.
type RestartSpec struct {
	IfStopped bool
}

// BuildPeerRestart is the script that restarts a peer.
func BuildPeerRestart(RestartSpec) string {
	return "exit 1\n"
}

const peerStartSection, peerLocateSection, peerStopSection = "", "", ""
