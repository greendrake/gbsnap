package pool

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"time"
)

// RecordsName is the file in a pool's directory that remembers, for each
// target the pool has sent to, the newest snapshot the two share. The leading
// dot keeps it out of the pool listing.
//
// A target that cannot be reached cannot say what it holds, and without that
// pruning would be free to let go of the very snapshot its next incremental
// send needs. The record says what to keep; it is never trusted to say what
// can be built on, which the UUID check settles once the target is reached.
const RecordsName = ".gbsnap-targets"

// Record is what the pool knows of one target.
type Record struct {
	// Target is what the record is keyed by: the target pool's ID.
	Target string `json:"target"`
	// Location is where the target was last reached.
	Location string `json:"location"`
	// Snapshot is the newest snapshot the target shares with the pool.
	Snapshot string `json:"snapshot"`
	// Since is when the target came to hold it.
	Since time.Time `json:"since"`
}

// Records is everything a pool knows of its targets.
type Records struct {
	List []Record `json:"targets"`
}

// Get finds the record for a target.
func (rs *Records) Get(target string) (Record, bool) {
	for _, r := range rs.List {
		if r.Target == target {
			return r, true
		}
	}
	return Record{}, false
}

// Set records the snapshot a target shares with the pool, reporting whether
// that changed anything.
func (rs *Records) Set(r Record) bool {
	for i, e := range rs.List {
		if e.Target == r.Target {
			if e.Snapshot == r.Snapshot && e.Location == r.Location {
				return false
			}
			if e.Snapshot == r.Snapshot {
				r.Since = e.Since // only the address changed
			}
			rs.List[i] = r
			return true
		}
	}
	rs.List = append(rs.List, r)
	return true
}

// Drop forgets the records that match, reporting them.
func (rs *Records) Drop(match func(Record) bool) []Record {
	var dropped []Record
	rs.List = slices.DeleteFunc(rs.List, func(r Record) bool {
		if match(r) {
			dropped = append(dropped, r)
			return true
		}
		return false
	})
	return dropped
}

// Rename follows a snapshot to a new name in every record that holds it.
func (rs *Records) Rename(from, to Name) bool {
	changed := false
	for i, r := range rs.List {
		if r.Snapshot == from.String() {
			rs.List[i].Snapshot = to.String()
			changed = true
		}
	}
	return changed
}

// LoadRecords reads what the pool knows of its targets. A pool that has never
// sent anything knows nothing.
func (p *Pool) LoadRecords(ctx context.Context) (*Records, error) {
	rs := &Records{}
	if !p.exists {
		return rs, nil
	}
	file := path.Join(p.Loc.Path, RecordsName)
	there, err := p.runner.Test(ctx, "test", "-f", file)
	if err != nil || !there {
		return rs, err
	}
	out, err := p.runner.Output(ctx, "cat", "--", file)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(out), rs); err != nil {
		return nil, fmt.Errorf("%s: %w", p.Loc.Child(RecordsName), err)
	}
	return rs, nil
}

// SaveRecords writes what the pool knows of its targets.
func (p *Pool) SaveRecords(ctx context.Context, rs *Records) error {
	data, err := json.MarshalIndent(rs, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(ctx, p.runner, path.Join(p.Loc.Path, RecordsName), string(data)+"\n")
}
