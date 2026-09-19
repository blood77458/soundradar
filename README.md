# SoundRadar

一边听系统正在播放的声音，一边告诉你刚才那是什么。命中后在屏幕上弹出图标和名称。

给 Windows 用。采集的是扬声器回环（WASAPI loopback），不需要虚拟声卡，也不需要把麦克风对准音箱。程序是纯 Go 写的，单个 exe，不依赖 CGO。

当前版本 `0.6.0-p4`。

## 下载就能用

从 [Releases](https://github.com/blood77458/soundradar/releases) 下载 `soundradar-windows.zip`，解压到一个单独的文件夹。压缩包里已经有程序和一份声纹库：

```
soundradar.exe
data\library.srz
data\index.bin
```

在该文件夹打开 PowerShell：

```powershell
.\soundradar.exe serve --overlay --open
```

这会做三件事：打开只在本机访问的管理页面、开始听系统正在播放的声音、打开命中悬浮窗。

- **F9** 显示或隐藏悬浮窗。悬浮窗置顶、鼠标穿透，不会抢走游戏焦点。
- **F8** 把刚才大约 3 秒存进「候选项」。游戏里做完动作、听到那一声之后再按。
- 管理页面的「候选项」里试听、起名、配图标，然后新建条目或追加到已有条目。
- 采集设备默认是系统当前的播放设备。耳机名字不对时，到管理页面的「设置」里改。设置写在 exe 旁边的 `config.json`，第一次运行会自己生成。
- 声纹库是 exe 旁边的 `data\library.srz`。压缩包里的这份可以直接用来认。你后来自己录的条目也写在这个文件里，换电脑时单独备份它。索引 `data\index.bin` 可以随时重算，库比索引新时，下次开始实时识别会自动重建。

自带库里现有这些声音：脑电接收装置-拖动，卡莫纳之星-拖动，金狮子-拖动，天命泥板-拖动、天命泥板-放下，古董茶壶-拿起、古董茶壶-放下，目标定位-拿起、目标定位-放下，琥珀天心-拿起、琥珀天心-放下。游戏里的声音和录音环境不完全一样，认不出的用 F8 再补一条即可。

## 录入时注意

F8 存下来的是一整段，真正的声音往往只有其中几十毫秒。入库时程序会对准这一小段最响的地方做声纹，前后的安静背景不会被算进去。

听起来一样的声音不要拆成好几条。两条模板太像时，识别会两边都不敢报，或者报错。只保留彼此分得开的声音，阈值按条目单独调。默认门槛大约 0.75，两条容易抢的可以再抬高。

更细的录音方法见 [录音与打标指南.md](录音与打标指南.md)，日常操作见 [使用手册.md](使用手册.md)。

## 从源码编译

需要 Go 1.27 或更高，在 Windows 上编译。不需要安装 gcc。

```powershell
Set-ExecutionPolicy -Scope Process -ExecutionPolicy Bypass -Force
cd soundradar
.\build.ps1
```

编出来的文件是 `soundradar\bin\soundradar.exe`。脚本会关掉 CGO，依赖从 `goproxy.cn` 下载。这台机器开了 Smart App Control 时，脚本会换构建盐重编，直到这个 exe 能真正启动。

常用命令：

| 命令 | 作用 |
|---|---|
| `soundradar devices` | 列出播放设备 |
| `soundradar serve --overlay --open` | 管理页面 + 实时识别 + 悬浮窗 |
| `soundradar match --wav 录音.wav` | 离线看这段录音最像库里的哪一条 |
| `soundradar index rebuild` | 手动重建指纹索引 |
| `soundradar version` | 看版本 |
