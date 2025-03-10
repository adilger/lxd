package network

import (
	"context"
	"encoding/binary"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mdlayher/netx/eui64"
	"github.com/pkg/errors"
	log "gopkg.in/inconshreveable/log15.v2"

	"github.com/lxc/lxd/client"
	"github.com/lxc/lxd/lxd/apparmor"
	"github.com/lxc/lxd/lxd/cluster"
	"github.com/lxc/lxd/lxd/cluster/request"
	"github.com/lxc/lxd/lxd/daemon"
	"github.com/lxc/lxd/lxd/db"
	dbCluster "github.com/lxc/lxd/lxd/db/cluster"
	deviceConfig "github.com/lxc/lxd/lxd/device/config"
	"github.com/lxc/lxd/lxd/device/nictype"
	"github.com/lxc/lxd/lxd/dnsmasq"
	"github.com/lxc/lxd/lxd/dnsmasq/dhcpalloc"
	firewallDrivers "github.com/lxc/lxd/lxd/firewall/drivers"
	"github.com/lxc/lxd/lxd/instance"
	"github.com/lxc/lxd/lxd/ip"
	"github.com/lxc/lxd/lxd/network/acl"
	"github.com/lxc/lxd/lxd/network/openvswitch"
	"github.com/lxc/lxd/lxd/node"
	"github.com/lxc/lxd/lxd/project"
	"github.com/lxc/lxd/lxd/revert"
	"github.com/lxc/lxd/lxd/util"
	"github.com/lxc/lxd/lxd/warnings"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/api"
	"github.com/lxc/lxd/shared/subprocess"
	"github.com/lxc/lxd/shared/validate"
	"github.com/lxc/lxd/shared/version"
)

// ForkdnsServersListPath defines the path that contains the forkdns server candidate file.
const ForkdnsServersListPath = "forkdns.servers"

// ForkdnsServersListFile file that contains the server candidates list.
const ForkdnsServersListFile = "servers.conf"

var forkdnsServersLock sync.Mutex

// bridge represents a LXD bridge network.
type bridge struct {
	common
}

// Type returns the network type.
func (n *bridge) Type() string {
	return "bridge"
}

// DBType returns the network type DB ID.
func (n *bridge) DBType() db.NetworkType {
	return db.NetworkTypeBridge
}

// Config returns the network driver info.
func (n *bridge) Info() Info {
	info := n.common.Info()
	info.AddressForwards = true

	return info
}

// checkClusterWideMACSafe returns whether it is safe to use the same MAC address for the bridge interface on all
// cluster nodes. It is not suitable to use a static MAC address when "bridge.external_interfaces" is non-empty and
// the bridge interface has no IPv4 or IPv6 address set. This is because in a clustered environment the same bridge
// config is applied to all nodes, and if the bridge is being used to connect multiple nodes to the same network
// segment it would cause MAC conflicts to use the the same MAC on all nodes. If an IP address is specified then
// connecting multiple nodes to the same network segment would also cause IP conflicts, so if an IP is defined
// then we assume this is not being done. However if IP addresses are explicitly set to "none" and
// "bridge.external_interfaces" is set then it may not be safe to use a the same MAC address on all nodes.
func (n *bridge) checkClusterWideMACSafe(config map[string]string) error {
	// Fan mode breaks if using the same MAC address on each node.
	if config["bridge.mode"] == "fan" {
		return fmt.Errorf(`Cannot use static "bridge.hwaddr" MAC address in fan mode`)
	}

	// We can't be sure that multiple clustered nodes aren't connected to the same network segment so don't
	// use a static MAC address for the bridge interface to avoid introducing a MAC conflict.
	if config["bridge.external_interfaces"] != "" && config["ipv4.address"] == "none" && config["ipv6.address"] == "none" {
		return fmt.Errorf(`Cannot use static "bridge.hwaddr" MAC address when bridge has no IP addresses and has external interfaces set`)
	}

	return nil
}

// FillConfig fills requested config with any default values.
func (n *bridge) FillConfig(config map[string]string) error {
	// Set some default values where needed.
	if config["bridge.mode"] == "fan" {
		if config["fan.underlay_subnet"] == "" {
			config["fan.underlay_subnet"] = "auto"
		}

		// We enable NAT by default even if address is manually specified.
		if config["ipv4.nat"] == "" {
			config["ipv4.nat"] = "true"
		}
	} else {
		if config["ipv4.address"] == "" {
			config["ipv4.address"] = "auto"
		}

		if config["ipv4.address"] == "auto" && config["ipv4.nat"] == "" {
			config["ipv4.nat"] = "true"
		}

		if config["ipv6.address"] == "" {
			content, err := ioutil.ReadFile("/proc/sys/net/ipv6/conf/default/disable_ipv6")
			if err == nil && string(content) == "0\n" {
				config["ipv6.address"] = "auto"
			}
		}

		if config["ipv6.address"] == "auto" && config["ipv6.nat"] == "" {
			config["ipv6.nat"] = "true"
		}
	}

	// Now replace any "auto" keys with generated values.
	err := n.populateAutoConfig(config)
	if err != nil {
		return errors.Wrapf(err, "Failed generating auto config")
	}

	return nil
}

// populateAutoConfig replaces "auto" in config with generated values.
func (n *bridge) populateAutoConfig(config map[string]string) error {
	changedConfig := false

	// Now populate "auto" values where needed.
	if config["ipv4.address"] == "auto" {
		subnet, err := randomSubnetV4()
		if err != nil {
			return err
		}

		config["ipv4.address"] = subnet
		changedConfig = true
	}

	if config["ipv6.address"] == "auto" {
		subnet, err := randomSubnetV6()
		if err != nil {
			return err
		}

		config["ipv6.address"] = subnet
		changedConfig = true
	}

	if config["fan.underlay_subnet"] == "auto" {
		subnet, _, err := DefaultGatewaySubnetV4()
		if err != nil {
			return err
		}

		config["fan.underlay_subnet"] = subnet.String()
		changedConfig = true
	}

	// Re-validate config if changed.
	if changedConfig && n.state != nil {
		return n.Validate(config)
	}

	return nil
}

// ValidateName validates network name.
func (n *bridge) ValidateName(name string) error {
	err := validate.IsInterfaceName(name)
	if err != nil {
		return err
	}

	// Apply common name validation that applies to all network types.
	return n.common.ValidateName(name)
}

// Validate network config.
func (n *bridge) Validate(config map[string]string) error {
	// Build driver specific rules dynamically.
	rules := map[string]func(value string) error{
		"bgp.ipv4.nexthop": validate.Optional(validate.IsNetworkAddressV4),
		"bgp.ipv6.nexthop": validate.Optional(validate.IsNetworkAddressV6),

		"bridge.driver": validate.Optional(validate.IsOneOf("native", "openvswitch")),
		"bridge.external_interfaces": validate.Optional(func(value string) error {
			for _, entry := range strings.Split(value, ",") {
				entry = strings.TrimSpace(entry)
				if err := validate.IsInterfaceName(entry); err != nil {
					return errors.Wrapf(err, "Invalid interface name %q", entry)
				}
			}

			return nil
		}),
		"bridge.hwaddr": validate.Optional(validate.IsNetworkMAC),
		"bridge.mtu":    validate.Optional(validate.IsNetworkMTU),
		"bridge.mode":   validate.Optional(validate.IsOneOf("standard", "fan")),

		"fan.overlay_subnet": validate.Optional(validate.IsNetworkV4),
		"fan.underlay_subnet": validate.Optional(func(value string) error {
			if value == "auto" {
				return nil
			}

			return validate.IsNetworkV4(value)
		}),
		"fan.type": validate.Optional(validate.IsOneOf("vxlan", "ipip")),

		"ipv4.address": validate.Optional(func(value string) error {
			if validate.IsOneOf("none", "auto")(value) == nil {
				return nil
			}

			return validate.IsNetworkAddressCIDRV4(value)
		}),
		"ipv4.firewall":     validate.Optional(validate.IsBool),
		"ipv4.nat":          validate.Optional(validate.IsBool),
		"ipv4.nat.order":    validate.Optional(validate.IsOneOf("before", "after")),
		"ipv4.nat.address":  validate.Optional(validate.IsNetworkAddressV4),
		"ipv4.dhcp":         validate.Optional(validate.IsBool),
		"ipv4.dhcp.gateway": validate.Optional(validate.IsNetworkAddressV4),
		"ipv4.dhcp.expiry":  validate.IsAny,
		"ipv4.dhcp.ranges":  validate.Optional(validate.IsNetworkRangeV4List),
		"ipv4.routes":       validate.Optional(validate.IsNetworkV4List),
		"ipv4.routing":      validate.Optional(validate.IsBool),
		"ipv4.ovn.ranges":   validate.Optional(validate.IsNetworkRangeV4List),

		"ipv6.address": validate.Optional(func(value string) error {
			if validate.IsOneOf("none", "auto")(value) == nil {
				return nil
			}

			return validate.IsNetworkAddressCIDRV6(value)
		}),
		"ipv6.firewall":                        validate.Optional(validate.IsBool),
		"ipv6.nat":                             validate.Optional(validate.IsBool),
		"ipv6.nat.order":                       validate.Optional(validate.IsOneOf("before", "after")),
		"ipv6.nat.address":                     validate.Optional(validate.IsNetworkAddressV6),
		"ipv6.dhcp":                            validate.Optional(validate.IsBool),
		"ipv6.dhcp.expiry":                     validate.IsAny,
		"ipv6.dhcp.stateful":                   validate.Optional(validate.IsBool),
		"ipv6.dhcp.ranges":                     validate.Optional(validate.IsNetworkRangeV6List),
		"ipv6.routes":                          validate.Optional(validate.IsNetworkV6List),
		"ipv6.routing":                         validate.Optional(validate.IsBool),
		"ipv6.ovn.ranges":                      validate.Optional(validate.IsNetworkRangeV6List),
		"dns.domain":                           validate.IsAny,
		"dns.mode":                             validate.Optional(validate.IsOneOf("dynamic", "managed", "none")),
		"dns.search":                           validate.IsAny,
		"dns.zone.forward":                     validate.Optional(n.validateZoneName),
		"dns.zone.reverse.ipv4":                validate.Optional(n.validateZoneName),
		"dns.zone.reverse.ipv6":                validate.Optional(n.validateZoneName),
		"raw.dnsmasq":                          validate.IsAny,
		"maas.subnet.ipv4":                     validate.IsAny,
		"maas.subnet.ipv6":                     validate.IsAny,
		"security.acls":                        validate.IsAny,
		"security.acls.default.ingress.action": validate.Optional(validate.IsOneOf(acl.ValidActions...)),
		"security.acls.default.egress.action":  validate.Optional(validate.IsOneOf(acl.ValidActions...)),
		"security.acls.default.ingress.logged": validate.Optional(validate.IsBool),
		"security.acls.default.egress.logged":  validate.Optional(validate.IsBool),
	}

	// Add dynamic validation rules.
	for k := range config {
		// Tunnel keys have the remote name in their name, extract the suffix.
		if strings.HasPrefix(k, "tunnel.") {
			// Validate remote name in key.
			fields := strings.Split(k, ".")
			if len(fields) != 3 {
				return fmt.Errorf("Invalid network configuration key: %s", k)
			}

			if len(n.name)+len(fields[1]) > 14 {
				return fmt.Errorf("Network name too long for tunnel interface: %s-%s", n.name, fields[1])
			}

			tunnelKey := fields[2]

			// Add the correct validation rule for the dynamic field based on last part of key.
			switch tunnelKey {
			case "protocol":
				rules[k] = validate.Optional(validate.IsOneOf("gre", "vxlan"))
			case "local":
				rules[k] = validate.Optional(validate.IsNetworkAddress)
			case "remote":
				rules[k] = validate.Optional(validate.IsNetworkAddress)
			case "port":
				rules[k] = networkValidPort
			case "group":
				rules[k] = validate.Optional(validate.IsNetworkAddress)
			case "id":
				rules[k] = validate.Optional(validate.IsInt64)
			case "inteface":
				rules[k] = validate.IsInterfaceName
			case "ttl":
				rules[k] = validate.Optional(validate.IsUint8)
			}
		}
	}

	// Add the BGP validation rules.
	bgpRules, err := n.bgpValidationRules(config)
	if err != nil {
		return err
	}

	for k, v := range bgpRules {
		rules[k] = v
	}

	// Validate the configuration.
	err = n.validate(config, rules)
	if err != nil {
		return err
	}

	// Peform composite key checks after per-key validation.

	// Validate network name when used in fan mode.
	bridgeMode := config["bridge.mode"]
	if bridgeMode == "fan" && len(n.name) > 11 {
		return fmt.Errorf("Network name too long to use with the FAN (must be 11 characters or less)")
	}

	for k, v := range config {
		key := k
		// Bridge mode checks
		if bridgeMode == "fan" && strings.HasPrefix(key, "ipv4.") && !shared.StringInSlice(key, []string{"ipv4.dhcp.expiry", "ipv4.firewall", "ipv4.nat", "ipv4.nat.order"}) && v != "" {
			return fmt.Errorf("IPv4 configuration may not be set when in 'fan' mode")
		}

		if bridgeMode == "fan" && strings.HasPrefix(key, "ipv6.") && v != "" {
			return fmt.Errorf("IPv6 configuration may not be set when in 'fan' mode")
		}

		if bridgeMode != "fan" && strings.HasPrefix(key, "fan.") && v != "" {
			return fmt.Errorf("FAN configuration may only be set when in 'fan' mode")
		}

		// MTU checks
		if key == "bridge.mtu" && v != "" {
			mtu, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return fmt.Errorf("Invalid value for an integer: %s", v)
			}

			ipv6 := config["ipv6.address"]
			if ipv6 != "" && ipv6 != "none" && mtu < 1280 {
				return fmt.Errorf("The minimum MTU for an IPv6 network is 1280")
			}

			ipv4 := config["ipv4.address"]
			if ipv4 != "" && ipv4 != "none" && mtu < 68 {
				return fmt.Errorf("The minimum MTU for an IPv4 network is 68")
			}

			if config["bridge.mode"] == "fan" {
				if config["fan.type"] == "ipip" {
					if mtu > 1480 {
						return fmt.Errorf("Maximum MTU for an IPIP FAN bridge is 1480")
					}
				} else {
					if mtu > 1450 {
						return fmt.Errorf("Maximum MTU for a VXLAN FAN bridge is 1450")
					}
				}
			}
		}
	}

	// Check using same MAC address on every cluster node is safe.
	if config["bridge.hwaddr"] != "" {
		err = n.checkClusterWideMACSafe(config)
		if err != nil {
			return err
		}
	}

	// Check IPv4 OVN ranges.
	if config["ipv4.ovn.ranges"] != "" {
		dhcpSubnet := n.DHCPv4Subnet()
		allowedNets := []*net.IPNet{}

		if dhcpSubnet != nil {
			if config["ipv4.dhcp.ranges"] == "" {
				return fmt.Errorf(`"ipv4.ovn.ranges" must be used in conjunction with non-overlapping "ipv4.dhcp.ranges" when DHCPv4 is enabled`)
			}

			allowedNets = append(allowedNets, dhcpSubnet)
		}

		ovnRanges, err := parseIPRanges(config["ipv4.ovn.ranges"], allowedNets...)
		if err != nil {
			return errors.Wrapf(err, "Failed parsing ipv4.ovn.ranges")
		}

		dhcpRanges, err := parseIPRanges(config["ipv4.dhcp.ranges"], allowedNets...)
		if err != nil {
			return errors.Wrapf(err, "Failed parsing ipv4.dhcp.ranges")
		}

		for _, ovnRange := range ovnRanges {
			for _, dhcpRange := range dhcpRanges {
				if IPRangesOverlap(ovnRange, dhcpRange) {
					return fmt.Errorf(`The range specified in "ipv4.ovn.ranges" (%q) cannot overlap with "ipv4.dhcp.ranges"`, ovnRange)
				}
			}
		}

	}

	// Check IPv6 OVN ranges.
	if config["ipv6.ovn.ranges"] != "" {
		dhcpSubnet := n.DHCPv6Subnet()
		allowedNets := []*net.IPNet{}

		if dhcpSubnet != nil {
			if config["ipv6.dhcp.ranges"] == "" && shared.IsTrue(config["ipv6.dhcp.stateful"]) {
				return fmt.Errorf(`"ipv6.ovn.ranges" must be used in conjunction with non-overlapping "ipv6.dhcp.ranges" when stateful DHCPv6 is enabled`)
			}

			allowedNets = append(allowedNets, dhcpSubnet)
		}

		ovnRanges, err := parseIPRanges(config["ipv6.ovn.ranges"], allowedNets...)
		if err != nil {
			return errors.Wrapf(err, "Failed parsing ipv6.ovn.ranges")
		}

		// If stateful DHCPv6 is enabled, check OVN ranges don't overlap with DHCPv6 stateful ranges.
		// Otherwise SLAAC will be being used to generate client IPs and predefined ranges aren't used.
		if dhcpSubnet != nil && shared.IsTrue(config["ipv6.dhcp.stateful"]) {
			dhcpRanges, err := parseIPRanges(config["ipv6.dhcp.ranges"], allowedNets...)
			if err != nil {
				return errors.Wrapf(err, "Failed parsing ipv6.dhcp.ranges")
			}

			for _, ovnRange := range ovnRanges {
				for _, dhcpRange := range dhcpRanges {
					if IPRangesOverlap(ovnRange, dhcpRange) {
						return fmt.Errorf(`The range specified in "ipv6.ovn.ranges" (%q) cannot overlap with "ipv6.dhcp.ranges"`, ovnRange)
					}
				}
			}
		}
	}

	// Check Security ACLs are supported and exist.
	if config["security.acls"] != "" {
		err = acl.Exists(n.state, n.Project(), util.SplitNTrimSpace(config["security.acls"], ",", -1, true)...)
		if err != nil {
			return err
		}
	}

	return nil
}

// Create checks whether the bridge interface name is used already.
func (n *bridge) Create(clientType request.ClientType) error {
	n.logger.Debug("Create", log.Ctx{"clientType": clientType, "config": n.config})

	if InterfaceExists(n.name) {
		return fmt.Errorf("Network interface %q already exists", n.name)
	}

	return nil
}

// isRunning returns whether the network is up.
func (n *bridge) isRunning() bool {
	return InterfaceExists(n.name)
}

// Delete deletes a network.
func (n *bridge) Delete(clientType request.ClientType) error {
	n.logger.Debug("Delete", log.Ctx{"clientType": clientType})

	// Delete all warnings regarding this network
	err := warnings.DeleteWarningsByLocalNodeAndProjectAndEntity(n.state.Cluster, n.project, dbCluster.TypeNetwork, int(n.id))
	if err != nil {
		n.logger.Warn("Failed to delete warnings", log.Ctx{"err": err})
	}

	if n.isRunning() {
		err := n.Stop()
		if err != nil {
			return err
		}
	}

	// Delete apparmor profiles.
	err = apparmor.NetworkDelete(n.state, n)
	if err != nil {
		return err
	}

	return n.common.delete(clientType)
}

// Rename renames a network.
func (n *bridge) Rename(newName string) error {
	n.logger.Debug("Rename", log.Ctx{"newName": newName})

	if InterfaceExists(newName) {
		return fmt.Errorf("Network interface %q already exists", newName)
	}

	// Bring the network down.
	if n.isRunning() {
		err := n.Stop()
		if err != nil {
			return err
		}
	}

	// Rename forkdns log file.
	forkDNSLogPath := fmt.Sprintf("forkdns.%s.log", n.name)
	if shared.PathExists(shared.LogPath(forkDNSLogPath)) {
		err := os.Rename(forkDNSLogPath, shared.LogPath(fmt.Sprintf("forkdns.%s.log", newName)))
		if err != nil {
			return err
		}
	}

	// Rename common steps.
	err := n.common.rename(newName)
	if err != nil {
		return err
	}

	// Bring the network up.
	err = n.Start()
	if err != nil {
		return err
	}

	return nil
}

// Start starts the network.
func (n *bridge) Start() error {
	n.logger.Debug("Start")

	err := n.setup(nil)
	if err != nil {
		err := n.state.Cluster.UpsertWarningLocalNode(n.project, dbCluster.TypeNetwork, int(n.id), db.WarningNetworkStartupFailure, err.Error())
		if err != nil {
			n.logger.Warn("Failed to create warning", log.Ctx{"err": err})
		}
	} else {
		err := warnings.ResolveWarningsByLocalNodeAndProjectAndTypeAndEntity(n.state.Cluster, n.project, db.WarningNetworkStartupFailure, dbCluster.TypeNetwork, int(n.id))
		if err != nil {
			n.logger.Warn("Failed to resolve warning", log.Ctx{"err": err})
		}
	}

	return err
}

// setup restarts the network.
func (n *bridge) setup(oldConfig map[string]string) error {
	// If we are in mock mode, just no-op.
	if n.state.OS.MockMode {
		return nil
	}

	n.logger.Debug("Setting up network")

	revert := revert.New()
	defer revert.Fail()

	// Create directory.
	if !shared.PathExists(shared.VarPath("networks", n.name)) {
		err := os.MkdirAll(shared.VarPath("networks", n.name), 0711)
		if err != nil {
			return err
		}
	}

	bridgeLink := &ip.Link{Name: n.name}

	// Create the bridge interface if doesn't exist.
	if !n.isRunning() {
		if n.config["bridge.driver"] == "openvswitch" {
			ovs := openvswitch.NewOVS()
			if !ovs.Installed() {
				return fmt.Errorf("Open vSwitch isn't installed on this system")
			}

			err := ovs.BridgeAdd(n.name, false)
			if err != nil {
				return err
			}
			revert.Add(func() { ovs.BridgeDelete(n.name) })
		} else {

			bridge := &ip.Bridge{
				Link: *bridgeLink,
			}
			err := bridge.Add()
			if err != nil {
				return err
			}
			revert.Add(func() { bridge.Delete() })
		}
	}

	// Get a list of tunnels.
	tunnels := n.getTunnels()

	// IPv6 bridge configuration.
	if !shared.StringInSlice(n.config["ipv6.address"], []string{"", "none"}) {
		if !shared.PathExists("/proc/sys/net/ipv6") {
			return fmt.Errorf("Network has ipv6.address but kernel IPv6 support is missing")
		}

		err := util.SysctlSet(fmt.Sprintf("net/ipv6/conf/%s/autoconf", n.name), "0")
		if err != nil {
			return err
		}

		err = util.SysctlSet(fmt.Sprintf("net/ipv6/conf/%s/accept_dad", n.name), "0")
		if err != nil {
			return err
		}
	}

	// Get a list of interfaces.
	ifaces, err := net.Interfaces()
	if err != nil {
		return err
	}

	// Cleanup any existing tunnel device.
	for _, iface := range ifaces {
		if strings.HasPrefix(iface.Name, fmt.Sprintf("%s-", n.name)) {
			tunLink := &ip.Link{Name: iface.Name}
			err = tunLink.Delete()
			if err != nil {
				return err
			}
		}
	}

	// Set the MTU.
	mtu := ""
	if n.config["bridge.mtu"] != "" {
		mtu = n.config["bridge.mtu"]
	} else if len(tunnels) > 0 {
		mtu = "1400"
	} else if n.config["bridge.mode"] == "fan" {
		if n.config["fan.type"] == "ipip" {
			mtu = "1480"
		} else {
			mtu = "1450"
		}
	}

	// Attempt to add a dummy device to the bridge to force the MTU.
	if mtu != "" && n.config["bridge.driver"] != "openvswitch" {
		dummy := &ip.Dummy{
			Link: ip.Link{Name: fmt.Sprintf("%s-mtu", n.name), MTU: mtu},
		}
		err = dummy.Add()
		if err == nil {
			revert.Add(func() { dummy.Delete() })
			err = dummy.SetUp()
			if err == nil {
				AttachInterface(n.name, fmt.Sprintf("%s-mtu", n.name))
			}
		}
	}

	// Now, set a default MTU.
	if mtu == "" {
		mtu = "1500"
	}

	err = bridgeLink.SetMTU(mtu)
	if err != nil {
		return err
	}

	// Always prefer static MAC address if set.
	hwAddr := n.config["bridge.hwaddr"]

	// If no cluster wide static MAC address set, then generate one.
	if hwAddr == "" {
		var seedNodeID int64

		if n.checkClusterWideMACSafe(n.config) != nil {
			// If not safe to use a cluster wide MAC or in in fan mode, then use cluster node's ID to
			// generate a stable per-node & network derived random MAC.
			seedNodeID = n.state.Cluster.GetNodeID()
		} else {
			// If safe to use a cluster wide MAC, then use a static cluster node of 0 to generate a
			// stable per-network derived random MAC.
			seedNodeID = 0
		}

		// Load server certificate. This is needs to be the same certificate for all nodes in a cluster.
		cert, err := util.LoadCert(n.state.OS.VarDir)
		if err != nil {
			return err
		}

		// Generate the random seed, this uses the server certificate fingerprint (to ensure that multiple
		// standalone nodes with the same network ID connected to the same external network don't generate
		// the same MAC for their networks). It relies on the certificate being the same for all nodes in a
		// cluster to allow the same MAC to be generated on each bridge interface in the network when
		// seedNodeID is 0 (when safe to do so).
		seed := fmt.Sprintf("%s.%d.%d", cert.Fingerprint(), seedNodeID, n.ID())
		r, err := util.GetStableRandomGenerator(seed)
		if err != nil {
			return errors.Wrapf(err, "Failed generating stable random bridge MAC")
		}

		hwAddr = randomHwaddr(r)
		n.logger.Debug("Stable MAC generated", log.Ctx{"seed": seed, "hwAddr": hwAddr})
	}

	// Set the MAC address on the bridge interface if specified.
	if hwAddr != "" {
		err = bridgeLink.SetAddress(hwAddr)
		if err != nil {
			return err
		}
	}

	// Enable VLAN filtering for Linux bridges.
	if n.config["bridge.driver"] != "openvswitch" {
		err = BridgeVLANFilterSetStatus(n.name, "1")
		if err != nil {
			n.logger.Warn(fmt.Sprintf("%v", err))
		}

		// Set the default PVID for new ports to 1.
		err = BridgeVLANSetDefaultPVID(n.name, "1")
		if err != nil {
			n.logger.Warn(fmt.Sprintf("%v", err))
		}
	}

	// Bring it up.
	err = bridgeLink.SetUp()
	if err != nil {
		return err
	}

	// Add any listed existing external interface.
	if n.config["bridge.external_interfaces"] != "" {
		for _, entry := range strings.Split(n.config["bridge.external_interfaces"], ",") {
			entry = strings.TrimSpace(entry)
			iface, err := net.InterfaceByName(entry)
			if err != nil {
				n.logger.Warn("Skipping attaching missing external interface", log.Ctx{"interface": entry})
				continue
			}

			unused := true
			addrs, err := iface.Addrs()
			if err == nil {
				for _, addr := range addrs {
					ip, _, err := net.ParseCIDR(addr.String())
					if ip != nil && err == nil && ip.IsGlobalUnicast() {
						unused = false
						break
					}
				}
			}

			if !unused {
				return fmt.Errorf("Only unconfigured network interfaces can be bridged")
			}

			err = AttachInterface(n.name, entry)
			if err != nil {
				return err
			}
		}
	}

	// Remove any existing firewall rules.
	fwClearIPVersions := []uint{}

	if usesIPv4Firewall(n.config) || usesIPv4Firewall(oldConfig) {
		fwClearIPVersions = append(fwClearIPVersions, 4)
	}

	if usesIPv6Firewall(n.config) || usesIPv6Firewall(oldConfig) {
		fwClearIPVersions = append(fwClearIPVersions, 6)
	}

	if len(fwClearIPVersions) > 0 {
		n.logger.Debug("Clearing firewall")
		err = n.state.Firewall.NetworkClear(n.name, false, fwClearIPVersions)
		if err != nil {
			return errors.Wrapf(err, "Failed clearing firewall")
		}
	}

	// Initialise a new firewall option set.
	fwOpts := firewallDrivers.Opts{}

	if n.hasIPv4Firewall() {
		fwOpts.FeaturesV4 = &firewallDrivers.FeatureOpts{}
	}

	if n.hasIPv6Firewall() {
		fwOpts.FeaturesV6 = &firewallDrivers.FeatureOpts{}
	}

	if n.config["security.acls"] != "" {
		fwOpts.ACL = true
	}

	// Snapshot container specific IPv4 routes (added with boot proto) before removing IPv4 addresses.
	// This is because the kernel removes any static routes on an interface when all addresses removed.
	ctRoutes, err := n.bootRoutesV4()
	if err != nil {
		return err
	}

	// Flush all IPv4 addresses and routes.
	addr := &ip.Addr{
		DevName: n.name,
		Scope:   "global",
		Family:  ip.FamilyV4,
	}
	err = addr.Flush()
	if err != nil {
		return err
	}

	r := &ip.Route{
		DevName: n.name,
		Proto:   "static",
		Family:  ip.FamilyV4,
	}
	err = r.Flush()
	if err != nil {
		return err
	}

	// Configure IPv4 firewall (includes fan).
	if n.config["bridge.mode"] == "fan" || !shared.StringInSlice(n.config["ipv4.address"], []string{"", "none"}) {
		if n.hasDHCPv4() && n.hasIPv4Firewall() {
			fwOpts.FeaturesV4.ICMPDHCPDNSAccess = true
		}

		// Allow forwarding.
		if n.config["bridge.mode"] == "fan" || n.config["ipv4.routing"] == "" || shared.IsTrue(n.config["ipv4.routing"]) {
			err = util.SysctlSet("net/ipv4/ip_forward", "1")
			if err != nil {
				return err
			}

			if n.hasIPv4Firewall() {
				fwOpts.FeaturesV4.ForwardingAllow = true
			}
		}
	}

	// Start building process using subprocess package.
	command := "dnsmasq"
	dnsmasqCmd := []string{"--keep-in-foreground", "--strict-order", "--bind-interfaces",
		"--except-interface=lo",
		"--pid-file=", // Disable attempt at writing a PID file.
		"--no-ping",   // --no-ping is very important to prevent delays to lease file updates.
		fmt.Sprintf("--interface=%s", n.name)}

	dnsmasqVersion, err := dnsmasq.GetVersion()
	if err != nil {
		return err
	}

	// --dhcp-rapid-commit option is only supported on >2.79.
	minVer, _ := version.NewDottedVersion("2.79")
	if dnsmasqVersion.Compare(minVer) > 0 {
		dnsmasqCmd = append(dnsmasqCmd, "--dhcp-rapid-commit")
	}

	if !daemon.Debug {
		// --quiet options are only supported on >2.67.
		minVer, _ := version.NewDottedVersion("2.67")

		if err == nil && dnsmasqVersion.Compare(minVer) > 0 {
			dnsmasqCmd = append(dnsmasqCmd, []string{"--quiet-dhcp", "--quiet-dhcp6", "--quiet-ra"}...)
		}
	}

	// Configure IPv4.
	if !shared.StringInSlice(n.config["ipv4.address"], []string{"", "none"}) {
		// Parse the subnet.
		ipAddress, subnet, err := net.ParseCIDR(n.config["ipv4.address"])
		if err != nil {
			return errors.Wrapf(err, "Failed parsing ipv4.address")
		}

		// Update the dnsmasq config.
		dnsmasqCmd = append(dnsmasqCmd, fmt.Sprintf("--listen-address=%s", ipAddress.String()))
		if n.DHCPv4Subnet() != nil {
			if !shared.StringInSlice("--dhcp-no-override", dnsmasqCmd) {
				dnsmasqCmd = append(dnsmasqCmd, []string{"--dhcp-no-override", "--dhcp-authoritative", fmt.Sprintf("--dhcp-leasefile=%s", shared.VarPath("networks", n.name, "dnsmasq.leases")), fmt.Sprintf("--dhcp-hostsfile=%s", shared.VarPath("networks", n.name, "dnsmasq.hosts"))}...)
			}

			if n.config["ipv4.dhcp.gateway"] != "" {
				dnsmasqCmd = append(dnsmasqCmd, fmt.Sprintf("--dhcp-option-force=3,%s", n.config["ipv4.dhcp.gateway"]))
			}

			if mtu != "1500" {
				dnsmasqCmd = append(dnsmasqCmd, fmt.Sprintf("--dhcp-option-force=26,%s", mtu))
			}

			dnsSearch := n.config["dns.search"]
			if dnsSearch != "" {
				dnsmasqCmd = append(dnsmasqCmd, fmt.Sprintf("--dhcp-option-force=119,%s", strings.Trim(dnsSearch, " ")))
			}

			expiry := "1h"
			if n.config["ipv4.dhcp.expiry"] != "" {
				expiry = n.config["ipv4.dhcp.expiry"]
			}

			if n.config["ipv4.dhcp.ranges"] != "" {
				for _, dhcpRange := range strings.Split(n.config["ipv4.dhcp.ranges"], ",") {
					dhcpRange = strings.TrimSpace(dhcpRange)
					dnsmasqCmd = append(dnsmasqCmd, []string{"--dhcp-range", fmt.Sprintf("%s,%s", strings.Replace(dhcpRange, "-", ",", -1), expiry)}...)
				}
			} else {
				dnsmasqCmd = append(dnsmasqCmd, []string{"--dhcp-range", fmt.Sprintf("%s,%s,%s", dhcpalloc.GetIP(subnet, 2).String(), dhcpalloc.GetIP(subnet, -2).String(), expiry)}...)
			}
		}

		// Add the address.
		addr := &ip.Addr{
			DevName: n.name,
			Address: n.config["ipv4.address"],
			Family:  ip.FamilyV4,
		}
		err = addr.Add()
		if err != nil {
			return err
		}

		// Configure NAT.
		if shared.IsTrue(n.config["ipv4.nat"]) {
			//If a SNAT source address is specified, use that, otherwise default to MASQUERADE mode.
			var srcIP net.IP
			if n.config["ipv4.nat.address"] != "" {
				srcIP = net.ParseIP(n.config["ipv4.nat.address"])
			}

			fwOpts.SNATV4 = &firewallDrivers.SNATOpts{
				SNATAddress: srcIP,
				Subnet:      subnet,
			}

			if n.config["ipv4.nat.order"] == "after" {
				fwOpts.SNATV4.Append = true
			}
		}

		// Add additional routes.
		if n.config["ipv4.routes"] != "" {
			for _, route := range strings.Split(n.config["ipv4.routes"], ",") {
				route = strings.TrimSpace(route)
				r := &ip.Route{
					DevName: n.name,
					Route:   route,
					Proto:   "static",
					Family:  ip.FamilyV4,
				}
				err = r.Add()
				if err != nil {
					return err
				}
			}
		}

		// Restore container specific IPv4 routes to interface.
		n.applyBootRoutesV4(ctRoutes)
	}

	// Snapshot container specific IPv6 routes (added with boot proto) before removing IPv6 addresses.
	// This is because the kernel removes any static routes on an interface when all addresses removed.
	ctRoutes, err = n.bootRoutesV6()
	if err != nil {
		return err
	}

	// Flush all IPv6 addresses and routes.
	addr = &ip.Addr{
		DevName: n.name,
		Scope:   "global",
		Family:  ip.FamilyV6,
	}
	err = addr.Flush()
	if err != nil {
		return err
	}

	r = &ip.Route{
		DevName: n.name,
		Proto:   "static",
		Family:  ip.FamilyV6,
	}
	err = r.Flush()
	if err != nil {
		return err
	}

	// Configure IPv6.
	if !shared.StringInSlice(n.config["ipv6.address"], []string{"", "none"}) {
		// Enable IPv6 for the subnet.
		err := util.SysctlSet(fmt.Sprintf("net/ipv6/conf/%s/disable_ipv6", n.name), "0")
		if err != nil {
			return err
		}

		// Parse the subnet.
		ipAddress, subnet, err := net.ParseCIDR(n.config["ipv6.address"])
		if err != nil {
			return errors.Wrapf(err, "Failed parsing ipv6.address")
		}
		subnetSize, _ := subnet.Mask.Size()

		if subnetSize > 64 {
			n.logger.Warn("IPv6 networks with a prefix larger than 64 aren't properly supported by dnsmasq")

			err = n.state.Cluster.UpsertWarningLocalNode(n.project, dbCluster.TypeNetwork, int(n.id), db.WarningLargerIPv6PrefixThanSupported, "")
			if err != nil {
				n.logger.Warn("Failed to create warning", log.Ctx{"err": err})
			}
		} else {
			err = warnings.ResolveWarningsByLocalNodeAndProjectAndTypeAndEntity(n.state.Cluster, n.project, db.WarningLargerIPv6PrefixThanSupported, dbCluster.TypeNetwork, int(n.id))
			if err != nil {
				n.logger.Warn("Failed to resolve warning", log.Ctx{"err": err})
			}
		}

		// Update the dnsmasq config.
		dnsmasqCmd = append(dnsmasqCmd, []string{fmt.Sprintf("--listen-address=%s", ipAddress.String()), "--enable-ra"}...)
		if n.DHCPv6Subnet() != nil {
			if n.hasIPv6Firewall() {
				fwOpts.FeaturesV6.ICMPDHCPDNSAccess = true
			}

			// Build DHCP configuration.
			if !shared.StringInSlice("--dhcp-no-override", dnsmasqCmd) {
				dnsmasqCmd = append(dnsmasqCmd, []string{"--dhcp-no-override", "--dhcp-authoritative", fmt.Sprintf("--dhcp-leasefile=%s", shared.VarPath("networks", n.name, "dnsmasq.leases")), fmt.Sprintf("--dhcp-hostsfile=%s", shared.VarPath("networks", n.name, "dnsmasq.hosts"))}...)
			}

			expiry := "1h"
			if n.config["ipv6.dhcp.expiry"] != "" {
				expiry = n.config["ipv6.dhcp.expiry"]
			}

			if shared.IsTrue(n.config["ipv6.dhcp.stateful"]) {
				if n.config["ipv6.dhcp.ranges"] != "" {
					for _, dhcpRange := range strings.Split(n.config["ipv6.dhcp.ranges"], ",") {
						dhcpRange = strings.TrimSpace(dhcpRange)
						dnsmasqCmd = append(dnsmasqCmd, []string{"--dhcp-range", fmt.Sprintf("%s,%d,%s", strings.Replace(dhcpRange, "-", ",", -1), subnetSize, expiry)}...)
					}
				} else {
					dnsmasqCmd = append(dnsmasqCmd, []string{"--dhcp-range", fmt.Sprintf("%s,%s,%d,%s", dhcpalloc.GetIP(subnet, 2), dhcpalloc.GetIP(subnet, -1), subnetSize, expiry)}...)
				}
			} else {
				dnsmasqCmd = append(dnsmasqCmd, []string{"--dhcp-range", fmt.Sprintf("::,constructor:%s,ra-stateless,ra-names", n.name)}...)
			}
		} else {
			dnsmasqCmd = append(dnsmasqCmd, []string{"--dhcp-range", fmt.Sprintf("::,constructor:%s,ra-only", n.name)}...)
		}

		// Allow forwarding.
		if n.config["ipv6.routing"] == "" || shared.IsTrue(n.config["ipv6.routing"]) {
			// Get a list of proc entries.
			entries, err := ioutil.ReadDir("/proc/sys/net/ipv6/conf/")
			if err != nil {
				return err
			}

			// First set accept_ra to 2 for everything.
			for _, entry := range entries {
				content, err := ioutil.ReadFile(fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/accept_ra", entry.Name()))
				if err == nil && string(content) != "1\n" {
					continue
				}

				err = util.SysctlSet(fmt.Sprintf("net/ipv6/conf/%s/accept_ra", entry.Name()), "2")
				if err != nil && !os.IsNotExist(err) {
					return err
				}
			}

			// Then set forwarding for all of them.
			for _, entry := range entries {
				err = util.SysctlSet(fmt.Sprintf("net/ipv6/conf/%s/forwarding", entry.Name()), "1")
				if err != nil && !os.IsNotExist(err) {
					return err
				}
			}

			if n.hasIPv6Firewall() {
				fwOpts.FeaturesV6.ForwardingAllow = true
			}
		}

		// Add the address.
		addr := &ip.Addr{
			DevName: n.name,
			Address: n.config["ipv6.address"],
			Family:  ip.FamilyV6,
		}
		err = addr.Add()
		if err != nil {
			return err
		}

		// Configure NAT.
		if shared.IsTrue(n.config["ipv6.nat"]) {
			//If a SNAT source address is specified, use that, otherwise default to MASQUERADE mode.
			var srcIP net.IP
			if n.config["ipv6.nat.address"] != "" {
				srcIP = net.ParseIP(n.config["ipv6.nat.address"])
			}

			fwOpts.SNATV6 = &firewallDrivers.SNATOpts{
				SNATAddress: srcIP,
				Subnet:      subnet,
			}

			if n.config["ipv6.nat.order"] == "after" {
				fwOpts.SNATV6.Append = true
			}
		}

		// Add additional routes.
		if n.config["ipv6.routes"] != "" {
			for _, route := range strings.Split(n.config["ipv6.routes"], ",") {
				route = strings.TrimSpace(route)
				r := &ip.Route{
					DevName: n.name,
					Route:   route,
					Proto:   "static",
					Family:  ip.FamilyV6,
				}
				err = r.Add()
				if err != nil {
					return err
				}
			}
		}

		// Restore container specific IPv6 routes to interface.
		n.applyBootRoutesV6(ctRoutes)
	}

	// Configure the fan.
	dnsClustered := false
	dnsClusteredAddress := ""
	var overlaySubnet *net.IPNet
	if n.config["bridge.mode"] == "fan" {
		tunName := fmt.Sprintf("%s-fan", n.name)

		// Parse the underlay.
		underlay := n.config["fan.underlay_subnet"]
		_, underlaySubnet, err := net.ParseCIDR(underlay)
		if err != nil {
			return errors.Wrapf(err, "Failed parsing fan.underlay_subnet")
		}

		// Parse the overlay.
		overlay := n.config["fan.overlay_subnet"]
		if overlay == "" {
			overlay = "240.0.0.0/8"
		}

		_, overlaySubnet, err = net.ParseCIDR(overlay)
		if err != nil {
			return errors.Wrapf(err, "Failed parsing fan.overlay_subnet")
		}

		// Get the address.
		fanAddress, devName, devAddr, err := n.fanAddress(underlaySubnet, overlaySubnet)
		if err != nil {
			return err
		}

		addr := strings.Split(fanAddress, "/")
		if n.config["fan.type"] == "ipip" {
			fanAddress = fmt.Sprintf("%s/24", addr[0])
		}

		// Update the MTU based on overlay device (if available).
		fanMtuInt, err := GetDevMTU(devName)
		if err == nil {
			// Apply overhead.
			if n.config["fan.type"] == "ipip" {
				fanMtuInt = fanMtuInt - 20
			} else {
				fanMtuInt = fanMtuInt - 50
			}

			// Apply changes.
			fanMtu := fmt.Sprintf("%d", fanMtuInt)
			if fanMtu != mtu {
				mtu = fanMtu
				if n.config["bridge.driver"] != "openvswitch" {
					mtuLink := &ip.Link{Name: fmt.Sprintf("%s-mtu", n.name)}
					err = mtuLink.SetMTU(mtu)
					if err != nil {
						return err
					}
				}

				err = bridgeLink.SetMTU(mtu)
				if err != nil {
					return err
				}
			}
		}

		// Parse the host subnet.
		_, hostSubnet, err := net.ParseCIDR(fmt.Sprintf("%s/24", addr[0]))
		if err != nil {
			return errors.Wrapf(err, "Failed parsing fan address")
		}

		// Add the address.
		ipAddr := &ip.Addr{
			DevName: n.name,
			Address: fanAddress,
			Family:  ip.FamilyV4,
		}
		err = ipAddr.Add()
		if err != nil {
			return err
		}

		// Update the dnsmasq config.
		expiry := "1h"
		if n.config["ipv4.dhcp.expiry"] != "" {
			expiry = n.config["ipv4.dhcp.expiry"]
		}

		dnsmasqCmd = append(dnsmasqCmd, []string{
			fmt.Sprintf("--listen-address=%s", addr[0]),
			"--dhcp-no-override", "--dhcp-authoritative",
			fmt.Sprintf("--dhcp-leasefile=%s", shared.VarPath("networks", n.name, "dnsmasq.leases")),
			fmt.Sprintf("--dhcp-hostsfile=%s", shared.VarPath("networks", n.name, "dnsmasq.hosts")),
			"--dhcp-range", fmt.Sprintf("%s,%s,%s", dhcpalloc.GetIP(hostSubnet, 2).String(), dhcpalloc.GetIP(hostSubnet, -2).String(), expiry)}...)

		// Setup the tunnel.
		if n.config["fan.type"] == "ipip" {
			r := &ip.Route{
				DevName: "tunl0",
				Family:  ip.FamilyV4,
			}
			err = r.Flush()
			if err != nil {
				return err
			}

			tunLink := &ip.Link{Name: "tunl0"}
			err = tunLink.SetUp()
			if err != nil {
				return err
			}

			// Fails if the map is already set.
			tunLink.Change("ipip", fmt.Sprintf("%s:%s", overlay, underlay))

			r = &ip.Route{
				DevName: "tunl0",
				Route:   overlay,
				Src:     addr[0],
				Proto:   "static",
			}
			err = r.Add()
			if err != nil {
				return err
			}
		} else {
			vxlanID := fmt.Sprintf("%d", binary.BigEndian.Uint32(overlaySubnet.IP.To4())>>8)
			vxlan := &ip.Vxlan{
				Link:    ip.Link{Name: tunName},
				VxlanID: vxlanID,
				DevName: devName,
				DstPort: "0",
				Local:   devAddr,
				FanMap:  fmt.Sprintf("%s:%s", overlay, underlay),
			}
			err = vxlan.Add()
			if err != nil {
				return err
			}

			err = AttachInterface(n.name, tunName)
			if err != nil {
				return err
			}

			err = vxlan.SetMTU(mtu)
			if err != nil {
				return err
			}

			err = vxlan.SetUp()
			if err != nil {
				return err
			}

			err = bridgeLink.SetUp()
			if err != nil {
				return err
			}
		}

		// Configure NAT.
		if shared.IsTrue(n.config["ipv4.nat"]) {
			fwOpts.SNATV4 = &firewallDrivers.SNATOpts{
				SNATAddress: nil, // Use MASQUERADE mode.
				Subnet:      overlaySubnet,
			}

			if n.config["ipv4.nat.order"] == "after" {
				fwOpts.SNATV4.Append = true

			}
		}

		// Setup clustered DNS.
		clusterAddress, err := node.ClusterAddress(n.state.Node)
		if err != nil {
			return err
		}

		// If clusterAddress is non-empty, this indicates the intention for this node to be
		// part of a cluster and so we should ensure that dnsmasq and forkdns are started
		// in cluster mode. Note: During LXD initialisation the cluster may not actually be
		// setup yet, but we want the DNS processes to be ready for when it is.
		if clusterAddress != "" {
			dnsClustered = true
		}

		dnsClusteredAddress = strings.Split(fanAddress, "/")[0]
	}

	// Configure tunnels.
	for _, tunnel := range tunnels {
		getConfig := func(key string) string {
			return n.config[fmt.Sprintf("tunnel.%s.%s", tunnel, key)]
		}

		tunProtocol := getConfig("protocol")
		tunLocal := getConfig("local")
		tunRemote := getConfig("remote")
		tunName := fmt.Sprintf("%s-%s", n.name, tunnel)

		// Configure the tunnel.
		if tunProtocol == "gre" {
			// Skip partial configs.
			if tunProtocol == "" || tunLocal == "" || tunRemote == "" {
				continue
			}

			gretap := &ip.Gretap{
				Link:   ip.Link{Name: tunName},
				Local:  tunLocal,
				Remote: tunRemote,
			}
			err := gretap.Add()
			if err != nil {
				return err
			}
		} else if tunProtocol == "vxlan" {
			tunGroup := getConfig("group")
			tunInterface := getConfig("interface")

			// Skip partial configs.
			if tunProtocol == "" {
				continue
			}

			vxlan := &ip.Vxlan{
				Link: ip.Link{Name: tunName},
			}
			if tunLocal != "" && tunRemote != "" {
				vxlan.Local = tunLocal
				vxlan.Remote = tunRemote
			} else {
				if tunGroup == "" {
					tunGroup = "239.0.0.1"
				}

				devName := tunInterface
				if devName == "" {
					_, devName, err = DefaultGatewaySubnetV4()
					if err != nil {
						return err
					}
				}

				vxlan.Group = tunGroup
				vxlan.DevName = devName
			}

			tunPort := getConfig("port")
			if tunPort == "" {
				tunPort = "0"
			}
			vxlan.DstPort = tunPort

			tunID := getConfig("id")
			if tunID == "" {
				tunID = "1"
			}
			vxlan.VxlanID = tunID

			tunTTL := getConfig("ttl")
			if tunTTL == "" {
				tunTTL = "1"
			}
			vxlan.TTL = tunTTL

			err := vxlan.Add()
			if err != nil {
				return err
			}
		}

		// Bridge it and bring up.
		err = AttachInterface(n.name, tunName)
		if err != nil {
			return err
		}

		tunLink := &ip.Link{Name: tunName}
		err = tunLink.SetMTU(mtu)
		if err != nil {
			return err
		}

		// Bring up tunnel interface.
		err = tunLink.SetUp()
		if err != nil {
			return err
		}

		// Bring up network interface.
		err = bridgeLink.SetUp()
		if err != nil {
			return err
		}
	}

	// Generate and load apparmor profiles.
	err = apparmor.NetworkLoad(n.state, n)
	if err != nil {
		return err
	}

	// Kill any existing dnsmasq and forkdns daemon for this network.
	err = dnsmasq.Kill(n.name, false)
	if err != nil {
		return err
	}

	err = n.killForkDNS()
	if err != nil {
		return err
	}

	// Configure dnsmasq.
	if n.UsesDNSMasq() {
		// Setup the dnsmasq domain.
		dnsDomain := n.config["dns.domain"]
		if dnsDomain == "" {
			dnsDomain = "lxd"
		}

		if n.config["dns.mode"] != "none" {
			dnsmasqCmd = append(dnsmasqCmd, "-s", dnsDomain)
			dnsmasqCmd = append(dnsmasqCmd, "--interface-name", fmt.Sprintf("_gateway.%s,%s", dnsDomain, n.name))

			if dnsClustered {
				dnsmasqCmd = append(dnsmasqCmd, "-S", fmt.Sprintf("/%s/%s#1053", dnsDomain, dnsClusteredAddress))
				dnsmasqCmd = append(dnsmasqCmd, fmt.Sprintf("--rev-server=%s,%s#1053", overlaySubnet, dnsClusteredAddress))
			} else {
				dnsmasqCmd = append(dnsmasqCmd, "-S", fmt.Sprintf("/%s/", dnsDomain))
			}
		}

		// Create a config file to contain additional config (and to prevent dnsmasq from reading /etc/dnsmasq.conf)
		err = ioutil.WriteFile(shared.VarPath("networks", n.name, "dnsmasq.raw"), []byte(fmt.Sprintf("%s\n", n.config["raw.dnsmasq"])), 0644)
		if err != nil {
			return err
		}
		dnsmasqCmd = append(dnsmasqCmd, fmt.Sprintf("--conf-file=%s", shared.VarPath("networks", n.name, "dnsmasq.raw")))

		// Attempt to drop privileges.
		if n.state.OS.UnprivUser != "" {
			dnsmasqCmd = append(dnsmasqCmd, []string{"-u", n.state.OS.UnprivUser}...)
		}
		if n.state.OS.UnprivGroup != "" {
			dnsmasqCmd = append(dnsmasqCmd, []string{"-g", n.state.OS.UnprivGroup}...)
		}

		// Create DHCP hosts directory.
		if !shared.PathExists(shared.VarPath("networks", n.name, "dnsmasq.hosts")) {
			err = os.MkdirAll(shared.VarPath("networks", n.name, "dnsmasq.hosts"), 0755)
			if err != nil {
				return err
			}
		}

		// Check for dnsmasq.
		_, err := exec.LookPath("dnsmasq")
		if err != nil {
			return fmt.Errorf("dnsmasq is required for LXD managed bridges")
		}

		// Update the static leases.
		err = UpdateDNSMasqStatic(n.state, n.name)
		if err != nil {
			return err
		}

		// Create subprocess object dnsmasq.
		dnsmasqLogPath := shared.LogPath(fmt.Sprintf("dnsmasq.%s.log", n.name))
		p, err := subprocess.NewProcess(command, dnsmasqCmd, "", dnsmasqLogPath)
		if err != nil {
			return fmt.Errorf("Failed to create subprocess: %s", err)
		}

		// Apply AppArmor confinement.
		if n.config["raw.dnsmasq"] == "" {
			p.SetApparmor(apparmor.DnsmasqProfileName(n))

			err = warnings.ResolveWarningsByLocalNodeAndProjectAndTypeAndEntity(n.state.Cluster, n.project, db.WarningAppArmorDisabledDueToRawDnsmasq, dbCluster.TypeNetwork, int(n.id))
			if err != nil {
				n.logger.Warn("Failed to resolve warning", log.Ctx{"err": err})
			}
		} else {
			n.logger.Warn("Skipping AppArmor for dnsmasq due to raw.dnsmasq being set", log.Ctx{"name": n.name})

			err = n.state.Cluster.UpsertWarningLocalNode(n.project, dbCluster.TypeNetwork, int(n.id), db.WarningAppArmorDisabledDueToRawDnsmasq, "")
			if err != nil {
				n.logger.Warn("Failed to create warning", log.Ctx{"err": err})
			}
		}

		// Start dnsmasq.
		err = p.Start()
		if err != nil {
			return fmt.Errorf("Failed to run: %s %s: %v", command, strings.Join(dnsmasqCmd, " "), err)
		}

		// Check dnsmasq started OK.
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(time.Millisecond*time.Duration(500)))
		_, err = p.Wait(ctx)
		if errors.Cause(err) != context.DeadlineExceeded {
			stderr, _ := ioutil.ReadFile(dnsmasqLogPath)

			// Just log an error if dnsmasq has exited, and still proceed with normal setup so we
			// don't leave the firewall in an inconsistent state.
			n.logger.Error("The dnsmasq process exited prematurely", log.Ctx{"err": err, "stderr": strings.TrimSpace(string(stderr))})
		}
		cancel()

		err = p.Save(shared.VarPath("networks", n.name, "dnsmasq.pid"))
		if err != nil {
			// Kill Process if started, but could not save the file.
			err2 := p.Stop()
			if err != nil {
				return fmt.Errorf("Could not kill subprocess while handling saving error: %s: %s", err, err2)
			}

			return fmt.Errorf("Failed to save subprocess details: %s", err)
		}

		// Spawn DNS forwarder if needed (backgrounded to avoid deadlocks during cluster boot).
		if dnsClustered {
			// Create forkdns servers directory.
			if !shared.PathExists(shared.VarPath("networks", n.name, ForkdnsServersListPath)) {
				err = os.MkdirAll(shared.VarPath("networks", n.name, ForkdnsServersListPath), 0755)
				if err != nil {
					return err
				}
			}

			// Create forkdns servers.conf file if doesn't exist.
			f, err := os.OpenFile(shared.VarPath("networks", n.name, ForkdnsServersListPath+"/"+ForkdnsServersListFile), os.O_RDONLY|os.O_CREATE, 0666)
			if err != nil {
				return err
			}
			f.Close()

			err = n.spawnForkDNS(dnsClusteredAddress)
			if err != nil {
				return err
			}
		}
	} else {
		// Clean up old dnsmasq config if exists and we are not starting dnsmasq.
		leasesPath := shared.VarPath("networks", n.name, "dnsmasq.leases")
		if shared.PathExists(leasesPath) {
			err := os.Remove(leasesPath)
			if err != nil {
				return errors.Wrapf(err, "Failed to remove old dnsmasq leases file %q", leasesPath)
			}
		}

		// Clean up old dnsmasq PID file.
		pidPath := shared.VarPath("networks", n.name, "dnsmasq.pid")
		if shared.PathExists(pidPath) {
			err := os.Remove(pidPath)
			if err != nil {
				return errors.Wrapf(err, "Failed to remove old dnsmasq pid file %q", pidPath)
			}
		}
	}

	// Setup firewall.
	n.logger.Debug("Setting up firewall")
	err = n.state.Firewall.NetworkSetup(n.name, fwOpts)
	if err != nil {
		return errors.Wrapf(err, "Failed to setup firewall")
	}

	if fwOpts.ACL {
		aclNet := acl.NetworkACLUsage{
			Name:   n.Name(),
			Type:   n.Type(),
			ID:     n.ID(),
			Config: n.Config(),
		}

		n.logger.Debug("Applying up firewall ACLs")
		err = acl.FirewallApplyACLRules(n.state, n.logger, n.Project(), aclNet)
		if err != nil {
			return err
		}
	}

	// Setup network address forwards.
	err = n.forwardSetupFirewall()
	if err != nil {
		return err
	}

	// Setup BGP.
	err = n.bgpSetup(oldConfig)
	if err != nil {
		return err
	}

	revert.Success()
	return nil
}

// Stop stops the network.
func (n *bridge) Stop() error {
	n.logger.Debug("Stop")

	if !n.isRunning() {
		return nil
	}

	// Clear BGP.
	err := n.bgpClear(n.config)
	if err != nil {
		return err
	}

	// Destroy the bridge interface
	if n.config["bridge.driver"] == "openvswitch" {
		ovs := openvswitch.NewOVS()
		err := ovs.BridgeDelete(n.name)
		if err != nil {
			return err
		}
	} else {
		bridgeLink := &ip.Link{Name: n.name}
		err := bridgeLink.Delete()
		if err != nil {
			return err
		}
	}

	// Fully clear firewall setup.
	fwClearIPVersions := []uint{}

	if usesIPv4Firewall(n.config) {
		fwClearIPVersions = append(fwClearIPVersions, 4)
	}

	if usesIPv6Firewall(n.config) {
		fwClearIPVersions = append(fwClearIPVersions, 6)
	}

	if len(fwClearIPVersions) > 0 {
		n.logger.Debug("Deleting firewall")
		err := n.state.Firewall.NetworkClear(n.name, true, fwClearIPVersions)
		if err != nil {
			return errors.Wrapf(err, "Failed deleting firewall")
		}
	}

	// Kill any existing dnsmasq and forkdns daemon for this network
	err = dnsmasq.Kill(n.name, false)
	if err != nil {
		return err
	}

	err = n.killForkDNS()
	if err != nil {
		return err
	}

	// Get a list of interfaces
	ifaces, err := net.Interfaces()
	if err != nil {
		return err
	}

	// Cleanup any existing tunnel device
	for _, iface := range ifaces {
		if strings.HasPrefix(iface.Name, fmt.Sprintf("%s-", n.name)) {
			tunLink := &ip.Link{Name: iface.Name}
			err = tunLink.Delete()
			if err != nil {
				return err
			}
		}
	}

	// Unload apparmor profiles.
	err = apparmor.NetworkUnload(n.state, n)
	if err != nil {
		return err
	}

	return nil
}

// Update updates the network. Accepts notification boolean indicating if this update request is coming from a
// cluster notification, in which case do not update the database, just apply local changes needed.
func (n *bridge) Update(newNetwork api.NetworkPut, targetNode string, clientType request.ClientType) error {
	n.logger.Debug("Update", log.Ctx{"clientType": clientType, "newNetwork": newNetwork})

	err := n.populateAutoConfig(newNetwork.Config)
	if err != nil {
		return errors.Wrapf(err, "Failed generating auto config")
	}

	dbUpdateNeeeded, changedKeys, oldNetwork, err := n.common.configChanged(newNetwork)
	if err != nil {
		return err
	}

	if !dbUpdateNeeeded {
		return nil // Nothing changed.
	}

	// If the network as a whole has not had any previous creation attempts, or the node itself is still
	// pending, then don't apply the new settings to the node, just to the database record (ready for the
	// actual global create request to be initiated).
	if n.Status() == api.NetworkStatusPending || n.LocalStatus() == api.NetworkStatusPending {
		return n.common.update(newNetwork, targetNode, clientType)
	}

	revert := revert.New()
	defer revert.Fail()

	// Perform any pre-update cleanup needed if local node network was already created.
	if len(changedKeys) > 0 {
		// Define a function which reverts everything.
		revert.Add(func() {
			// Reset changes to all nodes and database.
			n.common.update(oldNetwork, targetNode, clientType)

			// Reset any change that was made to local bridge.
			n.setup(newNetwork.Config)
		})

		// Bring the bridge down entirely if the driver has changed.
		if shared.StringInSlice("bridge.driver", changedKeys) && n.isRunning() {
			err = n.Stop()
			if err != nil {
				return err
			}
		}

		// Detach any external interfaces should no longer be attached.
		if shared.StringInSlice("bridge.external_interfaces", changedKeys) && n.isRunning() {
			devices := []string{}
			for _, dev := range strings.Split(newNetwork.Config["bridge.external_interfaces"], ",") {
				dev = strings.TrimSpace(dev)
				devices = append(devices, dev)
			}

			for _, dev := range strings.Split(oldNetwork.Config["bridge.external_interfaces"], ",") {
				dev = strings.TrimSpace(dev)
				if dev == "" {
					continue
				}

				if !shared.StringInSlice(dev, devices) && InterfaceExists(dev) {
					err = DetachInterface(n.name, dev)
					if err != nil {
						return err
					}
				}
			}
		}
	}

	// Apply changes to all nodes and database.
	err = n.common.update(newNetwork, targetNode, clientType)
	if err != nil {
		return err
	}

	// Restart the network if needed.
	if len(changedKeys) > 0 {
		err = n.setup(oldNetwork.Config)
		if err != nil {
			return err
		}
	}

	revert.Success()
	return nil
}

func (n *bridge) spawnForkDNS(listenAddress string) error {
	// Setup the dnsmasq domain
	dnsDomain := n.config["dns.domain"]
	if dnsDomain == "" {
		dnsDomain = "lxd"
	}

	// Spawn the daemon using subprocess
	command := n.state.OS.ExecPath
	forkdnsargs := []string{"forkdns",
		fmt.Sprintf("%s:1053", listenAddress),
		dnsDomain,
		n.name}

	logPath := shared.LogPath(fmt.Sprintf("forkdns.%s.log", n.name))

	p, err := subprocess.NewProcess(command, forkdnsargs, logPath, logPath)
	if err != nil {
		return fmt.Errorf("Failed to create subprocess: %s", err)
	}

	// Drop privileges.
	p.SetCreds(n.state.OS.UnprivUID, n.state.OS.UnprivGID)

	// Apply AppArmor profile.
	p.SetApparmor(apparmor.ForkdnsProfileName(n))

	err = p.Start()
	if err != nil {
		return fmt.Errorf("Failed to run: %s %s: %v", command, strings.Join(forkdnsargs, " "), err)
	}

	err = p.Save(shared.VarPath("networks", n.name, "forkdns.pid"))
	if err != nil {
		// Kill Process if started, but could not save the file
		err2 := p.Stop()
		if err != nil {
			return fmt.Errorf("Could not kill subprocess while handling saving error: %s: %s", err, err2)
		}

		return fmt.Errorf("Failed to save subprocess details: %s", err)
	}

	return nil
}

// HandleHeartbeat refreshes forkdns servers. Retrieves the IPv4 address of each cluster node (excluding ourselves)
// for this network. It then updates the forkdns server list file if there are changes.
func (n *bridge) HandleHeartbeat(heartbeatData *cluster.APIHeartbeat) error {
	addresses := []string{}
	localAddress, err := node.HTTPSAddress(n.state.Node)
	if err != nil {
		return err
	}

	n.logger.Info("Refreshing forkdns peers")

	networkCert := n.state.Endpoints.NetworkCert()
	for _, node := range heartbeatData.Members {
		if node.Address == localAddress {
			// No need to query ourselves.
			continue
		}

		if !node.Online {
			n.logger.Warn("Excluding offline member from DNS peers refresh", log.Ctx{"address": node.Address, "ID": node.ID, "raftID": node.RaftID, "lastHeartbeat": node.LastHeartbeat})
			continue
		}

		client, err := cluster.Connect(node.Address, networkCert, n.state.ServerCert(), nil, true)
		if err != nil {
			return err
		}

		state, err := client.GetNetworkState(n.name)
		if err != nil {
			return err
		}

		for _, addr := range state.Addresses {
			// Only get IPv4 addresses of nodes on network.
			if addr.Family != "inet" || addr.Scope != "global" {
				continue
			}

			addresses = append(addresses, addr.Address)
			break
		}
	}

	// Compare current stored list to retrieved list and see if we need to update.
	curList, err := ForkdnsServersList(n.name)
	if err != nil {
		// Only warn here, but continue on to regenerate the servers list from cluster info.
		n.logger.Warn("Failed to load existing forkdns server list", log.Ctx{"err": err})
	}

	// If current list is same as cluster list, nothing to do.
	if err == nil && reflect.DeepEqual(curList, addresses) {
		return nil
	}

	err = n.updateForkdnsServersFile(addresses)
	if err != nil {
		return err
	}

	n.logger.Info("Updated forkdns server list", log.Ctx{"nodes": addresses})
	return nil
}

func (n *bridge) getTunnels() []string {
	tunnels := []string{}

	for k := range n.config {
		if !strings.HasPrefix(k, "tunnel.") {
			continue
		}

		fields := strings.Split(k, ".")
		if !shared.StringInSlice(fields[1], tunnels) {
			tunnels = append(tunnels, fields[1])
		}
	}

	return tunnels
}

// bootRoutesV4 returns a list of IPv4 boot routes on the network's device.
func (n *bridge) bootRoutesV4() ([]string, error) {
	r := &ip.Route{
		DevName: n.name,
		Proto:   "boot",
		Family:  ip.FamilyV4,
	}
	routes, err := r.Show()
	if err != nil {
		return nil, err
	}
	return routes, nil
}

// bootRoutesV6 returns a list of IPv6 boot routes on the network's device.
func (n *bridge) bootRoutesV6() ([]string, error) {
	r := &ip.Route{
		DevName: n.name,
		Proto:   "boot",
		Family:  ip.FamilyV6,
	}
	routes, err := r.Show()
	if err != nil {
		return nil, err
	}
	return routes, nil
}

// applyBootRoutesV4 applies a list of IPv4 boot routes to the network's device.
func (n *bridge) applyBootRoutesV4(routes []string) {
	for _, route := range routes {
		r := &ip.Route{
			DevName: n.name,
			Proto:   "boot",
			Family:  ip.FamilyV4,
		}
		err := r.Replace(strings.Fields(route))
		if err != nil {
			// If it fails, then we can't stop as the route has already gone, so just log and continue.
			n.logger.Error("Failed to restore route", log.Ctx{"err": err})
		}
	}
}

// applyBootRoutesV6 applies a list of IPv6 boot routes to the network's device.
func (n *bridge) applyBootRoutesV6(routes []string) {
	for _, route := range routes {
		r := &ip.Route{
			DevName: n.name,
			Proto:   "boot",
			Family:  ip.FamilyV6,
		}
		err := r.Replace(strings.Fields(route))
		if err != nil {
			// If it fails, then we can't stop as the route has already gone, so just log and continue.
			n.logger.Error("Failed to restore route", log.Ctx{"err": err})
		}
	}
}

func (n *bridge) fanAddress(underlay *net.IPNet, overlay *net.IPNet) (string, string, string, error) {
	// Quick checks.
	underlaySize, _ := underlay.Mask.Size()
	if underlaySize != 16 && underlaySize != 24 {
		return "", "", "", fmt.Errorf("Only /16 or /24 underlays are supported at this time")
	}

	overlaySize, _ := overlay.Mask.Size()
	if overlaySize != 8 && overlaySize != 16 {
		return "", "", "", fmt.Errorf("Only /8 or /16 overlays are supported at this time")
	}

	if overlaySize+(32-underlaySize)+8 > 32 {
		return "", "", "", fmt.Errorf("Underlay or overlay networks too large to accommodate the FAN")
	}

	// Get the IP
	ip, dev, err := n.addressForSubnet(underlay)
	if err != nil {
		return "", "", "", err
	}
	ipStr := ip.String()

	// Force into IPv4 format
	ipBytes := ip.To4()
	if ipBytes == nil {
		return "", "", "", fmt.Errorf("Invalid IPv4: %s", ip)
	}

	// Compute the IP
	ipBytes[0] = overlay.IP[0]
	if overlaySize == 16 {
		ipBytes[1] = overlay.IP[1]
		ipBytes[2] = ipBytes[3]
	} else if underlaySize == 24 {
		ipBytes[1] = ipBytes[3]
		ipBytes[2] = 0
	} else if underlaySize == 16 {
		ipBytes[1] = ipBytes[2]
		ipBytes[2] = ipBytes[3]
	}

	ipBytes[3] = 1

	return fmt.Sprintf("%s/%d", ipBytes.String(), overlaySize), dev, ipStr, err
}

func (n *bridge) addressForSubnet(subnet *net.IPNet) (net.IP, string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return net.IP{}, "", err
	}

	for _, iface := range ifaces {
		// Skip addresses on lo interface in case VIPs are being used on that interface that are part of
		// the underlay subnet as is unlikely to be the actual intended underlay subnet interface.
		if iface.Name == "lo" {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		for _, addr := range addrs {
			ip, _, err := net.ParseCIDR(addr.String())
			if err != nil {
				continue
			}

			if subnet.Contains(ip) {
				return ip, iface.Name, nil
			}
		}
	}

	return net.IP{}, "", fmt.Errorf("No address found in subnet")
}

func (n *bridge) killForkDNS() error {
	// Check if we have a running forkdns at all
	pidPath := shared.VarPath("networks", n.name, "forkdns.pid")

	// If the pid file doesn't exist, there is no process to kill.
	if !shared.PathExists(pidPath) {
		return nil
	}

	p, err := subprocess.ImportProcess(pidPath)
	if err != nil {
		return fmt.Errorf("Could not read pid file: %s", err)
	}

	err = p.Stop()
	if err != nil && err != subprocess.ErrNotRunning {
		return fmt.Errorf("Unable to kill dnsmasq: %s", err)
	}

	return nil
}

// updateForkdnsServersFile takes a list of node addresses and writes them atomically to
// the forkdns.servers file ready for forkdns to notice and re-apply its config.
func (n *bridge) updateForkdnsServersFile(addresses []string) error {
	// We don't want to race with ourselves here
	forkdnsServersLock.Lock()
	defer forkdnsServersLock.Unlock()

	permName := shared.VarPath("networks", n.name, ForkdnsServersListPath+"/"+ForkdnsServersListFile)
	tmpName := permName + ".tmp"

	// Open tmp file and truncate
	tmpFile, err := os.Create(tmpName)
	if err != nil {
		return err
	}
	defer tmpFile.Close()

	for _, address := range addresses {
		_, err := tmpFile.WriteString(address + "\n")
		if err != nil {
			return err
		}
	}

	tmpFile.Close()

	// Atomically rename finished file into permanent location so forkdns can pick it up.
	err = os.Rename(tmpName, permName)
	if err != nil {
		return err
	}

	return nil
}

// hasIPv4Firewall indicates whether the network has IPv4 firewall enabled.
func (n *bridge) hasIPv4Firewall() bool {
	// IPv4 firewall is only enabled if there is a bridge ipv4.address or fan mode, and ipv4.firewall enabled.
	// When using fan bridge.mode, there can be an empty ipv4.address, so we assume it is active.
	if (n.config["bridge.mode"] == "fan" || !shared.StringInSlice(n.config["ipv4.address"], []string{"", "none"})) && (n.config["ipv4.firewall"] == "" || shared.IsTrue(n.config["ipv4.firewall"])) {
		return true
	}

	return false
}

// hasIPv6Firewall indicates whether the network has IPv6 firewall enabled.
func (n *bridge) hasIPv6Firewall() bool {
	// IPv6 firewall is only enabled if there is a bridge ipv6.address and ipv6.firewall enabled.
	if !shared.StringInSlice(n.config["ipv6.address"], []string{"", "none"}) && (n.config["ipv6.firewall"] == "" || shared.IsTrue(n.config["ipv6.firewall"])) {
		return true
	}

	return false
}

// hasDHCPv4 indicates whether the network has DHCPv4 enabled.
// An empty ipv4.dhcp setting indicates enabled by default.
func (n *bridge) hasDHCPv4() bool {
	if n.config["ipv4.dhcp"] == "" || shared.IsTrue(n.config["ipv4.dhcp"]) {
		return true
	}

	return false
}

// hasDHCPv6 indicates whether the network has DHCPv6 enabled.
// An empty ipv6.dhcp setting indicates enabled by default.
func (n *bridge) hasDHCPv6() bool {
	if n.config["ipv6.dhcp"] == "" || shared.IsTrue(n.config["ipv6.dhcp"]) {
		return true
	}

	return false
}

// DHCPv4Subnet returns the DHCPv4 subnet (if DHCP is enabled on network).
func (n *bridge) DHCPv4Subnet() *net.IPNet {
	// DHCP is disabled on this network.
	if !n.hasDHCPv4() {
		return nil
	}

	// Fan mode. Extract DHCP subnet from fan bridge address. Only detectable once network has started.
	// But if there is no address on the fan bridge then DHCP won't work anyway.
	if n.config["bridge.mode"] == "fan" {
		iface, err := net.InterfaceByName(n.name)
		if err != nil {
			return nil
		}

		addrs, err := iface.Addrs()
		if err != nil {
			return nil
		}

		for _, addr := range addrs {
			ip, subnet, err := net.ParseCIDR(addr.String())
			if err != nil {
				continue
			}

			if ip != nil && err == nil && ip.To4() != nil && ip.IsGlobalUnicast() {
				return subnet // Use first IPv4 unicast address on host for DHCP subnet.
			}
		}

		return nil // No addresses found, means DHCP must be disabled.
	}

	// Non-fan mode. Return configured bridge subnet directly.
	_, subnet, err := net.ParseCIDR(n.config["ipv4.address"])
	if err != nil {
		return nil
	}

	return subnet
}

// DHCPv6Subnet returns the DHCPv6 subnet (if DHCP or SLAAC is enabled on network).
func (n *bridge) DHCPv6Subnet() *net.IPNet {
	// DHCP is disabled on this network.
	if !n.hasDHCPv6() {
		return nil
	}

	_, subnet, err := net.ParseCIDR(n.config["ipv6.address"])
	if err != nil {
		return nil
	}

	return subnet
}

// forwardConvertToFirewallForward converts forwards into format compatible with the firewall package.
func (n *bridge) forwardConvertToFirewallForwards(listenAddress net.IP, defaultTargetAddress net.IP, portMaps []*forwardPortMap) []firewallDrivers.AddressForward {
	var vips []firewallDrivers.AddressForward

	if defaultTargetAddress != nil {
		vips = append(vips, firewallDrivers.AddressForward{
			ListenAddress: listenAddress,
			TargetAddress: defaultTargetAddress,
		})
	}

	for _, portMap := range portMaps {
		vips = append(vips, firewallDrivers.AddressForward{
			ListenAddress: listenAddress,
			Protocol:      portMap.protocol,
			TargetAddress: portMap.targetAddress,
			ListenPorts:   portMap.listenPorts,
			TargetPorts:   portMap.targetPorts,
		})
	}

	return vips
}

// bridgeProjectNetworks takes a map of all networks in all projects and returns a filtered map of bridge networks.
func (n *bridge) bridgeProjectNetworks(projectNetworks map[string]map[int64]api.Network) map[string][]*api.Network {
	bridgeProjectNetworks := make(map[string][]*api.Network)
	for netProject, networks := range projectNetworks {
		for _, ni := range networks {
			network := ni // Local var creating pointer to rather than iterator.

			// Skip non-bridge networks.
			if network.Type != "bridge" {
				continue
			}

			if bridgeProjectNetworks[netProject] == nil {
				bridgeProjectNetworks[netProject] = []*api.Network{&network}
			} else {
				bridgeProjectNetworks[netProject] = append(bridgeProjectNetworks[netProject], &network)
			}
		}
	}

	return bridgeProjectNetworks
}

// bridgeNetworkExternalSubnets returns a list of external subnets used by bridge networks. Networks are considered
// to be using external subnets for their ipv4.address and/or ipv6.address if they have NAT disabled, and/or if
// they have external NAT addresses specified.
func (n *bridge) bridgeNetworkExternalSubnets(bridgeProjectNetworks map[string][]*api.Network) ([]externalSubnetUsage, error) {
	externalSubnets := make([]externalSubnetUsage, 0)
	for netProject, networks := range bridgeProjectNetworks {
		for _, netInfo := range networks {
			for _, keyPrefix := range []string{"ipv4", "ipv6"} {
				// If NAT is disabled, then network subnet is an external subnet.
				if !shared.IsTrue(netInfo.Config[fmt.Sprintf("%s.nat", keyPrefix)]) {
					key := fmt.Sprintf("%s.address", keyPrefix)

					_, ipNet, err := net.ParseCIDR(netInfo.Config[key])
					if err != nil {
						continue // Skip invalid/unspecified network addresses.
					}

					externalSubnets = append(externalSubnets, externalSubnetUsage{
						subnet:         *ipNet,
						networkProject: netProject,
						networkName:    netInfo.Name,
					})
				}

				// Find any external subnets used for network SNAT.
				if netInfo.Config[fmt.Sprintf("%s.nat.address", keyPrefix)] != "" {
					key := fmt.Sprintf("%s.nat.address", keyPrefix)

					subnetSize := 128
					if keyPrefix == "ipv4" {
						subnetSize = 32
					}

					_, ipNet, err := net.ParseCIDR(fmt.Sprintf("%s/%d", netInfo.Config[key], subnetSize))
					if err != nil {
						return nil, errors.Wrapf(err, "Failed parsing %q of %q in project %q", key, netInfo.Name, netProject)
					}

					externalSubnets = append(externalSubnets, externalSubnetUsage{
						subnet:         *ipNet,
						networkProject: netProject,
						networkName:    netInfo.Name,
						networkSNAT:    true,
					})
				}

				// Find any routes being used by the network.
				for _, cidr := range util.SplitNTrimSpace(netInfo.Config[fmt.Sprintf("%s.routes", keyPrefix)], ",", -1, true) {
					_, ipNet, err := net.ParseCIDR(cidr)
					if err != nil {
						continue // Skip invalid/unspecified network addresses.
					}

					externalSubnets = append(externalSubnets, externalSubnetUsage{
						subnet:         *ipNet,
						networkProject: netProject,
						networkName:    netInfo.Name,
					})
				}
			}
		}
	}

	return externalSubnets, nil
}

// bridgedNICExternalRoutes returns a list of external routes currently used by bridged NICs that are connected to
// networks specified.
func (n *bridge) bridgedNICExternalRoutes(bridgeProjectNetworks map[string][]*api.Network) ([]externalSubnetUsage, error) {
	externalRoutes := make([]externalSubnetUsage, 0)

	err := n.state.Cluster.InstanceList(nil, func(inst db.Instance, p db.Project, profiles []api.Profile) error {
		// Get the instance's effective network project name.
		instNetworkProject := project.NetworkProjectFromRecord(&p)

		if instNetworkProject != project.Default {
			return nil // Managed bridge networks can only exist in default project.
		}

		devices := db.ExpandInstanceDevices(deviceConfig.NewDevices(db.DevicesToAPI(inst.Devices)), profiles)

		// Iterate through each of the instance's devices, looking for bridged NICs that are linked to
		// networks specified.
		for devName, devConfig := range devices {
			if devConfig["type"] != "nic" {
				continue
			}

			// Check whether the NIC device references one of the networks supplied.
			if !NICUsesNetwork(devConfig, bridgeProjectNetworks[instNetworkProject]...) {
				continue
			}

			// For bridged NICs that are connected to networks specified, check if they have any
			// routes or external routes configured, and if so add them to the list to return.
			for _, key := range []string{"ipv4.routes", "ipv6.routes", "ipv4.routes.external", "ipv6.routes.external"} {
				for _, cidr := range util.SplitNTrimSpace(devConfig[key], ",", -1, true) {
					_, ipNet, _ := net.ParseCIDR(cidr)
					if ipNet == nil {
						// Skip if NIC device doesn't have a valid route.
						continue
					}

					externalRoutes = append(externalRoutes, externalSubnetUsage{
						subnet:          *ipNet,
						networkProject:  instNetworkProject,
						networkName:     devConfig["network"],
						instanceProject: inst.Project,
						instanceName:    inst.Name,
						instanceDevice:  devName,
					})
				}
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return externalRoutes, nil
}

// getExternalSubnetInUse returns information about usage of external subnets by bridge networks (and NICs
// connected to them) on this member.
func (n *bridge) getExternalSubnetInUse() ([]externalSubnetUsage, error) {
	var err error
	var projectNetworks map[string]map[int64]api.Network
	var projectNetworksForwardsOnUplink map[string]map[int64][]string

	err = n.state.Cluster.Transaction(func(tx *db.ClusterTx) error {
		// Get all managed networks across all projects.
		projectNetworks, err = tx.GetCreatedNetworks()
		if err != nil {
			return errors.Wrapf(err, "Failed to load all networks")
		}

		// Get all network forward listen addresses for forwards assigned to this specific cluster member.
		projectNetworksForwardsOnUplink, err = tx.GetProjectNetworkForwardListenAddressesOnMember()
		if err != nil {
			return errors.Wrapf(err, "Failed loading network forward listen addresses")
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	// Get managed bridge networks.
	bridgeProjectNetworks := n.bridgeProjectNetworks(projectNetworks)

	// Get external subnets used by other managed bridge networks.
	bridgeNetworkExternalSubnets, err := n.bridgeNetworkExternalSubnets(bridgeProjectNetworks)
	if err != nil {
		return nil, err
	}

	// Get external routes configured on bridged NICs.
	bridgedNICExternalRoutes, err := n.bridgedNICExternalRoutes(bridgeProjectNetworks)
	if err != nil {
		return nil, err
	}

	externalSubnets := make([]externalSubnetUsage, 0, len(bridgeNetworkExternalSubnets)+len(bridgedNICExternalRoutes))
	externalSubnets = append(externalSubnets, bridgeNetworkExternalSubnets...)
	externalSubnets = append(externalSubnets, bridgedNICExternalRoutes...)

	// Add forward listen addresses to this list.
	for projectName, networks := range projectNetworksForwardsOnUplink {
		for networkID, listenAddresses := range networks {
			for _, listenAddress := range listenAddresses {
				// Convert listen address to subnet.
				listenAddressNet, err := ParseIPToNet(listenAddress)
				if err != nil {
					return nil, fmt.Errorf("Invalid existing forward listen address %q", listenAddress)
				}

				// Create an externalSubnetUsage for the listen address by using the network ID
				// of the listen address to retrieve the already loaded network name from the
				// projectNetworks map.
				externalSubnets = append(externalSubnets, externalSubnetUsage{
					subnet:         *listenAddressNet,
					networkProject: projectName,
					networkName:    projectNetworks[projectName][networkID].Name,
				})
			}
		}
	}

	return externalSubnets, nil
}

// ForwardCreate creates a network forward.
func (n *bridge) ForwardCreate(forward api.NetworkForwardsPost, clientType request.ClientType) error {
	memberSpecific := true // bridge supports per-member forwards.

	// Check if there is an existing forward using the same listen address.
	_, _, err := n.state.Cluster.GetNetworkForward(n.ID(), memberSpecific, forward.ListenAddress)
	if err == nil {
		return api.StatusErrorf(http.StatusConflict, "A forward for that listen address already exists")
	}

	// Convert listen address to subnet so we can check its valid and can be used.
	listenAddressNet, err := ParseIPToNet(forward.ListenAddress)
	if err != nil {
		return errors.Wrapf(err, "Failed parsing address forward listen address %q", forward.ListenAddress)
	}

	_, err = n.forwardValidate(listenAddressNet.IP, &forward.NetworkForwardPut)
	if err != nil {
		return err
	}

	externalSubnetsInUse, err := n.getExternalSubnetInUse()
	if err != nil {
		return err
	}

	// Check the listen address subnet doesn't fall within any existing network external subnets.
	for _, externalSubnetUser := range externalSubnetsInUse {
		// Skip our own network's SNAT address (as it can be used for NICs in the network).
		if externalSubnetUser.networkSNAT && externalSubnetUser.networkProject == n.project && externalSubnetUser.networkName == n.name {
			continue
		}

		// Skip our own network (but not NIC devices on our own network).
		if externalSubnetUser.networkProject == n.project && externalSubnetUser.networkName == n.name && externalSubnetUser.instanceDevice == "" {
			continue
		}

		if SubnetContains(&externalSubnetUser.subnet, listenAddressNet) || SubnetContains(listenAddressNet, &externalSubnetUser.subnet) {
			// This error is purposefully vague so that it doesn't reveal any names of
			// resources potentially outside of the network.
			return fmt.Errorf("Forward listen address %q overlaps with another network or NIC", listenAddressNet.String())
		}
	}

	revert := revert.New()
	defer revert.Fail()

	// Create forward DB record.
	forwardID, err := n.state.Cluster.CreateNetworkForward(n.ID(), memberSpecific, &forward)
	if err != nil {
		return err
	}

	revert.Add(func() {
		n.state.Cluster.DeleteNetworkForward(n.ID(), forwardID)
		n.forwardSetupFirewall()
		n.forwardBGPSetupPrefixes()
	})

	err = n.forwardSetupFirewall()
	if err != nil {
		return err
	}

	// Check if hairpin mode needs to be enabled on active NIC bridge ports.
	if n.config["bridge.driver"] != "openvswitch" {
		brNetfilterEnabled := false
		for _, ipVersion := range []uint{4, 6} {
			if BridgeNetfilterEnabled(ipVersion) == nil {
				brNetfilterEnabled = true
				break
			}
		}

		// If br_netfilter is enabled and bridge has forwards, we enable hairpin mode on each NIC's bridge
		// port in case any of the forwards target the NIC and the instance attempts to connect to the
		// forward's listener. Without hairpin mode on the target of the forward will not be able to
		// connect to the listener.
		if brNetfilterEnabled {
			listenAddresses, err := n.state.Cluster.GetNetworkForwardListenAddresses(n.ID(), true)
			if err != nil {
				return fmt.Errorf("Failed loading network forwards: %w", err)
			}

			// If we are the first forward on this bridge, enable hairpin mode on active NIC ports.
			if len(listenAddresses) <= 1 {
				var localNode string

				err = n.state.Cluster.Transaction(func(tx *db.ClusterTx) error {
					localNode, err = tx.GetLocalNodeName()
					if err != nil {
						return errors.Wrapf(err, "Failed to get local member name")
					}

					return err
				})
				if err != nil {
					return err
				}

				filter := db.InstanceFilter{
					Node: &localNode,
				}

				err = n.state.Cluster.InstanceList(&filter, func(inst db.Instance, p db.Project, profiles []api.Profile) error {
					// Get the instance's effective network project name.
					instNetworkProject := project.NetworkProjectFromRecord(&p)

					if instNetworkProject != project.Default {
						return nil // Managed bridge networks can only exist in default project.
					}
					devices := db.ExpandInstanceDevices(deviceConfig.NewDevices(db.DevicesToAPI(inst.Devices)), profiles)

					// Iterate through each of the instance's devices, looking for bridged NICs
					// that are linked to this network.
					for devName, devConfig := range devices {
						if devConfig["type"] != "nic" {
							continue
						}

						// Check whether the NIC device references our network..
						if !NICUsesNetwork(devConfig, &api.Network{Name: n.Name()}) {
							continue
						}

						hostName := inst.Config[fmt.Sprintf("volatile.%s.host_name", devName)]
						if InterfaceExists(hostName) {
							link := &ip.Link{Name: hostName}
							err = link.BridgeLinkSetHairpin(true)
							if err != nil {
								return errors.Wrapf(err, "Error enabling hairpin mode on bridge port %q", link.Name)
							}
							n.logger.Debug("Enabled hairpin mode on NIC bridge port", log.Ctx{"inst": inst.Name, "project": inst.Project, "device": devName, "dev": link.Name})
						}
					}

					return nil
				})
				if err != nil {
					return err
				}
			}
		}
	}

	// Refresh exported BGP prefixes on local member.
	err = n.forwardBGPSetupPrefixes()
	if err != nil {
		return fmt.Errorf("Failed applying BGP prefixes for address forwards: %w", err)
	}

	revert.Success()
	return nil
}

// ForwardUpdate updates a network forward.
func (n *bridge) ForwardUpdate(listenAddress string, req api.NetworkForwardPut, clientType request.ClientType) error {
	memberSpecific := true // bridge supports per-member forwards.
	curForwardID, curForward, err := n.state.Cluster.GetNetworkForward(n.ID(), memberSpecific, listenAddress)
	if err != nil {
		return err
	}

	_, err = n.forwardValidate(net.ParseIP(curForward.ListenAddress), &req)
	if err != nil {
		return err
	}

	curForwardEtagHash, err := util.EtagHash(curForward.Etag())
	if err != nil {
		return err
	}

	newForward := api.NetworkForward{
		ListenAddress:     curForward.ListenAddress,
		NetworkForwardPut: req,
	}

	newForwardEtagHash, err := util.EtagHash(newForward.Etag())
	if err != nil {
		return err
	}

	if curForwardEtagHash == newForwardEtagHash {
		return nil // Nothing has changed.
	}

	revert := revert.New()
	defer revert.Fail()

	err = n.state.Cluster.UpdateNetworkForward(n.ID(), curForwardID, &newForward.NetworkForwardPut)
	if err != nil {
		return err
	}

	revert.Add(func() {
		n.state.Cluster.UpdateNetworkForward(n.ID(), curForwardID, &curForward.NetworkForwardPut)
		n.forwardSetupFirewall()
		n.forwardBGPSetupPrefixes()
	})

	err = n.forwardSetupFirewall()
	if err != nil {
		return err
	}

	// Refresh exported BGP prefixes on local member.
	err = n.forwardBGPSetupPrefixes()
	if err != nil {
		return fmt.Errorf("Failed applying BGP prefixes for address forwards: %w", err)
	}

	revert.Success()
	return nil
}

// ForwardDelete deletes a network forward.
func (n *bridge) ForwardDelete(listenAddress string, clientType request.ClientType) error {
	memberSpecific := true // bridge supports per-member forwards.
	forwardID, forward, err := n.state.Cluster.GetNetworkForward(n.ID(), memberSpecific, listenAddress)
	if err != nil {
		return err
	}

	revert := revert.New()
	defer revert.Fail()

	err = n.state.Cluster.DeleteNetworkForward(n.ID(), forwardID)
	if err != nil {
		return err
	}

	revert.Add(func() {
		newForward := api.NetworkForwardsPost{
			NetworkForwardPut: forward.NetworkForwardPut,
			ListenAddress:     forward.ListenAddress,
		}
		n.state.Cluster.CreateNetworkForward(n.ID(), memberSpecific, &newForward)
		n.forwardSetupFirewall()
		n.forwardBGPSetupPrefixes()
	})

	err = n.forwardSetupFirewall()
	if err != nil {
		return err
	}

	// Refresh exported BGP prefixes on local member.
	err = n.forwardBGPSetupPrefixes()
	if err != nil {
		return fmt.Errorf("Failed applying BGP prefixes for address forwards: %w", err)
	}

	revert.Success()
	return nil
}

// forwardSetupFirewall applies all network address forwards defined for this network and this member.
func (n *bridge) forwardSetupFirewall() error {
	memberSpecific := true // Get all forwards for this cluster member.
	forwards, err := n.state.Cluster.GetNetworkForwards(n.ID(), memberSpecific)
	if err != nil {
		return fmt.Errorf("Failed loading network forwards: %w", err)
	}

	var fwForwards []firewallDrivers.AddressForward
	ipVersions := make(map[uint]struct{})

	for _, forward := range forwards {
		// Convert listen address to subnet so we can check its valid and can be used.
		listenAddressNet, err := ParseIPToNet(forward.ListenAddress)
		if err != nil {
			return errors.Wrapf(err, "Failed parsing address forward listen address %q", forward.ListenAddress)
		}

		// Track which IP versions we are using.
		if listenAddressNet.IP.To4() == nil {
			ipVersions[6] = struct{}{}
		} else {
			ipVersions[4] = struct{}{}
		}

		portMaps, err := n.forwardValidate(listenAddressNet.IP, &forward.NetworkForwardPut)
		if err != nil {
			return fmt.Errorf("Failed validating firewall address forward for listen address %q: %w", forward.ListenAddress, err)
		}

		fwForwards = append(fwForwards, n.forwardConvertToFirewallForwards(listenAddressNet.IP, net.ParseIP(forward.Config["target_address"]), portMaps)...)
	}

	if len(forwards) > 0 {
		// Check if br_netfilter is enabled to, and warn if not.
		brNetfilterWarning := false
		for ipVersion := range ipVersions {
			err = BridgeNetfilterEnabled(ipVersion)
			if err != nil {
				brNetfilterWarning = true
				msg := fmt.Sprintf("IPv%d bridge netfilter not enabled. Instances using the bridge will not be able to connect to the forward listen IPs", ipVersion)
				n.logger.Warn(msg, log.Ctx{"err": err})
				err = n.state.Cluster.UpsertWarningLocalNode(n.project, dbCluster.TypeNetwork, int(n.id), db.WarningProxyBridgeNetfilterNotEnabled, fmt.Sprintf("%s: %v", msg, err))
				if err != nil {
					n.logger.Warn("Failed to create warning", log.Ctx{"err": err})
				}
			}
		}

		if !brNetfilterWarning {
			err = warnings.ResolveWarningsByLocalNodeAndProjectAndTypeAndEntity(n.state.Cluster, n.project, db.WarningProxyBridgeNetfilterNotEnabled, dbCluster.TypeNetwork, int(n.id))
			if err != nil {
				n.logger.Warn("Failed to resolve warning", log.Ctx{"err": err})
			}
		}
	}

	err = n.state.Firewall.NetworkApplyForwards(n.name, fwForwards)
	if err != nil {
		return fmt.Errorf("Failed applying firewall address forwards: %w", err)
	}

	return nil
}

// Leases returns a list of leases for the bridged network. It will reach out to other cluster members as needed.
// The projectName passed here refers to the initial project from the API request which may differ from the network's project.
func (n *bridge) Leases(projectName string, clientType request.ClientType) ([]api.NetworkLease, error) {
	leases := []api.NetworkLease{}
	projectMacs := []string{}

	// Get all static leases.
	if clientType == request.ClientTypeNormal {
		// Get the downstream networks.
		if n.project == project.Default {
			var err error

			// Load all the networks.
			var projectNetworks map[string]map[int64]api.Network
			err = n.state.Cluster.Transaction(func(tx *db.ClusterTx) error {
				projectNetworks, err = tx.GetCreatedNetworks()
				return err
			})
			if err != nil {
				return nil, err
			}

			// Look for networks using the current network as an uplink.
			for projectName, networks := range projectNetworks {
				for _, network := range networks {
					if network.Config["network"] != n.name {
						continue
					}

					// Found a network, add leases.
					for _, k := range []string{"volatile.network.ipv4.address", "volatile.network.ipv6.address"} {
						v := network.Config[k]
						if v != "" {
							leases = append(leases, api.NetworkLease{
								Hostname: fmt.Sprintf("%s-%s.uplink", projectName, network.Name),
								Address:  v,
								Type:     "uplink",
							})
						}
					}
				}
			}
		}

		// Get all the instances.
		instances, err := instance.LoadByProject(n.state, projectName)
		if err != nil {
			return nil, err
		}

		for _, inst := range instances {
			// Go through all its devices (including profiles).
			for k, dev := range inst.ExpandedDevices() {
				// Skip uninteresting entries.
				if dev["type"] != "nic" {
					continue
				}

				nicType, err := nictype.NICType(n.state, inst.Project(), dev)
				if err != nil || nicType != "bridged" {
					continue
				}

				// Temporarily populate parent from network setting if used.
				if dev["network"] != "" {
					dev["parent"] = dev["network"]
				}

				if dev["parent"] != n.name {
					continue
				}

				// Fill in the hwaddr from volatile.
				if dev["hwaddr"] == "" {
					dev["hwaddr"] = inst.LocalConfig()[fmt.Sprintf("volatile.%s.hwaddr", k)]
				}

				// Record the MAC.
				if dev["hwaddr"] != "" {
					projectMacs = append(projectMacs, dev["hwaddr"])
				}

				// Add the lease.
				if dev["ipv4.address"] != "" {
					leases = append(leases, api.NetworkLease{
						Hostname: inst.Name(),
						Address:  dev["ipv4.address"],
						Hwaddr:   dev["hwaddr"],
						Type:     "static",
						Location: inst.Location(),
					})
				}

				if dev["ipv6.address"] != "" {
					leases = append(leases, api.NetworkLease{
						Hostname: inst.Name(),
						Address:  dev["ipv6.address"],
						Hwaddr:   dev["hwaddr"],
						Type:     "static",
						Location: inst.Location(),
					})
				}

				// Add EUI64 records.
				ipv6Address := n.config["ipv6.address"]
				if ipv6Address != "" && ipv6Address != "none" && !shared.IsTrue(n.config["ipv6.dhcp.stateful"]) {
					_, netAddress, _ := net.ParseCIDR(ipv6Address)
					hwAddr, _ := net.ParseMAC(dev["hwaddr"])
					if netAddress != nil && hwAddr != nil {
						ipv6, err := eui64.ParseMAC(netAddress.IP, hwAddr)
						if err == nil {
							leases = append(leases, api.NetworkLease{
								Hostname: inst.Name(),
								Address:  ipv6.String(),
								Hwaddr:   dev["hwaddr"],
								Type:     "dynamic",
								Location: inst.Location(),
							})
						}
					}
				}
			}
		}
	}

	// Local server name.
	var err error
	var serverName string
	err = n.state.Cluster.Transaction(func(tx *db.ClusterTx) error {
		serverName, err = tx.GetLocalNodeName()
		return err
	})
	if err != nil {
		return nil, err
	}

	// Get dynamic leases.
	leaseFile := shared.VarPath("networks", n.name, "dnsmasq.leases")
	if !shared.PathExists(leaseFile) {
		return leases, nil
	}

	content, err := ioutil.ReadFile(leaseFile)
	if err != nil {
		return nil, err
	}

	for _, lease := range strings.Split(string(content), "\n") {
		fields := strings.Fields(lease)
		if len(fields) >= 5 {
			// Parse the MAC.
			mac := GetMACSlice(fields[1])
			macStr := strings.Join(mac, ":")

			if len(macStr) < 17 && fields[4] != "" {
				macStr = fields[4][len(fields[4])-17:]
			}

			// Look for an existing static entry.
			found := false
			for _, entry := range leases {
				if entry.Hwaddr == macStr && entry.Address == fields[2] {
					found = true
					break
				}
			}

			if found {
				continue
			}

			// DHCPv6 leases can't be tracked down to a MAC so clear the field.
			// This means that instance project filtering will not work on IPv6 leases.
			if strings.Contains(fields[2], ":") {
				macStr = ""
			}

			// Skip leases that don't match any of the instance MACs from the project (only when we
			// have populated the projectMacs list in ClientTypeNormal mode). Otherwise get all local
			// leases and they will be filtered on the server handling the end user request.
			if clientType == request.ClientTypeNormal && macStr != "" && !shared.StringInSlice(macStr, projectMacs) {
				continue
			}

			// Add the lease to the list.
			leases = append(leases, api.NetworkLease{
				Hostname: fields[3],
				Address:  fields[2],
				Hwaddr:   macStr,
				Type:     "dynamic",
				Location: serverName,
			})
		}
	}

	// Collect leases from other servers.
	if clientType == request.ClientTypeNormal {
		notifier, err := cluster.NewNotifier(n.state, n.state.Endpoints.NetworkCert(), n.state.ServerCert(), cluster.NotifyAll)
		if err != nil {
			return nil, err
		}

		err = notifier(func(client lxd.InstanceServer) error {
			memberLeases, err := client.GetNetworkLeases(n.name)
			if err != nil {
				return err
			}

			// Add local leases from other members, filtering them for MACs that belong to the project.
			for _, lease := range memberLeases {
				if lease.Hwaddr != "" && shared.StringInSlice(lease.Hwaddr, projectMacs) {
					leases = append(leases, lease)
				}
			}

			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	return leases, nil
}

// UsesDNSMasq indicates if network's config indicates if it needs to use dnsmasq.
func (n *bridge) UsesDNSMasq() bool {
	return n.config["bridge.mode"] == "fan" || !shared.StringInSlice(n.config["ipv4.address"], []string{"", "none"}) || !shared.StringInSlice(n.config["ipv6.address"], []string{"", "none"})
}
