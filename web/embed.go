// Package web embeds the built browser application.
package web

import "embed"

// Dist contains the browser bundle under dist/.
//
//go:embed all:dist
var Dist embed.FS
