// Package web contains the prebuilt console assets, independent of HTTP.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var assets embed.FS

// Files is the embedded application rooted at its index document.
func Files() (fs.FS, error) { return fs.Sub(assets, "dist") }
