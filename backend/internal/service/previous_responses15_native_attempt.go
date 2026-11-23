package service

// validateStoredResponseAttemptEvent15 selects the envelope contract from the
// server-owned capture created by native admission. No request flag, header or
// public event field can opt a text capture into the native contract.
func validateStoredResponseAttemptEvent15(attempt *storedResponseAttempt, event string, raw []byte) error {
	if attempt == nil || attempt.capture == nil {
		return ErrStoredResponseInvalid
	}
	capture := attempt.capture
	if !capture.store || capture.record == nil || capture.record.RecoveryVersion != 2 {
		return validateStoredResponseEventEnvelope(event, raw)
	}
	if capture.service == nil || !capture.service.Enabled() {
		return ErrStoredResponsesDisabled
	}
	if int64(len(raw)) > capture.service.cfg.Limits.MaxResponseBytes {
		return ErrStoredResponseTooLarge
	}
	// The existing terminal envelope validator does not restrict nested output
	// to text. Keep its outer schema; CaptureEvent still validates native public
	// output and requires the authenticated private receipt before completion.
	switch event {
	case "response.completed", "response.incomplete", "response.failed":
		return validateStoredResponseEventEnvelope(event, raw)
	}
	value, err := decodeStoredResponseJSON(raw)
	if err != nil {
		return err
	}
	object, ok := value.(map[string]any)
	if !ok {
		return ErrStoredResponseInvalid
	}
	return nativeStoredProgressEnvelope15(event, object)
}
