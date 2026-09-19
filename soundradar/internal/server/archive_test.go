package server

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/znz/soundradar/internal/library"
)

// TestArchiveLayoutAfterUploads mirrors the "unzip library.srz and print the
// file list" step of the manual acceptance run, but in-process: it drives the
// real HTTP handlers, then inspects the real container on disk.
//
// It exists because this sandbox's Windows Smart App Control policy blocks
// freshly built, unsigned .exe files (Event ID 3077/3033, "did not meet the
// Enterprise signing level requirements"), so `soundradar.exe serve` cannot be
// launched here; the same code path is exercised through httptest instead.
func TestArchiveLayoutAfterUploads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data", "library.srz")
	store, err := library.Open(path)
	if err != nil {
		t.Fatalf("library.Open: %v", err)
	}
	ts := newTestServerWithStore(t, store)

	// 1) wav + png icon
	body, ctype := multipartBody(t,
		map[string]struct {
			Name string
			Data []byte
		}{
			"audio": {Name: "beep.wav", Data: makeWAV(t, 44100, 2, 0.5, 880, 0.7)},
			"icon":  {Name: "icon.png", Data: makePNG(t, 300, 180)},
		},
		map[string]string{"name": "归档检查", "tags": "验证"})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/items", body)
	req.Header.Set("Content-Type", ctype)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d: %s", res.StatusCode, raw)
	}
	var item ItemDetailDTO
	if err := json.Unmarshal(raw, &item); err != nil {
		t.Fatal(err)
	}

	// 2) append an mp3 variant
	body2, ctype2 := multipartBody(t,
		map[string]struct {
			Name string
			Data []byte
		}{"audio": {Name: "variant.mp3", Data: silentMP3Fixture(t)}},
		nil)
	req2, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/items/"+item.ID+"/samples", body2)
	req2.Header.Set("Content-Type", ctype2)
	res2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	res2.Body.Close()
	if res2.StatusCode != http.StatusCreated {
		t.Fatalf("append sample status = %d", res2.StatusCode)
	}

	// 3) inspect the container written by the handlers
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatalf("zip.OpenReader(%s): %v", path, err)
	}
	defer zr.Close()

	st, _ := os.Stat(path)
	t.Logf("library.srz on disk: %d bytes, %d entries", st.Size(), len(zr.File))
	var total int64
	for _, f := range zr.File {
		t.Logf("  %-42s %8d bytes  method=%d", f.Name, f.UncompressedSize64, f.Method)
		total += int64(f.UncompressedSize64)
	}

	want := []string{
		"manifest.json",
		"items/" + item.ID + "/meta.json",
		"items/" + item.ID + "/icon.png",
		"items/" + item.ID + "/samples/0001.wav",
		"items/" + item.ID + "/samples/0002.wav",
	}
	have := map[string]bool{}
	for _, f := range zr.File {
		have[f.Name] = true
	}
	for _, w := range want {
		if !have[w] {
			t.Fatalf("archive is missing %s", w)
		}
	}

	// manifest.json must be readable JSON with the documented fields
	mf := readEntry(t, zr, "manifest.json")
	var manifest library.Manifest
	if err := json.Unmarshal(mf, &manifest); err != nil {
		t.Fatalf("manifest.json: %v (%s)", err, mf)
	}
	t.Logf("manifest: schema=%d name=%q createdAt=%s items=%v feature.kind=%s",
		manifest.Schema, manifest.Name, manifest.CreatedAt.Format(time.RFC3339), manifest.Items, manifest.Feature.Kind)
	if manifest.Schema != library.Schema || len(manifest.Items) != 1 || manifest.Items[0] != item.ID {
		t.Fatalf("manifest content = %+v", manifest)
	}

	meta := readEntry(t, zr, "items/"+item.ID+"/meta.json")
	var metaItem library.Item
	if err := json.Unmarshal(meta, &metaItem); err != nil {
		t.Fatalf("meta.json: %v (%s)", err, meta)
	}
	if metaItem.Name != "归档检查" || len(metaItem.Samples) != 2 {
		t.Fatalf("meta.json = %+v", metaItem)
	}
	t.Logf("meta.json: id=%s name=%q threshold=%v cooldownMs=%d tags=%v samples=%d",
		metaItem.ID, metaItem.Name, metaItem.Threshold, metaItem.CooldownMs, metaItem.Tags, len(metaItem.Samples))
	for i, s := range metaItem.Samples {
		t.Logf("  sample[%d]: file=%s lenS=%v origin=%s/%d Hz/%d ch resampled=%v",
			i, s.File, s.LenS, s.Origin.Container, s.Origin.SampleRate, s.Origin.Channels, s.Origin.Resampled)
	}

	icon := readEntry(t, zr, "items/"+item.ID+"/icon.png")
	if len(icon) < 8 || string(icon[1:4]) != "PNG" {
		t.Fatalf("icon.png is not a PNG (%d bytes)", len(icon))
	}
	wav1 := readEntry(t, zr, "items/"+item.ID+"/samples/0001.wav")
	if len(wav1) < 44 || string(wav1[0:4]) != "RIFF" || string(wav1[8:12]) != "WAVE" {
		t.Fatalf("samples/0001.wav is not RIFF/WAVE")
	}
	if len(wav1) != 48044 {
		t.Fatalf("samples/0001.wav = %d bytes, want 48044 (0.5 s at 48 kHz mono)", len(wav1))
	}
	t.Logf("icon.png=%d bytes (128x128 expected), samples/0001.wav=%d bytes RIFF OK", len(icon), len(wav1))
	if fmt.Sprint(have) == "" {
		t.Fatal("unreachable")
	}
}

func readEntry(t *testing.T, zr *zip.ReadCloser, name string) []byte {
	t.Helper()
	for _, f := range zr.File {
		if f.Name != name {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("open %s: %v", name, err)
		}
		defer rc.Close()
		b, err := io.ReadAll(rc)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return b
	}
	t.Fatalf("archive has no entry %s", name)
	return nil
}
