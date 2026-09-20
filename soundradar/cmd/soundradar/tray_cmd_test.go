package main

import (
	"strings"
	"testing"
)

// TestServerChildNeverPassesTray is the regression guard for the duplicate tray
// icon bug: the launcher owns the notification-area icon, so the serve child it
// spawns must not register one of its own.
//
// The symptom was that choosing "打开管理界面" (which can lazily start the server)
// added a SECOND icon to the notification area, because the child was started as
// `serve --overlay --tray`. Two icons also meant two "exit" entries, and the
// child's exit only stopped the server.
func TestServerChildNeverPassesTray(t *testing.T) {
	s := &serverChild{
		exe:  `C:\fake\soundradar.exe`,
		base: 8765,
		cfg:  `C:\fake\config.json`,
		lib:  `C:\fake\library.srz`,
		open: true,
	}
	args := s.childArgs(8765)
	if len(args) == 0 || args[0] != "serve" {
		t.Fatalf("子进程参数应当以 serve 开头，得到 %v", args)
	}
	for _, a := range args {
		if a == "--tray" {
			t.Fatalf("子进程带了 --tray，会多出一个托盘图标: %v", args)
		}
	}
	// The settings that matter must survive.
	for _, want := range []string{"--overlay", "--port", "8765", "--config", `C:\fake\config.json`, "--library", `C:\fake\library.srz`, "--open"} {
		if !containsArg(args, want) {
			t.Errorf("子进程参数缺少 %q: %v", want, args)
		}
	}
}

// TestServerChildArgsOmitEmptyOptions checks that unset options are not passed
// as empty strings, which flag.Parse would read as a positional argument.
func TestServerChildArgsOmitEmptyOptions(t *testing.T) {
	s := &serverChild{exe: "x", base: 9000}
	args := s.childArgs(9000)
	joined := strings.Join(args, " ")
	for _, unwanted := range []string{"--config", "--library", "--open", "--tray"} {
		if strings.Contains(joined, unwanted) {
			t.Errorf("未设置的选项 %q 还是出现在了参数里: %v", unwanted, args)
		}
	}
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}
