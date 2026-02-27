/*
 * Copyright (c) 2025, Advanced Micro Devices, Inc. (AMD).  All rights reserved.
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

// TODO: nvlib.go has featuregate references that have been commented out due to missing featuregates package

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"k8s.io/klog/v2"

	"k8s.io/dynamic-resource-allocation/deviceattribute"

	goamdsmi "github.com/fishman/amdsmi"
)

type deviceLib struct {
	initialized       bool
	driverLibraryPath string
	devRoot           string
	amdsmiPath        string
}

func newDeviceLib(driverRoot root) (*deviceLib, error) {
	driverLibraryPath, err := driverRoot.getDriverLibraryPath()
	if err != nil {
		return nil, fmt.Errorf("failed to locate driver libraries: %w", err)
	}

	// For ROCm, use amdsmi-clitool
	amdsmiPath, err := driverRoot.getAmdsmiPath()
	if err != nil {
		return nil, fmt.Errorf("failed to locate amdsmi: %w", err)
	}

	d := deviceLib{
		driverLibraryPath: driverLibraryPath,
		devRoot:           driverRoot.getDevRoot(),
		amdsmiPath:        amdsmiPath,
	}
	return &d, nil
}

// prependPathListEnvvar prepends a specified list of strings to a specified envvar and returns its value.
func prependPathListEnvvar(envvar string, prepend ...string) string {
	if len(prepend) == 0 {
		return os.Getenv(envvar)
	}
	current := filepath.SplitList(os.Getenv(envvar))
	return strings.Join(append(prepend, current...), string(filepath.ListSeparator))
}

// setOrOverrideEnvvar adds or updates an envar to the list of specified envvars and returns it.
func setOrOverrideEnvvar(envvars []string, key, value string) []string {
	var updated []string
	for _, envvar := range envvars {
		pair := strings.SplitN(envvar, "=", 2)
		if pair[0] == key {
			continue
		}
		updated = append(updated, envvar)
	}
	return append(updated, fmt.Sprintf("%s=%s", key, value))
}

func (l *deviceLib) Init() error {
	if l.initialized {
		return nil
	}
	ret := goamdsmi.GO_gpu_init()
	if !ret {
		return fmt.Errorf("error initializing AMDSMI")
	}
	l.initialized = true
	return nil
}

func (l *deviceLib) alwaysShutdown() {
	if !l.initialized {
		return
	}
	ret := goamdsmi.GO_gpu_shutdown()
	if !ret {
		klog.Warningf("error shutting down AMDSMI")
	}
	l.initialized = false
}

func (l *deviceLib) enumerateAllPossibleDevices(config *Config) (AllocatableDevices, error) {
	alldevices := make(AllocatableDevices)

	// TODO: Feature gates not implemented - defaulting to enumerateGpusAndMigDevices
	// if featuregates.Enabled(featuregates.HAMiCoreSupport) {
	// 	gms, err := l.enumerateGpusDevicesForHAMiCore(config)
	// 	if err != nil {
	// 		return nil, fmt.Errorf("error enumerating GPUs devices for HAMiCore: %w", err)
	// 	}
	// 	for k, v := range gms {
	// 		alldevices[k] = v
	// 	}
	// } else {
	gms, err := l.enumerateGpusAndMigDevices(config)
	if err != nil {
		return nil, fmt.Errorf("error enumerating GPUs and MIG devices: %w", err)
	}
	for k, v := range gms {
		alldevices[k] = v
	}

	// TODO: PassthroughSupport feature gate not implemented - disabled
	// if featuregates.Enabled(featuregates.PassthroughSupport) {
	// 	passthroughDevices, err := l.enumerateGpuPciDevices(config, gms)
	// 	if err != nil {
	// 		return nil, fmt.Errorf("error enumerating GPU PCI devices: %w", err)
	// 	}
	// 	for k, v := range passthroughDevices {
	// 		alldevices[k] = v
	// 	}
	// }
	// }

	return alldevices, nil
}

func (l *deviceLib) enumerateGpusAndMigDevices(config *Config) (AllocatableDevices, error) {
	if err := l.Init(); err != nil {
		return nil, err
	}
	defer l.alwaysShutdown()

	devices := make(AllocatableDevices)

	// Get number of GPUs
	numDevices := goamdsmi.GO_gpu_num_monitor_devices()

	// Visit all GPU devices using integer indices
	for i := uint32(0); i < uint32(numDevices); i++ {
		gpuInfo, err := l.getGpuInfo(i)
		if err != nil {
			return nil, fmt.Errorf("error getting info for GPU %d: %w", i, err)
		}

		deviceInfo := &AllocatableDevice{
			Gpu: gpuInfo,
		}
		devices[gpuInfo.CanonicalName()] = deviceInfo

		// ROCm does not support MIG, so we skip MIG device discovery
		// This maintains API compatibility while acknowledging lack of MIG on ROCm
		// TODO: PassthroughSupport feature gate not implemented - disabled
		// if featuregates.Enabled(featuregates.PassthroughSupport) {
		// 	// If no MIG devices are found, allow VFIO devices.
		// 	gpuInfo.vfioEnabled = true
		// }
	}

	return devices, nil
}

func (l *deviceLib) discoverMigDevicesByGPU(gpuInfo *GpuInfo) (AllocatableDeviceList, error) {
	// ROCm does not support MIG - return empty list
	// This maintains API compatibility while acknowledging lack of MIG on ROCm
	return nil, nil
}

func (l *deviceLib) discoverGPUByPCIBusID(pcieBusID string) (*AllocatableDevice, AllocatableDeviceList, error) {
	if err := l.Init(); err != nil {
		return nil, nil, err
	}
	defer l.alwaysShutdown()

	// Get number of GPUs
	numDevices := goamdsmi.GO_gpu_num_monitor_devices()

	var gpu *AllocatableDevice
	var migs AllocatableDeviceList

	// Visit all GPU devices to find matching PCIe bus ID
	for i := uint32(0); i < uint32(numDevices); i++ {
		pcieID, err := l.getPCIBusID(i)
		if err != nil {
			klog.Warningf("error getting PCIe bus ID for device %d: %v", i, err)
			continue
		}
		if pcieID != pcieBusID {
			continue
		}

		gpuInfo, err := l.getGpuInfo(i)
		if err != nil {
			return nil, nil, fmt.Errorf("error getting info for GPU %d: %w", i, err)
		}

		// ROCm does not support MIG
		gpuInfo.vfioEnabled = true

		gpu = &AllocatableDevice{
			Gpu: gpuInfo,
		}
		break
	}

	if gpu == nil {
		return nil, nil, fmt.Errorf("GPU with PCIe bus ID %s not found", pcieBusID)
	}

	return gpu, migs, nil
}

func (l *deviceLib) discoverVfioDevice(gpuInfo *GpuInfo) (*AllocatableDevice, error) {
	// For ROCm, VFIO device discovery is handled differently
	// We need to get PCI device information from the system
	// Since we don't have direct PCI device enumeration for AMD GPUs,
	// we'll construct VFIO device info from GPU info

	vfioDeviceInfo := &VfioDeviceInfo{
		UUID:                   uuid.NewSHA1(uuid.NameSpaceDNS, []byte(gpuInfo.pcieBusID)).String(),
		index:                  gpuInfo.minor,
		productName:            gpuInfo.productName,
		pcieBusID:              gpuInfo.pcieBusID,
		pcieRootAttr:           gpuInfo.pcieRootAttr,
		addressableMemoryBytes: gpuInfo.memoryBytes,
		// These would need to be obtained from sysfs for AMD GPUs
		deviceID:   "0x0000", // Placeholder
		vendorID:   "0x1002", // AMD vendor ID
		numaNode:   -1,       // Unknown
		iommuGroup: -1,       // Unknown
	}
	vfioDeviceInfo.parent = gpuInfo

	return &AllocatableDevice{
		Vfio: vfioDeviceInfo,
	}, nil
}

func (l *deviceLib) getGpuInfo(index uint32) (*GpuInfo, error) {
	// ROCm doesn't use minor numbers the same way as other GPU vendors
	// Use the device index as the minor number
	minor := int(index)

	// Get UUID
	uuidStr, err := l.getUUID(index)
	if err != nil {
		return nil, fmt.Errorf("error getting UUID for device %d: %w", index, err)
	}

	// ROCm doesn't support MIG
	migEnabled := false

	// Get memory info
	memoryBytes, err := l.getMemoryTotal(index)
	if err != nil {
		return nil, fmt.Errorf("error getting memory info for device %d: %w", index, err)
	}

	// Get product name
	productName, err := l.getDeviceName(index)
	if err != nil {
		return nil, fmt.Errorf("error getting product name for device %d: %w", index, err)
	}

	// Get architecture (ROCm doesn't have a direct equivalent, use product name)
	architecture := productName

	// Get ROCm compute capability
	// ROCm doesn't have a direct equivalent to CUDA compute capability
	// Use a placeholder based on architecture
	rocmComputeCapability := "1.0.0" // Default placeholder

	// Get driver version
	driverVersion, err := l.getDriverVersion()
	if err != nil {
		return nil, fmt.Errorf("error getting driver version: %w", err)
	}

	// Get ROCm driver version (same as driver version for AMD)
	rocmDriverVersion := driverVersion

	// Get PCIe bus ID
	pcieBusID, err := l.getPCIBusID(index)
	if err != nil {
		return nil, fmt.Errorf("error getting PCIe bus ID for device %d: %w", index, err)
	}

	var pcieRootAttr *deviceattribute.DeviceAttribute
	if attr, err := deviceattribute.GetPCIeRootAttributeByPCIBusID(pcieBusID); err == nil {
		pcieRootAttr = &attr
	} else {
		klog.Warningf("error getting PCIe root for device %d, continuing without attribute: %v", index, err)
	}

	// Brand for AMD GPUs
	brand := "AMD"

	// ROCm does not support MIG profiles - return empty slice
	migProfiles := []*MigProfileInfo{}

	gpuInfo := &GpuInfo{
		UUID:                  uuidStr,
		minor:                 minor,
		migEnabled:            migEnabled,
		memoryBytes:           memoryBytes,
		productName:           productName,
		brand:                 brand,
		architecture:          architecture,
		rocmComputeCapability: rocmComputeCapability,
		driverVersion:         driverVersion,
		rocmDriverVersion:     rocmDriverVersion,
		pcieBusID:             pcieBusID,
		pcieRootAttr:          pcieRootAttr,
		migProfiles:           migProfiles,
	}

	return gpuInfo, nil
}

func (l *deviceLib) enumerateGpuPciDevices(config *Config, gms AllocatableDevices) (AllocatableDevices, error) {
	devices := make(AllocatableDevices)

	// For ROCm, iterate through all GPUs and check if VFIO is enabled
	if err := l.Init(); err != nil {
		return nil, err
	}
	defer l.alwaysShutdown()

	numDevices := goamdsmi.GO_gpu_num_monitor_devices()

	for i := uint32(0); i < uint32(numDevices); i++ {
		gpuInfo, err := l.getGpuInfo(i)
		if err != nil {
			klog.Warningf("error getting GPU info for device %d: %v", i, err)
			continue
		}

		parent := gms.GetGPUByPCIeBusID(gpuInfo.pcieBusID)
		if parent == nil || !parent.Gpu.vfioEnabled {
			continue
		}

		vfioDeviceInfo := &VfioDeviceInfo{
			UUID:                   uuid.NewSHA1(uuid.NameSpaceDNS, []byte(gpuInfo.pcieBusID)).String(),
			index:                  gpuInfo.minor,
			productName:            gpuInfo.productName,
			pcieBusID:              gpuInfo.pcieBusID,
			pcieRootAttr:           gpuInfo.pcieRootAttr,
			addressableMemoryBytes: gpuInfo.memoryBytes,
			deviceID:               "0x0000", // Placeholder - would need sysfs lookup
			vendorID:               "0x1002", // AMD vendor ID
			numaNode:               -1,       // Unknown
			iommuGroup:             -1,       // Unknown
		}
		vfioDeviceInfo.parent = parent.Gpu

		devices[vfioDeviceInfo.CanonicalName()] = &AllocatableDevice{
			Vfio: vfioDeviceInfo,
		}
	}

	return devices, nil
}

func (l *deviceLib) getMigDevices(gpuInfo *GpuInfo) (map[string]*MigDeviceInfo, error) {
	// ROCm does not support MIG - return nil
	return nil, nil
}

func (l *deviceLib) setTimeSlice(uuids []string, timeSlice int) error {
	// ROCm/amdsmi doesn't have a direct equivalent to GPU compute-policy timeslice
	// This function is provided for API compatibility but may not have effect
	for _, uuid := range uuids {
		cmd := exec.Command(
			l.amdsmiPath,
			"set",
			"--uuid", uuid)

		// In order for amdsmi to run, we need update LD_PRELOAD to include the path to libamdsmi.so.1.
		cmd.Env = setOrOverrideEnvvar(os.Environ(), "LD_PRELOAD", prependPathListEnvvar("LD_PRELOAD", l.driverLibraryPath))

		output, err := cmd.CombinedOutput()
		if err != nil {
			klog.Errorf("\n%v", string(output))
			// Don't fail the operation for ROCm as this may not be supported
			klog.Warningf("amdsmi timeslice setting may not be supported: %w", err)
		}
	}
	return nil
}

func (l *deviceLib) setComputeMode(uuids []string, mode string) error {
	// ROCm/amdsmi doesn't have a direct equivalent to GPU compute mode
	// This function is provided for API compatibility but may not have effect
	for _, uuid := range uuids {
		cmd := exec.Command(
			l.amdsmiPath,
			"--uuid", uuid,
			"--mode", mode)

		// In order for amdsmi to run, we need update LD_PRELOAD to include the path to libamdsmi.so.1.
		cmd.Env = setOrOverrideEnvvar(os.Environ(), "LD_PRELOAD", prependPathListEnvvar("LD_PRELOAD", l.driverLibraryPath))

		output, err := cmd.CombinedOutput()
		if err != nil {
			klog.Errorf("\n%v", string(output))
			// Don't fail the operation for ROCm as this may not be supported
			klog.Warningf("amdsmi compute mode setting may not be supported: %w", err)
		}
	}
	return nil
}

// Helper functions using amdsmi API

func (l *deviceLib) getDeviceName(index uint32) (string, error) {
	name := goamdsmi.GO_gpu_dev_name_get(index)
	return name, nil
}

func (l *deviceLib) getUUID(index uint32) (string, error) {
	uuidStr := goamdsmi.GO_gpu_dev_unique_id_get(index)
	if uuidStr == "" {
		// Fall back to generating a UUID based on PCIe bus ID
		pcieID, err := l.getPCIBusID(index)
		if err != nil {
			return "", fmt.Errorf("error getting UUID and fallback failed: %v", err)
		}
		return uuid.NewSHA1(uuid.NameSpaceDNS, []byte(pcieID)).String(), nil
	}
	return uuidStr, nil
}

func (l *deviceLib) getPCIBusID(index uint32) (string, error) {
	bdfid := goamdsmi.GO_gpu_dev_pci_id_get(index)
	// Convert BDF to bus ID string format (e.g., "0000:03:00.0")
	bus := (bdfid >> 8) & 0xFF
	device := (bdfid >> 3) & 0x1F
	function := bdfid & 0x7
	return fmt.Sprintf("0000:%02x:%02x.%x", bus, device, function), nil
}

func (l *deviceLib) getMemoryTotal(index uint32) (uint64, error) {
	total := goamdsmi.GO_gpu_dev_gpu_memory_total_get(index)
	return total, nil
}

func (l *deviceLib) getDriverVersion() (string, error) {
	version := goamdsmi.GO_gpu_amdsmi_version_get()
	return version, nil
}
