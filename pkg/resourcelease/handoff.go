package resourcelease

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

const (
	handoffVersion        = 1
	defaultHandoffTimeout = 3 * time.Second
)

// HandoffEntry is one lease a previous process left to its successor.
type HandoffEntry struct {
	ID       string `json:"id"`
	Workload string `json:"workload"`
	PID      int    `json:"pid"`
	Device   string `json:"device"`
}

// HandoffReport describes what ReleaseHandoff did with the handoff file.
type HandoffReport struct {
	Released []string // DELETE answered 204/404
	Skipped  []string // IDs this client holds itself
	Failed   []string
	Err      error // read/parse/remove error (the file is removed anyway), or a file of another client (left alone)
}

// handoffFile is the on-disk format, shared with the Python SDK:
//
//	{"version":1,"client":"photos","written_at":"2026-10-29T01:02:03Z",
//	 "leases":[{"id":"1792802682-1","workload":"resident:clip_textual","pid":1234,"device":"cpu"}]}
type handoffFile struct {
	Version   int            `json:"version"`
	Client    string         `json:"client"`
	WrittenAt string         `json:"written_at"`
	Leases    []HandoffEntry `json:"leases"`
}

// handoffAfterRead, when set (tests only), runs in ReleaseHandoff right
// after the handoff file was read.
var handoffAfterRead atomic.Pointer[func()]

// errForeignHandoff marks a handoff file written by another client (two
// clients misconfigured with one path): it is neither used nor removed.
var errForeignHandoff = errors.New("resourcelease: handoff file belongs to another client")

// readHandoff reads and parses a handoff file of client. A missing file is
// (nil, false, nil); a file that is not a version-1 handoff file is an
// error, and one whose client is not client wraps errForeignHandoff.
// Entries without an id are dropped.
func readHandoff(path, client string) ([]HandoffEntry, bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, err
	}
	var f handoffFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, true, fmt.Errorf("resourcelease: parse handoff file %s: %w", path, err)
	}
	if f.Version != handoffVersion {
		return nil, true, fmt.Errorf("resourcelease: handoff file %s has version %d, want %d", path, f.Version, handoffVersion)
	}
	if f.Client != client {
		return nil, true, fmt.Errorf("%w: %s was written by client %q, not %q", errForeignHandoff, path, f.Client, client)
	}
	out := make([]HandoffEntry, 0, len(f.Leases))
	for _, e := range f.Leases {
		if e.ID != "" {
			out = append(out, e)
		}
	}
	return out, true, nil
}

// writeFileAtomic writes data to path through ".<base>.tmp-<pid>" in the same
// directory: write, fsync, rename. The file mode is 0600.
func writeFileAtomic(path string, data []byte) (err error) {
	dir, base := filepath.Split(path)
	tmp := filepath.Join(dir, "."+base+".tmp-"+strconv.Itoa(os.Getpid()))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// writeHandoff atomically writes entries to Options.HandoffFile. While the
// file a previous process left has not been taken over by ReleaseHandoff,
// its entries are kept in the new file (deduplicated by id, the new entry
// winning), so the successor still releases them. A file that cannot be
// read, or one written by another client, is logged at Warn and
// overwritten.
func (c *Client) writeHandoff(entries []HandoffEntry) error {
	c.handoffMu.Lock()
	defer c.handoffMu.Unlock()
	path := c.opts.HandoffFile
	var all []HandoffEntry
	if !c.handoffProcessed {
		old, _, err := readHandoff(path, c.opts.Client)
		if err != nil {
			c.logger().Warn("resource lease handoff file unreadable; overwriting it",
				zap.String("file", path), zap.Error(err))
		}
		fresh := make(map[string]bool, len(entries))
		for _, e := range entries {
			fresh[e.ID] = true
		}
		for _, e := range old {
			if !fresh[e.ID] {
				all = append(all, e)
				fresh[e.ID] = true // dedup within the old file too
			}
		}
	}
	all = append(all, entries...)
	data, err := json.Marshal(handoffFile{
		Version:   handoffVersion,
		Client:    c.opts.Client,
		WrittenAt: time.Now().UTC().Format(time.RFC3339),
		Leases:    all,
	})
	if err != nil {
		return err
	}
	if err := writeFileAtomic(path, data); err != nil {
		return err
	}
	c.handoffWritten = true
	return nil
}

// ReleaseHandoff deletes the leases listed in Options.HandoffFile that this
// client does not hold itself, then removes the file.
//
// A missing file (or no HandoffFile, or a disabled client) is a no-op. A file
// that cannot be parsed is removed without any DELETE and reported in Err
// (logged at Error). A file written by another client is left alone, with
// nothing deleted (logged at Warn, reported in Err). Each DELETE is bounded
// by Options.BookkeepingTimeout (3 s when 0); 204 and 404 count as released.
//
// After Close (or once Close wrote this process' own file) it does nothing:
// the file then lists the leases this process kept for its successor.
func (c *Client) ReleaseHandoff(ctx context.Context) HandoffReport {
	if c.opts.HandoffFile == "" || c.opts.Disabled {
		return HandoffReport{}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	path := c.opts.HandoffFile

	c.handoffMu.Lock()
	if c.handoffWritten || c.isClosed() {
		c.handoffMu.Unlock()
		return HandoffReport{}
	}
	entries, exists, err := readHandoff(path, c.opts.Client)
	if hook := handoffAfterRead.Load(); hook != nil {
		(*hook)()
	}
	if errors.Is(err, errForeignHandoff) {
		c.handoffProcessed = true // not ours: Close does not carry it over
		c.handoffMu.Unlock()
		c.logger().Warn("resource lease handoff file of another client; left alone",
			zap.String("client", c.opts.Client), zap.String("file", path), zap.Error(err))
		return HandoffReport{Err: err}
	}
	if err != nil {
		c.handoffProcessed = true
		if rerr := os.Remove(path); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			err = errors.Join(err, rerr)
		}
		c.handoffMu.Unlock()
		c.logger().Error("resource lease handoff file unusable; removed it",
			zap.String("client", c.opts.Client), zap.String("file", path), zap.Error(err))
		return HandoffReport{Err: err}
	}

	// The skip set and the take-over are decided under c.mu (lock order
	// handoffMu -> c.mu), in one step with the closed check: Close clears the
	// registry under c.mu, so it either ran before (nothing is taken over and
	// Close carries the old entries into the file it writes) or runs after
	// (the kept ids of this process are skipped here and written by Close).
	var rep HandoffReport
	var todo []HandoffEntry
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		c.handoffMu.Unlock()
		return HandoffReport{}
	}
	// From here on this process owns the old entries: Close no longer
	// carries them over into the file it writes.
	c.handoffProcessed = true
	seen := map[string]bool{}
	for _, e := range entries {
		if seen[e.ID] {
			continue
		}
		seen[e.ID] = true
		_, registered := c.leases[e.ID]
		_, kept := c.keep[e.ID]
		if registered || kept {
			rep.Skipped = append(rep.Skipped, e.ID)
			continue
		}
		todo = append(todo, e)
	}
	c.mu.Unlock()
	c.handoffMu.Unlock()
	if !exists {
		return HandoffReport{}
	}

	timeout := c.opts.BookkeepingTimeout
	if timeout <= 0 {
		timeout = defaultHandoffTimeout
	}
	type answer struct {
		id string
		ok bool
	}
	answers := make(chan answer, len(todo))
	for _, e := range todo {
		go func(id string) {
			dctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			_, err := c.do(dctx, http.MethodDelete, leasesPath+"/"+id, nil)
			answers <- answer{id: id, ok: err == nil || errors.Is(err, ErrLeaseGone) || errors.Is(err, ErrNotFound)}
		}(e.ID)
	}
	for range todo {
		a := <-answers
		if a.ok {
			rep.Released = append(rep.Released, a.id)
		} else {
			rep.Failed = append(rep.Failed, a.id)
		}
	}
	sort.Strings(rep.Released)
	sort.Strings(rep.Failed)

	c.handoffMu.Lock()
	// A Close that ran meanwhile wrote this process' own kept leases to the
	// file: leave it for the successor.
	if !c.handoffWritten {
		if rerr := os.Remove(path); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			rep.Err = rerr
		}
	}
	c.handoffMu.Unlock()

	c.logger().Info("resource lease handoff released",
		zap.String("client", c.opts.Client),
		zap.Int("released", len(rep.Released)),
		zap.Int("skipped", len(rep.Skipped)),
		zap.Int("failed", len(rep.Failed)))
	return rep
}
