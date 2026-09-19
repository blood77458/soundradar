package server

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/znz/soundradar/internal/library"
)

// TestRealTCPListen reproduces the `soundradar serve` startup path (same
// Listen + Serve + Shutdown code the CLI uses) and drives it over a real TCP
// socket on 127.0.0.1 with net/http, instead of httptest.
//
// This is the closest this sandbox allows to the manual `soundradar.exe serve`
// run: Windows Smart App Control refuses to load any freshly built unsigned
// executable here (CodeIntegrity Event ID 3077/3033), so the CLI cannot be
// started, but the listening/serving code can.
func TestRealTCPListen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "library.srz")
	store, err := library.Open(path)
	if err != nil {
		t.Fatalf("library.Open: %v", err)
	}
	logger := log.New(io.Discard, "", 0)
	srv := New(Options{Store: store, Logger: logger})

	ln, port, err := srv.Listen(18765, false)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	base := "http://127.0.0.1:" + itoa(port)
	t.Logf("listening on %s (listener addr %s)", base, ln.Addr().String())

	go func() { _ = srv.Serve(ln) }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	}()
	waitForServer(t, base)

	// 1) embedded UI
	res, err := http.Get(base + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || !strings.Contains(string(body), "SoundRadar 音效库管理") {
		t.Fatalf("GET / -> %d, %d bytes", res.StatusCode, len(body))
	}
	t.Logf("GET / -> %d, %d bytes, title present", res.StatusCode, len(body))

	// 2) create an item
	mb, ctype := multipartBody(t,
		map[string]struct {
			Name string
			Data []byte
		}{
			"audio": {Name: "beep.wav", Data: makeWAV(t, 44100, 2, 0.4, 880, 0.7)},
			"icon":  {Name: "icon.png", Data: makePNG(t, 64, 64)},
		},
		map[string]string{"name": "TCP 验证", "tags": "真实端口"})
	req, _ := http.NewRequest(http.MethodPost, base+"/api/items", mb)
	req.Header.Set("Content-Type", ctype)
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /api/items: %v", err)
	}
	raw, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("POST /api/items -> %d: %s", res.StatusCode, raw)
	}
	var created ItemDetailDTO
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatal(err)
	}
	t.Logf("POST /api/items -> 201 id=%s name=%q", created.ID, created.Name)

	// 3) library listing
	res, err = http.Get(base + "/api/library")
	if err != nil {
		t.Fatal(err)
	}
	var lib LibraryDTO
	if err := json.NewDecoder(res.Body).Decode(&lib); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if lib.ItemCount != 1 || lib.FileBytes <= 0 {
		t.Fatalf("library = %+v", lib)
	}
	t.Logf("GET /api/library -> 200 path=%s items=%d bytes=%d", lib.Path, lib.ItemCount, lib.FileBytes)

	// 4) sample playback over TCP
	res, err = http.Get(base + "/api/items/" + created.ID + "/samples/1.wav")
	if err != nil {
		t.Fatal(err)
	}
	wavBody, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || string(wavBody[0:4]) != "RIFF" {
		t.Fatalf("GET sample -> %d, %d bytes", res.StatusCode, len(wavBody))
	}
	t.Logf("GET /api/items/%s/samples/1.wav -> 200, %d bytes, RIFF OK", created.ID, len(wavBody))

	// 5) icon
	res, err = http.Get(base + "/api/items/" + created.ID + "/icon.png")
	if err != nil {
		t.Fatal(err)
	}
	icon, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || len(icon) == 0 {
		t.Fatalf("GET icon -> %d", res.StatusCode)
	}
	t.Logf("GET icon.png -> 200, %d bytes", len(icon))

	// 6) the library really is on disk at the advertised path
	if _, err := os.Stat(lib.Path); err != nil {
		t.Fatalf("library file missing at %s: %v", lib.Path, err)
	}
}

func waitForServer(t *testing.T, base string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		res, err := http.Get(base + "/api/library")
		if err == nil {
			res.Body.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("server at %s never became reachable", base)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		p--
		b[p] = '-'
	}
	return string(b[p:])
}
