// Package recall implements the P4 "save the last N seconds" feature: a fixed
// size ring buffer of recently heard audio plus a small on-disk inbox of
// candidates that the user can name later.
//
// The pain it removes: a new sound appears during play, the player has no way to
// name it on the spot (today: alt-tab, start a recorder, trim afterwards). With a
// recall ring the whole pipeline is "press F8" - the audio is already in memory.
//
// The package is deliberately pure Go and free of any Win32 or HTTP dependency:
//
//   - Ring holds the trailing seconds of canonical 48 kHz / mono / int16 PCM.
//   - Store writes a snapshot as <dir>/<seq>-<ts>.wav plus a sibling .json.
//   - Recaller is the small piece of state that ties a Ring to a Store and
//     remembers the top-ranked library item so a saved candidate can carry the
//     "guess" that was on screen at that moment.
//
// internal/live only knows about the OnBlock callback (canonical 48 kHz mono
// float32 blocks); who consumes it is this package's business, so there is no
// import cycle and the whole thing is unit-testable without a sound card.
package recall

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/znz/soundradar/internal/index"
	"github.com/znz/soundradar/internal/wav"
)

// SampleRate is the canonical rate of every sample in this package. A snapshot
// is written to disk unchanged, which is exactly the format internal/library
// stores its samples in.
const SampleRate = 48000

// Length limits enforced by NewRing / NewStore.
const (
	// MinSeconds and MaxSeconds bound Ring windows. The P4 spec pins
	// config.recall.seconds to 1..30; the ring itself accepts the same range.
	MinSeconds = 1
	MaxSeconds = 30
	// MinFiles and MaxFiles bound Store.maxFiles.
	MinFiles = 1
	MaxFiles = 5000
)

// ---------------------------------------------------------------------------
// Ring
// ---------------------------------------------------------------------------

// Ring is a fixed-capacity circular buffer of 48 kHz mono int16 PCM.
//
// Capacity is exactly seconds*SampleRate samples and NEVER grows: Push
// overwrites the oldest sample once the buffer is full. That is the whole point
// - the realtime link may run for hours and the recall window must stay a few
// hundred kilobytes.
//
// It is safe for concurrent use: Push runs on the recognition goroutine while
// Snapshot runs on whichever goroutine handled the hotkey.
type Ring struct {
	mu   sync.Mutex
	buf  []int16
	head int // index of the oldest sample in buf
	n    int // number of valid samples (<= len(buf))
}

// NewRing returns a ring holding seconds seconds of audio. seconds is clamped to
// [MinSeconds, MaxSeconds], so a bad configuration value can never allocate an
// unbounded buffer (30 s = 2.88 MB of int16).
func NewRing(seconds int) *Ring {
	return &Ring{buf: make([]int16, clampSeconds(seconds)*SampleRate)}
}

// clampSeconds maps an out-of-range window onto the supported range.
func clampSeconds(seconds int) int {
	if seconds < MinSeconds {
		return MinSeconds
	}
	if seconds > MaxSeconds {
		return MaxSeconds
	}
	return seconds
}

// Cap returns the capacity in samples (fixed for the ring's whole life).
func (r *Ring) Cap() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.buf)
}

// Push appends pcm, dropping as many of the oldest samples as needed. Any length
// is handled, including a length larger than the whole capacity (only the newest
// capacity samples survive), and the buffer is written with at most two copies,
// so a block that straddles the wrap point stays in time order.
func (r *Ring) Push(pcm []int16) {
	if r == nil || len(pcm) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	capN := len(r.buf)
	if capN == 0 {
		return
	}
	if len(pcm) >= capN {
		// Only the newest capN samples can survive; they are contiguous in pcm.
		pcm = pcm[len(pcm)-capN:]
		r.head = 0
		r.n = capN
		copy(r.buf, pcm)
		return
	}

	// First chunk: from head to the end of the backing array.
	start := r.head
	if start >= capN {
		start = 0
	}
	first := capN - start
	if first > len(pcm) {
		first = len(pcm)
	}
	copy(r.buf[start:start+first], pcm[:first])
	rest := len(pcm) - first
	if rest > 0 {
		copy(r.buf[0:rest], pcm[first:])
	}
	r.head = (start + len(pcm)) % capN
	r.n += len(pcm)
	if r.n > capN {
		r.n = capN
	}
}

// Len returns the number of samples currently held (<= Cap).
func (r *Ring) Len() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n
}

// Seconds reports how much audio the ring actually covers right now: the
// requested window once it is full, less while it is still filling.
func (r *Ring) Seconds() float64 {
	return float64(r.Len()) / SampleRate
}

// Reset empties the ring without reallocating it.
func (r *Ring) Reset() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.head, r.n = 0, 0
}

// Snapshot returns a copy of the window in time order (newest sample last).
//
// Invariant, and the one the P4 acceptance test checks sample by sample: after
// pushing T seconds of audio, Snapshot() equals the last min(T, N) seconds of
// that input, exactly.
func (r *Ring) Snapshot() []int16 {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.n == 0 {
		return nil
	}
	out := make([]int16, r.n)
	start := (r.head - r.n + len(r.buf)) % len(r.buf)
	if start < 0 {
		start += len(r.buf)
	}
	first := len(r.buf) - start
	if first > r.n {
		first = r.n
	}
	copy(out, r.buf[start:start+first])
	if first < r.n {
		copy(out[first:], r.buf[:r.n-first])
	}
	return out
}

// ---------------------------------------------------------------------------
// Store
// ---------------------------------------------------------------------------

// ErrNotFound is wrapped by every lookup that misses, so callers can map it onto
// an HTTP 404 (or any other "no such thing" answer) with errors.Is.
var ErrNotFound = errors.New("候选项不存在")

// Candidate is one saved "what I just heard" clip.
type Candidate struct {
	// ID is "<seq>-<yyyymmdd-hhmmss>" and is also the file stem, so a candidate
	// is addressable from the API/CLI without a separate index.
	ID string `json:"id"`
	// File is the absolute path of the WAV.
	File string `json:"file"`
	// Seconds is the clip length (len(pcm)/48000).
	Seconds float64 `json:"seconds"`
	// PeakDBFS is the peak level; it is 0 and Silent is true for digital
	// silence. encoding/json refuses ±Inf, so the raw dBFS is never stored -
	// this is the P1 lesson repeated on purpose.
	PeakDBFS float64 `json:"peakDbfs"`
	// Silent marks a clip whose every sample is exactly zero.
	Silent bool `json:"silent"`
	// Frames is the sample count of the stored WAV.
	Frames int64 `json:"frames"`
	// CreatedAt is when Add ran (UTC).
	CreatedAt time.Time `json:"createdAt"`
	// GuessID/GuessName/GuessScore are the top-1 library hit at save time
	// ("this is probably 880Hz 测试音 0.98"). They are best-effort: the index may
	// be empty, in which case GuessID stays empty.
	GuessID    string  `json:"guessId,omitempty"`
	GuessName  string  `json:"guessName,omitempty"`
	GuessScore float64 `json:"guessScore,omitempty"`
	// GuessScoreValid distinguishes a real 0.0 score from "no guess".
	GuessScoreValid bool `json:"guessScoreValid,omitempty"`
	// Source records who produced the clip: "hotkey", "cli", "api", "overlay".
	Source string `json:"source,omitempty"`
}

// CandidateMeta is the extra information a caller knows at save time.
type CandidateMeta struct {
	GuessID    string
	GuessName  string
	GuessScore float64
	Source     string
}

// FileName returns the WAV file name of a candidate.
func (c Candidate) FileName() string { return filepath.Base(c.File) }

// Store is the candidate inbox: one directory of <id>.wav + <id>.json pairs.
//
// Writes are atomic (temp file + MoveFileExW/MOVEFILE_REPLACE_EXISTING, exactly
// like internal/library) and the WAV is always written BEFORE its JSON, so a
// crash can leave an orphan .wav but never a .json pointing at a missing file.
type Store struct {
	dir      string
	maxFiles int

	mu  sync.Mutex
	seq int
}

// NewStore prepares dir and returns a store keeping at most maxFiles candidates.
// maxFiles is clamped to [MinFiles, MaxFiles]; a non-positive value means the
// default of 200.
func NewStore(dir string, maxFiles int) (*Store, error) {
	d := strings.TrimSpace(dir)
	if d == "" {
		return nil, errors.New("候选项目录为空")
	}
	abs, err := filepath.Abs(d)
	if err != nil {
		return nil, fmt.Errorf("候选项目录无效 %q: %w", dir, err)
	}
	if maxFiles <= 0 {
		maxFiles = 200
	}
	if maxFiles < MinFiles {
		maxFiles = MinFiles
	}
	if maxFiles > MaxFiles {
		maxFiles = MaxFiles
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("创建候选项目录 %s 失败: %w", abs, err)
	}
	s := &Store{dir: abs, maxFiles: maxFiles}
	s.seq = s.scanSeq()
	return s, nil
}

// Dir returns the absolute directory of the inbox.
func (s *Store) Dir() string { return s.dir }

// MaxFiles returns the effective retention limit.
func (s *Store) MaxFiles() int { return s.maxFiles }

// scanSeq finds the highest sequence number already on disk so a restart never
// reuses an id (and therefore never overwrites an existing candidate).
func (s *Store) scanSeq() int {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return 0
	}
	best := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(strings.ToLower(name), ".wav") {
			continue
		}
		stem := name[:len(name)-4]
		dash := strings.IndexByte(stem, '-')
		if dash <= 0 {
			continue
		}
		if n, err := strconv.Atoi(stem[:dash]); err == nil && n > best {
			best = n
		}
	}
	return best
}

// Add writes pcm (canonical 48 kHz mono int16) as a candidate and returns it.
//
// The WAV is written first, atomically, then the JSON. The JSON is the source of
// truth for List/Get: a WAV without JSON is invisible (and cleaned up by Prune),
// while a JSON without WAV can never happen.
func (s *Store) Add(pcm []int16, meta CandidateMeta) (Candidate, error) {
	if s == nil {
		return Candidate{}, errors.New("候选项仓库未打开")
	}
	if len(pcm) == 0 {
		return Candidate{}, errors.New("候选音频为空（环形缓冲里还没有音频）")
	}
	now := time.Now().UTC()

	s.mu.Lock()
	defer s.mu.Unlock()

	s.seq++
	id := fmt.Sprintf("%06d-%s", s.seq, now.Format("20060102-150405"))
	wavPath := filepath.Join(s.dir, id+".wav")
	jsonPath := filepath.Join(s.dir, id+".json")

	stats := wav.StatsInt16(pcm, 1)
	cand := Candidate{
		ID:         id,
		File:       wavPath,
		Seconds:    float64(len(pcm)) / SampleRate,
		Silent:     stats.Silent,
		Frames:     int64(len(pcm)),
		CreatedAt:  now,
		GuessID:    strings.TrimSpace(meta.GuessID),
		GuessName:  strings.TrimSpace(meta.GuessName),
		GuessScore: meta.GuessScore,
		Source:     meta.Source,
	}
	if !stats.Silent && !math.IsInf(stats.PeakDBFS, 0) && !math.IsNaN(stats.PeakDBFS) {
		cand.PeakDBFS = round2(stats.PeakDBFS)
	}
	if cand.GuessID != "" {
		cand.GuessScoreValid = true
		cand.GuessScore = round4(cand.GuessScore)
	} else {
		cand.GuessScore = 0
	}

	if err := writeWAVAtomic(wavPath, pcm); err != nil {
		s.seq--
		return Candidate{}, err
	}
	body, err := json.MarshalIndent(&cand, "", "  ")
	if err != nil {
		os.Remove(wavPath)
		s.seq--
		return Candidate{}, fmt.Errorf("序列化候选项元数据失败: %w", err)
	}
	body = append(body, '\n')
	if err := writeFileAtomic(jsonPath, body); err != nil {
		os.Remove(wavPath)
		s.seq--
		return Candidate{}, err
	}
	return cand, nil
}

// List returns every candidate, newest first (timestamp, then sequence number).
func (s *Store) List() ([]Candidate, error) {
	if s == nil {
		return nil, errors.New("候选项仓库未打开")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("读取候选项目录 %s 失败: %w", s.dir, err)
	}
	out := make([]Candidate, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(name), ".json") {
			continue
		}
		// Already-promoted candidates live in the same directory as *.used.json
		// and are deliberately NOT part of the inbox.
		if strings.HasSuffix(name, ".used.json") {
			continue
		}
		id := name[:len(name)-5]
		c, err := s.getLocked(id)
		if err != nil {
			continue // a torn or unreadable entry must not break the whole list
		}
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	return out, nil
}

// Get returns one candidate by id.
func (s *Store) Get(id string) (Candidate, error) {
	if s == nil {
		return Candidate{}, errors.New("候选项仓库未打开")
	}
	if err := validateID(id); err != nil {
		return Candidate{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getLocked(id)
}

func (s *Store) getLocked(id string) (Candidate, error) {
	raw, err := os.ReadFile(s.jsonPath(id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Candidate{}, fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		return Candidate{}, fmt.Errorf("读取候选项 %s 失败: %w", id, err)
	}
	var c Candidate
	if err := json.Unmarshal(raw, &c); err != nil {
		return Candidate{}, fmt.Errorf("解析候选项 %s 的元数据失败: %w", id, err)
	}
	if c.ID == "" {
		c.ID = id
	}
	if c.File == "" {
		c.File = s.wavPath(id)
	}
	if c.Frames == 0 && c.Seconds > 0 {
		c.Frames = int64(math.Round(c.Seconds * SampleRate))
	}
	return c, nil
}

// WAVPath returns the absolute WAV path of a candidate id (the file may not
// exist; use Get or Open to check).
func (s *Store) WAVPath(id string) (string, error) {
	if err := validateID(id); err != nil {
		return "", err
	}
	return s.wavPath(id), nil
}

// Open returns the candidate's WAV bytes.
func (s *Store) Open(id string) ([]byte, error) {
	p, err := s.WAVPath(id)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s（音频文件缺失）", ErrNotFound, id)
		}
		return nil, fmt.Errorf("读取候选项音频 %s 失败: %w", id, err)
	}
	return b, nil
}

// Delete removes the candidate's .json and .wav. A missing file is not an error
// when the other half is gone too; deleting a completely unknown id is.
func (s *Store) Delete(id string) error {
	if s == nil {
		return errors.New("候选项仓库未打开")
	}
	if err := validateID(id); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	jsonPath, wavPath := s.jsonPath(id), s.wavPath(id)
	jErr := os.Remove(jsonPath)
	wErr := os.Remove(wavPath)
	if jErr != nil && wErr != nil {
		return fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return nil
}

// MarkUsed renames the candidate into the "已处理" corner of the inbox.
//
// The P4 promote flow CONSUMES a candidate: rather than deleting the audio the
// instant it becomes a library sample, the .json is renamed to .used.json and
// the .wav to .used.wav. The inbox (List) then no longer shows it, the audio is
// still on disk for a later "undo", and Prune() eventually removes the oldest
// used entries. This is the documented behaviour of the promote endpoint.
func (s *Store) MarkUsed(id string) error {
	if s == nil {
		return errors.New("候选项仓库未打开")
	}
	if err := validateID(id); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.Rename(s.jsonPath(id), s.usedJSONPath(id)); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		return fmt.Errorf("标记候选项 %s 已处理失败: %w", id, err)
	}
	if err := os.Rename(s.wavPath(id), s.usedWAVPath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("标记候选项音频 %s 已处理失败: %w", id, err)
	}
	return nil
}

// ListUsed returns the already-promoted candidates, newest first. They are not
// part of the inbox but they are still on disk until Prune removes them.
func (s *Store) ListUsed() ([]Candidate, error) {
	if s == nil {
		return nil, errors.New("候选项仓库未打开")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("读取候选项目录 %s 失败: %w", s.dir, err)
	}
	out := []Candidate{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".used.json") {
			continue
		}
		id := strings.TrimSuffix(name, ".used.json")
		raw, err := os.ReadFile(filepath.Join(s.dir, name))
		if err != nil {
			continue
		}
		var c Candidate
		if err := json.Unmarshal(raw, &c); err != nil {
			continue
		}
		if c.ID == "" {
			c.ID = id
		}
		if _, err := os.Stat(s.usedWAVPath(id)); err != nil {
			continue
		}
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// Prune enforces maxFiles and removes orphans. The policy, in order:
//
//  1. every .wav without a matching .json is deleted (a crash between the two
//     writes of Add, or a foreign file dropped into the directory);
//  2. every used candidate (.used.wav/.used.json) older than the newest
//     maxFiles/4 used entries is deleted - the "undo" copies do not grow forever;
//  3. while the inbox holds more than maxFiles entries, the OLDEST are deleted
//     (both halves), so the newest maxFiles always survive.
//
// It returns how many candidate entries it removed.
func (s *Store) Prune() (int, error) {
	if s == nil {
		return 0, errors.New("候选项仓库未打开")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return 0, fmt.Errorf("读取候选项目录 %s 失败: %w", s.dir, err)
	}

	haveJSON := map[string]bool{}
	haveWAV := map[string]bool{}
	used := []string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		switch {
		case strings.HasSuffix(name, ".used.json"):
			used = append(used, strings.TrimSuffix(name, ".used.json"))
		case strings.HasSuffix(name, ".used.wav"):
			// handled together with its .used.json
		case strings.HasSuffix(strings.ToLower(name), ".json"):
			haveJSON[name[:len(name)-5]] = true
		case strings.HasSuffix(strings.ToLower(name), ".wav"):
			haveWAV[name[:len(name)-4]] = true
		}
	}

	removed := 0
	// (1) orphan audio: written but its JSON never made it.
	for id := range haveWAV {
		if !haveJSON[id] {
			os.Remove(s.wavPath(id))
			removed++
		}
	}
	// (2) cap the used/"undo" pile at a quarter of the retention limit (min 1).
	keepUsed := s.maxFiles / 4
	if keepUsed < 1 {
		keepUsed = 1
	}
	sort.Sort(sort.Reverse(sort.StringSlice(used)))
	for i, id := range used {
		if i < keepUsed {
			continue
		}
		os.Remove(s.usedJSONPath(id))
		os.Remove(s.usedWAVPath(id))
		removed++
	}
	// (3) inbox retention: newest maxFiles win.
	list, err := s.listLocked()
	if err != nil {
		return removed, err
	}
	if len(list) > s.maxFiles {
		for _, c := range list[s.maxFiles:] {
			os.Remove(s.jsonPath(c.ID))
			os.Remove(s.wavPath(c.ID))
			removed++
		}
	}
	return removed, nil
}

// listLocked is List for callers that already hold s.mu.
func (s *Store) listLocked() ([]Candidate, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("读取候选项目录 %s 失败: %w", s.dir, err)
	}
	out := make([]Candidate, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".used.json") {
			continue
		}
		c, err := s.getLocked(strings.TrimSuffix(name, ".json"))
		if err != nil {
			continue
		}
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})
	return out, nil
}

func (s *Store) jsonPath(id string) string     { return filepath.Join(s.dir, id+".json") }
func (s *Store) wavPath(id string) string      { return filepath.Join(s.dir, id+".wav") }
func (s *Store) usedJSONPath(id string) string { return filepath.Join(s.dir, id+".used.json") }
func (s *Store) usedWAVPath(id string) string  { return filepath.Join(s.dir, id+".used.wav") }

// validateID rejects anything that could escape the store directory. Candidate
// ids come from the URL path, so this is a security boundary, not a formality.
func validateID(id string) error {
	if id == "" {
		return errors.New("候选项 id 为空")
	}
	if len(id) > 64 {
		return errors.New("候选项 id 太长（最多 64 个字符）")
	}
	for _, r := range id {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r == '-' || r == '_':
		default:
			return fmt.Errorf("候选项 id 含非法字符 %q", string(r))
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Recaller
// ---------------------------------------------------------------------------

// Recaller is the gluing state: one Ring, one Store and the last top-1 guess.
type Recaller struct {
	ring   *Ring
	store  *Store
	onSave func(Candidate)

	mu         sync.Mutex
	guessID    string
	guessName  string
	guessScore float64
	saves      int64
}

// NewRecaller builds a recaller over a freshly allocated ring.
func NewRecaller(seconds int, store *Store) *Recaller {
	return &Recaller{ring: NewRing(seconds), store: store}
}

// Ring exposes the buffer (for diagnostics and tests).
func (r *Recaller) Ring() *Ring { return r.ring }

// Store exposes the inbox.
func (r *Recaller) Store() *Store { return r.store }

// Seconds reports the covered audio length.
func (r *Recaller) Seconds() float64 { return r.ring.Seconds() }

// Saves counts how many candidates this recaller wrote.
func (r *Recaller) Saves() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.saves
}

// OnSave installs a callback invoked (outside the recaller's lock) after a
// candidate was written. It is how the CLI prints its one-line notice and how
// the HTTP layer publishes an SSE event.
func (r *Recaller) OnSave(fn func(Candidate)) {
	r.mu.Lock()
	r.onSave = fn
	r.mu.Unlock()
}

// PushBlock feeds one canonical 48 kHz mono float32 block into the ring. It is
// registered as live.Options.OnBlock, so it runs on the recognition goroutine
// and must never block: it does one int16 conversion and one slice copy under a
// short mutex.
func (r *Recaller) PushBlock(block []float32) {
	if r == nil || len(block) == 0 {
		return
	}
	pcm := make([]int16, len(block))
	for i, v := range block {
		// Full scale is 32768, which is the convention the realtime source uses
		// on the way in (streamConverter divides by 32768). Keeping the same
		// scale here makes the float32 -> int16 -> float32 round trip exact for
		// every int16 value, so a recall WAV is bit-identical to what a capture
		// of the same audio would have written.
		f := float64(v) * 32768
		if f >= 32767 {
			pcm[i] = 32767
		} else if f <= -32768 {
			pcm[i] = -32768
		} else {
			pcm[i] = int16(math.Round(f))
		}
	}
	r.ring.Push(pcm)
}

// Tick records the current ranking so the next saved candidate carries a guess.
// top must be sorted best-first (the order internal/index returns).
func (r *Recaller) Tick(top []index.ItemScore) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(top) == 0 {
		r.guessID, r.guessName, r.guessScore = "", "", 0
		return
	}
	best := top[0]
	r.guessID, r.guessName, r.guessScore = best.ID, best.Name, best.Score.Float()
}

// Guess returns the last recorded top-1 (empty id when there is no ranking yet).
func (r *Recaller) Guess() (id, name string, score float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.guessID, r.guessName, r.guessScore
}

// Save snapshots the ring and writes it as a candidate. It returns the candidate
// and a nil error; an empty ring is a normal condition and yields an error the
// caller can report verbatim.
//
// source is recorded on the candidate ("hotkey", "cli", "api", "overlay").
func (r *Recaller) Save(source string) (Candidate, error) {
	if r == nil || r.store == nil {
		return Candidate{}, errors.New("回溯保存未初始化")
	}
	r.mu.Lock()
	id, name, score := r.guessID, r.guessName, r.guessScore
	cb := r.onSave
	r.mu.Unlock()

	pcm := r.ring.Snapshot()
	if len(pcm) == 0 {
		return Candidate{}, errors.New("环形缓冲里还没有音频（识别链路刚启动或采集端点没有声音）")
	}
	cand, err := r.store.Add(pcm, CandidateMeta{
		GuessID: id, GuessName: name, GuessScore: score, Source: source,
	})
	if err != nil {
		return Candidate{}, err
	}
	// Retention is applied right after every save, so "maxFiles" is a hard
	// ceiling rather than something a later maintenance pass has to fix.
	if _, err := r.store.Prune(); err != nil {
		// A failed prune must not fail the save: the candidate IS on disk, and
		// the next save (or an explicit Prune) retries.
		_ = err
	}
	r.mu.Lock()
	r.saves++
	r.mu.Unlock()
	if cb != nil {
		cb(cand)
	}
	return cand, nil
}

// Describe renders the one-line notice the CLI prints after a save:
//
//	[recall] 已保存候选项 000004-20260101-120000（3.0 秒，峰值 -4.5 dBFS，猜测：880Hz 测试音 0.98）
func Describe(cand Candidate) string {
	level := wav.FormatDBFS(cand.PeakDBFS)
	if cand.Silent {
		level = "-inf"
	}
	out := fmt.Sprintf("%s（%.1f 秒，峰值 %s dBFS", cand.ID, cand.Seconds, level)
	if cand.GuessID != "" {
		out += fmt.Sprintf("，猜测：%s %.2f", cand.GuessName, cand.GuessScore)
	} else {
		out += "，猜测：无"
	}
	return out + "）"
}

// ---------------------------------------------------------------------------
// small helpers
// ---------------------------------------------------------------------------

func round2(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return math.Round(v*100) / 100
}

func round4(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return math.Round(v*10000) / 10000
}
