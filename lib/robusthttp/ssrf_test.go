package robusthttp

import (
	"net/http"
	"net/netip"
	"testing"
)

func TestValidateClientFetchURL_Default(t *testing.T) {
	policy := DefaultSSRFPolicy()

	// Public URL should pass.
	u, err := ValidateClientFetchURL("https://example.com/file.car", nil, &policy)
	if err != nil {
		t.Fatalf("expected public URL to pass: %v", err)
	}
	if u.Hostname() != "example.com" {
		t.Fatalf("unexpected host: %s", u.Hostname())
	}

	// Private IP literal should be rejected.
	_, err = ValidateClientFetchURL("http://192.168.1.100/file.car", nil, &policy)
	if err == nil {
		t.Fatal("expected private IP to be rejected")
	}

	// Loopback should be rejected.
	_, err = ValidateClientFetchURL("http://127.0.0.1/file.car", nil, &policy)
	if err == nil {
		t.Fatal("expected loopback to be rejected")
	}

	// Localhost hostname should be rejected.
	_, err = ValidateClientFetchURL("http://localhost/file.car", nil, &policy)
	if err == nil {
		t.Fatal("expected localhost to be rejected")
	}
}

func TestValidateClientFetchURL_Disabled(t *testing.T) {
	policy := DefaultSSRFPolicy()
	policy.Disabled = true

	// Private IP should pass when disabled.
	_, err := ValidateClientFetchURL("http://192.168.1.100/file.car", nil, &policy)
	if err != nil {
		t.Fatalf("expected private IP to pass when disabled: %v", err)
	}

	// Loopback should pass when disabled.
	_, err = ValidateClientFetchURL("http://127.0.0.1/file.car", nil, &policy)
	if err != nil {
		t.Fatalf("expected loopback to pass when disabled: %v", err)
	}

	// Localhost should pass when disabled.
	_, err = ValidateClientFetchURL("http://localhost/file.car", nil, &policy)
	if err != nil {
		t.Fatalf("expected localhost to pass when disabled: %v", err)
	}

	// Structural checks still enforced: bad scheme rejected.
	_, err = ValidateClientFetchURL("ftp://localhost/file.car", nil, &policy)
	if err == nil {
		t.Fatal("expected ftp scheme to be rejected even when disabled")
	}

	// Control characters still rejected.
	_, err = ValidateClientFetchURL("http://localhost/file\x00.car", nil, &policy)
	if err == nil {
		t.Fatal("expected control character to be rejected even when disabled")
	}
}

func TestValidateClientFetchURL_AllowedHosts(t *testing.T) {
	policy := DefaultSSRFPolicy()
	policy.AllowedHosts = []string{"192.168.1.100", "my-server.local:8080"}

	// Allowed host (any port) should pass.
	_, err := ValidateClientFetchURL("http://192.168.1.100/file.car", nil, &policy)
	if err != nil {
		t.Fatalf("expected allowed host to pass: %v", err)
	}

	// Allowed host on different port should also pass (entry has no port).
	_, err = ValidateClientFetchURL("http://192.168.1.100:9999/file.car", nil, &policy)
	if err != nil {
		t.Fatalf("expected allowed host on any port to pass: %v", err)
	}

	// Allowed host:port should pass.
	_, err = ValidateClientFetchURL("http://my-server.local:8080/file.car", nil, &policy)
	if err != nil {
		t.Fatalf("expected allowed host:port to pass: %v", err)
	}

	// Same host, wrong port should be rejected (entry specifies port).
	_, err = ValidateClientFetchURL("http://my-server.local:9999/file.car", nil, &policy)
	if err == nil {
		t.Fatal("expected non-matching port to be rejected")
	}

	// Non-allowed private IP should still be rejected.
	_, err = ValidateClientFetchURL("http://10.0.0.1/file.car", nil, &policy)
	if err == nil {
		t.Fatal("expected non-allowed private IP to be rejected")
	}
}

func TestValidateClientFetchURL_AllowedHostsCaseInsensitive(t *testing.T) {
	policy := DefaultSSRFPolicy()
	policy.AllowedHosts = []string{"My-Server.Local"}

	_, err := ValidateClientFetchURL("http://my-server.local/file.car", nil, &policy)
	if err != nil {
		t.Fatalf("expected case-insensitive match to pass: %v", err)
	}
}

func TestValidateFetchIP_AllowPrivateIPs(t *testing.T) {
	policy := DefaultSSRFPolicy()
	policy.AllowPrivateIPs = true

	tests := []struct {
		ip      string
		allowed bool
	}{
		{"192.168.1.100", true},
		{"10.0.0.1", true},
		{"172.16.0.1", true},
		{"169.254.1.1", true},  // link-local
		{"100.64.0.1", true},   // CGNAT
		{"198.18.0.1", true},   // benchmarking
		{"192.0.2.1", true},    // documentation
		{"203.0.113.1", true},  // documentation
		{"198.51.100.1", true}, // documentation
		{"240.0.0.1", true},    // reserved
		{"8.8.8.8", true},      // public — always allowed
		{"127.0.0.1", false},   // loopback needs AllowLoopbackIPs
		{"0.0.0.0", false},     // unspecified
		{"224.0.0.1", false},   // multicast
	}

	for _, tt := range tests {
		ip := netip.MustParseAddr(tt.ip)
		err := validateFetchIP(ip, &policy)
		if tt.allowed && err != nil {
			t.Errorf("AllowPrivateIPs: expected %s to be allowed, got: %v", tt.ip, err)
		}
		if !tt.allowed && err == nil {
			t.Errorf("AllowPrivateIPs: expected %s to be blocked", tt.ip)
		}
	}
}

func TestValidateFetchIP_AllowPrivateAndLoopback(t *testing.T) {
	policy := DefaultSSRFPolicy()
	policy.AllowPrivateIPs = true
	policy.AllowLoopbackIPs = true

	err := validateFetchIP(netip.MustParseAddr("127.0.0.1"), &policy)
	if err != nil {
		t.Fatalf("expected loopback to be allowed: %v", err)
	}

	err = validateFetchIP(netip.MustParseAddr("10.0.0.1"), &policy)
	if err != nil {
		t.Fatalf("expected private to be allowed: %v", err)
	}
}

func TestValidateFetchIP_DefaultBlocksPrivate(t *testing.T) {
	policy := DefaultSSRFPolicy()

	privateIPs := []string{
		"192.168.1.1",
		"10.0.0.1",
		"172.16.0.1",
		"169.254.1.1",
		"100.64.0.1",
		"198.18.0.1",
	}

	for _, s := range privateIPs {
		ip := netip.MustParseAddr(s)
		if err := validateFetchIP(ip, &policy); err == nil {
			t.Errorf("default policy should block %s", s)
		}
	}
}

func TestGlobalSSRFOverride(t *testing.T) {
	// Ensure we restore nil after the test.
	defer SetGlobalSSRFOverride(nil)

	// With no override, private IPs are blocked (nil policy -> defaults).
	_, err := ValidateClientFetchURL("http://192.168.1.100/file.car", nil, nil)
	if err == nil {
		t.Fatal("expected private IP to be rejected with nil policy and no override")
	}

	// Install a global override that disables SSRF.
	SetGlobalSSRFOverride(&SSRFPolicy{Disabled: true})

	_, err = ValidateClientFetchURL("http://192.168.1.100/file.car", nil, nil)
	if err != nil {
		t.Fatalf("expected private IP to pass with disabled override: %v", err)
	}

	// Explicit non-nil policy should NOT be affected by global override.
	strict := DefaultSSRFPolicy()
	_, err = ValidateClientFetchURL("http://192.168.1.100/file.car", nil, &strict)
	if err == nil {
		t.Fatal("expected private IP to be rejected with explicit strict policy")
	}

	// Clear override — back to blocking.
	SetGlobalSSRFOverride(nil)
	_, err = ValidateClientFetchURL("http://192.168.1.100/file.car", nil, nil)
	if err == nil {
		t.Fatal("expected private IP to be rejected after clearing override")
	}
}

func TestGlobalSSRFOverride_AllowedHosts(t *testing.T) {
	defer SetGlobalSSRFOverride(nil)

	SetGlobalSSRFOverride(&SSRFPolicy{
		AllowedHosts: []string{"192.168.1.100:8080"},
	})

	// Matching host:port passes.
	_, err := ValidateClientFetchURL("http://192.168.1.100:8080/file.car", nil, nil)
	if err != nil {
		t.Fatalf("expected allowed host:port to pass via override: %v", err)
	}

	// Different port is still blocked.
	_, err = ValidateClientFetchURL("http://192.168.1.100:9090/file.car", nil, nil)
	if err == nil {
		t.Fatal("expected non-matching port to be rejected via override")
	}
}

func TestIsHostAllowed(t *testing.T) {
	tests := []struct {
		host    string
		port    string
		allowed []string
		want    bool
	}{
		{"192.168.1.100", "80", []string{"192.168.1.100"}, true},
		{"192.168.1.100", "9999", []string{"192.168.1.100"}, true},
		{"192.168.1.100", "8080", []string{"192.168.1.100:8080"}, true},
		{"192.168.1.100", "80", []string{"192.168.1.100:8080"}, false},
		{"10.0.0.1", "80", []string{"192.168.1.100"}, false},
		{"My-Host", "80", []string{"my-host"}, true},
		{"my-host", "80", []string{"My-Host"}, true},
		{"host", "80", nil, false},
		{"host", "80", []string{}, false},
	}

	for _, tt := range tests {
		got := isHostAllowed(tt.host, tt.port, tt.allowed)
		if got != tt.want {
			t.Errorf("isHostAllowed(%q, %q, %v) = %v, want %v", tt.host, tt.port, tt.allowed, got, tt.want)
		}
	}
}

func TestValidateClientFetchURL_HeadersStillCheckedWithAllowedHost(t *testing.T) {
	policy := DefaultSSRFPolicy()
	policy.AllowedHosts = []string{"192.168.1.100"}

	// Host header is blocked even for allowed hosts.
	headers := http.Header{"Host": []string{"evil.com"}}
	_, err := ValidateClientFetchURL("http://192.168.1.100/file.car", headers, &policy)
	if err == nil {
		t.Fatal("expected blocked header to be rejected even for allowed hosts")
	}
}
