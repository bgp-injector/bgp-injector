package config

import (
	"encoding/json"
	"fmt"
	"strconv"
)

const (
	AnnotationPrefix = "bgp-injector.github.io"

	AnnotationIPv4Prefixes = AnnotationPrefix + "/routedIPv4Prefixes"
	AnnotationIPv6Prefixes = AnnotationPrefix + "/routedIPv6Prefixes"
	AnnotationGateOnReady  = AnnotationPrefix + "/gateOnReady"
)

// Defaults holds cluster-wide defaults configurable via the DaemonSet env vars.
type Defaults struct {
	GateOnReady bool
}

// PodBGPConfig is the resolved BGP config for a pod after merging annotations with defaults.
type PodBGPConfig struct {
	IPv4Prefixes []string
	IPv6Prefixes []string
	GateOnReady  bool
}

// ParseFromAnnotations extracts BGP config from pod annotations, falling back to defaults.
func ParseFromAnnotations(annotations map[string]string, defaults Defaults) (*PodBGPConfig, error) {
	cfg := &PodBGPConfig{
		GateOnReady: defaults.GateOnReady,
	}

	if v, ok := annotations[AnnotationIPv4Prefixes]; ok {
		if err := json.Unmarshal([]byte(v), &cfg.IPv4Prefixes); err != nil {
			return nil, fmt.Errorf("parsing %s: %w", AnnotationIPv4Prefixes, err)
		}
	}

	if v, ok := annotations[AnnotationIPv6Prefixes]; ok {
		if err := json.Unmarshal([]byte(v), &cfg.IPv6Prefixes); err != nil {
			return nil, fmt.Errorf("parsing %s: %w", AnnotationIPv6Prefixes, err)
		}
	}

	if v, ok := annotations[AnnotationGateOnReady]; ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", AnnotationGateOnReady, err)
		}
		cfg.GateOnReady = b
	}

	return cfg, nil
}

// HasPrefixes returns true if any prefixes are configured.
func (c *PodBGPConfig) HasPrefixes() bool {
	return len(c.IPv4Prefixes) > 0 || len(c.IPv6Prefixes) > 0
}
