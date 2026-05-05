package speaker

import (
	"testing"
)

func TestIPv4NLRI_Valid(t *testing.T) {
	nlri, err := ipv4NLRI("192.0.2.0/24")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if nlri == nil {
		t.Fatal("expected non-nil nlri")
	}
}

func TestIPv4NLRI_Invalid(t *testing.T) {
	_, err := ipv4NLRI("not-a-cidr")
	if err == nil {
		t.Error("expected error for invalid CIDR, got nil")
	}
}

func TestIPv6NLRI_Valid(t *testing.T) {
	nlri, err := ipv6NLRI("fc00::/64")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if nlri == nil {
		t.Fatal("expected non-nil nlri")
	}
}

func TestIPv6NLRI_Invalid(t *testing.T) {
	_, err := ipv6NLRI("not-a-cidr")
	if err == nil {
		t.Error("expected error for invalid CIDR, got nil")
	}
}

func TestIPv4NLRI_HostBitsCleared(t *testing.T) {
	// net.ParseCIDR masks host bits, so 192.0.2.1/24 becomes 192.0.2.0/24.
	_, err := ipv4NLRI("192.0.2.1/24")
	if err != nil {
		t.Errorf("unexpected error for host-bit CIDR: %v", err)
	}
}
