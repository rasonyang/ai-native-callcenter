// SPDX-License-Identifier: Apache-2.0

// Package web embeds the built single-page application.
//
// The frontend is built into web/dist by `npm run build`; the placeholder file
// keeps this package compilable before the first build, and Dist reports
// whether a real build is present.
package web

import (
	"embed"
	"errors"
	"io/fs"
)

//go:embed all:dist
var distFS embed.FS

// ErrNoBuild reports that no frontend build is embedded.
var ErrNoBuild = errors.New("no spa build embedded: run npm run build in web/")

// Dist returns the built SPA filesystem rooted at dist/.
func Dist() (fs.FS, error) {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		return nil, err
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return nil, ErrNoBuild
	}
	return sub, nil
}
