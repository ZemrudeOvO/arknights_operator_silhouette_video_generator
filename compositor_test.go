package main

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/mowshon/moviego/v2/clip"
	"github.com/mowshon/moviego/v2/composite"
	"github.com/mowshon/moviego/v2/effect"
	"github.com/mowshon/moviego/v2/video"
)

// TestCompositeMathRealFrame 绕过 ffmpeg 编码，直接用 moviego 的 Go 渲染管线
// 输出真实视频第 100 帧的合成结果（原始 RGB24 字节），供外部逐像素精确比对。
// 用法：go test -run TestCompositeMathRealFrame -v
func TestCompositeMathRealFrame(t *testing.T) {
	path := "enter.15923c.mp4"
	if _, err := os.Stat(path); err != nil {
		t.Skip("缺少真实测试视频 enter.15923c.mp4")
	}
	ctx := context.Background()
	n, err := video.OpenFile(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()

	ft := clip.Time(100 * int64(time.Second) / 30) // 与 rate.FrameTime(100) 一致（第 100 帧 @30fps）

	// 黑底：左半裁剪
	left, err := (effect.Crop{X: 0, Y: 0, W: OutWidth, H: OutHeight}).ApplyVideo(n)
	if err != nil {
		t.Fatal(err)
	}
	if err := dumpRaw(t, ctx, left, ft, "testdata/math_black_f100.raw"); err != nil {
		t.Fatal(err)
	}

	// 绿底 / 蓝底：反预乘节点 + Composite（与 compositor.go 的管线完全一致）
	rgba, err := (sideBySideAlphaEffect{}).ApplyVideo(n)
	if err != nil {
		t.Fatal(err)
	}
	for _, bg := range []BgColor{BgGreen, BgBlue} {
		comp := composite.New([]composite.CompositeChild{
			{Clip: rgba, Start: 0, Pos: composite.Position{}, Layer: 0},
		}, composite.Options{BGColor: bg.RGB})
		name := "testdata/math_green_f100.raw"
		if bg.Name == "蓝底" {
			name = "testdata/math_blue_f100.raw"
		}
		if err := dumpRaw(t, ctx, comp, ft, name); err != nil {
			t.Fatal(err)
		}
	}
	t.Log("已写出 testdata/math_{black,green,blue}_f100.raw")
}

func dumpRaw(t *testing.T, ctx context.Context, v video.VideoClip, ft clip.Time, name string) error {
	t.Helper()
	size := v.Size()
	rgb := clip.NewFrame(size.W, size.H, clip.RGB24)
	if _, err := v.RenderInto(ctx, ft, rgb, nil); err != nil {
		return err
	}
	return os.WriteFile(name, rgb.Pix, 0o644)
}
