// Package api holds the API's contract, openapi.yaml, for the code that
// reads it at run time: package httpapi takes each operation's access rule
// from its security requirement.
package api

import _ "embed"

// Spec is openapi.yaml.
//
//go:embed openapi.yaml
var Spec []byte
