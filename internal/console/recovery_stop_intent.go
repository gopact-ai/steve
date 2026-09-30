package console

// recordRecoveryStopLocked saves the task, pending stop and owner intent in
// one record. A rejected write cannot turn an automatic check into a cancel.
func (s *Service) recordRecoveryStopLocked(target *queuedExchange, taskID string, cancel bool) error {
	previous, previousTask, previousIntent := target.RecoveryStopPending, target.RecoveryStopTask, target.RecoveryCancelPending
	target.RecoveryStopPending, target.RecoveryStopTask, target.RecoveryCancelPending = stopRequested, taskID, cancel
	if err := s.save(); err != nil {
		target.RecoveryStopPending, target.RecoveryStopTask, target.RecoveryCancelPending = previous, previousTask, previousIntent
		return err
	}
	return nil
}
