package speaker

import (
	"context"
	"fmt"
	"net/http"

	api "github.com/osrg/gobgp/v3/api"
	"go.uber.org/zap"
)

// ServeHealth starts an HTTP server on the given port exposing /readyz.
// /readyz returns 200 only when all configured BGP peers are ESTABLISHED.
// It blocks until ctx is cancelled.
func (s *Speaker) ServeHealth(ctx context.Context, port int) {
	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", s.handleReadyz)

	srv := &http.Server{Addr: fmt.Sprintf(":%d", port), Handler: mux}
	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()

	s.log.Info("starting health server", zap.Int("port", port))
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		s.log.Error("health server error", zap.Error(err))
	}
}

func (s *Speaker) handleReadyz(w http.ResponseWriter, r *http.Request) {
	allEstablished := true
	_ = s.server.ListPeer(r.Context(), &api.ListPeerRequest{}, func(p *api.Peer) {
		if p.State == nil || p.State.SessionState != api.PeerState_ESTABLISHED {
			allEstablished = false
		}
	})

	if allEstablished {
		w.WriteHeader(http.StatusOK)
	} else {
		http.Error(w, "BGP sessions not yet established", http.StatusServiceUnavailable)
	}
}
