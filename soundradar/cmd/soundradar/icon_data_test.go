package main

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/znz/soundradar/internal/tray"
)

// TestTrayIconDataIsValidICO walks the embedded icon with the same .ico layout
// the tray loader reads, so a broken regeneration is caught by `go test` instead
// of by a user staring at an empty notification area.
//
// The generator is tools/iconmake; regenerate with:
//
//	go run ./tools/iconmake -out cmd/soundradar/icon.ico -go cmd/soundradar/icon_data.go \
//	    -package main -var trayIconICO -sizes 16,32,48
func TestTrayIconDataIsValidICO(t *testing.T) {
	ico := trayIconICO
	if len(ico) < 6 {
		t.Fatalf("内嵌图标只有 %d 字节", len(ico))
	}
	if got := binary.LittleEndian.Uint16(ico[0:2]); got != 0 {
		t.Errorf("保留字段应该是 0，实际 %d", got)
	}
	if got := binary.LittleEndian.Uint16(ico[2:4]); got != 1 {
		t.Errorf("类型字段应该是 1（图标），实际 %d", got)
	}
	count := int(binary.LittleEndian.Uint16(ico[4:6]))
	if count < 2 {
		t.Fatalf("只有 %d 个尺寸；托盘至少需要一个小尺寸（16px）", count)
	}
	need := 6 + 16*count
	if len(ico) < need {
		t.Fatalf("目录被截断：需要 %d 字节，实际 %d", need, len(ico))
	}
	saw16 := false
	for i := 0; i < count; i++ {
		e := ico[6+i*16:]
		w := int(e[0])
		if w == 0 {
			w = 256
		}
		size := int(binary.LittleEndian.Uint32(e[8:12]))
		off := int(binary.LittleEndian.Uint32(e[12:16]))
		if w == 16 {
			saw16 = true
		}
		if size <= 0 || off < need || off+size > len(ico) {
			t.Fatalf("第 %d 个图像越界：off=%d size=%d 文件=%d", i, off, size, len(ico))
		}
		if !bytes.HasPrefix(ico[off:off+size], []byte{0x89, 'P', 'N', 'G'}) {
			t.Errorf("第 %d 个图像不是 PNG", i)
		}
	}
	if !saw16 {
		t.Error("内嵌图标里没有 16x16 图像（托盘用的就是它）")
	}
}

// TestTrayIconIsRejectedAsGarbage is the control for the test above: the loader
// must refuse the same bytes when they are not an .ico, which proves the test
// above is actually checking something.
func TestTrayIconIsRejectedAsGarbage(t *testing.T) {
	if err := tray.ValidateICOForTest([]byte{1, 2, 3}); err == nil {
		t.Error("三字节的垃圾被当成合法 .ico")
	}
	if err := tray.ValidateICOForTest(trayIconICO); err != nil {
		t.Errorf("内嵌图标没有通过格式校验: %v", err)
	}
}
