package config_test

import (
	"testing"

	"github.com/bgp-injector/bgp-injector/pkg/config"
)

var defaults = config.Defaults{GateOnReady: true}

func TestParseFromAnnotations_UsesDefaults(t *testing.T) {
	ann := map[string]string{
		config.AnnotationIPv4Prefixes: `["1.0.0.0/24"]`,
	}
	cfg, err := config.ParseFromAnnotations(ann, defaults)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.GateOnReady {
		t.Error("GateOnReady: got false, want true")
	}
	if len(cfg.IPv4Prefixes) != 1 || cfg.IPv4Prefixes[0] != "1.0.0.0/24" {
		t.Errorf("IPv4Prefixes: got %v, want [1.0.0.0/24]", cfg.IPv4Prefixes)
	}
}

func TestParseFromAnnotations_AnnotationsOverrideDefaults(t *testing.T) {
	ann := map[string]string{
		config.AnnotationIPv4Prefixes: `["2.0.0.0/24","3.0.0.0/24"]`,
		config.AnnotationIPv6Prefixes: `["fc00::/64"]`,
		config.AnnotationGateOnReady:  "false",
	}
	cfg, err := config.ParseFromAnnotations(ann, defaults)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GateOnReady {
		t.Error("GateOnReady: got true, want false")
	}
	if len(cfg.IPv4Prefixes) != 2 {
		t.Errorf("IPv4Prefixes len: got %d, want 2", len(cfg.IPv4Prefixes))
	}
	if len(cfg.IPv6Prefixes) != 1 || cfg.IPv6Prefixes[0] != "fc00::/64" {
		t.Errorf("IPv6Prefixes: got %v, want [fc00::/64]", cfg.IPv6Prefixes)
	}
}

func TestParseFromAnnotations_NoPrefixes_HasPrefixesFalse(t *testing.T) {
	cfg, err := config.ParseFromAnnotations(map[string]string{}, defaults)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HasPrefixes() {
		t.Error("HasPrefixes: got true, want false for empty annotations")
	}
}

func TestParseFromAnnotations_InvalidJSON(t *testing.T) {
	ann := map[string]string{
		config.AnnotationIPv4Prefixes: `not-json`,
	}
	_, err := config.ParseFromAnnotations(ann, defaults)
	if err == nil {
		t.Error("expected error for invalid JSON, got nil")
	}
}

func TestParseFromAnnotations_InvalidGateOnReady(t *testing.T) {
	ann := map[string]string{
		config.AnnotationIPv4Prefixes: `["1.0.0.0/24"]`,
		config.AnnotationGateOnReady:  "yes-please",
	}
	_, err := config.ParseFromAnnotations(ann, defaults)
	if err == nil {
		t.Error("expected error for invalid gateOnReady, got nil")
	}
}
