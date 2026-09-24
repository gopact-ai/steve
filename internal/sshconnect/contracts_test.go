package sshconnect

// The only production implementation of a capability sshconnect probes for.
var _ ConnectionBinder = OpenSSH{}
