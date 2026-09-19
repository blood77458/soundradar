package library

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/znz/soundradar/internal/wav"
)

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

func testWAV(t *testing.T, seconds float64, freq float64) []byte {
	t.Helper()
	n := int(seconds * float64(SampleRate))
	pcm := make([]int16, n)
	for i := range pcm {
		v := math.Sin(2 * math.Pi * freq * float64(i) / float64(SampleRate))
		pcm[i] = wav.ClampInt16(v * 0.6)
	}
	var buf bytes.Buffer
	if err := wav.WriteInt16(&buf, SampleRate, Channels, pcm); err != nil {
		t.Fatalf("WriteInt16: %v", err)
	}
	return buf.Bytes()
}

func testIconPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 64, 48))
	for y := 0; y < 48; y++ {
		for x := 0; x < 64; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(x * 4), G: uint8(y * 5), B: 200, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	return buf.Bytes()
}

func newTestItem(t *testing.T, name string) (*Item, []byte, []byte) {
	t.Helper()
	wavBytes := testWAV(t, 0.25, 880)
	icon := testIconPNG(t)
	it := &Item{
		Name:       name,
		Threshold:  0.72,
		CooldownMs: 400,
		Profile:    "default",
		Tags:       []string{"UI", "拾取"},
		Note:       "搜包完成时听到的那声",
		Samples: []Sample{{
			File:    SampleFile(0),
			OffsetS: 0,
			LenS:    0.25,
			GainDb:  0,
			Source:  "upload",
			AddedAt: time.Now().UTC(),
			Origin: SourceFormat{
				FileName:      "tone.wav",
				Container:     "wav",
				FormatTag:     "PCM",
				SampleRate:    44100,
				Channels:      2,
				BitsPerSample: 16,
				DurationS:     0.25,
				Resampled:     true,
				Downmixed:     true,
			},
			StoredRate: SampleRate, StoredChans: Channels, StoredBits: BitsPerSample,
		}},
	}
	return it, icon, wavBytes
}

// ---------------------------------------------------------------------------
// ZIP round trip
// ---------------------------------------------------------------------------

func TestRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "library.srz")
	st := New(path, "暗区突围：无限")
	if err := st.Save(); err != nil {
		t.Fatalf("Save(new): %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("library file was not created: %v", err)
	}

	it, icon, wavBytes := newTestItem(t, "拾取物品")
	if err := st.AddItem(it, icon, [][]byte{wavBytes}); err != nil {
		t.Fatalf("AddItem: %v", err)
	}
	if len(it.ID) != 8 {
		t.Fatalf("id %q is not 8 characters", it.ID)
	}
	if err := st.Save(); err != nil {
		t.Fatalf("Save(item): %v", err)
	}

	// --- reopen -----------------------------------------------------------
	st2, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if st2.Path() != path {
		t.Fatalf("path = %q, want %q", st2.Path(), path)
	}
	mf := st2.Manifest()
	if mf.Schema != Schema {
		t.Fatalf("schema = %d", mf.Schema)
	}
	if mf.Name != "暗区突围：无限" {
		t.Fatalf("name = %q", mf.Name)
	}
	if len(mf.Items) != 1 || mf.Items[0] != it.ID {
		t.Fatalf("manifest items = %v, want [%s]", mf.Items, it.ID)
	}
	if mf.Feature.Kind == "" || mf.Feature.SampleRate != SampleRate || mf.Feature.MelBands == 0 {
		t.Fatalf("feature placeholder not written: %+v", mf.Feature)
	}
	if mf.CreatedAt.IsZero() {
		t.Fatalf("createdAt is zero")
	}

	got := st2.Get(it.ID)
	if got == nil {
		t.Fatalf("item %s disappeared after reopen", it.ID)
	}
	if got.Name != "拾取物品" || got.Profile != "default" {
		t.Fatalf("meta = %+v", got)
	}
	if math.Abs(got.Threshold-0.72) > 1e-9 || got.CooldownMs != 400 {
		t.Fatalf("threshold/cooldown = %v/%d", got.Threshold, got.CooldownMs)
	}
	if strings.Join(got.Tags, ",") != "UI,拾取" {
		t.Fatalf("tags = %v", got.Tags)
	}
	if got.Note != "搜包完成时听到的那声" {
		t.Fatalf("note = %q", got.Note)
	}
	if !bytes.Equal(got.IconPNG(), icon) {
		t.Fatalf("icon bytes differ: %d vs %d bytes", len(got.IconPNG()), len(icon))
	}
	if len(got.Samples) != 1 {
		t.Fatalf("samples = %d", len(got.Samples))
	}
	if !bytes.Equal(got.SampleWAV(0), wavBytes) {
		t.Fatalf("sample bytes differ: %d vs %d", len(got.SampleWAV(0)), len(wavBytes))
	}
	if got.Samples[0].Origin.SampleRate != 44100 || !got.Samples[0].Origin.Resampled {
		t.Fatalf("original format not preserved: %+v", got.Samples[0].Origin)
	}
	if got.Samples[0].StoredRate != SampleRate {
		t.Fatalf("storedSampleRate = %d", got.Samples[0].StoredRate)
	}

	// --- archive layout ---------------------------------------------------
	names := zipNames(t, path)
	for _, want := range []string{
		ManifestName,
		"items/" + it.ID + "/" + MetaName,
		"items/" + it.ID + "/" + IconName,
		"items/" + it.ID + "/samples/0001.wav",
	} {
		if !containsStr(names, want) {
			t.Fatalf("archive is missing %s (has %v)", want, names)
		}
	}
}

func TestSaveIsByteStable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.srz")
	st := New(path, "stable")
	it, icon, wavBytes := newTestItem(t, "稳定")
	if err := st.AddItem(it, icon, [][]byte{wavBytes}); err != nil {
		t.Fatalf("AddItem: %v", err)
	}
	if err := st.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// Reopen and save to a second file; the payloads must be identical.
	st2, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	second := filepath.Join(dir, "b.srz")
	st2.path = second
	if err := st2.Save(); err != nil {
		t.Fatalf("Save(second): %v", err)
	}
	b, err := os.ReadFile(second)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, b) {
		t.Fatalf("re-saving an unchanged library changed %d/%d bytes", len(first), len(b))
	}
}

// ---------------------------------------------------------------------------
// atomic write
// ---------------------------------------------------------------------------

// TestSaveIsAtomicReplace checks the two properties the requirement asks for:
// an existing library keeps its content when a later save fails, and a
// successful save replaces the file in one step (never in place).
func TestSaveIsAtomicReplace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "library.srz")

	st := New(path, "atomic")
	keep, icon, wavBytes := newTestItem(t, "保留")
	if err := st.AddItem(keep, icon, [][]byte{wavBytes}); err != nil {
		t.Fatalf("AddItem: %v", err)
	}
	if err := st.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// 1. A failed save must leave the previous library byte-for-byte intact and
	//    must not leave temporary files behind. Removing the item's audio blob
	//    makes saveLocked fail halfway through writing the archive.
	st.items[keep.ID].sampleWAV[0] = nil
	if err := st.Save(); err == nil {
		t.Fatal("expected Save to fail when a sample blob is missing")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("library file vanished after a failed save: %v", err)
	}
	if !bytes.Equal(good, after) {
		t.Fatalf("failed save modified the library (%d -> %d bytes)", len(good), len(after))
	}
	// The library must still be readable and still contain the item.
	stAgain, err := Open(path)
	if err != nil {
		t.Fatalf("Open after failed save: %v", err)
	}
	if stAgain.Get(keep.ID) == nil {
		t.Fatalf("item lost after a failed save")
	}

	// No stray temp files.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "library.srz" {
			t.Fatalf("unexpected leftover file after failed save: %s", e.Name())
		}
	}

	// 2. On Windows os.Rename fails when the destination exists, so this also
	//    proves the replace path works when the target file is already there.
	st.items[keep.ID].sampleWAV[0] = wavBytes // repair
	if err := st.Save(); err != nil {
		t.Fatalf("Save over an existing file: %v", err)
	}
	replaced, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(replaced) != len(good) {
		t.Fatalf("replace changed the library size: %d -> %d", len(good), len(replaced))
	}
	st3, err := Open(path)
	if err != nil {
		t.Fatalf("Open after replace: %v", err)
	}
	if got := st3.Get(keep.ID); got == nil || !bytes.Equal(got.SampleWAV(0), wavBytes) {
		t.Fatalf("replace did not persist the intended content")
	}
}

// ---------------------------------------------------------------------------
// delete
// ---------------------------------------------------------------------------

func TestDeleteItem(t *testing.T) {
	path := filepath.Join(t.TempDir(), "library.srz")
	st := New(path, "delete")

	itA, iconA, wavA := newTestItem(t, "A")
	itB, iconB, wavB := newTestItem(t, "B")
	if err := st.AddItem(itA, iconA, [][]byte{wavA}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddItem(itB, iconB, [][]byte{wavB}); err != nil {
		t.Fatal(err)
	}
	if st.Len() != 2 {
		t.Fatalf("len = %d", st.Len())
	}
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}

	if err := st.Delete(itA.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}

	st2, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if st2.Len() != 1 {
		t.Fatalf("len after delete = %d, want 1", st2.Len())
	}
	if st2.Get(itA.ID) != nil {
		t.Fatalf("deleted item still present")
	}
	if got := st2.Get(itB.ID); got == nil || !bytes.Equal(got.SampleWAV(0), wavB) {
		t.Fatalf("surviving item lost its data")
	}
	if mf := st2.Manifest(); len(mf.Items) != 1 || mf.Items[0] != itB.ID {
		t.Fatalf("manifest not updated: %v", mf.Items)
	}
	// Deleting twice must report "not found".
	if err := st2.Delete(itA.ID); err == nil {
		t.Fatal("expected an error deleting an unknown item")
	}
}

func TestDeleteSampleRenumbers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "library.srz")
	st := New(path, "samples")
	it, icon, wav := newTestItem(t, "变体")
	it.Samples = nil
	if err := st.AddItem(it, icon, nil); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		sm := Sample{File: SampleFile(i), LenS: 0.25, Source: "upload"}
		if _, err := st.AddSample(it.ID, sm, wav); err != nil {
			t.Fatalf("AddSample %d: %v", i, err)
		}
	}
	if got := st.Get(it.ID); len(got.Samples) != 3 {
		t.Fatalf("samples = %d", len(got.Samples))
	}
	if _, err := st.DeleteSample(it.ID, 1); err != nil {
		t.Fatalf("DeleteSample: %v", err)
	}
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}

	st2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	got := st2.Get(it.ID)
	if got == nil || len(got.Samples) != 2 {
		t.Fatalf("samples after delete = %+v", got)
	}
	if got.Samples[0].File != "samples/0001.wav" || got.Samples[1].File != "samples/0002.wav" {
		t.Fatalf("sample files not renumbered: %v", got.Samples)
	}
	names := zipNames(t, path)
	if containsStr(names, "items/"+it.ID+"/samples/0003.wav") {
		t.Fatalf("stale sample file still in the archive: %v", names)
	}
}

// ---------------------------------------------------------------------------
// id generation
// ---------------------------------------------------------------------------

func TestNewIDIsRandomHex8(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 500; i++ {
		id, err := NewID()
		if err != nil {
			t.Fatalf("NewID: %v", err)
		}
		if len(id) != 8 {
			t.Fatalf("id %q length %d", id, len(id))
		}
		for _, r := range id {
			if !strings.ContainsRune("0123456789abcdef", r) {
				t.Fatalf("id %q is not lower-case hex", id)
			}
		}
		if seen[id] {
			t.Fatalf("duplicate id %q within 500 draws", id)
		}
		seen[id] = true
	}
}

// ---------------------------------------------------------------------------
// manifest / tolerance
// ---------------------------------------------------------------------------

func TestOpenMissingFileCreatesEmptyLibrary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "library.srz")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if st.Len() != 0 {
		t.Fatalf("len = %d", st.Len())
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("empty library was not created on disk: %v", err)
	}
}

func TestOpenRejectsNonZip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.srz")
	if err := os.WriteFile(path, []byte("this is not a zip file"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("expected an error for a non-ZIP library")
	}
}

func TestRecoversUnreferencedSample(t *testing.T) {
	path := filepath.Join(t.TempDir(), "library.srz")
	st := New(path, "recover")
	it, icon, wav := newTestItem(t, "孤儿样本")
	if err := st.AddItem(it, icon, [][]byte{wav}); err != nil {
		t.Fatal(err)
	}
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}

	// Hand-edit meta.json to drop the sample reference while leaving the blob,
	// which is the state a crash between the two writes would produce.
	metaPath := filepath.Join(t.TempDir(), "meta.json")
	got := st.Get(it.ID)
	meta := map[string]any{}
	raw, _ := json.Marshal(got)
	if err := json.Unmarshal(raw, &meta); err != nil {
		t.Fatal(err)
	}
	meta["samples"] = []any{}
	edited, _ := json.Marshal(meta)
	if err := os.WriteFile(metaPath, edited, 0o644); err != nil {
		t.Fatal(err)
	}
	rewriteZipEntry(t, path, "items/"+it.ID+"/"+MetaName, edited)

	st2, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got2 := st2.Get(it.ID)
	if got2 == nil {
		t.Fatalf("item vanished")
	}
	if len(got2.Samples) != 1 {
		t.Fatalf("orphan sample was not recovered: %+v", got2.Samples)
	}
	if !bytes.Equal(got2.SampleWAV(0), wav) {
		t.Fatalf("recovered sample differs")
	}
	if len(st2.Warnings()) == 0 {
		t.Fatalf("no warning was recorded for the recovery")
	}
}

func TestStoreConcurrencySmoke(t *testing.T) {
	path := filepath.Join(t.TempDir(), "library.srz")
	st := New(path, "race")
	it, icon, wav := newTestItem(t, "并发")
	if err := st.AddItem(it, icon, [][]byte{wav}); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			_ = st.Order()
			_ = st.Stats()
			_ = st.Get(it.ID)
		}
	}()
	for i := 0; i < 50; i++ {
		if _, err := st.Update(it.ID, func(x *Item) error {
			x.Note = fmt.Sprintf("n%d", i)
			return nil
		}); err != nil {
			t.Fatalf("Update: %v", err)
		}
	}
	<-done
}

// ---------------------------------------------------------------------------
// icons
// ---------------------------------------------------------------------------

func TestScaleIconNormalisesTo128(t *testing.T) {
	pngBuf := testIconPNG(t) // 64x48
	out, err := ScaleIcon(pngBuf)
	if err != nil {
		t.Fatalf("ScaleIcon(png): %v", err)
	}
	w, h, err := IconInfo(out)
	if err != nil {
		t.Fatalf("IconInfo: %v", err)
	}
	if w != IconSize || h != IconSize {
		t.Fatalf("icon is %dx%d, want %dx%d", w, h, IconSize, IconSize)
	}

	// JPEG of a very wide image: must be centre-cropped, not squashed.
	img := image.NewRGBA(image.Rect(0, 0, 300, 60))
	for y := 0; y < 60; y++ {
		for x := 0; x < 300; x++ {
			img.Set(x, y, color.RGBA{R: 255, G: uint8(x % 255), B: 0, A: 255})
		}
	}
	var jb bytes.Buffer
	if err := jpeg.Encode(&jb, img, nil); err != nil {
		t.Fatal(err)
	}
	outJ, err := ScaleIcon(jb.Bytes())
	if err != nil {
		t.Fatalf("ScaleIcon(jpeg): %v", err)
	}
	w, h, err = IconInfo(outJ)
	if err != nil {
		t.Fatal(err)
	}
	if w != IconSize || h != IconSize {
		t.Fatalf("jpeg icon is %dx%d", w, h)
	}

	if _, err := ScaleIcon([]byte("not an image")); err == nil {
		t.Fatal("expected an error for a non-image")
	}
	if _, err := ScaleIcon(nil); err == nil {
		t.Fatal("expected an error for an empty icon")
	}
}

func TestPlaceholderIcon(t *testing.T) {
	pngBytes := PlaceholderIcon()
	w, h, err := IconInfo(pngBytes)
	if err != nil {
		t.Fatalf("PlaceholderIcon is not a PNG: %v", err)
	}
	if w != IconSize || h != IconSize {
		t.Fatalf("placeholder is %dx%d", w, h)
	}
	if len(pngBytes) < 200 {
		t.Fatalf("placeholder suspiciously small: %d bytes", len(pngBytes))
	}
	// Cached: a second call must return the same bytes.
	if !bytes.Equal(pngBytes, PlaceholderIcon()) {
		t.Fatalf("placeholder is not stable")
	}
}

func TestAddItemWithoutIconGetsPlaceholder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "library.srz")
	st := New(path, "noicon")
	it, _, wav := newTestItem(t, "无图")
	if err := st.AddItem(it, nil, [][]byte{wav}); err != nil {
		t.Fatal(err)
	}
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	st2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	got := st2.Get(it.ID)
	if got == nil || len(got.IconPNG()) == 0 {
		t.Fatalf("no placeholder icon was generated")
	}
	w, h, err := IconInfo(got.IconPNG())
	if err != nil || w != IconSize || h != IconSize {
		t.Fatalf("placeholder icon invalid: %dx%d err=%v", w, h, err)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func zipNames(t *testing.T, path string) []string {
	t.Helper()
	r, err := zip.OpenReader(path)
	if err != nil {
		t.Fatalf("zip.OpenReader: %v", err)
	}
	defer r.Close()
	names := make([]string, 0, len(r.File))
	for _, f := range r.File {
		names = append(names, f.Name)
	}
	return names
}

func containsStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// rewriteZipEntry replaces one entry's content by rebuilding the archive; used
// to simulate a hand-edited or half-written library.
func rewriteZipEntry(t *testing.T, path, name string, content []byte) {
	t.Helper()
	r, err := zip.OpenReader(path)
	if err != nil {
		t.Fatalf("zip.OpenReader: %v", err)
	}
	type ent struct {
		name string
		data []byte
	}
	ents := make([]ent, 0, len(r.File))
	for _, f := range r.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b := new(bytes.Buffer)
		if _, err := b.ReadFrom(rc); err != nil {
			t.Fatal(err)
		}
		rc.Close()
		data := b.Bytes()
		if f.Name == name {
			data = content
		}
		ents = append(ents, ent{f.Name, data})
	}
	r.Close()

	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	zw := zip.NewWriter(out)
	for _, e := range ents {
		w, err := zw.Create(e.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(e.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestHelperSanity keeps the fixtures honest: the generated WAV really is the
// canonical format the library claims to store.
func TestHelperSanity(t *testing.T) {
	raw := testWAV(t, 0.1, 440)
	a, err := wav.Read(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("wav.Read: %v", err)
	}
	if a.Info.SampleRate != SampleRate || a.Info.Channels != Channels || a.Info.BitsPerSample != BitsPerSample {
		t.Fatalf("fixture format = %s", a.Info.Layout())
	}
	if got := len(a.Samples); got != int(0.1*SampleRate) {
		t.Fatalf("fixture frames = %d", got)
	}
	// Little-endian sanity for the raw PCM path.
	if binary.LittleEndian.Uint16(raw[20:22]) != wav.FormatPCM {
		t.Fatalf("fixture is not PCM")
	}
}
