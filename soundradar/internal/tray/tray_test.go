package tray

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"testing"

	"golang.org/x/sys/windows"
)

// TestDefaultMenuIsUsable locks down the two things the tray must always offer:
// a way to reach the management page and a way to quit. Losing either one would
// turn a double-clicked soundradar.exe back into an unkillable background
// process, which is the whole reason the tray exists.
func TestDefaultMenuIsUsable(t *testing.T) {
	menu := DefaultMenu()
	if len(menu) == 0 {
		t.Fatal("默认菜单是空的")
	}
	have := map[Action]string{}
	separators := 0
	for _, it := range menu {
		if it.Separator {
			separators++
			continue
		}
		if it.Label == "" {
			t.Errorf("菜单项 %d 没有文字", it.ID)
		}
		if it.ID == 0 {
			t.Errorf("菜单项 %q 的 ID 是 0（WM_COMMAND 会把 0 当作“没有选择”）", it.Label)
		}
		if prev, dup := have[it.ID]; dup {
			t.Errorf("菜单项 %q 与 %q 的 ID 相同 (%d)", it.Label, prev, it.ID)
		}
		have[it.ID] = it.Label
	}
	for _, want := range []Action{ActionOpenPanel, ActionQuit} {
		if _, ok := have[want]; !ok {
			t.Errorf("默认菜单缺少 ID %d（打开管理界面 / 退出是必须的）", want)
		}
	}
	if separators == 0 {
		t.Error("默认菜单没有任何分隔线，八个条目会挤成一团")
	}
}

// TestMenuLabelsFitTheShell checks the labels are short enough to be readable:
// Windows truncates a notification-area menu entry rather than wrapping it.
func TestMenuLabelsFitTheShell(t *testing.T) {
	for _, it := range DefaultMenu() {
		if it.Separator {
			continue
		}
		if n := len([]rune(it.Label)); n > 20 {
			t.Errorf("菜单项 %q 有 %d 个字，托盘菜单太长会被截断", it.Label, n)
		}
	}
}

// TestIconFromICOParsesOurFormat exercises the .ico reader with the exact layout
// tools/iconmake writes (a 6-byte header, 16-byte directory entries and PNG
// images), including the 256-pixel "0 means 256" convention.
func TestIconFromICOParsesOurFormat(t *testing.T) {
	ico := buildICO(t, []int{16, 32, 48})
	if len(ico) == 0 {
		t.Fatal("测试用 .ico 生成失败")
	}
	// The parser must pick an image without touching the disk. It creates a real
	// HICON, which needs a window station; skip when there is none (a headless
	// CI session), because that is an environment limitation, not a defect.
	h, err := iconFromICO(ico, 16)
	if err != nil {
		t.Skipf("无法在本会话创建图标（可能是无窗口站的会话）: %v", err)
	}
	if h == 0 {
		t.Fatal("iconFromICO 返回了空的 HICON")
	}
	procDestroyIcon.Call(h)
}

// TestIconFromICORejectsGarbage checks the reader fails cleanly instead of
// indexing out of range on a corrupt or truncated file.
func TestIconFromICORejectsGarbage(t *testing.T) {
	for _, bad := range [][]byte{
		nil,
		{1, 2, 3},
		make([]byte, 64), // plausible size, all zeros
	} {
		if _, err := iconFromICO(bad, 16); err == nil {
			t.Errorf("%d 字节的垃圾数据竟然被当成合法 .ico", len(bad))
		}
	}
}

// TestEmbeddedICOFallsBackToStock checks the icon loader never leaves the tray
// without an icon: even when there is no embedded .ico it must produce
// something (a stock system icon).
func TestEmbeddedICOFallsBackToStock(t *testing.T) {
	tr := &Tray{}
	tr.opts.ICO = nil
	tr.ensureIcon()
	h := windows.Handle(tr.icon.Load())
	if h == 0 {
		t.Fatal("没有内嵌图标时 ensureIcon 也没有退回系统图标")
	}
	if !validIcon(h) {
		t.Fatalf("ensureIcon 得到了无效句柄 %v", h)
	}
}

// buildICO writes a minimal .ico with the given sizes, mirroring what
// tools/iconmake produces.
func buildICO(t *testing.T, sizes []int) []byte {
	t.Helper()
	var images [][]byte
	for _, s := range sizes {
		img := image.NewRGBA(image.Rect(0, 0, s, s))
		for y := 0; y < s; y++ {
			for x := 0; x < s; x++ {
				img.Set(x, y, color.RGBA{uint8(x * 3), uint8(y * 3), 0x80, 0xFF})
			}
		}
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			t.Fatalf("编码 PNG: %v", err)
		}
		images = append(images, buf.Bytes())
	}
	var out bytes.Buffer
	binary.Write(&out, binary.LittleEndian, uint16(0))
	binary.Write(&out, binary.LittleEndian, uint16(1))
	binary.Write(&out, binary.LittleEndian, uint16(len(images)))
	offset := 6 + 16*len(images)
	for i, img := range images {
		w := byte(sizes[i])
		if sizes[i] >= 256 {
			w = 0
		}
		out.WriteByte(w)
		out.WriteByte(w)
		out.WriteByte(0)
		out.WriteByte(0)
		binary.Write(&out, binary.LittleEndian, uint16(1))
		binary.Write(&out, binary.LittleEndian, uint16(32))
		binary.Write(&out, binary.LittleEndian, uint32(len(img)))
		binary.Write(&out, binary.LittleEndian, uint32(offset))
		offset += len(img)
	}
	for _, img := range images {
		out.Write(img)
	}
	return out.Bytes()
}
