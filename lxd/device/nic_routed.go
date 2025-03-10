package device

import (
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/pkg/errors"

	deviceConfig "github.com/lxc/lxd/lxd/device/config"
	"github.com/lxc/lxd/lxd/instance"
	"github.com/lxc/lxd/lxd/instance/instancetype"
	"github.com/lxc/lxd/lxd/ip"
	"github.com/lxc/lxd/lxd/network"
	"github.com/lxc/lxd/lxd/revert"
	"github.com/lxc/lxd/lxd/util"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/validate"
)

var nicRoutedIPGateway = map[string]string{
	"ipv4": "169.254.0.1",
	"ipv6": "fe80::1",
}

type nicRouted struct {
	deviceCommon
}

// CanHotPlug returns whether the device can be managed whilst the instance is running.
func (d *nicRouted) CanHotPlug() bool {
	return false
}

// UpdatableFields returns a list of fields that can be updated without triggering a device remove & add.
func (d *nicRouted) UpdatableFields(oldDevice Type) []string {
	// Check old and new device types match.
	_, match := oldDevice.(*nicRouted)
	if !match {
		return []string{}
	}

	return []string{"limits.ingress", "limits.egress", "limits.max"}
}

// validateConfig checks the supplied config for correctness.
func (d *nicRouted) validateConfig(instConf instance.ConfigReader) error {
	if !instanceSupported(instConf.Type(), instancetype.Container, instancetype.VM) {
		return ErrUnsupportedDevType
	}

	err := d.isUniqueWithGatewayAutoMode(instConf)
	if err != nil {
		return err
	}

	requiredFields := []string{}
	optionalFields := []string{
		"name",
		"parent",
		"mtu",
		"hwaddr",
		"host_name",
		"vlan",
		"limits.ingress",
		"limits.egress",
		"limits.max",
		"ipv4.gateway",
		"ipv6.gateway",
		"ipv4.routes",
		"ipv6.routes",
		"ipv4.host_address",
		"ipv6.host_address",
		"ipv4.host_table",
		"ipv6.host_table",
		"gvrp",
	}

	rules := nicValidationRules(requiredFields, optionalFields, instConf)
	rules["ipv4.address"] = validate.Optional(validate.IsNetworkAddressV4List)
	rules["ipv6.address"] = validate.Optional(validate.IsNetworkAddressV6List)
	rules["gvrp"] = validate.Optional(validate.IsBool)

	err = d.config.Validate(rules)
	if err != nil {
		return err
	}

	// Detect duplicate IPs in config.
	for _, key := range []string{"ipv4.address", "ipv6.address"} {
		ips := make(map[string]struct{})

		if d.config[key] != "" {
			for _, addr := range strings.Split(d.config[key], ",") {
				addr = strings.TrimSpace(addr)
				if _, dupe := ips[addr]; dupe {
					return fmt.Errorf("Duplicate address %q in %q", addr, key)
				}

				ips[addr] = struct{}{}
			}
		}
	}

	// Ensure that address is set if routes is set
	for _, keyPrefix := range []string{"ipv4", "ipv6"} {
		if d.config[fmt.Sprintf("%s.routes", keyPrefix)] != "" && d.config[fmt.Sprintf("%s.address", keyPrefix)] == "" {
			return fmt.Errorf("%s.routes requires %s.address to be set", keyPrefix, keyPrefix)
		}
	}

	return nil
}

// validateEnvironment checks the runtime environment for correctness.
func (d *nicRouted) validateEnvironment() error {
	if d.inst.Type() == instancetype.Container && d.config["name"] == "" {
		return fmt.Errorf("Requires name property to start")
	}

	extensions := d.state.OS.LXCFeatures
	if !extensions["network_veth_router"] || !extensions["network_l2proxy"] {
		return fmt.Errorf("Requires liblxc has following API extensions: network_veth_router, network_l2proxy")
	}

	if d.config["parent"] != "" && !network.InterfaceExists(d.config["parent"]) {
		return fmt.Errorf("Parent device %q doesn't exist", d.config["parent"])
	}

	if d.config["parent"] == "" && d.config["vlan"] != "" {
		return fmt.Errorf("The vlan setting can only be used when combined with a parent interface")
	}

	// Check necessary "all" sysctls are configured for use with l2proxy parent for routed mode.
	if d.config["parent"] != "" && d.config["ipv6.address"] != "" {
		// net.ipv6.conf.all.forwarding=1 is required to enable general packet forwarding for IPv6.
		ipv6FwdPath := fmt.Sprintf("net/ipv6/conf/%s/forwarding", "all")
		sysctlVal, err := util.SysctlGet(ipv6FwdPath)
		if err != nil {
			return fmt.Errorf("Error reading net sysctl %s: %v", ipv6FwdPath, err)
		}
		if sysctlVal != "1\n" {
			return fmt.Errorf("Routed mode requires sysctl net.ipv6.conf.%s.forwarding=1", "all")
		}

		// net.ipv6.conf.all.proxy_ndp=1 is needed otherwise unicast neighbour solicitations are rejected.
		// This causes periodic latency spikes every 15-20s as the neighbour has to resort to using
		// multicast NDP resolution and expires the previous neighbour entry.
		ipv6ProxyNdpPath := fmt.Sprintf("net/ipv6/conf/%s/proxy_ndp", "all")
		sysctlVal, err = util.SysctlGet(ipv6ProxyNdpPath)
		if err != nil {
			return fmt.Errorf("Error reading net sysctl %s: %v", ipv6ProxyNdpPath, err)
		}
		if sysctlVal != "1\n" {
			return fmt.Errorf("Routed mode requires sysctl net.ipv6.conf.%s.proxy_ndp=1", "all")
		}
	}

	// Generate effective parent name, including the VLAN part if option used.
	effectiveParentName := network.GetHostDevice(d.config["parent"], d.config["vlan"])

	// If the effective parent doesn't exist and the vlan option is specified, it means we are going to create
	// the VLAN parent at start, and we will configure the needed sysctls so don't need to check them yet.
	if d.config["vlan"] != "" && network.InterfaceExists(effectiveParentName) {
		return nil
	}

	// Check necessary sysctls are configured for use with l2proxy parent for routed mode.
	if effectiveParentName != "" && d.config["ipv4.address"] != "" {
		ipv4FwdPath := fmt.Sprintf("net/ipv4/conf/%s/forwarding", effectiveParentName)
		sysctlVal, err := util.SysctlGet(ipv4FwdPath)
		if err != nil {
			return fmt.Errorf("Error reading net sysctl %s: %v", ipv4FwdPath, err)
		}
		if sysctlVal != "1\n" {
			// Replace . in parent name with / for sysctl formatting.
			return fmt.Errorf("Routed mode requires sysctl net.ipv4.conf.%s.forwarding=1", strings.Replace(effectiveParentName, ".", "/", -1))
		}
	}

	// Check necessary devic specific sysctls are configured for use with l2proxy parent for routed mode.
	if effectiveParentName != "" && d.config["ipv6.address"] != "" {
		ipv6FwdPath := fmt.Sprintf("net/ipv6/conf/%s/forwarding", effectiveParentName)
		sysctlVal, err := util.SysctlGet(ipv6FwdPath)
		if err != nil {
			return fmt.Errorf("Error reading net sysctl %s: %v", ipv6FwdPath, err)
		}
		if sysctlVal != "1\n" {
			// Replace . in parent name with / for sysctl formatting.
			return fmt.Errorf("Routed mode requires sysctl net.ipv6.conf.%s.forwarding=1", strings.Replace(effectiveParentName, ".", "/", -1))
		}

		ipv6ProxyNdpPath := fmt.Sprintf("net/ipv6/conf/%s/proxy_ndp", effectiveParentName)
		sysctlVal, err = util.SysctlGet(ipv6ProxyNdpPath)
		if err != nil {
			return fmt.Errorf("Error reading net sysctl %s: %v", ipv6ProxyNdpPath, err)
		}
		if sysctlVal != "1\n" {
			// Replace . in parent name with / for sysctl formatting.
			return fmt.Errorf("Routed mode requires sysctl net.ipv6.conf.%s.proxy_ndp=1", strings.Replace(effectiveParentName, ".", "/", -1))
		}
	}

	return nil
}

// Start is run when the instance is starting up (Routed mode doesn't support hot plugging).
func (d *nicRouted) Start() (*deviceConfig.RunConfig, error) {
	err := d.validateEnvironment()
	if err != nil {
		return nil, err
	}

	// Lock to avoid issues with containers starting in parallel.
	networkCreateSharedDeviceLock.Lock()
	defer networkCreateSharedDeviceLock.Unlock()

	saveData := make(map[string]string)

	// Decide which parent we should use based on VLAN setting.
	parentName := ""
	if d.config["parent"] != "" {
		parentName = network.GetHostDevice(d.config["parent"], d.config["vlan"])
		statusDev, err := networkCreateVlanDeviceIfNeeded(d.state, d.config["parent"], parentName, d.config["vlan"], shared.IsTrue(d.config["gvrp"]))
		if err != nil {
			return nil, err
		}

		// Record whether we created this device or not so it can be removed on stop.
		saveData["last_state.created"] = fmt.Sprintf("%t", statusDev != "existing")

		// If we created a VLAN interface, we need to setup the sysctls on that interface.
		if statusDev == "created" {
			err := d.setupParentSysctls(parentName)
			if err != nil {
				return nil, err
			}
		}
	}

	revert := revert.New()
	defer revert.Fail()

	saveData["host_name"] = d.config["host_name"]

	var peerName string

	// Create veth pair and configure the peer end with custom hwaddr and mtu if supplied.
	if d.inst.Type() == instancetype.Container {
		if saveData["host_name"] == "" {
			saveData["host_name"] = network.RandomDevName("veth")
		}
		peerName, err = networkCreateVethPair(saveData["host_name"], d.config)
	} else if d.inst.Type() == instancetype.VM {
		if saveData["host_name"] == "" {
			saveData["host_name"] = network.RandomDevName("tap")
		}
		peerName = saveData["host_name"] // VMs use the host_name to link to the TAP FD.
		err = networkCreateTap(saveData["host_name"], d.config)
	}
	if err != nil {
		return nil, err
	}

	revert.Add(func() { network.InterfaceRemove(saveData["host_name"]) })

	// Populate device config with volatile fields if needed.
	networkVethFillFromVolatile(d.config, saveData)

	// Apply host-side limits.
	err = networkSetupHostVethLimits(d.config)
	if err != nil {
		return nil, err
	}

	// Attempt to disable IPv6 router advertisement acceptance from instance.
	err = util.SysctlSet(fmt.Sprintf("net/ipv6/conf/%s/accept_ra", saveData["host_name"]), "0")
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	// Prevent source address spoofing by requiring a return path.
	err = util.SysctlSet(fmt.Sprintf("net/ipv4/conf/%s/rp_filter", saveData["host_name"]), "1")
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	// Apply firewall rules for reverse path filtering of IPv4 and IPv6.
	err = d.state.Firewall.InstanceSetupRPFilter(d.inst.Project(), d.inst.Name(), d.name, saveData["host_name"])
	if err != nil {
		return nil, errors.Wrapf(err, "Error setting up reverse path filter")
	}

	// Perform host-side address configuration.
	for _, keyPrefix := range []string{"ipv4", "ipv6"} {
		subnetSize := 32
		ipFamilyArg := ip.FamilyV4
		if keyPrefix == "ipv6" {
			subnetSize = 128
			ipFamilyArg = ip.FamilyV6
		}

		addresses := util.SplitNTrimSpace(d.config[fmt.Sprintf("%s.address", keyPrefix)], ",", -1, true)

		// Add host-side gateway addresses.
		if len(addresses) > 0 {
			// Add gateway IPs to the host end of the veth pair. This ensures that liveness detection
			// of the gateways inside the instance work and ensure that traffic doesn't periodically
			// halt whilst ARP/NDP is re-detected (which is what happens with just neighbour proxies).
			addr := &ip.Addr{
				DevName: saveData["host_name"],
				Address: fmt.Sprintf("%s/%d", d.ipHostAddress(keyPrefix), subnetSize),
				Family:  ipFamilyArg,
			}
			err = addr.Add()
			if err != nil {
				return nil, fmt.Errorf("Failed adding host gateway IP %q: %w", addr.Address, err)
			}

			// Enable IP forwarding on host_name.
			err = util.SysctlSet(fmt.Sprintf("net/%s/conf/%s/forwarding", keyPrefix, saveData["host_name"]), "1")
			if err != nil {
				return nil, err
			}
		}

		// Perform per-address host-side configuration (static routes and neighbour proxy entries).
		for _, addrStr := range addresses {
			// Apply host-side static routes to main routing table.
			r := ip.Route{
				DevName: saveData["host_name"],
				Route:   fmt.Sprintf("%s/%d", addrStr, subnetSize),
				Table:   "main",
				Family:  ipFamilyArg,
			}
			err = r.Add()
			if err != nil {
				return nil, fmt.Errorf("Failed adding host route %q: %w", r.Route, err)
			}

			// Add host-side static routes to instance IPs to custom routing table if specified.
			// This is in addition to the static route added to the main routing table, which is still
			// critical to ensure that reverse path filtering doesn't kick in blocking traffic from
			// the instance.
			if d.config[fmt.Sprintf("%s.host_table", keyPrefix)] != "" {
				r := ip.Route{
					DevName: saveData["host_name"],
					Route:   fmt.Sprintf("%s/%d", addrStr, subnetSize),
					Table:   d.config[fmt.Sprintf("%s.host_table", keyPrefix)],
					Family:  ipFamilyArg,
				}
				err = r.Add()
				if err != nil {
					return nil, fmt.Errorf("Failed adding host route %q to table %q: %w", r.Route, r.Table, err)
				}
			}

			// If there is a parent interface, add neighbour proxy entry.
			if parentName != "" {
				np := ip.NeighProxy{
					DevName: parentName,
					Addr:    net.ParseIP(addrStr),
				}
				err = np.Add()
				if err != nil {
					return nil, fmt.Errorf("Failed adding neighbour proxy %q to %q: %w", np.Addr.String(), np.DevName, err)
				}

				revert.Add(func() { np.Delete() })
			}
		}

		if d.config[fmt.Sprintf("%s.routes", keyPrefix)] != "" {
			routes := util.SplitNTrimSpace(d.config[fmt.Sprintf("%s.routes", keyPrefix)], ",", -1, true)

			if len(addresses) == 0 {
				return nil, fmt.Errorf("%s.routes requires %s.address to be set", keyPrefix, keyPrefix)
			}
			// Add routes
			for _, routeStr := range routes {
				// Apply host-side static routes to main routing table.
				r := ip.Route{
					DevName: saveData["host_name"],
					Route:   routeStr,
					Table:   "main",
					Family:  ipFamilyArg,
					Via:     addresses[0],
				}
				err = r.Add()
				if err != nil {
					return nil, fmt.Errorf("Failed adding route %q: %w", r.Route, err)
				}
			}

		}
	}

	err = d.volatileSet(saveData)
	if err != nil {
		return nil, err
	}

	// Perform instance NIC configuration.
	var nic []deviceConfig.RunConfigItem

	if d.inst.Type() == instancetype.Container {
		nic = append(nic, []deviceConfig.RunConfigItem{
			{Key: "type", Value: "phys"},
			{Key: "link", Value: peerName},
			{Key: "name", Value: d.config["name"]},
			{Key: "flags", Value: "up"},
		}...)

		for _, keyPrefix := range []string{"ipv4", "ipv6"} {
			ipAddresses := util.SplitNTrimSpace(d.config[fmt.Sprintf("%s.address", keyPrefix)], ",", -1, true)

			// Use a fixed address as the auto next-hop default gateway if using this IP family.
			if len(ipAddresses) > 0 && nicHasAutoGateway(d.config[fmt.Sprintf("%s.gateway", keyPrefix)]) {
				nic = append(nic, deviceConfig.RunConfigItem{Key: fmt.Sprintf("%s.gateway", keyPrefix), Value: d.ipHostAddress(keyPrefix)})
			}

			for _, addrStr := range ipAddresses {
				// Add addresses to instance NIC.
				if keyPrefix == "ipv6" {
					nic = append(nic, deviceConfig.RunConfigItem{Key: "ipv6.address", Value: fmt.Sprintf("%s/128", addrStr)})
				} else {
					// Specify the broadcast address as 0.0.0.0 as there is no broadcast address on
					// this link. This stops liblxc from trying to calculate a broadcast address
					// (and getting it wrong) which can prevent instances communicating with each other
					// using adjacent IP addresses.
					nic = append(nic, deviceConfig.RunConfigItem{Key: "ipv4.address", Value: fmt.Sprintf("%s/32 0.0.0.0", addrStr)})
				}
			}
		}
	} else if d.inst.Type() == instancetype.VM {
		nic = append(nic, []deviceConfig.RunConfigItem{
			{Key: "devName", Value: d.name},
			{Key: "link", Value: peerName},
			{Key: "hwaddr", Value: d.config["hwaddr"]},
		}...)
	}

	runConf := deviceConfig.RunConfig{
		NetworkInterface: nic,
	}

	revert.Success()
	return &runConf, nil
}

// setupParentSysctls configures the required sysctls on the parent to allow l2proxy to work.
// Because of our policy not to modify sysctls on existing interfaces, this should only be called
// if we created the parent interface.
func (d *nicRouted) setupParentSysctls(parentName string) error {
	if d.config["ipv4.address"] != "" {
		// Set necessary sysctls for use with l2proxy parent in routed mode.
		ipv4FwdPath := fmt.Sprintf("net/ipv4/conf/%s/forwarding", parentName)
		err := util.SysctlSet(ipv4FwdPath, "1")
		if err != nil {
			return fmt.Errorf("Error setting net sysctl %s: %v", ipv4FwdPath, err)
		}
	}

	if d.config["ipv6.address"] != "" {
		// Set necessary sysctls use with l2proxy parent in routed mode.
		ipv6FwdPath := fmt.Sprintf("net/ipv6/conf/%s/forwarding", parentName)
		err := util.SysctlSet(ipv6FwdPath, "1")
		if err != nil {
			return fmt.Errorf("Error setting net sysctl %s: %v", ipv6FwdPath, err)
		}

		ipv6ProxyNdpPath := fmt.Sprintf("net/ipv6/conf/%s/proxy_ndp", parentName)
		err = util.SysctlSet(ipv6ProxyNdpPath, "1")
		if err != nil {
			return fmt.Errorf("Error setting net sysctl %s: %v", ipv6ProxyNdpPath, err)
		}
	}

	return nil
}

// Update returns an error as most devices do not support live updates without being restarted.
func (d *nicRouted) Update(oldDevices deviceConfig.Devices, isRunning bool) error {
	v := d.volatileGet()

	// If instance is running, apply host side limits.
	if isRunning {
		err := d.validateEnvironment()
		if err != nil {
			return err
		}

		// Populate device config with volatile fields if needed.
		networkVethFillFromVolatile(d.config, v)

		// Apply host-side limits.
		err = networkSetupHostVethLimits(d.config)
		if err != nil {
			return err
		}
	}

	return nil
}

// Stop is run when the device is removed from the instance.
func (d *nicRouted) Stop() (*deviceConfig.RunConfig, error) {
	runConf := deviceConfig.RunConfig{
		PostHooks: []func() error{d.postStop},
	}

	return &runConf, nil
}

// postStop is run after the device is removed from the instance.
func (d *nicRouted) postStop() error {
	defer d.volatileSet(map[string]string{
		"last_state.created": "",
		"host_name":          "",
	})

	errs := []error{}

	v := d.volatileGet()

	networkVethFillFromVolatile(d.config, v)

	parentName := ""
	if d.config["parent"] != "" {
		parentName = network.GetHostDevice(d.config["parent"], d.config["vlan"])
	}

	// Delete host-side interface.
	if network.InterfaceExists(d.config["host_name"]) {
		// Removing host-side end of veth pair will delete the peer end too.
		err := network.InterfaceRemove(d.config["host_name"])
		if err != nil {
			errs = append(errs, errors.Wrapf(err, "Failed to remove interface %q", d.config["host_name"]))
		}
	}

	// Delete IP neighbour proxy entries on the parent.
	if parentName != "" {
		for _, key := range []string{"ipv4.address", "ipv6.address"} {
			for _, addr := range util.SplitNTrimSpace(d.config[key], ",", -1, true) {
				neighProxy := &ip.NeighProxy{
					DevName: parentName,
					Addr:    net.ParseIP(addr),
				}

				neighProxy.Delete()
			}
		}
	}

	// This will delete the parent interface if we created it for VLAN parent.
	if shared.IsTrue(v["last_state.created"]) {
		err := networkRemoveInterfaceIfNeeded(d.state, parentName, d.inst, d.config["parent"], d.config["vlan"])
		if err != nil {
			errs = append(errs, err)
		}
	}

	// Remove reverse path filters.
	err := d.state.Firewall.InstanceClearRPFilter(d.inst.Project(), d.inst.Name(), d.name)
	if err != nil {
		errs = append(errs, err)
	}

	if len(errs) > 0 {
		return fmt.Errorf("%v", errs)
	}

	return nil
}

func (d *nicRouted) ipHostAddress(ipFamily string) string {
	key := fmt.Sprintf("%s.host_address", ipFamily)
	if d.config[key] != "" {
		return d.config[key]
	}

	return nicRoutedIPGateway[ipFamily]
}

func (d *nicRouted) isUniqueWithGatewayAutoMode(instConf instance.ConfigReader) error {
	instDevs := instConf.ExpandedDevices()
	for _, k := range []string{"ipv4.gateway", "ipv6.gateway"} {
		if d.config[k] != "auto" && d.config[k] != "" {
			continue // nothing to do as auto not being used.
		}

		// Check other routed NIC devices don't have auto set.
		for nicName, nicConfig := range instDevs {
			if nicName == d.name || nicConfig["nictype"] != "routed" {
				continue // Skip ourselves.
			}

			if nicConfig[k] == "auto" || nicConfig[k] == "" {
				return fmt.Errorf("Existing NIC %q already uses %q in auto mode", nicName, k)
			}
		}
	}

	return nil
}
