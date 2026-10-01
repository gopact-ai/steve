package console

import "errors"

func (w *recoveryStopWait) abandon() error {
	return errors.New("abandonment confirmation is unavailable")
}
