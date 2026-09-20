// Package library implements the editable sound-effect library of the
// soundradar P1 milestone.
//
// A library is a single ZIP container ("library.srz") with this layout:
//
//	manifest.json                  {"schema":1,"name":...,"feature":{...},"items":["<id>",...]}
//	items/<id>/meta.json           item metadata (camelCase JSON)
//	items/<id>/icon.png            normalised 128x128 PNG
//	items/<id>/samples/0001.wav    canonical 48 kHz / 16-bit PCM / mono WAV
//
// Everything (manifest, metadata, icons and sample bytes) is held in memory by
// a Store and flushed back with a single atomic replace, so a failed write can
// never leave a half-written library behind.
//
// Only the standard library is used.
package library

import (
	"archive/zip"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Schema is the manifest schema version written by this package.
const Schema = 1

// Canonical audio format of every sample stored in a library.
const (
	SampleRate    = 48000
	Channels      = 1
	BitsPerSample = 16
)

// File names inside the container.
const (
	ManifestName = "manifest.json"
	MetaName     = "meta.json"
	IconName     = "icon.png"
	SamplesDir   = "samples"
	HintsDir     = "hints"
	DefaultName  = "library.srz"
)

// Feature is the placeholder for the P2 acoustic fingerprint configuration.
// P1 only defines it and writes the defaults; nothing computes it yet.
type Feature struct {
	Kind        string  `json:"kind"`        // algorithm identifier, e.g. "mel-goertzel-v1"
	SampleRate  int     `json:"sampleRate"`  // analysis rate in Hz (== stored rate)
	FrameSize   int     `json:"frameSize"`   // STFT/analysis window length in samples
	HopSize     int     `json:"hopSize"`     // window step in samples
	Window      string  `json:"window"`      // window function name
	MelBands    int     `json:"melBands"`    // number of mel filters
	FMin        float64 `json:"fMinHz"`      // lowest mel band edge in Hz
	FMax        float64 `json:"fMaxHz"`      // highest mel band edge in Hz
	PeaksPerSec int     `json:"peaksPerSec"` // target landmark density
}

// DefaultFeature returns the default (unused-for-now) fingerprint parameters.
func DefaultFeature() Feature {
	return Feature{
		Kind:        "mel-goertzel-v1",
		SampleRate:  SampleRate,
		FrameSize:   2048,
		HopSize:     512,
		Window:      "hann",
		MelBands:    64,
		FMin:        40,
		FMax:        16000,
		PeaksPerSec: 30,
	}
}

// Manifest is the top level document of library.srz.
type Manifest struct {
	Schema    int       `json:"schema"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"createdAt"`
	Feature   Feature   `json:"feature"`
	Items     []string  `json:"items"` // item ids, in display order
}

// SourceFormat records the format of the file the user actually uploaded, so a
// stored (resampled) sample can always be traced back to its origin.
type SourceFormat struct {
	FileName      string  `json:"fileName,omitempty"`
	Container     string  `json:"container"`           // "wav" or "mp3"
	FormatTag     string  `json:"formatTag,omitempty"` // e.g. "PCM", "IEEE float", "MPEG-1 Layer III"
	SampleRate    int     `json:"sampleRate"`
	Channels      int     `json:"channels"`
	BitsPerSample int     `json:"bitsPerSample,omitempty"`
	DurationS     float64 `json:"durationS"`
	Frames        int64   `json:"frames,omitempty"`
	Bytes         int64   `json:"bytes,omitempty"`
	Resampled     bool    `json:"resampled"` // true when sampleRate != 48000
	Downmixed     bool    `json:"downmixed"` // true when channels != 1
}

// Sample is one audio variant of an item ("near"/"far"/"through wall").
type Sample struct {
	File        string       `json:"file"` // path inside the container
	OffsetS     float64      `json:"offsetS"`
	LenS        float64      `json:"lenS"`
	GainDb      float64      `json:"gainDb"`
	Source      string       `json:"source"` // "upload" | "generated"
	AddedAt     time.Time    `json:"addedAt"`
	Origin      SourceFormat `json:"origin"` // what the uploader gave us
	StoredRate  int          `json:"storedSampleRate"`
	StoredChans int          `json:"storedChannels"`
	StoredBits  int          `json:"storedBitsPerSample"`
	Frames      int64        `json:"frames"`
}

// DisplayHint maps one inventory grid size to a possible item name for a
// shared-sound ("同音类") library entry. It is display-only metadata: it is not
// part of the acoustic fingerprint, so editing hints does not require an index
// rebuild.
type DisplayHint struct {
	Grid string `json:"grid"`           // "3x2" (width x height in stash cells)
	Name string `json:"name"`           // possible item for that grid, e.g. "天命泥板"
	Icon string `json:"icon,omitempty"` // optional path inside item, e.g. "hints/00.png"
}

// MaxDisplayHints caps how many grid→name rows one item may carry.
const MaxDisplayHints = 16

// gridHintRE matches WxH cell sizes such as "3x2" or "2×2" (normalized to "x").
var gridHintRE = regexp.MustCompile(`(?i)^\d{1,2}x\d{1,2}$`)

// NormalizeDisplayHints trims, validates, and returns a clean copy of hints.
// An empty/nil input yields an empty slice (never nil) with no error.
func NormalizeDisplayHints(in []DisplayHint) ([]DisplayHint, error) {
	if len(in) == 0 {
		return []DisplayHint{}, nil
	}
	if len(in) > MaxDisplayHints {
		return nil, fmt.Errorf("displayHints 最多 %d 条", MaxDisplayHints)
	}
	out := make([]DisplayHint, 0, len(in))
	for i, h := range in {
		grid := strings.ToLower(strings.TrimSpace(h.Grid))
		grid = strings.ReplaceAll(grid, "×", "x")
		grid = strings.ReplaceAll(grid, "Ｘ", "x")
		grid = strings.ReplaceAll(grid, "ｘ", "x")
		name := strings.TrimSpace(h.Name)
		if grid == "" && name == "" {
			continue
		}
		if !gridHintRE.MatchString(grid) {
			return nil, fmt.Errorf("displayHints[%d].grid 须为 WxH（如 3x2）", i)
		}
		if name == "" {
			return nil, fmt.Errorf("displayHints[%d].name 不能为空", i)
		}
		out = append(out, DisplayHint{Grid: grid, Name: name})
	}
	return out, nil
}

// FormatHitLabel builds the single-line hit text shown in the live panel and
// overlay: class name alone when hints are empty, otherwise
// "类名｜3×2 天命泥板 / 2×2 理想国". maxRunes truncates with an ellipsis when >0.
func FormatHitLabel(className string, hints []DisplayHint, maxRunes int) string {
	className = strings.TrimSpace(className)
	if len(hints) == 0 {
		return className
	}
	parts := make([]string, 0, len(hints))
	for _, h := range hints {
		g := strings.ReplaceAll(h.Grid, "x", "×")
		parts = append(parts, g+" "+h.Name)
	}
	line := className + "｜" + strings.Join(parts, " / ")
	if maxRunes > 0 {
		runes := []rune(line)
		if len(runes) > maxRunes {
			if maxRunes <= 1 {
				return "…"
			}
			return string(runes[:maxRunes-1]) + "…"
		}
	}
	return line
}

// Item is the meta.json document plus the raw blobs held in memory.
type Item struct {
	ID           string        `json:"id"`
	Name         string        `json:"name"`
	Icon         string        `json:"icon"`
	Threshold    float64       `json:"threshold"`
	CooldownMs   int           `json:"cooldownMs"`
	Profile      string        `json:"profile"`
	Tags         []string      `json:"tags"`
	Note         string        `json:"note"`
	DisplayHints []DisplayHint `json:"displayHints,omitempty"`
	Samples      []Sample      `json:"samples"`
	CreatedAt    time.Time     `json:"createdAt"`
	UpdatedAt    time.Time     `json:"updatedAt"`

	// iconPNG and sampleWAV hold the container payloads; they are never part of
	// meta.json (json:"-"). hintPNG is parallel to DisplayHints (index i → hints/i.png).
	iconPNG   []byte
	sampleWAV [][]byte
	hintPNG   [][]byte
}

// IconPNG returns the stored icon bytes.
func (it *Item) IconPNG() []byte { return it.iconPNG }

// HintIcon returns the PNG for displayHints[i], or nil.
func (it *Item) HintIcon(i int) []byte {
	if i < 0 || i >= len(it.hintPNG) {
		return nil
	}
	return it.hintPNG[i]
}

// SampleWAV returns the stored canonical WAV bytes of sample index i.
func (it *Item) SampleWAV(i int) []byte {
	if i < 0 || i >= len(it.sampleWAV) {
		return nil
	}
	return it.sampleWAV[i]
}

// blob returns the container payload for a metadata sample path, so a save can
// rebuild the archive without re-reading the old one.
func (it *Item) blob(file string) []byte {
	if file == IconName {
		return it.iconPNG
	}
	for i := range it.Samples {
		if it.Samples[i].File == file {
			return it.SampleWAV(i)
		}
	}
	return nil
}

// Warmup/consistency report produced while loading.
type Warning struct {
	Item    string `json:"item,omitempty"`
	Message string `json:"message"`
}

// Store is an open library: the parsed manifest plus every item in memory.
type Store struct {
	path string
	mf   Manifest
	// order keeps manifest.Items authoritative but tolerant of a manifest that
	// is missing/partial, so a hand-edited archive still opens.
	items    map[string]*Item
	order    []string
	warnings []Warning
	mu       sync.RWMutex
}

// ---------------------------------------------------------------------------
// paths
// ---------------------------------------------------------------------------

// DefaultPath returns the library path used when --library is not given.
//
// Rule: <directory of the running executable>/data/library.srz when that
// directory is writable; otherwise <current working directory>/data/library.srz.
// Resolution happens once per process, at startup, so `serve` always reports
// the same absolute path it uses.
func DefaultPath() (string, error) {
	exe, err := os.Executable()
	if err == nil {
		dir := filepath.Dir(exe)
		if isWritableDir(dir) {
			return filepath.Join(dir, "data", DefaultName), nil
		}
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("无法确定库文件路径: %w", err)
	}
	return filepath.Join(wd, "data", DefaultName), nil
}

// isWritableDir reports whether dir exists (or can be created) and accepts a
// temporary file.
func isWritableDir(dir string) bool {
	if dir == "" {
		return false
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return false
	}
	f, err := os.CreateTemp(dir, ".srzprobe-*")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close()
	os.Remove(name)
	return true
}

// ---------------------------------------------------------------------------
// open / new
// ---------------------------------------------------------------------------

// New creates an in-memory, empty library that will be written to path on the
// next Save. The file itself is not touched until then.
func New(path, name string) *Store {
	if strings.TrimSpace(name) == "" {
		name = "未命名音效库"
	}
	return &Store{
		path:  path,
		items: make(map[string]*Item),
		mf: Manifest{
			Schema:    Schema,
			Name:      name,
			CreatedAt: time.Now().UTC(),
			Feature:   DefaultFeature(),
			Items:     []string{},
		},
	}
}

// Open loads path; when the file does not exist an empty library is returned
// (and created on disk, so the user immediately sees a real file).
func Open(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("库文件路径为空")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(abs)
	if errors.Is(err, os.ErrNotExist) {
		s := New(abs, "未命名音效库")
		if err := s.Save(); err != nil {
			return nil, fmt.Errorf("创建库文件 %s: %w", abs, err)
		}
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	zr, err := zip.NewReader(f, st.Size())
	if err != nil {
		return nil, fmt.Errorf("%s 不是有效的 ZIP/SRZ 库文件: %w", abs, err)
	}

	s := &Store{path: abs, items: make(map[string]*Item)}

	// manifest.json
	if rc, err := openZipEntry(zr, ManifestName); err == nil {
		raw, rerr := io.ReadAll(rc)
		rc.Close()
		if rerr != nil {
			return nil, fmt.Errorf("读取 %s: %w", ManifestName, rerr)
		}
		if err := json.Unmarshal(raw, &s.mf); err != nil {
			return nil, fmt.Errorf("解析 %s: %w", ManifestName, err)
		}
	} else {
		s.warnings = append(s.warnings, Warning{Message: "库里缺少 manifest.json，已按条目目录重建"})
	}
	if s.mf.Schema == 0 {
		s.mf.Schema = Schema
	}
	if s.mf.Feature.Kind == "" {
		s.mf.Feature = DefaultFeature()
	}
	if s.mf.CreatedAt.IsZero() {
		s.mf.CreatedAt = time.Now().UTC()
	}

	// group entries by item id
	type entry struct {
		meta  []byte
		icon  []byte
		order []string
		wavs  map[string][]byte
		hints map[string][]byte
	}
	groups := make(map[string]*entry)
	seen := make(map[string]bool)
	for _, zf := range zr.File {
		name := strings.ReplaceAll(zf.Name, "\\", "/")
		if !strings.HasPrefix(name, "items/") {
			continue
		}
		rest := name[len("items/"):]
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) != 2 || parts[0] == "" {
			continue
		}
		id, rel := parts[0], parts[1]
		if !seen[id] {
			seen[id] = true
			groups[id] = &entry{wavs: make(map[string][]byte), hints: make(map[string][]byte)}
		}
		g := groups[id]

		switch {
		case rel == MetaName:
			b, err := readZipEntry(zf)
			if err != nil {
				s.warnings = append(s.warnings, Warning{Item: id, Message: err.Error()})
				continue
			}
			g.meta = b
		case rel == IconName:
			b, err := readZipEntry(zf)
			if err != nil {
				s.warnings = append(s.warnings, Warning{Item: id, Message: err.Error()})
				continue
			}
			g.icon = b
		case strings.HasPrefix(rel, SamplesDir+"/") && strings.HasSuffix(strings.ToLower(rel), ".wav"):
			b, err := readZipEntry(zf)
			if err != nil {
				s.warnings = append(s.warnings, Warning{Item: id, Message: err.Error()})
				continue
			}
			g.wavs[rel] = b
			g.order = append(g.order, rel)
		case strings.HasPrefix(rel, HintsDir+"/") && strings.HasSuffix(strings.ToLower(rel), ".png"):
			b, err := readZipEntry(zf)
			if err != nil {
				s.warnings = append(s.warnings, Warning{Item: id, Message: err.Error()})
				continue
			}
			g.hints[rel] = b
		}
	}

	ids := make([]string, 0, len(groups))
	for id := range groups {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, id := range ids {
		g := groups[id]
		if g.meta == nil {
			s.warnings = append(s.warnings, Warning{Item: id, Message: "缺少 meta.json，条目已跳过"})
			continue
		}
		var it Item
		if err := json.Unmarshal(g.meta, &it); err != nil {
			s.warnings = append(s.warnings, Warning{Item: id, Message: "meta.json 解析失败: " + err.Error()})
			continue
		}
		if it.ID == "" {
			it.ID = id
		}
		it.iconPNG = g.icon
		if len(it.iconPNG) == 0 {
			it.iconPNG = PlaceholderIcon()
			s.warnings = append(s.warnings, Warning{Item: id, Message: "缺少 icon.png，已用占位图代替"})
		}
		sort.Strings(g.order)
		kept := make([]Sample, 0, len(it.Samples))
		for _, sm := range it.Samples {
			b, ok := g.wavs[sm.File]
			if !ok {
				s.warnings = append(s.warnings, Warning{Item: id, Message: "样本文件缺失: " + sm.File})
				continue
			}
			it.sampleWAV = append(it.sampleWAV, b)
			kept = append(kept, sm)
			delete(g.wavs, sm.File)
		}
		// Any WAV present but not referenced by meta.json is still kept, so a
		// crash between "write blob" and "write meta" cannot lose audio.
		for _, rel := range g.order {
			if b, ok := g.wavs[rel]; ok {
				kept = append(kept, Sample{
					File:       rel,
					Source:     "recovered",
					AddedAt:    time.Now().UTC(),
					StoredRate: SampleRate, StoredChans: Channels, StoredBits: BitsPerSample,
				})
				it.sampleWAV = append(it.sampleWAV, b)
				s.warnings = append(s.warnings, Warning{Item: id, Message: "发现未被 meta.json 引用的样本，已恢复: " + rel})
			}
		}
		it.Samples = kept
		it.hintPNG = make([][]byte, len(it.DisplayHints))
		for i := range it.DisplayHints {
			file := it.DisplayHints[i].Icon
			if file == "" {
				file = HintFile(i)
			}
			if b, ok := g.hints[file]; ok {
				it.hintPNG[i] = b
				it.DisplayHints[i].Icon = file
			}
		}
		s.items[id] = &it
		s.order = append(s.order, id)
	}

	// manifest order first, then anything the manifest forgot.
	final := make([]string, 0, len(s.order))
	for _, id := range s.mf.Items {
		if _, ok := s.items[id]; ok && !contains(final, id) {
			final = append(final, id)
		}
	}
	for _, id := range s.order {
		if !contains(final, id) {
			final = append(final, id)
		}
	}
	s.order = final
	s.mf.Items = append([]string(nil), final...)

	if len(s.mf.Items) != len(groups) {
		s.warnings = append(s.warnings, Warning{Message: fmt.Sprintf(
			"manifest 列出 %d 个条目，归档里发现 %d 个条目目录", len(s.mf.Items), len(groups))})
	}
	return s, nil
}

func contains(ss []string, v string) bool {
	for _, s := range ss {
		if s == v {
			return true
		}
	}
	return false
}

func openZipEntry(zr *zip.Reader, name string) (io.ReadCloser, error) {
	for _, zf := range zr.File {
		if strings.ReplaceAll(zf.Name, "\\", "/") == name {
			return zf.Open()
		}
	}
	return nil, os.ErrNotExist
}

func readZipEntry(zf *zip.File) ([]byte, error) {
	rc, err := zf.Open()
	if err != nil {
		return nil, fmt.Errorf("打开 %s: %w", zf.Name, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("读取 %s: %w", zf.Name, err)
	}
	return b, nil
}

// ---------------------------------------------------------------------------
// accessors
// ---------------------------------------------------------------------------

// Path returns the absolute path of the library file.
func (s *Store) Path() string { return s.path }

// Manifest returns a copy of the manifest.
func (s *Store) Manifest() Manifest {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m := s.mf
	m.Items = append([]string(nil), s.mf.Items...)
	return m
}

// SetName renames the library.
func (s *Store) SetName(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mf.Name = strings.TrimSpace(name)
}

// Warnings returns the load-time consistency warnings.
func (s *Store) Warnings() []Warning {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]Warning(nil), s.warnings...)
}

// Len returns the number of items.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.order)
}

// Order returns the item ids in display order.
func (s *Store) Order() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), s.order...)
}

// Get returns a deep copy of an item (including blobs) or nil.
func (s *Store) Get(id string) *Item {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.getLocked(id)
}

func (s *Store) getLocked(id string) *Item {
	it, ok := s.items[id]
	if !ok {
		return nil
	}
	return cloneItem(it)
}

func cloneItem(it *Item) *Item {
	c := *it
	c.Tags = append([]string(nil), it.Tags...)
	c.DisplayHints = append([]DisplayHint(nil), it.DisplayHints...)
	c.Samples = append([]Sample(nil), it.Samples...)
	c.iconPNG = append([]byte(nil), it.iconPNG...)
	c.sampleWAV = make([][]byte, len(it.sampleWAV))
	for i, b := range it.sampleWAV {
		c.sampleWAV[i] = append([]byte(nil), b...)
	}
	c.hintPNG = make([][]byte, len(it.hintPNG))
	for i, b := range it.hintPNG {
		c.hintPNG[i] = append([]byte(nil), b...)
	}
	return &c
}

// Stats summarizes the library for the web UI header.
type Stats struct {
	Path        string    `json:"path"`
	Name        string    `json:"name"`
	Schema      int       `json:"schema"`
	CreatedAt   time.Time `json:"createdAt"`
	ItemCount   int       `json:"itemCount"`
	SampleCount int       `json:"sampleCount"`
	FileBytes   int64     `json:"fileBytes"`
	SampleRate  int       `json:"sampleRate"`
	Channels    int       `json:"channels"`
	Bits        int       `json:"bitsPerSample"`
	Feature     Feature   `json:"feature"`
}

// Stats returns current counters, including the on-disk size of the container.
func (s *Store) Stats() Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := Stats{
		Path:       s.path,
		Name:       s.mf.Name,
		Schema:     s.mf.Schema,
		CreatedAt:  s.mf.CreatedAt,
		ItemCount:  len(s.order),
		SampleRate: SampleRate,
		Channels:   Channels,
		Bits:       BitsPerSample,
		Feature:    s.mf.Feature,
	}
	for _, it := range s.items {
		st.SampleCount += len(it.Samples)
	}
	if fi, err := os.Stat(s.path); err == nil {
		st.FileBytes = fi.Size()
	}
	return st
}

// ---------------------------------------------------------------------------
// mutations
// ---------------------------------------------------------------------------

// NewID returns a random 8 hex character identifier.
func NewID() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("生成条目 id 失败: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// ErrExists is returned when a generated id collides with an existing item.
var ErrExists = errors.New("条目已存在")

// ErrNotFound is returned when the requested item does not exist.
var ErrNotFound = errors.New("条目不存在")

// AddItem inserts a new item. meta.ID may be empty, in which case a fresh id is
// generated. wavs must be canonical 48 kHz/mono/16-bit WAV blobs and match
// meta.Samples one for one. The library is not written to disk here; call Save.
func (s *Store) AddItem(it *Item, iconPNG []byte, wavs [][]byte) error {
	if it == nil {
		return errors.New("条目为空")
	}
	if strings.TrimSpace(it.Name) == "" {
		return errors.New("名称不能为空")
	}
	if len(wavs) != len(it.Samples) {
		return fmt.Errorf("样本数量不匹配: meta=%d wav=%d", len(it.Samples), len(wavs))
	}
	for i, w := range wavs {
		if len(w) == 0 {
			return fmt.Errorf("样本 %d 为空", i)
		}
	}
	hints, err := NormalizeDisplayHints(it.DisplayHints)
	if err != nil {
		return err
	}
	it.DisplayHints = hints
	if len(iconPNG) == 0 {
		iconPNG = PlaceholderIcon()
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if it.ID == "" {
		for attempt := 0; attempt < 64; attempt++ {
			id, err := NewID()
			if err != nil {
				return err
			}
			if _, dup := s.items[id]; !dup {
				it.ID = id
				break
			}
		}
		if it.ID == "" {
			return errors.New("无法生成唯一 id")
		}
	} else if _, dup := s.items[it.ID]; dup {
		return fmt.Errorf("%w: %s", ErrExists, it.ID)
	}

	now := time.Now().UTC()
	if it.CreatedAt.IsZero() {
		it.CreatedAt = now
	}
	it.UpdatedAt = now
	if it.Icon == "" {
		it.Icon = IconName
	}
	if it.Profile == "" {
		it.Profile = "default"
	}
	if it.Tags == nil {
		it.Tags = []string{}
	}
	for i := range it.Samples {
		if it.Samples[i].AddedAt.IsZero() {
			it.Samples[i].AddedAt = now
		}
		if it.Samples[i].Source == "" {
			it.Samples[i].Source = "upload"
		}
	}

	stored := cloneItem(it)
	stored.iconPNG = append([]byte(nil), iconPNG...)
	stored.sampleWAV = make([][]byte, len(wavs))
	for i, b := range wavs {
		stored.sampleWAV[i] = append([]byte(nil), b...)
	}
	s.items[it.ID] = stored
	s.order = append(s.order, it.ID)
	s.mf.Items = append([]string(nil), s.order...)
	return nil
}

// UpdateFunc mutates a copy of the item in place; returning an error aborts.
type UpdateFunc func(*Item) error

// Update applies fn to an item under the write lock.
func (s *Store) Update(id string, fn UpdateFunc) (*Item, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it, ok := s.items[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	work := cloneItem(it)
	oldHints := append([]DisplayHint(nil), work.DisplayHints...)
	pngBefore := work.hintPNG
	if err := fn(work); err != nil {
		return nil, err
	}
	work.ID = id
	work.UpdatedAt = time.Now().UTC()
	if strings.TrimSpace(work.Name) == "" {
		return nil, errors.New("名称不能为空")
	}
	hints, err := NormalizeDisplayHints(work.DisplayHints)
	if err != nil {
		return nil, err
	}
	work.DisplayHints = hints
	// If the callback did not install a new icon list, keep pictures on the
	// same item name. Otherwise a newly added row inherits a neighbour's file
	// (or, in the UI, the class icon).
	if sameHintPNG(work.hintPNG, pngBefore) {
		realignHintIcons(work, oldHints, pngBefore)
	} else {
		syncHintIconPaths(work)
	}
	if work.Icon == "" {
		work.Icon = IconName
	}
	if work.Tags == nil {
		work.Tags = []string{}
	}
	if work.DisplayHints == nil {
		work.DisplayHints = []DisplayHint{}
	}
	s.items[id] = work
	out := cloneItem(work)
	out.iconPNG = nil
	out.sampleWAV = nil
	return out, nil
}

// Delete removes an item.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.items[id]; !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	delete(s.items, id)
	next := s.order[:0:0]
	for _, x := range s.order {
		if x != id {
			next = append(next, x)
		}
	}
	s.order = next
	s.mf.Items = append([]string(nil), s.order...)
	return nil
}

// AddSample appends one audio variant (already converted to canonical WAV).
func (s *Store) AddSample(id string, sm Sample, wavBytes []byte) (*Item, error) {
	if len(wavBytes) == 0 {
		return nil, errors.New("样本音频为空")
	}
	return s.Update(id, func(it *Item) error {
		if sm.AddedAt.IsZero() {
			sm.AddedAt = time.Now().UTC()
		}
		if sm.Source == "" {
			sm.Source = "upload"
		}
		if sm.StoredRate == 0 {
			sm.StoredRate = SampleRate
		}
		if sm.StoredChans == 0 {
			sm.StoredChans = Channels
		}
		if sm.StoredBits == 0 {
			sm.StoredBits = BitsPerSample
		}
		it.Samples = append(it.Samples, sm)
		it.sampleWAV = append(it.sampleWAV, append([]byte(nil), wavBytes...))
		return nil
	})
}

// DeleteSample removes sample index n and renumbers the remaining files so the
// on-disk names stay contiguous (0001.wav, 0002.wav, ...).
func (s *Store) DeleteSample(id string, n int) (*Item, error) {
	return s.Update(id, func(it *Item) error {
		if n < 0 || n >= len(it.Samples) {
			return fmt.Errorf("样本序号 %d 超出范围（共 %d 个）", n, len(it.Samples))
		}
		it.Samples = append(it.Samples[:n:n], it.Samples[n+1:]...)
		it.sampleWAV = append(it.sampleWAV[:n:n], it.sampleWAV[n+1:]...)
		for i := range it.Samples {
			it.Samples[i].File = SampleFile(i)
		}
		return nil
	})
}

// SetIcon replaces the icon blob.
func (s *Store) SetIcon(id string, png []byte) (*Item, error) {
	if len(png) == 0 {
		return nil, errors.New("图标数据为空")
	}
	return s.Update(id, func(it *Item) error {
		it.iconPNG = append([]byte(nil), png...)
		it.Icon = IconName
		return nil
	})
}

// SampleFile returns the container path of sample index i (0-based).
func SampleFile(i int) string {
	return fmt.Sprintf("%s/%04d.wav", SamplesDir, i+1)
}

// HintFile returns the container path of hint icon index i (0-based).
func HintFile(i int) string {
	return fmt.Sprintf("%s/%02d.png", HintsDir, i)
}

// realignHintIcons keeps hint pictures attached to item names when the hint
// list is edited. Rows with no previous picture stay empty.
func realignHintIcons(it *Item, oldHints []DisplayHint, oldPNG [][]byte) {
	byName := map[string][]byte{}
	for i, h := range oldHints {
		name := strings.TrimSpace(h.Name)
		if name == "" || i >= len(oldPNG) || len(oldPNG[i]) == 0 {
			continue
		}
		if _, ok := byName[name]; !ok {
			byName[name] = oldPNG[i]
		}
	}
	out := make([][]byte, len(it.DisplayHints))
	for i, h := range it.DisplayHints {
		name := strings.TrimSpace(h.Name)
		if b, ok := byName[name]; ok {
			out[i] = b
			it.DisplayHints[i].Icon = HintFile(i)
		} else {
			it.DisplayHints[i].Icon = ""
		}
	}
	it.hintPNG = out
}

func syncHintIconPaths(it *Item) {
	for i := range it.DisplayHints {
		if len(it.HintIcon(i)) > 0 {
			it.DisplayHints[i].Icon = HintFile(i)
		} else {
			it.DisplayHints[i].Icon = ""
		}
	}
}

func sameHintPNG(a, b [][]byte) bool {
	if len(a) != len(b) || cap(a) != cap(b) {
		return false
	}
	if len(a) == 0 {
		return true
	}
	return &a[0] == &b[0]
}

// SetHintIcons replaces per-hint PNG blobs (parallel to DisplayHints).
// Missing slots may be nil; length should match DisplayHints (extra ignored, short padded).
func (s *Store) SetHintIcons(id string, pngs [][]byte) (*Item, error) {
	return s.Update(id, func(it *Item) error {
		out := make([][]byte, len(it.DisplayHints))
		for i := range out {
			if i < len(pngs) && len(pngs[i]) > 0 {
				out[i] = append([]byte(nil), pngs[i]...)
			} else if len(it.HintIcon(i)) > 0 {
				// No new file for this row: keep its own picture.
				// Never copy another row's (or the class) icon into the gap.
				out[i] = append([]byte(nil), it.HintIcon(i)...)
			}
			if len(out[i]) > 0 {
				it.DisplayHints[i].Icon = HintFile(i)
			}
		}
		it.hintPNG = out
		return nil
	})
}

// ---------------------------------------------------------------------------
// persistence
// ---------------------------------------------------------------------------

// Save writes the whole library to disk through a temporary file + rename, so
// a crash mid-write can never truncate an existing library.
func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked()
}

func (s *Store) saveLocked() error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建目录 %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".library-*.srz.tmp")
	if err != nil {
		return fmt.Errorf("创建临时文件: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpName)
	}

	zw := zip.NewWriter(tmp)
	s.mf.Items = append([]string(nil), s.order...)
	if s.mf.Schema == 0 {
		s.mf.Schema = Schema
	}
	if s.mf.Feature.Kind == "" {
		s.mf.Feature = DefaultFeature()
	}
	// Zeros are never valid for the analysis parameters.
	if s.mf.Feature.SampleRate == 0 {
		s.mf.Feature.SampleRate = SampleRate
	}

	mfBytes, err := json.MarshalIndent(&s.mf, "", "  ")
	if err != nil {
		cleanup()
		return fmt.Errorf("序列化 manifest: %w", err)
	}
	if err := writeZipFile(zw, ManifestName, mfBytes); err != nil {
		cleanup()
		return err
	}

	for _, id := range s.order {
		it, ok := s.items[id]
		if !ok {
			continue
		}
		// meta.json must exist and stay in sync with the blobs actually written.
		meta := *it
		meta.Samples = append([]Sample(nil), it.Samples...)
		for i := range meta.Samples {
			meta.Samples[i].File = SampleFile(i)
		}
		meta.DisplayHints = append([]DisplayHint(nil), it.DisplayHints...)
		for i := range meta.DisplayHints {
			if len(it.HintIcon(i)) > 0 {
				meta.DisplayHints[i].Icon = HintFile(i)
				it.DisplayHints[i].Icon = meta.DisplayHints[i].Icon
			}
		}
		mb, err := json.MarshalIndent(&meta, "", "  ")
		if err != nil {
			cleanup()
			return fmt.Errorf("序列化 %s/meta.json: %w", id, err)
		}
		icon := it.iconPNG
		if len(icon) == 0 {
			icon = PlaceholderIcon()
		}
		if err := writeZipFile(zw, "items/"+id+"/"+MetaName, mb); err != nil {
			cleanup()
			return err
		}
		if err := writeZipFile(zw, "items/"+id+"/"+IconName, icon); err != nil {
			cleanup()
			return err
		}
		for i := range it.DisplayHints {
			png := it.HintIcon(i)
			if len(png) == 0 {
				continue
			}
			if err := writeZipFile(zw, "items/"+id+"/"+HintFile(i), png); err != nil {
				cleanup()
				return err
			}
		}
		for i, sm := range meta.Samples {
			blob := it.BlobFor(i)
			if len(blob) == 0 {
				cleanup()
				return fmt.Errorf("条目 %s 的样本 %s 数据缺失", id, sm.File)
			}
			if err := writeZipFile(zw, "items/"+id+"/"+sm.File, blob); err != nil {
				cleanup()
				return err
			}
		}
	}

	if err := zw.Close(); err != nil {
		cleanup()
		return fmt.Errorf("关闭 ZIP: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("同步临时文件: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("关闭临时文件: %w", err)
	}
	if err := replaceFile(tmpName, s.path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("替换库文件 %s: %w", s.path, err)
	}
	syncDir(dir)
	return nil
}

// BlobFor returns the stored payload for the i-th sample.
func (it *Item) BlobFor(i int) []byte { return it.SampleWAV(i) }

func writeZipFile(zw *zip.Writer, name string, data []byte) error {
	hdr := &zip.FileHeader{
		Name:     name,
		Method:   zip.Deflate,
		Modified: time.Now().UTC(),
	}
	hdr.SetMode(0o644)
	w, err := zw.CreateHeader(hdr)
	if err != nil {
		return fmt.Errorf("写入 %s: %w", name, err)
	}
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("写入 %s: %w", name, err)
	}
	return nil
}

// MarshalItemJSON renders an item's meta.json (useful for tests/tools).
func MarshalItemJSON(it *Item) ([]byte, error) {
	c := *it
	c.Samples = append([]Sample(nil), it.Samples...)
	return json.MarshalIndent(&c, "", "  ")
}

// UnmarshalManifest parses a manifest document.
func UnmarshalManifest(b []byte) (Manifest, error) {
	var m Manifest
	err := json.Unmarshal(b, &m)
	return m, err
}
