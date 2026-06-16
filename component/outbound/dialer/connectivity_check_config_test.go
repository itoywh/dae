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

// TestConnectivityCheckDisabled_NoConfig tests that connectivity check is skipped
// when no check_interval, tcp_check_url, or udp_check_dns is configured.
func TestConnectivityCheckDisabled_NoConfig(t *testing.T) {
	log := logrus.New()
	log.SetOutput(io.Discard)

	d := NewDialer(
		direct.SymmetricDirect,
		&GlobalOption{
			Log:            log,
			CheckInterval:  0,                    // Not configured
			TcpCheckUrl:    nil,                  // Not configured
			UdpCheckDns:    nil,                  // Not configured
			CheckTolerance: 0,
		},
		InstanceOption{},
		&Property{Name: "test-dialer"},
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

// TestConnectivityCheckDisabled_EmptyUrls tests that connectivity check is skipped
// when urls are empty (even if check_interval is set).
func TestConnectivityCheckDisabled_EmptyUrls(t *testing.T) {
	log := logrus.New()
	log.SetOutput(io.Discard)

	d := NewDialer(
		direct.SymmetricDirect,
		&GlobalOption{
			Log:            log,
			CheckInterval:  30 * time.Second,    // Configured
			TcpCheckUrl:    []string{},          // Empty
			UdpCheckDns:    []string{},          // Empty
			CheckTolerance: 0,
		},
		InstanceOption{},
		&Property{Name: "test-dialer"},
	)
	t.Cleanup(func() { _ = d.Close() })

	// When both TcpCheckUrl and UdpCheckDns are empty, aliveBackground should return
	d.aliveBackground()

	// Verify check options are empty
	if len(d.TcpCheckOptionRaw.Raw) > 0 {
		t.Errorf("TcpCheckOptionRaw.Raw should be empty, got %v", d.TcpCheckOptionRaw.Raw)
	}
	if len(d.CheckDnsOptionRaw.Raw) > 0 {
		t.Errorf("CheckDnsOptionRaw.Raw should be empty, got %v", d.CheckDnsOptionRaw.Raw)
	}
}

// TestConnectivityCheckEnabled_TcpOnly tests that only TCP check is enabled
// when only tcp_check_url is configured.
func TestConnectivityCheckEnabled_TcpOnly(t *testing.T) {
	log := logrus.New()
	log.SetOutput(io.Discard)

	d := NewDialer(
		direct.SymmetricDirect,
		&GlobalOption{
			Log:            log,
			CheckInterval:  30 * time.Second,
			TcpCheckUrl:    []string{"http://cp.cloudflare.com"},
			UdpCheckDns:    nil,                  // Not configured
			CheckTolerance: 0,
		},
		InstanceOption{},
		&Property{Name: "test-dialer"},
	)
	t.Cleanup(func() { _ = d.Close() })

	// TCP check should be enabled, UDP check should be disabled
	if len(d.TcpCheckOptionRaw.Raw) == 0 {
		t.Error("TcpCheckOptionRaw.Raw should not be empty")
	}
	if len(d.CheckDnsOptionRaw.Raw) > 0 {
		t.Errorf("CheckDnsOptionRaw.Raw should be empty, got %v", d.CheckDnsOptionRaw.Raw)
	}
}

// TestConnectivityCheckEnabled_UdpOnly tests that only UDP check is enabled
// when only udp_check_dns is configured.
func TestConnectivityCheckEnabled_UdpOnly(t *testing.T) {
	log := logrus.New()
	log.SetOutput(io.Discard)

	d := NewDialer(
		direct.SymmetricDirect,
		&GlobalOption{
			Log:            log,
			CheckInterval:  30 * time.Second,
			TcpCheckUrl:    nil,                  // Not configured
			UdpCheckDns:    []string{"8.8.8.8"},
			CheckTolerance: 0,
		},
		InstanceOption{},
		&Property{Name: "test-dialer"},
	)
	t.Cleanup(func() { _ = d.Close() })

	// UDP check should be enabled, TCP check should be disabled
	if len(d.TcpCheckOptionRaw.Raw) > 0 {
		t.Errorf("TcpCheckOptionRaw.Raw should be empty, got %v", d.TcpCheckOptionRaw.Raw)
	}
	if len(d.CheckDnsOptionRaw.Raw) == 0 {
		t.Error("CheckDnsOptionRaw.Raw should not be empty")
	}
}

// TestConnectivityCheck_Ipv4Only tests that only IPv4 check is performed
// when urls contain only IPv4 addresses.
func TestConnectivityCheck_Ipv4Only(t *testing.T) {
	// Test TCP IPv4 only
	tcpRaw := []string{"http://cp.cloudflare.com", "1.1.1.1"}
	if !shouldSkipTcp6Probes(tcpRaw) {
		t.Error("shouldSkipTcp6Probes should return true for IPv4-only tcp_check_url")
	}

	// Test UDP IPv4 only
	udpRaw := []string{"8.8.8.8"}
	if !shouldSkipUdp6Probes(udpRaw) {
		t.Error("shouldSkipUdp6Probes should return true for IPv4-only udp_check_dns")
	}
}

// TestConnectivityCheck_WithIpv6 tests that IPv6 check is performed
// when urls contain IPv6 addresses.
func TestConnectivityCheck_WithIpv6(t *testing.T) {
	// Test TCP with IPv6
	tcpRaw := []string{"http://cp.cloudflare.com", "2606:4700:4700::1111"}
	if shouldSkipTcp6Probes(tcpRaw) {
		t.Error("shouldSkipTcp6Probes should return false when IPv6 is present")
	}

	// Test UDP with IPv6
	udpRaw := []string{"2001:4860:4860::8888"}
	if shouldSkipUdp6Probes(udpRaw) {
		t.Error("shouldSkipUdp6Probes should return false when IPv6 is present")
	}
}