package speaker

import (
	"context"
	"fmt"
	"net"

	api "github.com/osrg/gobgp/v3/api"
	gobgp "github.com/osrg/gobgp/v3/pkg/server"
	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/anypb"
)

// Config holds the BGP session configuration for the speaker.
type Config struct {
	LocalAS  uint32
	RemoteAS uint32
	// PeerAddress is the IPv4 BGP peer (required).
	PeerAddress string
	// PeerAddressV6 is the IPv6 BGP peer (optional). When set, a separate
	// session is opened for IPv6 routes. When empty, AnnounceIPv6 returns an error.
	PeerAddressV6 string
	// RouterID is the BGP router ID, typically the node's primary IPv4 address.
	// Defaults to "0.0.0.0" if empty.
	RouterID string
	// GracefulRestartTime is the BGP graceful restart time in seconds advertised to
	// peers. When non-zero, Calico will hold stale routes for this duration during
	// a speaker restart, eliminating forwarding gaps. Set to 0 to disable.
	GracefulRestartTime uint32
}

// Speaker manages GoBGP sessions and exposes announce/withdraw operations.
// Call Start before announcing routes, and Stop after all routes have been withdrawn.
type Speaker struct {
	cfg    *Config
	server *gobgp.BgpServer
	log    *zap.Logger
}

func New(cfg *Config, log *zap.Logger) *Speaker {
	return &Speaker{
		cfg:    cfg,
		server: gobgp.NewBgpServer(),
		log:    log,
	}
}

// Start initializes the BGP server and establishes peer sessions.
func (s *Speaker) Start(ctx context.Context) error {
	go s.server.Serve()

	routerID := s.cfg.RouterID
	if routerID == "" {
		routerID = "0.0.0.0"
	}

	if err := s.server.StartBgp(ctx, &api.StartBgpRequest{
		Global: &api.Global{
			Asn:        s.cfg.LocalAS,
			RouterId:   routerID,
			ListenPort: -1,
		},
	}); err != nil {
		return fmt.Errorf("starting BGP server: %w", err)
	}
	s.log.Info("BGP server started", zap.String("routerID", routerID), zap.Uint32("localAS", s.cfg.LocalAS))

	if err := s.addPeer(ctx, s.cfg.PeerAddress, api.Family_AFI_IP); err != nil {
		return err
	}

	if s.cfg.PeerAddressV6 != "" {
		if err := s.addPeer(ctx, s.cfg.PeerAddressV6, api.Family_AFI_IP6); err != nil {
			return err
		}
	}

	go s.monitorPeers(ctx)

	return nil
}

func (s *Speaker) monitorPeers(ctx context.Context) {
	_ = s.server.WatchEvent(ctx, &api.WatchEventRequest{
		Peer: &api.WatchEventRequest_Peer{},
	}, func(r *api.WatchEventResponse) {
		ev := r.GetPeer()
		if ev == nil || ev.Peer == nil || ev.Peer.State == nil || ev.Peer.Conf == nil {
			return
		}
		peer := ev.Peer
		log := s.log.With(zap.String("peer", peer.Conf.NeighborAddress))
		switch {
		case peer.State.SessionState == api.PeerState_ESTABLISHED:
			s.logPeerEstablished(peer, log)
		// Only warn on IDLE for state-change events, not the initial INIT dump.
		case peer.State.SessionState == api.PeerState_IDLE &&
			ev.Type == api.WatchEventResponse_PeerEvent_STATE:
			log.Warn("BGP session down")
		}
	})
}

func (s *Speaker) logPeerEstablished(peer *api.Peer, log *zap.Logger) {
	fields := []zap.Field{}
	if gr := peer.GracefulRestart; gr != nil && gr.GetEnabled() {
		fields = append(fields,
			zap.Bool("gracefulRestart", true),
			zap.Uint32("localRestartTime", gr.GetRestartTime()),
			zap.Uint32("peerRestartTime", gr.GetPeerRestartTime()),
		)
	} else {
		fields = append(fields, zap.Bool("gracefulRestart", false))
	}
	log.Info("BGP session established", fields...)

	for _, af := range peer.AfiSafis {
		if af.Config == nil || af.Config.Family == nil || af.State == nil || !af.State.GetEnabled() {
			continue
		}
		family := af.Config.Family.Afi.String() + "/" + af.Config.Family.Safi.String()
		afFields := []zap.Field{zap.String("family", family)}
		if af.MpGracefulRestart != nil && af.MpGracefulRestart.State != nil {
			afFields = append(afFields,
				zap.Bool("grAdvertised", af.MpGracefulRestart.State.GetAdvertised()),
				zap.Bool("grReceived", af.MpGracefulRestart.State.GetReceived()),
			)
		}
		log.Info("BGP AfiSafi negotiated", afFields...)
	}
}

func (s *Speaker) addPeer(ctx context.Context, addr string, afi api.Family_Afi) error {
	afiSafi := &api.AfiSafi{
		Config: &api.AfiSafiConfig{
			Family: &api.Family{Afi: afi, Safi: api.Family_SAFI_UNICAST},
		},
	}

	peer := &api.Peer{
		Conf: &api.PeerConf{
			NeighborAddress: addr,
			PeerAsn:         s.cfg.RemoteAS,
		},
		AfiSafis: []*api.AfiSafi{afiSafi},
		Timers: &api.Timers{
			Config: &api.TimersConfig{
				ConnectRetry:      10,
				HoldTime:          90,
				KeepaliveInterval: 30,
			},
		},
	}

	if s.cfg.GracefulRestartTime > 0 {
		peer.GracefulRestart = &api.GracefulRestart{
			Enabled:             true,
			RestartTime:         s.cfg.GracefulRestartTime,
			NotificationEnabled: true,
		}
		afiSafi.MpGracefulRestart = &api.MpGracefulRestart{
			Config: &api.MpGracefulRestartConfig{Enabled: true},
		}
	}

	if err := s.server.AddPeer(ctx, &api.AddPeerRequest{Peer: peer}); err != nil {
		return fmt.Errorf("adding peer %s: %w", addr, err)
	}
	s.log.Info("BGP peer configured", zap.String("peer", addr), zap.Uint32("remoteAS", s.cfg.RemoteAS))
	return nil
}

// Stop shuts down the BGP server. All routes should be withdrawn before calling Stop.
func (s *Speaker) Stop() {
	if err := s.server.StopBgp(context.Background(), &api.StopBgpRequest{}); err != nil {
		s.log.Warn("error stopping BGP server", zap.Error(err))
	}
}

// AnnounceIPv4 announces an IPv4 prefix with the given next-hop.
func (s *Speaker) AnnounceIPv4(ctx context.Context, cidr, nexthop string) error {
	nlri, err := ipv4NLRI(cidr)
	if err != nil {
		return err
	}
	origin, _ := anypb.New(&api.OriginAttribute{Origin: 0})
	nh, _ := anypb.New(&api.NextHopAttribute{NextHop: nexthop})
	asPath, _ := anypb.New(&api.AsPathAttribute{Segments: []*api.AsSegment{
		{Type: 2, Numbers: []uint32{s.cfg.LocalAS}},
	}})
	_, err = s.server.AddPath(ctx, &api.AddPathRequest{
		Path: &api.Path{
			Family: &api.Family{Afi: api.Family_AFI_IP, Safi: api.Family_SAFI_UNICAST},
			Nlri:   nlri,
			Pattrs: []*anypb.Any{origin, nh, asPath},
		},
	})
	return err
}

// WithdrawIPv4 withdraws a previously announced IPv4 prefix.
func (s *Speaker) WithdrawIPv4(ctx context.Context, cidr, nexthop string) error {
	nlri, err := ipv4NLRI(cidr)
	if err != nil {
		return err
	}
	origin, _ := anypb.New(&api.OriginAttribute{Origin: 0})
	nh, _ := anypb.New(&api.NextHopAttribute{NextHop: nexthop})
	asPath, _ := anypb.New(&api.AsPathAttribute{Segments: []*api.AsSegment{
		{Type: 2, Numbers: []uint32{s.cfg.LocalAS}},
	}})
	return s.server.DeletePath(ctx, &api.DeletePathRequest{
		Path: &api.Path{
			Family: &api.Family{Afi: api.Family_AFI_IP, Safi: api.Family_SAFI_UNICAST},
			Nlri:   nlri,
			Pattrs: []*anypb.Any{origin, nh, asPath},
		},
	})
}

// AnnounceIPv6 announces an IPv6 prefix with the given next-hop.
// Returns an error if no IPv6 peer is configured.
func (s *Speaker) AnnounceIPv6(ctx context.Context, cidr, nexthop string) error {
	if s.cfg.PeerAddressV6 == "" {
		return fmt.Errorf("no IPv6 peer configured (set BGP_PEER_ADDRESS_V6)")
	}
	nlri, err := ipv6NLRI(cidr)
	if err != nil {
		return err
	}
	origin, _ := anypb.New(&api.OriginAttribute{Origin: 0})
	asPath, _ := anypb.New(&api.AsPathAttribute{Segments: []*api.AsSegment{
		{Type: 2, Numbers: []uint32{s.cfg.LocalAS}},
	}})
	mpReach, _ := anypb.New(&api.MpReachNLRIAttribute{
		Family:   &api.Family{Afi: api.Family_AFI_IP6, Safi: api.Family_SAFI_UNICAST},
		NextHops: []string{nexthop},
		Nlris:    []*anypb.Any{nlri},
	})
	_, err = s.server.AddPath(ctx, &api.AddPathRequest{
		Path: &api.Path{
			Family: &api.Family{Afi: api.Family_AFI_IP6, Safi: api.Family_SAFI_UNICAST},
			Nlri:   nlri,
			Pattrs: []*anypb.Any{origin, asPath, mpReach},
		},
	})
	return err
}

// WithdrawIPv6 withdraws a previously announced IPv6 prefix.
// Returns an error if no IPv6 peer is configured.
func (s *Speaker) WithdrawIPv6(ctx context.Context, cidr, nexthop string) error {
	if s.cfg.PeerAddressV6 == "" {
		return fmt.Errorf("no IPv6 peer configured (set BGP_PEER_ADDRESS_V6)")
	}
	nlri, err := ipv6NLRI(cidr)
	if err != nil {
		return err
	}
	origin, _ := anypb.New(&api.OriginAttribute{Origin: 0})
	asPath, _ := anypb.New(&api.AsPathAttribute{Segments: []*api.AsSegment{
		{Type: 2, Numbers: []uint32{s.cfg.LocalAS}},
	}})
	mpReach, _ := anypb.New(&api.MpReachNLRIAttribute{
		Family:   &api.Family{Afi: api.Family_AFI_IP6, Safi: api.Family_SAFI_UNICAST},
		NextHops: []string{nexthop},
		Nlris:    []*anypb.Any{nlri},
	})
	return s.server.DeletePath(ctx, &api.DeletePathRequest{
		Path: &api.Path{
			Family: &api.Family{Afi: api.Family_AFI_IP6, Safi: api.Family_SAFI_UNICAST},
			Nlri:   nlri,
			Pattrs: []*anypb.Any{origin, asPath, mpReach},
		},
	})
}

func ipv4NLRI(cidr string) (*anypb.Any, error) {
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("invalid CIDR %s: %w", cidr, err)
	}
	ones, _ := ipNet.Mask.Size()
	return anypb.New(&api.IPAddressPrefix{
		PrefixLen: uint32(ones),
		Prefix:    ipNet.IP.String(),
	})
}

func ipv6NLRI(cidr string) (*anypb.Any, error) {
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("invalid CIDR %s: %w", cidr, err)
	}
	ones, _ := ipNet.Mask.Size()
	return anypb.New(&api.IPAddressPrefix{
		PrefixLen: uint32(ones),
		Prefix:    ipNet.IP.String(),
	})
}
