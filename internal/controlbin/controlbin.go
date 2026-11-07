// Package controlbin carries the sb-control binary inside release builds of sb,
// so `sb init` can install the control plane without Go or a container registry.
package controlbin

import "embed"

//go:embed all:payload
var payload embed.FS

// Binary returns the embedded linux/amd64 sb-control binary, if this build has one.
func Binary() ([]byte, bool) {
	b, err := payload.ReadFile("payload/sb-control")
	if err != nil || len(b) == 0 {
		return nil, false
	}
	return b, true
}
