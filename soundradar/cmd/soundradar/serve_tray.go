package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/znz/soundradar/internal/overlay"
	"github.com/znz/soundradar/internal/tray"
)

// trayOptions is what `serve --tray` needs to build a useful menu.
type trayOptions struct {
	// PanelURL is the management page address this process is serving.
	PanelURL string
	// DataDir is the directory holding config.json / library.srz.
	DataDir string
	// CandidatesDir is the recall inbox ("" when it is unavailable).
	CandidatesDir string
	// OverlayKey is the configured overlay toggle hotkey, shown in the menu.
	OverlayKey string
	// Overlay is this process's overlay window, when it has one.
	Overlay    *overlay.Overlay
	HasOverlay bool
}

// newServeTray builds the notification-area icon for a `serve` process.
//
// The action handler is installed after New returns so it can close over the
// *tray.Tray it belongs to (it needs it for the balloon messages); the handler
// can never run before that, because actions only arrive from the message loop
// the constructor starts.
func newServeTray(o trayOptions) (*tray.Tray, error) {
	t, err := tray.New(tray.Options{
		Tooltip: "SoundRadar 声音识别（双击打开管理界面）",
		ICO:     trayIconICO,
		OnActivate: func() {
			if err := tray.OpenURL(o.PanelURL); err != nil {
				fmt.Fprintf(os.Stderr, "[serve] 打开管理界面失败: %v\n", err)
			}
		},
	})
	if err != nil {
		return nil, err
	}

	var quitting bool
	t.OnAction(func(a tray.Action) {
		switch a {
		case tray.ActionOpenPanel, tray.ActionLiveStatus:
			if err := tray.OpenURL(o.PanelURL); err != nil {
				fmt.Fprintf(os.Stderr, "[serve] 打开管理界面失败: %v\n", err)
			}
		case tray.ActionToggleOverlay:
			if o.HasOverlay && o.Overlay != nil {
				_ = o.Overlay.SetVisible(!o.Overlay.Visible())
				return
			}
			key := o.OverlayKey
			if key == "" || strings.EqualFold(key, "none") {
				key = "F9"
			}
			_ = t.Notify("显示 / 隐藏悬浮窗", "按 "+key+" 切换（本进程没有创建悬浮窗）")
		case tray.ActionCandidates:
			if o.CandidatesDir == "" {
				_ = t.Notify("候选项目录", "候选项目录不可用（config 里 recall.dir 为空？）")
				return
			}
			if err := tray.OpenURL(o.CandidatesDir); err != nil {
				fmt.Fprintf(os.Stderr, "[serve] 打开候选项目录失败: %v\n", err)
			}
		case tray.ActionOpenDataDir:
			if err := tray.OpenURL(o.DataDir); err != nil {
				fmt.Fprintf(os.Stderr, "[serve] 打开数据目录失败: %v\n", err)
			}
		case tray.ActionQuit:
			if quitting {
				return
			}
			quitting = true
			requestServeExit()
		}
	})
	return t, nil
}
