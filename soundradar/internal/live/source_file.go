package live

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/znz/soundradar/internal/audio"
)

// fileSource replays a WAV (or MP3, since internal/audio handles both) as the
// audio source of the realtime link.
//
// It is the offline/replay source: the P2 `live --wav` flag and the tests use
// it to reproduce a problem without a sound card. With realtime=false the whole
// file is pushed as fast as the engine can take it, and because the engine's
// hand-off is a blocking send, the file is replayed in full (no block is ever
// dropped - "no drops" is exactly what a deterministic replay needs). With
// realtime=true the blocks are paced to the audio clock instead, which is what
// the SSE/panel verification uses.
type fileSource struct {
	path     string
	pcm      []float32
	src      audio.Source
	realtime bool

	ch chan []float32
	// quit is closed by Stop; done is closed when the producer has returned.
	quit chan struct{}
	done chan struct{}

	startOnce sync.Once
	stopOnce  sync.Once
	mu        sync.Mutex
	err       error
	started   bool
}

// NewFileSource decodes path to 48 kHz mono float32 and prepares a source.
// The file is read eagerly so a bad path fails here (at startup, where the CLI
// can report it) instead of half a second into the run.
func NewFileSource(path string, realtime bool) (FrameSource, error) {
	if path == "" {
		return nil, errors.New("live: 音频文件路径为空")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取音频文件失败: %w", err)
	}
	conv, err := audio.Convert(raw, filepath.Base(path))
	if err != nil {
		return nil, err
	}
	pcm := make([]float32, len(conv.Mono48k))
	for i, v := range conv.Mono48k {
		pcm[i] = float32(v)
	}
	return &fileSource{
		path:     path,
		pcm:      pcm,
		src:      conv.Source,
		realtime: realtime,
		ch:       make(chan []float32, 64),
		quit:     make(chan struct{}),
		done:     make(chan struct{}),
	}, nil
}

// Start launches the replay goroutine.
func (s *fileSource) Start() error {
	select {
	case <-s.quit:
		return errors.New("live: 音源已停止，不能重新启动")
	default:
	}
	s.startOnce.Do(func() {
		s.mu.Lock()
		s.started = true
		s.mu.Unlock()
		go s.produce()
	})
	return nil
}

// Frames returns the block channel.
func (s *fileSource) Frames() <-chan []float32 { return s.ch }

// Stop cancels the replay; it is safe to call without Start and more than once.
func (s *fileSource) Stop() error {
	s.mu.Lock()
	started := s.started
	s.mu.Unlock()
	if !started {
		// Nothing is producing, so close the channel here (the engine relies on
		// a closed Frames channel to mean "finished").
		s.stopOnce.Do(func() { close(s.quit); close(s.ch) })
		return nil
	}
	s.stopOnce.Do(func() { close(s.quit) })
	<-s.done
	return nil
}

// Err returns the error that stopped the replay.
func (s *fileSource) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Info describes the file for the banner/API.
func (s *fileSource) Info() SourceInfo {
	return SourceInfo{
		Kind:     "file",
		Detail:   fmt.Sprintf("文件回放 %s（%d Hz / %d 声道 / %.3f s）%s", filepath.Base(s.path), s.src.SampleRate, s.src.Channels, s.Duration(), realtimeLabel(s.realtime)),
		Path:     s.path,
		Rate:     s.src.SampleRate,
		Channels: s.src.Channels,
		Format:   s.src.FormatTag,
		Seconds:  s.Duration(),
		Realtime: s.realtime,
	}
}

// Duration is the audio length of the file in seconds.
func (s *fileSource) Duration() float64 { return float64(len(s.pcm)) / SampleRate }

func realtimeLabel(rt bool) string {
	if rt {
		return "，按实时速度回放"
	}
	return "，尽快回放（离线）"
}

// produce pushes 20 ms blocks. Realtime mode sleeps on an absolute deadline so
// pacing errors cannot accumulate.
func (s *fileSource) produce() {
	defer close(s.done)
	defer close(s.ch)

	var sent int64
	start := time.Now()
	for off := 0; off < len(s.pcm); off += BlockFrames {
		end := min(off+BlockFrames, len(s.pcm))
		blk := s.pcm[off:end]
		if s.realtime {
			want := time.Duration(float64(sent) / SampleRate * float64(time.Second))
			if d := want - time.Since(start); d > 0 {
				t := time.NewTimer(d)
				select {
				case <-s.quit:
					t.Stop()
					return
				case <-t.C:
				}
			}
		}
		select {
		case s.ch <- blk:
		case <-s.quit:
			return
		}
		sent += int64(end - off)
	}
}
