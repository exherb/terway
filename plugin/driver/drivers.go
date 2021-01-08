package driver

import (
	"fmt"
	"net"
	"os"
	"syscall"

	"github.com/AliyunContainerService/terway/pkg/tc"
	"github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/pkg/errors"
	"github.com/vishvananda/netlink"
	k8snet "k8s.io/apimachinery/pkg/util/net"
)

// drivers implement objects
var (
	VethDriver   NetnsDriver = &vethDriver{}
	NicDriver    NetnsDriver = &rawNicDriver{}
	IPVlanDriver NetnsDriver = newIPVlanDriver()
)

type RecordPodEvent func(msg string)

type CheckConfig struct {
	RecordPodEvent

	NetNS ns.NetNS

	HostVethName string
	DeviceID     int32 // phy device
	MTU          int

	ContainerIFName string
	// for pod
	IPv4Addr *net.IPNet
	Gateway  net.IP
}

// NetnsDriver to config container netns interface and routes
type NetnsDriver interface {
	Setup(hostVeth string,
		containerVeth string,
		ipv4Addr *net.IPNet,
		primaryIpv4Addr net.IP,
		serviceCIDR *net.IPNet,
		hostStackCIDRs []*net.IPNet,
		gateway net.IP,
		extraRoutes []*types.Route,
		deviceID int,
		ingress uint64,
		egress uint64,
		mtu int,
		netNS ns.NetNS) error

	Teardown(hostVeth string,
		containerVeth string,
		netNS ns.NetNS,
		containerIP net.IP) error

	Check(cfg *CheckConfig) error
}

type vethDriver struct {
}

const (
	// mainRouteTable the system "main" route table id
	mainRouteTable        = 254
	toContainerPriority   = 512
	fromContainerPriority = 2048
)

var (
	_, defaultRoute, _ = net.ParseCIDR("0.0.0.0/0")
	linkIP             = &net.IPNet{
		IP:   net.IPv4(169, 254, 1, 1),
		Mask: net.CIDRMask(32, 32),
	}
)

func (d *vethDriver) Setup(
	hostIfName string,
	containerVeth string,
	ipv4Addr *net.IPNet,
	primaryIpv4Addr net.IP,
	serviceCIDR *net.IPNet,
	hostStackCIDRs []*net.IPNet,
	gateway net.IP,
	extraRoutes []*types.Route,
	deviceID int,
	ingress uint64,
	egress uint64,
	mtu int,
	netNS ns.NetNS) error {
	var err error
	preHostLink, err := netlink.LinkByName(hostIfName)
	if err == nil {
		if err = netlink.LinkDel(preHostLink); err != nil {
			return errors.Wrap(err, "vethDriver, error delete previous link")
		}
	}

	hostNs, err := ns.GetCurrentNS()
	if err != nil {
		return errors.Wrap(err, "vethDriver, error get current netns")
	}

	var (
		hostVeth net.Interface
		contVeth net.Interface
	)

	// config in container netns
	err = netNS.Do(func(_ ns.NetNS) error {
		// 1. create veth pair
		hostVeth, contVeth, err = setupVethPair(containerVeth, hostIfName, mtu, hostNs)
		if err != nil {
			return errors.Wrap(err, "vethDriver, error create veth pair for container")
		}

		// 2. add address for container interface
		var contLink netlink.Link
		contLink, err = netlink.LinkByName(contVeth.Name)
		if err != nil {
			return errors.Wrap(err, "vethDriver, error get veth pair in container ns")
		}
		err = netlink.AddrAdd(contLink, &netlink.Addr{
			IPNet: &net.IPNet{
				IP:   ipv4Addr.IP,
				Mask: net.CIDRMask(32, 32),
			},
		})
		if err != nil {
			return errors.Wrap(err, "error add addr for container veth")
		}

		// 3. add route and neigh for container
		err = netlink.NeighAdd(&netlink.Neigh{
			LinkIndex:    contLink.Attrs().Index,
			IP:           linkIP.IP,
			HardwareAddr: hostVeth.HardwareAddr,
			State:        netlink.NUD_PERMANENT,
			Family:       syscall.AF_INET,
		})
		if err != nil {
			return errors.Wrap(err, "error add permanent arp for container veth")
		}

		err = netlink.RouteAdd(&netlink.Route{
			LinkIndex: contLink.Attrs().Index,
			Scope:     netlink.SCOPE_UNIVERSE,
			Flags:     int(netlink.FLAG_ONLINK),
			Dst:       defaultRoute,
			Gw:        linkIP.IP,
		})
		if err != nil {
			return errors.Wrap(err, "error add route for container veth")
		}

		if len(extraRoutes) != 0 {
			err = netlink.RouteAdd(&netlink.Route{
				LinkIndex: contLink.Attrs().Index,
				Scope:     netlink.SCOPE_LINK,
				Dst:       linkIP,
			})
			if err != nil {
				return errors.Wrap(err, "error add route for container veth")
			}
		}

		for _, extraRoute := range extraRoutes {
			err = netlink.RouteAdd(&netlink.Route{
				LinkIndex: contLink.Attrs().Index,
				Scope:     netlink.SCOPE_UNIVERSE,
				Flags:     int(netlink.FLAG_ONLINK),
				Dst:       &extraRoute.Dst,
				Gw:        linkIP.IP,
			})
			if err != nil {
				return errors.Wrapf(err, "error add extra route for container veth")
			}
		}

		if egress > 0 {
			return d.setupTC(contLink, egress)
		}
		return nil
	})

	if err != nil {
		return errors.Wrap(err, "error config veth in container netns")
	}

	// config in host netns
	hostLink, err := netlink.LinkByName(hostVeth.Name)
	if err != nil {
		return errors.Wrap(err, "vethDriver, error get veth pair in host ns")
	}

	err = netlink.LinkSetUp(hostLink)
	if err != nil {
		return errors.Wrap(err, "vethDriver, error set veth pair in host ns up")
	}

	// 1. config to container routes
	containerDst := &net.IPNet{
		IP:   ipv4Addr.IP,
		Mask: net.CIDRMask(32, 32),
	}
	err = EnsureHostToContainerRoute(containerDst, hostLink.Attrs().Index)
	if err != nil {
		return err
	}

	if len(extraRoutes) != 0 {
		err = netlink.AddrAdd(hostLink, &netlink.Addr{
			IPNet: linkIP,
		})
		if err != nil {
			return errors.Wrap(err, "error add extra addr for host veth")
		}
	}

	// 2. config from container routes
	if deviceID != 0 {
		var parentLink netlink.Link
		parentLink, err = netlink.LinkByIndex(deviceID)
		if err != nil {
			return errors.Wrapf(err, "vethDriver, error get eni parent Link, deviceID: %v", deviceID)
		}

		tableID := getRouteTableID(parentLink.Attrs().Index)

		// ensure eni config
		err = d.ensureEniConfig(parentLink, mtu, tableID, gateway)
		if err != nil {
			return errors.Wrapf(err, "vethDriver, fail ensure eni config")
		}

		ruleList, err := netlink.RuleList(netlink.FAMILY_V4)
		if err != nil {
			return errors.Wrapf(err, "vethDriver, fail list rule")
		}

		// check exist rule
		for _, rule := range ruleList {
			if ipNetEqual(containerDst, rule.Src) || ipNetEqual(containerDst, rule.Dst) {
				err = netlink.RuleDel(&rule)
				if os.IsNotExist(err) {
					// remove orphan rule with veth interface detached
					rule.IifName = ""
					err = netlink.RuleDel(&rule)
					if err != nil {
						return errors.Wrapf(err, "vethDriver, error clean up exist rule remove veth name not exist: %+v", rule)
					}
				} else if err != nil {
					return errors.Wrapf(err, "vethDriver, error clean up exist rule: %+v", rule)
				}
			}
		}

		// to container rule
		toContainerRule := netlink.NewRule()
		toContainerRule.Dst = containerDst
		toContainerRule.Table = mainRouteTable
		toContainerRule.Priority = toContainerPriority

		err = netlink.RuleAdd(toContainerRule)
		if err != nil {
			return errors.Wrapf(err, "vethDriver, fail add container add rule")
		}

		// from container rule
		fromContainerRule := netlink.NewRule()
		fromContainerRule.IifName = hostIfName
		fromContainerRule.Src = containerDst
		fromContainerRule.Table = tableID
		fromContainerRule.Priority = fromContainerPriority
		err = netlink.RuleAdd(fromContainerRule)
		if err != nil {
			return errors.Wrapf(err, "vethDriver, fail add container add rule")
		}
	}

	if ingress > 0 {
		return d.setupTC(hostLink, ingress)
	}

	return nil
}

func (d *vethDriver) setupTC(dev netlink.Link, bandwidthInBytes uint64) error {
	rule := &tc.TrafficShapingRule{
		Rate: bandwidthInBytes,
	}
	return tc.SetRule(dev, rule)
}

func (d *vethDriver) ensureEniConfig(eni netlink.Link, mtu, tableID int, gw net.IP) error {
	var err error
	// set link up
	_, err = EnsureLinkUp(eni)
	if err != nil {
		return errors.Wrapf(err, "error set eni parent link up")
	}
	nodeIPNet := *linkIP
	if nodeIP, err := k8snet.ChooseBindAddress(nil); err == nil {
		nodeIPNet.IP = nodeIP
	}

	// ensure mtu setting
	if eni.Attrs().MTU != mtu {
		if err = netlink.LinkSetMTU(eni, mtu); err != nil {
			return errors.Wrapf(err, "error set eni parent link mtu to %v", mtu)
		}
	}

	// remove eni extra address
	addrDel := 0
	addrs, err := netlink.AddrList(eni, netlink.FAMILY_ALL)
	if err != nil {
		return fmt.Errorf("error list address for eni: %v", err)
	}
	for _, addr := range addrs {
		if !addr.IP.Equal(nodeIPNet.IP) {
			err = netlink.AddrDel(eni, &addr)
			if err != nil {
				return fmt.Errorf("error remove extra address for eni: %v", err)
			}
			addrDel++
		}
	}

	if addrDel == len(addrs) {
		err = netlink.AddrAdd(eni, &netlink.Addr{
			IPNet: &nodeIPNet,
		})
		if err != nil {
			return fmt.Errorf("error set address for eni: %v", err)
		}
	}

	// ensure default route
	eniDefaultRoute, err := netlink.RouteListFiltered(netlink.FAMILY_ALL,
		&netlink.Route{
			Table: tableID,
			Dst:   nil,
		}, netlink.RT_FILTER_TABLE|netlink.RT_FILTER_DST)
	if err != nil {
		return fmt.Errorf("error list route for eni route table: %v", err)
	}

	routeDelete := 0
	for _, route := range eniDefaultRoute {
		if route.LinkIndex != eni.Attrs().Index {
			err = netlink.RouteDel(&route)
			if err != nil {
				return fmt.Errorf("error deletion conflict default route, %v, route: %+v", err, route)
			}
			routeDelete++
		}
	}
	if routeDelete == len(eniDefaultRoute) {
		err = netlink.RouteAdd(
			&netlink.Route{
				LinkIndex: eni.Attrs().Index,
				Scope:     netlink.SCOPE_UNIVERSE,
				Dst:       defaultRoute,
				Table:     tableID,
				Flags:     int(netlink.FLAG_ONLINK),
				Gw:        gw,
			},
		)
		if err != nil {
			return fmt.Errorf("error add default route for eni, %v", err)
		}
	}
	return nil
}

func (d *vethDriver) Teardown(hostIfName string,
	containerVeth string,
	netNS ns.NetNS,
	_ net.IP) error {
	var (
		hostVeth netlink.Link
		err      error
	)
	if hostVeth, err = netlink.LinkByName(hostIfName); err != nil {
		return errors.Wrapf(err, "vethDriver, error found host side link")
	}

	// 1. get container ip
	containerIP, err := getNSIP(containerVeth, netNS)
	if err != nil {
		return errors.Wrapf(err, "vethDriver, error get container ip")
	}

	// 2. fixme remove ingress/egress rule for pod ip

	// found table for container
	ruleList, err := netlink.RuleList(netlink.FAMILY_V4)
	if err != nil {
		return errors.Wrapf(err, "failed list ip rule from netlink")
	}

	var toContainerRule *netlink.Rule
	var fromContainerRule *netlink.Rule

	for _, rule := range ruleList {
		var bits int
		ruleInner := rule
		if ruleInner.Src != nil {
			_, bits = ruleInner.Src.Mask.Size()
			if bits == len(ruleInner.Src.IP)*8 && ruleInner.Src.IP.Equal(containerIP) {
				fromContainerRule = &ruleInner
			}
		}

		if ruleInner.Dst != nil {
			_, bits = ruleInner.Dst.Mask.Size()
			if bits == len(ruleInner.Dst.IP)*8 && ruleInner.Dst.IP.Equal(containerIP) {
				toContainerRule = &ruleInner
			}
		}
	}

	// 4. cleanup policy route of route tables of containerip
	if toContainerRule != nil {
		err = netlink.RuleDel(toContainerRule)
		if err != nil {
			return errors.Wrapf(err, "VethDriver, error clean up policy rule for container")
		}
	}

	if fromContainerRule != nil {
		err = netlink.RuleDel(fromContainerRule)
		if err != nil {
			return errors.Wrapf(err, "VethDriver, error clean up policy rule for container")
		}
	}

	// 4. remove container veth
	return netlink.LinkDel(hostVeth)
}

func (d *vethDriver) Check(cfg *CheckConfig) error {
	err := cfg.NetNS.Do(func(netNS ns.NetNS) error {
		link, err := netlink.LinkByName(cfg.ContainerIFName)
		if err != nil {
			return err
		}
		if link.Type() != "veth" {
			return fmt.Errorf("link type mismatch want veth, got %s", link.Type())
		}
		return nil
	})
	if err != nil {
		cfg.RecordPodEvent(fmt.Sprintf("veth driver failed to check nic %#v", err))
		return nil
	}
	containerDst := &net.IPNet{
		IP:   cfg.IPv4Addr.IP,
		Mask: net.CIDRMask(32, 32),
	}

	vethHostLink, err := netlink.LinkByName(cfg.HostVethName)
	if err != nil {
		Log.Debugf("can't found veth %s on host", cfg.HostVethName)
		if os.IsNotExist(err) {
			cfg.RecordPodEvent(fmt.Sprintf("can't found veth %s on host", cfg.HostVethName))
		}
		return nil
	}

	err = EnsureHostToContainerRoute(containerDst, vethHostLink.Attrs().Index)
	if err != nil {
		return nil
	}
	if cfg.DeviceID == 0 {
		Log.Debugf("device id=0 invalid")
		return nil
	}

	// sync policy route
	parentLink, err := netlink.LinkByIndex(int(cfg.DeviceID))
	if err != nil {
		cfg.RecordPodEvent(fmt.Sprintf("failed to get nic by id %d %#v", cfg.DeviceID, err))
		Log.Debugf("failed to get nic by id %d %#v", cfg.DeviceID, err)
		return nil
	}
	tableID := getRouteTableID(parentLink.Attrs().Index)
	// ensure eni config
	err = d.ensureEniConfig(parentLink, cfg.MTU, tableID, cfg.Gateway)
	if err != nil {
		Log.Debug(errors.Wrapf(err, "vethDriver, fail ensure eni config"))
		return nil
	}

	ruleList, err := netlink.RuleList(netlink.FAMILY_V4)
	if err != nil {
		Log.Debug(errors.Wrapf(err, "vethDriver, fail list rule"))
		return nil
	}

	// to container rule
	toContainerRule := netlink.NewRule()
	toContainerRule.Dst = containerDst
	toContainerRule.Table = mainRouteTable
	toContainerRule.Priority = toContainerPriority

	found := false
	for _, got := range ruleList {
		if !ipNetEqual(containerDst, got.Dst) {
			continue
		}
		if got.Table != toContainerRule.Table ||
			got.Priority != toContainerRule.Priority {
			Log.Debugf("del %s", got.String())
			err := netlink.RuleDel(&got)
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				Log.Debugf("failed to del rule %s, %v", got.String(), err)
				continue
			}
		}
		found = true
	}
	if !found {
		Log.Debugf("add %s", toContainerRule.String())
		err := netlink.RuleAdd(toContainerRule)
		if err != nil {
			return errors.Wrapf(err, "vethDriver, fail add container add rule")
		}
	}

	// from container rule
	fromContainerRule := netlink.NewRule()
	fromContainerRule.IifName = cfg.HostVethName
	fromContainerRule.Src = containerDst
	fromContainerRule.Table = tableID
	fromContainerRule.Priority = fromContainerPriority
	found = false
	for _, got := range ruleList {
		if !ipNetEqual(fromContainerRule.Src, got.Src) {
			continue
		}
		if got.IifName != fromContainerRule.IifName ||
			got.Table != fromContainerRule.Table ||
			got.Priority != fromContainerRule.Priority {
			Log.Debugf("del %s", got.String())
			err := netlink.RuleDel(&got)
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				Log.Debugf("failed to del rule %s, %v", got.String(), err)
				continue
			}
		}
		found = true
	}
	if !found {
		Log.Debugf("add %s", fromContainerRule.String())
		err := netlink.RuleAdd(fromContainerRule)
		if err != nil {
			return errors.Wrapf(err, "vethDriver, fail add container add rule")
		}
	}

	return nil
}

func setupVethPair(contVethName, pairName string, mtu int, hostNS ns.NetNS) (net.Interface, net.Interface, error) {
	contVeth, err := makeVethPair(contVethName, pairName, mtu)
	if err != nil {
		return net.Interface{}, net.Interface{}, err
	}

	if err = netlink.LinkSetUp(contVeth); err != nil {
		return net.Interface{}, net.Interface{}, fmt.Errorf("failed to set %q up: %v", contVethName, err)
	}

	hostVeth, err := netlink.LinkByName(pairName)
	if err != nil {
		return net.Interface{}, net.Interface{}, fmt.Errorf("failed to lookup %q: %v", pairName, err)
	}

	if err = netlink.LinkSetNsFd(hostVeth, int(hostNS.Fd())); err != nil {
		return net.Interface{}, net.Interface{}, fmt.Errorf("failed to move veth to host netns: %v", err)
	}

	err = hostNS.Do(func(_ ns.NetNS) error {
		hostVeth, err = netlink.LinkByName(pairName)
		if err != nil {
			return fmt.Errorf("failed to lookup %q in %q: %v", pairName, hostNS.Path(), err)
		}

		if err = netlink.LinkSetUp(hostVeth); err != nil {
			return fmt.Errorf("failed to set %q up: %v", pairName, err)
		}
		return nil
	})
	if err != nil {
		return net.Interface{}, net.Interface{}, err
	}
	return ifaceFromNetlinkLink(hostVeth), ifaceFromNetlinkLink(contVeth), nil
}

func makeVethPair(name, peer string, mtu int) (netlink.Link, error) {
	veth := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{
			Name:  name,
			Flags: net.FlagUp,
			MTU:   mtu,
		},
		PeerName: peer,
	}
	if err := netlink.LinkAdd(veth); err != nil {
		return nil, err
	}

	return veth, nil
}

func ifaceFromNetlinkLink(l netlink.Link) net.Interface {
	a := l.Attrs()
	return net.Interface{
		Index:        a.Index,
		MTU:          a.MTU,
		Name:         a.Name,
		HardwareAddr: a.HardwareAddr,
		Flags:        a.Flags,
	}
}

func getNSIP(ifName string, nsHandler ns.NetNS) (net.IP, error) {
	var nsIP net.IP
	err := nsHandler.Do(func(netNS ns.NetNS) error {
		link, err := netlink.LinkByName(ifName)
		if err != nil {
			return err
		}
		addr, err := netlink.AddrList(link, netlink.FAMILY_V4)
		if err != nil {
			return err
		} else if len(addr) != 1 {
			return fmt.Errorf("error get ip from link: %v", ifName)
		}
		nsIP = addr[0].IP
		return nil
	})
	return nsIP, err
}
