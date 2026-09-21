package server

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/znz/soundradar/internal/library"
	"github.com/znz/soundradar/internal/testaudio"
	"github.com/znz/soundradar/internal/wav"
)

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

func makeWAV(t *testing.T, rate, chans int, seconds, freq, amp float64) []byte {
	t.Helper()
	n := int(seconds * float64(rate))
	pcm := make([]int16, n*chans)
	for i := 0; i < n; i++ {
		v := wav.ClampInt16(amp * math.Sin(2*math.Pi*freq*float64(i)/float64(rate)))
		for c := 0; c < chans; c++ {
			pcm[i*chans+c] = v
		}
	}
	var buf bytes.Buffer
	if err := wav.WriteInt16(&buf, rate, chans, pcm); err != nil {
		t.Fatalf("WriteInt16: %v", err)
	}
	return buf.Bytes()
}

func makePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: 20, G: uint8(x % 255), B: uint8(y % 255), A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	return buf.Bytes()
}

// multipartBody builds a multipart/form-data body; files maps field name to
// (filename, content), values maps field name to value.
func multipartBody(t *testing.T, files map[string]struct {
	Name string
	Data []byte
}, values map[string]string) (io.Reader, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range values {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatal(err)
		}
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

func newTestServer(t *testing.T) (*httptest.Server, *library.Store) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "data", "library.srz")
	st, err := library.Open(path)
	if err != nil {
		t.Fatalf("library.Open: %v", err)
	}
	return newTestServerWithStore(t, st), st
}

// newTestServerWithStore wraps an existing store in an httptest server.
func newTestServerWithStore(t *testing.T, st *library.Store) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(New(Options{Store: st}).Handler())
	t.Cleanup(ts.Close)
	return ts
}

// silentMP3Fixture returns a structurally valid silent MPEG-1 Layer III stream.
func silentMP3Fixture(t *testing.T) []byte {
	t.Helper()
	return testaudio.SilentMP3(20)
}

func getJSON(t *testing.T, url string, out any) int {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer res.Body.Close()
	if out != nil {
		if err := json.NewDecoder(res.Body).Decode(out); err != nil {
			t.Fatalf("decoding %s: %v", url, err)
		}
	}
	return res.StatusCode
}

// ---------------------------------------------------------------------------
// happy path
// ---------------------------------------------------------------------------

func TestAPIEndToEnd(t *testing.T) {
	ts, _ := newTestServer(t)

	// --- POST /api/items --------------------------------------------------
	body, ctype := multipartBody(t,
		map[string]struct {
			Name string
			Data []byte
		}{
			"audio": {Name: "beep.wav", Data: makeWAV(t, 44100, 2, 0.5, 880, 0.7)},
			"icon":  {Name: "icon.png", Data: makePNG(t, 40, 90)},
		},
		map[string]string{
			"name":       "测试音效",
			"tags":       "测试,UI",
			"note":       "端到端测试样本",
			"threshold":  "0.81",
			"cooldownMs": "250",
			"profile":    "default",
		})
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/items", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", ctype)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /api/items: %v", err)
	}
	raw, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/items status = %d, body = %s", res.StatusCode, raw)
	}
	var created ItemDetailDTO
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatalf("decoding created item: %v (body %s)", err, raw)
	}
	t.Logf("created id=%s name=%q samples=%d threshold=%v", created.ID, created.Name, created.SampleCount, created.Threshold)
	if len(created.ID) != 8 {
		t.Fatalf("id = %q", created.ID)
	}
	if created.Name != "测试音效" || created.Threshold != 0.81 || created.CooldownMs != 250 {
		t.Fatalf("created = %+v", created.ItemDTO)
	}
	if strings.Join(created.Tags, ",") != "测试,UI" {
		t.Fatalf("tags = %v", created.Tags)
	}
	if created.SampleCount != 1 || len(created.Samples) != 1 {
		t.Fatalf("samples = %+v", created.Samples)
	}
	s0 := created.Samples[0]
	if s0.WAVRate != 48000 || s0.WAVChans != 1 || s0.WAVBits != 16 {
		t.Fatalf("stored format = %d/%d/%d", s0.WAVRate, s0.WAVChans, s0.WAVBits)
	}
	if s0.Origin.SampleRate != 44100 || s0.Origin.Channels != 2 || !s0.Origin.Resampled || !s0.Origin.Downmixed {
		t.Fatalf("origin not recorded: %+v", s0.Origin)
	}
	if s0.Frames != 24000 {
		t.Fatalf("stored frames = %d, want 24000 (0.5 s at 48 kHz)", s0.Frames)
	}

	// --- GET /api/library -------------------------------------------------
	var lib LibraryDTO
	if code := getJSON(t, ts.URL+"/api/library", &lib); code != http.StatusOK {
		t.Fatalf("GET /api/library status = %d", code)
	}
	if lib.ItemCount != 1 || lib.SampleCount != 1 {
		t.Fatalf("library counts = %d/%d", lib.ItemCount, lib.SampleCount)
	}
	if len(lib.Items) != 1 || lib.Items[0].ID != created.ID {
		t.Fatalf("items = %+v", lib.Items)
	}
	if lib.FileBytes <= 0 || lib.Schema != library.Schema {
		t.Fatalf("library meta = %+v", lib)
	}
	if lib.Feature.Kind == "" || lib.Feature.SampleRate != 48000 {
		t.Fatalf("feature placeholder missing: %+v", lib.Feature)
	}
	if lib.Bits != 16 || lib.Channels != 1 {
		t.Fatalf("canonical format advertised as %d/%d/%d", lib.SampleRate, lib.Channels, lib.Bits)
	}
	t.Logf("library: path=%s items=%d samples=%d bytes=%d", lib.Path, lib.ItemCount, lib.SampleCount, lib.FileBytes)

	// --- GET /api/items/{id} ---------------------------------------------
	var detail ItemDetailDTO
	if code := getJSON(t, ts.URL+"/api/items/"+created.ID, &detail); code != http.StatusOK {
		t.Fatalf("GET item status = %d", code)
	}
	if detail.Name != created.Name || len(detail.Samples) != 1 {
		t.Fatalf("detail = %+v", detail)
	}

	// --- GET sample WAV ---------------------------------------------------
	wavRes, err := http.Get(ts.URL + fmt.Sprintf("/api/items/%s/samples/1.wav", created.ID))
	if err != nil {
		t.Fatal(err)
	}
	wavBody, _ := io.ReadAll(wavRes.Body)
	wavRes.Body.Close()
	if wavRes.StatusCode != http.StatusOK {
		t.Fatalf("GET sample status = %d", wavRes.StatusCode)
	}
	if len(wavBody) < 44 || string(wavBody[0:4]) != "RIFF" || string(wavBody[8:12]) != "WAVE" {
		t.Fatalf("sample is not a RIFF/WAVE file (first bytes %q)", wavBody[:min(12, len(wavBody))])
	}
	if got := wavRes.Header.Get("Accept-Ranges"); got != "bytes" {
		t.Fatalf("Accept-Ranges = %q", got)
	}
	audio, err := wav.Read(bytes.NewReader(wavBody))
	if err != nil {
		t.Fatalf("re-reading served wav: %v", err)
	}
	if audio.Info.SampleRate != 48000 || audio.Info.Channels != 1 {
		t.Fatalf("served wav format = %s", audio.Info.Layout())
	}
	t.Logf("served sample: %d bytes, %s, %d frames", len(wavBody), audio.Info.Layout(), audio.Info.Frames)

	// --- Range request ----------------------------------------------------
	rreq, _ := http.NewRequest(http.MethodGet, ts.URL+fmt.Sprintf("/api/items/%s/samples/1.wav", created.ID), nil)
	rreq.Header.Set("Range", "bytes=0-99")
	rres, err := http.DefaultClient.Do(rreq)
	if err != nil {
		t.Fatal(err)
	}
	part, _ := io.ReadAll(rres.Body)
	rres.Body.Close()
	if rres.StatusCode != http.StatusPartialContent {
		t.Fatalf("Range status = %d, want 206", rres.StatusCode)
	}
	if len(part) != 100 || !bytes.Equal(part, wavBody[:100]) {
		t.Fatalf("Range body is %d bytes (want 100, matching prefix)", len(part))
	}
	t.Logf("range request: status=%d bytes=%d content-range=%q",
		rres.StatusCode, len(part), rres.Header.Get("Content-Range"))

	// --- GET icon ---------------------------------------------------------
	iconRes, err := http.Get(ts.URL + "/api/items/" + created.ID + "/icon.png")
	if err != nil {
		t.Fatal(err)
	}
	iconBody, _ := io.ReadAll(iconRes.Body)
	iconRes.Body.Close()
	if iconRes.StatusCode != http.StatusOK || iconRes.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("icon status/type = %d/%s", iconRes.StatusCode, iconRes.Header.Get("Content-Type"))
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(iconBody))
	if err != nil {
		t.Fatalf("icon is not a PNG: %v", err)
	}
	if cfg.Width != library.IconSize || cfg.Height != library.IconSize {
		t.Fatalf("icon is %dx%d", cfg.Width, cfg.Height)
	}
	t.Logf("icon: %d bytes, %dx%d", len(iconBody), cfg.Width, cfg.Height)

	// --- PATCH (JSON) -----------------------------------------------------
	patch := `{"name":"改名后的音效","tags":["改名"],"note":"补丁","threshold":0.5,"cooldownMs":900,"profile":"aggressive"}`
	preq, _ := http.NewRequest(http.MethodPatch, ts.URL+"/api/items/"+created.ID, strings.NewReader(patch))
	preq.Header.Set("Content-Type", "application/json")
	pres, err := http.DefaultClient.Do(preq)
	if err != nil {
		t.Fatal(err)
	}
	pbody, _ := io.ReadAll(pres.Body)
	pres.Body.Close()
	if pres.StatusCode != http.StatusOK {
		t.Fatalf("PATCH status = %d, body = %s", pres.StatusCode, pbody)
	}
	var patched ItemDetailDTO
	if err := json.Unmarshal(pbody, &patched); err != nil {
		t.Fatal(err)
	}
	if patched.Name != "改名后的音效" || patched.Threshold != 0.5 || patched.CooldownMs != 900 || patched.Profile != "aggressive" {
		t.Fatalf("patched = %+v", patched.ItemDTO)
	}
	if len(patched.Samples) != 1 {
		t.Fatalf("patch lost the samples: %+v", patched.Samples)
	}

	// --- PATCH with a new icon (multipart) --------------------------------
	newIcon := makePNG(t, 16, 16)
	ibody, ictype := multipartBody(t,
		map[string]struct {
			Name string
			Data []byte
		}{"icon": {Name: "new.png", Data: newIcon}},
		map[string]string{"patch": `{"name":"带新图标"}`})
	ireq, _ := http.NewRequest(http.MethodPatch, ts.URL+"/api/items/"+created.ID, ibody)
	ireq.Header.Set("Content-Type", ictype)
	ires, err := http.DefaultClient.Do(ireq)
	if err != nil {
		t.Fatal(err)
	}
	ires.Body.Close()
	if ires.StatusCode != http.StatusOK {
		t.Fatalf("PATCH icon status = %d", ires.StatusCode)
	}
	iconRes2, _ := http.Get(ts.URL + "/api/items/" + created.ID + "/icon.png")
	iconBody2, _ := io.ReadAll(iconRes2.Body)
	iconRes2.Body.Close()
	if bytes.Equal(iconBody, iconBody2) {
		t.Fatalf("icon was not replaced")
	}

	// --- POST samples -----------------------------------------------------
	sbody, sctype := multipartBody(t,
		map[string]struct {
			Name string
			Data []byte
		}{"audio": {Name: "far.wav", Data: makeWAV(t, 48000, 1, 0.2, 440, 0.3)}},
		map[string]string{"offsetS": "0.05"})
	sreq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/items/"+created.ID+"/samples", sbody)
	sreq.Header.Set("Content-Type", sctype)
	sres, err := http.DefaultClient.Do(sreq)
	if err != nil {
		t.Fatal(err)
	}
	sraw, _ := io.ReadAll(sres.Body)
	sres.Body.Close()
	if sres.StatusCode != http.StatusCreated {
		t.Fatalf("POST samples status = %d, body = %s", sres.StatusCode, sraw)
	}
	var withTwo ItemDetailDTO
	if err := json.Unmarshal(sraw, &withTwo); err != nil {
		t.Fatal(err)
	}
	if len(withTwo.Samples) != 2 {
		t.Fatalf("samples = %d, want 2", len(withTwo.Samples))
	}
	if withTwo.Samples[1].OffsetS != 0.05 {
		t.Fatalf("offsetS = %v", withTwo.Samples[1].OffsetS)
	}
	if withTwo.Samples[1].Origin.SampleRate != 48000 || withTwo.Samples[1].Origin.Resampled {
		t.Fatalf("48k upload should not be marked resampled: %+v", withTwo.Samples[1].Origin)
	}

	// --- DELETE samples/{n} ----------------------------------------------
	dreq, _ := http.NewRequest(http.MethodDelete, ts.URL+fmt.Sprintf("/api/items/%s/samples/1", created.ID), nil)
	dres, err := http.DefaultClient.Do(dreq)
	if err != nil {
		t.Fatal(err)
	}
	dbody, _ := io.ReadAll(dres.Body)
	dres.Body.Close()
	if dres.StatusCode != http.StatusOK {
		t.Fatalf("DELETE sample status = %d, body = %s", dres.StatusCode, dbody)
	}
	var afterDelete ItemDetailDTO
	if err := json.Unmarshal(dbody, &afterDelete); err != nil {
		t.Fatal(err)
	}
	if len(afterDelete.Samples) != 1 || afterDelete.Samples[0].File != "samples/0001.wav" {
		t.Fatalf("after deleting sample 1: %+v", afterDelete.Samples)
	}

	// --- DELETE item ------------------------------------------------------
	del, _ := http.NewRequest(http.MethodDelete, ts.URL+"/api/items/"+created.ID, nil)
	delRes, err := http.DefaultClient.Do(del)
	if err != nil {
		t.Fatal(err)
	}
	delRes.Body.Close()
	if delRes.StatusCode != http.StatusOK {
		t.Fatalf("DELETE item status = %d", delRes.StatusCode)
	}
	var empty LibraryDTO
	getJSON(t, ts.URL+"/api/library", &empty)
	if empty.ItemCount != 0 {
		t.Fatalf("item survived deletion: %+v", empty.Items)
	}
}

// ---------------------------------------------------------------------------
// errors
// ---------------------------------------------------------------------------

func TestAPIUnsupportedAudioFormat(t *testing.T) {
	ts, _ := newTestServer(t)
	body, ctype := multipartBody(t,
		map[string]struct {
			Name string
			Data []byte
		}{"audio": {Name: "boom.ogg", Data: []byte("OggS\x00\x02\x00\x00 not really an ogg")}},
		map[string]string{"name": "ogg 测试"})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/items", body)
	req.Header.Set("Content-Type", ctype)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", res.StatusCode, raw)
	}
	if got := res.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("error response content type = %q", got)
	}
	var e struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatalf("error body is not JSON: %s", raw)
	}
	if !strings.Contains(e.Error, ".ogg") || !strings.Contains(e.Error, "暂不支持") {
		t.Fatalf("error message = %q", e.Error)
	}
	t.Logf("unsupported format -> %d %s", res.StatusCode, e.Error)
}

func TestAPIErrors(t *testing.T) {
	ts, _ := newTestServer(t)
	cases := []struct {
		name   string
		method string
		path   string
		body   func(t *testing.T) (io.Reader, string)
		want   int
	}{
		{"missing audio", http.MethodPost, "/api/items", func(t *testing.T) (io.Reader, string) {
			b, c := multipartBody(t, nil, map[string]string{"name": "无名"})
			return b, c
		}, http.StatusBadRequest},
		{"missing name", http.MethodPost, "/api/items", func(t *testing.T) (io.Reader, string) {
			b, c := multipartBody(t, map[string]struct {
				Name string
				Data []byte
			}{"audio": {Name: "a.wav", Data: makeWAV(t, 48000, 1, 0.1, 440, 0.5)}}, nil)
			return b, c
		}, http.StatusBadRequest},
		{"bad icon", http.MethodPost, "/api/items", func(t *testing.T) (io.Reader, string) {
			b, c := multipartBody(t, map[string]struct {
				Name string
				Data []byte
			}{
				"audio": {Name: "a.wav", Data: makeWAV(t, 48000, 1, 0.1, 440, 0.5)},
				"icon":  {Name: "icon.png", Data: []byte("not a png")},
			}, map[string]string{"name": "坏图标"})
			return b, c
		}, http.StatusBadRequest},
		{"unknown item", http.MethodGet, "/api/items/deadbeef", nil, http.StatusNotFound},
		{"unknown sample", http.MethodGet, "/api/items/deadbeef/samples/1.wav", nil, http.StatusNotFound},
		{"unknown route", http.MethodGet, "/api/nope", nil, http.StatusNotFound},
		{"bad method", http.MethodPut, "/api/library", nil, http.StatusMethodNotAllowed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var req *http.Request
			var err error
			if tc.body != nil {
				b, c := tc.body(t)
				req, err = http.NewRequest(tc.method, ts.URL+tc.path, b)
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Content-Type", c)
			} else {
				req, err = http.NewRequest(tc.method, ts.URL+tc.path, nil)
				if err != nil {
					t.Fatal(err)
				}
			}
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := io.ReadAll(res.Body)
			res.Body.Close()
			if res.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", res.StatusCode, tc.want, raw)
			}
			var e struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(raw, &e); err != nil || e.Error == "" {
				t.Fatalf("body is not {\"error\":...}: %s", raw)
			}
			t.Logf("%s -> %d %s", tc.name, res.StatusCode, e.Error)
		})
	}
}

// ---------------------------------------------------------------------------
// static UI
// ---------------------------------------------------------------------------

func TestStaticUI(t *testing.T) {
	ts, _ := newTestServer(t)
	for _, tc := range []struct{ path, wantType, wantContains string }{
		{"/", "text/html", "SoundRadar 音效库管理"},
		{"/index.html", "text/html", "新增音效条目"},
		{"/app.js", "application/javascript", "/api/items"},
		{"/style.css", "text/css", "--accent"},
	} {
		res, err := http.Get(ts.URL + tc.path)
		if err != nil {
			t.Fatalf("GET %s: %v", tc.path, err)
		}
		raw, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("GET %s status = %d", tc.path, res.StatusCode)
		}
		if got := res.Header.Get("Content-Type"); !strings.Contains(got, tc.wantType) {
			t.Fatalf("GET %s content type = %q, want %q", tc.path, got, tc.wantType)
		}
		if !strings.Contains(string(raw), tc.wantContains) {
			t.Fatalf("GET %s does not contain %q (%d bytes)", tc.path, tc.wantContains, len(raw))
		}
		t.Logf("GET %-11s -> %d %s (%d bytes)", tc.path, res.StatusCode, res.Header.Get("Content-Type"), len(raw))
	}
}

// ---------------------------------------------------------------------------
// persistence across restarts
// ---------------------------------------------------------------------------

func TestPersistenceAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "library.srz")

	st1, err := library.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ts1 := httptest.NewServer(New(Options{Store: st1}).Handler())

	body, ctype := multipartBody(t,
		map[string]struct {
			Name string
			Data []byte
		}{"audio": {Name: "persist.wav", Data: makeWAV(t, 44100, 1, 0.3, 660, 0.6)}},
		map[string]string{"name": "持久化", "tags": "重启"})
	req, _ := http.NewRequest(http.MethodPost, ts1.URL+"/api/items", body)
	req.Header.Set("Content-Type", ctype)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var created ItemDetailDTO
	json.NewDecoder(res.Body).Decode(&created)
	res.Body.Close()
	ts1.Close()

	// "restart": fresh store + fresh server over the same file.
	st2, err := library.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	ts2 := httptest.NewServer(New(Options{Store: st2}).Handler())
	defer ts2.Close()

	var lib LibraryDTO
	if code := getJSON(t, ts2.URL+"/api/library", &lib); code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if len(lib.Items) != 1 || lib.Items[0].ID != created.ID || lib.Items[0].Name != "持久化" {
		t.Fatalf("item did not survive restart: %+v", lib.Items)
	}
	var detail ItemDetailDTO
	getJSON(t, ts2.URL+"/api/items/"+created.ID, &detail)
	if len(detail.Samples) != 1 || detail.Samples[0].Origin.SampleRate != 44100 {
		t.Fatalf("sample metadata lost: %+v", detail.Samples)
	}

	// The archive on disk must contain exactly the documented layout.
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	names := map[string]bool{}
	for _, f := range zr.File {
		names[f.Name] = true
	}
	for _, want := range []string{
		"manifest.json",
		"items/" + created.ID + "/meta.json",
		"items/" + created.ID + "/icon.png",
		"items/" + created.ID + "/samples/0001.wav",
	} {
		if !names[want] {
			t.Fatalf("archive missing %s", want)
		}
	}
	t.Logf("archive entries after restart: %v", keys(names))
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestAPISilentMP3HasBody is a regression test for a real bug found during the
// P1 end-to-end run: a digital-silence sample has a peak of -Inf dBFS, which
// encoding/json refuses to encode. The handler had already written the 200
// status and Content-Type by then, so the client received an empty body. The
// response must stay a complete JSON document (peakDbfs = null, silent = true).
func TestAPISilentMP3HasBody(t *testing.T) {
	ts, _ := newTestServer(t)

	body, ctype := multipartBody(t,
		map[string]struct {
			Name string
			Data []byte
		}{"audio": {Name: "silence.mp3", Data: testaudio.SilentMP3(20)}},
		map[string]string{"name": "静音 MP3"})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/items", body)
	req.Header.Set("Content-Type", ctype)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %q)", res.StatusCode, raw)
	}
	if len(raw) == 0 {
		t.Fatal("response body is empty: the JSON encoder failed on -Inf dBFS")
	}
	var created ItemDetailDTO
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, raw)
	}
	if len(created.Samples) != 1 {
		t.Fatalf("samples = %d", len(created.Samples))
	}
	s := created.Samples[0]
	if s.Origin.Container != "mp3" {
		t.Fatalf("container = %q", s.Origin.Container)
	}
	if s.Origin.SampleRate != 48000 || s.Origin.Channels != 2 {
		t.Fatalf("mp3 origin = %+v", s.Origin)
	}
	if !s.Silent || s.PeakDBFS != nil {
		t.Fatalf("silent sample must report peakDbfs=null/silent=true, got %v/%v", s.PeakDBFS, s.Silent)
	}
	if s.Frames != 20*testaudio.SamplesPerMPEGFrame/2 {
		t.Fatalf("frames = %d", s.Frames)
	}
	t.Logf("silent mp3 item %s: body=%d bytes, samples=%d, frames=%d (peakDbfs=null)",
		created.ID, len(raw), created.SampleCount, s.Frames)

	// The item must also be listed and fetchable afterwards.
	var listed LibraryDTO
	if code := getJSON(t, ts.URL+"/api/library", &listed); code != http.StatusOK {
		t.Fatalf("GET /api/library status = %d", code)
	}
	if listed.ItemCount != 1 {
		t.Fatalf("itemCount = %d", listed.ItemCount)
	}
	var detail ItemDetailDTO
	code := getJSON(t, ts.URL+"/api/items/"+created.ID, &detail)
	if code != http.StatusOK || len(detail.Samples) != 1 {
		t.Fatalf("GET item -> %d with %d samples", code, len(detail.Samples))
	}
}

// TestLittleEndianCheck makes sure the fixture PCM really is little-endian
// 16-bit data (guards against a wrong helper in the tests themselves).
func TestLittleEndianCheck(t *testing.T) {
	raw := makeWAV(t, 48000, 1, 0.01, 1000, 1.0)
	if binary.LittleEndian.Uint16(raw[20:22]) != 1 {
		t.Fatalf("fixture is not PCM")
	}
	if binary.LittleEndian.Uint32(raw[24:28]) != 48000 {
		t.Fatalf("fixture sample rate is not 48000")
	}
}

func TestHintIconsCreateAndPatch(t *testing.T) {
	ts, st := newTestServer(t)
	pngA := makePNG(t, 40, 80)
	pngB := makePNG(t, 60, 60)
	body, ctype := multipartBody(t,
		map[string]struct {
			Name string
			Data []byte
		}{
			"audio":     {Name: "beep.wav", Data: makeWAV(t, 48000, 1, 0.3, 880, 0.6)},
			"hintIcon0": {Name: "a.png", Data: pngA},
			"hintIcon1": {Name: "b.png", Data: pngB},
		},
		map[string]string{
			"name":         "同音对照",
			"displayHints": `[{"grid":"3x2","name":"泥板"},{"grid":"2x2","name":"理想国"}]`,
		})
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/items", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", ctype)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/items -> %d %s", res.StatusCode, raw)
	}
	var created ItemDetailDTO
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatal(err)
	}
	if len(created.DisplayHints) != 2 {
		t.Fatalf("hints = %+v", created.DisplayHints)
	}
	if created.DisplayHints[0].IconURL == "" || created.DisplayHints[1].IconURL == "" {
		t.Fatalf("每行都应有对照图 URL: %+v", created.DisplayHints)
	}

	it := st.Get(created.ID)
	if it == nil || len(it.HintIcon(0)) == 0 || len(it.HintIcon(1)) == 0 {
		t.Fatal("库里没有按行存下对照图")
	}
	if bytes.Equal(it.HintIcon(0), it.HintIcon(1)) {
		t.Fatal("两行对照图不应相同")
	}

	icon0 := doGETBytes(t, ts.URL+created.DisplayHints[0].IconURL)
	if _, err := png.Decode(bytes.NewReader(icon0)); err != nil {
		t.Fatalf("hint 0 不是 PNG: %v", err)
	}

	pngC := makePNG(t, 32, 96)
	pbody, pctype := multipartBody(t,
		map[string]struct {
			Name string
			Data []byte
		}{
			"hintIcon0": {Name: "c.png", Data: pngC},
		},
		map[string]string{
			"patch": `{"displayHints":[{"grid":"3x2","name":"泥板"},{"grid":"2x2","name":"理想国"}]}`,
		})
	preq, err := http.NewRequest(http.MethodPatch, ts.URL+"/api/items/"+created.ID, pbody)
	if err != nil {
		t.Fatal(err)
	}
	preq.Header.Set("Content-Type", pctype)
	pres, err := http.DefaultClient.Do(preq)
	if err != nil {
		t.Fatal(err)
	}
	praw, _ := io.ReadAll(pres.Body)
	pres.Body.Close()
	if pres.StatusCode != http.StatusOK {
		t.Fatalf("PATCH -> %d %s", pres.StatusCode, praw)
	}
	it = st.Get(created.ID)
	if len(it.HintIcon(1)) == 0 {
		t.Fatal("只改第 0 行时第 1 行的图应保留")
	}
	if bytes.Equal(it.HintIcon(0), it.HintIcon(1)) {
		t.Fatal("替换第 0 行后两行仍不应相同")
	}
}

func doGETBytes(t *testing.T, url string) []byte {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s -> %d %s", url, res.StatusCode, raw)
	}
	return raw
}
