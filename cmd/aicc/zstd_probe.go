// SPDX-License-Identifier: Apache-2.0

package main

import "github.com/DataDog/zstd"

// verification-only: a dependency that needs cgo.
var _ = zstd.BestSpeed
