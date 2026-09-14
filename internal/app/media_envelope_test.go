package app

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// 用线上真实信封形状构造测试样本：field1=元数据、field2={field1:"video/mp4",
// field2={field1:mp4}}、trailer field3。
func makeEnvelope(t *testing.T) []byte {
	t.Helper()
	var mp4 []byte
	mp4 = append(mp4, 0x00, 0x00, 0x00, 0x20) // ftyp box 长度
	mp4 = append(mp4, []byte("ftypisom")...)
	mp4 = append(mp4, make([]byte, 128)...)

	lenPrefix := func(b []byte) []byte {
		// 完整 varint 编码（mp4 是 128+ 字节，需要多字节 varint）
		var out []byte
		n := len(b)
		for {
			x := byte(n & 0x7f)
			n >>= 7
			if n != 0 {
				x |= 0x80
			}
			out = append(out, x)
			if n == 0 {
				break
			}
		}
		return append(out, b...)
	}

	inner := append([]byte{0x0a}, lenPrefix(mp4)...)                          // field1: mp4
	resource := append([]byte{0x12}, lenPrefix([]byte("video/mp4"))...)       // field1: mime
	resource = append(resource, append([]byte{0x12}, lenPrefix(inner)...)...) // field2: {mp4}

	meta := append([]byte{0x0a}, lenPrefix([]byte("a red balloon"))...)
	out := append([]byte{0x0a}, lenPrefix(meta)...)
	out = append(out, append([]byte{0x12}, lenPrefix(resource)...)...)
	out = append(out, append([]byte{0x1a}, lenPrefix([]byte("Current time is Monday"))...)...)
	return out
}

func TestExtractVideoFromEnvelope(t *testing.T) {
	env := makeEnvelope(t)
	mp4, mime := extractVideoFromEnvelope(env)
	if mp4 == nil {
		t.Fatal("没能从信封里剥出 mp4")
	}
	if mime != "video/mp4" {
		t.Errorf("mime = %q, want video/mp4", mime)
	}
	if !looksLikeMP4(mp4) {
		t.Errorf("剥出的不是以 ftyp 开头的 mp4: %x", mp4[:8])
	}
	// mp4 必须完整（到我们写进去的长度），不能带 trailer
	if bytes.Contains(mp4, []byte("Current time")) {
		t.Error("trailer 没剥干净")
	}
}

func TestExtractVideoFromEnvelopePlainMP3(t *testing.T) {
	// 裸 mp3（非 protobuf）不应被错误处理
	if mp4, _ := extractVideoFromEnvelope([]byte("ID3raw mp3 data")); mp4 != nil {
		t.Error("非 protobuf 数据被误剥")
	}
}

func TestMP4BoxLen(t *testing.T) {
	// ftyp box 头：4 字节大端长度 + "ftyp" + brand（isom）
	box := make([]byte, 16)
	binary.BigEndian.PutUint32(box, 32)
	copy(box[4:], "ftypisom")
	if !looksLikeMP4(box) {
		t.Error("ftyp 识别失败")
	}
	// 非 ftyp 开头的不算
	if looksLikeMP4([]byte("not an mp4 at all........")) {
		t.Error("非 mp4 数据被误认")
	}
}

// TestExtractVideoEnvelopeDeepField1 覆盖第二个线上样本：资源子消息出现在
// 顶层第二个 field1（不是 field2），且 mime/mp4 再包一层 field3。
func TestExtractVideoEnvelopeDeepField1(t *testing.T) {
	lenPrefix := func(b []byte) []byte {
		var out []byte
		n := len(b)
		for {
			x := byte(n & 0x7f)
			n >>= 7
			if n != 0 {
				x |= 0x80
			}
			out = append(out, x)
			if n == 0 {
				break
			}
		}
		return append(out, b...)
	}
	var mp4 []byte
	mp4 = append(mp4, 0x00, 0x00, 0x00, 0x20)
	mp4 = append(mp4, []byte("ftypisom")...)
	mp4 = append(mp4, make([]byte, 256)...)

	res := append([]byte{0x0a}, lenPrefix([]byte("video/mp4"))...) // field1: mime
	res = append(res, append([]byte{0x12}, lenPrefix(mp4)...)...)  // field2: mp4
	sub := append([]byte{0x1a}, lenPrefix(res)...)                 // field3: {res}
	innerMeta := append([]byte{0x12}, lenPrefix([]byte("To satisfy ..."))...)
	big := append([]byte{0x0a}, lenPrefix(innerMeta)...)          // field1: 文本
	big = append(big, append([]byte{0x0a}, lenPrefix(sub)...)...) // 第二个 field1: 资源!
	big = append(big, append([]byte{0x12}, lenPrefix([]byte("model"))...)...)

	meta := append([]byte{0x0a}, lenPrefix([]byte("a red balloon"))...)
	out := append([]byte{0x0a}, lenPrefix(meta)...)
	out = append(out, append([]byte{0x0a}, lenPrefix(big)...)...) // 顶层第二个 field1

	got, mime := extractVideoFromEnvelope(out)
	if got == nil {
		t.Fatal("深嵌套信封没剥出 mp4")
	}
	if mime != "video/mp4" {
		t.Errorf("mime = %q", mime)
	}
	if !looksLikeMP4(got) {
		t.Error("剥出的不是 ftyp 开头")
	}
	if bytes.Contains(got, []byte("model")) {
		t.Error("混入了非 mp4 尾部")
	}
}
