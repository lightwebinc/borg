package module

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// JournalFile is the journal's file name in the state directory: the
// TypeScript module's, so one state directory reads under either.
const JournalFile = "borg-rows.jsonl"

// Line is one journal line: what ls_borg keeps for the host's life beside
// the engine's storage (spec sections 11.2 and 11.3), as an append-only
// journal. The engine's storage holds the outputs a host still keeps; it
// cannot hold what must outlive them: a dropped carrier, which carrier won
// an outpoint, the sweeps that spent it, and when the host first saw each
// token. One JSON object a line:
//
//	{"k":"c","op":"<txid>.<vout>","c":"<C>","txid":"<txid>","kind":"g","hall":"...","at":<s>}
//	{"k":"d","op":"...","c":"<C>","w":true}
//	{"k":"s","op":"...","s":"<sweep txid>","hall":"...","h":<height>,"at":<s>}
//	{"k":"t","txid":"<txid>","vout":<n>,"hall":"...","at":<s>}
//	{"k":"w","txid":"<sweep txid>","hall":"..."}
//
// c lines are every carrier any hall admitted that its row keeps (kind g a
// grant, p a policy record, w a wrap) with the funding outpoint it spent;
// d lines are drops, w when the dropped carrier was its outpoint's winner;
// s lines are admitted sweeps' spends of outpoints a carrier spent (spends
// of any other outpoint live only in the bounded table, never here); t
// lines are chain tokens; a w line names a held sweep whose tombstone an
// admitted transaction spent, which storage no longer hands a restore among
// its unspent rows. Commitments are in hash byte order and txids in display
// order, both lowercase hex; at is the host's time of first sight in Unix
// seconds. The bytes are the TypeScript module's, key for key, so one
// journal reads under either.
type Line struct {
	K    string `json:"k"`
	Op   string `json:"op,omitempty"`
	C    string `json:"c,omitempty"`
	Txid string `json:"txid,omitempty"`
	Kind string `json:"kind,omitempty"`
	Hall string `json:"hall,omitempty"`
	At   *int64 `json:"at,omitempty"`
	W    *bool  `json:"w,omitempty"`
	S    string `json:"s,omitempty"`
	H    *int64 `json:"h,omitempty"`
	Vout *int64 `json:"vout,omitempty"`
}

func i64(n int64) *int64 { return &n }
func boolp(b bool) *bool { return &b }

// at is a line's first sight, 0 when it has none.
func (l *Line) at() int64 {
	if l.At == nil {
		return 0
	}
	return *l.At
}

// encode writes a line as JSON.stringify writes the TypeScript module's
// object: the keys in the order the module sets them.
func (l *Line) encode() ([]byte, error) {
	var v any
	switch l.K {
	case "c":
		v = struct {
			K    string `json:"k"`
			Op   string `json:"op"`
			C    string `json:"c"`
			Txid string `json:"txid"`
			Kind string `json:"kind"`
			Hall string `json:"hall"`
			At   int64  `json:"at"`
		}{"c", l.Op, l.C, l.Txid, l.Kind, l.Hall, l.at()}
	case "d":
		w := l.W != nil && *l.W
		v = struct {
			K  string `json:"k"`
			Op string `json:"op"`
			C  string `json:"c"`
			W  bool   `json:"w"`
		}{"d", l.Op, l.C, w}
	case "s":
		var h int64
		if l.H != nil {
			h = *l.H
		}
		v = struct {
			K    string `json:"k"`
			Op   string `json:"op"`
			S    string `json:"s"`
			Hall string `json:"hall"`
			H    int64  `json:"h"`
			At   int64  `json:"at"`
		}{"s", l.Op, l.S, l.Hall, h, l.at()}
	case "t":
		var vout int64
		if l.Vout != nil {
			vout = *l.Vout
		}
		v = struct {
			K    string `json:"k"`
			Txid string `json:"txid"`
			Vout int64  `json:"vout"`
			Hall string `json:"hall"`
			At   int64  `json:"at"`
		}{"t", l.Txid, vout, l.Hall, l.at()}
	case "w":
		v = struct {
			K    string `json:"k"`
			Txid string `json:"txid"`
			Hall string `json:"hall"`
		}{"w", l.Txid, l.Hall}
	default:
		return nil, fmt.Errorf("journal: no line kind %q", l.K)
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// valid is whether a decoded line has its kind's members, as the
// TypeScript module's isLine reads one.
func (l *Line) valid() bool {
	nonneg := func(p *int64) bool { return p != nil && *p >= 0 }
	switch l.K {
	case "c":
		return l.Op != "" && l.C != "" && l.Txid != "" && (l.Kind == "g" || l.Kind == "p" || l.Kind == "w") && l.Hall != "" && nonneg(l.At)
	case "d":
		return l.Op != "" && l.C != "" && l.W != nil
	case "s":
		return l.Op != "" && l.S != "" && l.Hall != "" && nonneg(l.H) && nonneg(l.At)
	case "t":
		return l.Txid != "" && nonneg(l.Vout) && l.Hall != "" && nonneg(l.At)
	case "w":
		return l.Txid != "" && l.Hall != ""
	}
	return false
}

// Journal is where the rows live. Append is durable when it returns or
// within the journal's sync window (FileJournal.SyncEvery).
type Journal interface {
	Load() ([]Line, error)
	Append(l Line) error
}

// MemoryJournal is a journal in memory, for tests and for a host that
// accepts losing it.
type MemoryJournal struct {
	mu    sync.Mutex
	Lines []Line
}

// Load implements Journal.
func (m *MemoryJournal) Load() ([]Line, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Line(nil), m.Lines...), nil
}

// Append implements Journal.
func (m *MemoryJournal) Append(l Line) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Lines = append(m.Lines, l)
	return nil
}

// FileJournal is the journal as a file, borg-rows.jsonl in a directory.
// Each line is written to the file before Append returns, so a process
// that dies loses nothing (the line is in the page cache). When it reaches
// the disk is SyncEvery: zero flushes each line before Append returns, as
// the TypeScript module does; more is a group commit, one fsync for every
// line written in the window, run by a timer off the caller's path, so a
// power loss loses at most the window (the posture of the engine's store,
// which syncs its write-ahead log on a timer). Close flushes. A last line
// cut short by a crash is cut off on load; any other line that does not
// parse stops the load, because a row silently skipped could let a swept
// or superseded carrier be answered again.
type FileJournal struct {
	Path string
	// SyncEvery is the group commit window; zero syncs every line.
	SyncEvery time.Duration
	// OnSync, when set, is called after each timed fsync with the lines it
	// covered, how long it took and its error. It runs on the timer's
	// goroutine, holding no lock of the journal's.
	OnSync func(lines int, took time.Duration, err error)

	// syncMu orders fsyncs: one at a time, and Close waits for the one in
	// flight before it closes the file. Taken before mu, never after.
	syncMu sync.Mutex
	mu     sync.Mutex
	f      *os.File
	// pending counts the lines written since the last fsync began; armed is
	// whether a timed fsync is scheduled; failed is the last timed fsync's
	// error, returned by the next Append.
	pending int
	armed   bool
	timer   *time.Timer
	failed  error
	// fsync is (*os.File).Sync; tests count or fail it.
	fsync func(*os.File) error
}

func (j *FileJournal) sync(f *os.File) error {
	if j.fsync != nil {
		return j.fsync(f)
	}
	return f.Sync()
}

// NewFileJournal makes the directory and names the file. The journal
// syncs every line until SyncEvery is set.
func NewFileJournal(dir string) (*FileJournal, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &FileJournal{Path: filepath.Join(dir, JournalFile)}, nil
}

// Load implements Journal.
func (j *FileJournal) Load() ([]Line, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	text, err := os.ReadFile(j.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if len(text) > 0 && text[len(text)-1] != '\n' {
		// A last line cut short by a crash was never acknowledged: cut it
		// off, so the next line starts on its own.
		keep := bytes.LastIndexByte(text, '\n') + 1
		if err := os.Truncate(j.Path, int64(keep)); err != nil {
			return nil, err
		}
		text = text[:keep]
	}
	var out []Line
	sc := bufio.NewScanner(bytes.NewReader(text))
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	n := 0
	for sc.Scan() {
		n++
		raw := sc.Bytes()
		if len(raw) == 0 {
			continue
		}
		var l Line
		if err := json.Unmarshal(raw, &l); err != nil {
			return nil, fmt.Errorf("%s: line %d is not JSON", j.Path, n)
		}
		if !l.valid() {
			return nil, fmt.Errorf("%s: line %d is not a borg row", j.Path, n)
		}
		out = append(out, l)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", j.Path, err)
	}
	return out, nil
}

// Append implements Journal: the line is in the file when it returns, and
// on disk too when SyncEvery is zero; otherwise a timed fsync takes it
// there within SyncEvery. An error of an earlier timed fsync is returned
// once, by the next Append, after its line is written.
func (j *FileJournal) Append(l Line) error {
	b, err := l.encode()
	if err != nil {
		return err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.f == nil {
		f, err := os.OpenFile(j.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return err
		}
		j.f = f
	}
	if _, err := j.f.Write(b); err != nil {
		return err
	}
	if j.SyncEvery <= 0 {
		return j.sync(j.f)
	}
	j.pending++
	if !j.armed {
		j.armed = true
		j.timer = time.AfterFunc(j.SyncEvery, j.timedSync)
	}
	if err := j.failed; err != nil {
		j.failed = nil
		return fmt.Errorf("journal: a timed fsync failed: %w", err)
	}
	return nil
}

// timedSync is the group commit: one fsync for every line written since
// the last one began, outside the journal's lock, so Append goes on
// writing while it runs. A failed fsync leaves its lines pending for the
// next.
func (j *FileJournal) timedSync() {
	j.syncMu.Lock()
	j.mu.Lock()
	j.armed = false
	f, n := j.f, j.pending
	j.pending = 0
	j.mu.Unlock()
	if f == nil || n == 0 {
		j.syncMu.Unlock()
		return
	}
	start := time.Now()
	err := j.sync(f)
	took := time.Since(start)
	if err != nil {
		j.mu.Lock()
		j.pending += n
		j.failed = err
		if !j.armed && j.f != nil {
			j.armed = true
			j.timer = time.AfterFunc(j.SyncEvery, j.timedSync)
		}
		j.mu.Unlock()
	}
	j.syncMu.Unlock()
	if j.OnSync != nil {
		j.OnSync(n, took, err)
	}
}

// Sync flushes every line written so far to disk now.
func (j *FileJournal) Sync() error {
	j.syncMu.Lock()
	defer j.syncMu.Unlock()
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.flushLocked()
}

// flushLocked fsyncs the open file when lines are pending. The caller holds
// syncMu and mu.
func (j *FileJournal) flushLocked() error {
	if j.f == nil || j.pending == 0 {
		return nil
	}
	if err := j.sync(j.f); err != nil {
		return err
	}
	j.pending = 0
	j.failed = nil
	return nil
}

// Close flushes what is pending and closes the file. A later Append opens
// it again.
func (j *FileJournal) Close() error {
	j.syncMu.Lock()
	defer j.syncMu.Unlock()
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.timer != nil {
		j.timer.Stop()
		j.timer = nil
	}
	j.armed = false
	if j.f == nil {
		return nil
	}
	err := j.flushLocked()
	if cerr := j.f.Close(); err == nil {
		err = cerr
	}
	j.f = nil
	return err
}
