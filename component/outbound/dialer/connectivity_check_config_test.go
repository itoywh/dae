/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2025, daeuniverse Organization <dae@v2raya.org>
 */

package dialer

import (
	"io"
	"testing"
	"time"

	"github.com/daeuniverse/outbound/protocol/direct"
	"github.com/sirupsen/logrus"
)

func TestConnectivityCheckDisabled_NoConfig(t *testing.T) {
	log := logrus.New()
	log.SetOutput(io.Discard)

	d := NewDialer(
		direct.SymmetricDirect,
		&GlobalOption{
			Log:               log,
			CheckInterval:     0,                    // Not configured
			TcpCheckOptionRaw: TcpCheckOptionRaw{},  // Empty
			CheckDnsOptionRaw: CheckDnsOptionRaw{},  // Empty
			CheckTolerance:    0,
		},
		InstanceOption{},
		&Property{},
	)
	t.Cleanup(func() { _ = d.Close() })

	// When check_interval is 0, aliveBackground should return immediately
	// without setting up any check goroutines
	d.aliveBackground()

	// Verify no check options were set up
	if len(d.TcpCheckOptionRaw.Raw) > 0 {
		t.Errorf("TcpCheckOptionRaw.Raw should be empty, got %v", d.TcpCheckOptionRaw.Raw)
	}
	if len(d.CheckDnsOptionRaw.Raw) > 0 {
		t.Errorf("CheckDnsOptionRaw.Raw should be empty, got %v", d.CheckDnsOptionRaw.Raw)
	}
	if d.CheckInterval != 0 {
		t.Errorf("CheckInterval should be 0, got %v", d.CheckInterval)
	}
}

func TestConnectivityCheckDisabled_EmptyUrls(t *testing.T) {
	log := logrus.New()
	log.SetOutput(io.Discard)

	d := NewDialer(
		direct.SymmetricDirect,
		&GlobalOption{
			Log:               log,
			CheckInterval:     30 * time.Second,     // Configured
			TcpCheckOptionRaw: TcpCheckOptionRaw{},  // Empty Raw
			CheckDnsOptionRaw: CheckDnsOptionRaw{},  // Empty Raw
			CheckTolerance:    0,
		},
		InstanceOption{},
		&Property{},
	)
	t.Cleanup(func() { _ = d.Close() })

	// aliveBackground only checks CheckInterval == 0; it does NOT check
	// for empty Raw fields. When CheckInterval > 0, goroutines are started
	// even with empty Raw — they will fail when Option() is called.
	// The Raw field itself is not modified by aliveBackground.
	d.aliveBackground()

	// Raw fields remain as-was (empty in this case)
	if len(d.TcpCheckOptionRaw.Raw) > 0 {
		t.Errorf("TcpCheckOptionRaw.Raw should be empty, got %v", d.TcpCheckOptionRaw.Raw)
	}
	if len(d.CheckDnsOptionRaw.Raw) > 0 {
		t.Errorf("CheckDnsOptionRaw.Raw should be empty, got %v", d.CheckDnsOptionRaw.Raw)
	}
}

func TestConnectivityCheckEnabled_TcpOnly(t *testing.T) {
	log := logrus.New()
	log.SetOutput(io.Discard)

	d := NewDialer(
		direct.SymmetricDirect,
		&GlobalOption{
			Log:               log,
			CheckInterval:     30 * time.Second,
			TcpCheckOptionRaw: TcpCheckOptionRaw{Raw: []string{"http://cp.cloudflare.com"}},
			CheckDnsOptionRaw: CheckDnsOptionRaw{}, // Not configured
			CheckTolerance:    0,
		},
		InstanceOption{},
		&Property{},
	)
	t.Cleanup(func() { _ = d.Close() })

	d.aliveBackground()

	// TCP check should be enabled, UDP check should be disabled
	if len(d.TcpCheckOptionRaw.Raw) == 0 {
		t.Error("TcpCheckOptionRaw.Raw should not be empty")
	}
	if len(d.CheckDnsOptionRaw.Raw) > 0 {
		t.Errorf("CheckDnsOptionRaw.Raw should be empty, got %v", d.CheckDnsOptionRaw.Raw)
	}
}

func TestConnectivityCheckEnabled_UdpOnly(t *testing.T) {
	log := logrus.New()
	log.SetOutput(io.Discard)

	d := NewDialer(
		direct.SymmetricDirect,
		&GlobalOption{
			Log:               log,
			CheckInterval:     30 * time.Second,
			TcpCheckOptionRaw: TcpCheckOptionRaw{}, // Not configured
			CheckDnsOptionRaw: CheckDnsOptionRaw{Raw: []string{"8.8.8.8"}},
			CheckTolerance:    0,
		},
		InstanceOption{},
		&Property{},
	)
	t.Cleanup(func() { _ = d.Close() })

	d.aliveBackground()

	// UDP check should be enabled, TCP check should be disabled
	if len(d.TcpCheckOptionRaw.Raw) > 0 {
		t.Errorf("TcpCheckOptionRaw.Raw should be empty, got %v", d.TcpCheckOptionRaw.Raw)
	}
	if len(d.CheckDnsOptionRaw.Raw) == 0 {
		t.Error("CheckDnsOptionRaw.Raw should not be empty")
	}
}

func TestConnectivityCheck_Ipv4Only(t *testing.T) {
	// Test TCP IPv4 only
	tcpRaw := []string{"http://cp.cloudflare.com", "1.1.1.1"}
	if !shouldSkipIpFamily6(tcpRaw) {
		t.Error("shouldSkipIpFamily6 should return true for IPv4-only tcp_check_url")
	}

	// Test UDP IPv4 only
	udpRaw := []string{"dns.google:53", "8.8.8.8"}
	if !shouldSkipIpFamily6(udpRaw) {
		t.Error("shouldSkipIpFamily6 should return true for IPv4-only udp_check_dns")
	}
}

func TestConnectivityCheck_WithIpv6(t *testing.T) {
	// Test TCP with IPv6
	tcpRaw := []string{"http://cp.cloudflare.com", "2606:4700:4700::1111"}
	if shouldSkipIpFamily6(tcpRaw) {
		t.Error("shouldSkipIpFamily6 should return false when IPv6 is present")
	}

	// Test UDP with IPv6
	udpRaw := []string{"dns.google:53", "2001:4860:4860::8888"}
	if shouldSkipIpFamily6(udpRaw) {
		t.Error("shouldSkipIpFamily6 should return false when IPv6 is present")
	}
}
