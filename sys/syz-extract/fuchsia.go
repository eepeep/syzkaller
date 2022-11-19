// Copyright 2017 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package main

import (
	"path/filepath"

	"github.com/google/syzkaller/pkg/compiler"
)

type fuchsia struct{}

func (*fuchsia) prepare(sourcedir string, build bool, arches []*Arch) error {
	return nil
}

func (*fuchsia) prepareArch(arch *Arch) error {
	return nil
}

func (*fuchsia) processFile(arch *Arch, info *compiler.ConstInfo) (map[string]uint64, map[string]bool, error) {
	srcDir := arch.sourceDir
	outDir := filepath.Join(srcDir, "out", arch.target.KernelHeaderArch)

	cc := filepath.Join(srcDir, "prebuilt/third_party/clang/linux-x64/bin/clang")
	args := []string{
		"-fmessage-length=0",
		"-I", filepath.Join(outDir, "sdk/exported/core/arch/"+arch.target.KernelHeaderArch+"/sysroot/include"),
	}
	for _, incdir := range info.Incdirs {
		args = append(args, "-I", filepath.Join(srcDir, incdir))
	}

	params := &extractParams{
		DeclarePrintf:  true,
		DefineGlibcUse: true,
		TargetEndian:   arch.target.HostEndian,
	}
	return extract(info, cc, args, params)
}
