package coordination

// The only production implementation of a capability coordination probes for.
var _ membershipRevoker = (*TLSStreamLayer)(nil)
