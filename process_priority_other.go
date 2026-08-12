//go:build !linux && !windows

package main

func lowerProcessPriority() error { return nil }
