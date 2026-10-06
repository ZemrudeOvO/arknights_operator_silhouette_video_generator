package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	mgo "github.com/mowshon/moviego/v2"
	"github.com/mowshon/moviego/v2/audio"
	"github.com/mowshon/moviego/v2/clip"
	"github.com/mowshon/moviego/v2/effect"
	"github.com/mowshon/moviego/v2/video"
)

const (
	SourceWidth  = 3840
	SourceHeight = 1080
	OutWidth     = 1920
	OutHeight    = 1080
)

// BgColor 是一种输出底色。
type BgColor struct {
	Name string
	RGB  [3]byte
}

var (
	BgBlack = BgColor{Name: "黑底", RGB: [3]byte{0, 0, 0}}
	BgGreen = BgColor{Name: "绿底", RGB: [3]byte{0, 255, 0}}
	BgBlue  = BgColor{Name: "蓝底", RGB: [3]byte{0, 0, 255}}
)

// ProcessConfig 控制一次合成任务。
type ProcessConfig struct {
	FFmpegBinary  string // 可选，绝对路径；留空则使用 PATH / MGO_FFMPEG 环境变量
	FFprobeBinary string // 可选，绝对路径；留空则使用 PATH / MGO_FFPROBE 环境变量
	CRF           int    // x264 质量，默认 18
	Workers       int    // 渲染协程数，0 表示自动

	// Logf 接收进度日志（可空）。
	Logf func(format string, args ...any)
	// StageProgress 报告某一阶段的帧进度：stage 为 0(黑底)/1(绿底)/2(蓝底)。
	StageProgress func(stage int, name string, done, total int)
}

// Process 读取 3840x1080 的左右拼接视频：
//   - 左半 1920x1080：黑底（预乘）原视频
//   - 右半 1920x1080：左半画面的 Alpha 亮度通道
//
// 分别输出叠加到黑底 / 纯绿底 / 纯蓝底上的三个 1920x1080 视频，
// 返回三个输出文件的绝对路径。
func Process(ctx context.Context, input, outDir string, cfg ProcessConfig) ([]string, error) {
	if cfg.FFmpegBinary != "" {
		os.Setenv("MGO_FFMPEG", cfg.FFmpegBinary)
	}
	if cfg.FFprobeBinary != "" {
		os.Setenv("MGO_FFPROBE", cfg.FFprobeBinary)
	}
	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if cfg.CRF == 0 {
		cfg.CRF = 18
	}

	// 1. 探测元数据并校验尺寸
	info, err := mgo.Probe(input)
	if err != nil {
		return nil, fmt.Errorf("读取视频信息失败: %w", err)
	}
	if info.Size.W != SourceWidth || info.Size.H != SourceHeight {
		return nil, fmt.Errorf("输入视频必须为 %dx%d（左半画面 + 右半 Alpha），当前为 %dx%d",
			SourceWidth, SourceHeight, info.Size.W, info.Size.H)
	}
	fps := 0.0
	if info.Rate.Den != 0 {
		fps = float64(info.Rate.Num) / float64(info.Rate.Den)
	}
	audioDesc := "无音频"
	if info.Audio != nil {
		audioDesc = fmt.Sprintf("音频 %s", info.Audio.Codec)
	}
	logf("视频信息: %dx%d · %.3g fps · %s · %s · %s",
		info.Size.W, info.Size.H, fps, info.Duration.Round(1e6), info.PixFmt, audioDesc)

	// 2. 打开视频（解码器在导出时才懒创建）
	vid, err := mgo.OpenVideo(input)
	if err != nil {
		return nil, fmt.Errorf("打开视频失败: %w", err)
	}
	defer vid.Close()

	// 3. 输出路径
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建输出目录失败: %w", err)
	}
	base := strings.TrimSuffix(filepath.Base(input), filepath.Ext(input))
	outBlack := filepath.Join(outDir, base+"_black.mp4")
	outGreen := filepath.Join(outDir, base+"_green.mp4")
	outBlue := filepath.Join(outDir, base+"_blue.mp4")

	rate := clip.Rate{Num: info.Rate.Num, Den: info.Rate.Den}
	if rate.Num == 0 || rate.Den == 0 {
		rate = clip.Rate{Num: 30, Den: 1}
	}

	common := mgo.ExportOptions{
		Rate:    rate,
		Codec:   "libx264",
		Preset:  "medium",
		CRF:     cfg.CRF,
		PixFmt:  "yuv420p",
		Workers: cfg.Workers,
	}

	// 4. 黑底：左半画面本身就是黑底（预乘）结果，直接裁出。
	//    纯 crop 图可以走 moviego 的 FFmpeg 融合快路径（音频会一并保留）。
	stageCB := cfg.StageProgress
	if stageCB == nil {
		stageCB = func(int, string, int, int) {}
	}
	{
		left := vid.Crop(0, 0, OutWidth, OutHeight)
		opts := common
		opts.Progress = newStageProgress(func(done, total int) { stageCB(0, BgBlack.Name, done, total) })
		opts.EnableFusion = true
		opts.Debugf = func(format string, args ...any) {
			logf("[黑底] "+format, args...)
		}
		logf("开始渲染 黑底 → %s", outBlack)
		if err := mgo.WriteVideo(ctx, left, outBlack, opts); err != nil {
			return nil, fmt.Errorf("渲染黑底失败: %w", err)
		}
		logf("✓ 黑底完成: %s", outBlack)
	}

	// 5. 绿底 / 蓝底：把左半视频按右半亮度作 Alpha 叠加到纯色背景。
	//    左半是预乘（premultiplied）画面，moviego 的 Composite 使用
	//    straight-alpha 混合（fg*α + bg*(1-α)），因此先在自定义节点里
	//    做反预乘（fg = rgb/α），Composite 混合后恰好得到
	//    out = rgb + bg*(1-α) —— 即预乘画面叠加到纯色背景的正确结果。
	rgba := vid.Fx(sideBySideAlphaEffect{})
	targets := []struct {
		bg    BgColor
		out   string
		stage int
	}{
		{BgGreen, outGreen, 1},
		{BgBlue, outBlue, 2},
	}
	for _, t := range targets {
		comp := mgo.CompositeWith(mgo.CompositeOptions{BGColor: t.bg.RGB}, rgba)
		opts := common
		opts.Progress = newStageProgress(func(done, total int) { stageCB(t.stage, t.bg.Name, done, total) })
		logf("开始渲染 %s → %s", t.bg.Name, t.out)
		if err := mgo.WriteVideo(ctx, comp, t.out, opts); err != nil {
			return nil, fmt.Errorf("渲染%s失败: %w", t.bg.Name, err)
		}
		logf("✓ %s完成: %s", t.bg.Name, t.out)
	}

	return []string{outBlack, outGreen, outBlue}, nil
}

// ---------------------------------------------------------------------------
// 自定义 moviego 效果：把一个 3840x1080 的“左 RGB + 右 Alpha 亮度”视频
// 变成 1920x1080 的带蒙版视频。RGB 输出为反预乘后的 straight 颜色，
// 蒙版输出为右半边的亮度（Gray8）。
// ---------------------------------------------------------------------------

type sideBySideAlphaEffect struct{}

func (sideBySideAlphaEffect) Targets() effect.EffectTargets {
	return effect.EffectTargets{Video: true, Mask: true, Audio: true}
}

func (sideBySideAlphaEffect) ApplyVideo(v video.VideoClip) (video.VideoClip, error) {
	sz := v.Size()
	w, h := sz.W/2, sz.H
	if sz.W%2 != 0 || w <= 0 || h <= 0 {
		return nil, fmt.Errorf("输入尺寸 %dx%d 无法对半切分（宽度需为偶数）", sz.W, sz.H)
	}
	return &sideBySideAlphaNode{inner: v, size: clip.Size{W: w, H: h}}, nil
}

type sideBySideAlphaNode struct {
	inner video.VideoClip
	size  clip.Size
}

func (n *sideBySideAlphaNode) Start() clip.Time    { return n.inner.Start() }
func (n *sideBySideAlphaNode) Duration() clip.Time { return n.inner.Duration() }
func (n *sideBySideAlphaNode) End() clip.Time      { return n.inner.End() }

func (n *sideBySideAlphaNode) WithStart(t clip.Time) clip.Clip {
	c := *n
	c.inner = n.inner.WithStart(t).(video.VideoClip)
	return &c
}

func (n *sideBySideAlphaNode) WithDuration(d clip.Time) clip.Clip {
	c := *n
	c.inner = n.inner.WithDuration(d).(video.VideoClip)
	return &c
}

func (n *sideBySideAlphaNode) WithEnd(end clip.Time, changeDuration bool) clip.Clip {
	c := *n
	c.inner = n.inner.WithEnd(end, changeDuration).(video.VideoClip)
	return &c
}

func (n *sideBySideAlphaNode) Close() error    { return n.inner.Close() }
func (n *sideBySideAlphaNode) Size() clip.Size { return n.size }
func (n *sideBySideAlphaNode) Rate() (clip.Rate, bool) {
	return n.inner.Rate()
}
func (n *sideBySideAlphaNode) HasMask() bool          { return true }
func (n *sideBySideAlphaNode) Audio() audio.AudioClip { return n.inner.Audio() }
func (n *sideBySideAlphaNode) ParallelSafe() bool     { return n.inner.ParallelSafe() }
func (n *sideBySideAlphaNode) SourceAccess() video.AccessClass {
	return n.inner.SourceAccess()
}
func (n *sideBySideAlphaNode) Children() []video.VideoClip {
	return []video.VideoClip{n.inner}
}

// RenderInto 一次解码完整 3840x1080 帧，拆分出反预乘 RGB 与 Alpha。
func (n *sideBySideAlphaNode) RenderInto(ctx context.Context, t clip.Time, rgbDst, alphaDst *clip.Frame) (bool, error) {
	src, err := n.fetch(ctx, t)
	if err != nil {
		return false, err
	}
	defer src.Release()
	splitPremultiplied(src, rgbDst, alphaDst)
	return alphaDst != nil, nil
}

// FrameInto 只输出反预乘后的 RGB。
func (n *sideBySideAlphaNode) FrameInto(ctx context.Context, t clip.Time, dst *clip.Frame) error {
	src, err := n.fetch(ctx, t)
	if err != nil {
		return err
	}
	defer src.Release()
	mask := clip.NewFrame(n.size.W, n.size.H, clip.Gray8)
	splitPremultiplied(src, dst, mask)
	return nil
}

// MaskInto 只输出右半边亮度作为 Alpha。
func (n *sideBySideAlphaNode) MaskInto(ctx context.Context, t clip.Time, dst *clip.Frame) (bool, error) {
	src, err := n.fetch(ctx, t)
	if err != nil {
		return false, err
	}
	defer src.Release()
	fillLumaRightHalf(src, dst)
	return true, nil
}

func (n *sideBySideAlphaNode) fetch(ctx context.Context, t clip.Time) (*clip.Frame, error) {
	full := n.inner.Size()
	src := clip.NewFrame(full.W, full.H, clip.RGB24)
	if err := n.inner.FrameInto(ctx, t, src); err != nil {
		src.Release()
		return nil, err
	}
	return src, nil
}

// splitPremultiplied 把完整帧拆成左半（反预乘 RGB）与右半（亮度 Alpha）。
func splitPremultiplied(src, rgbDst, maskDst *clip.Frame) {
	w, h := rgbDst.W, rgbDst.H
	for y := 0; y < h; y++ {
		lRow := src.Pix[y*src.Stride : y*src.Stride+w*3]
		rRow := src.Pix[y*src.Stride+w*3 : y*src.Stride+2*w*3]
		dRow := rgbDst.Pix[y*rgbDst.Stride : y*rgbDst.Stride+w*3]
		aRow := maskDst.Pix[y*maskDst.Stride : y*maskDst.Stride+w]
		for x := 0; x < w; x++ {
			a := luma601(rRow[x*3], rRow[x*3+1], rRow[x*3+2])
			aRow[x] = a
			unpremultiplyPixel(lRow[x*3:x*3+3], dRow[x*3:x*3+3], a)
		}
	}
}

// fillLumaRightHalf 把右半边亮度写入 Gray8 蒙版帧。
func fillLumaRightHalf(src, maskDst *clip.Frame) {
	w, h := maskDst.W, maskDst.H
	for y := 0; y < h; y++ {
		rRow := src.Pix[y*src.Stride+w*3 : y*src.Stride+2*w*3]
		aRow := maskDst.Pix[y*maskDst.Stride : y*maskDst.Stride+w]
		for x := 0; x < w; x++ {
			aRow[x] = luma601(rRow[x*3], rRow[x*3+1], rRow[x*3+2])
		}
	}
}

// luma601 计算 Rec.601 亮度（右半是灰度视频，R≈G≈B，结果即灰度值）。
func luma601(r, g, b byte) byte {
	return byte((299*int(r) + 587*int(g) + 114*int(b) + 500) / 1000)
}

// unpremultiplyPixel 反预乘：fg = rgb * 255 / α（α=0 时置 0，α=255 时原样）。
func unpremultiplyPixel(src, dst []byte, a byte) {
	switch a {
	case 0:
		dst[0], dst[1], dst[2] = 0, 0, 0
		return
	case 255:
		dst[0], dst[1], dst[2] = src[0], src[1], src[2]
		return
	}
	for c := 0; c < 3; c++ {
		v := (int(src[c])*255 + int(a)/2) / int(a)
		if v > 255 {
			v = 255
		}
		dst[c] = byte(v)
	}
}

// ---------------------------------------------------------------------------
// 阶段进度（实现 moviego 的 render.Progress 接口）
// ---------------------------------------------------------------------------

type stageProgress struct {
	onStep func(done, total int)
	done   int
	total  int
}

func newStageProgress(onStep func(done, total int)) *stageProgress {
	return &stageProgress{onStep: onStep}
}

func (p *stageProgress) SetTotal(frames int) {
	p.total = frames
	p.emit()
}

func (p *stageProgress) Step() {
	p.done++
	p.emit()
}

func (p *stageProgress) emit() {
	if p.onStep != nil && p.total > 0 {
		p.onStep(p.done, p.total)
	}
}
