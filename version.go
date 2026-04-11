package main

import (
	"fmt"
	"os"
	"runtime"
)

var (
	// 以下变量通过 -ldflags 在构建时注入
	version   = "dev"
	gitBranch = "unknown"
	gitCommit = "unknown"
	buildTime = "unknown"
)

func printVersion() {
	fmt.Printf("clipboard-sync %s\n", version)
	fmt.Printf("  branch:      %s\n", gitBranch)
	fmt.Printf("  commit:      %s\n", gitCommit)
	fmt.Printf("  build time:  %s\n", buildTime)
	fmt.Printf("  go version:  %s\n", runtime.Version())
	fmt.Printf("  os/arch:     %s/%s\n", runtime.GOOS, runtime.GOARCH)
	os.Exit(0)
}
