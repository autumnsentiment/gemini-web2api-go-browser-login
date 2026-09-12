package app

import (
	"fmt"
	"os"
	"sync"

	"github.com/pkoukk/tiktoken-go"
)

// tiktoken cl100k_base — GPT-4 BPE。Gemini 真 tokenizer Google 没公开，
// 但 BPE 层面规则相近，对中英混合都比 chars/4 准确得多（实测中文偏差 ±20% 内）。
//
// 加载一次到内存（~5MB），后续 Encode 每次 ~微秒级。
var (
	tokenizer     *tiktoken.Tiktoken
	tokenizerOnce sync.Once
	tokenizerOK   bool
)

func initTokenizer() {
	tokenizerOnce.Do(func() {
		// tiktoken-go 第一次加载会从远端下 cl100k_base.tiktoken (~3MB)，
		// 缓存到 ${TIKTOKEN_CACHE_DIR} 或 ${HOME}/.cache/tiktoken。
		// docker 镜像里建议预先 docker build 时缓存。
		t, err := tiktoken.GetEncoding("cl100k_base")
		if err != nil {
			fmt.Fprintf(os.Stderr, "[tokenizer] init failed (will fall back to chars/4): %v\n", err)
			return
		}
		tokenizer = t
		tokenizerOK = true
	})
}

// countTokens 返回文本的近似 token 数。
// tokenizer 加载失败时退回到 chars/4 估算。
func countTokens(s string) int {
	if !tokenizerOK || tokenizer == nil {
		return len(s) / 4
	}
	return len(tokenizer.Encode(s, nil, nil))
}
