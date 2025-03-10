package device

import (
	"fmt"

	"github.com/pkg/errors"

	deviceConfig "github.com/lxc/lxd/lxd/device/config"
	"github.com/lxc/lxd/lxd/instance"
	"github.com/lxc/lxd/lxd/instance/instancetype"
	"github.com/lxc/lxd/lxd/ip"
	"github.com/lxc/lxd/lxd/network"
	"github.com/lxc/lxd/lxd/resources"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/api"
)

type infinibandSRIOV struct {
	deviceCommon
}

// validateConfig checks the supplied config for correctness.
func (d *infinibandSRIOV) validateConfig(instConf instance.ConfigReader) error {
	if !instanceSupported(instConf.Type(), instancetype.Container) {
		return ErrUnsupportedDevType
	}

	requiredFields := []string{"parent"}
	optionalFields := []string{
		"name",
		"mtu",
		"hwaddr",
	}

	rules := nicValidationRules(requiredFields, optionalFields, instConf)
	rules["hwaddr"] = func(value string) error {
		if value == "" {
			return nil
		}

		return infinibandValidMAC(value)
	}

	err := d.config.Validate(rules)
	if err != nil {
		return err
	}

	return nil
}

// validateEnvironment checks the runtime environment for correctness.
func (d *infinibandSRIOV) validateEnvironment() error {
	if d.inst.Type() == instancetype.Container && d.config["name"] == "" {
		return fmt.Errorf("Requires name property to start")
	}

	if !shared.PathExists(fmt.Sprintf("/sys/class/net/%s", d.config["parent"])) {
		return fmt.Errorf("Parent device '%s' doesn't exist", d.config["parent"])
	}

	return nil
}

// Start is run when the device is added to a running instance or instance is starting up.
func (d *infinibandSRIOV) Start() (*deviceConfig.RunConfig, error) {
	err := d.validateEnvironment()
	if err != nil {
		return nil, err
	}

	saveData := make(map[string]string)

	// Load network interface info.
	nics, err := resources.GetNetwork()
	if err != nil {
		return nil, err
	}

	// Filter the network interfaces to just infiniband devices related to parent.
	ibDevs := infinibandDevices(nics, d.config["parent"])

	// We don't count the parent as an available VF.
	delete(ibDevs, d.config["parent"])

	// Load any interfaces already allocated to other devices.
	reservedDevices, err := network.SRIOVGetHostDevicesInUse(d.state)
	if err != nil {
		return nil, err
	}

	// Remove reserved devices from available list.
	for k := range reservedDevices {
		delete(ibDevs, k)
	}

	if len(ibDevs) < 1 {
		return nil, fmt.Errorf("All virtual functions on parent device are already in use")
	}

	// Get first VF device that is free.
	var vfDev *api.ResourcesNetworkCardPort
	for _, v := range ibDevs {
		vfDev = v
		break
	}

	saveData["host_name"] = vfDev.ID

	// Record hwaddr and mtu before potentially modifying them.
	err = networkSnapshotPhysicalNIC(saveData["host_name"], saveData)
	if err != nil {
		return nil, err
	}

	// Set the MAC address.
	if d.config["hwaddr"] != "" {
		err := infinibandSetDevMAC(saveData["host_name"], d.config["hwaddr"])
		if err != nil {
			return nil, fmt.Errorf("Failed to set the MAC address: %s", err)
		}
	}

	// Set the MTU.
	if d.config["mtu"] != "" {
		link := &ip.Link{Name: saveData["host_name"]}
		err := link.SetMTU(d.config["mtu"])
		if err != nil {
			return nil, errors.Wrapf(err, "Failed setting MTU %q on %q", d.config["mtu"], saveData["host_name"])
		}
	}

	runConf := deviceConfig.RunConfig{}

	// Configure runConf with infiniband setup instructions.
	err = infinibandAddDevices(d.state, d.inst.DevicesPath(), d.name, vfDev, &runConf)
	if err != nil {
		return nil, err
	}

	err = d.volatileSet(saveData)
	if err != nil {
		return nil, err
	}

	runConf.NetworkInterface = []deviceConfig.RunConfigItem{
		{Key: "type", Value: "phys"},
		{Key: "name", Value: d.config["name"]},
		{Key: "flags", Value: "up"},
		{Key: "link", Value: saveData["host_name"]},
	}

	return &runConf, nil
}

// Stop is run when the device is removed from the instance.
func (d *infinibandSRIOV) Stop() (*deviceConfig.RunConfig, error) {
	v := d.volatileGet()
	runConf := deviceConfig.RunConfig{
		PostHooks:        []func() error{d.postStop},
		NetworkInterface: []deviceConfig.RunConfigItem{{Key: "link", Value: v["host_name"]}},
	}

	err := unixDeviceRemove(d.inst.DevicesPath(), IBDevPrefix, d.name, "", &runConf)
	if err != nil {
		return nil, err
	}

	return &runConf, nil
}

// postStop is run after the device is removed from the instance.
func (d *infinibandSRIOV) postStop() error {
	defer d.volatileSet(map[string]string{
		"host_name":         "",
		"last_state.hwaddr": "",
		"last_state.mtu":    "",
	})

	// Remove infiniband host files for this device.
	err := unixDeviceDeleteFiles(d.state, d.inst.DevicesPath(), IBDevPrefix, d.name, "")
	if err != nil {
		return fmt.Errorf("Failed to delete files for device '%s': %v", d.name, err)
	}

	// Restore hwaddr and mtu.
	v := d.volatileGet()
	if v["host_name"] != "" {
		err := networkRestorePhysicalNIC(v["host_name"], v)
		if err != nil {
			return err
		}
	}

	return nil
}
