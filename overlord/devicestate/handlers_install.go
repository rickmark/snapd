// -*- Mode: Go; indent-tabs-mode: t -*-

/*
 * Copyright (C) 2019 Canonical Ltd
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License version 3 as
 * published by the Free Software Foundation.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package devicestate

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"gopkg.in/tomb.v2"

	"github.com/snapcore/snapd/asserts"
	"github.com/snapcore/snapd/boot"
	"github.com/snapcore/snapd/dirs"
	"github.com/snapcore/snapd/logger"
	"github.com/snapcore/snapd/osutil"
	"github.com/snapcore/snapd/overlord/snapstate"
	"github.com/snapcore/snapd/overlord/state"
	"github.com/snapcore/snapd/secboot"
	"github.com/snapcore/snapd/sysconfig"
)

var (
	bootMakeBootable            = boot.MakeBootable
	sysconfigConfigureRunSystem = sysconfig.ConfigureRunSystem
)

func writeModel(model *asserts.Model, where string) error {
	f, err := os.OpenFile(where, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	return asserts.NewEncoder(f).Encode(model)
}

func (m *DeviceManager) doSetupRunSystem(t *state.Task, _ *tomb.Tomb) error {
	st := t.State()
	st.Lock()
	defer st.Unlock()

	perfTimings := state.TimingsForTask(t)
	defer perfTimings.Save(st)

	// get gadget dir
	deviceCtx, err := DeviceCtx(st, t, nil)
	if err != nil {
		return fmt.Errorf("cannot get device context: %v", err)
	}
	gadgetInfo, err := snapstate.GadgetInfo(st, deviceCtx)
	if err != nil {
		return fmt.Errorf("cannot get gadget info: %v", err)
	}
	gadgetDir := gadgetInfo.MountDir()

	kernelInfo, err := snapstate.KernelInfo(st, deviceCtx)
	if err != nil {
		return fmt.Errorf("cannot get kernel info: %v", err)
	}
	kernelDir := kernelInfo.MountDir()

	args := []string{
		// create partitions missing from the device
		"create-partitions",
		// mount filesystems after they're created
		"--mount",
	}

	useEncryption, err := checkEncryption(deviceCtx.Model())
	if err != nil {
		return err
	}
	if useEncryption {
		fdeDir := "var/lib/snapd/device/fde"
		args = append(args,
			// enable data encryption
			"--encrypt",
			// location to store the keyfile
			"--key-file", filepath.Join(boot.InitramfsEncryptionKeyDir, "ubuntu-data.sealed-key"),
			// location to store the recovery keyfile
			"--recovery-key-file", filepath.Join(boot.InitramfsWritableDir, fdeDir, "recovery.key"),
			// location to store the recovery keyfile
			"--tpm-lockout-auth", filepath.Join(boot.InitramfsWritableDir, fdeDir, "tpm-lockout-auth"),
			// location to store the authorization policy update data
			"--policy-update-data-file", filepath.Join(boot.InitramfsWritableDir, fdeDir, "policy-update-data"),
			// path to the kernel to install
			"--kernel", filepath.Join(kernelDir, "kernel.efi"),
		)
	}
	args = append(args, gadgetDir)

	// run the create partition code
	logger.Noticef("create and deploy partitions")
	st.Unlock()
	cmd := exec.Command(filepath.Join(dirs.DistroLibExecDir, "snap-bootstrap"), args...)
	cmd.Stderr = os.Stderr
	output, err := cmd.Output()
	st.Lock()
	if err != nil {
		return fmt.Errorf("cannot create partitions: %v", osutil.OutputErr(output, err))
	}

	// keep track of the model we installed
	err = writeModel(deviceCtx.Model(), filepath.Join(boot.InitramfsUbuntuBootDir, "model"))
	if err != nil {
		return fmt.Errorf("cannot store the model: %v", err)
	}

	// configure the run system
	opts := &sysconfig.Options{TargetRootDir: boot.InitramfsWritableDir}
	cloudCfg := filepath.Join(boot.InitramfsUbuntuSeedDir, "data/etc/cloud/cloud.cfg.d")
	// Support custom cloud.cfg.d/*.cfg files on the ubuntu-seed partition
	// during install when in grade "dangerous". We will support configs
	// from the gadget later too, see sysconfig/cloudinit.go
	//
	// XXX: maybe move policy decision into configureRunSystem later?
	if osutil.IsDirectory(cloudCfg) && deviceCtx.Model().Grade() == asserts.ModelDangerous {
		opts.CloudInitSrcDir = cloudCfg
	}
	if err := sysconfigConfigureRunSystem(opts); err != nil {
		return err
	}

	// make it bootable
	logger.Noticef("make system bootable")
	bootBaseInfo, err := snapstate.BootBaseInfo(st, deviceCtx)
	if err != nil {
		return fmt.Errorf("cannot get boot base info: %v", err)
	}
	modeEnv, err := m.maybeReadModeenv()
	if err != nil {
		return err
	}
	if modeEnv == nil {
		return fmt.Errorf("missing modeenv, cannot proceed")
	}
	recoverySystemDir := filepath.Join("/systems", modeEnv.RecoverySystem)
	bootWith := &boot.BootableSet{
		Base:              bootBaseInfo,
		BasePath:          bootBaseInfo.MountFile(),
		Kernel:            kernelInfo,
		KernelPath:        kernelInfo.MountFile(),
		RecoverySystemDir: recoverySystemDir,
	}
	rootdir := dirs.GlobalRootDir
	if err := bootMakeBootable(deviceCtx.Model(), rootdir, bootWith); err != nil {
		return fmt.Errorf("cannot make run system bootable: %v", err)
	}

	// request a restart as the last action after a successful install
	logger.Noticef("request system restart")
	st.RequestRestart(state.RestartSystemNow)

	return nil
}

var secbootCheckKeySealingSupported = secboot.CheckKeySealingSupported

// checkEncryption verifies whether encryption should be used based on the
// model grade and the availability of a TPM device.
func checkEncryption(model *asserts.Model) (res bool, err error) {
	secured := model.Grade() == asserts.ModelSecured
	dangerous := model.Grade() == asserts.ModelDangerous

	// check if we should disable encryption non-secured devices
	// TODO:UC20: this is not the final mechanism to bypass encryption
	if dangerous && osutil.FileExists(filepath.Join(boot.InitramfsUbuntuSeedDir, ".force-unencrypted")) {
		return false, nil
	}

	// encryption is required in secured devices and optional in other grades
	if err := secbootCheckKeySealingSupported(); err != nil {
		if secured {
			return false, fmt.Errorf("cannot encrypt secured device: %v", err)
		}
		return false, nil
	}

	return true, nil
}
