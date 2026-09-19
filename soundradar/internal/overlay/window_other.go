//go:build !windows

// Package overlay on non-Windows platforms is a stub: the overlay is a Win32
// layered window and there is no portable equivalent that keeps the "pure Go,
// no cgo, no GUI toolkit" constraint. The package still compiles so the rest of
// the module (config, library, live, server) can be built and tested anywhere.
package overlay

import (
	"errors"
	"image"

	"github.com/znz/soundradar/internal/config"
)

// ErrUnsupported is returned by every window entry point off Windows.
var ErrUnsupported = errors.New("悬浮窗只在 Windows 上可用（需要分层窗口 + UpdateLayeredWindow）")

// State mirrors the Windows structure so callers compile unchanged.
type State struct {
	Visible   bool                 `json:"visible"`
	Enabled   bool                 `json:"enabled"`
	ClassName string               `json:"className"`
	Monitors  []config.MonitorInfo `json:"monitors"`
	Config    config.OverlayConfig `json:"config"`
	LastError string               `json:"lastError,omitempty"`
}

// IconProvider is the icon lookup contract (identical to the Windows build).
type IconProvider interface {
	Icon(id string) (image.Image, error)
}

// Overlay is the unsupported stub.
type Overlay struct{}

// New always fails off Windows.
func New(cfg config.OverlayConfig, toggleHotkey string, icons IconProvider) (*Overlay, error) {
	return nil, ErrUnsupported
}

// Show is unavailable off Windows.
func (o *Overlay) Show(it DisplayItem) error { return ErrUnsupported }

// ApplyConfig is unavailable off Windows.
func (o *Overlay) ApplyConfig(cfg config.OverlayConfig) error { return ErrUnsupported }

// ApplyHotkey is unavailable off Windows.
func (o *Overlay) ApplyHotkey(spec string) error { return ErrUnsupported }

// ToggleHotkey returns the configured hotkey spelling off Windows.
func (o *Overlay) ToggleHotkey() string { return "none" }

// ThreadID is always 0 off Windows.
func (o *Overlay) ThreadID() uint32 { return 0 }

// SetVisible is unavailable off Windows.
func (o *Overlay) SetVisible(v bool) error { return ErrUnsupported }

// Visible always reports false off Windows.
func (o *Overlay) Visible() bool { return false }

// Rect is unavailable off Windows.
func (o *Overlay) Rect() (x, y, w, h int, err error) { return 0, 0, 0, 0, ErrUnsupported }

// HWND is always 0 off Windows.
func (o *Overlay) HWND() uintptr { return 0 }

// ClassName is empty off Windows.
func (o *Overlay) ClassName() string { return "" }

// HotkeyRegistered reports nothing off Windows.
func (o *Overlay) HotkeyRegistered() (string, bool) { return "", false }

// HotkeyError is empty off Windows.
func (o *Overlay) HotkeyError() string { return "" }

// Frames is always 0 off Windows.
func (o *Overlay) Frames() int64 { return 0 }

// State returns an empty snapshot off Windows.
func (o *Overlay) State() State { return State{LastError: ErrUnsupported.Error()} }

// Monitors returns nothing off Windows.
func Monitors() []config.MonitorInfo { return nil }

// Close is a no-op off Windows.
func (o *Overlay) Close() error { return nil }
