package service

import (
	"encoding/hex"
	"strings"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
)

// Constructible only after the HMAC transport verifier succeeds. Persistence
// must recheck exact payload digests and its fresh record authorization too.
type verifiedNativeTerminal15 struct {
	publicID                                                            string
	userID, apiKeyID, groupID, accountID                                int64
	publicModel, resolvedModel, subjectDigest                           string
	backendID, credentialGeneration                                     string
	requestWireSHA256, nativeWireSHA256, terminalSHA256, snapshotSHA256 string
	nonce, admissionID, admissionBindingSHA256                          string
}

func verifyNativeTerminal15(raw []byte, key string, expected managedfiles.NativeTerminalBinding15, expectedNativeWireSHA256 string, now time.Time, maxBytes int) (*verifiedNativeTerminal15, []byte, []byte, error) {
	receipt, terminal, snapshot, err := managedfiles.VerifyNativeTerminal15(raw, key, expected, now, maxBytes)
	if err != nil || !nativeTerminalDigest15(expectedNativeWireSHA256) || receipt.NativeWireSHA256 != expectedNativeWireSHA256 {
		return nil, nil, nil, ErrStoredResponseInvalid
	}
	b := receipt.Binding
	verified := &verifiedNativeTerminal15{publicID: b.PublicID, userID: b.UserID, apiKeyID: b.APIKeyID, groupID: b.GroupID, accountID: b.AccountID, publicModel: b.PublicModel, resolvedModel: b.ResolvedModel, subjectDigest: b.SubjectDigest, backendID: b.BackendID, credentialGeneration: b.CredentialGeneration, requestWireSHA256: b.RequestWireSHA256, nativeWireSHA256: receipt.NativeWireSHA256, terminalSHA256: receipt.TerminalSHA256, snapshotSHA256: receipt.SnapshotSHA256, nonce: b.Nonce, admissionID: b.AdmissionID, admissionBindingSHA256: b.AdmissionBindingSHA256}
	return verified, terminal, snapshot, nil
}

func nativeTerminalDigest15(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
