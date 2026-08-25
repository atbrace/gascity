package events

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Filter specifies predicates for ReadFiltered. Zero values are ignored.
type Filter struct {
	Type     string    // match events with this Type
	Actor    string    // match events with this Actor
	Subject  string    // match events with this Subject
	Since    time.Time // match events at or after this time
	Until    time.Time // match events at or before this time
	AfterSeq uint64    // match events with Seq > AfterSeq (0 = no filter)
	Limit    int       // cap results at this count (0 or negative = unlimited)
}

// matchesFilter reports whether e satisfies all non-zero predicates in f.
// It does not enforce Limit — that is applied by the caller.
func matchesFilter(e Event, f Filter) bool {
	if f.AfterSeq > 0 && e.Seq <= f.AfterSeq {
		return false
	}
	if f.Type != "" && e.Type != f.Type {
		return false
	}
	if f.Actor != "" && e.Actor != f.Actor {
		return false
	}
	if f.Subject != "" && e.Subject != f.Subject {
		return false
	}
	if !f.Since.IsZero() && e.Ts.Before(f.Since) {
		return false
	}
	if !f.Until.IsZero() && e.Ts.After(f.Until) {
		return false
	}
	return true
}

// ApplyFilter returns events matching all non-zero predicates in filter.
// It preserves input order and applies a positive Limit after matching.
func ApplyFilter(evts []Event, filter Filter) []Event {
	var result []Event
	for _, e := range evts {
		if !matchesFilter(e, filter) {
			continue
		}
		result = append(result, e)
		if limitReached(len(result), filter) {
			break
		}
	}
	return result
}

func limitReached(count int, filter Filter) bool {
	return filter.Limit > 0 && count >= filter.Limit
}

// ReadAll reads all events from the JSONL file at path, transparently
// walking sibling archives produced by rotation. Archives are read in
// seq order before the active file, yielding a single chronological
// stream. Returns (nil, nil) if neither the active file nor any
// archives exist.
func ReadAll(path string) ([]Event, error) {
	return ReadFiltered(path, Filter{})
}

// seqProbeBufSize bounds the read-ahead used when probing a byte offset for the
// line that begins there. Events average ~1.4 KB on a live log; 64 KB
// comfortably spans one line plus the partial line a probe may land inside.
const seqProbeBufSize = 64 * 1024

// seqLineAt probes byte offset off and returns the seq of the next parseable
// line, along with that line's start offset.
//
// Contract, which the binary search in activeScanStart depends on: for off > 0
// the line containing off is treated as a partial line and DISCARDED, so the
// result is the first parseable line beginning strictly after off. Only off == 0
// reads the line starting at off itself. Probing a known line start therefore
// returns the FOLLOWING line, not the line at that offset.
//
// start is always a true line boundary. Unparseable lines are skipped, matching
// the scanner's long-standing tolerance for malformed rows. ok is false when EOF
// is reached without finding a parseable line.
func seqLineAt(f *os.File, off int64) (seq uint64, start int64, ok bool) {
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return 0, 0, false
	}
	r := bufio.NewReaderSize(f, seqProbeBufSize)
	pos := off
	if off > 0 {
		partial, err := r.ReadBytes('\n')
		if err != nil {
			return 0, 0, false
		}
		pos += int64(len(partial))
	}
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			var probe struct {
				Seq uint64 `json:"seq"`
			}
			if json.Unmarshal(bytes.TrimRight(line, "\r\n"), &probe) == nil && probe.Seq > 0 {
				return probe.Seq, pos, true
			}
			pos += int64(len(line))
		}
		if err != nil {
			return 0, 0, false
		}
	}
}

// activeScanStart returns the byte offset in the active log at which a scan can
// begin without missing any event Filter.AfterSeq can match.
//
// The active log is append-only with strictly increasing seq (FileRecorder
// assigns e.Seq = r.seq++ under its mutex), so every line at or below the
// cursor sorts before every line above it and can be skipped wholesale. This is
// the seq-window skip archiveOverlapsFilter already applies to .gz archives,
// extended to the active file. Without it a cursored read re-parses the entire
// log on every call and AfterSeq saves nothing, because matchesFilter applies
// it only after each line has been unmarshalled -- the dominant cost of the
// supervisor's per-tick order-trigger check (gcy-ocb5).
//
// Returns 0 (full scan) whenever the boundary cannot be established, so a log
// that violates the ordering invariant degrades in speed, never in correctness.
func activeScanStart(f *os.File, size int64, afterSeq uint64) int64 {
	if afterSeq == 0 || size <= 0 {
		return 0
	}
	// Nothing to skip when the log's first line is already above the cursor.
	if first, _, ok := seqLineAt(f, 0); !ok || first > afterSeq {
		return 0
	}
	i := sort.Search(int(size), func(i int) bool {
		seq, _, ok := seqLineAt(f, int64(i))
		return !ok || seq > afterSeq
	})
	seq, start, ok := seqLineAt(f, int64(i))
	if !ok || seq <= afterSeq {
		// Cursor is at or beyond the newest event: skip the file entirely.
		return size
	}
	return start
}

// ReadFiltered reads events from path and sibling archives, returning
// only those matching all non-zero fields in filter. Archives whose
// seq window is fully excluded by the filter's AfterSeq predicate are
// skipped without gunzipping. Returns (nil, nil) if no events exist.
// Scanner errors return the events parsed before the error alongside
// the error.
func ReadFiltered(path string, filter Filter) ([]Event, error) {
	dir := filepath.Dir(path)
	archives, err := archiveFilesIn(dir)
	if err != nil {
		// Listing the dir failed (most often: dir doesn't exist).
		// Fall through to the active-file path; if that also fails,
		// the caller gets a single error.
		archives = nil
	}

	var result []Event
	for _, info := range archives {
		if !archiveOverlapsFilter(info, filter) {
			continue
		}
		archivePath := filepath.Join(dir, info.Basename)
		err := streamArchive(archivePath, filter, func(e Event) bool {
			if !matchesFilter(e, filter) {
				return true
			}
			result = append(result, e)
			return !limitReached(len(result), filter)
		})
		if err != nil {
			return result, fmt.Errorf("reading archive %q: %w", info.Basename, err)
		}
		if limitReached(len(result), filter) {
			return result, nil
		}
	}

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			if len(result) == 0 {
				return nil, nil
			}
			return result, nil
		}
		return result, fmt.Errorf("reading events: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only file

	// Skip the prefix of the active log the cursor already covers. seqLineAt
	// leaves the descriptor wherever its last probe ended, so seek explicitly
	// even when the scan starts at 0 (the uncursored case).
	scanFrom := int64(0)
	if st, statErr := f.Stat(); statErr == nil {
		scanFrom = activeScanStart(f, st.Size(), filter.AfterSeq)
	}
	if _, err := f.Seek(scanFrom, io.SeekStart); err != nil {
		return result, fmt.Errorf("seeking events: %w", err)
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024) // handle lines up to 1MB
	for scanner.Scan() {
		var e Event
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			continue // skip malformed lines
		}
		if !matchesFilter(e, filter) {
			continue
		}
		result = append(result, e)
		if limitReached(len(result), filter) {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return result, fmt.Errorf("scanning events: %w", err)
	}
	return result, nil
}

// ReadFilteredWithInFlight is ReadFiltered plus events still stranded in
// in-flight rotation files. When rotateLocked renames the active log to
// events.jsonl.rotating-<ts>-seq-<a>-<b>, a background goroutine gzips it into
// the canonical .gz archive and only then removes the rotating file. In that
// window the just-rotated events live ONLY in the plain-JSONL rotating file,
// which ReadFiltered (it lists only .gz archives) cannot see. The live run
// tailer folds these in when it detects a rotation, before resetting its
// active-file cursor, so events written to the old active log in the poll window
// before the rename are not lost during the asynchronous compression.
//
// Callers must be seq-idempotent: during the brief window when a canonical .gz
// and its source rotating file coexist, an event can appear in both. The result
// is de-duplicated by seq and returned in seq order. Intended for the AfterSeq
// catch-up path; a positive Filter.Limit bounds only ReadFiltered's own scan,
// not the merged in-flight events.
func ReadFilteredWithInFlight(path string, filter Filter) ([]Event, error) {
	base, baseErr := ReadFiltered(path, filter)
	inflight, inErr := readInFlightRotating(path, filter)
	if len(inflight) == 0 {
		if baseErr == nil {
			return base, inErr
		}
		return base, baseErr
	}
	merged := mergeEventsBySeq(base, inflight)
	if baseErr != nil {
		return merged, baseErr
	}
	return merged, inErr
}

// readInFlightRotating reads events matching filter from any in-flight rotation
// files (events.jsonl.rotating-<ts>-seq-<a>-<b>) beside path — the plain-JSONL
// renames of a just-rotated active log the background gzip has not yet promoted
// to a canonical .gz archive. Files whose seq window is fully excluded by
// filter.AfterSeq are skipped without opening. Results are in seq order across
// rotating files (sorted by FirstSeq; each file is internally seq ordered).
// Returns (nil, nil) when nothing is rotating — the overwhelmingly common case.
func readInFlightRotating(path string, filter Filter) ([]Event, error) {
	dir := filepath.Dir(path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	type rotatingFile struct {
		name     string
		firstSeq uint64
	}
	var files []rotatingFile
	for _, e := range entries {
		if e.IsDir() || !hasRotatingPrefix(e.Name()) {
			continue
		}
		_, first, last, ok := parseRotatingBasename(e.Name())
		if !ok {
			// Legacy rotating file without a seq window; the startup orphan
			// reaper promotes it — a live reader skips it rather than guess.
			continue
		}
		if filter.AfterSeq > 0 && last <= filter.AfterSeq {
			continue
		}
		files = append(files, rotatingFile{name: e.Name(), firstSeq: first})
	}
	if len(files) == 0 {
		return nil, nil
	}
	sort.Slice(files, func(i, j int) bool { return files[i].firstSeq < files[j].firstSeq })

	var result []Event
	for _, rf := range files {
		evts, err := readPlainJSONLFiltered(filepath.Join(dir, rf.name), filter)
		if err != nil {
			return result, fmt.Errorf("reading in-flight rotation %q: %w", rf.name, err)
		}
		result = append(result, evts...)
	}
	return result, nil
}

// readPlainJSONLFiltered reads every filter-matching event from a plain-JSONL
// events file, scanning the whole file from the start. Unlike ReadFrom it keeps
// no byte offset; unlike the active-file scan in ReadFiltered it does not honor
// Filter.Limit (its only caller merges the result under an AfterSeq filter).
func readPlainJSONLFiltered(path string, filter Filter) ([]Event, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only file

	var result []Event
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		var e Event
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			continue // skip malformed lines (partial write mid-rename)
		}
		if !matchesFilter(e, filter) {
			continue
		}
		result = append(result, e)
	}
	if err := scanner.Err(); err != nil {
		return result, fmt.Errorf("scanning: %w", err)
	}
	return result, nil
}

// mergeEventsBySeq merges two seq-ascending event slices into one seq-ascending
// slice, dropping exact seq duplicates — an event present in both a canonical
// archive and its not-yet-removed source rotating file. Event seqs are globally
// monotonic and unique, so equal seq means the same event.
func mergeEventsBySeq(a, b []Event) []Event {
	out := make([]Event, 0, len(a)+len(b))
	appendUnique := func(e Event) {
		if n := len(out); n > 0 && out[n-1].Seq == e.Seq {
			return
		}
		out = append(out, e)
	}
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if a[i].Seq <= b[j].Seq {
			appendUnique(a[i])
			i++
		} else {
			appendUnique(b[j])
			j++
		}
	}
	for ; i < len(a); i++ {
		appendUnique(a[i])
	}
	for ; j < len(b); j++ {
		appendUnique(b[j])
	}
	return out
}

// archiveFilesIn lists canonical events archives in dir, sorted by
// FirstSeq ascending so callers can read them in chronological order.
// Files that don't match the canonical name pattern (legacy archives,
// unrelated files) are silently skipped — a corrupt archive in the
// dir must not poison the read path.
func archiveFilesIn(dir string) ([]archiveInfo, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var archives []archiveInfo
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := parseArchiveBasename(e.Name())
		if err != nil {
			continue
		}
		archives = append(archives, info)
	}
	sort.Slice(archives, func(i, j int) bool {
		return archives[i].FirstSeq < archives[j].FirstSeq
	})
	return archives, nil
}

// streamArchive gunzip-streams the file at path, decoding each line
// as an Event and invoking fn for every event. fn returns false to
// abort iteration early. Returns nil if iteration completed cleanly
// or fn requested abort; errors from gzip / scanner are wrapped.
func streamArchive(path string, _ Filter, fn func(Event) bool) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // read-only file

	gr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("gunzip: %w", err)
	}
	defer gr.Close() //nolint:errcheck // read-only stream

	scanner := bufio.NewScanner(gr)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		var e Event
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			continue
		}
		if !fn(e) {
			return nil
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scanning archive: %w", err)
	}
	return nil
}

// ReadFilteredTail reads the trailing matching events from path. A positive
// limit returns at most that many events in chronological order; limit <= 0
// falls back to ReadFiltered.
func ReadFilteredTail(path string, filter Filter, limit int) ([]Event, error) {
	if limit <= 0 {
		return ReadFiltered(path, filter)
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading events tail: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only file

	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat events tail: %w", err)
	}
	return readFilteredTailFromFile(f, info.Size(), filter, limit)
}

func readFilteredTailFromFile(f *os.File, size int64, filter Filter, limit int) ([]Event, error) {
	if size <= 0 {
		return nil, nil
	}
	const chunkSize int64 = 64 * 1024
	var reversed []Event
	var pending []byte
	end := size
	for end > 0 && len(reversed) < limit {
		n := chunkSize
		if end < n {
			n = end
		}
		start := end - n
		chunk := make([]byte, n)
		if _, err := f.ReadAt(chunk, start); err != nil && err != io.EOF {
			return nil, fmt.Errorf("reading events tail: %w", err)
		}
		data := make([]byte, 0, len(chunk)+len(pending))
		data = append(data, chunk...)
		data = append(data, pending...)
		parts := bytes.Split(data, []byte{'\n'})
		firstComplete := 0
		if start > 0 {
			pending = append(pending[:0], parts[0]...)
			firstComplete = 1
		} else {
			pending = nil
		}
		for i := len(parts) - 1; i >= firstComplete && len(reversed) < limit; i-- {
			line := bytes.TrimSuffix(parts[i], []byte{'\r'})
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			var e Event
			if err := json.Unmarshal(line, &e); err != nil {
				continue
			}
			if matchesFilter(e, filter) {
				reversed = append(reversed, e)
			}
		}
		end = start
	}
	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}
	return reversed, nil
}

// ReadLatestSeq returns the highest complete event Seq visible in the
// active events file or any canonical sibling archive. Event logs are
// append-only and sequence numbers are monotonic, so the active file
// is read backward from the tail and archives contribute their
// filename-encoded LastSeq without being gunzipped.
func ReadLatestSeq(path string) (uint64, error) {
	seq, err := readLatestActiveSeq(path)
	if err != nil {
		return 0, err
	}
	archives, err := archiveFilesIn(filepath.Dir(path))
	if err == nil {
		for _, info := range archives {
			if info.LastSeq > seq {
				seq = info.LastSeq
			}
		}
	}
	return seq, nil
}

func readLatestActiveSeq(path string) (uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("reading latest seq: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only file

	info, err := f.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat events: %w", err)
	}
	return readLatestSeqFromTail(f, info.Size())
}

func readLatestSeqFromTail(f *os.File, size int64) (uint64, error) {
	if size <= 0 {
		return 0, nil
	}
	const chunkSize int64 = 64 * 1024
	var suffix []byte
	end := size
	first := true
	for end > 0 {
		n := chunkSize
		if end < n {
			n = end
		}
		start := end - n
		chunk := make([]byte, n)
		if _, err := f.ReadAt(chunk, start); err != nil && err != io.EOF {
			return 0, fmt.Errorf("reading latest seq: %w", err)
		}
		data := make([]byte, 0, len(chunk)+len(suffix))
		data = append(data, chunk...)
		data = append(data, suffix...)
		searchEnd := len(data)
		if first && len(data) > 0 && data[len(data)-1] != '\n' {
			idx := bytes.LastIndexByte(data, '\n')
			if idx < 0 {
				suffix = data
				end = start
				first = false
				continue
			}
			searchEnd = idx
		}
		searchStart := 0
		if start > 0 {
			idx := bytes.IndexByte(data, '\n')
			if idx < 0 {
				suffix = data
				end = start
				first = false
				continue
			}
			searchStart = idx + 1
		}
		if seq, ok := latestSeqInCompleteLines(data[searchStart:searchEnd]); ok {
			return seq, nil
		}
		suffix = data
		end = start
		first = false
	}
	return 0, nil
}

func latestSeqInCompleteLines(data []byte) (uint64, bool) {
	for len(data) > 0 {
		idx := bytes.LastIndexByte(data, '\n')
		var line []byte
		if idx >= 0 {
			line = data[idx+1:]
			data = data[:idx]
		} else {
			line = data
			data = nil
		}
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var header struct {
			Seq uint64 `json:"seq"`
		}
		if err := json.Unmarshal(line, &header); err == nil && header.Seq > 0 {
			return header.Seq, true
		}
	}
	return 0, false
}

// ReadFrom reads events starting at the given byte offset in the file.
// Returns the events read, the byte offset after the last complete line,
// and any error. Returns (nil, offset, nil) if no new data is available
// or the file doesn't exist yet. Skips malformed lines (partial writes).
func ReadFrom(path string, offset int64) ([]Event, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, offset, nil
		}
		return nil, offset, fmt.Errorf("reading events: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only file

	return readEventsFrom(f, offset)
}

// readEventsFrom scans events from an already-open active log starting at offset,
// returning the decoded events and the offset advanced past every complete line.
// A trailing partial line (no newline) does not advance the offset, so a later
// read re-reads it once the writer completes it. Reading from a caller-supplied
// fd (rather than re-opening by path) lets a tailer pin the file identity across
// a concurrent rotation.
func readEventsFrom(f *os.File, offset int64) ([]Event, int64, error) {
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, offset, fmt.Errorf("seeking events: %w", err)
	}

	var result []Event
	r := bufio.NewReader(f)
	bytesRead := int64(0)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			if line[len(line)-1] == '\n' {
				// Complete line — safe to advance offset past it.
				bytesRead += int64(len(line))
				trimmed := line[:len(line)-1]
				if len(trimmed) > 0 && trimmed[len(trimmed)-1] == '\r' {
					trimmed = trimmed[:len(trimmed)-1]
				}
				var e Event
				if jsonErr := json.Unmarshal(trimmed, &e); jsonErr == nil {
					result = append(result, e)
				}
				// skip malformed lines (partial writes)
			}
			// Partial line (no trailing \n): don't advance offset.
			// The next ReadFrom call will re-read it once complete.
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return result, offset + bytesRead, fmt.Errorf("scanning events: %w", err)
		}
	}
	return result, offset + bytesRead, nil
}
