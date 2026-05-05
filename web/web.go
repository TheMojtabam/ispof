// Package web contains the embedded UI files (HTML, CSS, font).
package web

import (
	"embed"
	"io/fs"
)

//go:embed server-panel.html client-panel.html panel-base.css arad-font.css
var embedded embed.FS

// FS returns the embedded UI as an fs.FS rooted at the package dir.
func FS() fs.FS { return embedded }

// Server returns the foreign-side HTML file content.
func Server() ([]byte, error) { return embedded.ReadFile("server-panel.html") }

// Client returns the iran-side HTML file content.
func Client() ([]byte, error) { return embedded.ReadFile("client-panel.html") }

// CSS returns the panel-base.css content.
func CSS() ([]byte, error) { return embedded.ReadFile("panel-base.css") }

// Font returns the arad-font.css content.
func Font() ([]byte, error) { return embedded.ReadFile("arad-font.css") }
