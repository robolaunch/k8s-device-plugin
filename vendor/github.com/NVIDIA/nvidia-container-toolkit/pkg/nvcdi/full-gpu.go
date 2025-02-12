/**
# Copyright (c) NVIDIA CORPORATION.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
**/

package nvcdi

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/NVIDIA/nvidia-container-toolkit/internal/discover"
	"github.com/NVIDIA/nvidia-container-toolkit/internal/edits"
	"github.com/NVIDIA/nvidia-container-toolkit/internal/info/drm"
	"github.com/container-orchestrated-devices/container-device-interface/pkg/cdi"
	"github.com/container-orchestrated-devices/container-device-interface/specs-go"
	"github.com/sirupsen/logrus"
	"gitlab.com/nvidia/cloud-native/go-nvlib/pkg/nvlib/device"
	"gitlab.com/nvidia/cloud-native/go-nvlib/pkg/nvml"
)

// GetGPUDeviceSpecs returns the CDI device specs for the full GPU represented by 'device'.
func (l *nvcdilib) GetGPUDeviceSpecs(i int, d device.Device) (*specs.Device, error) {
	edits, err := l.GetGPUDeviceEdits(d)
	if err != nil {
		return nil, fmt.Errorf("failed to get edits for device: %v", err)
	}

	name, err := l.deviceNamer.GetDeviceName(i, d)
	if err != nil {
		return nil, fmt.Errorf("failed to get device name: %v", err)
	}

	spec := specs.Device{
		Name:           name,
		ContainerEdits: *edits.ContainerEdits,
	}

	return &spec, nil
}

// GetGPUDeviceEdits returns the CDI edits for the full GPU represented by 'device'.
func (l *nvcdilib) GetGPUDeviceEdits(d device.Device) (*cdi.ContainerEdits, error) {
	device, err := newFullGPUDiscoverer(l.logger, l.driverRoot, l.nvidiaCTKPath, d)
	if err != nil {
		return nil, fmt.Errorf("failed to create device discoverer: %v", err)
	}

	editsForDevice, err := edits.FromDiscoverer(device)
	if err != nil {
		return nil, fmt.Errorf("failed to create container edits for device: %v", err)
	}

	return editsForDevice, nil
}

// byPathHookDiscoverer discovers the entities required for injecting by-path DRM device links
type byPathHookDiscoverer struct {
	logger        *logrus.Logger
	driverRoot    string
	nvidiaCTKPath string
	pciBusID      string
}

var _ discover.Discover = (*byPathHookDiscoverer)(nil)

// newFullGPUDiscoverer creates a discoverer for the full GPU defined by the specified device.
func newFullGPUDiscoverer(logger *logrus.Logger, driverRoot string, nvidiaCTKPath string, d device.Device) (discover.Discover, error) {
	// TODO: The functionality to get device paths should be integrated into the go-nvlib/pkg/device.Device interface.
	// This will allow reuse here and in other code where the paths are queried such as the NVIDIA device plugin.
	minor, ret := d.GetMinorNumber()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("error getting GPU device minor number: %v", ret)
	}
	path := fmt.Sprintf("/dev/nvidia%d", minor)

	pciInfo, ret := d.GetPciInfo()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("error getting PCI info for device: %v", ret)
	}
	pciBusID := getBusID(pciInfo)

	drmDeviceNodes, err := drm.GetDeviceNodesByBusID(pciBusID)
	if err != nil {
		return nil, fmt.Errorf("failed to determine DRM devices for %v: %v", pciBusID, err)
	}

	deviceNodePaths := append([]string{path}, drmDeviceNodes...)

	deviceNodes := discover.NewCharDeviceDiscoverer(
		logger,
		deviceNodePaths,
		driverRoot,
	)

	byPathHooks := &byPathHookDiscoverer{
		logger:        logger,
		driverRoot:    driverRoot,
		nvidiaCTKPath: nvidiaCTKPath,
		pciBusID:      pciBusID,
	}

	dd := discover.Merge(
		deviceNodes,
		byPathHooks,
	)

	return dd, nil
}

// Devices returns the empty list for the by-path hook discoverer
func (d *byPathHookDiscoverer) Devices() ([]discover.Device, error) {
	return nil, nil
}

// Hooks returns the hooks for the GPU device.
// The following hooks are detected:
//  1. A hook to create /dev/dri/by-path symlinks
func (d *byPathHookDiscoverer) Hooks() ([]discover.Hook, error) {
	links, err := d.deviceNodeLinks()
	if err != nil {
		return nil, fmt.Errorf("failed to discover DRA device links: %v", err)
	}
	if len(links) == 0 {
		return nil, nil
	}

	var args []string
	for _, l := range links {
		args = append(args, "--link", l)
	}

	hook := discover.CreateNvidiaCTKHook(
		d.nvidiaCTKPath,
		"create-symlinks",
		args...,
	)

	return []discover.Hook{hook}, nil
}

// Mounts returns an empty slice for a full GPU
func (d *byPathHookDiscoverer) Mounts() ([]discover.Mount, error) {
	return nil, nil
}

func (d *byPathHookDiscoverer) deviceNodeLinks() ([]string, error) {
	candidates := []string{
		fmt.Sprintf("/dev/dri/by-path/pci-%s-card", d.pciBusID),
		fmt.Sprintf("/dev/dri/by-path/pci-%s-render", d.pciBusID),
	}

	var links []string
	for _, c := range candidates {
		linkPath := filepath.Join(d.driverRoot, c)
		device, err := os.Readlink(linkPath)
		if err != nil {
			d.logger.Warningf("Failed to evaluate symlink %v; ignoring", linkPath)
			continue
		}

		d.logger.Debugf("adding device symlink %v -> %v", linkPath, device)
		links = append(links, fmt.Sprintf("%v::%v", device, linkPath))
	}

	return links, nil
}

// getBusID provides a utility function that returns the string representation of the bus ID.
func getBusID(p nvml.PciInfo) string {
	var bytes []byte
	for _, b := range p.BusId {
		if byte(b) == '\x00' {
			break
		}
		bytes = append(bytes, byte(b))
	}
	id := strings.ToLower(string(bytes))

	if id != "0000" {
		id = strings.TrimPrefix(id, "0000")
	}

	return id
}
