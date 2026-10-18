//go:build !darwin && !linux

package crq

import "os/exec"

func configureDispatchProcess(_ *exec.Cmd) {}

func sweepDispatchProcess(_ *exec.Cmd) {}
