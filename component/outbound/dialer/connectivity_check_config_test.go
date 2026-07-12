/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2025, daeuniverse Organization <dae@v2raya.org>
 */

package dialer

import (
	"io"
	"testing"
	"time"

	"github.com/daeuniverse/dae/common/consts"
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
			CheckInterval:     0,                   // Not configured
			TcpCheckOptionRaw: TcpCheckOptionRaw{}, // Empty
			CheckDnsOptionRaw: CheckDnsOptionRaw{}, // Empty
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
			CheckInterval:     30 * time.Second,    // Configured
			TcpCheckOptionRaw: TcpCheckOptionRaw{}, // Empty Raw
			CheckDnsOptionRaw: CheckDnsOptionRaw{}, // Empty Raw
			CheckTolerance:    0,
		},
		InstanceOption{},
		&Property{},
	)
	t.Cleanup(func() { _ = d.Close() })

	// aliveBackground checks both CheckInterval (must be > 0) and the Raw
	// fields (at least one of tcp_check_url / udp_check_dns must be set).
	// With a non-zero CheckInterval but empty Raw, it builds no CheckOpts and
	// returns early ("neither ... configured"), so no check goroutine is
	// started and the Raw fields are left untouched. This verifies the opt-in
	// design: omitting health-check URLs disables checking entirely.
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

// TestConnectivityCheck_GatingHonorsAliveSetsAndIpFamily covers the two gating
// layers that the plain TcpOnly/UdpOnly tests could not reach: a dialer without
// any AliveDialerSet is treated as "unused" by checkUnused() and the whole
// check path short-circuits, so the IPv4/IPv6 three-state gating is never
// exercised. Here we register a real AliveDialerSet and assert both that the
// dialer is now considered used and that an IPv4-only URL skips the IPv6 check.
//
// Note: we deliberately call the gating predicates directly rather than
// aliveBackground(), because aliveBackground() blocks on its probe ticker once
// the dialer is "used".
func TestConnectivityCheck_GatingHonorsAliveSetsAndIpFamily(t *testing.T) {
	log := logrus.New()
	log.SetOutput(io.Discard)
	nt := newTestNetworkType()

	d := NewDialer(
		direct.SymmetricDirect,
		&GlobalOption{
			Log:               log,
			CheckInterval:     30 * time.Second,
			TcpCheckOptionRaw: TcpCheckOptionRaw{Raw: []string{"1.1.1.1"}},
		},
		InstanceOption{},
		&Property{},
	)
	t.Cleanup(func() { _ = d.Close() })

	// Before registration the dialer is "unused" and checkUnused() would
	// short-circuit before any gating decision.
	if d.hasAliveDialerSets(nt) {
		t.Fatal("expected no alive dialer sets before registration")
	}

	set := NewAliveDialerSet(log, "g", nt, 0, consts.DialerSelectionPolicy_Random,
		[]*Dialer{d}, []*Annotation{{}}, func(bool) {}, true)
	d.RegisterAliveDialerSet(set)
	t.Cleanup(func() { d.UnregisterAliveDialerSet(set) })

	if !d.hasAliveDialerSets(nt) {
		t.Fatal("expected an alive dialer set after registration")
	}

	// Three-state gating: IPv4-only URL must skip the IPv6 probe, while a
	// mixed IPv4/IPv6 URL must keep both.
	if !shouldSkipIpFamily6([]string{"1.1.1.1"}) {
		t.Error("IPv4-only tcp_check_url should skip the IPv6 check")
	}
	if shouldSkipIpFamily6([]string{"1.1.1.1", "2606:4700:4700::1111"}) {
		t.Error("mixed IPv4/IPv6 tcp_check_url should NOT skip the IPv6 check")
	}
}

// TestDialer_MarkUnavailableFlipsMustGetAlive verifies that the real Dialer
// health API (markUnavailable/markAvailable) actually flips Dialer.MustGetAlive
// by mutating the real collection — not a mock field. This closes the gap where
// fixed_fallback unit tests used a mockDialer whose SetAlive only touched the
// mock's own field, so the death path was never truly exercised against a real
// Dialer collection.
func TestDialer_MarkUnavailableFlipsMustGetAlive(t *testing.T) {
	log := logrus.New()
	log.SetOutput(io.Discard)
	d := NewDialer(
		direct.SymmetricDirect,
		&GlobalOption{Log: log, CheckInterval: 0},
		InstanceOption{},
		&Property{},
	)
	t.Cleanup(func() { _ = d.Close() })

	nt := newTestNetworkType()
	if !d.MustGetAlive(nt) {
		t.Fatal("a freshly created dialer should be alive")
	}

	d.markUnavailable(nt)
	if d.MustGetAlive(nt) {
		t.Error("markUnavailable should flip MustGetAlive to false")
	}

	d.markAvailable(nt, 10*time.Millisecond)
	if !d.MustGetAlive(nt) {
		t.Error("markAvailable should flip MustGetAlive back to true")
	}
}
