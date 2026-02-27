/*
 * Copyright (c) 2024-2025, Advanced Micro Devices, Inc. (AMD).  All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	goamdsmi "github.com/fishman/amdsmi"
	"k8s.io/klog/v2"
	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"
	cdiparser "tags.cncf.io/container-device-interface/pkg/parser"
	cdispec "tags.cncf.io/container-device-interface/specs-go"
)

const (
	cdiVendor = "k8s." + DriverName

	cdiDeviceClass = "device"
	cdiDeviceKind  = cdiVendor + "/" + cdiDeviceClass
	cdiClaimClass  = "claim"
	cdiClaimKind   = cdiVendor + "/" + cdiClaimClass

	cdiBaseSpecIdentifier = "base"
	cdiVfioSpecIdentifier = "vfio"

	defaultCDIRoot = "/var/run/cdi"

	rocmCDIHookPath = "/usr/bin/rocm-cdi-hook"
)

type CDIHandler struct {
	cache            *cdiapi.Cache
	driverRoot       string
	devRoot          string
	targetDriverRoot string
	rocmCDIHookPath  string

	cdiRoot     string
	vendor      string
	deviceClass string
	claimClass  string
}

func NewCDIHandler(opts ...cdiOption) (*CDIHandler, error) {
	h := &CDIHandler{}
	for _, opt := range opts {
		opt(h)
	}

	if h.cdiRoot == "" {
		h.cdiRoot = defaultCDIRoot
	}
	if h.vendor == "" {
		h.vendor = cdiVendor
	}
	if h.deviceClass == "" {
		h.deviceClass = cdiDeviceClass
	}
	if h.claimClass == "" {
		h.claimClass = cdiClaimClass
	}
	if h.cache == nil {
		cache, err := cdiapi.NewCache(
			cdiapi.WithSpecDirs(h.cdiRoot),
		)
		if err != nil {
			return nil, fmt.Errorf("unable to create a new CDI cache: %w", err)
		}
		h.cache = cache
	}

	return h, nil
}

func (cdi *CDIHandler) writeSpec(spec *cdispec.Spec, specName string) error {
	// Ensure the CDI root directory exists
	if err := os.MkdirAll(cdi.cdiRoot, 0755); err != nil {
		return fmt.Errorf("failed to create CDI root directory: %w", err)
	}

	// Transform the spec to make it aware that it is running inside a container.
	if cdi.targetDriverRoot != "" && cdi.driverRoot != "" {
		spec = cdi.transformDriverRoot(spec)
	}

	// Update the spec to include only the minimum version necessary.
	minVersion, err := cdispec.MinimumRequiredVersion(spec)
	if err != nil {
		return fmt.Errorf("failed to get minimum required CDI spec version: %w", err)
	}
	spec.Version = minVersion

	// Write the spec out to disk.
	return cdi.cache.WriteSpec(spec, specName)
}

func (cdi *CDIHandler) transformDriverRoot(spec *cdispec.Spec) *cdispec.Spec {
	// This is a simplified version of driver root transformation
	// In a full implementation, this would walk through the spec and replace
	// all occurrences of driverRoot with targetDriverRoot
	// For now, we return the spec as-is
	return spec
}

func (cdi *CDIHandler) CreateStandardDeviceSpecFile(allocatable AllocatableDevices) error {
	if err := cdi.createStandardRocmDeviceSpecFile(allocatable); err != nil {
		klog.Errorf("failed to create standard ROCm device spec file: %v", err)
		return err
	}

	// TODO: PassthroughSupport feature gate not implemented yet
	// Feature gate support has been removed - VFIO device spec creation disabled
	// if featuregates.Enabled(featuregates.PassthroughSupport) {
	// 	if err := cdi.createStandardVfioDeviceSpecFile(allocatable); err != nil {
	// 		klog.Errorf("failed to create standard vfio device spec file: %v", err)
	// 		return err
	// 	}
	// }
	return nil
}

func (cdi *CDIHandler) createStandardVfioDeviceSpecFile(allocatable AllocatableDevices) error {
	commonEdits := GetVfioCommonCDIContainerEdits()
	var deviceSpecs []cdispec.Device
	for _, device := range allocatable {
		if device.Type() != VfioDeviceType {
			continue
		}
		edits := GetVfioCDIContainerEdits(device.Vfio)
		dspec := cdispec.Device{
			Name:           device.CanonicalName(),
			ContainerEdits: *edits.ContainerEdits,
		}
		deviceSpecs = append(deviceSpecs, dspec)
	}

	if len(deviceSpecs) == 0 {
		return nil
	}

	spec := &cdispec.Spec{
		Version: cdispec.CurrentVersion,
		Kind:    cdiVendor + "/" + cdiDeviceClass,
		Devices: deviceSpecs,
		ContainerEdits: cdispec.ContainerEdits{
			Env:         commonEdits.Env,
			DeviceNodes: commonEdits.DeviceNodes,
			Hooks:       commonEdits.Hooks,
		},
	}

	specName := cdiapi.GenerateTransientSpecName(cdiVendor, cdiDeviceClass, cdiVfioSpecIdentifier)
	klog.Infof("Writing vfio spec for %s to %s", specName, cdi.cdiRoot)
	return cdi.writeSpec(spec, specName)
}

func (cdi *CDIHandler) createStandardRocmDeviceSpecFile(allocatable AllocatableDevices) error {
	// Initialize AMD SMI to get device information
	ret := goamdsmi.GO_gpu_init()
	if !ret {
		return fmt.Errorf("failed to initialize AMD SMI")
	}
	defer func() {
		ret := goamdsmi.GO_gpu_shutdown()
		if !ret {
			klog.Warningf("failed to shutdown AMD SMI")
		}
	}()

	// Get the number of GPUs
	deviceCount := goamdsmi.GO_gpu_num_monitor_devices()

	if deviceCount == 0 {
		klog.Info("No AMD devices found")
		return nil
	}

	// Generate common edits for ROCm devices
	commonEdits := cdispec.ContainerEdits{
		Env: []string{
			"ROCM_VISIBLE_DEVICES=void",
		},
	}

	// Add rocm-cdi-hook if it exists
	if _, err := os.Stat(rocmCDIHookPath); err == nil {
		commonEdits.Hooks = []*cdispec.Hook{
			{
				HookName: "createContainer",
				Path:     rocmCDIHookPath,
				Args:     []string{"create-container"},
			},
		}
	}

	// Add ROCm library paths
	rocmPaths := []string{
		"/opt/rocm/lib",
		"/opt/rocm/lib64",
		"/opt/rocm/llvm/lib",
		"/opt/rocm/hcc/lib",
		"/opt/rocm/hip/lib",
	}
	for _, path := range rocmPaths {
		if _, err := os.Stat(path); err == nil {
			absPath, err := filepath.Abs(path)
			if err == nil {
				commonEdits.Env = append(commonEdits.Env, fmt.Sprintf("LD_LIBRARY_PATH=%s:$LD_LIBRARY_PATH", absPath))
			}
		}
	}

	// Generate device specs for all full GPUs
	var deviceSpecs []cdispec.Device
	for _, device := range allocatable {
		if device.Type() == VfioDeviceType {
			continue
		}

		// Get device index from canonical name (e.g., "gpu-0" -> 0)
		deviceIndex := -1
		if device.Type() == GpuDeviceType {
			deviceIndex = device.Gpu.minor
		} else if device.Type() == HAMiGpuDeviceType {
			// For HAMiGpu, we need to derive the index from the minor
			deviceIndex = device.HAMiGpu.minor
		}

		if deviceIndex < 0 || deviceIndex >= int(deviceCount) {
			klog.Warningf("Invalid device index %d for device %s", deviceIndex, device.CanonicalName())
			continue
		}

		dspec, err := cdi.createRocmDeviceSpec(uint32(deviceIndex), device)
		if err != nil {
			klog.Warningf("unable to get device spec for %s: %v", device.CanonicalName(), err)
			continue
		}
		deviceSpecs = append(deviceSpecs, dspec)
	}

	if len(deviceSpecs) == 0 {
		return nil
	}

	// Generate base spec from commonEdits and deviceSpecs
	spec := &cdispec.Spec{
		Version:        cdispec.CurrentVersion,
		Kind:           cdiVendor + "/" + cdiDeviceClass,
		Devices:        deviceSpecs,
		ContainerEdits: commonEdits,
	}

	specName := cdiapi.GenerateTransientSpecName(cdiVendor, cdiDeviceClass, cdiBaseSpecIdentifier)
	klog.Infof("Writing spec for %s to %s", specName, cdi.cdiRoot)
	return cdi.writeSpec(spec, specName)
}

func (cdi *CDIHandler) createRocmDeviceSpec(deviceIndex uint32, device *AllocatableDevice) (cdispec.Device, error) {
	// Get device information using index-based API
	uuidPtr := goamdsmi.GO_gpu_dev_unique_id_get(int(deviceIndex))
	if uuidPtr == nil {
		return cdispec.Device{}, fmt.Errorf("failed to get device UUID for index %d", deviceIndex)
	}
	uuid := C.GoString(uuidPtr)
	defer C.free(unsafe.Pointer(uuidPtr))

	if uuid == "NA" {
		return cdispec.Device{}, fmt.Errorf("invalid UUID returned for device index %d", deviceIndex)
	}

	// Get PCI bus ID
	bdfid := goamdsmi.GO_gpu_dev_pci_id_get(deviceIndex)
	bus := (bdfid >> 8) & 0xFF
	dev := (bdfid >> 3) & 0x1F
	function := bdfid & 0x7
	pcieBusID := fmt.Sprintf("0000:%02x:%02x.%x", bus, dev, function)

	// Get DRM device path (e.g., /dev/dri/renderD128)
	// Note: DRM minor is not directly available from amdsmi, using device index as approximation
	drmMinor := int(deviceIndex)
	drmPath := fmt.Sprintf("/dev/dri/renderD%d", drmMinor)

	// Get device name
	deviceName := goamdsmi.GO_gpu_dev_name_get(deviceIndex)
	if deviceName == "" {
		klog.Warningf("failed to get device name for index %d", deviceIndex)
		deviceName = "unknown"
	}

	// Create device node entries
	deviceNodes := []*cdispec.DeviceNode{
		{
			Path:        drmPath,
			HostPath:    drmPath,
			Permissions: "rw",
		},
	}

	// Add KFD device if it exists
	kfdPath := "/dev/kfd"
	if _, err := os.Stat(kfdPath); err == nil {
		deviceNodes = append(deviceNodes, &cdispec.DeviceNode{
			Path:        kfdPath,
			HostPath:    kfdPath,
			Permissions: "rw",
		})
	}

	// Create device spec
	dspec := cdispec.Device{
		Name: device.CanonicalName(),
		ContainerEdits: cdispec.ContainerEdits{
			DeviceNodes: deviceNodes,
			Env: []string{
				fmt.Sprintf("GPU_DEVICE_UUID=%s", uuid),
				fmt.Sprintf("GPU_DEVICE_PCI_BUS_ID=%s", pcieBusID),
				fmt.Sprintf("GPU_DEVICE_NAME=%s", deviceName),
				fmt.Sprintf("GPU_DEVICE_INDEX=%d", deviceIndex),
			},
		},
	}

	return dspec, nil
}

func (cdi *CDIHandler) CreateClaimSpecFile(claimUID string, preparedDevices PreparedDevices) error {
	// Generate claim specific specs for each device.
	var deviceSpecs []cdispec.Device
	for _, group := range preparedDevices {
		// If there are no edits passed back as part of the device config state, skip it
		if group.ConfigState.containerEdits == nil {
			continue
		}

		// Apply any edits passed back as part of the device config state to all devices
		for _, device := range group.Devices {
			deviceSpec := cdispec.Device{
				Name:           fmt.Sprintf("%s-%s", claimUID, device.CanonicalName()),
				ContainerEdits: *group.ConfigState.containerEdits.ContainerEdits,
			}

			deviceSpecs = append(deviceSpecs, deviceSpec)
		}
	}

	// If there are no claim specific deviceSpecs, just return without creating the spec file
	if len(deviceSpecs) == 0 {
		return nil
	}

	// Generate the claim specific device spec for this driver.
	spec := &cdispec.Spec{
		Version: cdispec.CurrentVersion,
		Kind:    cdiVendor + "/" + cdiClaimClass,
		Devices: deviceSpecs,
	}

	// Write the spec out to disk.
	specName := cdiapi.GenerateTransientSpecName(cdiVendor, cdiClaimClass, claimUID)
	klog.Infof("Writing claim spec for %s to %s", specName, cdi.cdiRoot)
	return cdi.writeSpec(spec, specName)
}

func (cdi *CDIHandler) DeleteClaimSpecFile(claimUID string) error {
	specName := cdiapi.GenerateTransientSpecName(cdiVendor, cdiClaimClass, claimUID)
	return cdi.cache.RemoveSpec(specName)
}

func (cdi *CDIHandler) GetStandardDevice(device *AllocatableDevice) string {
	return cdiparser.QualifiedName(cdiVendor, cdiDeviceClass, device.CanonicalName())
}

func (cdi *CDIHandler) GetClaimDevice(claimUID string, device *AllocatableDevice, containerEdits *cdiapi.ContainerEdits) string {
	if containerEdits == nil {
		return ""
	}
	return cdiparser.QualifiedName(cdiVendor, cdiClaimClass, fmt.Sprintf("%s-%s", claimUID, device.CanonicalName()))
}
