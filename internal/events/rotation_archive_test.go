package events

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestArchiveFilenameRoundTrip(t *testing.T) {
	tests := []struct {
		name     string
		ts       time.Time
		first    uint64
		last     uint64
		wantBase string
	}{
		{
			name:     "midnight UTC",
			ts:       time.Date(2026, 5, 7, 0, 0, 0, 0, time.UTC),
			first:    1,
			last:     100,
			wantBase: "events.jsonl.archive-20260507T000000Z-seq-1-100.gz",
		},
		{
			name:     "afternoon UTC",
			ts:       time.Date(2026, 5, 7, 18, 30, 45, 0, time.UTC),
			first:    1234,
			last:     5678,
			wantBase: "events.jsonl.archive-20260507T183045Z-seq-1234-5678.gz",
		},
		{
			name:     "non-UTC zone is normalized to UTC",
			ts:       time.Date(2026, 5, 7, 14, 0, 0, 0, time.FixedZone("EST", -5*3600)),
			first:    1,
			last:     2,
			wantBase: "events.jsonl.archive-20260507T190000Z-seq-1-2.gz",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := formatArchiveBasename(tc.ts, tc.first, tc.last)
			if got != tc.wantBase {
				t.Errorf("formatArchiveBasename(%v,%d,%d) = %q, want %q",
					tc.ts, tc.first, tc.last, got, tc.wantBase)
			}
			info, err := parseArchiveBasename(tc.wantBase)
			if err != nil {
				t.Fatalf("parseArchiveBasename(%q): %v", tc.wantBase, err)
			}
			if !info.Timestamp.Equal(tc.ts.UTC()) {
				t.Errorf("Timestamp = %v, want %v", info.Timestamp, tc.ts.UTC())
			}
			if info.FirstSeq != tc.first {
				t.Errorf("FirstSeq = %d, want %d", info.FirstSeq, tc.first)
			}
			if info.LastSeq != tc.last {
				t.Errorf("LastSeq = %d, want %d", info.LastSeq, tc.last)
			}
		})
	}
}

func TestParseArchiveBasenameRejectsBadInput(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"not an archive", "events.jsonl"},
		{"legacy archive (no seq)", "events.jsonl.archive-20260416.gz"},
		{"missing .gz", "events.jsonl.archive-20260507T000000Z-seq-1-100"},
		{"non-numeric first", "events.jsonl.archive-20260507T000000Z-seq-foo-100.gz"},
		{"non-numeric last", "events.jsonl.archive-20260507T000000Z-seq-1-bar.gz"},
		{"missing seq segment", "events.jsonl.archive-20260507T000000Z.gz"},
		{"unrelated file", "snapshot-20260507.tar.gz"},
		{"first > last", "events.jsonl.archive-20260507T000000Z-seq-200-100.gz"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseArchiveBasename(tc.in); err == nil {
				t.Errorf("parseArchiveBasename(%q): expected error, got nil", tc.in)
			}
		})
	}
}

func TestIsLegacyArchive(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"events.jsonl.archive-20260416.gz", true},
		{"events.jsonl.archive-20260507T000000Z-seq-1-100.gz", false},
		{"events.jsonl", false},
		{"events.jsonl.archive-20260416.tar.gz", false},
		{"events.jsonl.archive-bogus.gz", false},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got := isLegacyArchiveBasename(tc.in)
			if got != tc.want {
				t.Errorf("isLegacyArchiveBasename(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestArchiveOverlapsFilter(t *testing.T) {
	info := archiveInfo{
		Basename:  "events.jsonl.archive-20260507T000000Z-seq-100-200.gz",
		Timestamp: time.Date(2026, 5, 7, 0, 0, 0, 0, time.UTC),
		FirstSeq:  100,
		LastSeq:   200,
	}
	tests := []struct {
		name string
		f    Filter
		want bool
	}{
		{"empty filter overlaps everything", Filter{}, true},
		{"AfterSeq below archive range", Filter{AfterSeq: 50}, true},
		{"AfterSeq inside archive range", Filter{AfterSeq: 150}, true},
		{"AfterSeq at archive last seq", Filter{AfterSeq: 200}, false},
		{"AfterSeq above archive range", Filter{AfterSeq: 250}, false},
		{"BeforeSeq above archive range", Filter{BeforeSeq: 250}, true},
		{"BeforeSeq inside archive range", Filter{BeforeSeq: 150}, true},
		{"BeforeSeq just above archive first seq", Filter{BeforeSeq: 101}, true},
		{"BeforeSeq at archive first seq", Filter{BeforeSeq: 100}, false},
		{"BeforeSeq below archive range", Filter{BeforeSeq: 50}, false},
		{"AfterSeq and BeforeSeq window inside range", Filter{AfterSeq: 120, BeforeSeq: 180}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := archiveOverlapsFilter(info, tc.f)
			if got != tc.want {
				t.Errorf("overlap = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestArchiveOverlapsFilterSkipsOnSince pins the TIME gate.
//
// An archive's Timestamp is stamped at rotation (recorder.go rotateLocked:
// ts = time.Now().UTC()), so every event inside it was appended strictly
// before that moment. A Since later than the stamp therefore cannot match
// anything in the archive and it must be skipped WITHOUT gunzipping.
//
// Why this test exists: the gate used to be seq-only, so a time-filtered
// request — `since=5m`, which is what every watcher sends — could never skip
// anything and re-parsed every archive in full on every call.
func TestArchiveOverlapsFilterSkipsOnSince(t *testing.T) {
	stamp := time.Date(2026, 5, 7, 0, 0, 0, 0, time.UTC)
	info := archiveInfo{
		Basename:  "events.jsonl.archive-20260507T000000Z-seq-100-200.gz",
		Timestamp: stamp,
		FirstSeq:  100,
		LastSeq:   200,
	}
	tests := []struct {
		name string
		f    Filter
		want bool
	}{
		{"Since after archive stamp skips", Filter{Since: stamp.Add(time.Hour)}, false},
		{"Since before archive stamp reads", Filter{Since: stamp.Add(-time.Hour)}, true},
		// Boundary stays inclusive: an event stamped exactly at rotation time
		// satisfies `e.Ts.Before(Since) == false`, so it would match.
		{"Since exactly at stamp reads", Filter{Since: stamp}, true},
		// The basename encodes no LOWER time bound, so Until can never prove an
		// archive irrelevant. Not gating on it is deliberate, not an oversight.
		{"Until before archive does not skip", Filter{Until: stamp.Add(-time.Hour)}, true},
		{"Since skip applies even when the seq range overlaps", Filter{AfterSeq: 100, Since: stamp.Add(time.Hour)}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := archiveOverlapsFilter(info, tc.f)
			if got != tc.want {
				t.Errorf("overlap = %v, want %v", got, tc.want)
			}
		})
	}
}

// writeSeqArchive writes a gzipped archive holding events seq 1..n, one second
// apart starting at base, and returns its path.
func writeSeqArchive(t *testing.T, dir string, n int, base time.Time) string {
	t.Helper()

	var body strings.Builder
	for seq := 1; seq <= n; seq++ {
		ts := base.Add(time.Duration(seq) * time.Second).UTC().Format(time.RFC3339Nano)
		fmt.Fprintf(&body, `{"seq":%d,"type":"t","ts":%q,"actor":"a"}`+"\n", seq, ts)
	}
	path := filepath.Join(dir, formatArchiveBasename(base.Add(time.Duration(n+1)*time.Second), 1, uint64(n)))
	writeGzipFile(t, path, body.String())
	return path
}

// TestStreamArchiveHonorsFilter pins that streamArchive USES its Filter
// argument. It used to be declared as `_ Filter` and discarded outright, so
// every call decoded the whole archive even when the caller had already
// bounded the window it wanted.
//
// The early exit is keyed on SEQ only. The log is append-only and seq-ordered
// by construction, so "every later line has a higher seq" is guaranteed;
// wall-clock ordering is not (a clock adjustment could invert two adjacent
// timestamps), and exiting early on Until would silently drop events.
func TestStreamArchiveHonorsFilter(t *testing.T) {
	base := time.Date(2026, 5, 7, 0, 0, 0, 0, time.UTC)
	path := writeSeqArchive(t, t.TempDir(), 5, base)

	collect := func(f Filter) []uint64 {
		t.Helper()
		var seen []uint64
		if err := streamArchive(path, f, func(e Event) bool {
			seen = append(seen, e.Seq)
			return true
		}); err != nil {
			t.Fatalf("streamArchive: %v", err)
		}
		return seen
	}

	t.Run("stops at BeforeSeq instead of decoding the tail", func(t *testing.T) {
		got := collect(Filter{BeforeSeq: 3})
		want := []uint64{1, 2}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("decoded %v, want %v — the tail past BeforeSeq must not be decoded", got, want)
		}
	})

	t.Run("empty filter still streams everything", func(t *testing.T) {
		got := collect(Filter{})
		want := []uint64{1, 2, 3, 4, 5}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("decoded %v, want %v", got, want)
		}
	})

	t.Run("Until does not truncate the stream", func(t *testing.T) {
		got := collect(Filter{Until: base.Add(2 * time.Second)})
		want := []uint64{1, 2, 3, 4, 5}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("decoded %v, want %v — Until must not drive the early exit", got, want)
		}
	})

	t.Run("fn abort still wins", func(t *testing.T) {
		var seen []uint64
		if err := streamArchive(path, Filter{}, func(e Event) bool {
			seen = append(seen, e.Seq)
			return e.Seq < 2
		}); err != nil {
			t.Fatalf("streamArchive: %v", err)
		}
		if fmt.Sprint(seen) != fmt.Sprint([]uint64{1, 2}) {
			t.Errorf("decoded %v, want [1 2]", seen)
		}
	})
}
