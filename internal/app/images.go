package app

// /v1/images/* —— OpenAI 图像 API 形状，内部转成 /v1/chat/completions 那条链。
//
// 上游（Gemini 网页版）只有一条生成通道：StreamGenerate + inner[49]=14（Nano Banana，
// 见 gemini.go 的 Models["gemini-image"]）。它同时管文生图和图生图 —— 区别只在请求里
// 带不带附件。所以这里不做两套逻辑，两个端点都收敛到 runImageRequest：
//
//	POST /v1/images/generations   JSON（gpt-image-1 风格，可选 image 字段）
//	POST /v1/images/edits         multipart/form-data（image 部件可重复，另有 mask）
//
// 之所以要有这两个端点：很多客户端（newapi / Cherry Studio / LobeChat 的画图页）
// 只认 OpenAI 图像 API，不会用 chat 端点发图，于是 gemini-image 在它们那里根本选不到。
//
// 产物回传：callGemini 内部已经把 MediaArtifact 走 hNvQHb 取回原始字节，
// 这里直接把它转成 OpenAI images 形状的 data[].b64_json —— 不解析正文里的
// data URL（那是给 chat 端点用的 markdown 形态）。

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

// maxImageUploadBytes 是单个上传部件的读取上限。比 vision.go 的 maxImageBytes 宽一点：
// 这里多一层 multipart 包装，且要留出「超限时报错」的余量（多读 1 字节判超）。
const maxImageUploadBytes = 24 << 20

// maxUploadTotalBytes 是整个请求体的硬上限，防止一个超大 body 把内存/磁盘吃满。
const maxUploadTotalBytes = 64 << 20

// maxImageFanout 是 n>1 时最多真打几次上游。Gemini 一次出一张（偶尔两张），
// n 只能靠多次请求凑，而每次都要几十秒 —— 封顶免得客户端一个 n=10 把出口打爆。
const maxImageFanout = 4

// imageEndpoints 把请求路径映射成 requests 表里的 endpoint 名。
func imageEndpoint(path string) string {
	if strings.Contains(path, "/edits") {
		return "images.edits"
	}
	return "images.generations"
}

// handleImageGenerations 处理 POST /v1/images/generations（JSON）。
// 有些客户端（gpt-image-1 那套）会往这个端点发 multipart，所以按 Content-Type 分流。
func handleImageGenerations(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		handleImageEdits(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadTotalBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, 400, imageErr("invalid request body: "+err.Error(), "invalid_request_error"))
		return
	}
	var req map[string]interface{}
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, 400, imageErr("invalid JSON", "invalid_request_error"))
		return
	}
	// image 字段（可选）：data URL / http(s) 链接，单个或数组。带它就是图生图。
	images, err := imagesFromAny(req["image"])
	if err != nil {
		writeJSON(w, 400, imageErr(err.Error(), "invalid_request_error"))
		return
	}
	if extra, err := imagesFromAny(req["images"]); err == nil {
		images = append(images, extra...)
	} else {
		writeJSON(w, 400, imageErr(err.Error(), "invalid_request_error"))
		return
	}
	runImageRequest(w, r,
		getStr(req, "prompt"),
		images,
		getStr(req, "model"),
		getStr(req, "size"),
		getStr(req, "response_format"),
		intOf(req["n"], 1),
	)
}

// handleImageEdits 处理 POST /v1/images/edits（multipart）。
//
// 认这些部件：
//
//	image / image[]   源图，可重复（OpenAI 标准）
//	mask              遮罩。上游没有 mask 概念，所以当第二张图带上、并在 prompt 里说明；
//	                  留空比硬塞更安全，但客户端多半会一起传，说明清楚模型反而知道要改哪儿。
//	prompt / model / n / size / response_format / user
//
// 也兼容把源图当**字符串字段**发（data URL）而不是文件部件的客户端。
func handleImageEdits(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		// 少量客户端把 edits 也写成 JSON + data URL。
		handleImageGenerations(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadTotalBytes)
	if err := r.ParseMultipartForm(16 << 20); err != nil {
		writeJSON(w, 400, imageErr("invalid multipart body: "+err.Error(), "invalid_request_error"))
		return
	}
	var images []pendingUpload
	for _, key := range []string{"image", "image[]"} {
		for _, fh := range r.MultipartForm.File[key] {
			data, mime, err := readImagePart(fh)
			if err != nil {
				writeJSON(w, 400, imageErr(fmt.Sprintf("%s: %v", key, err), "invalid_request_error"))
				return
			}
			pu, err := newMediaUpload(data, mime, len(images)+1)
			if err != nil {
				writeJSON(w, 400, imageErr(err.Error(), "invalid_request_error"))
				return
			}
			images = append(images, pu)
		}
	}
	// 没有文件部件时，退一步找字符串字段（data URL / http 链接）。
	if len(images) == 0 {
		for _, key := range []string{"image", "image[]", "image_url"} {
			for _, v := range r.MultipartForm.Value[key] {
				extra, err := imagesFromAny(v)
				if err != nil {
					writeJSON(w, 400, imageErr(err.Error(), "invalid_request_error"))
					return
				}
				images = append(images, extra...)
			}
		}
	}
	prompt := strings.TrimSpace(r.FormValue("prompt"))
	// mask 单独取：它只影响 prompt 说明，不当独立编辑对象。
	maskNote := ""
	for _, key := range []string{"mask", "mask[]"} {
		n := len(r.MultipartForm.File[key])
		if n == 0 {
			n = len(r.MultipartForm.Value[key])
		}
		if n > 0 {
			maskNote = fmt.Sprintf(
				"\n\n(Image %d is a mask: apply the edit only inside its transparent/white region and keep everything else unchanged.)",
				len(images)+1)
			break
		}
	}
	if maskNote != "" {
		prompt += maskNote
	}
	runImageRequest(w, r,
		prompt,
		images,
		r.FormValue("model"),
		r.FormValue("size"),
		r.FormValue("response_format"),
		atoiDefault(r.FormValue("n"), 1),
	)
}

// runImageRequest 是两条路的公共尾巴：解析模型 → 校验是生图模型 → callGemini →
// 把 Artifacts 转成 OpenAI images 形状返回。
func runImageRequest(w http.ResponseWriter, r *http.Request, prompt string,
	images []pendingUpload, modelInput, size, responseFormat string, n int) {

	endpoint := imageEndpoint(r.URL.Path)
	if strings.TrimSpace(modelInput) == "" {
		modelInput = "gemini-image"
	}
	modelName, modelCfg, err := resolveModel(modelInput)
	if err != nil {
		writeJSON(w, 400, imageErr(err.Error(), "invalid_request_error"))
		return
	}
	// 只接生图模型。拿 gemini-3.6-flash 打这个端点会返回一段文字、data 是空的，
	// 客户端只会看到「成功但没图」—— 明确报错比这种静默失败好。
	if modelCfg.Tool != toolImage {
		writeJSON(w, 400, imageErr(fmt.Sprintf(
			"model %q is not an image model: the images API needs gemini-image (chat models go to /v1/chat/completions)",
			modelName), "invalid_request_error"))
		return
	}
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		writeJSON(w, 400, imageErr("empty prompt", "invalid_request_error"))
		return
	}
	// size 上游没有对应参数，转成一句画幅提示塞进 prompt（纯文本，不影响别的行为）。
	if hint := aspectHint(size); hint != "" {
		prompt += "\n\n" + hint
	}

	want := n
	if want < 1 {
		want = 1
	}
	if want > maxImageFanout {
		want = maxImageFanout
	}

	var arts []MediaArtifact
	var lastRes *StreamResult
	var lastErr error
	// want 张图要打 want 次上游（它一次出一张），但**每次都可能空手而归**。
	//
	// 实测（各 5 次、串行 + 5s 间隔）：纯文生图 5/5 一次成功，从不回声；
	// 带附件的图生图约 5 次里 1 次回声 —— 上游把 inner[49]=14 丢掉、退回纯文本
	// 模型（响应自带模型报 "3.6 Flash"），它不生图、只把用户附件原样引用回来。
	// 那种结果必须丢掉重打，否则客户端会拿到一张跟原图一模一样的「修改后」图片，
	// 比直接报错还难发现。
	//
	// 回声是**连片**出现的：实测见过连续 6 次都是回声、第 7 次才成功（同一分钟内
	// 上游那个后端实例没换）。所以带附件时预算给足 —— 单次 ~20s，9 次约 3 分钟。
	// 纯文生图从不回声，少给。
	maxTries := want + 2
	if len(images) > 0 {
		maxTries = want + 8
	}
	if maxTries > 10 {
		maxTries = 10
	}
	echoes := 0
	for attempt := 0; attempt < maxTries && len(arts) < want; attempt++ {
		_, _, res, err := callGemini(prompt, prompt, modelCfg, nil, images, nil, nil)
		if err != nil {
			lastErr, lastRes = err, res
			// 已经拿到图就别把整个请求判失败 —— 客户端要的是图，少几张比全丢好。
			if len(arts) > 0 {
				break
			}
			continue // 上游是抖的（没产物链接 / 空响应），换一次往往就好
		}
		lastRes, lastErr = res, nil
		if res == nil {
			continue
		}
		if isEchoArtifacts(res.Artifacts, images) {
			echoes++
			logf("[images] 第 %d 次拿到的是输入图回声（上游模型 %q），丢弃重试", attempt+1, res.UpstreamModel)
			// 回声连片 = 这几秒里一直路由到同一个不认 inner[49] 的后端实例。
			// 停一下再打，比立刻连打更容易落到另一个实例上。
			time.Sleep(3 * time.Second)
			continue
		}
		arts = append(arts, res.Artifacts...)
	}

	if len(arts) == 0 {
		if lastErr != nil {
			recordRequest(endpoint, modelName, prompt, "", lastRes, imageStatus(lastErr), lastErr.Error(), false)
			writeImageFailure(w, lastErr)
			return
		}
		if echoes > 0 {
			msg := "upstream only echoed the input image back instead of editing it (the image tool was not engaged); try again"
			recordRequest(endpoint, modelName, prompt, "", lastRes, 502, msg, false)
			writeJSON(w, 502, imageErr(msg, "upstream_error"))
			return
		}
		// callGemini 在媒体产物取不回时已经报错了，走到这里说明上游 200 但没产物。
		recordRequest(endpoint, modelName, prompt, "", lastRes, 502, "no image produced", false)
		writeJSON(w, 502, imageErr("upstream returned no image", "upstream_error"))
		return
	}

	items := make([]map[string]interface{}, 0, len(arts))
	for _, a := range arts {
		b64 := base64.StdEncoding.EncodeToString(a.Data)
		item := map[string]interface{}{"b64_json": b64}
		// 上游没有公网可取的图片 URL（产物挂在账号会话上，要 cookie 才能下），
		// 所以 response_format=url 时给一条 data URL —— 浏览器和多数 UI 都能直接渲染。
		if responseFormat == "url" {
			item["url"] = "data:" + a.Mime + ";base64," + b64
		}
		items = append(items, item)
	}
	recordRequest(endpoint, modelName, prompt, "", lastRes, 200, "", false)
	writeJSON(w, 200, map[string]interface{}{
		"created": time.Now().Unix(),
		"data":    items,
	})
}

// isEchoArtifacts 判断这一批取回的「产物」是不是把输入图原样送回来了。
//
// 触发场景：带附件的图生图，上游偶尔丢掉 inner[49]=14（退回纯文本模型，响应自带
// 模型标 "3.6 Flash"），它既不生成图，又把用户附件在回复里引用了一遍 —— 取回来的
// 产物跟输入图一模一样。实测图生图约 5 次里 1 次会撞上。这不是编辑，必须丢弃重打，
// 否则客户端会拿到一张跟原图一模一样的「修改后」图片，而且毫无提示。
//
// 判定不能只比字节：上游会把图重新编码（PNG 进、PNG 出，字节不同），也不能只比
// 精确像素 —— 实测 1408x768 的图，回声产物最大通道差 8、差>16 的像素为 0，
// 全是被重新压缩出来的噪声。所以按「近似像素」比：
//
//	尺寸一致，且几乎所有像素的通道差都 <= nearEchoDelta，才算回声。
//
// 真编辑（换颜色、加元素、改风格）总有一片区域差得很大，不会误判。只在小图上
// 做单像素改动时，那 1 个像素远超过阈值，同样不会误判。
//
// 尺寸不一致直接判「不是回声」：上游要真改图，画幅可能变（比如重排版），
// 宁可放过也不要错杀。
const (
	// nearEchoDelta 是「算同一个像素」允许的最大通道差。
	//
	// 实测（1408x768 的图，PNG 进 / JPEG 出）：回声产物与原图的最大通道差是 8，
	// 差 > 8 的像素只有 42 个，差 > 16 的一个都没有 —— 噪声几乎全部来自重编码。
	// 阈值取 24（噪声上限的 3 倍）留足余量：真的「把红色改成蓝色」这类编辑，
	// 通道差动辄 100+，绝不会落进来。
	nearEchoDelta = 24
	// nearEchoRatio 是「算同一张图」的近似像素占比阈值。回声是整图重编码，超过
	// 阈值的像素数实测为 0；真编辑改动再小也是一片区域。取 0.999（允许 0.1% 的
	// 像素超阈），1408x768 上有 1081 个像素的余量。
	//
	// 为什么不能更严：把 nearEchoDelta 压到 16 以下，低对比度的真实编辑（比如
	// 「整体调亮一点」）会被误判成回声，白白重打十次再报错。为什么不能更松：
	// 漏判回声会让用户拿到一张跟自己上传的一模一样的「修改后」图片，且毫无提示。
	nearEchoRatio = 0.999
)

func isEchoArtifacts(arts []MediaArtifact, inputs []pendingUpload) bool {
	if len(arts) == 0 || len(inputs) == 0 {
		return false
	}
	var inPix [][]byte
	var inDims [][2]int
	for _, p := range inputs {
		px, w, h, ok := decodePixels(p.Data)
		if !ok {
			continue
		}
		inPix = append(inPix, px)
		inDims = append(inDims, [2]int{w, h})
	}
	if len(inPix) == 0 {
		return false
	}
	for _, a := range arts {
		px, w, h, ok := decodePixels(a.Data)
		if !ok {
			return false // 解不开就不是这次编辑的产物，不能当成回声丢掉
		}
		hit := false
		for i, ip := range inPix {
			if inDims[i][0] != w || inDims[i][1] != h {
				continue
			}
			if pixelsNearEqual(px, ip) {
				hit = true
				break
			}
		}
		if !hit {
			return false // 只要有一张是新的，就不算整批回声
		}
	}
	return true
}

// pixelsNearEqual 判断两张同尺寸的 RGBA 像素是否「几乎是同一张图」。
func pixelsNearEqual(a, b []byte) bool {
	if len(a) != len(b) || len(a) == 0 || len(a)%4 != 0 {
		return false
	}
	total := len(a) / 4
	// 允许不等的像素数 = 1 - ratio；至少给 1 个像素的余量，避免小图被取整判死。
	allow := int(float64(total) * (1 - nearEchoRatio))
	if allow < 1 {
		allow = 1
	}
	diff := 0
	for i := 0; i+3 < len(a); i += 4 {
		dr := int(a[i]) - int(b[i])
		dg := int(a[i+1]) - int(b[i+1])
		db := int(a[i+2]) - int(b[i+2])
		if dr < 0 {
			dr = -dr
		}
		if dg < 0 {
			dg = -dg
		}
		if db < 0 {
			db = -db
		}
		if dr > nearEchoDelta || dg > nearEchoDelta || db > nearEchoDelta {
			diff++
			if diff > allow {
				return false
			}
		}
	}
	return true
}

// decodePixels 把一张图解码成 RGBA 字节流，返回宽高，供像素级比对用。
// ok=false 表示格式不认识。
func decodePixels(data []byte) ([]byte, int, int, bool) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, 0, 0, false
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return nil, 0, 0, false
	}
	// 统一成 RGBA/Pix 布局：同一张图无论 PNG 还是 JPEG 编出来，比出来都一样。
	if rgba, ok := img.(*image.RGBA); ok && rgba.Rect == b {
		return rgba.Pix, w, h, true
	}
	out := make([]byte, 0, w*h*4)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, a := img.At(x, y).RGBA()
			out = append(out, byte(r>>8), byte(g>>8), byte(bl>>8), byte(a>>8))
		}
	}
	return out, w, h, true
}

func imageStatus(err error) int {
	if _, ok := err.(*PromptTooLongError); ok {
		return 400
	}
	if _, ok := err.(*RateLimitError); ok {
		return 429
	}
	return 502
}

// writeImageFailure 按错误类型写响应体。
func writeImageFailure(w http.ResponseWriter, err error) {
	status := imageStatus(err)
	typ := "upstream_error"
	code := ""
	if ptl, ok := err.(*PromptTooLongError); ok {
		typ = "invalid_request_error"
		code = "context_length_exceeded"
		_ = ptl
	} else if _, ok := err.(*RateLimitError); ok {
		typ = "rate_limit_exceeded"
		code = "ip_slot_full"
	}
	body := map[string]interface{}{"message": err.Error(), "type": typ}
	if code != "" {
		body["code"] = code
	}
	writeJSON(w, status, map[string]interface{}{"error": body})
}

func imageErr(msg, typ string) map[string]interface{} {
	return map[string]interface{}{"error": map[string]string{"message": msg, "type": typ}}
}

// imagesFromAny 把请求里的图片字段（字符串 / 字符串数组 / {url:...}）解成待上传附件。
func imagesFromAny(v interface{}) ([]pendingUpload, error) {
	var out []pendingUpload
	add := func(src string) error {
		src = strings.TrimSpace(src)
		if src == "" {
			return nil
		}
		img, err := materializeImage(src, "", len(out)+1)
		if err != nil {
			return err
		}
		out = append(out, img)
		return nil
	}
	switch t := v.(type) {
	case string:
		if err := add(t); err != nil {
			return nil, err
		}
	case []interface{}:
		for _, e := range t {
			switch et := e.(type) {
			case string:
				if err := add(et); err != nil {
					return nil, err
				}
			case map[string]interface{}:
				if err := add(firstNonEmpty(getStr(et, "image_url"), getStr(et, "url"), getStr(et, "data"))); err != nil {
					return nil, err
				}
			}
		}
	case map[string]interface{}:
		if err := add(firstNonEmpty(getStr(t, "image_url"), getStr(t, "url"), getStr(t, "data"))); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// readImagePart 读一个 multipart 文件部件，顺带把 mime 认准。
func readImagePart(fh *multipart.FileHeader) ([]byte, string, error) {
	f, err := fh.Open()
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxImageUploadBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) == 0 {
		return nil, "", fmt.Errorf("empty file")
	}
	if len(data) > maxImageUploadBytes {
		return nil, "", fmt.Errorf("file is larger than %d bytes", maxImageUploadBytes)
	}
	return data, sniffImageMime(data, fh.Header.Get("Content-Type")), nil
}

// sniffImageMime 认图片类型。客户端经常把 PNG 标成 application/octet-stream，
// 那种 mime 会让上游按错的类型解析附件，所以按魔数再认一遍。
func sniffImageMime(data []byte, declared string) string {
	clean := func(s string) string {
		s = strings.ToLower(strings.TrimSpace(s))
		if i := strings.Index(s, ";"); i > 0 {
			s = s[:i]
		}
		return s
	}
	declared = clean(declared)
	if strings.HasPrefix(declared, "image/") {
		return declared
	}
	if sniffed := clean(http.DetectContentType(data)); strings.HasPrefix(sniffed, "image/") {
		return sniffed
	}
	if declared != "" && declared != "application/octet-stream" {
		return declared
	}
	return "image/png"
}

// aspectHint 把 OpenAI 的 size 参数翻成一句画幅提示。上游只吃文本，
// 所以这是「尽量让它照着来」而不是硬约束。认不出来就不加，别给 prompt 添噪音。
func aspectHint(size string) string {
	switch strings.ToLower(strings.TrimSpace(size)) {
	case "1024x1024", "512x512", "256x256", "1:1", "square":
		return "Output aspect ratio: 1:1 (square)."
	case "1792x1024", "1536x1024", "1344x768", "16:9", "3:2", "landscape":
		return "Output aspect ratio: 16:9 (landscape)."
	case "1024x1792", "1024x1536", "768x1344", "9:16", "2:3", "portrait":
		return "Output aspect ratio: 9:16 (portrait)."
	}
	return ""
}

// intOf 从 JSON 解出来的值里取整数。JSON 数字都是 float64，multipart 表单是字符串。
func intOf(v interface{}, def int) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case int64:
		return int(t)
	case string:
		return atoiDefault(t, def)
	}
	return def
}
