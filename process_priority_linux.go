//go:build linux && !android

package main

import "golang.org/x/sys/unix"

func lowerProcessPriority() error {
	return unix.Setpriority(unix.PRIO_PROCESS, 0, 5)
}
