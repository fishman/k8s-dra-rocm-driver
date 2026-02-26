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

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"k8s.io/klog/v2"
	cdiapi "tags.cncf.io/container-device-interface/pkg/cdi"
	cdispec "tags.cncf.io/container-device-interface/specs-go"

	goamdsmi "github.com/ROCm/amdsmi"
)

const (
	kernelIommuGroupPath   = "/sys/kernel/iommu_groups"
	vfioPciModule          = "vfio_pci"
	vfioPciDriver          = "vfio-pci"
	rocmDriver             = "amdgpu"
	hostRoot               = "/host-root"
	sysModulesRoot         = "/sys/module"
	pciDevicesRoot         = "/sys/bus/pci/devices"
	vfioDevicesRoot        = "/dev/vfio"
	unbindFromDriverScript = "/usr/bin/unbind_from_driver.sh"
	bindToDriverScript     = "/usr/bin/bind_to_driver.sh"
	driverResetRetries     = "5"
	gpuFreeCheckInterval   = 1 * time.Second
	gpuFreeCheckTimeout    = 60 * time.Second
)

type VfioPciManager struct {
	containerDriverRoot string
	hostDriverRoot      string
	driver              string
	amdsmi              goamdsmi.AmdSmiHandle
	rocmEnabled         bool
}

func NewVfioPciManager(containerDriverRoot string, hostDriverRoot string, rocmEnabled bool) (*VfioPciManager, error) {
	amdsmi := goamdsmi.NewAmdSmiHandle()

	vm := &VfioPciManager{
		containerDriverRoot: containerDriverRoot,
		hostDriverRoot:      hostDriverRoot,
		driver:              vfioPciDriver,
		amdsmi:              amdsmi,
		rocmEnabled:         rocmEnabled,
	}

	// Initialize AMDSMI
	ret := amdsmi.AmdSmiInit()
	if ret != goamdsmi.AMDSMI_STATUS_SUCCESS {
		return nil, fmt.Errorf("failed to initialize AMDSMI: %v", ret)
	}

	if !vm.isVfioPCIModuleLoaded() {
		err := vm.loadVfioPciModule()
		if err != nil {
			amdsmi.AmdSmiShutDown()
			return nil, fmt.Errorf("failed to load vfio_pci module: %v", err)
		}
	}

	return vm, nil
}

// Shutdown properly shuts down the AMDSMI interface
func (vm *VfioPciManager) Shutdown() {
	if vm.amdsmi != nil {
		vm.amdsmi.AmdSmiShutDown()
	}
}

// PreChecks tests if vfio-pci device allocations can be used.
func (vm *VfioPciManager) Prechecks() error {
	if !vm.isVfioPCIModuleLoaded() {
		return fmt.Errorf("vfio_pci module is not loaded")
	}
	iommuEnabled, err := vm.isIommuEnabled()
	if err != nil {
		return err
	}
	if !iommuEnabled {
		return fmt.Errorf("IOMMU is not enabled in the kernel")
	}
	return nil
}

func (vm *VfioPciManager) isIommuEnabled() (bool, error) {
	f, err := os.Open(kernelIommuGroupPath)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// defer f.Close()
	defer func() { _ = f.Close() }()
	_, err = f.Readdirnames(1)
	if err == io.EOF {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	return true, nil
}

func (vm *VfioPciManager) isVfioPCIModuleLoaded() bool {
	f, err := os.Stat(filepath.Join(sysModulesRoot, vfioPciModule))
	if err != nil {
		klog.Fatalf("failed to check if vfio_pci module is loaded: %v", err)
	}

	if !f.IsDir() {
		return false
	}

	return true
}

func (vm *VfioPciManager) loadVfioPciModule() error {
	_, err := execCommandWithChroot(hostRoot, "modprobe", []string{vfioPciModule}) //nolint:gosec
	if err != nil {
		return err
	}

	return nil
}

func (vm *VfioPciManager) WaitForGPUFree(ctx context.Context, info *VfioDeviceInfo) error {
	if info.parent == nil {
		return nil
	}
	timeout := time.After(gpuFreeCheckTimeout)
	ticker := time.NewTicker(gpuFreeCheckInterval)
	defer ticker.Stop()

	gpuDeviceNode := filepath.Join(vm.hostDriverRoot, "dev", "dri", fmt.Sprintf("renderD%d", info.parent.minor))
	for {
		select {
		case <-timeout:
			return fmt.Errorf("timed out waiting for gpu to be free")
		case <-ticker.C:
			out, err := execCommandWithChroot(hostRoot, "fuser", []string{gpuDeviceNode}) //nolint:gosec
			if err != nil {
				if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
					return nil
				}
				klog.Errorf("Unexpected error checking if gpu device %q is free: %v", info.pcieBusID, err)
				continue
			}
			klog.Infof("gpu device %q has open fds by process(es): %s", info.pcieBusID, string(out))
		}
	}
}

// Verify there are no VFs on the GPU using AMDSMI.
func (vm *VfioPciManager) verifyDisabledVFs(pcieBusID string) error {
	// Get all GPU devices using AMDSMI
	numDevices := uint32(0)
	ret := vm.amdsmi.AmdSmiGetNumDevices(&numDevices)
	if ret != goamdsmi.AMDSMI_STATUS_SUCCESS {
		return fmt.Errorf("failed to get number of GPU devices: %v", ret)
	}

	// Search for the device with matching PCIe bus ID
	for i := uint32(0); i < numDevices; i++ {
		deviceHandle, ret := vm.amdsmi.AmdSmiGetDeviceHandle(i)
		if ret != goamdsmi.AMDSMI_STATUS_SUCCESS {
			klog.Warningf("failed to get device handle for index %d: %v", i, ret)
			continue
		}

		bdf := uint64(0)
		ret = vm.amdsmi.AmdSmiGetDeviceBdf(deviceHandle, &bdf)
		if ret != goamdsmi.AMDSMI_STATUS_SUCCESS {
			klog.Warningf("failed to get BDF for device index %d: %v", i, ret)
			continue
		}

		// Convert BDF to PCIe bus ID format (XXXX:XX:XX.X)
		deviceBusID := fmt.Sprintf("%04x:%02x:%02x.%0x",
			(bdf>>24)&0xFFFF,
			(bdf>>12)&0xFF,
			(bdf>>4)&0xFF,
			bdf&0xF)

		if deviceBusID == pcieBusID {
			// Found the matching device, check SR-IOV status
			// AMDSMI doesn't have a direct API for VFs, so we check the sysfs
			sriovNumVFsPath := filepath.Join(pciDevicesRoot, pcieBusID, "sriov_numvfs")
			numVFs := 0
			if data, err := os.ReadFile(sriovNumVFsPath); err == nil {
				fmt.Sscanf(string(data), "%d", &numVFs)
			}

			if numVFs > 0 {
				return fmt.Errorf("gpu has %d VFs, cannot unbind", numVFs)
			}
			return nil
		}
	}

	return fmt.Errorf("GPU with PCIe bus ID %s not found", pcieBusID)
}

// Configure binds the GPU to the vfio-pci driver.
func (vm *VfioPciManager) Configure(ctx context.Context, info *VfioDeviceInfo) error {
	perGpuLock.Get(info.pcieBusID).Lock()
	defer perGpuLock.Get(info.pcieBusID).Unlock()

	driver, err := getDriver(pciDevicesRoot, info.pcieBusID)
	if err != nil {
		return err
	}
	if driver == vm.driver {
		return nil
	}
	// Only support vfio-pci or amdgpu (if vm.rocmEnabled) driver.
	if !vm.rocmEnabled || driver != rocmDriver {
		return fmt.Errorf("gpu is bound to %q driver, expected %q or %q", driver, vm.driver, rocmDriver)
	}
	err = vm.WaitForGPUFree(ctx, info)
	if err != nil {
		return err
	}
	err = vm.verifyDisabledVFs(info.pcieBusID)
	if err != nil {
		return err
	}
	err = vm.changeDriver(info.pcieBusID, vm.driver)
	if err != nil {
		return err
	}
	return nil
}

// Unconfigure binds the GPU to the amdgpu driver.
func (vm *VfioPciManager) Unconfigure(ctx context.Context, info *VfioDeviceInfo) error {
	perGpuLock.Get(info.pcieBusID).Lock()
	defer perGpuLock.Get(info.pcieBusID).Unlock()

	// Do nothing if we dont expect to switch to amdgpu driver.
	if !vm.rocmEnabled {
		return nil
	}

	driver, err := getDriver(pciDevicesRoot, info.pcieBusID)
	if err != nil {
		return err
	}
	if driver == rocmDriver {
		return nil
	}
	err = vm.changeDriver(info.pcieBusID, rocmDriver)
	if err != nil {
		return err
	}
	return nil
}

func getDriver(pciDevicesRoot, pciAddress string) (string, error) {
	driverPath, err := os.Readlink(filepath.Join(pciDevicesRoot, pciAddress, "driver"))
	if err != nil {
		return "", err
	}
	_, driver := filepath.Split(driverPath)
	return driver, nil
}

func (vm *VfioPciManager) changeDriver(pciAddress, driver string) error {
	err := vm.unbindFromDriver(pciAddress)
	if err != nil {
		return err
	}
	err = vm.bindToDriver(pciAddress, driver)
	if err != nil {
		return err
	}
	return nil
}

func (vm *VfioPciManager) unbindFromDriver(pciAddress string) error {
	out, err := execCommand(unbindFromDriverScript, []string{pciAddress}) //nolint:gosec
	if err != nil {
		klog.Errorf("Attempting to unbind %s from its driver failed; stdout: %s, err: %v", pciAddress, string(out), err)
		return err
	}
	return nil
}

func (vm *VfioPciManager) bindToDriver(pciAddress, driver string) error {
	out, err := execCommand(bindToDriverScript, []string{pciAddress, driver}) //nolint:gosec
	if err != nil {
		klog.Errorf("Attempting to bind %s to %s driver failed; stdout: %s, err: %v", pciAddress, driver, string(out), err)
		return err
	}
	return nil
}

func GetVfioCommonCDIContainerEdits() *cdiapi.ContainerEdits {
	return &cdiapi.ContainerEdits{
		ContainerEdits: &cdispec.ContainerEdits{
			DeviceNodes: []*cdispec.DeviceNode{
				{
					Path: filepath.Join(vfioDevicesRoot, "vfio"),
				},
			},
		},
	}
}

// GetVfioCDIContainerEdits returns the CDI spec for a container to have access to the GPU while bound on vfio-pci driver.
func GetVfioCDIContainerEdits(info *VfioDeviceInfo) *cdiapi.ContainerEdits {
	vfioDevicePath := filepath.Join(vfioDevicesRoot, fmt.Sprintf("%d", info.iommuGroup))
	return &cdiapi.ContainerEdits{
		ContainerEdits: &cdispec.ContainerEdits{
			DeviceNodes: []*cdispec.DeviceNode{
				{
					Path: vfioDevicePath,
				},
			},
		},
	}
}

func execCommandWithChroot(fsRoot, cmd string, args []string) ([]byte, error) {
	chrootArgs := []string{fsRoot, cmd}
	chrootArgs = append(chrootArgs, args...)
	return exec.Command("chroot", chrootArgs...).CombinedOutput()
}

func execCommand(cmd string, args []string) ([]byte, error) {
	return exec.Command(cmd, args...).CombinedOutput()
}
