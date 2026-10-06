package main

import (
	"context"
	_ "embed"
	"flag"
	"fmt"
	"image/color"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/data/binding"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/storage"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	mgo "github.com/mowshon/moviego/v2"
)

//go:embed assets/DroidSansFallback.ttf
var cjkFontData []byte

// cjkFont 是嵌入的中文字体（fyne 自带字体不含 CJK 字形）。
var cjkFont = fyne.NewStaticResource("DroidSansFallback.ttf", cjkFontData)

// cjkTheme 用嵌入的中文字体覆盖默认主题的 Font。
type cjkTheme struct{ fyne.Theme }

func (t *cjkTheme) Font(style fyne.TextStyle) fyne.Resource {
	if style.Monospace {
		return t.Theme.Font(style)
	}
	return cjkFont
}

func main() {
	input := flag.String("input", "", "无界面模式：输入视频路径（3840x1080）")
	out := flag.String("out", "", "无界面模式：输出目录（默认与输入相同）")
	crf := flag.Int("crf", 18, "无界面模式：x264 CRF 质量")
	flag.Parse()

	if *input != "" {
		runHeadless(*input, *out, *crf)
		return
	}
	runGUI()
}

// runHeadless 供命令行/自动化使用：打印进度并退出。
func runHeadless(input, out string, crf int) {
	if out == "" {
		out = filepath.Dir(input)
	}
	cfg := ProcessConfig{
		CRF: crf,
		Logf: func(format string, args ...any) {
			fmt.Printf("[%s] %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
		},
		StageProgress: func(stage int, name string, done, total int) {
			fmt.Printf("\r[%s] %s (%d/3): %d/%d 帧 %3.0f%%", time.Now().Format("15:04:05"),
				name, stage+1, done, total, 100*float64(done)/float64(max(total, 1)))
		},
	}
	results, err := Process(context.Background(), input, out, cfg)
	fmt.Println()
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
	fmt.Println("完成，输出文件：")
	for _, r := range results {
		fmt.Println(" ", r)
	}
}

func runGUI() {
	a := app.NewWithID("com.operator.silhouette")
	a.Settings().SetTheme(&cjkTheme{Theme: theme.DefaultTheme()})
	w := a.NewWindow("视频 Alpha 叠加合成工具 — 3840×1080 → 1920×1080 黑底/绿底/蓝底")
	w.Resize(fyne.NewSize(880, 700))

	// ---- 绑定 ----
	bInput := binding.NewString()
	bOutDir := binding.NewString()
	bInfo := binding.NewString()
	bStatus := binding.NewString()
	bProgress := binding.NewFloat()
	bLog := binding.NewString()
	bFFmpeg := binding.NewString()
	bFFprobe := binding.NewString()
	bCRF := binding.NewString()
	bCRF.Set("18")

	// ---- 输入视频 ----
	inputEntry := widget.NewEntry()
	inputEntry.Bind(bInput)
	inputEntry.SetPlaceHolder("选择或输入 3840×1080 视频路径…")

	applyToolchainEnv := func() {
		if p, _ := bFFmpeg.Get(); strings.TrimSpace(p) != "" {
			os.Setenv("MGO_FFMPEG", strings.TrimSpace(p))
		}
		if p, _ := bFFprobe.Get(); strings.TrimSpace(p) != "" {
			os.Setenv("MGO_FFPROBE", strings.TrimSpace(p))
		}
	}

	onInputSelected := func(path string) {
		bInput.Set(path)
		if cur, _ := bOutDir.Get(); cur == "" {
			bOutDir.Set(filepath.Dir(path))
		}
		applyToolchainEnv()
		go func() {
			bInfo.Set("正在读取视频信息…")
			info, err := mgo.Probe(path)
			if err != nil {
				bInfo.Set("无法读取视频信息：" + err.Error())
				return
			}
			fps := 0.0
			if info.Rate.Den != 0 {
				fps = float64(info.Rate.Num) / float64(info.Rate.Den)
			}
			audioDesc := "无音频"
			if info.Audio != nil {
				audioDesc = "含音频 (" + info.Audio.Codec + ")"
			}
			ok := "✓"
			if info.Size.W != SourceWidth || info.Size.H != SourceHeight {
				ok = "✗ 尺寸不符"
			}
			bInfo.Set(fmt.Sprintf("%s %d×%d · %.3g fps · %s · %s · %s",
				ok, info.Size.W, info.Size.H, fps, info.Duration.Round(time.Millisecond), info.PixFmt, audioDesc))
		}()
	}

	pickBtn := widget.NewButton("选择视频…", func() {
		fd := dialog.NewFileOpen(func(rc fyne.URIReadCloser, err error) {
			if err != nil {
				dialog.ShowError(err, w)
				return
			}
			if rc == nil {
				return
			}
			path := rc.URI().Path()
			rc.Close()
			onInputSelected(path)
		}, w)
		fd.SetFilter(storage.NewExtensionFileFilter([]string{".mp4", ".mov", ".mkv", ".avi", ".webm", ".m4v", ".ts", ".mts"}))
		fd.Show()
	})

	// ---- 输出目录 ----
	outEntry := widget.NewEntry()
	outEntry.Bind(bOutDir)
	outEntry.SetPlaceHolder("输出目录（默认与输入相同）…")

	pickDirBtn := widget.NewButton("选择目录…", func() {
		fd := dialog.NewFolderOpen(func(lu fyne.ListableURI, err error) {
			if err != nil {
				dialog.ShowError(err, w)
				return
			}
			if lu == nil {
				return
			}
			bOutDir.Set(lu.Path())
		}, w)
		fd.Show()
	})

	// ---- 方案说明 ----
	desc := widget.NewLabel("输入 3840×1080：左半 = 黑底原视频（RGB），右半 = Alpha 亮度。\n将左半画面以右半亮度为 Alpha 分别叠加到黑底 / 纯绿底 / 纯蓝底，输出三个 1920×1080 视频。")
	desc.Wrapping = fyne.TextWrapWord

	greenRect := canvas.NewRectangle(color.NRGBA{R: 0, G: 255, B: 0, A: 255})
	greenRect.SetMinSize(fyne.NewSize(28, 20))
	blueRect := canvas.NewRectangle(color.NRGBA{R: 0, G: 0, B: 255, A: 255})
	blueRect.SetMinSize(fyne.NewSize(28, 20))
	scheme := container.NewHBox(
		widget.NewLabel("合成底色："),
		greenRect, widget.NewLabel("#00FF00"),
		blueRect, widget.NewLabel("#0000FF"),
		widget.NewLabel("  输出尺寸 1920×1080 · 30fps"),
	)

	// ---- 高级设置 ----
	ffmpegEntry := widget.NewEntry()
	ffmpegEntry.Bind(bFFmpeg)
	ffmpegEntry.SetPlaceHolder("ffmpeg 绝对路径（留空则自动查找 PATH / MGO_FFMPEG）")
	ffprobeEntry := widget.NewEntry()
	ffprobeEntry.Bind(bFFprobe)
	ffprobeEntry.SetPlaceHolder("ffprobe 绝对路径（留空则自动查找 PATH / MGO_FFPROBE）")
	crfEntry := widget.NewEntry()
	crfEntry.Bind(bCRF)
	crfEntry.SetPlaceHolder("18")

	advanced := widget.NewAccordion(
		widget.NewAccordionItem("高级设置", container.NewVBox(
			container.NewBorder(nil, nil, widget.NewLabel("ffmpeg "), nil, ffmpegEntry),
			container.NewBorder(nil, nil, widget.NewLabel("ffprobe"), nil, ffprobeEntry),
			container.NewBorder(nil, nil, widget.NewLabel("CRF 质量"), nil, crfEntry),
			widget.NewLabel("提示：自定义 ffmpeg 路径需在首次选择视频之前填好。"),
		)),
	)

	// ---- 进度与日志 ----
	progressBar := widget.NewProgressBar()
	progressBar.Bind(bProgress)
	statusLabel := widget.NewLabel("就绪")
	statusLabel.TextStyle = fyne.TextStyle{Bold: true}
	statusLabel.Bind(bStatus)
	logLabel := widget.NewLabel("")
	logLabel.Wrapping = fyne.TextWrapWord
	logLabel.Bind(bLog)

	appendLog := func(format string, args ...any) {
		var mu sync.Mutex
		mu.Lock()
		defer mu.Unlock()
		cur, _ := bLog.Get()
		line := fmt.Sprintf(format, args...)
		if cur == "" {
			bLog.Set(line)
		} else {
			bLog.Set(cur + "\n" + line)
		}
	}

	// ---- 运行控制 ----
	var (
		cancelMu sync.Mutex
		cancelFn context.CancelFunc
	)

	var startBtn, cancelBtn, openBtn *widget.Button
	startBtn = widget.NewButton("开始合成", func() {
		in, _ := bInput.Get()
		in = strings.TrimSpace(in)
		if in == "" {
			dialog.ShowInformation("提示", "请先选择输入视频文件", w)
			return
		}
		if _, err := os.Stat(in); err != nil {
			dialog.ShowError(fmt.Errorf("输入文件不存在：%v", err), w)
			return
		}
		outDir, _ := bOutDir.Get()
		outDir = strings.TrimSpace(outDir)
		if outDir == "" {
			outDir = filepath.Dir(in)
			bOutDir.Set(outDir)
		}
		crf := 18
		if v, err := strconv.Atoi(strings.TrimSpace(mustGet(bCRF))); err == nil && v >= 0 && v <= 51 {
			crf = v
		}
		applyToolchainEnv()

		ctx, cancel := context.WithCancel(context.Background())
		cancelMu.Lock()
		cancelFn = cancel
		cancelMu.Unlock()

		startBtn.Disable()
		cancelBtn.Enable()
		bProgress.Set(0)
		bLog.Set("")
		bStatus.Set("准备中…")

		cfg := ProcessConfig{
			CRF: crf,
			Logf: func(format string, args ...any) {
				appendLog(format, args...)
			},
			StageProgress: func(stage int, name string, done, total int) {
				frac := float64(stage)/3 + (float64(done)/float64(max(total, 1)))/3
				bProgress.Set(frac)
				bStatus.Set(fmt.Sprintf("正在渲染 %s (%d/3) — %d/%d 帧", name, stage+1, done, total))
			},
		}

		go func() {
			defer func() {
				cancelMu.Lock()
				cancelFn = nil
				cancelMu.Unlock()
			}()
			results, err := Process(ctx, in, outDir, cfg)
			fyne.Do(func() {
				startBtn.Enable()
				cancelBtn.Disable()
			})
			if err != nil {
				appendLog("失败: %v", err)
				bStatus.Set("合成失败")
				return
			}
			bProgress.Set(1)
			bStatus.Set("合成完成 ✓")
			appendLog("全部完成，输出：")
			for _, r := range results {
				appendLog("  %s", r)
			}
		}()
	})

	cancelBtn = widget.NewButton("取消", func() {
		cancelMu.Lock()
		defer cancelMu.Unlock()
		if cancelFn != nil {
			cancelFn()
		}
		bStatus.Set("正在取消…")
	})
	cancelBtn.Disable()

	openBtn = widget.NewButton("打开输出文件夹", func() {
		dir, _ := bOutDir.Get()
		if dir == "" {
			return
		}
		_ = exec.Command("open", dir).Start()
	})

	// ---- 布局 ----
	// 上方为固定控件；日志区放在 Border 布局中央，随窗口拉伸自动占满剩余高度。
	top := container.NewVBox(
		desc,
		container.NewBorder(nil, nil, widget.NewLabel("输入视频"), pickBtn, inputEntry),
		widget.NewLabel("视频信息"),
		wrapBindLabel(bInfo),
		container.NewBorder(nil, nil, widget.NewLabel("输出目录"), pickDirBtn, outEntry),
		scheme,
		advanced,
		container.NewHBox(startBtn, cancelBtn, openBtn),
		progressBar,
		statusLabel,
		widget.NewLabel("日志："),
	)

	content := container.NewBorder(top, nil, nil, nil, container.NewVScroll(logLabel))

	w.SetContent(container.NewPadded(content))
	w.ShowAndRun()
}

// wrapBindLabel 返回绑定到字符串的自动换行 Label。
func wrapBindLabel(b binding.String) *widget.Label {
	l := widget.NewLabel("")
	l.Wrapping = fyne.TextWrapWord
	l.Bind(b)
	return l
}

func mustGet(b binding.String) string {
	v, _ := b.Get()
	return v
}
