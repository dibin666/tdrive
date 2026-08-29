// Package tagcodec encodes and decodes the tdrive record that lives in a
// Telegram message caption.
//
// The SQLite index is a cache; these captions are the durable record. Every
// directory is one text message and every file segment is one document message,
// so a full drive can be rebuilt by replaying the channel history through
// Decode. That is why the machine-readable tags are never abbreviated or
// dropped: only the cosmetic parts of a caption are allowed to shrink.
//
// Caption layout:
//
//	影片.mkv
//
//	#tdrive #v1 #file #id_01K2… #pid_01K2… #n_MFYGCYTB #seg_1_7 #sz_13421772800 #ss_1992294400 #电影 #_2024
//
// The leading display line is for humans reading the channel in a Telegram
// client. The authoritative name is #n_, base32 of the UTF-8 bytes, so names
// containing spaces, slashes or emoji survive intact.
package tagcodec

import (
	"encoding/base32"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// Marker identifies a caption as belonging to tdrive. Searching a channel for
// "#tdrive" is how the indexer finds its own messages among unrelated ones.
const Marker = "tdrive"

// Version is the caption schema version written by this build. Decode accepts
// any version it knows how to parse so that an older drive keeps working.
const Version = 1

// MaxCaptionUnits is Telegram's caption limit for non-premium accounts,
// measured the way Telegram measures it: UTF-16 code units, so an emoji outside
// the BMP costs two.
const MaxCaptionUnits = 1024

// MaxNameBytes bounds a single directory or file name. It keeps the
// non-negotiable part of a caption comfortably under MaxCaptionUnits and
// matches the usual filesystem limit, so names that round-trip here also
// round-trip through WebDAV clients.
const MaxNameBytes = 255

// MaxHumanTags caps how many ancestor folder tags are appended. Beyond a
// handful they stop helping anyone browsing the channel.
const MaxHumanTags = 5

// RootParent is the #pid_ value used by records that sit at the drive root.
const RootParent = "root"

// Kind distinguishes the two record types stored in a channel.
type Kind string

const (
	KindDir  Kind = "dir"
	KindFile Kind = "file"
)

var (
	// ErrNotTagged means the caption carries no #tdrive marker. The indexer
	// treats this as "not ours" rather than as corruption.
	ErrNotTagged = errors.New("tagcodec: caption is not a tdrive record")
	// ErrUnknownVersion means the caption was written by a newer build.
	ErrUnknownVersion = errors.New("tagcodec: unsupported caption version")
	// ErrMalformed means the marker was present but the record is unusable.
	ErrMalformed = errors.New("tagcodec: malformed tdrive record")
)

// nameEnc is RFC 4648 base32 without padding. Its alphabet (A-Z, 2-7) is
// entirely hashtag-safe, and dropping '=' matters because Telegram would end
// the hashtag there.
var nameEnc = base32.StdEncoding.WithPadding(base32.NoPadding)

// ulidRe matches a Crockford base32 ULID as produced by oklog/ulid.
var ulidRe = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)

// Record is one decoded caption. Dir records use ID, ParentID and Name; file
// records additionally carry the segment fields.
type Record struct {
	Kind     Kind
	Version  int
	ID       string
	ParentID string // "" at the drive root
	Name     string

	// SegIndex is 1-based; SegCount is 1 for files that fit in one Telegram
	// object. Both are always written, so segmented and unsegmented files
	// decode through the same path.
	SegIndex int
	SegCount int

	// TotalSize is the size of the whole logical file, not of this segment.
	// SegmentSize is the split size every segment but the last one uses.
	TotalSize   int64
	SegmentSize int64

	// HumanTags are the sanitized ancestor folder names, nearest first. They
	// exist so that searching a Telegram client for "#电影" surfaces the
	// folder's files; nothing in tdrive reads them back.
	HumanTags []string
}

// DirDisplay is the cosmetic first line of a directory caption.
func DirDisplay(path string) string { return "📁 " + path }

// EncodeDir renders a directory record. path is display-only; name is what
// gets round-tripped.
func EncodeDir(id, parentID, name, path string) (string, error) {
	if err := validateID(id); err != nil {
		return "", err
	}
	if parentID != "" {
		if err := validateID(parentID); err != nil {
			return "", fmt.Errorf("parent: %w", err)
		}
	}
	if err := validateName(name); err != nil {
		return "", err
	}

	tags := []string{
		"#" + Marker,
		fmt.Sprintf("#v%d", Version),
		"#" + string(KindDir),
		"#id_" + id,
		"#pid_" + orRoot(parentID),
		"#n_" + nameEnc.EncodeToString([]byte(name)),
	}
	return assemble(DirDisplay(path), tags, nil)
}

// EncodeFile renders one segment of a file. Every segment of a file repeats the
// same ID, Name, TotalSize and SegmentSize and differs only in SegIndex, which
// is what lets the indexer regroup them after an index loss.
func EncodeFile(r Record) (string, error) {
	if err := validateID(r.ID); err != nil {
		return "", err
	}
	if r.ParentID != "" {
		if err := validateID(r.ParentID); err != nil {
			return "", fmt.Errorf("parent: %w", err)
		}
	}
	if err := validateName(r.Name); err != nil {
		return "", err
	}
	if r.SegCount < 1 || r.SegIndex < 1 || r.SegIndex > r.SegCount {
		return "", fmt.Errorf("%w: segment %d of %d", ErrMalformed, r.SegIndex, r.SegCount)
	}
	if r.TotalSize < 0 || r.SegmentSize <= 0 {
		return "", fmt.Errorf("%w: size %d segment size %d", ErrMalformed, r.TotalSize, r.SegmentSize)
	}

	tags := []string{
		"#" + Marker,
		fmt.Sprintf("#v%d", Version),
		"#" + string(KindFile),
		"#id_" + r.ID,
		"#pid_" + orRoot(r.ParentID),
		"#n_" + nameEnc.EncodeToString([]byte(r.Name)),
		fmt.Sprintf("#seg_%d_%d", r.SegIndex, r.SegCount),
		fmt.Sprintf("#sz_%d", r.TotalSize),
		fmt.Sprintf("#ss_%d", r.SegmentSize),
	}

	human := make([]string, 0, MaxHumanTags)
	seen := map[string]bool{}
	for _, raw := range r.HumanTags {
		t := SanitizeTag(raw)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		human = append(human, "#"+t)
		if len(human) == MaxHumanTags {
			break
		}
	}
	return assemble(r.Name, tags, human)
}

// Decode parses a caption back into a Record. It returns ErrNotTagged for
// captions that were not written by tdrive, which the indexer skips silently.
func Decode(caption string) (Record, error) {
	var rec Record

	tokens, ok := tagTokens(caption)
	if !ok {
		return rec, ErrNotTagged
	}

	var (
		sawKind   bool
		encName   string
		sawVer    bool
		sawSeg    bool
		sawSize   bool
		sawSegSz  bool
		humanTags []string
	)

	for _, tok := range tokens {
		switch {
		case tok == Marker:
			// The marker itself carries no data.
		case tok == string(KindDir) || tok == string(KindFile):
			rec.Kind, sawKind = Kind(tok), true
		case len(tok) > 1 && tok[0] == 'v' && isDigits(tok[1:]):
			n, err := strconv.Atoi(tok[1:])
			if err != nil {
				return rec, fmt.Errorf("%w: version %q", ErrMalformed, tok)
			}
			rec.Version, sawVer = n, true
		case strings.HasPrefix(tok, "id_"):
			rec.ID = strings.TrimPrefix(tok, "id_")
		case strings.HasPrefix(tok, "pid_"):
			if p := strings.TrimPrefix(tok, "pid_"); p != RootParent {
				rec.ParentID = p
			}
		case strings.HasPrefix(tok, "n_"):
			encName = strings.TrimPrefix(tok, "n_")
		case strings.HasPrefix(tok, "seg_"):
			i, n, err := parseSeg(strings.TrimPrefix(tok, "seg_"))
			if err != nil {
				return rec, err
			}
			rec.SegIndex, rec.SegCount, sawSeg = i, n, true
		case strings.HasPrefix(tok, "sz_"):
			n, err := strconv.ParseInt(strings.TrimPrefix(tok, "sz_"), 10, 64)
			if err != nil {
				return rec, fmt.Errorf("%w: size %q", ErrMalformed, tok)
			}
			rec.TotalSize, sawSize = n, true
		case strings.HasPrefix(tok, "ss_"):
			n, err := strconv.ParseInt(strings.TrimPrefix(tok, "ss_"), 10, 64)
			if err != nil {
				return rec, fmt.Errorf("%w: segment size %q", ErrMalformed, tok)
			}
			rec.SegmentSize, sawSegSz = n, true
		default:
			humanTags = append(humanTags, tok)
		}
	}

	if !sawVer {
		return rec, fmt.Errorf("%w: missing version", ErrMalformed)
	}
	if rec.Version > Version {
		return rec, fmt.Errorf("%w: v%d", ErrUnknownVersion, rec.Version)
	}
	if !sawKind {
		return rec, fmt.Errorf("%w: missing record kind", ErrMalformed)
	}
	if err := validateID(rec.ID); err != nil {
		return rec, err
	}
	if rec.ParentID != "" {
		if err := validateID(rec.ParentID); err != nil {
			return rec, fmt.Errorf("parent: %w", err)
		}
	}

	// #n_ is authoritative, but a caption hand-edited in a Telegram client can
	// lose it. Falling back to the display line keeps such a record usable
	// instead of dropping the file out of the drive entirely.
	if encName != "" {
		raw, err := nameEnc.DecodeString(encName)
		if err != nil {
			return rec, fmt.Errorf("%w: name %q: %v", ErrMalformed, encName, err)
		}
		rec.Name = string(raw)
	} else {
		rec.Name = fallbackName(caption, rec.Kind)
	}
	if rec.Name == "" {
		return rec, fmt.Errorf("%w: empty name", ErrMalformed)
	}

	if rec.Kind == KindFile {
		if !sawSeg || !sawSize || !sawSegSz {
			return rec, fmt.Errorf("%w: file record missing segment metadata", ErrMalformed)
		}
		if rec.SegmentSize <= 0 || rec.TotalSize < 0 {
			return rec, fmt.Errorf("%w: file record has non-positive sizes", ErrMalformed)
		}
		rec.HumanTags = humanTags
	}
	return rec, nil
}

// SanitizeTag turns a folder name into something Telegram will linkify. It
// keeps Unicode letters, digits and underscores (so Chinese folder names stay
// readable as tags) and drops everything else. An all-digit result gets an
// underscore prefix because some clients refuse to linkify a bare number.
func SanitizeTag(name string) string {
	var b strings.Builder
	digitsOnly := true
	for _, r := range name {
		switch {
		case unicode.IsLetter(r):
			digitsOnly = false
			b.WriteRune(r)
		case unicode.IsDigit(r):
			b.WriteRune(r)
		case r == '_':
			digitsOnly = false
			b.WriteRune('_')
		}
	}
	out := b.String()
	if out == "" {
		return ""
	}
	if digitsOnly {
		return "_" + out
	}
	return out
}

// assemble joins the display line with the tag block, shrinking only the parts
// that are safe to shrink. Human tags go first (farthest ancestor is last in
// the slice, so trimming from the end keeps the most specific folder), then the
// display line. Machine tags are never touched.
func assemble(display string, machine, human []string) (string, error) {
	base := strings.Join(machine, " ")
	if utf16Len(base) > MaxCaptionUnits {
		// Only reachable if MaxNameBytes was bypassed; refuse rather than
		// write a caption that cannot be decoded.
		return "", fmt.Errorf("%w: machine tags exceed caption limit", ErrMalformed)
	}

	for {
		tags := base
		if len(human) > 0 {
			tags += " " + strings.Join(human, " ")
		}
		body := display + "\n\n" + tags
		if utf16Len(body) <= MaxCaptionUnits {
			return body, nil
		}
		if len(human) > 0 {
			human = human[:len(human)-1]
			continue
		}
		budget := MaxCaptionUnits - utf16Len(tags) - 2
		if budget <= 0 {
			return tags, nil
		}
		display = truncateUTF16(display, budget)
	}
}

// tagTokens locates the tag block and returns the tokens with '#' stripped.
// The block starts at the first line whose first token is "#tdrive", so a file
// named "#tdrive" on the display line cannot be confused for it.
func tagTokens(caption string) ([]string, bool) {
	lines := strings.Split(caption, "\n")
	start := -1
	for i, line := range lines {
		if f := strings.Fields(line); len(f) > 0 && f[0] == "#"+Marker {
			start = i
			break
		}
	}
	if start < 0 {
		return nil, false
	}

	var tokens []string
	for _, line := range lines[start:] {
		for _, f := range strings.Fields(line) {
			if strings.HasPrefix(f, "#") && len(f) > 1 {
				tokens = append(tokens, f[1:])
			}
		}
	}
	return tokens, len(tokens) > 0
}

// fallbackName recovers a name from the display line when #n_ is missing. Dir
// captions show a full path, so only the last element is the name.
func fallbackName(caption string, kind Kind) string {
	line, _, _ := strings.Cut(caption, "\n")
	line = strings.TrimSpace(line)
	if kind == KindDir {
		line = strings.TrimSpace(strings.TrimPrefix(line, "📁"))
		if i := strings.LastIndex(line, "/"); i >= 0 {
			line = line[i+1:]
		}
	}
	if strings.HasPrefix(line, "#") {
		return ""
	}
	return line
}

func parseSeg(s string) (idx, count int, err error) {
	a, b, ok := strings.Cut(s, "_")
	if !ok {
		return 0, 0, fmt.Errorf("%w: segment %q", ErrMalformed, s)
	}
	if idx, err = strconv.Atoi(a); err != nil {
		return 0, 0, fmt.Errorf("%w: segment index %q", ErrMalformed, a)
	}
	if count, err = strconv.Atoi(b); err != nil {
		return 0, 0, fmt.Errorf("%w: segment count %q", ErrMalformed, b)
	}
	if count < 1 || idx < 1 || idx > count {
		return 0, 0, fmt.Errorf("%w: segment %d of %d", ErrMalformed, idx, count)
	}
	return idx, count, nil
}

func validateID(id string) error {
	if !ulidRe.MatchString(id) {
		return fmt.Errorf("%w: bad id %q", ErrMalformed, id)
	}
	return nil
}

func validateName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("%w: empty name", ErrMalformed)
	case len(name) > MaxNameBytes:
		return fmt.Errorf("%w: name is %d bytes, limit is %d", ErrMalformed, len(name), MaxNameBytes)
	case strings.ContainsAny(name, "\n\r/"):
		return fmt.Errorf("%w: name %q contains a separator", ErrMalformed, name)
	}
	return nil
}

func orRoot(id string) string {
	if id == "" {
		return RootParent
	}
	return id
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// utf16Len counts the caption the way Telegram does.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n++
		if r > 0xFFFF {
			n++
		}
	}
	return n
}

// truncateUTF16 cuts s to at most limit UTF-16 units without splitting a
// surrogate pair or a rune.
func truncateUTF16(s string, limit int) string {
	if utf16Len(s) <= limit {
		return s
	}
	n := 0
	for i, r := range s {
		w := 1
		if r > 0xFFFF {
			w = 2
		}
		if n+w > limit {
			return s[:i]
		}
		n += w
	}
	return s
}

// EncodedNameOf exposes the base32 form used in #n_ so the indexer can build a
// Telegram search query for an exact name.
func EncodedNameOf(name string) string { return nameEnc.EncodeToString([]byte(name)) }
