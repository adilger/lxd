package resources

import (
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pkg/errors"
)

var sysBusPci = "/sys/bus/pci/devices"

func readUint(path string) (uint64, error) {
	content, err := ioutil.ReadFile(path)
	if err != nil {
		return 0, err
	}

	value, err := strconv.ParseUint(strings.TrimSpace(string(content)), 10, 64)
	if err != nil {
		return 0, err
	}

	return value, nil
}

func readInt(path string) (int64, error) {
	content, err := ioutil.ReadFile(path)
	if err != nil {
		return -1, err
	}

	value, err := strconv.ParseInt(strings.TrimSpace(string(content)), 10, 64)
	if err != nil {
		return -1, err
	}

	return value, nil
}

func stringInSlice(key string, list []string) bool {
	for _, entry := range list {
		if entry == key {
			return true
		}
	}
	return false
}

func int64InSlice(key int64, list []int64) bool {
	for _, entry := range list {
		if entry == key {
			return true
		}
	}
	return false
}

func sysfsExists(path string) bool {
	_, err := os.Lstat(path)
	if err == nil {
		return true
	}

	return false
}

func sysfsNumaNode(path string) (uint64, error) {
	// List all the directory entries
	entries, err := ioutil.ReadDir(path)
	if err != nil {
		return 0, err
	}

	// Iterate and look for NUMA
	for _, entry := range entries {
		entryName := entry.Name()

		if strings.HasPrefix(entryName, "node") && sysfsExists(filepath.Join(path, entryName, "numastat")) {
			node := strings.TrimPrefix(entryName, "node")

			nodeNumber, err := strconv.ParseUint(node, 10, 64)
			if err != nil {
				return 0, err
			}

			// Return the node we found
			return nodeNumber, nil
		}
	}

	// Didn't find a NUMA node for the device, assume single-node
	return 0, nil
}

func hasBit(n uint32, pos uint) bool {
	val := n & (1 << pos)
	return (val > 0)
}

func hasBitField(n []uint32, bit uint) bool {
	return (n[bit/32] & (1 << (bit % 32))) != 0
}

func udevDecode(s string) (string, error) {
	// Inverse of https://github.com/systemd/systemd/blob/master/src/basic/device-nodes.c#L22
	ret := ""
	for i := 0; i < len(s); i++ {
		// udev converts non-devnode supported chars to four byte encode hex strings.
		if s[i] == '\\' && i+4 <= len(s) && s[i+1] == 'x' {
			hexValue := s[i+2 : i+4]
			strValue, err := hex.DecodeString(hexValue)
			if err == nil {
				ret += string(strValue)
				i += 3
			} else {
				return ret, err
			}
		} else {
			ret += s[i : i+1]
		}
	}

	return ret, nil
}

func pciAddress(devicePath string) (string, error) {
	// Check if we have a subsystem listed at all.
	if !sysfsExists(filepath.Join(devicePath, "subsystem")) {
		return "", nil
	}

	// Track down the device.
	linkTarget, err := filepath.EvalSymlinks(devicePath)
	if err != nil {
		return "", errors.Wrapf(err, "Failed to find %q", devicePath)
	}

	// Extract the subsystem.
	subsystemTarget, err := filepath.EvalSymlinks(filepath.Join(linkTarget, "subsystem"))
	if err != nil {
		return "", errors.Wrapf(err, "Failed to find %q", filepath.Join(devicePath, "subsystem"))
	}
	subsystem := filepath.Base(subsystemTarget)

	if subsystem == "virtio" {
		// If virtio, consider the parent.
		linkTarget = filepath.Dir(linkTarget)
		subsystemTarget, err := filepath.EvalSymlinks(filepath.Join(linkTarget, "subsystem"))
		if err != nil {
			return "", errors.Wrapf(err, "Failed to find %q", filepath.Join(devicePath, "subsystem"))
		}

		subsystem = filepath.Base(subsystemTarget)
	}

	if subsystem != "pci" {
		return "", nil
	}

	// Address is the last entry.
	return filepath.Base(linkTarget), nil
}

// usbAddress returns the suspected USB address (bus:dev) for the device.
func usbAddress(devicePath string) (string, error) {
	// Resolve symlink.
	devicePath, err := filepath.EvalSymlinks(devicePath)
	if err != nil {
		return "", errors.Wrap(err, "Failed to resolve device symlink")
	}

	// Check if it looks like a USB device.
	if !strings.Contains(devicePath, "/usb") {
		return "", nil
	}

	path := devicePath
	for {
		// Avoid infinite loops.
		if path == "" || path == "/" {
			return "", nil
		}

		// Check if we found a usb device path.
		if !sysfsExists(filepath.Join(path, "busnum")) || !sysfsExists(filepath.Join(path, "devnum")) {
			path = filepath.Dir(path)
			continue
		}

		// Bus address.
		bus, err := readUint(filepath.Join(path, "busnum"))
		if err != nil {
			return "", errors.Wrap(err, "Unable to parse USB bus addr")
		}

		// Device address.
		dev, err := readUint(filepath.Join(path, "devnum"))
		if err != nil {
			return "", errors.Wrap(err, "Unable to parse USB device addr")
		}

		return fmt.Sprintf("%d:%d", bus, dev), nil
	}
}
