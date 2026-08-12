//go:build windows

package main

import "golang.org/x/sys/windows"

const belowNormalPriorityClass = 0x00004000

func lowerProcessPriority() error {
	return windows.SetPriorityClass(windows.CurrentProcess(), belowNormalPriorityClass)
}
