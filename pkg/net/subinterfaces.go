package net

import (
	"fmt"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

func addMacVlan(containerNsPAth string, devName string, mode netlink.MacvlanMode) error {
	containerNs, err := netns.GetFromPath(containerNsPAth)
	if err != nil {
		return fmt.Errorf("could not get network namespace from path %s for network device %s : %w", containerNsPAth, devName, err)
	}
	defer containerNs.Close()

	parentLink, err := netlink.LinkByName(devName)
	if err != nil {
		return fmt.Errorf("could not find parent interface %s : %w", devName, err)
	}

	macvlan := &netlink.Macvlan{
		LinkAttrs: netlink.LinkAttrs{
			Name:        "mavlan-" + devName,
			ParentIndex: parentLink.Attrs().Index,
			NetNsID:     int(containerNs),
		},
		Mode: mode,
	}
	if err := netlink.LinkAdd(macvlan); err != nil {
		// If a user creates a macvlan and ipvlan on same parent, only one slave iface can be active at a time.
		return fmt.Errorf("failed to create the %s macvlan interface: %v", macvlan.Name, err)
	}

	return nil
}

func addIPVlan(containerNsPAth string, devName string, mode netlink.IPVlanMode) error {
	containerNs, err := netns.GetFromPath(containerNsPAth)
	if err != nil {
		return fmt.Errorf("could not get network namespace from path %s for network device %s : %w", containerNsPAth, devName, err)
	}
	defer containerNs.Close()

	parentLink, err := netlink.LinkByName(devName)
	if err != nil {
		return fmt.Errorf("could not find parent interface %s : %w", devName, err)
	}

	ipvlan := &netlink.IPVlan{
		LinkAttrs: netlink.LinkAttrs{
			Name:        "ipvaln-" + devName,
			ParentIndex: parentLink.Attrs().Index,
			NetNsID:     int(containerNs),
		},
		Mode: mode,
	}

	if err := netlink.LinkAdd(ipvlan); err != nil {
		// If a user creates a macvlan and ipvlan on same parent, only one slave iface can be active at a time.
		return fmt.Errorf("failed to create the %s ipvlan interface: %v", ipvlan.Name, err)
	}

	return nil
}
