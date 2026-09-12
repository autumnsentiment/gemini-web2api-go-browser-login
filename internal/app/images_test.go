package app

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func pngBytes(t *testing.T, w, h int, fill color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, fill)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png encode: %v", err)
	}
	return buf.Bytes()
}

// 上游回的是重新压过的 PNG（实测 1288B 进、1753B 出，像素一样），必须认出来。
func TestEchoArtifactsDetectsReencodedCopy(t *testing.T) {
	square := pngBytes(t, 64, 64, color.RGBA{220, 40, 40, 255})
	inputs := []pendingUpload{{Data: square, Mime: "image/png", Kind: 1}}

	// 同一画面换一种压缩级别重新编码：字节必然不同，像素完全一致。
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			img.Set(x, y, color.RGBA{220, 40, 40, 255})
		}
	}
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, img); err != nil {
		t.Fatalf("png encode: %v", err)
	}
	if bytes.Equal(buf.Bytes(), square) {
		t.Skip("encoder produced identical bytes; re-encode premise not met")
	}
	arts := []MediaArtifact{{Mime: "image/png", Data: buf.Bytes()}}
	if !isEchoArtifacts(arts, inputs) {
		t.Fatal("re-encoded copy of the input should be detected as an echo")
	}
}

// 小范围但可见的编辑（画布 2.7% 的圆形区域换色）不能误判成回声。
//
// 这里刻意不测「只改 1 个像素」：在 64x64 画布上单像素差异占比 0.024%，比重编码
// 噪声还小，任何全局指标都分不出来。取舍见 nearEchoRatio 的注释 —— 漏判回声的
// 代价是用户拿到一张没改过的图，误判的代价只是重打一次（外层有重试兜底）。
func TestEchoArtifactsKeepsSmallVisibleEdit(t *testing.T) {
	square := pngBytes(t, 64, 64, color.RGBA{220, 40, 40, 255})
	inputs := []pendingUpload{{Data: square, Mime: "image/png", Kind: 1}}

	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			img.Set(x, y, color.RGBA{220, 40, 40, 255})
		}
	}
	// 半径 6 的圆 ≈ 113 像素 ≈ 2.7%，远超 nearEchoRatio 允许的 0.5%。
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			dx, dy := x-32, y-32
			if dx*dx+dy*dy <= 36 {
				img.Set(x, y, color.RGBA{40, 40, 220, 255})
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png encode: %v", err)
	}
	arts := []MediaArtifact{{Mime: "image/png", Data: buf.Bytes()}}
	if isEchoArtifacts(arts, inputs) {
		t.Fatal("a small but visible edit must not be treated as an echo")
	}
}

func TestEchoArtifactsNoInputs(t *testing.T) {
	arts := []MediaArtifact{{Mime: "image/png", Data: pngBytes(t, 8, 8, color.RGBA{0, 0, 0, 255})}}
	if isEchoArtifacts(arts, nil) {
		t.Fatal("no inputs means nothing can be an echo")
	}
}

// 解不开的产物（JPEG 压缩图 / 未知格式）放行，别把真结果误判成回声。
func TestEchoArtifactsUndecodableIsNotEcho(t *testing.T) {
	square := pngBytes(t, 32, 32, color.RGBA{1, 2, 3, 255})
	inputs := []pendingUpload{{Data: square, Mime: "image/png", Kind: 1}}
	arts := []MediaArtifact{{Mime: "image/png", Data: []byte("not an image at all")}}
	if isEchoArtifacts(arts, inputs) {
		t.Fatal("undecodable artifact must not be flagged as an echo")
	}
}

func TestDecodePixelsNormalizesLayout(t *testing.T) {
	a := pngBytes(t, 16, 16, color.RGBA{9, 8, 7, 255})
	// 用 NRGBA 走另一条编码路径，解出来应当归一到同一份 RGBA 字节。
	img := image.NewNRGBA(image.Rect(0, 0, 16, 16))
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			img.SetNRGBA(x, y, color.NRGBA{9, 8, 7, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png encode: %v", err)
	}
	pa, _, _, okA := decodePixels(a)
	pb, _, _, okB := decodePixels(buf.Bytes())
	if !okA || !okB {
		t.Fatalf("decode failed: %v %v", okA, okB)
	}
	if !bytes.Equal(pa, pb) {
		t.Fatal("same picture encoded two ways should decode to identical pixels")
	}
}

// 真实回声：上游把输入图重新编码后原样送回来。实测 1408x768 的图，回声产物
// 最大通道差 8、差>16 的像素为 0 —— 严格逐字节比对会漏判，必须按近似像素比。
func TestEchoArtifactsDetectsReencodedNoise(t *testing.T) {
	src := pngBytes(t, 96, 96, color.RGBA{200, 30, 30, 255})
	inputs := []pendingUpload{{Data: src, Mime: "image/png", Kind: 1}}

	// 模拟「重新压缩」：每个通道抖动 ±8（实测上限），再换一种编码器写出去。
	img := image.NewRGBA(image.Rect(0, 0, 96, 96))
	for y := 0; y < 96; y++ {
		for x := 0; x < 96; x++ {
			j := (x*7 + y*13) % 17 // 伪随机但可复现的抖动 0..16
			d := j - 8
			img.Set(x, y, color.RGBA{
				clamp8(200 + d), clamp8(30 + d), clamp8(30 + d), 255,
			})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png encode: %v", err)
	}
	if bytes.Equal(buf.Bytes(), src) {
		t.Fatal("premise broken: re-encode produced identical bytes")
	}
	arts := []MediaArtifact{{Mime: "image/png", Data: buf.Bytes()}}
	if !isEchoArtifacts(arts, inputs) {
		t.Fatal("a re-encoded copy with <=8 LSB noise must be detected as an echo")
	}
}

// 真编辑：改了 2% 的像素（远超噪声），不能误判成回声。
func TestEchoArtifactsRealEditLargeRegionNotEcho(t *testing.T) {
	src := pngBytes(t, 100, 100, color.RGBA{200, 30, 30, 255})
	inputs := []pendingUpload{{Data: src, Mime: "image/png", Kind: 1}}

	img := image.NewRGBA(image.Rect(0, 0, 100, 100))
	for y := 0; y < 100; y++ {
		for x := 0; x < 100; x++ {
			if x < 20 && y < 20 { // 4% 的区域改成蓝色
				img.Set(x, y, color.RGBA{30, 30, 200, 255})
				continue
			}
			img.Set(x, y, color.RGBA{200, 30, 30, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png encode: %v", err)
	}
	arts := []MediaArtifact{{Mime: "image/png", Data: buf.Bytes()}}
	if isEchoArtifacts(arts, inputs) {
		t.Fatal("a 4% region edit must not be treated as an echo")
	}
}

// 尺寸变了就不算回声（上游要真改画幅就放行，宁可放过不可错杀）。
func TestEchoArtifactsDifferentSizeNotEcho(t *testing.T) {
	src := pngBytes(t, 64, 64, color.RGBA{10, 20, 30, 255})
	inputs := []pendingUpload{{Data: src, Mime: "image/png", Kind: 1}}
	arts := []MediaArtifact{{Mime: "image/png", Data: pngBytes(t, 32, 32, color.RGBA{10, 20, 30, 255})}}
	if isEchoArtifacts(arts, inputs) {
		t.Fatal("different dimensions must not be treated as an echo")
	}
}

func clamp8(v int) uint8 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v)
}
