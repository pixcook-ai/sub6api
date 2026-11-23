//go:build recovery15native

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
	"github.com/gin-gonic/gin"
)

func nativeTerminalDeliveryFixture15(t *testing.T, expectedWire ...string) (context.Context, []byte, []byte, []byte, time.Time) {
	t.Helper()
	now := time.Unix(1800000000, 0)
	binding := managedfiles.NativeTerminalBinding15{PublicID: "resp_local_00000000000000000000000000000001", UserID: 1, APIKeyID: 2, GroupID: 0, AccountID: 3, PublicModel: "fixture-public", ResolvedModel: "fixture-native", BackendID: "backend", SubjectDigest: strings.Repeat("a", 64), CredentialGeneration: "generation-1", AdmissionID: "responses-" + strings.Repeat("b", 64), AdmissionBindingSHA256: strings.Repeat("c", 64), RequestWireSHA256: strings.Repeat("d", 64), Nonce: strings.Repeat("e", 64)}
	key := strings.Repeat("k", 32)
	terminal := []byte(`{ "object" : "response", "status":"completed", "output":[], "usage":{"input_tokens":7,"output_tokens":2} }`)
	snapshot := []byte(`{ "version" : 1, "private":"native-identity" }`)
	envelope, err := managedfiles.EncodeNativeTerminal15(key, binding, []byte(`{}`), terminal, snapshot, now, time.Minute, 4096)
	if err != nil {
		t.Fatal(err)
	}
	wireHash := "44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a"
	if len(expectedWire) > 0 {
		wireHash = expectedWire[0]
	}
	gate, err := newNativeTerminalGate15(key, binding, wireHash, 4096)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := withNativeTerminalAttempt15(context.Background(), gate)
	if err != nil {
		t.Fatal(err)
	}
	return ctx, envelope, terminal, snapshot, now
}
func nativeCompletedEventFixture15(terminal []byte) []byte {
	return []byte(`{"type":"response.completed","sequence_number":9,"response":` + string(terminal) + `}`)
}
func TestNativeTerminalDelivery15ReleasesProofOnlyAfterExactCompleted(t *testing.T) {
	ctx, envelope, terminal, snapshot, now := nativeTerminalDeliveryFixture15(t)
	if drop, err := consumeNativeTerminalEvent15(ctx, "response.in_progress", []byte(`{"type":"response.in_progress"}`), now); err != nil || drop {
		t.Fatal("ordinary progress changed")
	}
	if drop, err := consumeNativeTerminalEvent15(ctx, managedfiles.NativeTerminalEvent15, envelope, now); err != nil || !drop || nativeTerminalDeliveryFromContext15(ctx) != nil {
		t.Fatal("private proof was exposed before completed")
	}
	if drop, err := consumeNativeTerminalEvent15(ctx, "response.completed", nativeCompletedEventFixture15(terminal), now); err != nil || drop {
		t.Fatalf("exact terminal rejected: %v", err)
	}
	delivery := nativeTerminalDeliveryFromContext15(ctx)
	if delivery == nil || delivery.receipt == nil || delivery.receipt.publicID != "resp_local_00000000000000000000000000000001" || !bytes.Equal(delivery.gatewayTerminal, terminal) || !bytes.Equal(delivery.snapshot, snapshot) {
		t.Fatal("verified delivery lost its original bytes")
	}
	if err := finishNativeTerminalAttempt15(ctx); err != nil {
		t.Fatal(err)
	}
	if drop, err := consumeNativeTerminalEvent15(ctx, managedfiles.NativeTerminalEvent15, envelope, now); err == nil || !drop {
		t.Fatal("receipt replay accepted")
	}
}
func TestNativeTerminalDelivery15RejectsMissingReorderedOrAmbiguousProof(t *testing.T) {
	for _, name := range []string{"duplicate private", "wrong next event", "semantic heartbeat", "terminal mismatch", "whitespace rewrite", "duplicate terminal JSON", "pending EOF"} {
		t.Run(name, func(t *testing.T) {
			ctx, envelope, terminal, _, now := nativeTerminalDeliveryFixture15(t)
			if _, err := consumeNativeTerminalEvent15(ctx, managedfiles.NativeTerminalEvent15, envelope, now); err != nil {
				t.Fatal(err)
			}
			var err error
			switch name {
			case "duplicate private":
				_, err = consumeNativeTerminalEvent15(ctx, managedfiles.NativeTerminalEvent15, envelope, now)
			case "wrong next event":
				_, err = consumeNativeTerminalEvent15(ctx, "response.output_text.delta", []byte(`{"type":"response.output_text.delta","delta":"x"}`), now)
			case "semantic heartbeat":
				_, err = consumeNativeTerminalEvent15(ctx, "keepalive", []byte(`{"type":"keepalive"}`), now)
			case "terminal mismatch":
				_, err = consumeNativeTerminalEvent15(ctx, "response.completed", nativeCompletedEventFixture15([]byte(`{"object":"response","status":"completed","output":[1]}`)), now)
			case "whitespace rewrite":
				var compact bytes.Buffer
				if e := json.Compact(&compact, terminal); e != nil {
					t.Fatal(e)
				}
				_, err = consumeNativeTerminalEvent15(ctx, "response.completed", nativeCompletedEventFixture15(compact.Bytes()), now)
			case "duplicate terminal JSON":
				_, err = consumeNativeTerminalEvent15(ctx, "response.completed", []byte(`{"type":"response.completed","type":"response.failed","response":`+string(terminal)+`}`), now)
			case "pending EOF":
				err = finishNativeTerminalAttempt15(ctx)
			}
			if err == nil || nativeTerminalDeliveryFromContext15(ctx) != nil {
				t.Fatal("unverified/reordered proof accepted")
			}
		})
	}
	ctx, _, terminal, _, now := nativeTerminalDeliveryFixture15(t)
	if _, err := consumeNativeTerminalEvent15(ctx, "response.completed", nativeCompletedEventFixture15(terminal), now); err == nil {
		t.Fatal("success without receipt accepted")
	}
	if drop, err := consumeNativeTerminalEvent15(context.Background(), managedfiles.NativeTerminalEvent15, []byte(`{}`), now); err == nil || !drop {
		t.Fatal("unsolicited private frame could reach public passthrough")
	}
}
func TestNativeTerminalDelivery15KeepsFailureAndIncompleteOutcomesWithoutProof(t *testing.T) {
	for _, event := range []string{"response.failed", "response.incomplete", "response.cancelled", "response.canceled", "error"} {
		ctx, _, _, _, now := nativeTerminalDeliveryFixture15(t)
		if drop, err := consumeNativeTerminalEvent15(ctx, event, []byte(`{}`), now); err != nil || drop {
			t.Fatalf("ordinary failure event was changed: %s %v", event, err)
		}
		if nativeTerminalDeliveryFromContext15(ctx) != nil || finishNativeTerminalAttempt15(ctx) != nil {
			t.Fatal("failure issued native history or could not finish")
		}
	}
}
func TestNativeTerminalDelivery15BodyFilterRemovesPrivateFramesBeforeGenericParsing(t *testing.T) {
	ctx, envelope, terminal, _, now := nativeTerminalDeliveryFixture15(t)
	comment := ": heartbeat\r\n\r\n"
	progress := "event: response.in_progress\ndata: {\"type\":\"response.in_progress\"}\n\n"
	completed := "event: response.completed\ndata: " + string(nativeCompletedEventFixture15(terminal)) + "\n\n"
	input := progress + "event: " + managedfiles.NativeTerminalEvent15 + "\ndata: " + string(envelope) + "\n\n" + comment + completed + "data: [DONE]\n\n"
	reader, err := newNativeTerminalBody15(ctx, io.NopCloser(strings.NewReader(input)), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	public, err := io.ReadAll(reader)
	if err != nil || string(public) != progress+comment+completed {
		t.Fatalf("public stream mismatch: %v", err)
	}
	if bytes.Contains(public, []byte("bps.private.")) || bytes.Contains(public, []byte("snapshot_base64")) || bytes.Contains(public, []byte("native-identity")) {
		t.Fatal("private bytes reached generic parser")
	}
	if nativeTerminalDeliveryFromContext15(ctx) == nil {
		t.Fatal("matched terminal did not retain durable proof")
	}
}
func TestNativeTerminalDelivery15BodyFilterRejectsTruncationReorderAndOversize(t *testing.T) {
	for _, name := range []string{"pending EOF", "wrong next frame", "unknown private", "oversize data line", "duplicate event header"} {
		t.Run(name, func(t *testing.T) {
			ctx, envelope, _, _, now := nativeTerminalDeliveryFixture15(t)
			private := "event: " + managedfiles.NativeTerminalEvent15 + "\ndata: " + string(envelope) + "\n\n"
			input := private
			switch name {
			case "wrong next frame":
				input += "event: keepalive\ndata: {\"type\":\"keepalive\"}\n\n"
			case "unknown private":
				input = "event: bps.private.unknown\ndata: {}\n\n"
			case "oversize data line":
				input = "data: " + strings.Repeat("x", 100000)
			case "duplicate event header":
				input = "event: first\nevent: " + managedfiles.NativeTerminalEvent15 + "\ndata: " + string(envelope) + "\n\n"
			}
			reader, err := newNativeTerminalBody15(ctx, io.NopCloser(strings.NewReader(input)), func() time.Time { return now })
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			public, err := io.ReadAll(reader)
			if err == nil || len(public) != 0 || nativeTerminalDeliveryFromContext15(ctx) != nil {
				t.Fatal("invalid private stream was released")
			}
		})
	}
}
func TestNativeTerminalDelivery15NonstreamUnwrapReleasesOnlyPublicJSON(t *testing.T) {
	ctx, envelope, terminal, _, now := nativeTerminalDeliveryFixture15(t)
	resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{nativeTerminalMediaType15}}, Body: io.NopCloser(bytes.NewReader(envelope))}
	if err := prepareNativeTerminalResponse15(ctx, resp, func() time.Time { return now }); err != nil {
		t.Fatal(err)
	}
	public, err := io.ReadAll(resp.Body)
	if err != nil || !bytes.Equal(public, terminal) || resp.Header.Get("Content-Type") != "application/json" || nativeTerminalDeliveryFromContext15(ctx) == nil {
		t.Fatal("nonstream private envelope was not unwrapped")
	}
	ctx, _, terminal, _, now = nativeTerminalDeliveryFixture15(t)
	missing := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewReader(terminal))}
	if err := prepareNativeTerminalResponse15(ctx, missing, func() time.Time { return now }); err == nil {
		t.Fatal("ordinary completed JSON bypassed required private receipt")
	}
}

func TestNativeTerminalDelivery15RejectsUnexpectedNativeWire(t *testing.T) {
	ctx, envelope, _, _, now := nativeTerminalDeliveryFixture15(t, strings.Repeat("f", 64))
	if drop, err := consumeNativeTerminalEvent15(ctx, managedfiles.NativeTerminalEvent15, envelope, now); err == nil || !drop || nativeTerminalDeliveryFromContext15(ctx) != nil {
		t.Fatal("proof for unexpected native wire accepted")
	}
}

func TestNativeTerminalDelivery15BodyRejectsPrivateEnvelopeWithoutReservedEvent(t *testing.T) {
	for _, prefix := range []string{"", "event: response.in_progress\n"} {
		ctx, envelope, _, _, now := nativeTerminalDeliveryFixture15(t)
		reader, err := newNativeTerminalBody15(ctx, io.NopCloser(strings.NewReader(prefix+"data: "+string(envelope)+"\n\n")), func() time.Time { return now })
		if err != nil {
			t.Fatal(err)
		}
		public, err := io.ReadAll(reader)
		_ = reader.Close()
		if err == nil || len(public) != 0 {
			t.Fatal("private envelope without reserved event entered generic parser")
		}
	}
}
func TestNativeTerminalDelivery15PrepareRejectsUnsolicitedPrivateSSE(t *testing.T) {
	_, envelope, _, _, now := nativeTerminalDeliveryFixture15(t)
	for _, prefix := range []string{"event: " + managedfiles.NativeTerminalEvent15 + "\n", "", "event: response.in_progress\n"} {
		resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(prefix + "data: " + string(envelope) + "\n\n"))}
		if err := prepareNativeTerminalResponse15(context.Background(), resp, func() time.Time { return now }); err != nil {
			continue
		}
		public, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err == nil || len(public) != 0 {
			t.Fatal("unsolicited private SSE entered generic response handler")
		}
	}
}

func TestNativeTerminalDelivery15PrepareRejectsMislabeledUnsolicitedPrivateJSON(t *testing.T) {
	_, envelope, _, _, now := nativeTerminalDeliveryFixture15(t)
	resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewReader(envelope))}
	if err := prepareNativeTerminalResponse15(context.Background(), resp, func() time.Time { return now }); err == nil {
		t.Fatal("mislabeled private JSON entered generic parser")
	}
}
func TestNativeTerminalDelivery15PrepareKeepsOrdinaryUnsolicitedBPSJSON(t *testing.T) {
	public := []byte(`{"object":"response","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":2}}`)
	resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewReader(public))}
	if err := prepareNativeTerminalResponse15(context.Background(), resp, time.Now); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil || !bytes.Equal(got, public) {
		t.Fatal("ordinary BPS JSON changed")
	}
}

func TestNativeTerminalDelivery15NonstreamErrorSettlesAuthenticatedTerminalUsage(t *testing.T) {
	for _, next := range []string{"", "event: keepalive\ndata: {\"type\":\"keepalive\"}\n\n"} {
		ctx, envelope, _, _, now := nativeTerminalDeliveryFixture15(t)
		input := "event: " + managedfiles.NativeTerminalEvent15 + "\ndata: " + string(envelope) + "\n\n" + next
		resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(input))}
		if err := prepareNativeTerminalResponse15(ctx, resp, func() time.Time { return now }); err != nil {
			t.Fatal(err)
		}
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		service := &OpenAIGatewayService{}
		result, err := service.handleNonStreamingResponse(ctx, resp, c, &Account{ID: 3, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, "fixture-public", "fixture-native")
		if err == nil || result == nil || result.usage == nil || result.usage.InputTokens != 7 || result.usage.OutputTokens != 2 || nativeTerminalDeliveryFromContext15(ctx) != nil {
			t.Fatalf("authenticated terminal usage lost on delivery failure: result=%+v error=%v", result, err)
		}
		if strings.Contains(recorder.Body.String(), "snapshot_base64") || strings.Contains(recorder.Body.String(), "completed") {
			t.Fatal("failed delivery released private data or public completion")
		}
	}
}

func TestNativeTerminalDelivery15RejectsPrivateEnvelopeOnAllFailureStatuses(t *testing.T) {
	for _, native := range []bool{false, true} {
		for _, media := range []string{"application/json", nativeTerminalMediaType15, "text/event-stream", "text/plain"} {
			ctx, envelope, _, _, now := nativeTerminalDeliveryFixture15(t)
			if !native {
				ctx = context.Background()
			}
			input := string(envelope)
			if media == "text/event-stream" {
				input = "event: " + managedfiles.NativeTerminalEvent15 + "\ndata: " + input + "\n\n"
			}
			resp := &http.Response{StatusCode: 502, Header: http.Header{"Content-Type": []string{media}}, Body: io.NopCloser(strings.NewReader(input))}
			if err := prepareNativeTerminalResponse15(ctx, resp, func() time.Time { return now }); err != nil {
				continue
			}
			public, err := io.ReadAll(resp.Body)
			if err == nil || len(public) != 0 {
				t.Fatalf("private error payload entered generic failure handling native=%v media=%s", native, media)
			}
		}
	}
}

type nativeTerminalEOFSource15 struct {
	reader    io.Reader
	eof       bool
	failAtEnd bool
}

func (s *nativeTerminalEOFSource15) Read(p []byte) (int, error) {
	n, err := s.reader.Read(p)
	if err == io.EOF {
		if s.failAtEnd {
			return n, io.ErrUnexpectedEOF
		}
		s.eof = true
	}
	return n, err
}
func (s *nativeTerminalEOFSource15) Close() error { return nil }

func TestNativeTerminalDelivery15BodyHoldsCompletedUntilRawEOF(t *testing.T) {
	for _, name := range []string{"plain EOF", "DONE", "comments", "DONE and comments", "unterminated final frame"} {
		t.Run(name, func(t *testing.T) {
			ctx, envelope, terminal, _, now := nativeTerminalDeliveryFixture15(t)
			before := ": before terminal\r\n\r\n"
			completed := "event: response.completed\ndata: " + string(nativeCompletedEventFixture15(terminal)) + "\n\n"
			trailer, comments := "", ""
			switch name {
			case "DONE":
				trailer = "data: [DONE]\n\n"
			case "comments":
				comments = ": after terminal\r\n\r\n"
				trailer = comments
			case "DONE and comments":
				comments = ": after DONE\n\n"
				trailer = "data: [DONE]\n\n" + comments
			case "unterminated final frame":
				completed = strings.TrimSuffix(completed, "\n\n")
			}
			wire := before + "event: " + managedfiles.NativeTerminalEvent15 + "\ndata: " + string(envelope) + "\n\n" + completed + trailer
			source := &nativeTerminalEOFSource15{reader: strings.NewReader(wire)}
			reader, err := newNativeTerminalBody15(ctx, source, func() time.Time { return now })
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			var public []byte
			chunk := make([]byte, 65536)
			for {
				n, readErr := reader.Read(chunk)
				public = append(public, chunk[:n]...)
				if bytes.Contains(public, []byte("response.completed")) && !source.eof {
					t.Fatal("public completion escaped before the raw HTTP EOF")
				}
				if readErr != nil {
					if readErr != io.EOF {
						t.Fatal(readErr)
					}
					break
				}
			}
			if !source.eof || string(public) != before+completed+comments || nativeTerminalDeliveryFromContext15(ctx) == nil {
				t.Fatal("valid EOF/DONE/comments did not release the exact bounded public stream and proof")
			}
		})
	}
}

func TestNativeTerminalDelivery15BodyRejectsTrailingFramesBeforeCompletedRelease(t *testing.T) {
	for _, name := range []string{"semantic", "private", "DONE then semantic", "DONE then private", "unknown private DONE", "reserved private DONE", "semantic DONE", "malformed JSON", "duplicate completed", "bounded trailing comments"} {
		t.Run(name, func(t *testing.T) {
			ctx, envelope, terminal, _, now := nativeTerminalDeliveryFixture15(t)
			progress := "event: response.in_progress\ndata: {\"type\":\"response.in_progress\"}\n\n"
			private := "event: " + managedfiles.NativeTerminalEvent15 + "\ndata: " + string(envelope) + "\n\n"
			completed := "event: response.completed\ndata: " + string(nativeCompletedEventFixture15(terminal)) + "\n\n"
			semantic := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"late\"}\n\n"
			trailer := semantic
			switch name {
			case "private":
				trailer = private
			case "DONE then semantic":
				trailer = "data: [DONE]\n\n" + semantic
			case "DONE then private":
				trailer = "data: [DONE]\n\n" + private
			case "unknown private DONE":
				trailer = "event: bps.private.unknown\ndata: [DONE]\n\n"
			case "reserved private DONE":
				trailer = "event: " + managedfiles.NativeTerminalEvent15 + "\ndata: [DONE]\n\n"
			case "semantic DONE":
				trailer = "event: response.in_progress\ndata: [DONE]\n\n"
			case "malformed JSON":
				trailer = "data: {broken\n\n"
			case "duplicate completed":
				trailer = completed
			case "bounded trailing comments":
				trailer = strings.Repeat(": "+strings.Repeat("x", 8000)+"\n\n", 10)
			}
			reader, err := newNativeTerminalBody15(ctx, io.NopCloser(strings.NewReader(progress+private+completed+trailer)), func() time.Time { return now })
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			public, err := io.ReadAll(reader)
			if err == nil || string(public) != progress || nativeTerminalDeliveryFromContext15(ctx) != nil || finishNativeTerminalAttempt15(ctx) == nil {
				t.Fatal("invalid trailer released completion/reusable proof or rewrote prior progress")
			}
			usage, ok := nativeTerminalUsage15(ctx)
			if !ok || usage.InputTokens != 7 || usage.OutputTokens != 2 {
				t.Fatal("invalid trailer discarded independently authenticated observed usage")
			}
		})
	}
}

func TestNativeTerminalDelivery15BodyReadFailureAfterTerminalWithholdsCompleted(t *testing.T) {
	ctx, envelope, terminal, _, now := nativeTerminalDeliveryFixture15(t)
	wire := "event: " + managedfiles.NativeTerminalEvent15 + "\ndata: " + string(envelope) + "\n\n" + "event: response.completed\ndata: " + string(nativeCompletedEventFixture15(terminal)) + "\n\n" + "data: [DONE]\n\n: still waiting for raw EOF\n\n"
	source := &nativeTerminalEOFSource15{reader: strings.NewReader(wire), failAtEnd: true}
	reader, err := newNativeTerminalBody15(ctx, source, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	public, err := io.ReadAll(reader)
	if err == nil || len(public) != 0 || source.eof || nativeTerminalDeliveryFromContext15(ctx) != nil || finishNativeTerminalAttempt15(ctx) == nil {
		t.Fatal("read failure after a signed terminal/DONE released public completion or proof")
	}
	usage, ok := nativeTerminalUsage15(ctx)
	if !ok || usage.InputTokens != 7 || usage.OutputTokens != 2 {
		t.Fatal("read failure discarded independently authenticated observed usage")
	}
}
