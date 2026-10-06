# 视频 Alpha 叠加合成工具（Fyne + MovieGo v2）

一个基于 **Fyne** GUI 与 **moviego v2**（`github.com/mowshon/moviego/v2`）的桌面工具：
把 **3840×1080** 的左右拼接视频（左半 = 黑底原视频 RGB，右半 = 左半画面的
Alpha 亮度通道）拆开，用右半亮度作为 Alpha 将左半画面分别叠加到
**黑底 / 纯绿底 / 纯蓝底** 上，输出三个 **1920×1080** 的 MP4。

## 合成原理

输入左半是“黑底的原视频”，即 **预乘（premultiplied）** 画面：
`rgb = color × α`。叠加到纯色背景的正确公式是：

```
out = rgb + bg × (1 − α)
```

moviego v2 的 `Composite` 使用 straight-alpha 混合（`fg×α + bg×(1−α)`），
因此本工具通过一个自定义 moviego 效果节点（`compositor.go` 中的
`sideBySideAlphaNode`）先做 **反预乘**（`fg = rgb / α`），再交给
`Composite` 混合，结果恰好等于上面的正确公式：

- 黑底：`rgb`（即左半原样裁出，走 FFmpeg 融合快路径，不重编码整个画面）
- 绿底：`rgb + (0,255,0)×(1−α)`
- 蓝底：`rgb + (0,0,255)×(1−α)`

每个像素的验证：见 `testdata/` 下的合成测试视频与校验脚本（18 项条纹
像素全部通过，误差 ≤3/255，为 yuv420p 编解码噪声）。

## 验证

本项目已通过三层验证（详见 `testdata/`）：

1. **合成条纹测试**（`testdata/test_3840.mp4`，6 组已知 RGB/Alpha 条纹）：
   三路输出 18 项采样像素全部与理论公式一致（误差 ≤3/255，为编解码噪声）。
2. **真实视频端到端**（`enter.15923c.mp4`、`enter.3f3dd4.mp4`）：
   6 个输出均为 1920×1080、30fps，时长/帧数与源完全一致
   （15.2s/456 帧、10.633s/319 帧）。
3. **数学精确性**（`go test -run TestCompositeMathRealFrame -v`）：
   绕过 ffmpeg 编码直接取 moviego Go 渲染管线的原始 RGB 帧，
   与 `out = rgb + bg×(1−α)` 的整数运算**逐位一致（100.00%，最大偏差 0）**。
   视频文件里的边缘伪影全部来自 yuv420p/x264 有损编码，与合成算法无关。

## 构建

依赖：Go 1.25+、Xcode Command Line Tools（macOS 上 Fyne 需要 CGO）、
系统 `PATH` 中有 **ffmpeg** 与 **ffprobe**（也可在 GUI 高级设置里指定，
或用环境变量 `MGO_FFMPEG` / `MGO_FFPROBE`）。

```sh
go mod tidy
go build -o silhouette .
```

> 本仓库的 go.mod 已固定 `fyne.io/fyne/v2 v2.8.1` 与
> `github.com/mowshon/moviego/v2 v2.0.2`。GUI 中文字体使用内嵌的
> Droid Sans Fallback（Apache-2.0，AOSP）。

## 使用

图形界面：

```sh
./silhouette
```

1. 点击「选择视频…」选择 3840×1080 的输入视频（会自动读取分辨率/时长/帧率/音频）；
2. 输出目录默认与输入相同，可修改；
3. 点击「开始合成」，依次渲染黑底、绿底、蓝底三个视频：
   `<原名>_black.mp4`、`<原名>_green.mp4`、`<原名>_blue.mp4`；
4. 完成后可点「打开输出文件夹」。

命令行 / 自动化（无界面模式）：

```sh
./silhouette -input 输入.mp4 -out 输出目录 -crf 18
```

## 说明与限制

- 输入必须是 **3840×1080**，否则直接报错（其它分辨率请先预处理）。
- 输出固定为 1920×1080、30fps（沿用输入帧率）、H.264 + yuv420p，
  音频若存在会保留（黑底走融合路径时同样保留）。
- 黑底输出走 moviego 的 FFmpeg 融合快路径（纯 crop 不经过 Go 像素管线，
  速度快）；绿底/蓝底走 moviego 的 Go 渲染管线（自定义 Alpha 节点无法融合），
  单帧解码一次 3840×1080，速度取决于机器。
- 绿底/蓝底为纯色 `#00FF00` / `#0000FF`（chroma key 用途）；如需其它底色，
  改 `compositor.go` 里的 `BgGreen` / `BgBlue` 即可。
- 「取消」会终止当前渲染（context 取消，moviego 会杀掉 ffmpeg 进程）。

## 文件结构

```
main.go            Fyne GUI（+ 无界面 CLI 模式，供自动化/测试）
compositor.go      合成管线：moviego v2 Probe/OpenVideo/Composite/WriteVideo
                   + 自定义“左 RGB + 右 Alpha”效果节点（反预乘）
compositor_test.go 单帧数学精确性测试（绕过编码，逐位比对）
assets/            内嵌中文字体
testdata/          合成测试视频与校验数据
```
