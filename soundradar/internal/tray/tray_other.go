//go:build !windows

package tray

import "errors"

// New reports that the notification area is a Windows feature. The rest of the
// package compiles everywhere so the command layer does not need build tags.
func New(Options) (*Tray, error) {
	return nil, errors.New("系统托盘只在 Windows 上可用")
}

// SetMenu is a no-op on other platforms.
func (t *Tray) SetMenu([]Item) error { return errors.New("系统托盘只在 Windows 上可用") }

// OnAction is a no-op on other platforms.
func (t *Tray) OnAction(func(Action)) {}

// OnActivate is a no-op on other platforms.
func (t *Tray) OnActivate(func()) {}

// SetTooltip is a no-op on other platforms.
func (t *Tray) SetTooltip(string) error { return errors.New("系统托盘只在 Windows 上可用") }

// SetIcon is a no-op on other platforms.
func (t *Tray) SetIcon([]byte) error { return errors.New("系统托盘只在 Windows 上可用") }

// Notify is a no-op on other platforms.
func (t *Tray) Notify(string, string) error { return errors.New("系统托盘只在 Windows 上可用") }

// Close is a no-op on other platforms.
func (t *Tray) Close() error { return nil }

// HWND returns 0 on other platforms.
func (t *Tray) HWND() uintptr { return 0 }

// OpenURL is unsupported on other platforms.
func OpenURL(string) error { return errors.New("仅 Windows 支持") }
