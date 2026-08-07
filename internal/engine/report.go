package engine

import (
	"context"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/greendrake/gbsnap/internal/config"
	"github.com/greendrake/gbsnap/internal/pool"
)

// List writes out a volume's snapshots, saying which are protected from
// pruning and which targets already hold them.
func (e *Engine) List(ctx context.Context, v *config.Volume) error {
	s, err := e.open(ctx, v)
	if err != nil {
		return err
	}
	holders := map[string][]string{}
	for _, loc := range v.Targets {
		dst, err := s.target(ctx, loc)
		if err != nil {
			return err
		}
		for _, n := range dst.Names() {
			holders[n.String()] = append(holders[n.String()], loc.String())
		}
	}

	w := tabwriter.NewWriter(e.printer.Out, 0, 0, 2, ' ', 0)
	defer w.Flush()
	header := "SNAPSHOT\tPROTECTED"
	if len(v.Targets) > 0 {
		header += "\tHELD BY"
	}
	fmt.Fprintln(w, header)
	for _, n := range s.src.Names() {
		protection := "-"
		if n.Protected() {
			protection = n.Tag
		}
		row := n.String() + "\t" + protection
		if len(v.Targets) > 0 {
			at := "-"
			if held := holders[n.String()]; len(held) > 0 {
				at = strings.Join(held, ", ")
			}
			row += "\t" + at
		}
		fmt.Fprintln(w, row)
	}
	return nil
}

// Status writes out where one volume stands and how far its targets trail it.
func (e *Engine) Status(ctx context.Context, v *config.Volume) error {
	s, err := e.open(ctx, v)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(e.printer.Out, 0, 0, 2, ' ', 0)
	defer w.Flush()
	fmt.Fprintf(w, "%s\t%s\t%s\t%s\t\n", v.Name, v.Pool, count(len(s.src.Names())), latest(s.src))

	for _, loc := range v.Targets {
		dst, err := s.target(ctx, loc)
		if err != nil {
			return err
		}
		parent, ok := pool.Common(s.src.Names(), dst.Names())
		behind := len(pool.After(s.src.Names(), parent, ok))
		var trail string
		switch {
		case !dst.Exists():
			trail = "not created yet"
		case behind > 0:
			trail = count(behind) + " to send"
		default:
			trail = "up to date"
		}
		fmt.Fprintf(w, "  →\t%s\t%s\t%s\t%s\n", loc, count(len(dst.Names())), latest(dst), trail)
	}
	return nil
}

func count(n int) string {
	if n == 1 {
		return "1 snapshot"
	}
	return fmt.Sprintf("%d snapshots", n)
}

func latest(p *pool.Pool) string {
	n, ok := p.Latest()
	if !ok {
		return "empty"
	}
	return "latest " + n.String()
}
