package app

import "testing"

func TestTaskTransportNeverInfersDestination(t *testing.T) {
	for _, transport := range []string{"console", "feishu", "", "unknown", "console:message"} {
		t.Run(transport, func(t *testing.T) {
			page, chat := 0, 0
			err := routeTask(transport, func() error { page++; return nil }, func() error { chat++; return nil })
			switch transport {
			case "console":
				if err != nil || page != 1 || chat != 0 {
					t.Fatalf("page=%d chat=%d error=%v", page, chat, err)
				}
			case "feishu":
				if err != nil || page != 0 || chat != 1 {
					t.Fatalf("page=%d chat=%d error=%v", page, chat, err)
				}
			default:
				if err == nil || page != 0 || chat != 0 {
					t.Fatalf("unknown transport dispatched: page=%d chat=%d error=%v", page, chat, err)
				}
			}
		})
	}
}
