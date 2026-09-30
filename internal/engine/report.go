package engine

import (
	"context"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/greendrake/gbsnap/internal/config"
	"github.com/greendrake/gbsnap/internal/pool"
)

// List writes out a volume's snapshots, saying which are protected from
// pruning and which targets already hold them. A target that is offline cannot
// say, and is left out.
func (e *Engine) List(ctx context.Context, v *config.Volume) error {
	s, err := e.open(ctx, v)
	if err != nil {
		return err
	}
	holders := map[string][]string{}
	for _, loc := range v.Targets {
		t, err := s.target(ctx, loc)
		if err != nil {
			return err
		}
		if t.offline != "" {
			e.printer.Detail("%s: %s is offline: %s", v.Name, loc, t.offline)
			continue
		}
		for _, n := range t.pool.Names() {
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

// Status writes out where one volume stands and how far its targets trail it,
// and when each last received. A target the pool has a record of but that is
// no longer configured is shown too, since the pool keeps a snapshot for it.
func (e *Engine) Status(ctx context.Context, v *config.Volume) error {
	s, err := e.open(ctx, v)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(e.printer.Out, 0, 0, 2, ' ', 0)
	defer w.Flush()
	fmt.Fprintf(w, "%s\t%s\t%s\t%s\t\n", v.Name, v.Pool, count(len(s.src.Names())), latest(s.src))

	accounted := map[string]bool{}
	for _, loc := range v.Targets {
		t, err := s.target(ctx, loc)
		if err != nil {
			return err
		}
		r, ok, err := s.recorded(ctx, t)
		if err != nil {
			return err
		}
		last := "never received"
		if ok {
			accounted[r.Target] = true
			last = received(r)
		}
		if t.offline != "" {
			fmt.Fprintf(w, "  →\t%s\toffline\t%s\t%s\n", loc, last, t.offline)
			continue
		}
		parent, has := pool.Common(s.src.Names(), t.pool.Names())
		behind := len(pool.After(s.src.Names(), parent, has))
		var trail string
		switch {
		case !t.pool.Exists():
			trail = "not created yet"
		case behind > 0:
			trail = count(behind) + " to send"
		default:
			trail = "up to date"
		}
		fmt.Fprintf(w, "  →\t%s\t%s\t%s\t%s; %s\n", loc, count(len(t.pool.Names())), latest(t.pool), trail, last)
	}

	rs, err := s.loadRecords(ctx)
	if err != nil {
		return err
	}
	for _, r := range rs.List {
		if !accounted[r.Target] {
			fmt.Fprintf(w, "  ·\t%s\tnot a target now\t%s\tkept for it until gbsnap forget %s\n",
				r.Location, received(r), r.Location)
		}
	}
	return nil
}

func received(r pool.Record) string {
	return fmt.Sprintf("last received %s on %s", r.Snapshot, r.Since.Local().Format(time.DateTime))
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
