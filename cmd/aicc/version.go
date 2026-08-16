// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// version is stamped by the release build:
//
//	go build -ldflags "-X main.version=v0.1.0" ./cmd/aicc
//
// Left unstamped — a `go build` from a working tree, or `go install` — it
// falls back to what the toolchain recorded about the source it compiled.
var version = ""

// runVersion implements `aicc version`. A deployment that cannot say which
// build it is running cannot be supported, and the container image has no
// shell to ask a package manager.
func runVersion([]string) error {
	fmt.Printf("aicc %s %s/%s %s\n", describeVersion(), runtime.GOOS, runtime.GOARCH, runtime.Version())
	return nil
}

func describeVersion() string {
	if version != "" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "(unknown)"
	}
	revision, modified := "", false
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}
	switch {
	case revision == "":
		return "(devel)"
	case modified:
		return revision[:min(len(revision), 12)] + "-dirty"
	default:
		return revision[:min(len(revision), 12)]
	}
}
