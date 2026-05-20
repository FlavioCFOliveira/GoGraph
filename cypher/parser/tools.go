//go:build tools

// Package parser — tools.go pins build-time tool dependencies so that
// "go mod tidy" keeps them in go.mod/go.sum even when no generated source
// imports them yet. The blank import below is never compiled into normal
// builds; it is only visible when the "tools" build tag is active.
package parser

import (
	_ "github.com/antlr4-go/antlr/v4" // ANTLR4 Go runtime, required by the generated parser
)
