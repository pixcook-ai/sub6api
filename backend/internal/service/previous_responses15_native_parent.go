package service

import (
	"bytes"
	"encoding/json"
	"time"
)

func storedNativeParent15(record, parent *StoredResponse, now time.Time) error {
	if record == nil {
		return ErrStoredResponseInvalid
	}
	if record.PreviousResponseID == "" {
		if parent != nil {
			return ErrStoredResponseInvalid
		}
		return nil
	}
	if parent == nil || parent.PublicID != record.PreviousResponseID || parent.UserID != record.UserID || parent.APIKeyID != record.APIKeyID || parent.GroupID != record.GroupID || parent.AccountID != record.AccountID || parent.Model != record.Model || parent.ResolvedModel != record.ResolvedModel || parent.RecoveryVersion != 2 || parent.State != "completed" || parent.ExecutionState != "terminal" || parent.DeletedAt != nil || !parent.ExpiresAt.After(now) {
		return ErrStoredResponseNotFound
	}
	return nil
}

// ValidateStoredResponseNativeReservationParent15 runs while repository global
// and owner locks are held. It compares the admitted raw prefix to the actual
// PreviousResponseID row, rather than any same-owner/same-account snapshot.
func ValidateStoredResponseNativeReservationParent15(record, parent *StoredResponse, now time.Time, maxBytes int64) error {
	if err := storedNativeParent15(record, parent, now); err != nil {
		return err
	}
	if record.nativeAdmission15 == nil {
		return ErrStoredResponseInvalid
	}
	admission := record.nativeAdmission15
	if parent == nil {
		if len(admission.parentSnapshot) != 0 {
			return ErrStoredResponseInvalid
		}
		return nil
	}
	material, err := readStoredNative15(parent, parent.PublicResponse, parent.Recovery, maxBytes)
	if err != nil {
		return err
	}
	if material.BackendID != admission.backendID || material.SubjectDigest != admission.subjectDigest || material.CredentialGeneration != admission.credentialGeneration || !bytes.Equal(material.Snapshot, admission.parentSnapshot) {
		return ErrStoredResponseInvalid
	}
	return nil
}

// ValidateStoredResponseNativeRecoveryParent15 runs inside the same transaction
// as the public/private terminal UPDATE. Re-fetching the parent is essential:
// the transient admission capability is intentionally absent after row reload.
func ValidateStoredResponseNativeRecoveryParent15(record *StoredResponse, public, recovery []byte, parent *StoredResponse, now time.Time, maxBytes int64) error {
	if err := storedNativeParent15(record, parent, now); err != nil {
		return err
	}
	current, err := readStoredNative15(record, public, recovery, maxBytes)
	if err != nil {
		return err
	}
	currentTurns, err := storedNativeTurns15(current.Snapshot, record, current.SubjectDigest, maxBytes)
	if err != nil {
		return err
	}
	if parent == nil {
		if len(currentTurns) != 1 {
			return ErrStoredResponseInvalid
		}
		return nil
	}
	previous, err := readStoredNative15(parent, parent.PublicResponse, parent.Recovery, maxBytes)
	if err != nil {
		return err
	}
	if current.BackendID != previous.BackendID || current.SubjectDigest != previous.SubjectDigest || current.CredentialGeneration != previous.CredentialGeneration {
		return ErrStoredResponseInvalid
	}
	previousTurns, err := storedNativeTurns15(previous.Snapshot, parent, previous.SubjectDigest, maxBytes)
	if err != nil {
		return err
	}
	if len(currentTurns) != len(previousTurns)+1 {
		return ErrStoredResponseInvalid
	}
	prefix, _ := json.Marshal(currentTurns[:len(previousTurns)])
	expected, _ := json.Marshal(previousTurns)
	if !bytes.Equal(prefix, expected) {
		return ErrStoredResponseInvalid
	}
	return nil
}
