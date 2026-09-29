package feishu

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"

	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"

	messagechannel "github.com/gopact-ai/steve/internal/channel"
)

// authPaths prefixes the token requests the SDK makes before an API
// request that needs a token.
const authPaths = "/open-apis/auth/"

// call records which requests of one API call reached the HTTP client.
type call struct {
	token atomic.Bool
	api   atomic.Bool
}

type callKey struct{}

// tracked marks ctx so the requests made with it are recorded in the
// returned call.
func tracked(ctx context.Context) (context.Context, *call) {
	c := &call{}
	return context.WithValue(ctx, callKey{}, c), c
}

// trackingClient records, for a tracked call, whether its token request
// and its API request reached the HTTP client.
type trackingClient struct{ next larkcore.HttpClient }

func (t trackingClient) Do(req *http.Request) (*http.Response, error) {
	if c, ok := req.Context().Value(callKey{}).(*call); ok {
		if strings.HasPrefix(req.URL.Path, authPaths) {
			c.token.Store(true)
		} else {
			c.api.Store(true)
		}
	}
	return t.next.Do(req)
}

// failed classifies a failed call. It is a definite failure when its
// request never reached Feishu: the SDK refused it before sending, the
// token it needed could not be obtained, or no connection could be dialed.
// The cause is then kept as text only, because callers read a transport or
// context error as an uncertain outcome. Any other failure may have come
// after Feishu received the request, so its outcome is unknown.
func (c *call) failed(op string, err error) error {
	var illegal *larkcore.IllegalParamError
	var code larkcore.CodeError
	var codeRef *larkcore.CodeError
	var dial *larkcore.DialFailedError
	// Only the SDK's own checks and a token request return a CodeError; an
	// API request's business code arrives in its response.
	if errors.As(err, &illegal) || errors.As(err, &code) || errors.As(err, &codeRef) || errors.As(err, &dial) ||
		(c.token.Load() && !c.api.Load()) {
		return fmt.Errorf("%s: not sent: %v", op, err)
	}
	return fmt.Errorf("%w: %s: %w", messagechannel.ErrOutcomeUnknown, op, err)
}

// refused reports a business code Feishu answered with. A 5xx says Feishu
// failed while handling the request, which does not prove it had no effect.
func refused(op string, resp *larkcore.ApiResp, code int, msg string) error {
	err := fmt.Errorf("%s: code=%d msg=%s", op, code, msg)
	if resp != nil && resp.StatusCode >= http.StatusInternalServerError {
		return fmt.Errorf("%w: %w", messagechannel.ErrOutcomeUnknown, err)
	}
	return err
}
