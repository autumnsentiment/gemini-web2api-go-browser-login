package app

import "testing"

// TestVideoAsyncAcceptedResponse 回归 2026-10-01：Veo 纯异步后 StreamGenerate 只回
// 会话 id + 标题，没有正文帧。必须能取到 cid（交给产物轮询），且确实判为无内容帧。
func TestVideoAsyncAcceptedResponse(t *testing.T) {
	raw := ")]}'\n\n177\n" +
		`[["wrb.fr",null,"[null,[\"c_3d6a8ca18abd4eb4\",\"r_1062cdfd1a3b6dce\"],{\"18\":\"r_1062cdfd1a3b6dce\",\"21\":[\"ICpdGefopqKOl3hbcTLynFx24KaPvnXzVe0NHYgFo8w\"],\"44\":true}]"]]` +
		"\n124\n" +
		`[["wrb.fr",null,"[null,[\"c_3d6a8ca18abd4eb4\",\"r_1062cdfd1a3b6dce\"],{\"11\":[\"Red Kite Over Meadow\"],\"44\":true}]"]]` + "\n"
	if got := extractConversationID(raw); got != "c_3d6a8ca18abd4eb4" {
		t.Fatalf("cid = %q", got)
	}
	if hasContentFrame(raw) {
		t.Fatal("expected no content frame for async video acceptance")
	}
}

// TestVideoTextOnlyReply 用线上 hNvQHb 样本的关键片段：纯文字回复要快速失败，
// 生成中 / 已完成都不能误判。
func TestVideoTextOnlyReply(t *testing.T) {
	textOnly := `[["wrb.fr","hNvQHb","[[[[\\"c_1\\",\\"r_1\\"],null,[[\\"hi\\"]],[[[\\"rc_a\\",[\\"Hi there! I can help you generate some videos.\\"]]]]]]]"]]`
	if !videoTextOnlyReply(textOnly) {
		t.Error("text-only reply not detected")
	}
	generating := `[["wrb.fr","hNvQHb","[[\\"rc_a\\",[\\"I'm generating your video. This could take a few minutes\\"]]]"]]`
	if videoTextOnlyReply(generating) {
		t.Error("generating state misjudged as text-only")
	}
	done := `[["wrb.fr","hNvQHb","[[\\"rc_a\\",[\\"Your video is ready!\\"]],[\\"video_generation\\"]]"]]`
	if videoTextOnlyReply(done) {
		t.Error("completed state misjudged as text-only")
	}
	if videoTextOnlyReply(`[["wrb.fr","hNvQHb",null,null,null,[7],"generic"]]`) {
		t.Error("error/empty body misjudged as text-only")
	}
}
