package library

import "testing"

func TestNormalizeDisplayHints(t *testing.T) {
	ok, err := NormalizeDisplayHints([]DisplayHint{
		{Grid: "3×2", Name: " 天命泥板 "},
		{Grid: "2x2", Name: "理想国"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ok) != 2 || ok[0].Grid != "3x2" || ok[0].Name != "天命泥板" {
		t.Fatalf("got %+v", ok)
	}

	if _, err := NormalizeDisplayHints([]DisplayHint{{Grid: "big", Name: "x"}}); err == nil {
		t.Fatal("expected grid format error")
	}
	tooMany := make([]DisplayHint, MaxDisplayHints+1)
	for i := range tooMany {
		tooMany[i] = DisplayHint{Grid: "1x1", Name: "a"}
	}
	if _, err := NormalizeDisplayHints(tooMany); err == nil {
		t.Fatal("expected max hints error")
	}
}

func TestFormatHitLabel(t *testing.T) {
	if got := FormatHitLabel("茶壶", nil, 0); got != "茶壶" {
		t.Fatalf("empty hints: %q", got)
	}
	hints := []DisplayHint{{Grid: "3x2", Name: "泥板"}, {Grid: "2x2", Name: "理想国"}}
	got := FormatHitLabel("同音·泥板/理想国", hints, 0)
	want := "同音·泥板/理想国｜3×2 泥板 / 2×2 理想国"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	short := FormatHitLabel("同音·泥板/理想国", hints, 12)
	if short != "同音·泥板/理想…" && !hasEllipsis(short) {
		t.Fatalf("truncate: %q", short)
	}
}

func hasEllipsis(s string) bool {
	return len([]rune(s)) > 0 && []rune(s)[len([]rune(s))-1] == '…'
}
