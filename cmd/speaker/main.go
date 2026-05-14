package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"go.uber.org/zap"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/bgp-injector/bgp-injector/pkg/config"
	"github.com/bgp-injector/bgp-injector/pkg/speaker"
)

func main() {
	log, _ := zap.NewProduction()
	defer log.Sync() //nolint:errcheck

	spkCfg, defaults, gracefulRestart, err := configFromEnv()
	if err != nil {
		log.Fatal("invalid configuration", zap.Error(err))
	}

	nodeName := os.Getenv("NODE_NAME")
	if nodeName == "" {
		log.Fatal("NODE_NAME is required")
	}

	restCfg, err := rest.InClusterConfig()
	if err != nil {
		log.Fatal("building in-cluster config", zap.Error(err))
	}
	k8s, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		log.Fatal("building kubernetes client", zap.Error(err))
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	spk := speaker.New(spkCfg, log)
	if err := spk.Start(ctx); err != nil {
		log.Fatal("starting BGP speaker", zap.Error(err))
	}

	go spk.ServeHealth(ctx, 8080)

	watcher := speaker.NewWatcher(spk, defaults, nodeName, k8s, log, gracefulRestart)
	watcher.Run(ctx)

	spk.Stop()
}

func configFromEnv() (*speaker.Config, config.Defaults, bool, error) {
	localAS, err := parseUint32Env("BGP_LOCAL_AS")
	if err != nil {
		return nil, config.Defaults{}, false, err
	}
	remoteAS, err := parseUint32Env("BGP_REMOTE_AS")
	if err != nil {
		return nil, config.Defaults{}, false, err
	}
	peerAddress := os.Getenv("BGP_PEER_ADDRESS")
	if peerAddress == "" {
		return nil, config.Defaults{}, false, fmt.Errorf("BGP_PEER_ADDRESS is required")
	}

	gateOnReady := true
	if v := os.Getenv("BGP_GATE_ON_READY"); v != "" {
		gateOnReady, err = strconv.ParseBool(v)
		if err != nil {
			return nil, config.Defaults{}, false, fmt.Errorf("parsing BGP_GATE_ON_READY: %w", err)
		}
	}

	var gracefulRestartTime uint32
	if v := os.Getenv("BGP_GRACEFUL_RESTART_TIME"); v != "" {
		gracefulRestartTime, err = parseUint32Env("BGP_GRACEFUL_RESTART_TIME")
		if err != nil {
			return nil, config.Defaults{}, false, err
		}
	}

	return &speaker.Config{
		LocalAS:             localAS,
		RemoteAS:            remoteAS,
		PeerAddress:         peerAddress,
		PeerAddressV6:       os.Getenv("BGP_PEER_ADDRESS_V6"),
		RouterID:            os.Getenv("BGP_ROUTER_ID"),
		GracefulRestartTime: gracefulRestartTime,
	}, config.Defaults{GateOnReady: gateOnReady}, gracefulRestartTime > 0, nil
}

func parseUint32Env(key string) (uint32, error) {
	v := os.Getenv(key)
	if v == "" {
		return 0, fmt.Errorf("%s is required", key)
	}
	n, err := strconv.ParseUint(v, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("parsing %s: %w", key, err)
	}
	return uint32(n), nil
}
