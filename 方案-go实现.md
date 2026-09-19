# Sound Radar — Go 实现方案（小体积 / 单文件 / 跨机器）

> **需求已锁定（只做 Windows / 游戏：暗区突围：无限 / 图标悬浮窗 / 可编辑向量库），
> 具体实现规格见 [`方案-落地设计.md`](方案-落地设计.md)。** 本文档保留通用的 Go 选型与体积分析。
>
> 结论：**能做，而且 Go 比 Python 更适合**这个需求。
> 但"任何机器"需要精确定义：**音频回环采集是平台强相关的**，必须先定清楚范围（见第 6 节）。
> 目标产物：`soundradar.exe`，纯静态、无运行时依赖、双击即跑，**2–4 MB**（模板内嵌后 <4.5 MB）。

---

## 1. 语言选型结论表（每一层用什么）

| 层 | 方案 | cgo | 说明 |
|---|---|---|---|
| 音频采集（Windows） | [moutend/go-wca](https://pkg.go.dev/github.com/moutend/go-wca@v0.1.1)（纯 Go WASAPI 绑定） | 否 | CGO_ENABLED=0 即可拿到 loopback 数据；官方有 [LoopbackCaptureSharedTimerDriven 例子](https://pkg.go.dev/github.com/briight/go-wca@v0.1.0/example/LoopbackCaptureSharedTimerDriven) |
| 音频采集（跨平台兜底） | [gen2brain/malgo](https://products.fileformat.com/zh/audio/go/malgo/)（miniaudio 绑定） | **是** | 需 gcc，产物体积上涨，只在确实要 macOS/Linux 时启用 |
| FFT / mel / log-mel | **自己写**（radix-2 FFT ~120 行 + 预计算 mel 矩阵 ~60 行） | 否 | 不引 gonum/librosa，省 1–3 MB；游戏音效识别用不到高级 DSP |
| 向量检索（"向量库"） | **不需要向量库**，扁平数组 + 手写点积 | 否 | 见第 2 节，规模算完就知道暴力检索比 FFT 还便宜 |
| 向量检索（万一要） | [coder/hnsw](https://pkg.go.dev/github.com/coder/hnsw@v0.5.0)（纯 Go ANN） | 否 | 上万条以上再上；或者 [viterin/vek](https://pkg.go.dev/github.com/viterin/vek@v0.4.0) 做 SIMD 点积 |
| 悬浮窗 / 托盘 | [rodrigocfd/windigo](https://pkg.go.dev/github.com/rodrigocfd/windigo@v0.2.3) 或裸 syscall 调 Win32 | 否 | 置顶 + 鼠标穿透 + 半透明，零额外体积 |
| 热键打标 | Win32 `RegisterHotKey` + 消息循环 | 否 | 已含在 windigo/syscall 里 |
| 语音播报 | SAPI（`ISpVoice`）via [go-ole](https://github.com/go-ole/go-ole) | 否 | 用 Windows 自带中文语音，零依赖 |
| 提示音 | `winmm.dll PlaySound(SND_MEMORY\|SND_ASYNC)` + 自合成 WAV | 否 | 裸 syscall，几行代码 |
| 配置/日志/内嵌资源 | stdlib（`encoding/json` / `embed` / `log`） | 否 | — |
| CNN 推理（后期） | 不用 onnxruntime（cgo+DLL，最重）；**手写前向** conv2d/bn/relu/pool/fc ~300 行 | 否 | Python 离线训练 → 导出权重 bin → Go 读进来算 |

**直接依赖总数：3–4 个**（go-ole、go-wca、windigo、可选 vek）。

---

## 2. "向量库最轻量"的正确答案：不是换库，是换表示

先算规模（这决定一切）：

```
200 个音效类别 × 8 条模板 = 1600 条向量
log-mel patch = 64 × 32 = 2048 维 float32 = 8 KB/条   → 共 12.8 MB ❌ 太大
```

两种压缩，效果叠加：

1. **降维**：对 patch 做 PCA / 随机投影，2048 → **128 维**（离线算一次，矩阵内嵌）。
   准确率损失通常在 1–2% 以内，因为音效的判别信息集中在少数主成分上。
2. **量化**：float32 → **int8 对称量化**（每类存一个 scale）。体积 ÷4，点积可用整数累加。

```
1600 × 128 维 int8 = 200 KB   ✅ 直接 embed 进 exe，零外部文件
```

**性能**：一次全库暴力检索 = 1600 × 128 ≈ 20 万次乘加 ≈ **几十微秒**，
比一次 FFT 还便宜。每秒最多也就 50 次检索 → 完全不需要 ANN 索引。

结论：**扁平数组 + 手写点积（可选 vek 的 SIMD 版本）就是最优解**。
引 HNSW/faiss 只会增加体积和复杂度，换不来任何可感知收益。真正要"轻量"的地方是**特征表示**，不是检索算法。

> 什么时候才需要 HNSW：类别 > 5000、模板 > 5 万条，或要做"未知音效发现"的全库近邻聚类。
> 那时再 `go get github.com/coder/hnsw`，接口已经预留（`internal/index/hnsw.go`）。

---

## 3. 二进制体积预算

| 项 | 体积 |
|---|---|
| 纯 Go、Win32 原生 UI、`CGO_ENABLED=0` | 2–3 MB |
| `go build -trimpath -ldflags "-s -w"` | 约 **2–4 MB** |
| 内嵌模板（量化后 200–400 KB） | +0.3 MB |
| 可选 UPX 压缩 | ≈1.5 MB（部分杀软误报，默认不用） |
| **对比**：Python + PyInstaller 同功能 | 40–80 MB，且首次启动慢 |

构建命令：
```bash
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 \
  go build -trimpath -ldflags "-s -w -H=windowsgui" -o soundradar.exe ./cmd/soundradar
```
（`-H=windowsgui`：双击不弹黑框；调试期先去掉看日志。）

---

## 4. UI 方案对比（推荐"分层"而不是一步到位）

| 方案 | 依赖 | 体积 | 评价 |
|---|---|---|---|
| **Win32 分层窗口**（置顶/穿透/半透明） | 无 | +0 | 最贴合"小工具"；鼠标穿透 `WS_EX_TRANSPARENT`，不挡操作 |
| **托盘图标 + 气泡/Toast 通知** | 无 | +0 | 最省事、最不打扰，适合 MVP |
| 终端 TUI（bubbletea） | 小库 | +1 MB | 先跑通识别逻辑最快 |
| webview（WebView2） | cgo + 系统组件 | 中 | 界面最好看，但破坏"单文件零依赖" |
| Fyne / Gio | 大 | +8–20 MB | **不推荐**，与目标冲突 |

**路线**：`core（采集+识别）` → MVP 用托盘/TUI 提示 → 稳定后加 Win32 悬浮窗（大字 + 图标 + 置信度条 + 事件时间线）。

---

## 5. 目录结构（Go）

```
sound_check/
  cmd/soundradar/main.go            # 组装：采集→特征→检索→事件→提示
  internal/capture/
      capturer.go                   # interface Capturer { Start(cfg) (<-chan Frame, error); Stop() error }
      loopback_windows.go           # go-wca 实现（//go:build windows）
      monitor_linux.go             # PulseAudio parec 子进程（//go:build linux）
      tap_darwin.go                 # malgo/CoreAudio tap（//go:build darwin）
  internal/dsp/     fft.go mel.go logmel.go norm.go vad.go spectralsub.go
  internal/index/   quantize.go flat.go hnsw.go        # 扁平检索为主，hnsw 预留
  internal/match/   template.go detector.go debounce.go
  internal/events/  rules.go state.go                   # 时间逻辑/状态机
  internal/labeler/ ring.go wav.go                      # 环形缓冲 + 热键打标
  internal/hotkey/  hotkey_windows.go
  internal/ui/      tray_windows.go overlay_windows.go tui.go
  internal/tts/     sapi_windows.go beep_windows.go
  internal/config/  config.go
  data/templates/   *.bin  index.bin  meta.json         # 量化模板 + 元数据（可 embed）
  tools/train/      train_cnn.py  pca_export.py         # 仅离线用 Python
```

Go 侧承担全部运行时逻辑；Python 只出现在**离线训练/PCA 导出**这一个可选环节。

---

## 6. "任何机器上都能跑"——必须先定范围（重要）

| 平台 | 系统音频回环能力 | 本方案支持度 |
|---|---|---|
| **Windows 10/11 x64** | WASAPI loopback 原生支持 | ✅ 完美：纯 Go、单文件、零运行时、零驱动 |
| Linux | PulseAudio/PipeWire monitor source 可抓 | ⚠️ 可做（`parec` 子进程，零 cgo），但发行版差异大 |
| macOS 14.2+ | Core Audio process tap（需授权） | ⚠️ 需 cgo/malgo + 用户授权，编译链复杂 |
| macOS <14.2 | 系统不提供，必须装 BlackHole | ❌ 无法"双击即用" |
| 手机 / 主机 | 无系统级回环 | ❌ 不在范围内 |

**建议：先做 Windows 单文件 exe**（真正意义上的"拷贝即用"），
代码从第一天就用 `Capturer` 接口 + build tags 隔离平台，将来加 Linux/macOS 不用重构。

> 注意：目前本机**没有安装 Go**（我已确认：`go` 不在 PATH，也无 gcc/scoop/choco），
> 但有 winget：`winget install GoLang.Go`。**没有 gcc 反而正好**——坚定了"不碰 cgo"的路线。

---

## 7. 阶段路线图（Go 版）

| 阶段 | 内容 | 时间 | 验收标准 |
|---|---|---|---|
| P0 | 装 Go；go-wca 跑通 WASAPI loopback；存 WAV | 0.5–1 天 | 录下一段游戏战斗音频，能看波形/谱图 |
| P1 | 环形缓冲 + `RegisterHotKey` 热键打标 → `data/raw/<label>/*.wav` | 1 天 | 每个关键音效 ≥5 条干净样本 |
| P2 | 自写 FFT+mel → PCA/int8 量化 → 扁平检索 → 终端实时打印 | 1–2 天 | 已知音效 top1 ≥90%，误报 <1 次/分钟 |
| P3 | 托盘/悬浮窗 + 自合成提示音 + CSV 日志 | 1 天 | 挂着打完整一局不打扰操作 |
| P4 | 规则/状态机（"附近有人""Boss 转阶段"）+ SAPI 语音 | 按需 | 组合语义提示生效 |
| P5 | 若模板不够 → 数据增强 + 手写 CNN 前向推理 | 2–4 天 | top1 ≥95%，泛化到不同局面 |

**MVP（一周内）**：Windows 单 exe、8 个关键音效、量化扁平检索、托盘/悬浮窗文字提示、CSV 日志。

---

## 8. 风险与坑（Go 特有部分）

1. **go-wca 是个人维护的老库**，API 风格旧、可能踩坑。
   对策：P0 就把采集封装成 `Capturer` 接口；踩坑时两条后路——
   (a) 裸 syscall 手写 WASAPI COM（约 300 行）；(b) cgo 版 malgo（代价是体积和 gcc）。
2. **不要引入 cgo**：本机无 gcc，且 cgo 破坏单文件静态 exe 目标（体积 + 运行库依赖）。
3. **不要用 Fyne/Gio** 做界面——体积 8–20 MB，直接违背需求；Win32 分层窗口是最优解。
4. **WASAPI loopback 抓不到独占模式（Exclusive）音频**，少数游戏如此 → 兜底用 VB-Cable 虚拟声卡。
5. **int8 量化必须在同一增益链下校准**（关闭游戏内"夜间模式"/空间音效/Dolby/DTS），否则量化 scale 和阈值都会漂。
6. **热键冲突**：`RegisterHotKey` 失败要给出明确报错并允许换键；全屏独占游戏可能收不到热键。
7. **多输出端点**：游戏走 HDMI/显示器时 loopback 端点不同 → 端点做成配置项（本机端点：`Realtek(R) Audio` / `AMD High Definition Audio Device` / `High Definition Audio Device`）。
8. **反作弊**：只读系统音频、不注入进程、不改游戏文件。悬浮窗必须是**独立进程的普通置顶窗口**，绝不能注入游戏。
9. **GC 抖动**：音频热路径用预分配缓冲、`sync.Pool`、避免每帧分配；20 ms 块的处理预算很宽裕。

---

## 9. 下一步（需要你拍板）

1. **平台范围**：只要 Windows（推荐，真"双击即用"），还是必须含 macOS/Linux？（决定是否引入 cgo）
2. **游戏名 / 引擎**：决定能否直接拆包拿原始音效（省 80% 标注工作）。
3. **提示方式**：托盘通知 / 悬浮窗大字 / SAPI 语音 / 组合？

你确认后我立刻做 P0：装 Go → 跑通 loopback 抓到游戏声音 → 存 WAV 看波形。
