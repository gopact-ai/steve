package console

import (
	"context"

	"github.com/gopact-ai/steve/internal/turn"
)

// neverAdmittedDriver confirms that this exact input finished before execution
// admission, using the execution owner's durable evidence. A missing retained
// candidate, runner or OnTurnReady callback is not that evidence. False (or an
// error) leaves the original exchange protected; it never authorizes replay.
type neverAdmittedDriver interface {
	ConfirmNeverAdmitted(context.Context, turn.Request) (bool, error)
}

func confirmNeverAdmitted(ctx context.Context, driver RetainedChatDriver, e Exchange, requester string) (bool, error) {
	proof, ok := driver.(neverAdmittedDriver)
	if !ok {
		return false, nil
	}
	confirmed, err := proof.ConfirmNeverAdmitted(ctx, turn.Request{
		Channel: "console", ConversationID: e.Conversation, MessageID: AnchorMark + e.ID,
		ExchangeID: e.ID, SenderOpenID: requester, ExpectedProject: e.ExpectedProject, ExpectedTask: e.ExpectedTask,
		ResumeAdmission: e.ResumeAdmission, Origin: e.Origin, Locale: e.Locale,
	})
	return confirmed && err == nil, err
}

func neverAdmittedMessage(e Exchange) string {
	return line(e.Locale == "en",
		"已确认这条输入在执行接纳前结束，没有启动执行，也没有重新发送。",
		"Confirmed that this input ended before execution admission. No execution started and the input was not resent.")
}
