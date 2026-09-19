package recall

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"

	"github.com/znz/soundradar/internal/wav"
)

// writeWAVAtomic writes pcm as a canonical 48 kHz / mono / 16-bit WAV through a
// temporary file in the destination directory plus an atomic replace.
//
// Writing the WAV "in place" would be the one way a candidate could end up
// truncated: a snapshot is up to 30 s = 2.88 MB and a crash half-way through the
// header/data write would leave a file whose RIFF header claims more bytes than
// it has. The temp+rename path means a WAV visible to a reader is always
// complete.
func writeWAVAtomic(path string, pcm []int16) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".cand-*.wav.tmp")
	if err != nil {
		return fmt.Errorf("创建候选音频临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpName)
	}

	bw := bufio.NewWriterSize(tmp, 1<<16)
	if err := wav.WriteInt16(bw, SampleRate, 1, pcm); err != nil {
		cleanup()
		return fmt.Errorf("写入候选音频失败: %w", err)
	}
	if err := bw.Flush(); err != nil {
		cleanup()
		return fmt.Errorf("刷新候选音频失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("同步候选音频失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("关闭候选音频失败: %w", err)
	}
	if err := replaceFile(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("安装候选音频 %s 失败: %w", path, err)
	}
	return nil
}

// writeFileAtomic writes body to path through a temporary file plus an atomic
// replace, so a torn JSON can never be observed.
func writeFileAtomic(path string, body []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".cand-*.json.tmp")
	if err != nil {
		return fmt.Errorf("创建候选项元数据临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpName)
	}
	if _, err := tmp.Write(body); err != nil {
		cleanup()
		return fmt.Errorf("写入候选项元数据失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("同步候选项元数据失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("关闭候选项元数据失败: %w", err)
	}
	if err := replaceFile(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("安装候选项元数据 %s 失败: %w", path, err)
	}
	return nil
}
