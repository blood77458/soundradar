package library

import (
	"fmt"
	"strings"
)

// TingshengClass is one metadata-only 同音类 preset from the 听声鉴宝对照 doc.
type TingshengClass struct {
	Name  string
	Note  string
	Hints []DisplayHint
}

// TingshengSoundClasses returns the five shared-sound groups documented in
// artifacts/tingsheng/听声鉴宝-大金诈骗对照.md. Unique (A-class) items are omitted;
// existing single-item library entries are left untouched.
func TingshengSoundClasses() []TingshengClass {
	return []TingshengClass{
		{
			Name: "同音·泥板/理想国",
			Note: "听声鉴宝同音组：3×2 天命泥板 vs 2×2 理想国。元数据预置，请用 F8/录入补真实同音样本。",
			Hints: []DisplayHint{
				{Grid: "3x2", Name: "天命泥板"},
				{Grid: "2x2", Name: "理想国"},
			},
		},
		{
			Name: "同音·雕塑组",
			Note: "听声鉴宝同音组：盛宴雕塑 / 盘蛇金像 / 光电模型。元数据预置，请补样本。",
			Hints: []DisplayHint{
				{Grid: "2x3", Name: "盛宴雕塑"},
				{Grid: "2x2", Name: "盘蛇金像"},
				{Grid: "2x2", Name: "光电模型"},
			},
		},
		{
			Name: "同音·定位组",
			Note: "听声鉴宝同音组：2×2 目标定位 vs 2×1 热成像。元数据预置，请补样本。",
			Hints: []DisplayHint{
				{Grid: "2x2", Name: "目标定位模块"},
				{Grid: "2x1", Name: "热成像模块"},
			},
		},
		{
			Name: "同音·右侧C类",
			Note: "听声鉴宝右侧科技/工艺同音组（含阵列镜片等）。元数据预置，请补样本。",
			Hints: []DisplayHint{
				{Grid: "2x2", Name: "三轴陀螺仪"},
				{Grid: "2x2", Name: "原型机模块"},
				{Grid: "2x2", Name: "胶囊电视"},
				{Grid: "2x3", Name: "琥珀天心"},
				{Grid: "2x2", Name: "里拉琴"},
				{Grid: "2x2", Name: "承天浑仪"},
				{Grid: "1x2", Name: "赫美拉彩蛋"},
				{Grid: "1x1", Name: "阵列镜片"},
			},
		},
		{
			Name: "同音·大框杂项",
			Note: "听声鉴宝大框杂项同音组（面具/羽宫等与低价同音件）。元数据预置，请补样本。",
			Hints: []DisplayHint{
				{Grid: "2x2", Name: "真实矩阵"},
				{Grid: "2x2", Name: "孔雀羽扇"},
				{Grid: "2x2", Name: "黎明"},
				{Grid: "2x2", Name: "航天导航仪"},
				{Grid: "2x2", Name: "法厄同之钟"},
				{Grid: "2x2", Name: "脑电装置"},
				{Grid: "2x2", Name: "文明之声"},
				{Grid: "2x3", Name: "黄金面具"},
				{Grid: "2x3", Name: "鎏金羽宫"},
				{Grid: "1x3", Name: "机械手臂"},
			},
		},
	}
}

// SeedTingshengResult summarizes a SeedTingshengClasses run.
type SeedTingshengResult struct {
	Added   []string
	Skipped []string // already present by name
}

// SeedTingshengClasses inserts missing 同音类 metadata items (no audio samples).
// Existing items with the same name are skipped. Call Save() afterward.
func (s *Store) SeedTingshengClasses() (SeedTingshengResult, error) {
	var out SeedTingshengResult
	existing := map[string]bool{}
	for _, id := range s.Order() {
		it := s.Get(id)
		if it == nil {
			continue
		}
		existing[strings.TrimSpace(it.Name)] = true
	}
	for _, cls := range TingshengSoundClasses() {
		if existing[cls.Name] {
			out.Skipped = append(out.Skipped, cls.Name)
			continue
		}
		hints, err := NormalizeDisplayHints(cls.Hints)
		if err != nil {
			return out, fmt.Errorf("%s: %w", cls.Name, err)
		}
		it := &Item{
			Name:         cls.Name,
			Threshold:    0.72,
			CooldownMs:   400,
			Profile:      "default",
			Tags:         []string{"同音类", "听声鉴宝"},
			Note:         cls.Note,
			DisplayHints: hints,
			Samples:      []Sample{},
		}
		if err := s.AddItem(it, nil, nil); err != nil {
			return out, fmt.Errorf("添加 %s: %w", cls.Name, err)
		}
		out.Added = append(out.Added, cls.Name)
		existing[cls.Name] = true
	}
	return out, nil
}
