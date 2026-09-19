package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/znz/soundradar/internal/library"
	"github.com/znz/soundradar/internal/recall"
)

// ---------------------------------------------------------------------------
// fixtures: a server with a real recall Store behind the P4 sink
// ---------------------------------------------------------------------------

// fakeRecall is a RecallSink over a real recall.Store, so the HTTP tests
// exercise the real file layout (WAV + JSON, MarkUsed/Delete, ids and all)
// without needing a sound card or a hotkey.
type fakeRecall struct {
	store   *recall.Store
	rec     *recall.Recaller
	enabled bool
	dir     string
	ensured int
	ensureE error
}

func newFakeRecall(t *testing.T, maxFiles int) *fakeRecall {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "candidates")
	store, err := recall.NewStore(dir, maxFiles)
	if err != nil {
		t.Fatalf("recall.NewStore: %v", err)
	}
	r := &fakeRecall{
		store:   store,
		rec:     recall.NewRecaller(3, store),
		enabled: true,
		dir:     dir,
	}
	return r
}

// buffer fills the ring with a deterministic 880 Hz tone, the same signal the
// acceptance run plays through the speakers.
func (f *fakeRecall) buffer(seconds float64) {
	n := int(seconds * recall.SampleRate)
	for off := 0; off < n; off += 960 {
		end := off + 960
		if end > n {
			end = n
		}
		blk := make([]float32, end-off)
		for i := range blk {
			t := float64(off+i) / recall.SampleRate
			blk[i] = float32(0.6 * sin2pi(880, t))
		}
		f.rec.PushBlock(blk)
	}
}

func sin2pi(hz float64, t float64) float64 {
	return sin(2 * 3.141592653589793 * hz * t)
}

func sin(x float64) float64 {
	// A tiny local sin keeps the test file dependency-free of math while still
	// producing a real signal (and it is exact enough for a level check).
	if x == 0 {
		return 0
	}
	// Taylor series around 0 after range reduction to [-pi, pi].
	const pi = 3.141592653589793
	for x > pi {
		x -= 2 * pi
	}
	for x < -pi {
		x += 2 * pi
	}
	x2 := x * x
	term := x
	sum := x
	for k := 3; k < 18; k += 2 {
		term = -term * x2 / float64(k*(k-1))
		sum += term
	}
	return sum
}

func (f *fakeRecall) Enabled() bool { return f.enabled }

func (f *fakeRecall) Save(source string) (recall.Candidate, error) {
	if !f.enabled {
		return recall.Candidate{}, fmt.Errorf("未启用")
	}
	if source == "" {
		source = "api"
	}
	return f.rec.Save(source)
}

func (f *fakeRecall) Trigger() error {
	_, err := f.Save("hotkey")
	return err
}

func (f *fakeRecall) List() ([]recall.Candidate, error) { return f.store.List() }
func (f *fakeRecall) Get(id string) (recall.Candidate, error) {
	return f.store.Get(id)
}
func (f *fakeRecall) WAVBytes(id string) ([]byte, error) { return f.store.Open(id) }
func (f *fakeRecall) Delete(id string) error             { return f.store.Delete(id) }
func (f *fakeRecall) Dir() string                        { return f.dir }
func (f *fakeRecall) RingSeconds() float64               { return 3 }
func (f *fakeRecall) CoverSeconds() float64              { return f.rec.Seconds() }
func (f *fakeRecall) HotkeyStatus() string {
	return "F8（RegisterHotKey 成功，回调 → 保存最近 3.0 秒）"
}
func (f *fakeRecall) MaxFiles() int { return f.store.MaxFiles() }
func (f *fakeRecall) EnsureCapture() error {
	f.ensured++
	return f.ensureE
}

// newRecallServer builds a Server over a fresh library and installs sink.
func newRecallServer(t *testing.T, sink RecallSink) *Server {
	t.Helper()
	libPath := filepath.Join(t.TempDir(), "library.srz")
	store, err := library.Open(libPath)
	if err != nil {
		t.Fatalf("library.Open: %v", err)
	}
	s := New(Options{Store: store, Logger: testLogger(t)})
	if sink != nil {
		s.SetRecall(sink)
	}
	return s
}

// ---------------------------------------------------------------------------
// tests
// ---------------------------------------------------------------------------

// TestCandidatesTriggerListPlayDelete walks the whole inbox API.
func TestCandidatesTriggerListPlayDelete(t *testing.T) {
	sink := newFakeRecall(t, 20)
	sink.buffer(3)
	srv := newRecallServer(t, sink)

	// 1. POST /api/recall/trigger saves a candidate.
	rec := doJSON(t, srv, http.MethodPost, "/api/recall/trigger", `{"source":"api"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/recall/trigger -> %d, body=%s", rec.Code, rec.Body.String())
	}
	var created struct {
		OK        bool         `json:"ok"`
		Candidate CandidateDTO `json:"candidate"`
		Count     int          `json:"count"`
	}
	decode(t, rec.Body.Bytes(), &created)
	if !created.OK || created.Candidate.ID == "" {
		t.Fatalf("trigger 响应不完整: %s", rec.Body.String())
	}
	if created.Count != 1 {
		t.Fatalf("count=%d，期望 1", created.Count)
	}
	if created.Candidate.WAVURL != "/api/candidates/"+created.Candidate.ID+".wav" {
		t.Fatalf("wavUrl=%q", created.Candidate.WAVURL)
	}
	if created.Candidate.PeakDBFS == nil {
		t.Fatalf("peakDbfs 为 null（非静音候选项不该是 null）")
	}
	if got := *created.Candidate.PeakDBFS; got > -3 || got < -8 {
		t.Fatalf("peakDbfs=%.2f，880Hz@0.6 应在 -6..-3 dBFS 附近", got)
	}
	if sink.ensured == 0 {
		t.Fatalf("trigger 没有调用 EnsureCapture（serve 里环形缓冲就没人填）")
	}

	// 2. GET /api/candidates lists it.
	list := doJSON(t, srv, http.MethodGet, "/api/candidates", "")
	if list.Code != http.StatusOK {
		t.Fatalf("GET /api/candidates -> %d", list.Code)
	}
	var listing candidatesDTO
	decode(t, list.Body.Bytes(), &listing)
	if !listing.Available || listing.Count != 1 || len(listing.Items) != 1 {
		t.Fatalf("列表不对: %+v", listing)
	}
	if listing.Items[0].ID != created.Candidate.ID {
		t.Fatalf("列表里的 id 不对: %s", listing.Items[0].ID)
	}
	if !listing.HotkeyOK || listing.Hotkey != "F8" {
		t.Fatalf("热键状态解析失败: ok=%v spec=%q status=%q", listing.HotkeyOK, listing.Hotkey, listing.Status)
	}

	// 3. GET /api/candidates/{id}.wav returns a real RIFF file, Range aware.
	wavRec := doJSON(t, srv, http.MethodGet, "/api/candidates/"+created.Candidate.ID+".wav", "")
	if wavRec.Code != http.StatusOK {
		t.Fatalf("GET wav -> %d, body=%s", wavRec.Code, wavRec.Body.String())
	}
	body := wavRec.Body.Bytes()
	if len(body) < 4 || string(body[:4]) != "RIFF" {
		t.Fatalf("前 4 字节不是 RIFF: %q", body[:min(4, len(body))])
	}
	if ct := wavRec.Header().Get("Content-Type"); ct != "audio/wav" {
		t.Fatalf("Content-Type=%q", ct)
	}
	rng := doJSONHeader(t, srv, http.MethodGet, "/api/candidates/"+created.Candidate.ID+".wav", "", map[string]string{"Range": "bytes=0-99"})
	if rng.Code != http.StatusPartialContent {
		t.Fatalf("Range 请求 -> %d，期望 206", rng.Code)
	}
	if rng.Body.Len() != 100 {
		t.Fatalf("Range 返回 %d 字节，期望 100", rng.Body.Len())
	}

	// 4. GET missing id -> 404 JSON with an error message.
	missing := doJSON(t, srv, http.MethodGet, "/api/candidates/000999-20200101-000000.wav", "")
	if missing.Code != http.StatusNotFound {
		t.Fatalf("不存在的候选项 -> %d，期望 404", missing.Code)
	}
	var errBody map[string]string
	decode(t, missing.Body.Bytes(), &errBody)
	if errBody["error"] == "" {
		t.Fatalf("404 响应没有 error 字段: %s", missing.Body.String())
	}

	// 5. DELETE removes it.
	del := doJSON(t, srv, http.MethodDelete, "/api/candidates/"+created.Candidate.ID, "")
	if del.Code != http.StatusOK {
		t.Fatalf("DELETE -> %d, body=%s", del.Code, del.Body.String())
	}
	after := doJSON(t, srv, http.MethodGet, "/api/candidates", "")
	decode(t, after.Body.Bytes(), &listing)
	if listing.Count != 0 {
		t.Fatalf("删除后 count=%d", listing.Count)
	}
}

// TestCandidatesUnavailableIsHonest pins the "no sink" behaviour: the page must
// still load and say why.
func TestCandidatesUnavailableIsHonest(t *testing.T) {
	srv := newRecallServer(t, nil)

	list := doJSON(t, srv, http.MethodGet, "/api/candidates", "")
	if list.Code != http.StatusOK {
		t.Fatalf("GET /api/candidates -> %d", list.Code)
	}
	var listing candidatesDTO
	decode(t, list.Body.Bytes(), &listing)
	if listing.Available {
		t.Fatalf("没有 sink 时 available 应为 false")
	}
	if listing.Note == "" {
		t.Fatalf("没有 sink 时必须给出说明")
	}
	if listing.Items == nil {
		t.Fatalf("items 必须是 []（不能是 null）")
	}

	trig := doJSON(t, srv, http.MethodPost, "/api/recall/trigger", "")
	if trig.Code != http.StatusConflict {
		t.Fatalf("没有 sink 时 trigger -> %d，期望 409", trig.Code)
	}
}

// TestPromoteCreatesItem is the core P4 loop: candidate -> library item.
func TestPromoteCreatesItem(t *testing.T) {
	sink := newFakeRecall(t, 20)
	sink.buffer(3)
	srv := newRecallServer(t, sink)

	trig := doJSON(t, srv, http.MethodPost, "/api/recall/trigger", "")
	var created struct {
		Candidate CandidateDTO `json:"candidate"`
	}
	decode(t, trig.Body.Bytes(), &created)
	id := created.Candidate.ID
	if id == "" {
		t.Fatalf("没有候选项: %s", trig.Body.String())
	}

	// Promote with an uploaded icon (multipart, the browser path).
	body, ctype := promoteMultipart(t, map[string]any{
		"name": "880Hz 回溯测试音",
		"tags": []string{"p4", "回溯"},
		"note": "来自回溯保存",
	}, map[string]struct {
		Name string
		Data []byte
	}{"icon": {"icon.png", makePNG(t, 64, 64)}})
	req := httptest.NewRequest(http.MethodPost, "/api/candidates/"+id+"/promote", body)
	req.Header.Set("Content-Type", ctype)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("promote -> %d, body=%s", w.Code, w.Body.String())
	}
	var promoted promoteResponse
	decode(t, w.Body.Bytes(), &promoted)
	if !promoted.OK || promoted.Action != "created" || promoted.ItemID == "" {
		t.Fatalf("promote 响应不对: %s", w.Body.String())
	}
	if !promoted.CandidateConsumed {
		t.Fatalf("promote 成功后候选项应被消费")
	}

	// The item is in the library with exactly one sample.
	lib := doJSON(t, srv, http.MethodGet, "/api/library", "")
	var listing LibraryDTO
	decode(t, lib.Body.Bytes(), &listing)
	found := false
	for _, it := range listing.Items {
		if it.ID == promoted.ItemID {
			found = true
			if it.Name != "880Hz 回溯测试音" {
				t.Fatalf("条目名 %q", it.Name)
			}
			if it.SampleCount != 1 {
				t.Fatalf("样本数 %d，期望 1", it.SampleCount)
			}
		}
	}
	if !found {
		t.Fatalf("新条目 %s 不在 /api/library 里", promoted.ItemID)
	}
	if listing.SampleCount != 1 || listing.ItemCount != 1 {
		t.Fatalf("库统计不对: items=%d samples=%d", listing.ItemCount, listing.SampleCount)
	}

	// Sample 1 is a real RIFF WAV.
	wavRec := doJSON(t, srv, http.MethodGet, "/api/items/"+promoted.ItemID+"/samples/1.wav", "")
	if wavRec.Code != http.StatusOK {
		t.Fatalf("样本回放 -> %d", wavRec.Code)
	}
	if b := wavRec.Body.Bytes(); len(b) < 4 || string(b[:4]) != "RIFF" {
		t.Fatalf("样本前 4 字节不是 RIFF")
	}

	// The candidate is gone from the inbox.
	after := doJSON(t, srv, http.MethodGet, "/api/candidates", "")
	var afterList candidatesDTO
	decode(t, after.Body.Bytes(), &afterList)
	if afterList.Count != 0 {
		t.Fatalf("候选项已被消费，收件箱应为空，实际 %d", afterList.Count)
	}
	if _, err := sink.store.Get(id); err == nil {
		t.Fatalf("候选项文件应该已被删除")
	}

	// The new sample carries the P1 origin traceability (source=recall).
	detail := doJSON(t, srv, http.MethodGet, "/api/items/"+promoted.ItemID, "")
	var item ItemDetailDTO
	decode(t, detail.Body.Bytes(), &item)
	if len(item.Samples) != 1 {
		t.Fatalf("samples=%d", len(item.Samples))
	}
	sm := item.Samples[0]
	if sm.Source != "recall" {
		t.Fatalf("sample.source=%q，期望 recall", sm.Source)
	}
	if sm.StoredRate != 48000 || sm.StoredChans != 1 || sm.StoredBits != 16 {
		t.Fatalf("入库格式不是 48k/mono/16-bit: %d/%d/%d", sm.StoredRate, sm.StoredChans, sm.StoredBits)
	}
	if sm.Origin.SampleRate != 48000 || sm.Origin.Container != "wav" {
		t.Fatalf("origin 没有留痕: %+v", sm.Origin)
	}
	if sm.PeakDBFS == nil {
		t.Fatalf("样本没有峰值")
	}
	if item.Tags == nil || len(item.Tags) != 2 {
		t.Fatalf("标签没有入库: %v", item.Tags)
	}
}

// TestPromoteAppendsToExistingItem covers the targetItemId path.
func TestPromoteAppendsToExistingItem(t *testing.T) {
	sink := newFakeRecall(t, 20)
	srv := newRecallServer(t, sink)

	// Build one item the normal way (upload path).
	item := uploadItem(t, srv, "目标条目")
	if len(item.Samples) != 1 {
		t.Fatalf("新条目样本数 %d", len(item.Samples))
	}

	// Save a candidate and append it.
	sink.buffer(3)
	trig := doJSON(t, srv, http.MethodPost, "/api/recall/trigger", "")
	var created struct {
		Candidate CandidateDTO `json:"candidate"`
	}
	decode(t, trig.Body.Bytes(), &created)

	body, _ := json.Marshal(map[string]any{"targetItemId": item.ID})
	rec := doJSON(t, srv, http.MethodPost, "/api/candidates/"+created.Candidate.ID+"/promote", string(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("追加 promote -> %d, body=%s", rec.Code, rec.Body.String())
	}
	var promoted promoteResponse
	decode(t, rec.Body.Bytes(), &promoted)
	if promoted.Action != "appended" || promoted.ItemID != item.ID {
		t.Fatalf("promote 响应不对: %s", rec.Body.String())
	}

	detail := doJSON(t, srv, http.MethodGet, "/api/items/"+item.ID, "")
	var after ItemDetailDTO
	decode(t, detail.Body.Bytes(), &after)
	if len(after.Samples) != 2 {
		t.Fatalf("追加后样本数 %d，期望 2", len(after.Samples))
	}
	if after.Samples[1].Source != "recall" {
		t.Fatalf("追加的样本 source=%q", after.Samples[1].Source)
	}

	// An unknown target is a clean 404, and the candidate must survive it.
	// (A second candidate is saved because the first one was already consumed by
	// the successful append above.)
	sink.buffer(1)
	trig2 := doJSON(t, srv, http.MethodPost, "/api/recall/trigger", "")
	var created2 struct {
		Candidate CandidateDTO `json:"candidate"`
	}
	decode(t, trig2.Body.Bytes(), &created2)
	body2, _ := json.Marshal(map[string]any{"targetItemId": "deadbeef"})
	bad := doJSON(t, srv, http.MethodPost, "/api/candidates/"+created2.Candidate.ID+"/promote", string(body2))
	if bad.Code != http.StatusNotFound {
		t.Fatalf("不存在的 targetItemId -> %d，期望 404", bad.Code)
	}
	if _, err := sink.store.Get(created2.Candidate.ID); err != nil {
		t.Fatalf("失败的 promote 不该消费候选项: %v", err)
	}
}

// TestPromoteBorrowsIconFromExistingItem covers "icon": "<existing item id>".
func TestPromoteBorrowsIconFromExistingItem(t *testing.T) {
	sink := newFakeRecall(t, 20)
	sink.buffer(2)
	srv := newRecallServer(t, sink)

	up := uploadItem(t, srv, "图标来源")
	src := up

	trig := doJSON(t, srv, http.MethodPost, "/api/recall/trigger", "")
	var created struct {
		Candidate CandidateDTO `json:"candidate"`
	}
	decode(t, trig.Body.Bytes(), &created)

	body, _ := json.Marshal(map[string]any{"name": "借用图标", "icon": src.ID})
	rec := doJSON(t, srv, http.MethodPost, "/api/candidates/"+created.Candidate.ID+"/promote", string(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("借用图标 promote -> %d, body=%s", rec.Code, rec.Body.String())
	}
	var promoted promoteResponse
	decode(t, rec.Body.Bytes(), &promoted)

	icon := doJSON(t, srv, http.MethodGet, "/api/items/"+promoted.ItemID+"/icon.png", "")
	if icon.Code != http.StatusOK || !bytes.HasPrefix(icon.Body.Bytes(), []byte("\x89PNG")) {
		t.Fatalf("借来的图标不是 PNG: %d", icon.Code)
	}

	// A bad icon id must not create anything. A second candidate is saved because
	// the first one was consumed by the successful promote above.
	sink.buffer(1)
	trig2 := doJSON(t, srv, http.MethodPost, "/api/recall/trigger", "")
	var created2 struct {
		Candidate CandidateDTO `json:"candidate"`
	}
	decode(t, trig2.Body.Bytes(), &created2)
	body2, _ := json.Marshal(map[string]any{"name": "x", "icon": "nosuchid"})
	bad := doJSON(t, srv, http.MethodPost, "/api/candidates/"+created2.Candidate.ID+"/promote", string(body2))
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("不存在的 icon id -> %d（body=%s），期望 400", bad.Code, bad.Body.String())
	}
	if _, err := sink.store.Get(created2.Candidate.ID); err != nil {
		t.Fatalf("失败的 promote 不该消费候选项: %v", err)
	}
	// 库里只能有第一次成功创建的那一个条目。
	lib := doJSON(t, srv, http.MethodGet, "/api/library", "")
	var listing LibraryDTO
	decode(t, lib.Body.Bytes(), &listing)
	if listing.ItemCount != 2 {
		t.Fatalf("失败的 promote 不该建条目：ItemCount=%d，期望 2（图标来源 + 借用图标）", listing.ItemCount)
	}
}

// TestPromoteDefaultsNameAndRejectsBadIDs pins the edge cases.
func TestPromoteDefaultsNameAndRejectsBadIDs(t *testing.T) {
	sink := newFakeRecall(t, 20)
	sink.buffer(1)
	srv := newRecallServer(t, sink)

	trig := doJSON(t, srv, http.MethodPost, "/api/recall/trigger", "")
	var created struct {
		Candidate CandidateDTO `json:"candidate"`
	}
	decode(t, trig.Body.Bytes(), &created)

	// No name at all: the endpoint still succeeds with a timestamped name.
	rec := doJSON(t, srv, http.MethodPost, "/api/candidates/"+created.Candidate.ID+"/promote", `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("空名称 promote -> %d, body=%s", rec.Code, rec.Body.String())
	}
	var promoted promoteResponse
	decode(t, rec.Body.Bytes(), &promoted)
	if !strings.HasPrefix(promoted.Item.Name, "候选项") {
		t.Fatalf("默认名称不对: %q", promoted.Item.Name)
	}

	// A path-traversal id must be rejected before touching the filesystem.
	for _, bad := range []string{"..%2f..%2fetc", "a..b", "abc$def"} {
		r := doJSON(t, srv, http.MethodDelete, "/api/candidates/"+bad, "")
		if r.Code != http.StatusBadRequest && r.Code != http.StatusNotFound {
			t.Fatalf("非法 id %q -> %d，期望 400/404", bad, r.Code)
		}
	}
}

// TestRecallStatusEndpoint checks GET /api/recall.
func TestRecallStatusEndpoint(t *testing.T) {
	sink := newFakeRecall(t, 7)
	srv := newRecallServer(t, sink)

	rec := doJSON(t, srv, http.MethodGet, "/api/recall", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/recall -> %d", rec.Code)
	}
	var st map[string]any
	decode(t, rec.Body.Bytes(), &st)
	if st["available"] != true {
		t.Fatalf("available=%v", st["available"])
	}
	if st["hotkey"] != "F8" {
		t.Fatalf("hotkey=%v", st["hotkey"])
	}
	if st["hotkeyRegistered"] != true {
		t.Fatalf("hotkeyRegistered=%v", st["hotkeyRegistered"])
	}
	if got, _ := st["maxFiles"].(float64); int(got) != 7 {
		t.Fatalf("maxFiles=%v，期望 7", st["maxFiles"])
	}
}

// TestSilentCandidateIsJSONSafe pins the -Inf lesson at the API boundary.
func TestSilentCandidateIsJSONSafe(t *testing.T) {
	sink := newFakeRecall(t, 7)
	// Fill the ring with pure silence.
	silence := make([]float32, 3*recall.SampleRate)
	sink.rec.PushBlock(silence)
	srv := newRecallServer(t, sink)

	rec := doJSON(t, srv, http.MethodPost, "/api/recall/trigger", "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("静音 trigger -> %d, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, bad := range []string{"Inf", "NaN"} {
		if strings.Contains(body, bad) {
			t.Fatalf("静音候选项响应含 %q: %s", bad, body)
		}
	}
	var created struct {
		Candidate CandidateDTO `json:"candidate"`
	}
	decode(t, rec.Body.Bytes(), &created)
	if !created.Candidate.Silent {
		t.Fatalf("全零音频应报告 silent=true")
	}
	if created.Candidate.PeakDBFS != nil {
		t.Fatalf("静音候选项的 peakDbfs 应为 null，得到 %v", *created.Candidate.PeakDBFS)
	}

	list := doJSON(t, srv, http.MethodGet, "/api/candidates", "")
	if strings.Contains(list.Body.String(), "Inf") {
		t.Fatalf("列表响应含 Inf: %s", list.Body.String())
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func testLogger(t *testing.T) *log.Logger {
	t.Helper()
	return log.New(io.Discard, "", 0)
}

func decode(t *testing.T, raw []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("解析响应失败: %v\n%s", err, raw)
	}
}

func doJSON(t *testing.T, srv *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doJSONHeader(t, srv, method, path, body, nil)
}

func doJSONHeader(t *testing.T, srv *Server, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

// promoteMultipart builds a multipart body with a "promote" JSON field plus
// optional files, which is what the management page sends.
func promoteMultipart(t *testing.T, patch map[string]any, files map[string]struct {
	Name string
	Data []byte
}) (io.Reader, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	raw, err := json.Marshal(patch)
	if err != nil {
		t.Fatal(err)
	}
	if err := mw.WriteField("promote", string(raw)); err != nil {
		t.Fatal(err)
	}
	for field, f := range files {
		w, err := mw.CreateFormFile(field, f.Name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(f.Data); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf, mw.FormDataContentType()
}

// multilineTextBody is gone; uploadItem is the one way tests create an item.

// uploadItem POSTs a real 880 Hz WAV through the P1 endpoint and returns the
// created item.
func uploadItem(t *testing.T, srv *Server, name string) ItemDetailDTO {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("name", name); err != nil {
		t.Fatal(err)
	}
	w, err := mw.CreateFormFile("audio", "tone.wav")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(makeWAV(t, 48000, 1, 0.5, 880, 0.6)); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/items", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("上传条目 -> %d, body=%s", rec.Code, rec.Body.String())
	}
	var item ItemDetailDTO
	decode(t, rec.Body.Bytes(), &item)
	if item.ID == "" {
		t.Fatalf("上传后没有 id: %s", rec.Body.String())
	}
	return item
}
