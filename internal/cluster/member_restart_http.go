package cluster

import "net/http"

func (p *Peer) serveMemberRestart(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "member restart unavailable", http.StatusServiceUnavailable)
}
