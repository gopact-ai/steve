package chief

import "testing"

func TestParseReplySeparatesTextFromActions(t *testing.T) {
	text, a, err := parseReply("好的，我来安排。\n\n```steve-actions\n{\"delegate\":[{\"goal\":\" fix issue 7 in gopact-ai/steve \",\"mode\":\"auto\"}],\"stop\":[\"#12\"]}\n```\n")
	if err != nil || text != "好的，我来安排。" || len(a.Delegate) != 1 || a.Delegate[0].Goal != "fix issue 7 in gopact-ai/steve" || len(a.Stop) != 1 || a.Stop[0] != "12" {
		t.Fatalf("text=%q actions=%+v err=%v", text, a, err)
	}
	text, a, err = parseReply("现在有 2 个任务在跑。")
	if err != nil || text != "现在有 2 个任务在跑。" || len(a.Delegate)+len(a.Stop) != 0 {
		t.Fatalf("plain reply = %q %+v %v", text, a, err)
	}
}

func TestParseReplyDoesNothingWithAMalformedBlock(t *testing.T) {
	for _, reply := range []string{
		"x\n```steve-actions\n{\"delegate\":[{\"goal\":\"a\"}],}\n```",
		"x\n```steve-actions\n{\"run\":[\"rm -rf /\"]}\n```",
		"x\n```steve-actions\n{\"delegate\":[{\"goal\":\"\"}]}\n```",
		"x\n```steve-actions\n{\"delegate\":[{\"goal\":\"a\",\"mode\":\"root\"}]}\n```",
		"```steve-actions\n{}\n```\n```steve-actions\n{}\n```",
	} {
		text, a, err := parseReply(reply)
		if err == nil || len(a.Delegate)+len(a.Stop) != 0 {
			t.Fatalf("%q acted: %+v %v", reply, a, err)
		}
		if text == "" && reply[0] == 'x' {
			t.Fatalf("%q lost its text", reply)
		}
	}
}
