//go:build !linux

package main

import "syscall"

// dieWithParent is Linux-only; the launcher's stop path is documented for
// the pod, which is Linux. Elsewhere the agent is stopped by the forwarded
// signal alone.
func dieWithParent(*syscall.SysProcAttr) {}
