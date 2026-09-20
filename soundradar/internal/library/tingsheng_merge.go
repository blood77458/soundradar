package library

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// MergeTingshengResult summarizes MergeTingshengAssets.
type MergeTingshengResult struct {
	MergedSamples map[string]int // class name → samples added
	Deleted       []string
	Renamed       []string
	IconsApplied  int
	ClassIcons    int
}

// imageAliases maps display-hint names to alternate filenames in the 大金 folder.
var imageAliases = map[string]string{
	"赫美拉彩蛋":  "赫芙拉彩蛋",
	"金狮雕像":   "金狮雕像",
	"金狮子":    "金狮雕像",
	"脑电接收装置": "脑电装置",
	"目标定位":   "目标定位模块",
	"石膏像":    "石膏雕塑",
	"密码机":    "K7-L密码机",
	"数据线":    "数据连接线",
	"除颤仪":    "体外去颤器",
	"体外去颤器":  "体外去颤器",
	"吹风机":    "鼓风机",
	"鼓风机":    "鼓风机",
}

// MergeTingshengAssets folds old 拿起/拖动 samples into 同音类 entries, deletes
// 放下 items and consumed sources, and attaches icons from iconDir (大金 folder).
func (s *Store) MergeTingshengAssets(iconDir string) (MergeTingshengResult, error) {
	var out MergeTingshengResult
	out.MergedSamples = map[string]int{}

	if _, err := s.SeedTingshengClasses(); err != nil {
		return out, err
	}

	byName := map[string]*Item{}
	for _, id := range s.Order() {
		it := s.Get(id)
		if it == nil {
			continue
		}
		byName[strings.TrimSpace(it.Name)] = it
	}

	// className → source item names whose 拿起/拖动 samples should be merged in.
	mergeInto := map[string][]string{
		"同音·泥板/理想国": {"天命泥板-拖动"},
		"同音·雕塑组":    {"盛宴雕塑"},
		"同音·定位组":    {"目标定位-拿起"},
		"同音·右侧C类":   {"琥珀天心-拿起"},
		"同音·大框杂项":   {"脑电接收装置-拖动"},
	}
	deleteNames := []string{
		"天命泥板-放下",
		"目标定位-放下",
		"琥珀天心-放下",
		"古董茶壶-放下",
	}
	// Unique items: rename + keep 拿起/拖动 samples.
	uniqueRename := map[string]string{
		"古董茶壶-拿起":  "古董茶壶",
		"花瓶-拿起":    "花瓶",
		"卡莫纳之星-拖动": "卡莫纳之星",
		"金狮子-拖动":   "金狮雕像",
	}
	uniqueHints := map[string][]DisplayHint{
		"古董茶壶":  {{Grid: "2x2", Name: "古董茶壶"}},
		"花瓶":    {{Grid: "2x3", Name: "花瓶"}},
		"卡莫纳之星": {{Grid: "2x2", Name: "卡莫纳之星"}},
		"金狮雕像":  {{Grid: "3x2", Name: "金狮雕像"}},
	}

	toDelete := map[string]bool{}
	for _, n := range deleteNames {
		toDelete[n] = true
	}

	for className, sources := range mergeInto {
		dst := findByName(byName, className)
		if dst == nil {
			return out, fmt.Errorf("缺少同音类条目: %s（请先 seed-tingsheng）", className)
		}
		for _, srcName := range sources {
			src := findByName(byName, srcName)
			if src == nil {
				continue
			}
			if isPutDownName(srcName) {
				toDelete[srcName] = true
				continue
			}
			n, err := s.copySamples(dst.ID, src)
			if err != nil {
				return out, fmt.Errorf("合并 %s → %s: %w", srcName, className, err)
			}
			out.MergedSamples[className] += n
			toDelete[srcName] = true
			// refresh dst pointer after mutation
			dst = s.Get(dst.ID)
			byName[className] = dst
		}
	}

	for oldName, newName := range uniqueRename {
		src := findByName(byName, oldName)
		if src == nil {
			continue
		}
		if existing := findByName(byName, newName); existing != nil && existing.ID != src.ID {
			// Target name already exists: merge samples then delete source.
			n, err := s.copySamples(existing.ID, src)
			if err != nil {
				return out, err
			}
			out.MergedSamples[newName] += n
			toDelete[oldName] = true
			continue
		}
		_, err := s.Update(src.ID, func(it *Item) error {
			it.Name = newName
			it.Tags = []string{"大金", "唯一类"}
			if hints, ok := uniqueHints[newName]; ok {
				norm, nerr := NormalizeDisplayHints(hints)
				if nerr != nil {
					return nerr
				}
				it.DisplayHints = norm
			}
			it.Note = "听声鉴宝唯一类（无同音诈骗）"
			return nil
		})
		if err != nil {
			return out, err
		}
		out.Renamed = append(out.Renamed, oldName+" → "+newName)
		delete(byName, oldName)
		byName[newName] = s.Get(src.ID)
	}

	// Also delete any remaining *放下* items not already listed.
	for name := range byName {
		if isPutDownName(name) {
			toDelete[name] = true
		}
	}

	// Delete put-down items and consumed merge sources (collect ids first).
	var kill []string
	for _, id := range s.Order() {
		it := s.Get(id)
		if it == nil {
			continue
		}
		name := strings.TrimSpace(it.Name)
		if toDelete[name] || isPutDownName(name) {
			kill = append(kill, id)
			out.Deleted = append(out.Deleted, name)
		}
	}
	for _, id := range kill {
		if err := s.Delete(id); err != nil {
			return out, fmt.Errorf("删除 %s: %w", id, err)
		}
	}

	// Refresh name index after deletes/renames.
	byName = map[string]*Item{}
	for _, id := range s.Order() {
		it := s.Get(id)
		if it != nil {
			byName[strings.TrimSpace(it.Name)] = it
		}
	}

	iconDir = strings.TrimSpace(iconDir)
	if iconDir != "" {
		n, cn, err := s.applyDajinIcons(iconDir, byName, true)
		if err != nil {
			return out, err
		}
		out.IconsApplied = n
		out.ClassIcons = cn
	}

	return out, nil
}

func (s *Store) findItemByName(name string) *Item {
	name = strings.TrimSpace(name)
	for _, id := range s.Order() {
		it := s.Get(id)
		if it != nil && strings.TrimSpace(it.Name) == name {
			return it
		}
	}
	return nil
}

func findByName(m map[string]*Item, name string) *Item {
	return m[strings.TrimSpace(name)]
}

func isPutDownName(name string) bool {
	return strings.Contains(name, "放下")
}

func (s *Store) copySamples(dstID string, src *Item) (int, error) {
	if src == nil || len(src.Samples) == 0 {
		return 0, nil
	}
	n := 0
	for i, sm := range src.Samples {
		wav := src.SampleWAV(i)
		if len(wav) == 0 {
			continue
		}
		cp := sm
		cp.File = ""
		cp.Source = "merged:" + src.Name
		if cp.AddedAt.IsZero() {
			cp.AddedAt = time.Now().UTC()
		}
		if _, err := s.AddSample(dstID, cp, wav); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func (s *Store) applyDajinIcons(iconDir string, byName map[string]*Item, replaceClass bool) (hintIcons, classIcons int, err error) {
	files, err := os.ReadDir(iconDir)
	if err != nil {
		return 0, 0, fmt.Errorf("读取图片目录: %w", err)
	}
	stem := map[string]string{} // base name without ext → full path
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		name := f.Name()
		ext := strings.ToLower(filepath.Ext(name))
		if ext != ".png" && ext != ".jpg" && ext != ".jpeg" {
			continue
		}
		base := strings.TrimSuffix(name, filepath.Ext(name))
		stem[base] = filepath.Join(iconDir, name)
	}

	loadScaled := func(itemName string) ([]byte, error) {
		path := resolveImagePath(stem, itemName)
		if path == "" {
			return nil, nil
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil, rerr
		}
		return ScaleIconFit(raw)
	}

	for _, it := range byName {
		if len(it.DisplayHints) == 0 {
			// Still try class icon from item name.
			if png, lerr := loadScaled(it.Name); lerr != nil {
				return hintIcons, classIcons, lerr
			} else if len(png) > 0 {
				if _, serr := s.SetIcon(it.ID, png); serr != nil {
					return hintIcons, classIcons, serr
				}
				classIcons++
			}
			continue
		}
		pngs := make([][]byte, len(it.DisplayHints))
		for i, h := range it.DisplayHints {
			png, lerr := loadScaled(h.Name)
			if lerr != nil {
				return hintIcons, classIcons, lerr
			}
			if len(png) > 0 {
				pngs[i] = png
				hintIcons++
			}
		}
		if _, serr := s.SetHintIcons(it.ID, pngs); serr != nil {
			return hintIcons, classIcons, serr
		}
		if !replaceClass {
			continue
		}
		// Class icon: first available hint icon, else item name.
		var classPNG []byte
		for _, p := range pngs {
			if len(p) > 0 {
				classPNG = p
				break
			}
		}
		if len(classPNG) == 0 {
			classPNG, err = loadScaled(it.Name)
			if err != nil {
				return hintIcons, classIcons, err
			}
		}
		if len(classPNG) > 0 {
			if _, serr := s.SetIcon(it.ID, classPNG); serr != nil {
				return hintIcons, classIcons, serr
			}
			classIcons++
		}
	}
	return hintIcons, classIcons, nil
}

func resolveImagePath(stem map[string]string, itemName string) string {
	itemName = strings.TrimSpace(itemName)
	if itemName == "" {
		return ""
	}
	if p, ok := stem[itemName]; ok {
		return p
	}
	if alt, ok := imageAliases[itemName]; ok {
		if p, ok := stem[alt]; ok {
			return p
		}
	}
	clean := strings.Trim(itemName, "「」《》\"'")
	if p, ok := stem[clean]; ok {
		return p
	}
	// Unique substring: "密码机" → "K7-L密码机", "数据线" → "数据连接线".
	var hit string
	for base, path := range stem {
		if base == "" || itemName == "" {
			continue
		}
		if strings.Contains(base, itemName) || strings.Contains(itemName, base) {
			if hit != "" && hit != path {
				return "" // ambiguous
			}
			hit = path
		}
	}
	return hit
}

// RefreshHintIcons reloads each grid row's picture from iconDir by item name.
// Rows with no matching file keep the picture they already have. The class icon
// is not copied onto rows that have no file of their own.
func (s *Store) RefreshHintIcons(iconDir string) (int, error) {
	byName := map[string]*Item{}
	for _, id := range s.Order() {
		it := s.Get(id)
		if it != nil {
			byName[it.Name] = it
		}
	}
	n, _, err := s.applyDajinIcons(iconDir, byName, false)
	return n, err
}
