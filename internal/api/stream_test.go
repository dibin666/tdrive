package api

import "testing"

// The distinction that matters here is not the bounds — it is whether the
// caller asked for a range at all. A reader probes for range support by sending
// "bytes=0-" and reading the status code, so answering that with 200 tells it
// the server cannot seek, and a demuxer looking for a Matroska cue list at the
// end of the file falls back to streaming the whole thing from byte zero.
func TestParseRange(t *testing.T) {
	const size = 1000

	cases := []struct {
		name   string
		header string
		size   int64
		start  int64
		end    int64
		ranged bool
		ok     bool
	}{
		{name: "no header", header: "", size: size, end: 999, ok: true},
		{name: "whole file as a range", header: "bytes=0-", size: size, end: 999, ranged: true, ok: true},
		{name: "open ended", header: "bytes=500-", size: size, start: 500, end: 999, ranged: true, ok: true},
		{name: "closed", header: "bytes=10-19", size: size, start: 10, end: 19, ranged: true, ok: true},
		{name: "end past the file", header: "bytes=900-5000", size: size, start: 900, end: 999, ranged: true, ok: true},
		{name: "suffix", header: "bytes=-100", size: size, start: 900, end: 999, ranged: true, ok: true},
		{name: "suffix longer than the file", header: "bytes=-5000", size: size, end: 999, ranged: true, ok: true},
		{name: "first of several", header: "bytes=0-9,20-29", size: size, end: 9, ranged: true, ok: true},
		{name: "unknown unit is ignored", header: "items=0-9", size: size, end: 999, ok: true},
		{name: "empty file", header: "bytes=0-", size: 0, ok: true},
		{name: "start past the end", header: "bytes=1000-", size: size},
		{name: "reversed", header: "bytes=50-10", size: size},
		{name: "garbage", header: "bytes=abc", size: size},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start, end, ranged, ok := parseRange(tc.header, tc.size)
			if ok != tc.ok {
				t.Fatalf("parseRange(%q, %d) ok = %v, want %v", tc.header, tc.size, ok, tc.ok)
			}
			if !ok {
				return
			}
			if start != tc.start || end != tc.end {
				t.Fatalf("parseRange(%q, %d) = %d-%d, want %d-%d",
					tc.header, tc.size, start, end, tc.start, tc.end)
			}
			if ranged != tc.ranged {
				t.Fatalf("parseRange(%q, %d) ranged = %v, want %v", tc.header, tc.size, ranged, tc.ranged)
			}
		})
	}
}
