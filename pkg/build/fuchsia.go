// Copyright 2018 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package build

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/google/syzkaller/pkg/osutil"
	"github.com/google/syzkaller/sys/targets"
)

type fuchsia struct{}

// syzRoot returns $GOPATH/src/github.com/google/syzkaller.
func syzRoot() (string, error) {
	_, selfPath, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("runtime.Caller failed")
	}

	return filepath.Abs(filepath.Join(filepath.Dir(selfPath), "../.."))
}

func (fu fuchsia) build(params Params) (ImageDetails, error) {
	syzDir, err := syzRoot()
	if err != nil {
		return ImageDetails{}, err
	}

	sysTarget := targets.Get(targets.Fuchsia, params.TargetArch)
	if sysTarget == nil {
		return ImageDetails{}, fmt.Errorf("unsupported fuchsia arch %v", params.TargetArch)
	}
	arch := sysTarget.KernelHeaderArch
	product := fmt.Sprintf("%s.%s", "core", arch)
	buildDir := filepath.Join(params.KernelDir, "out", arch)

	if _, err := runSandboxed(time.Hour, params.KernelDir,
		"scripts/fx", "--dir", buildDir,
		"set", product,
		"--args", fmt.Sprintf(`syzkaller_dir="%s"`, syzDir),
		"--with-base", "//bundles:tools",
		"--with-base", "//src/testing/fuzzing/syzkaller",
		"--variant", "kasan",
		"--no-goma",
	); err != nil {
		return ImageDetails{}, err
	}
	if _, err := runSandboxed(time.Hour*2, params.KernelDir, "scripts/fx", "clean-build"); err != nil {
		return ImageDetails{}, err
	}

	// Add ssh keys to the zbi image so syzkaller can access the fuchsia vm.
	_, sshKeyPub, err := genSSHKeys(params.OutputDir)
	if err != nil {
		return ImageDetails{}, err
	}

	sshZBI := filepath.Join(params.OutputDir, "initrd")
	kernelZBI, err := getImagePath(buildDir, "zircon-a", "zbi")
	if err != nil {
		return ImageDetails{}, err
	}
	authorizedKeys := fmt.Sprintf("data/ssh/authorized_keys=%s", sshKeyPub)

	zbiTool := filepath.Join(buildDir, "host_x64", "zbi")
	if _, err := osutil.RunCmd(time.Minute, params.KernelDir, zbiTool,
		"-o", sshZBI, kernelZBI, "--entry", authorizedKeys); err != nil {
		return ImageDetails{}, err
	}

	// Copy and extend the fvm.
	fvmTool := filepath.Join(buildDir, "host_x64", "fvm")
	fvmDst := filepath.Join(params.OutputDir, "image")
	fvmSrc, err := getImagePath(buildDir, "storage-full", "blk")
	if err != nil {
		return ImageDetails{}, err
	}
	if err := osutil.CopyFile(fvmSrc, fvmDst); err != nil {
		return ImageDetails{}, err
	}
	if _, err := osutil.RunCmd(time.Minute*5, params.KernelDir, fvmTool, fvmDst, "extend", "--length", "3G"); err != nil {
		return ImageDetails{}, err
	}

	kernelSrc, err := getImagePath(buildDir, "qemu-kernel", "kernel")
	if err != nil {
		return ImageDetails{}, err
	}
	zirconSrc := filepath.Join(buildDir, "kernel_"+arch+"-kasan", "zircon.elf")
	for src, dst := range map[string]string{
		zirconSrc: "obj/zircon.elf",
		kernelSrc: "kernel",
	} {
		fullDst := filepath.Join(params.OutputDir, filepath.FromSlash(dst))
		if err := osutil.CopyFile(src, fullDst); err != nil {
			return ImageDetails{}, fmt.Errorf("failed to copy %v: %v", src, err)
		}
	}
	return ImageDetails{}, nil
}

func (fu fuchsia) clean(kernelDir, targetArch string) error {
	// We always do clean build because incremental build is frequently broken.
	// So no need to clean separately.
	return nil
}

func runSandboxed(timeout time.Duration, dir, command string, arg ...string) ([]byte, error) {
	cmd := osutil.Command(command, arg...)
	cmd.Dir = dir
	if err := osutil.Sandbox(cmd, true, false); err != nil {
		return nil, err
	}
	return osutil.Run(timeout, cmd)
}

// genSSHKeys generates a pair of ssh keys inside the given directory, named key and key.pub.
// If both files already exist, this function does nothing.
// The function returns the path to both keys.
func genSSHKeys(dir string) (privKey, pubKey string, err error) {
	privKey = filepath.Join(dir, "key")
	pubKey = filepath.Join(dir, "key.pub")

	os.Remove(privKey)
	os.Remove(pubKey)

	if _, err := osutil.RunCmd(time.Minute*5, dir, "ssh-keygen", "-t", "rsa", "-b", "2048",
		"-N", "", "-C", "syzkaller-ssh", "-f", privKey); err != nil {
		return "", "", err
	}
	return privKey, pubKey, nil
}

// Relevant subset of Fuchsia build_api metadata stored in images.json
type imageMetadata struct {
	Name string
	Path string
	Type string
}

// Look up the path for the image with the given name and type
func getImagePath(buildDir, name, imageType string) (string, error) {
	jsonPath := filepath.Join(buildDir, "images.json")
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		return "", fmt.Errorf("failed to read %v: %v", jsonPath, err)
	}

	var images []imageMetadata
	if err := json.Unmarshal(data, &images); err != nil {
		return "", fmt.Errorf("failed to unmarshal %v: %v", jsonPath, err)
	}

	for _, metadata := range images {
		if metadata.Name == name && metadata.Type == imageType {
			return filepath.Join(buildDir, metadata.Path), nil
		}
	}

	return "", fmt.Errorf("no image found for name=%s, type=%s", name, imageType)
}
