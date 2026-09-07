//go:build linux

package main

import "syscall"

// dieWithParent makes the agent's process die with this launcher, whatever
// killed it: the kernel sends the parent-death signal with the launcher's
// credentials, so cap_kill carries it across the user boundary.
func dieWithParent(attr *syscall.SysProcAttr) { attr.Pdeathsig = syscall.SIGKILL }
