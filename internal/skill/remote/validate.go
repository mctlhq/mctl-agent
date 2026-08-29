// Copyright 2025 MCTL Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package remote

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"time"

	"github.com/mctlhq/mctl-agent/internal/skill"
)

// cgnatBlock is the Carrier-Grade NAT range (RFC 6598), not covered by any
// net.IP helper method.
var cgnatBlock = mustParseCIDR("100.64.0.0/10")

func mustParseCIDR(s string) *net.IPNet {
	_, block, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return block
}

// isDeniedIP reports whether ip falls in a loopback, private (RFC 1918 /
// fc00::/7), link-local, or CGNAT range. Shared between ValidateRegistration
// (registration-time) and the connection-time dialer guard so the two never
// drift apart.
func isDeniedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() ||
		cgnatBlock.Contains(ip)
}

// resolveHost returns every IP the given host resolves to. If host is
// already a literal IP address, it is returned as-is (no DNS lookup).
func resolveHost(ctx context.Context, host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, nil
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	ips := make([]net.IP, len(addrs))
	for i, a := range addrs {
		ips[i] = a.IP
	}
	return ips, nil
}

// ValidateRegistration checks that reg is safe to register: the endpoint
// must be an https:// URL whose host is not a literal or DNS-resolved
// loopback/private/link-local/CGNAT address, and every declared capability
// must be a known skill.CapabilityID.
//
// DNS resolution here is best-effort and registration-time only — see
// remote.New's guardedDialer for the connection-time check that also covers
// a host re-pointed via DNS rebinding after registration.
func ValidateRegistration(reg Registration) error {
	u, err := url.Parse(reg.Endpoint)
	if err != nil {
		return fmt.Errorf("invalid endpoint URL: %w", err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("endpoint must use https, got scheme %q", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("endpoint must have a host")
	}

	// Bounded lookup: Register is reached from an HTTP handler, and an
	// endpoint pointed at a tarpit DNS server must not be able to pin the
	// handler goroutine indefinitely.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ips, err := resolveHost(ctx, host)
	if err != nil {
		return fmt.Errorf("resolving endpoint host %q: %w", host, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("endpoint host %q did not resolve to any address", host)
	}
	for _, ip := range ips {
		if isDeniedIP(ip) {
			return fmt.Errorf("endpoint host %q resolves to a disallowed address (%s)", host, ip)
		}
	}

	known := make(map[string]bool, len(skill.AllCapabilityIDs()))
	for _, c := range skill.AllCapabilityIDs() {
		known[string(c)] = true
	}
	for _, c := range reg.Capabilities {
		if !known[c] {
			return fmt.Errorf("unknown capability %q", c)
		}
	}

	return nil
}
