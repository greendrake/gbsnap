// Package ui renders gbsnap's user-facing output.
package ui

import (
	"fmt"
	"io"
)

// Printer writes progress and diagnostics at three levels: actions (what gbsnap
// does to the system), details (command echoes and non-events), and errors.
type Printer struct {
	Out     io.Writer
	Err     io.Writer
	Verbose bool
	Quiet   bool
}

// Action reports a change gbsnap makes, or would make under --dry-run.
func (p *Printer) Action(format string, a ...any) {
	if p.Quiet {
		return
	}
	fmt.Fprintf(p.Out, format+"\n", a...)
}

// Detail reports information that only --verbose asks for.
func (p *Printer) Detail(format string, a ...any) {
	if p.Quiet || !p.Verbose {
		return
	}
	fmt.Fprintf(p.Out, format+"\n", a...)
}

// Error reports a failure. Errors are never suppressed.
func (p *Printer) Error(format string, a ...any) {
	fmt.Fprintf(p.Err, "gbsnap: "+format+"\n", a...)
}
