package managedfiles

import (
	"strings"
	"testing"
)

func TestNativeResponsesPolicyError15FixedPublicEnvelope(t *testing.T) {
	const valid = `{"error":{"type":"invalid_request_error","code":"unsupported_parameter","param":"tools[0].strict","message":"The requested capability is not supported by this gateway."}}`
	if p := DecodeNativeResponsesPolicyError15(400, []byte(valid)); p == nil || p.Param != "tools[0].strict" {
		t.Fatal("fixed public policy rejected")
	}
	for _, raw := range []string{strings.Replace(valid, "gateway.", "gateway. private-token", 1), strings.Replace(valid, `"param":`, `"private":"secret","param":`, 1), strings.Replace(valid, `"param":`, `"Param":`, 1), strings.Replace(valid, `"tools[0].strict"`, `"https://private.invalid"`, 1)} {
		if DecodeNativeResponsesPolicyError15(400, []byte(raw)) != nil {
			t.Fatal("private or ambiguous policy admitted")
		}
	}
	if DecodeNativeResponsesPolicyError15(503, []byte(valid)) != nil {
		t.Fatal("non-policy status admitted")
	}
}
