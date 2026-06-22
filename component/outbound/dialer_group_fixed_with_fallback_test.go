/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2025, daeuniverse Organization <dae@v2raya.org>
 */

package outbound

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daeuniverse/dae/common/consts"
	"github.com/daeuniverse/dae/component/outbound/dialer"
	"github.com/daeuniverse/outbound/netproxy"
)

// mockDialer implements a simple mock dialer for testing FixedWithFallback.
type mockDialer struct {
	index      int
	alive      atomic.Bool
	name       string
}

func (m *mockDialer) DialContext(ctx context.Context, network, addr string) (netproxy.Conn, error) {
	return nil, errors.New("not implemented")
}

func (m *mockDialer) Property() *dialer.Property {
	return &dialer.Property{Name: m.name}
}

func (m *mockDialer) IsAlive(nt *dialer.NetworkType) bool {
	return m.alive.Load()
}

func (m *mockDialer) MustGetAlive(nt *dialer.NetworkType) bool {
	return m.alive.Load()
}

func (m *mockDialer) SetAlive(alive bool) {
	m.alive.Store(alive)
}

// TestFixedWithFallback_NodeAlive tests that fixed node is always selected when alive.
func TestFixedWithFallback_NodeAlive(t *testing.T) {
	option := &dialer.GlobalOption{
		Log: log,
	}
	d0 := &mockDialer{index: 0, name: "d0"}
	d1 := &mockDialer{index: 1, name: "d1"}
	dialers := []*dialer.Dialer{
		dialer.NewDialer(d0, option, dialer.InstanceOption{DisableCheck: true}, d0.Property()),
		dialer.NewDialer(d1, option, dialer.InstanceOption{DisableCheck: true}, d1.Property()),
	}

	// Set both dialers as alive
	d0.SetAlive(true)
	d1.SetAlive(true)

	g := NewDialerGroup(option, "test-group", dialers, newEmptyAnnotations(len(dialers)),
		DialerSelectionPolicy{
			Policy:               consts.DialerSelectionPolicy_FixedWithFallback,
			FixedIndex:           0,
			FixedFallbackTimeout: 3 * time.Second,
			FixedFallbackRetries: 3,
			FallbackPolicy:       consts.DialerSelectionPolicy_Random,
		}, func(alive bool, networkType *dialer.NetworkType, isInit bool) {})

	// Select 10 times, all should return d0
	for i := 0; i < 10; i++ {
		d, _, err := g.Select(TestNetworkType, false)
		if err != nil {
			t.Fatalf("Select() error: %v", err)
		}
		if d != dialers[0] {
			t.Errorf("Select() = %v, want dialers[0]", d)
		}
	}
}

// TestFixedWithFallback_DeadNodeFirstDetection tests that first dead detection records time.
func TestFixedWithFallback_DeadNodeFirstDetection(t *testing.T) {
	option := &dialer.GlobalOption{
		Log: log,
	}
	d0 := &mockDialer{index: 0, name: "d0"}
	d1 := &mockDialer{index: 1, name: "d1"}
	dialers := []*dialer.Dialer{
		dialer.NewDialer(d0, option, dialer.InstanceOption{DisableCheck: true}, d0.Property()),
		dialer.NewDialer(d1, option, dialer.InstanceOption{DisableCheck: true}, d1.Property()),
	}

	// Set both dialers as alive initially
	d0.SetAlive(true)
	d1.SetAlive(true)

	g := NewDialerGroup(option, "test-group", dialers, newEmptyAnnotations(len(dialers)),
		DialerSelectionPolicy{
			Policy:               consts.DialerSelectionPolicy_FixedWithFallback,
			FixedIndex:           0,
			FixedFallbackTimeout: 30 * time.Second, // Long timeout
			FixedFallbackRetries: 3,
			FallbackPolicy:       consts.DialerSelectionPolicy_Random,
		}, func(alive bool, networkType *dialer.NetworkType, isInit bool) {})

	// First select should return d0
	d, _, err := g.Select(TestNetworkType, false)
	if err != nil {
		t.Fatalf("Select() error: %v", err)
	}
	if d != dialers[0] {
		t.Errorf("Select() = %v, want dialers[0]", d)
	}

	// Now mark d0 as dead
	d0.SetAlive(false)
	// Mark d1 as alive for fallback
	d1.SetAlive(true)

	// Second select should still return d0 (dead node) but record the dead time
	d, _, err = g.Select(TestNetworkType, false)
	if err != nil {
		t.Fatalf("Select() error: %v", err)
	}
	if d != dialers[0] {
		t.Errorf("Select() = %v, want dialers[0] (dead node, still trying)", d)
	}

	// Third select should still return d0 (within timeout)
	d, _, err = g.Select(TestNetworkType, false)
	if err != nil {
		t.Fatalf("Select() error: %v", err)
	}
	if d != dialers[0] {
		t.Errorf("Select() = %v, want dialers[0] (within timeout)", d)
	}
}

// TestFixedWithFallback_RetriesExhaustedFallback tests that fallback happens after retries exhausted.
func TestFixedWithFallback_RetriesExhaustedFallback(t *testing.T) {
	option := &dialer.GlobalOption{
		Log: log,
	}
	d0 := &mockDialer{index: 0, name: "d0"}
	d1 := &mockDialer{index: 1, name: "d1"}
	dialers := []*dialer.Dialer{
		dialer.NewDialer(d0, option, dialer.InstanceOption{DisableCheck: true}, d0.Property()),
		dialer.NewDialer(d1, option, dialer.InstanceOption{DisableCheck: true}, d1.Property()),
	}

	// Set both dialers as alive initially
	d0.SetAlive(true)
	d1.SetAlive(true)

	timeout := 1 * time.Millisecond // Very short timeout for testing
	retries := 2

	g := NewDialerGroup(option, "test-group", dialers, newEmptyAnnotations(len(dialers)),
		DialerSelectionPolicy{
			Policy:               consts.DialerSelectionPolicy_FixedWithFallback,
			FixedIndex:           0,
			FixedFallbackTimeout: timeout,
			FixedFallbackRetries: retries,
			FallbackPolicy:       consts.DialerSelectionPolicy_Random,
		}, func(alive bool, networkType *dialer.NetworkType, isInit bool) {})

	// First select should return d0 (alive)
	d, _, err := g.Select(TestNetworkType, false)
	if err != nil {
		t.Fatalf("Select() error: %v", err)
	}
	if d != dialers[0] {
		t.Errorf("Select() = %v, want dialers[0]", d)
	}

	// Mark d0 as dead
	d0.SetAlive(false)
	// d1 is already alive

	// First request after d0 dead - should return d0 (first detection, records time)
	d, _, err = g.Select(TestNetworkType, false)
	if err != nil {
		t.Fatalf("Select() error: %v", err)
	}
	if d != dialers[0] {
		t.Errorf("Select() = %v, want dialers[0] (first dead detection)", d)
	}

	// Wait for timeout
	time.Sleep(timeout * 2)

	// Retry 1 - should return d0 (still trying)
	d, _, err = g.Select(TestNetworkType, false)
	if err != nil {
		t.Fatalf("Select() error: %v", err)
	}
	if d != dialers[0] {
		t.Errorf("Select() = %v, want dialers[0] (retry 1)", d)
	}

	// Wait for timeout again
	time.Sleep(timeout * 2)

	// Retry 2 - should return d0 (still trying)
	d, _, err = g.Select(TestNetworkType, false)
	if err != nil {
		t.Fatalf("Select() error: %v", err)
	}
	if d != dialers[0] {
		t.Errorf("Select() = %v, want dialers[0] (retry 2)", d)
	}

	// Wait for timeout again
	time.Sleep(timeout * 2)

	// Retry 3 (exhausted) - should fallback to d1
	d, _, err = g.Select(TestNetworkType, false)
	if err != nil {
		t.Fatalf("Select() error: %v", err)
	}
	if d != dialers[1] {
		t.Errorf("Select() = %v, want dialers[1] (fallback after retries exhausted)", d)
	}
}

// TestFixedWithFallback_RetriesZeroImmediateFallback tests that retries=0 causes immediate fallback.
func TestFixedWithFallback_RetriesZeroImmediateFallback(t *testing.T) {
	option := &dialer.GlobalOption{
		Log: log,
	}
	d0 := &mockDialer{index: 0, name: "d0"}
	d1 := &mockDialer{index: 1, name: "d1"}
	dialers := []*dialer.Dialer{
		dialer.NewDialer(d0, option, dialer.InstanceOption{DisableCheck: true}, d0.Property()),
		dialer.NewDialer(d1, option, dialer.InstanceOption{DisableCheck: true}, d1.Property()),
	}

	// Set both dialers as alive initially
	d0.SetAlive(true)
	d1.SetAlive(true)

	g := NewDialerGroup(option, "test-group", dialers, newEmptyAnnotations(len(dialers)),
		DialerSelectionPolicy{
			Policy:               consts.DialerSelectionPolicy_FixedWithFallback,
			FixedIndex:           0,
			FixedFallbackTimeout: 30 * time.Second, // Long timeout (shouldn't matter)
			FixedFallbackRetries: 0,                // No retries - immediate fallback
			FallbackPolicy:       consts.DialerSelectionPolicy_Random,
		}, func(alive bool, networkType *dialer.NetworkType, isInit bool) {})

	// First select should return d0 (alive)
	d, _, err := g.Select(TestNetworkType, false)
	if err != nil {
		t.Fatalf("Select() error: %v", err)
	}
	if d != dialers[0] {
		t.Errorf("Select() = %v, want dialers[0]", d)
	}

	// Mark d0 as dead
	d0.SetAlive(false)
	// d1 is already alive

	// First request after d0 dead - should immediately fallback to d1
	d, _, err = g.Select(TestNetworkType, false)
	if err != nil {
		t.Fatalf("Select() error: %v", err)
	}
	if d != dialers[1] {
		t.Errorf("Select() = %v, want dialers[1] (immediate fallback with retries=0)", d)
	}
}

// TestFixedWithFallback_NodeRecovery tests that traffic restores when node recovers.
func TestFixedWithFallback_NodeRecovery(t *testing.T) {
	option := &dialer.GlobalOption{
		Log: log,
	}
	d0 := &mockDialer{index: 0, name: "d0"}
	d1 := &mockDialer{index: 1, name: "d1"}
	dialers := []*dialer.Dialer{
		dialer.NewDialer(d0, option, dialer.InstanceOption{DisableCheck: true}, d0.Property()),
		dialer.NewDialer(d1, option, dialer.InstanceOption{DisableCheck: true}, d1.Property()),
	}

	// Set both dialers as alive initially
	d0.SetAlive(true)
	d1.SetAlive(true)

	timeout := 1 * time.Millisecond
	retries := 1

	g := NewDialerGroup(option, "test-group", dialers, newEmptyAnnotations(len(dialers)),
		DialerSelectionPolicy{
			Policy:               consts.DialerSelectionPolicy_FixedWithFallback,
			FixedIndex:           0,
			FixedFallbackTimeout: timeout,
			FixedFallbackRetries: retries,
			FallbackPolicy:       consts.DialerSelectionPolicy_Random,
		}, func(alive bool, networkType *dialer.NetworkType, isInit bool) {})

	// First select should return d0 (alive)
	d, _, err := g.Select(TestNetworkType, false)
	if err != nil {
		t.Fatalf("Select() error: %v", err)
	}
	if d != dialers[0] {
		t.Errorf("Select() = %v, want dialers[0]", d)
	}

	// Mark d0 as dead
	d0.SetAlive(false)
	// d1 is alive

	// First request after d0 dead - should return d0 (first detection)
	d, _, err = g.Select(TestNetworkType, false)
	if err != nil {
		t.Fatalf("Select() error: %v", err)
	}

	// Wait for timeout
	time.Sleep(timeout * 2)

	// Retry 1 (exhausted) - should fallback to d1
	d, _, err = g.Select(TestNetworkType, false)
	if err != nil {
		t.Fatalf("Select() error: %v", err)
	}
	if d != dialers[1] {
		t.Errorf("Select() = %v, want dialers[1] (fallback)", d)
	}

	// Now d0 recovers
	d0.SetAlive(true)

	// Next select should return d0 (recovered)
	d, _, err = g.Select(TestNetworkType, false)
	if err != nil {
		t.Fatalf("Select() error: %v", err)
	}
	if d != dialers[0] {
		t.Errorf("Select() = %v, want dialers[0] (node recovered)", d)
	}
}

// TestFixedWithFallback_MinLatencyFallback tests fallback to min_latency policy.
func TestFixedWithFallback_MinLatencyFallback(t *testing.T) {
	option := &dialer.GlobalOption{
		Log: log,
	}
	d0 := &mockDialer{index: 0, name: "d0"}
	d1 := &mockDialer{index: 1, name: "d1"}
	dialers := []*dialer.Dialer{
		dialer.NewDialer(d0, option, dialer.InstanceOption{DisableCheck: true}, d0.Property()),
		dialer.NewDialer(d1, option, dialer.InstanceOption{DisableCheck: true}, d1.Property()),
	}

	// Set d0 as alive, d1 as alive
	d0.SetAlive(true)
	d1.SetAlive(true)

	timeout := 1 * time.Millisecond
	retries := 0 // Immediate fallback

	g := NewDialerGroup(option, "test-group", dialers, newEmptyAnnotations(len(dialers)),
		DialerSelectionPolicy{
			Policy:               consts.DialerSelectionPolicy_FixedWithFallback,
			FixedIndex:           0,
			FixedFallbackTimeout: timeout,
			FixedFallbackRetries: retries,
			FallbackPolicy:       consts.DialerSelectionPolicy_MinLastLatency,
		}, func(alive bool, networkType *dialer.NetworkType, isInit bool) {})

	// First select should return d0 (alive)
	d, _, err := g.Select(TestNetworkType, false)
	if err != nil {
		t.Fatalf("Select() error: %v", err)
	}
	if d != dialers[0] {
		t.Errorf("Select() = %v, want dialers[0]", d)
	}

	// Mark d0 as dead
	d0.SetAlive(false)
	// Set d1 latency
	dialers[1].MustGetLatencies10(TestNetworkType).AppendLatency(50 * time.Millisecond)

	// Immediate fallback - should use min_latency
	d, _, err = g.Select(TestNetworkType, false)
	if err != nil {
		t.Fatalf("Select() error: %v", err)
	}
	if d != dialers[1] {
		t.Errorf("Select() = %v, want dialers[1] (min_latency fallback)", d)
	}
}

// TestFixedWithFallback_NoAliveFallback tests error when no fallback dialers are alive.
func TestFixedWithFallback_NoAliveFallback(t *testing.T) {
	option := &dialer.GlobalOption{
		Log: log,
	}
	d0 := &mockDialer{index: 0, name: "d0"}
	d1 := &mockDialer{index: 1, name: "d1"}
	dialers := []*dialer.Dialer{
		dialer.NewDialer(d0, option, dialer.InstanceOption{DisableCheck: true}, d0.Property()),
		dialer.NewDialer(d1, option, dialer.InstanceOption{DisableCheck: true}, d1.Property()),
	}

	// Set d0 as alive, d1 as dead
	d0.SetAlive(true)
	d1.SetAlive(false)

	g := NewDialerGroup(option, "test-group", dialers, newEmptyAnnotations(len(dialers)),
		DialerSelectionPolicy{
			Policy:               consts.DialerSelectionPolicy_FixedWithFallback,
			FixedIndex:           0,
			FixedFallbackTimeout: 1 * time.Millisecond,
			FixedFallbackRetries: 0, // Immediate fallback
			FallbackPolicy:       consts.DialerSelectionPolicy_Random,
		}, func(alive bool, networkType *dialer.NetworkType, isInit bool) {})

	// First select should return d0 (alive)
	d, _, err := g.Select(TestNetworkType, false)
	if err != nil {
		t.Fatalf("Select() error: %v", err)
	}
	if d != dialers[0] {
		t.Errorf("Select() = %v, want dialers[0]", d)
	}

	// Mark d0 as dead
	d0.SetAlive(false)
	// d1 is already dead

	// Should return ErrNoAliveDialer since no fallback dialers are alive
	_, _, err = g.Select(TestNetworkType, false)
	if !errors.Is(err, ErrNoAliveDialer) {
		t.Errorf("Select() error = %v, want ErrNoAliveDialer", err)
	}
}
