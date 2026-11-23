//go:build recovery15native

package service

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
)

func TestStoredResponseNative15VerifiesCompleteNativeAckBeforeAnyBody(t *testing.T) {
	key := strings.Repeat("k", 32)
	id := "resp_local_00000000000000000000000000000001"
	result := managedfiles.NativeResponsesPreflightResult15{Version: managedfiles.NativeResponsesContract15, PolicyVersion: managedfiles.NativeResponsesPolicy15, MaxBytes: 4096, Identity: managedfiles.Identity{BackendID: "backend", SubjectFingerprint: "sha256:" + strings.Repeat("a", 64), CredentialGeneration: "generation"}, AdmissionID: "responses-" + strings.Repeat("a", 64), ExpiresAt: time.Now().Add(20 * time.Second).Unix(), RequestWireSHA256: strings.Repeat("b", 64), NativeWireSHA256: strings.Repeat("c", 64), BindingSHA256: strings.Repeat("d", 64)}
	ack, err := managedfiles.NativeResponsesAck15(key, result, id)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), storedResponseNativeAckKey15{}, storedResponseNativeAck15{key: key, result: result, publicID: id})
	header := make(http.Header)
	header.Set(managedfiles.NativeResponsesAdmittedHeader15, ack)
	if !verifyStoredResponseAck(ctx, header) {
		t.Fatal("exact native ACK rejected")
	}
	header.Add(managedfiles.NativeResponsesAdmittedHeader15, ack)
	if verifyStoredResponseAck(ctx, header) {
		t.Fatal("duplicate native ACK accepted")
	}
	header.Set(managedfiles.NativeResponsesAdmittedHeader15, ack)
	changed := result
	changed.MaxBytes++
	ctx = context.WithValue(context.Background(), storedResponseNativeAckKey15{}, storedResponseNativeAck15{key: key, result: changed, publicID: id})
	if verifyStoredResponseAck(ctx, header) {
		t.Fatal("ACK for different negotiated byte limit accepted")
	}
	ctx = context.WithValue(context.Background(), storedResponseNativeAckKey15{}, storedResponseNativeAck15{key: key, result: result, publicID: "resp_local_00000000000000000000000000000002"})
	if verifyStoredResponseAck(ctx, header) {
		t.Fatal("ACK for different local response ID accepted")
	}
	header.Del(managedfiles.NativeResponsesAdmittedHeader15)
	header.Set(managedfiles.ResponsesAdmittedHeader, ack)
	if verifyStoredResponseAck(ctx, header) {
		t.Fatal("old-v1 header substituted for native ACK")
	}
}

func TestStoredResponseNative15StripsClientPrivateHeadersBeforeActivation(t *testing.T) {
	header := make(http.Header)
	for _, key := range []string{managedfiles.ServiceKeyHeader, managedfiles.OwnerRefHeader, managedfiles.GroupRefHeader, managedfiles.ResponsesAdmissionHeader, managedfiles.NativeResponsesActivationHeader15, "X-Bps-Forged-Private"} {
		header.Set(key, "forged")
	}
	header.Set("Authorization", "Bearer public-route-token")
	stripNativeResponsePrivateHeaders15(header)
	for key := range header {
		if strings.HasPrefix(strings.ToLower(key), "x-bps-") {
			t.Fatal("client private header survived native activation")
		}
	}
	if header.Get("Authorization") != "Bearer public-route-token" {
		t.Fatal("ordinary route authorization removed")
	}
}
