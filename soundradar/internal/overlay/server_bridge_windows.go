//go:build windows

package overlay

import (
	"image"

	"github.com/znz/soundradar/internal/config"
)

// ServerBridge adapts an Overlay to server.OverlayBridge, so the HTTP layer
// never has to import this Windows-only package. cmd/soundradar installs it
// with server.SetOverlay.
type ServerBridge struct{ Overlay *Overlay }

// ApplyConfig hot-applies a config change.
func (b *ServerBridge) ApplyConfig(cfg config.OverlayConfig) error { return b.Overlay.ApplyConfig(cfg) }

// ApplyHotkey re-registers the toggle hotkey.
func (b *ServerBridge) ApplyHotkey(spec string) error { return b.Overlay.ApplyHotkey(spec) }

// SetVisible shows/hides the overlay (the settings-page switch).
func (b *ServerBridge) SetVisible(v bool) error { return b.Overlay.SetVisible(v) }

// Visible reports the last requested visibility.
func (b *ServerBridge) Visible() bool { return b.Overlay.Visible() }

// State is the JSON snapshot GET /api/overlay returns under "state".
func (b *ServerBridge) State() any { return b.Overlay.State() }

// ShowHit enqueues one recognised hit.
func (b *ServerBridge) ShowHit(id, name string, score float64) {
	_ = b.Overlay.Show(DisplayItem{ID: id, Name: name, Score: score})
}

// NewServerBridge wraps an Overlay for server.SetOverlay.
func NewServerBridge(o *Overlay) *ServerBridge { return &ServerBridge{Overlay: o} }

// IconImage decodes a library icon blob (the CLI's IconProvider uses it).
func IconImage(png []byte) (image.Image, error) { return DecodeIconPNG(png) }
