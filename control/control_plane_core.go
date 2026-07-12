/*
 * SPDX-License-Identifier: AGPL-3.0-only
 * Copyright (c) 2022-2026, daeuniverse Organization <dae@v2raya.org>
 */

package control

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"regexp"
	"sync"
	"sync/atomic"

	"github.com/cilium/ebpf"
	ciliumLink "github.com/cilium/ebpf/link"
	"github.com/daeuniverse/dae/common/consts"
	"github.com/daeuniverse/dae/component"
	internal "github.com/daeuniverse/dae/pkg/ebpf_internal"
	"github.com/mohae/deepcopy"
	"github.com/safchain/ethtool"
	"github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// coreFlip should be 0 or 1; accessed atomically.
var coreFlip int32

type cgroupAttachment interface {
	io.Closer
}

var detectCgroupPathFunc = detectCgroupPath
var attachCgroupFunc = func(opts ciliumLink.CgroupOptions) (cgroupAttachment, error) {
	return ciliumLink.AttachCgroup(opts)
}

type sharedUdpConnStateTrackerEntry struct {
	tracker *udpConnStateTracker
	refs    int
}

var sharedUdpConnStateTrackerRegistry = struct {
	mu      sync.Mutex
	entries map[*bpfObjects]*sharedUdpConnStateTrackerEntry
}{
	entries: make(map[*bpfObjects]*sharedUdpConnStateTrackerEntry),
}

func acquireSharedUdpConnStateTracker(bpf *bpfObjects) *udpConnStateTracker {
	if bpf == nil {
		return newUdpConnStateTracker()
	}

	sharedUdpConnStateTrackerRegistry.mu.Lock()
	defer sharedUdpConnStateTrackerRegistry.mu.Unlock()

	entry := sharedUdpConnStateTrackerRegistry.entries[bpf]
	if entry == nil {
		entry = &sharedUdpConnStateTrackerEntry{
			tracker: newUdpConnStateTracker(),
		}
		sharedUdpConnStateTrackerRegistry.entries[bpf] = entry
	}
	entry.refs++
	return entry.tracker
}

func releaseSharedUdpConnStateTracker(bpf *bpfObjects, tracker *udpConnStateTracker) {
	if bpf == nil || tracker == nil {
		return
	}

	sharedUdpConnStateTrackerRegistry.mu.Lock()
	defer sharedUdpConnStateTrackerRegistry.mu.Unlock()

	entry := sharedUdpConnStateTrackerRegistry.entries[bpf]
	if entry == nil || entry.tracker != tracker {
		return
	}
	entry.refs--
	if entry.refs <= 0 {
		delete(sharedUdpConnStateTrackerRegistry.entries, bpf)
	}
}

type controlPlaneCore struct {
	mu      sync.Mutex
	deferMu sync.Mutex

	log        *logrus.Logger
	deferFuncs []func() error
	// bpfHookDetachFuncs contains only BPF hook detachment functions (FilterDel, tc detach)
	// These are tracked separately so they can be detached immediately on SIGTERM
	// before other cleanup that might take longer (like dialer shutdown).
	// Protected by bpfHookMu to avoid deadlock with c.mu in _bindLan/_bindWan.
	bpfHookDetachFuncs []func() error
	// bpfHookDetachKeys are the dedup keys passed to addManagedBpfHookCleanup,
	// so self-heal re-binds (which re-call _bindLan/_bindWan on the same
	// ControlPlaneCore) do not accumulate duplicate detach funcs.
	bpfHookDetachKeys []string
	bpfHookMu         sync.Mutex
	bpf               atomic.Pointer[bpfObjects]
	outboundId2Name   map[uint8]string
	// tcpRelayOffload is permanently disabled due to kernel panic issues.
	// See: https://github.com/daeuniverse/dae/pull/912
	// Field preserved for ABI compatibility; always remains false.

	kernelVersion *internal.Version

	flip             int
	isReload         bool
	bpfEjected       bool
	bpfHooksDetached bool // Track if BPF hooks were already detached
	retired          atomic.Bool

	closed context.Context
	close  context.CancelFunc
	ifmgr  *component.InterfaceManager

	udpConnStateTracker atomic.Pointer[udpConnStateTracker]
	domainRouting       *domainRoutingTracker
	lpmTrieIndices      []uint32
	bpfOwned            bool

	// datapathIfaces records the interfaces that were successfully bound,
	// so validateDatapathBindings can verify the filters are actually attached.
	datapathIfaces []boundIface
	datapathMu     sync.Mutex
}

func newControlPlaneCore(log *logrus.Logger,
	bpf *bpfObjects,
	outboundId2Name map[uint8]string,
	kernelVersion *internal.Version,
	isReload bool,
) *controlPlaneCore {
	var flip int
	if isReload {
		flip = int(atomic.LoadInt32(&coreFlip)&1 ^ 1)
		atomic.StoreInt32(&coreFlip, int32(flip))
	} else {
		flip = int(atomic.LoadInt32(&coreFlip))
	}
	var deferFuncs []func() error
	bpfOwned := !isReload
	closed, toClose := context.WithCancel(context.Background())
	ifmgr := component.NewInterfaceManager(log)
	deferFuncs = append(deferFuncs, ifmgr.Close)
	core := &controlPlaneCore{
		log:                log,
		deferFuncs:         deferFuncs,
		bpfHookDetachFuncs: make([]func() error, 0),
		bpfHookDetachKeys:  make([]string, 0),
		outboundId2Name:    outboundId2Name,
		kernelVersion:      kernelVersion,
		flip:               flip,
		isReload:           isReload,
		bpfEjected:         false,
		bpfHooksDetached:   false,
		ifmgr:              ifmgr,
		closed:             closed,
		close:              toClose,
		domainRouting:      newDomainRoutingTracker(),
		bpfOwned:           bpfOwned,
	}
	core.bpf.Store(bpf)
	core.udpConnStateTracker.Store(acquireSharedUdpConnStateTracker(bpf))
	return core
}

func (c *controlPlaneCore) getUdpConnStateTracker() *udpConnStateTracker {
	if c == nil {
		return nil
	}
	if tracker := c.udpConnStateTracker.Load(); tracker != nil {
		return tracker
	}
	tracker := acquireSharedUdpConnStateTracker(c.bpf.Load())
	if c.udpConnStateTracker.CompareAndSwap(nil, tracker) {
		return tracker
	}
	releaseSharedUdpConnStateTracker(c.bpf.Load(), tracker)
	return c.udpConnStateTracker.Load()
}

func (c *controlPlaneCore) Flip() {
	// Use CAS loop to avoid race condition between Load and Store.
	for {
		old := atomic.LoadInt32(&coreFlip)
		newVal := old&1 ^ 1
		if atomic.CompareAndSwapInt32(&coreFlip, old, newVal) {
			break
		}
	}
}

// addBpfHookDetach adds a BPF hook detachment function to the dedicated list.
// These functions will be executed immediately on SIGTERM before other cleanup.
// Uses bpfHookMu to avoid deadlock with c.mu held by callers like _bindLan/_bindWan.
func (c *controlPlaneCore) addBpfHookDetach(detachFunc func() error) {
	c.bpfHookMu.Lock()
	defer c.bpfHookMu.Unlock()
	c.bpfHookDetachFuncs = append(c.bpfHookDetachFuncs, detachFunc)
}

func (c *controlPlaneCore) addDeferFunc(deferFunc func() error) bool {
	c.deferMu.Lock()
	defer c.deferMu.Unlock()
	select {
	case <-c.closed.Done():
		return false
	default:
	}
	c.deferFuncs = append(c.deferFuncs, deferFunc)
	return true
}

// addManagedBpfHookCleanup registers hook cleanup for both regular close and
// immediate detach paths. Hook cleanup must remain active after EjectBpf():
// ownership transfer only skips bpf.Close(), not removal of this generation's
// TC filters from the system.
// addManagedBpfHookCleanup registers hook cleanup for both regular close and
// immediate detach paths. Hook cleanup must remain active after EjectBpf():
// ownership transfer only skips bpf.Close(), not removal of this generation's
// TC filters from the system.
//
// key dedupes registrations: self-heal re-binds call _bindLan/_bindWan
// again on the same ControlPlaneCore, and without dedup each reload that
// loses a filter would append another (idempotent but ever-growing) detach
// func to both deferFuncs and bpfHookDetachFuncs. The key identifies
// the bound (iface, filter) pair.
func (c *controlPlaneCore) addManagedBpfHookCleanup(key string, detachFunc func() error) {
	c.bpfHookMu.Lock()
	for _, k := range c.bpfHookDetachKeys {
		if k == key {
			c.bpfHookMu.Unlock()
			return
		}
	}
	c.bpfHookDetachKeys = append(c.bpfHookDetachKeys, key)
	c.bpfHookMu.Unlock()

	if !c.addDeferFunc(detachFunc) {
		if err := detachFunc(); err != nil && c.log != nil {
			c.log.WithError(err).Warn("controlPlaneCore: failed to detach hook after close began")
		}
		return
	}
	c.addBpfHookDetach(detachFunc)
}

// DetachBpfHooks immediately detaches all BPF hooks from the system.
// This should be called first when receiving SIGTERM to ensure network is restored
// even if the rest of the shutdown process takes too long and gets SIGKILL'd.
// This is safe to call multiple times - subsequent calls will be no-ops.
func (c *controlPlaneCore) DetachBpfHooks() error {
	c.bpfHookMu.Lock()
	defer c.bpfHookMu.Unlock()

	// Already detached, skip
	if c.bpfHooksDetached {
		return nil
	}

	c.log.Infoln("[Shutdown] Detaching BPF hooks immediately to restore network")

	var errs []error
	// Execute in reverse order (last attached, first detached)
	for i := len(c.bpfHookDetachFuncs) - 1; i >= 0; i-- {
		if e := c.bpfHookDetachFuncs[i](); e != nil {
			// Log but continue detaching other hooks
			c.log.WithError(e).Warnln("[Shutdown] Failed to detach BPF hook")
			errs = append(errs, e)
		}
	}

	c.bpfHooksDetached = true
	c.log.Infoln("[Shutdown] BPF hooks detached, network should be restored")

	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

func (c *controlPlaneCore) Close() (err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	bpf := c.bpf.Load()
	select {
	case <-c.closed.Done():
		return nil
	default:
	}
	// Invoke defer funcs in reverse order and collect errors.
	// Use errors.Join (Go 1.20+) for clean multi-error handling.
	var errs []error
	// Clear LPM slots still owned by this generation. Retired generations hand
	// their slots to the next generation before draining so they cannot delete a
	// slot that has already been reused by a later reload.
	if bpf != nil && bpf.LpmArrayMap != nil && len(c.lpmTrieIndices) > 0 {
		for _, idx := range c.lpmTrieIndices {
			if e := bpf.LpmArrayMap.Delete(idx); e != nil && !errors.Is(e, ebpf.ErrKeyNotExist) {
				c.log.Errorf("Failed to clear BPF LPM slot %d: %v", idx, e)
			}
		}
	}
	c.close()
	c.deferMu.Lock()
	deferFuncs := append([]func() error(nil), c.deferFuncs...)
	c.deferFuncs = nil
	c.deferMu.Unlock()

	for i := len(deferFuncs) - 1; i >= 0; i-- {
		if e := deferFuncs[i](); e != nil {
			errs = append(errs, e)
		}
	}

	if c.bpfOwned && bpf != nil {
		if e := bpf.Close(); e != nil {
			errs = append(errs, e)
		}
	}
	if tracker := c.udpConnStateTracker.Swap(nil); tracker != nil {
		releaseSharedUdpConnStateTracker(bpf, tracker)
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

func getIfParamsFromLink(link netlink.Link) (ifParams bpfIfParams, err error) {
	// Get link offload features.
	et, err := ethtool.NewEthtool()
	if err != nil {
		// ethtool may be unavailable in restricted environments (e.g. containers
		// without CAP_NET_ADMIN). Silently degrade to defaults.
		return bpfIfParams{}, nil
	}
	defer et.Close()
	features, err := et.Features(link.Attrs().Name)
	if err != nil {
		// Virtual interfaces (TUN/TAP, WireGuard, etc.) or older kernels
		// may not support ETHTOOL_GFEATURES.  Silently degrade to defaults
		// (all offload flags = false) rather than blocking interface binding.
		return bpfIfParams{}, nil
	}
	if features["tx-checksum-ip-generic"] {
		ifParams.TxL4CksmIp4Offload = true
		ifParams.TxL4CksmIp6Offload = true
	}
	if features["tx-checksum-ipv4"] {
		ifParams.TxL4CksmIp4Offload = true
	}
	if features["tx-checksum-ipv6"] {
		ifParams.TxL4CksmIp6Offload = true
	}
	if features["rx-checksum"] {
		ifParams.RxCksmOffload = true
	}
	switch {
	case regexp.MustCompile(`^docker\d+$`).MatchString(link.Attrs().Name):
		ifParams.UseNonstandardOffloadAlgorithm = true
	default:
	}
	return ifParams, nil
}

func (c *controlPlaneCore) linkHdrLen(ifname string) (uint32, error) {
	link, err := netlink.LinkByName(ifname)
	if err != nil {
		return 0, err
	}
	var linkHdrLen uint32
	switch link.Attrs().EncapType {
	case "none", "ipip", "ppp", "tun":
		linkHdrLen = consts.LinkHdrLen_None
	case "ether":
		linkHdrLen = consts.LinkHdrLen_Ethernet
	default:
		c.log.Warnf("Maybe unsupported link type %v, using default link header length", link.Attrs().EncapType)
		linkHdrLen = consts.LinkHdrLen_Ethernet
	}
	return linkHdrLen, nil
}

func buildClsactQdisc(link netlink.Link) *netlink.GenericQdisc {
	return &netlink.GenericQdisc{
		QdiscAttrs: netlink.QdiscAttrs{
			LinkIndex: link.Attrs().Index,
			Handle:    netlink.MakeHandle(0xffff, 0),
			Parent:    netlink.HANDLE_CLSACT,
		},
		QdiscType: "clsact",
	}
}

func (c *controlPlaneCore) addQdisc(link netlink.Link) error {
	qdisc := buildClsactQdisc(link)
	if err := netlink.QdiscAdd(qdisc); err != nil && !errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("cannot add clsact qdisc: %w", err)
	}
	return nil
}

func (c *controlPlaneCore) delQdisc(link netlink.Link) error {
	qdisc := buildClsactQdisc(link)
	if err := netlink.QdiscDel(qdisc); err != nil && !errors.Is(err, unix.ENOENT) && !errors.Is(err, unix.ENODEV) {
		return fmt.Errorf("cannot delete clsact qdisc: %w", err)
	} else if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENODEV) {
		c.log.Debugf("delQdisc: clsact qdisc or link not found for %v (already gone)", link.Attrs().Name)
	}
	return nil
}

// bindLan automatically configures kernel parameters and bind to lan interface `ifname`.
// bindLan supports lazy-bind if interface `ifname` is not found.
// bindLan supports rebinding when the interface `ifname` is detected in the future.
func (c *controlPlaneCore) bindLan(ifname string, autoConfigKernelParameter bool) {
	initlinkCallback := func(link netlink.Link) {
		if link.Attrs().Name == HostVethName {
			return
		}
		if autoConfigKernelParameter {
			SetSendRedirects(link.Attrs().Name, "0")
			SetForwarding(link.Attrs().Name, "1")
		}
		if err := c._bindLan(link.Attrs().Name); err != nil {
			c.log.Errorf("bindLan: %v", err)
		}
	}
	newlinkCallback := func(link netlink.Link) {
		if link.Attrs().Name == HostVethName {
			return
		}
		c.log.Warnf("New link creation of '%v' is detected. Bind LAN program to it.", link.Attrs().Name)
		initlinkCallback(link)
	}
	dellinkCallback := func(link netlink.Link) {
		if link.Attrs().Name == HostVethName {
			return
		}
		c.log.Warnf("Link deletion of '%v' is detected. Bind LAN program to it once it is re-created.", link.Attrs().Name)
		if err := c.delQdisc(link); err != nil {
			c.log.Errorf("delQdisc: %v", err)
		}
	}
	c.ifmgr.RegisterWithPattern(ifname, initlinkCallback, newlinkCallback, dellinkCallback)
}

func (c *controlPlaneCore) _bindLan(ifname string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	bpf := c.bpf.Load()
	if bpf == nil {
		return nil
	}
	select {
	case <-c.closed.Done():
		return nil
	default:
	}
	c.log.Infof("Bind to LAN: %v", ifname)

	link, err := netlink.LinkByName(ifname)
	if err != nil {
		return err
	}
	if err = CheckIpforward(ifname); err != nil {
		return err
	}
	if err = CheckSendRedirects(ifname); err != nil {
		return err
	}
	// Best effort to add qdisc; it may already exist.
	_ = c.addQdisc(link)
	linkHdrLen, err := c.linkHdrLen(ifname)
	if err != nil {
		return err
	}
	/// Insert an elem into IfindexParamsMap.
	ifParams, err := getIfParamsFromLink(link)
	if err != nil {
		return err
	}
	if err = ifParams.CheckVersionRequirement(c.kernelVersion); err != nil {
		return err
	}

	// Insert filters.
	filterIngress := &netlink.BpfFilter{
		FilterAttrs: netlink.FilterAttrs{
			LinkIndex: link.Attrs().Index,
			Parent:    netlink.HANDLE_MIN_INGRESS,
			Handle:    netlink.MakeHandle(0x2023, 0b100+uint16(c.flip)),
			Protocol:  unix.ETH_P_ALL,
			// Priority should be behind of WAN's
			Priority: 2,
		},
		Name:         consts.AppName + "_lan_ingress",
		DirectAction: true,
	}
	var lanIngressProg *ebpf.Program
	if linkHdrLen > 0 {
		lanIngressProg = bpf.TproxyLanIngressL2
		filterIngress.Name += "_l2"
	} else {
		lanIngressProg = bpf.TproxyLanIngressL3
		filterIngress.Name += "_l3"
	}
	filterIngress.Fd = lanIngressProg.FD()
	// Remove and add.
	// Best effort to remove old filter; it may not exist.
	_ = netlink.FilterDel(filterIngress)
	if !c.isReload {
		tryDeleteFlippedFilter(filterIngress)
	}
	if err := netlink.FilterAdd(filterIngress); err != nil && !errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("cannot attach ebpf object to filter ingress: %w", err)
	}
	if c.isReload {
		// Reload make-before-break: the new-flip filter is now attached and
		// serving, so removing the retiring generation's opposite-flip filter
		// here leaves no datapath gap. Doing it eagerly (instead of relying on
		// the retiring core's async Close()) prevents the stale filter that
		// otherwise black-holes traffic when that Close() is delayed or killed.
		tryDeleteFlippedFilter(filterIngress)
	}
	detachFunc := func() error {
		if err := netlink.FilterDel(filterIngress); err != nil && !os.IsNotExist(err) && !errors.Is(err, unix.ENODEV) {
			return fmt.Errorf("FilterDel(%v:%v): %w", ifname, filterIngress.Name, err)
		}
		return nil
	}
	c.addManagedBpfHookCleanup("lan-ingress", detachFunc)

	filterEgress := &netlink.BpfFilter{
		FilterAttrs: netlink.FilterAttrs{
			LinkIndex: link.Attrs().Index,
			Parent:    netlink.HANDLE_MIN_EGRESS,
			Handle:    netlink.MakeHandle(0x2023, 0b010+uint16(c.flip)),
			Protocol:  unix.ETH_P_ALL,
			// Priority should be front of WAN's
			Priority: 1,
		},
		Name:         consts.AppName + "_lan_egress",
		DirectAction: true,
	}
	var lanEgressProg *ebpf.Program
	if linkHdrLen > 0 {
		lanEgressProg = bpf.TproxyLanEgressL2
		filterEgress.Name += "_l2"
	} else {
		lanEgressProg = bpf.TproxyLanEgressL3
		filterEgress.Name += "_l3"
	}
	filterEgress.Fd = lanEgressProg.FD()
	// Remove and add.
	// Best effort to remove old filter; it may not exist.
	_ = netlink.FilterDel(filterEgress)
	if !c.isReload {
		tryDeleteFlippedFilter(filterEgress)
	}
	if err := netlink.FilterAdd(filterEgress); err != nil && !errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("cannot attach ebpf object to filter egress: %w", err)
	}
	if c.isReload {
		// Reload make-before-break (see the ingress filter above).
		tryDeleteFlippedFilter(filterEgress)
	}
	egressDetachFunc := func() error {
		if err := netlink.FilterDel(filterEgress); err != nil && !os.IsNotExist(err) && !errors.Is(err, unix.ENODEV) {
			return fmt.Errorf("FilterDel(%v:%v): %w", ifname, filterEgress.Name, err)
		}
		return nil
	}
	c.addManagedBpfHookCleanup("lan-egress", egressDetachFunc)

	// Record the binding only after both filters are attached, so a
	// failure mid-bind does not leave a "claimed but absent" entry (M6).
	// Each handle is paired with the program attached at it so the self-check
	// can verify the filter still points at this generation's program.
	c.recordBoundIface(ifname, "LAN",
		boundFilter{netlink.MakeHandle(0x2023, 0b100+uint16(c.flip)), lanIngressProg}, // ingress
		boundFilter{netlink.MakeHandle(0x2023, 0b010+uint16(c.flip)), lanEgressProg},  // egress
	)

	return nil
}

func (c *controlPlaneCore) setupSkPidMonitor() error {
	select {
	case <-c.closed.Done():
		return nil
	default:
	}
	/// Set-up SrcPidMapper.
	/// Attach programs to support pname routing.
	// Get the first-mounted cgroupv2 path.
	cgroupPath, err := detectCgroupPathFunc()
	if err != nil {
		return err
	}
	// Bind cg programs
	type cgProg struct {
		Name   string
		Prog   *ebpf.Program
		Attach ebpf.AttachType
	}
	bpf := c.bpf.Load()
	cgProgs := []cgProg{
		{Prog: bpf.TproxyWanCgSockCreate, Attach: ebpf.AttachCGroupInetSockCreate},
		{Prog: bpf.TproxyWanCgSockRelease, Attach: ebpf.AttachCgroupInetSockRelease},
		{Prog: bpf.TproxyWanCgConnect4, Attach: ebpf.AttachCGroupInet4Connect},
		{Prog: bpf.TproxyWanCgConnect6, Attach: ebpf.AttachCGroupInet6Connect},
		{Prog: bpf.TproxyWanCgSendmsg4, Attach: ebpf.AttachCGroupUDP4Sendmsg},
		{Prog: bpf.TproxyWanCgSendmsg6, Attach: ebpf.AttachCGroupUDP6Sendmsg},
	}
	attachedLinks := make([]cgroupAttachment, 0, len(cgProgs))
	detachFuncs := make([]func() error, 0, len(cgProgs))
	for _, prog := range cgProgs {
		attached, err := attachCgroupFunc(ciliumLink.CgroupOptions{
			Path:    cgroupPath,
			Attach:  prog.Attach,
			Program: prog.Prog,
		})
		if err != nil {
			for i := len(attachedLinks) - 1; i >= 0; i-- {
				_ = attachedLinks[i].Close()
			}
			return fmt.Errorf("AttachCgroup: %v: %w", prog.Prog.String(), err)
		}
		attachedLinks = append(attachedLinks, attached)
		attachedLink := attached
		detachFunc := func() error {
			if err := attachedLink.Close(); err != nil {
				return fmt.Errorf("inet6Bind.Close(): %w", err)
			}
			return nil
		}
		detachFuncs = append(detachFuncs, detachFunc)
	}
	for i, detachFunc := range detachFuncs {
		c.addManagedBpfHookCleanup(fmt.Sprintf("inet6bind-%d", i), detachFunc)
	}
	return nil
}

func (c *controlPlaneCore) setupTCPRelayOffload() error {
	if os.Getenv("DAE_DISABLE_TCP_RELAY_OFFLOAD") == "1" {
		c.log.Debug("TCP relay eBPF offload disabled by DAE_DISABLE_TCP_RELAY_OFFLOAD=1")
		return nil
	}
	// TCP relay eBPF offload is disabled due to kernel panic issues with bpf_msg_redirect_hash().
	// See: https://github.com/daeuniverse/dae/pull/912
	// The sk_msg program now returns SK_PASS, so we must not enable offload or connections will hang.
	// The function body below is preserved for potential future re-enabling.
	c.log.Info("TCP relay eBPF offload is disabled due to kernel panic issues; falling back to userspace relay")
	return nil
}

// bindWan supports lazy-bind if interface `ifname` is not found.
// bindWan supports rebinding when the interface `ifname` is detected in the future.
func (c *controlPlaneCore) bindWan(ifname string) {
	initlinkCallback := func(link netlink.Link) {
		if link.Attrs().Name == HostVethName {
			return
		}
		if err := c._bindWan(link.Attrs().Name); err != nil {
			c.log.Errorf("bindWan: %v", err)
		}
	}
	newlinkCallback := func(link netlink.Link) {
		if link.Attrs().Name == HostVethName {
			return
		}
		c.log.Warnf("New link creation of '%v' is detected. Bind WAN program to it.", link.Attrs().Name)
		initlinkCallback(link)
	}
	dellinkCallback := func(link netlink.Link) {
		if link.Attrs().Name == HostVethName {
			return
		}
		c.log.Warnf("Link deletion of '%v' is detected. Bind WAN program to it once it is re-created.", link.Attrs().Name)
		if err := c.delQdisc(link); err != nil {
			c.log.Errorf("delQdisc: %v", err)
		}
	}
	c.ifmgr.RegisterWithPattern(ifname, initlinkCallback, newlinkCallback, dellinkCallback)
}

func (c *controlPlaneCore) _bindWan(ifname string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	bpf := c.bpf.Load()
	if bpf == nil {
		return nil
	}
	select {
	case <-c.closed.Done():
		return nil
	default:
	}
	c.log.Infof("Bind to WAN: %v", ifname)

	link, err := netlink.LinkByName(ifname)
	if err != nil {
		return err
	}
	if link.Attrs().Index == consts.LoopbackIfIndex {
		return fmt.Errorf("cannot bind to loopback interface")
	}
	// Best effort to add qdisc; it may already exist.
	_ = c.addQdisc(link)
	linkHdrLen, err := c.linkHdrLen(ifname)
	if err != nil {
		return err
	}

	/// Insert an elem into IfindexParamsMap.
	ifParams, err := getIfParamsFromLink(link)
	if err != nil {
		return err
	}
	if err = ifParams.CheckVersionRequirement(c.kernelVersion); err != nil {
		return err
	}

	/// Set-up WAN ingress/egress TC programs.
	// Insert TC filters
	filterEgress := &netlink.BpfFilter{
		FilterAttrs: netlink.FilterAttrs{
			LinkIndex: link.Attrs().Index,
			Parent:    netlink.HANDLE_MIN_EGRESS,
			Handle:    netlink.MakeHandle(0x2023, 0b100+uint16(c.flip)),
			Protocol:  unix.ETH_P_ALL,
			Priority:  2,
		},
		Name:         consts.AppName + "_wan_egress",
		DirectAction: true,
	}
	var wanEgressProg *ebpf.Program
	if linkHdrLen > 0 {
		wanEgressProg = bpf.TproxyWanEgressL2
		filterEgress.Name += "_l2"
	} else {
		wanEgressProg = bpf.TproxyWanEgressL3
		filterEgress.Name += "_l3"
	}
	filterEgress.Fd = wanEgressProg.FD()
	// Best effort to remove old filter; it may not exist.
	_ = netlink.FilterDel(filterEgress)
	if !c.isReload {
		tryDeleteFlippedFilter(filterEgress)
	}
	if err := netlink.FilterAdd(filterEgress); err != nil && !errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("cannot attach ebpf object to filter egress: %w", err)
	}
	if c.isReload {
		// Reload make-before-break (see _bindLan for the rationale).
		tryDeleteFlippedFilter(filterEgress)
	}
	egressDetachFunc := func() error {
		if err := netlink.FilterDel(filterEgress); err != nil && !os.IsNotExist(err) && !errors.Is(err, unix.ENODEV) {
			return fmt.Errorf("FilterDel(%v:%v): %w", ifname, filterEgress.Name, err)
		}
		return nil
	}
	c.addManagedBpfHookCleanup("wan-egress", egressDetachFunc)

	filterIngress := &netlink.BpfFilter{
		FilterAttrs: netlink.FilterAttrs{
			LinkIndex: link.Attrs().Index,
			Parent:    netlink.HANDLE_MIN_INGRESS,
			Handle:    netlink.MakeHandle(0x2023, 0b010+uint16(c.flip)),
			Protocol:  unix.ETH_P_ALL,
			Priority:  1,
		},
		Name:         consts.AppName + "_wan_ingress",
		DirectAction: true,
	}
	var wanIngressProg *ebpf.Program
	if linkHdrLen > 0 {
		wanIngressProg = bpf.TproxyWanIngressL2
		filterIngress.Name += "_l2"
	} else {
		wanIngressProg = bpf.TproxyWanIngressL3
		filterIngress.Name += "_l3"
	}
	filterIngress.Fd = wanIngressProg.FD()
	// Best effort to remove old filter; it may not exist.
	_ = netlink.FilterDel(filterIngress)
	if !c.isReload {
		tryDeleteFlippedFilter(filterIngress)
	}
	if err := netlink.FilterAdd(filterIngress); err != nil && !errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("cannot attach ebpf object to filter ingress: %w", err)
	}
	if c.isReload {
		// Reload make-before-break (see _bindLan for the rationale).
		tryDeleteFlippedFilter(filterIngress)
	}
	ingressDetachFunc := func() error {
		if err := netlink.FilterDel(filterIngress); err != nil && !os.IsNotExist(err) && !errors.Is(err, unix.ENODEV) {
			return fmt.Errorf("FilterDel(%v:%v): %w", ifname, filterIngress.Name, err)
		}
		return nil
	}
	c.addManagedBpfHookCleanup("wan-ingress", ingressDetachFunc)

	// Record the binding only after both filters are attached (M6). Each handle
	// is paired with the program attached at it for the program-aware self-check.
	c.recordBoundIface(ifname, "WAN",
		boundFilter{netlink.MakeHandle(0x2023, 0b100+uint16(c.flip)), wanEgressProg},  // egress
		boundFilter{netlink.MakeHandle(0x2023, 0b010+uint16(c.flip)), wanIngressProg}, // ingress
	)

	return nil
}

func (c *controlPlaneCore) bindDaens() (err error) {
	bpf := c.bpf.Load()
	daens := GetDaeNetns()

	// tproxy_dae0peer_ingress@eth0 at dae netns
	// Best effort: qdisc may already exist and tx queue tuning is non-critical.
	daens.WithBestEffort("set dae0peer tx queue and add clsact qdisc", func() error {
		err := netlink.LinkSetTxQLen(daens.Dae0Peer(), DaeVethTxQLen)
		if err == nil {
			err = c.addQdisc(daens.Dae0Peer())
		}
		return err
	})
	filterDae0peerIngress := &netlink.BpfFilter{
		FilterAttrs: netlink.FilterAttrs{
			LinkIndex: daens.Dae0Peer().Attrs().Index,
			Parent:    netlink.HANDLE_MIN_INGRESS,
			Handle:    netlink.MakeHandle(0x2022, 0b010+uint16(c.flip)),
			Protocol:  unix.ETH_P_ALL,
			Priority:  0,
		},
		Fd:           bpf.TproxyDae0peerIngress.FD(),
		Name:         consts.AppName + "_dae0peer_ingress",
		DirectAction: true,
	}
	// Best effort to remove old filter; it may not exist.
	daens.WithBestEffort("delete old dae0peer ingress filter", func() error {
		err := netlink.FilterDel(filterDae0peerIngress)
		if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ESRCH) {
			return nil
		}
		return err
	})
	// Clean up thoroughly: delete the filter with the flipped (previous
	// generation's) handle. Defined once so both the fresh-start and the reload
	// make-before-break paths reuse it.
	deleteFlippedDae0peer := func() {
		filterIngressFlipped := deepcopy.Copy(filterDae0peerIngress).(*netlink.BpfFilter)
		filterIngressFlipped.Handle ^= 1
		daens.WithBestEffort("delete flipped dae0peer ingress filter", func() error {
			err := netlink.FilterDel(filterIngressFlipped)
			if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ESRCH) {
				return nil
			}
			return err
		})
	}
	// Remove and add.
	if !c.isReload {
		deleteFlippedDae0peer()
	}
	if err = daens.WithRequired("add dae0peer ingress filter", func() error {
		if err := netlink.FilterAdd(filterDae0peerIngress); err != nil && !errors.Is(err, unix.EEXIST) {
			return err
		}
		return nil
	}); err != nil {
		return fmt.Errorf("cannot attach ebpf object to filter ingress: %w", err)
	}
	if c.isReload {
		// Reload make-before-break: the new-flip dae0peer filter is attached and
		// serving, so remove the retiring generation's opposite-flip filter now
		// rather than waiting for its async Close() (see _bindLan).
		deleteFlippedDae0peer()
	}
	detachFunc := func() error {
		return daens.WithRequired("delete dae0peer ingress filter", func() error {
			if err := netlink.FilterDel(filterDae0peerIngress); err != nil && !os.IsNotExist(err) && !errors.Is(err, unix.ENODEV) {
				return fmt.Errorf("FilterDel(%v:%v): %w", daens.Dae0Peer().Attrs().Name, filterDae0peerIngress.Name, err)
			}
			return nil
		})
	}
	c.addManagedBpfHookCleanup("dae0peer-ingress", detachFunc)
	// Record the dae0peer binding (in the dae netns) so validateDatapathBindings
	// can confirm its TC filter survived a reload. It is checked inside the dae
	// netns; a missing dae0peer filter is a warning (non-fatal). Note: this is the
	// opposite of dae0, whose missing filter is fatal (it aborts startup/reload).
	// dae0peer only warns so the proxy keeps running.
	c.recordBoundIface(daens.Dae0Peer().Attrs().Name, "dae0peer",
		boundFilter{netlink.MakeHandle(0x2022, 0b010+uint16(c.flip)), bpf.TproxyDae0peerIngress})

	// tproxy_dae0_ingress@dae0 at host netns
	// Best effort to add qdisc; it may already exist.
	_ = c.addQdisc(daens.Dae0())
	filterDae0Ingress := &netlink.BpfFilter{
		FilterAttrs: netlink.FilterAttrs{
			LinkIndex: daens.Dae0().Attrs().Index,
			Parent:    netlink.HANDLE_MIN_INGRESS,
			Handle:    netlink.MakeHandle(0x2022, 0b010+uint16(c.flip)),
			Protocol:  unix.ETH_P_ALL,
			Priority:  0,
		},
		Fd:           bpf.TproxyDae0Ingress.FD(),
		Name:         consts.AppName + "_dae0_ingress",
		DirectAction: true,
	}
	// Best effort to remove old filter; it may not exist.
	_ = netlink.FilterDel(filterDae0Ingress)
	// Remove and add.
	if !c.isReload {
		tryDeleteFlippedFilter(filterDae0Ingress)
	}
	if err := netlink.FilterAdd(filterDae0Ingress); err != nil && !errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("cannot attach ebpf object to filter ingress: %w", err)
	}
	if c.isReload {
		// Reload make-before-break (see _bindLan for the rationale). dae0 is the
		// host-side veth dae itself owns; clearing its stale opposite-flip filter
		// eagerly is what prevents the reload black-hole on veth/netkit.
		tryDeleteFlippedFilter(filterDae0Ingress)
	}
	c.recordBoundIface(daens.Dae0().Attrs().Name, "dae0",
		boundFilter{netlink.MakeHandle(0x2022, 0b010+uint16(c.flip)), bpf.TproxyDae0Ingress})
	dae0DetachFunc := func() error {
		if err := netlink.FilterDel(filterDae0Ingress); err != nil && !os.IsNotExist(err) && !errors.Is(err, unix.ENODEV) {
			return fmt.Errorf("FilterDel(%v:%v): %w", daens.Dae0().Attrs().Name, filterDae0Ingress.Name, err)
		}
		return nil
	}
	c.addManagedBpfHookCleanup("dae0-ingress", dae0DetachFunc)
	return
}

// boundFilter pairs a TC filter handle with the BPF program that is expected to
// be attached at that handle. Validation checks not merely that *a* filter
// exists at the handle, but that it points at this generation's program, which
// catches the stale/zombie filter case: a leftover handle from a previous
// generation whose backing program is no longer valid (a reload black-hole).
// prog may be nil (e.g. in unit tests), in which case validation falls back to
// a handle-only presence check.
type boundFilter struct {
	handle uint32
	prog   *ebpf.Program
}

// boundIface is an interface that was successfully bound, paired with the full
// TC filter(s) and a human-readable label, so validateDatapathBindings can
// confirm the corresponding filter(s) are actually attached. LAN/WAN carry two
// filters (ingress + egress) under the same major but different minor — and,
// importantly, backed by *different* programs — so filters is a slice: every
// listed filter must be present (and, when its program is known, point at that
// program) for the binding to be considered healthy.
type boundIface struct {
	name    string
	label   string
	filters []boundFilter
}

// linkByName looks up a network interface by name. It is a package-level
// variable (defaulting to netlink.LinkByName) so tests can substitute a mock.
var linkByName = netlink.LinkByName

// recordBoundIface records a successfully bound interface so that a later
// validateDatapathBindings call can confirm its TC filter(s) are attached.
// Duplicate (name+label+filters) entries are ignored, which keeps the recorded
// set stable when an interface is re-bound during self-heal.
func (c *controlPlaneCore) recordBoundIface(name, label string, filters ...boundFilter) {
	c.datapathMu.Lock()
	defer c.datapathMu.Unlock()
	for _, b := range c.datapathIfaces {
		if b.name == name && b.label == label && equalBoundFilters(b.filters, filters) {
			return
		}
	}
	c.datapathIfaces = append(c.datapathIfaces, boundIface{name: name, label: label, filters: filters})
}

// equalBoundFilters reports whether two boundFilter slices contain the same
// (handle, prog) pairs in the same order.
func equalBoundFilters(a, b []boundFilter) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].handle != b[i].handle || a[i].prog != b[i].prog {
			return false
		}
	}
	return true
}

// resetBoundIfaces clears the recorded bindings so each commitInterfaceBindings
// run validates only the interfaces bound during that run.
func (c *controlPlaneCore) resetBoundIfaces() {
	c.datapathMu.Lock()
	c.datapathIfaces = nil
	c.datapathMu.Unlock()
}

// validateDatapathBindings verifies that TC filters are actually attached on
// every interface that was successfully bound during commitInterfaceBindings.
// This catches silent failures where bindLan/bindWan/bindDaens partially
// succeed (e.g. qdisc missing → FilterAdd silently fails, or the interface
// disappeared between lookup and attach). It validates only the *resolved*
// interface names recorded by the bind functions (so a configured "auto" LAN
// is expanded to the real NIC, and dae0 is only checked when it was actually
// created), which keeps it correct in both unit-test and production settings.
//
// The returned bool reports whether a *fatal* binding is missing — currently
// only the dae0 host veth, which dae itself creates and fully controls. A
// missing dae0 filter means the transparent proxy cannot function at all, so
// it aborts startup/reload. LAN/WAN filters depend on the host environment
// (e.g. clsact availability on user interfaces, which can be unavailable when
// dae runs inside a container), so a missing LAN/WAN filter is reported but
// does not abort — it is surfaced as a warning so operators still get
// visibility into silent datapath breakage without breaking environments
// where such filters legitimately cannot be attached.
// checkBindingInNetns performs the actual link lookup and TC-filter check for
// bi in whatever network namespace the caller is currently executing in. Every
// handle recorded for bi must be attached; the first missing one is reported.
// It is separated from checkBinding so the dae0peer case can run it inside the
// dae netns (via GetDaeNetns().WithRequired), while all host-netns interfaces
// run it directly.
func (c *controlPlaneCore) checkBindingInNetns(bi boundIface) (ok bool, reason string) {
	link, err := linkByName(bi.name)
	if err != nil {
		return false, fmt.Sprintf("%s (%s, link not found)", bi.name, bi.label)
	}
	// dae0/dae0peer attach only an ingress filter; LAN/WAN attach both ingress
	// and egress. Scope the enumeration accordingly to skip a wasted FilterList.
	parents := []uint32{netlink.HANDLE_MIN_INGRESS, netlink.HANDLE_MIN_EGRESS}
	if bi.label == "dae0" || bi.label == "dae0peer" {
		parents = []uint32{netlink.HANDLE_MIN_INGRESS}
	}
	for _, bf := range bi.filters {
		if !hasDaeTcFilter(link, bf.handle, parents, bf.prog) {
			return false, fmt.Sprintf("%s (%s, handle 0x%x missing)", bi.name, bi.label, bf.handle)
		}
	}
	return true, ""
}

// checkBinding reports whether the TC filter(s) for bi are actually attached,
// returning a human-readable reason when not. dae0peer lives inside the dae
// netns (host-netns linkByName cannot see it), so its check is executed there;
// all other interfaces are resolved in the host netns.
func (c *controlPlaneCore) checkBinding(bi boundIface) (ok bool, reason string) {
	if bi.label == "dae0peer" {
		ns := GetDaeNetns()
		if ns == nil {
			// Defensive: the dae netns must exist by the time bindings are
			// validated, but a nil receiver here would panic the whole check.
			return false, fmt.Sprintf("%s (%s): dae netns not yet initialised", bi.name, bi.label)
		}
		var innerOk bool
		var innerReason string
		if err := ns.WithRequired("check dae0peer binding", func() error {
			innerOk, innerReason = c.checkBindingInNetns(bi)
			return nil
		}); err != nil {
			return false, fmt.Sprintf("%s (%s, dae netns error: %v)", bi.name, bi.label, err)
		}
		return innerOk, innerReason
	}
	return c.checkBindingInNetns(bi)
}

// missingBindings returns the recorded interfaces whose TC filter is not
// actually attached, plus whether a *fatal* binding (dae0) is among them.
// dae0 is created and fully controlled by dae, so a missing dae0 filter means
// the transparent proxy cannot function at all; LAN/WAN depend on the host
// environment (e.g. clsact availability) and are not fatal.
func (c *controlPlaneCore) missingBindings() (missing []boundIface, fatal bool) {
	c.datapathMu.Lock()
	ifaces := make([]boundIface, len(c.datapathIfaces))
	copy(ifaces, c.datapathIfaces)
	c.datapathMu.Unlock()

	for _, bi := range ifaces {
		if ok, _ := c.checkBinding(bi); !ok {
			missing = append(missing, bi)
			if bi.label == "dae0" {
				fatal = true
			}
		}
	}
	return missing, fatal
}

// validateDatapathBindings is a diagnostic entry point used by unit tests to
// assert that all recorded TC filters are attached. In production the bind
// path uses missingBindings()/repairDatapathBindings() instead; this helper
// is intentionally not part of the live startup/reload flow (see L6 review note).
func (c *controlPlaneCore) validateDatapathBindings() (missing []string, fatal bool) {
	bad, fatal := c.missingBindings()
	for _, bi := range bad {
		_, reason := c.checkBinding(bi)
		missing = append(missing, reason)
	}
	return missing, fatal
}

// rebindLanFn / rebindWanFn are package-level indirection over the actual bind
// functions so unit tests can substitute stubs without touching the real
// netlink stack. In production they call _bindLan / _bindWan directly (which
// ensure the clsact qdisc and re-attach the filter, both idempotent).
var rebindLanFn = func(c *controlPlaneCore, name string) error { return c._bindLan(name) }
var rebindWanFn = func(c *controlPlaneCore, name string) error { return c._bindWan(name) }

// repairDatapathBindings detects missing TC filters and attempts to re-attach
// them automatically (self-heal) for LAN/WAN interfaces, then re-validates
// once. dae0 is intentionally never self-healed: a missing dae0 filter is a
// fatal, deep failure and is reported so the caller can abort.
//
// It returns the interfaces that are *still* missing after the re-attach
// attempt, plus fatal (true only when dae0 is among the still-missing), so
// callers keep the existing fatal/warning policy unchanged:
//   - dae0 missing  -> fatal -> caller aborts startup/reload
//   - LAN/WAN missing but re-attached -> silently recovered (Debugf)
//   - LAN/WAN missing and re-attach failed -> warning, proxy keeps running
func (c *controlPlaneCore) repairDatapathBindings() (stillMissing []string, fatal bool) {
	bad, _ := c.missingBindings()
	if len(bad) == 0 {
		return nil, false
	}
	repairedCount := 0
	for _, bi := range bad {
		switch bi.label {
		case "LAN":
			if err := rebindLanFn(c, bi.name); err != nil {
				c.log.Warnf("datapath self-heal: failed to re-bind LAN %s: %v", bi.name, err)
			} else {
				repairedCount++
			}
		case "WAN":
			if err := rebindWanFn(c, bi.name); err != nil {
				c.log.Warnf("datapath self-heal: failed to re-bind WAN %s: %v", bi.name, err)
			} else {
				repairedCount++
			}
		case "dae0peer":
			// dae0peer lives in the dae netns and is created/controlled by dae,
			// like dae0. It is not self-healed here; a missing dae0peer filter is
			// surfaced as a (non-fatal) warning so operators keep visibility.
		}
		// dae0: not self-healed.
	}
	// Re-check once (single pass, no retry loop to avoid livelock).
	stillBad, fatal := c.missingBindings()
	for _, bi := range stillBad {
		_, reason := c.checkBinding(bi)
		stillMissing = append(stillMissing, reason)
	}
	// Unified logging exit (O4 review note): success is promoted to Info with a
	// recovered count; any remaining gaps are warned here ONCE so the callers
	// don't duplicate the stillMissing warning.
	if len(stillMissing) == 0 {
		if repairedCount > 0 {
			c.log.Infof("datapath self-heal: recovered %d missing TC filter(s)", repairedCount)
		}
	} else {
		c.log.Warnf("datapath self-heal: %d filter(s) still missing after self-heal: %v", len(stillMissing), stillMissing)
	}
	return stillMissing, fatal
}

// filterLister lists TC filters attached to link on the given parent. It is a
// package-level variable (defaulting to netlink.FilterList) so tests can swap
// in a mock without touching the real netlink stack.
var filterLister = netlink.FilterList

// hasDaeTcFilter reports whether a healthy TC filter with the exact given full
// handle (major<<16 | minor, e.g. 0x20220002 for dae0, 0x20230004/0x20230002
// for the LAN/WAN ingress/egress pair) is attached on link within the given
// parents. Comparing the full handle (not just the major) lets
// validateDatapathBindings distinguish the two filters LAN/WAN carry under the
// same major.
//
// When expectProg is non-nil and its kernel program id can be resolved, the
// filter at the matching handle must also point at that program id. This is the
// program-aware self-check: after a reload, a TC filter handle can survive while
// the program it references becomes stale/invalid (the veth/netkit reload
// black-hole), leaving a "handle present but datapath dead" zombie. Counting the
// handle alone would report such a binding as healthy; verifying the program id
// catches it so repairDatapathBindings can re-attach. If the id cannot be
// resolved (expectProg nil, Info() error, or the kernel not reporting
// TCA_BPF_ID) it degrades gracefully to a handle-only presence check, so it
// never regresses below the previous behaviour.
//
// parents scopes the netlink enumeration: dae0/dae0peer carry only an ingress
// filter, so callers pass [HANDLE_MIN_INGRESS] for them; LAN/WAN carry both an
// ingress and an egress filter, so callers pass both. This avoids one wasted
// FilterList call per binding on interfaces that never have an egress filter.
func hasDaeTcFilter(link netlink.Link, handle uint32, parents []uint32, expectProg *ebpf.Program) bool {
	expectID, haveID := expectedProgID(expectProg)
	for _, parent := range parents {
		filters, err := filterLister(link, parent)
		if err != nil {
			continue // no clsact qdisc attached
		}
		for _, f := range filters {
			if f.Attrs().Handle != handle {
				continue
			}
			if !haveID {
				// Cannot verify program identity; a matching handle is
				// sufficient (previous behaviour).
				return true
			}
			bf, ok := f.(*netlink.BpfFilter)
			if !ok {
				// Not a bpf filter (e.g. a mocked GenericFilter in tests, or an
				// unexpected filter type); do not second-guess a handle match.
				return true
			}
			if bf.Id == 0 {
				// Kernel did not report the program id for this filter; fall
				// back to a handle-only match rather than false-alarming.
				return true
			}
			if bf.Id == expectID {
				return true // handle and program id both match: healthy
			}
			// A filter exists at this handle but points at a different (stale)
			// program. Handles are unique per parent, so keep scanning the other
			// parent (if any) rather than accepting this zombie.
		}
	}
	return false
}

// expectedProgID resolves the kernel program id of prog, returning ok=false if
// it cannot be determined (nil program, Info() error, or an id the kernel does
// not expose). Callers treat ok=false as "unverifiable" and fall back to a
// handle-only check.
func expectedProgID(prog *ebpf.Program) (id int, ok bool) {
	if prog == nil {
		return 0, false
	}
	info, err := prog.Info()
	if err != nil {
		return 0, false
	}
	pid, exposed := info.ID()
	if !exposed {
		return 0, false
	}
	return int(pid), true
}

// tryDeleteFlippedFilter deletes the TC filter obtained by flipping the
// low bit of the handle. Used during non-reload startup to remove any
// stale filter from a previous run that used the opposite flip value.
func tryDeleteFlippedFilter(f *netlink.BpfFilter) {
	flipped := deepcopy.Copy(f).(*netlink.BpfFilter)
	flipped.Handle ^= 1
	_ = netlink.FilterDel(flipped)
}

// extractIpsFromDnsCache returns the unique, valid non-unspecified IP addresses
// contained in the A/AAAA records of a DNS cache entry.
func extractIPsFromDnsCache(cache *DnsCache) []netip.Addr {
	if cache == nil || len(cache.Answer) == 0 {
		return nil
	}
	ips := make([]netip.Addr, 0, len(cache.Answer))
	for _, ans := range cache.Answer {
		ip, ok := dnsAnswerIP(ans)
		if !ok || ip.IsUnspecified() {
			continue
		}
		ips = append(ips, ip)
	}
	return ips
}

// BatchUpdateDomainRouting update bpf map domain_routing. Since one IP may have multiple domains, this function should
// be invoked every A/AAAA-record lookup.
func (c *controlPlaneCore) BatchUpdateDomainRouting(cache *DnsCache) error {
	if c == nil || cache == nil {
		return nil
	}
	if c.domainRouting == nil {
		c.domainRouting = newDomainRoutingTracker()
	}
	snapshot, err := buildDomainRoutingOwnerSnapshot(cache)
	if err != nil {
		return err
	}
	bpf := c.PeekBpf()
	if bpf == nil {
		return nil
	}
	return c.domainRouting.syncOwner(bpf.DomainRoutingMap, cache.RouteOwnerKey, snapshot)
}

// BatchRemoveDomainRouting remove bpf map domain_routing.
func (c *controlPlaneCore) BatchRemoveDomainRouting(cache *DnsCache) error {
	if c == nil || cache == nil {
		return nil
	}
	if c.domainRouting == nil {
		c.domainRouting = newDomainRoutingTracker()
	}
	bpf := c.PeekBpf()
	if bpf == nil {
		return nil
	}
	return c.domainRouting.syncOwner(bpf.DomainRoutingMap, cache.RouteOwnerKey, domainRoutingOwnerSnapshot{})
}

func (c *controlPlaneCore) RetainUdpConnStateTuples(keys []bpfTuplesKey) {
	if tracker := c.getUdpConnStateTracker(); tracker != nil {
		tracker.Retain(keys)
	}
}

func (c *controlPlaneCore) TransferRetainedUdpConnStateTuplesFrom(previous udpConnStateOwner, keys []bpfTuplesKey) {
	if c == nil || previous == nil || previous == c || len(keys) == 0 {
		return
	}

	previousCore, ok := previous.(*controlPlaneCore)
	if !ok || previousCore == nil {
		return
	}

	currentTracker := c.getUdpConnStateTracker()
	previousTracker := previousCore.getUdpConnStateTracker()
	if currentTracker == nil || previousTracker == nil || currentTracker == previousTracker {
		return
	}

	currentTracker.Retain(keys)
	previousTracker.Forget(keys)
}

func (c *controlPlaneCore) ReleaseUdpConnStateTuples(keys []bpfTuplesKey) error {
	if c == nil || len(keys) == 0 {
		return nil
	}
	tracker := c.getUdpConnStateTracker()
	if tracker == nil {
		bpf := c.PeekBpf()
		if bpf == nil || bpf.ConnStateMap == nil {
			return nil
		}
		_, err := BpfMapBatchDelete(bpf.ConnStateMap, keys)
		return err
	}
	releases := tracker.BeginRelease(keys)
	defer tracker.FinalizeRelease(releases)
	if len(releases) == 0 {
		return nil
	}
	bpf := c.PeekBpf()
	if bpf == nil || bpf.ConnStateMap == nil {
		return nil
	}
	deleteKeys := make([]bpfTuplesKey, 0, len(releases))
	for _, release := range releases {
		deleteKeys = append(deleteKeys, release.key)
	}
	_, err := BpfMapBatchDelete(bpf.ConnStateMap, deleteKeys)
	return err
}

// IsBpfEjected reports whether BPF ownership has been transferred to another
// generation via EjectBpf. When true, core.Close() is purely cleanup (TC
// filter detach, UDP tracker release) and can run asynchronously without
// risking the closeTail timeout.
func (c *controlPlaneCore) IsBpfEjected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bpfEjected
}

// EjectBpf will resect bpf from destroying life-cycle of control plane core.
func (c *controlPlaneCore) EjectBpf() *bpfObjects {
	c.mu.Lock()
	defer c.mu.Unlock()
	bpf := c.bpf.Load()
	if c.bpfEjected {
		return bpf
	}

	// Transfer ownership: this generation is no longer responsible for closing BPF.
	c.bpfOwned = false
	c.bpfEjected = true

	// Stop link watcher immediately during handover period to avoid race condition
	// between old and new control planes reacting to link events (e.g. PPPoE flapping).
	_ = c.ifmgr.Close()

	return bpf
}

func (c *controlPlaneCore) EjectLpmIndices() []uint32 {
	c.mu.Lock()
	defer c.mu.Unlock()

	indices := c.lpmTrieIndices
	c.lpmTrieIndices = nil
	return indices
}

// InheritLpmIndices adopts retired generations' ring slots. Slots that are no
// longer referenced by the current generation are deleted immediately to free
// memory; slots already reused by the current generation are skipped.
func (c *controlPlaneCore) InheritLpmIndices(indices []uint32) {
	if len(indices) == 0 {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	bpf := c.bpf.Load()

	current := make(map[uint32]struct{}, len(c.lpmTrieIndices))
	for _, idx := range c.lpmTrieIndices {
		current[idx] = struct{}{}
	}

	pending := make([]uint32, 0, len(indices))
	for _, idx := range indices {
		if _, reused := current[idx]; reused {
			continue
		}
		if bpf == nil || bpf.LpmArrayMap == nil {
			pending = append(pending, idx)
			current[idx] = struct{}{}
			continue
		}
		if err := bpf.LpmArrayMap.Delete(idx); err != nil && !errors.Is(err, ebpf.ErrKeyNotExist) {
			c.log.Errorf("Failed to clear inherited BPF LPM slot %d: %v", idx, err)
			pending = append(pending, idx)
			current[idx] = struct{}{}
		}
	}
	c.lpmTrieIndices = append(c.lpmTrieIndices, pending...)
}

// ReplaceLpmIndices installs a new active LPM index set for this generation
// and eagerly reclaims the superseded indices when possible.
func (c *controlPlaneCore) ReplaceLpmIndices(indices []uint32) {
	c.mu.Lock()
	bpf := c.bpf.Load()
	old := c.lpmTrieIndices
	c.lpmTrieIndices = append([]uint32(nil), indices...)
	shouldCleanupOld := bpf != nil && bpf.LpmArrayMap != nil
	c.mu.Unlock()

	if shouldCleanupOld {
		c.InheritLpmIndices(old)
	}
}

// InjectBpf will inject bpf back.
func (c *controlPlaneCore) InjectBpf(bpf *bpfObjects) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if bpf != nil {
		c.bpf.Store(bpf)
	}
	c.bpfEjected = false
	c.bpfOwned = true
}

// PeekBpf returns the current BPF objects without transferring ownership.
// Background maintenance paths such as janitors and health checks should use
// this accessor instead of EjectBpf to avoid disturbing reload lifecycle.
func (c *controlPlaneCore) PeekBpf() *bpfObjects {
	return c.bpf.Load()
}
