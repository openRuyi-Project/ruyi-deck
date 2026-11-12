// Package templates embeds the release index so deployment needs only the binary
// and YAML configuration files.
package templates

import _ "embed"

// Index is based on the supplied openRuyi release directory page.
//
//go:embed index.html.tmpl
var Index string
