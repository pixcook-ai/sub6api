package service

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"strings"
	"testing"
)

func TestManagedInlinePreflightKeepsMixedPositionsAndRequiresPublicBudget(t *testing.T) {
	body := []byte(`{"input":[{"type":"function_call","arguments":"{\"type\":\"input_file\",\"file_data\":\"bad\"}"},{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,opaque"},{"type":"input_file","file_id":"file-public"},{"type":"input_file","filename":"hello.txt","file_data":"aGVsbG8="}]}]}`)
	plan, err := PreflightManagedInlineFiles(context.Background(), body, DefaultManagedInlineLimits())
	if err != nil {
		t.Fatal(err)
	}
	files := plan.Files()
	if len(files) != 2 || files[0].PublicID != "file-public" || files[0].Position.Input != 1 || files[0].Position.Part != 1 || files[1].Filename != "hello.txt" || files[1].Bytes != 5 || files[1].Position.Input != 1 || files[1].Position.Part != 2 {
		t.Fatalf("unexpected files: %+v", files)
	}
	if plan.ReadyForStaging() {
		t.Fatal("public metadata has not been authorized and budgeted")
	}
	checked, err := plan.WithPublicFiles([]ManagedInlinePublicFile{{PublicID: "file-public", Bytes: 6}})
	if err != nil || !checked.ReadyForStaging() {
		t.Fatalf("public budget: %v", err)
	}
	files[0].PublicID = "mutated"
	if plan.Files()[0].PublicID != "file-public" || plan.ReadyForStaging() {
		t.Fatal("description was mutable")
	}
}

func TestManagedInlinePreflightAcceptsDataURIWithoutFilename(t *testing.T) {
	plan, err := PreflightManagedInlineFiles(context.Background(), []byte(`{"input":[{"role":"user","content":[{"type":"input_file","file_data":"data:text/plain;base64,aGVsbG8="}]}]}`), DefaultManagedInlineLimits())
	if err != nil {
		t.Fatal(err)
	}
	files := plan.Files()
	if !plan.ReadyForStaging() || len(files) != 1 || files[0].Filename != "attachment" || files[0].MIME != "text/plain" || files[0].Bytes != 5 {
		t.Fatalf("unexpected data URI: %+v", files)
	}
}

func TestManagedInlinePreflightRequiresOneSafeCanonicalSource(t *testing.T) {
	cases := map[string]string{
		"bare needs filename":     `"file_data":"aGVsbG8="`,
		"unsafe filename":         `"filename":"../hello.txt","file_data":"aGVsbG8="`,
		"filename type":           `"filename":7,"file_data":"data:text/plain;base64,aGVsbG8="`,
		"dual source":             `"file_id":"file-public","filename":"hello.txt","file_data":"aGVsbG8="`,
		"url with data":           `"file_url":"https://files.example/a.txt","file_data":"aGVsbG8="`,
		"public wrong type":       `"file_id":7`,
		"public syntax":           `"file_id":" ../a"`,
		"empty bytes":             `"filename":"hello.txt","file_data":""`,
		"decoder ignores newline": `"filename":"hello.txt","file_data":"aGVs\nbG8="`,
		"nonzero tail bits":       `"filename":"hello.txt","file_data":"Zh=="`,
		"missing padding":         `"filename":"hello.txt","file_data":"aGVsbG8"`,
		"URI wildcard":            `"file_data":"data:text/*;base64,aGVsbG8="`,
		"oversized URI header":    fmt.Sprintf(`"file_data":%q`, "data:text/plain;x="+strings.Repeat("a", 1024)+";base64,aGVsbG8="),
	}
	for name, fields := range cases {
		t.Run(name, func(t *testing.T) {
			body := []byte(fmt.Sprintf(`{"input":[{"role":"user","content":[{"type":"input_file",%s}]}]}`, fields))
			if _, err := PreflightManagedInlineFiles(context.Background(), body, DefaultManagedInlineLimits()); err == nil {
				t.Fatal("invalid source accepted")
			}
		})
	}
}

func TestManagedInlineDataURICanonicalizesCaseAndMediaParameters(t *testing.T) {
	for _, data := range []string{"DATA:TEXT/PLAIN;BASE64,aGVsbG8=", "data:text/plain;charset=utf-8;base64,aGVsbG8=", "data:;base64,aGVsbG8="} {
		body := fmt.Sprintf(`{"input":[{"role":"user","content":[{"type":"input_file","filename":"hello.txt","file_data":%q}]}]}`, data)
		plan, err := PreflightManagedInlineFiles(context.Background(), []byte(body), DefaultManagedInlineLimits())
		if err != nil {
			t.Fatalf("canonical data URI: %v", err)
		}
		if plan.Files()[0].MIME != "text/plain" || plan.Files()[0].Bytes != 5 {
			t.Fatalf("canonical data URI: %+v", plan.Files())
		}
	}
}

func TestManagedInlinePreflightRejectsFilesOutsideCanonicalMessageContent(t *testing.T) {
	const file = `{"type":"input_file","filename":"hello.txt","file_data":"aGVsbG8="}`
	cases := map[string]string{
		"input object":            `{"input":{"role":"user","content":[` + file + `]}}`,
		"top level part":          `{"input":[` + file + `]}`,
		"function output":         `{"input":[{"type":"function_call_output","output":[` + file + `]}]}`,
		"custom output":           `{"input":[{"type":"custom_tool_call_output","output":[` + file + `]}]}`,
		"content object":          `{"input":[{"role":"user","content":` + file + `}]}`,
		"typed tool result":       `{"input":[{"role":"user","content":[{"type":"tool_result","content":[` + file + `]}]}]}`,
		"unknown typed container": `{"input":[{"role":"user","content":[{"type":"future_result","content":[` + file + `]}]}]}`,
		"numeric message type":    `{"input":[{"type":7,"role":"user","content":[` + file + `]}]}`,
		"null message type":       `{"input":[{"type":null,"role":"user","content":[` + file + `]}]}`,
		"empty message type":      `{"input":[{"type":"","role":"user","content":[` + file + `]}]}`,
		"root alias":              `{"Input":[{"role":"user","content":[` + file + `]}]}`,
		"content alias":           `{"input":[{"role":"user","Content":[` + file + `]}]}`,
		"field alias":             `{"input":[{"role":"user","content":[{"type":"input_file","filename":"hello.txt","File_Data":"aGVsbG8="}]}]}`,
		"type alias":              `{"input":[{"role":"user","content":[{"Type":"input_file","filename":"hello.txt","file_data":"aGVsbG8="}]}]}`,
		"kind alias":              `{"input":[{"role":"user","content":[{"type":"INPUT_FILE","filename":"hello.txt","file_data":"aGVsbG8="}]}]}`,
		"image public id":         `{"input":[{"role":"user","content":[{"type":"input_image","file_id":"file-public"}]}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := PreflightManagedInlineFiles(context.Background(), []byte(body), DefaultManagedInlineLimits()); err == nil {
				t.Fatal("unsupported owned container accepted")
			}
		})
	}
}

func TestManagedInlineBudgetsIncludeEveryPositionAndPublicMetadata(t *testing.T) {
	const body = `{"input":[{"role":"user","content":[{"type":"input_file","file_id":"file-public"},{"type":"input_file","filename":"hello.txt","file_data":"aGVsbG8="}]}]}`
	for name, change := range map[string]func(*ManagedInlineLimits){
		"JSON":              func(l *ManagedInlineLimits) { l.JSONBytes = int64(len(body) - 1) },
		"inline bytes":      func(l *ManagedInlineLimits) { l.FileBytes = 4 },
		"message positions": func(l *ManagedInlineLimits) { l.MessagePositions = 1 },
		"unique files":      func(l *ManagedInlineLimits) { l.UniqueFiles = 1 },
		"hard ceiling":      func(l *ManagedInlineLimits) { l.FileBytes = (200 << 20) + 1 },
		"negative limit":    func(l *ManagedInlineLimits) { l.TotalBytes = -1 },
	} {
		t.Run(name, func(t *testing.T) {
			limits := DefaultManagedInlineLimits()
			change(&limits)
			if _, err := PreflightManagedInlineFiles(context.Background(), []byte(body), limits); err == nil {
				t.Fatal("preflight limit accepted")
			}
		})
	}
	for name, change := range map[string]func(*ManagedInlineLimits){
		"public bytes":     func(l *ManagedInlineLimits) { l.FileBytes = 5 },
		"combined message": func(l *ManagedInlineLimits) { l.MessageBytes = 10 },
		"combined request": func(l *ManagedInlineLimits) { l.TotalBytes = 10 },
	} {
		t.Run(name, func(t *testing.T) {
			limits := DefaultManagedInlineLimits()
			change(&limits)
			plan, err := PreflightManagedInlineFiles(context.Background(), []byte(body), limits)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := plan.WithPublicFiles([]ManagedInlinePublicFile{{PublicID: "file-public", Bytes: 6}}); err == nil {
				t.Fatal("combined limit accepted")
			}
			if plan.ReadyForStaging() {
				t.Fatal("failed public snapshot mutated plan")
			}
		})
	}
	const repeated = `{"input":[{"role":"user","content":[{"type":"input_file","file_id":"file-public"},{"type":"input_file","file_id":"file-public"}]},{"role":"user","content":[{"type":"input_file","file_id":"file-public"}]}]}`
	limits := DefaultManagedInlineLimits()
	limits.UniqueFiles = 1
	limits.MessagePositions = 2
	limits.MessageBytes = 10
	plan, err := PreflightManagedInlineFiles(context.Background(), []byte(repeated), limits)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan.WithPublicFiles([]ManagedInlinePublicFile{{PublicID: "file-public", Bytes: 6}}); err == nil {
		t.Fatal("repeated public positions undercounted")
	}
}

func TestManagedInlinePreflightMatchesOfficialTypeAdmission(t *testing.T) {
	cases := []struct {
		name, filename, data string
		valid                bool
	}{
		{"MIME only", "", "data:application/graphql;base64,aGVsbG8=", true},
		{"extension only", "hello.txt", "data:application/x-example;base64,aGVsbG8=", true},
		{"unsupported", "payload.exe", "data:application/x-msdownload;base64,aGVsbG8=", false},
		{"anonymous octets", "", "data:application/octet-stream;base64,aGVsbG8=", false},
		{"PDF mismatch", "hello.pdf", "aGVsbG8=", false},
		{"executable disguised", "hello.txt", "TVo=", false},
		{"ZIP disguised", "hello.txt", "UEsDBA==", false},
		{"image mismatch", "hello.png", "aGVsbG8=", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			part := fmt.Sprintf(`{"type":"input_file","file_data":%q`, tc.data)
			if tc.filename != "" {
				part += fmt.Sprintf(`,"filename":%q`, tc.filename)
			}
			part += "}"
			_, err := PreflightManagedInlineFiles(context.Background(), []byte(`{"input":[{"role":"user","content":[`+part+`]}]}`), DefaultManagedInlineLimits())
			if (err == nil) != tc.valid {
				t.Fatalf("type admission: valid=%v err=%v", tc.valid, err)
			}
		})
	}
}

func TestManagedInlineImageDimensionsMatchGatewayBoundary(t *testing.T) {
	for _, width := range []uint32{8192, 8193} {
		header := make([]byte, 33)
		copy(header, "\x89PNG\r\n\x1a\n")
		binary.BigEndian.PutUint32(header[8:12], 13)
		copy(header[12:16], "IHDR")
		binary.BigEndian.PutUint32(header[16:20], width)
		binary.BigEndian.PutUint32(header[20:24], 8192)
		header[24] = 8
		header[25] = 6
		binary.BigEndian.PutUint32(header[29:33], crc32.ChecksumIEEE(header[12:29]))
		body := fmt.Sprintf(`{"input":[{"role":"user","content":[{"type":"input_file","filename":"header.png","file_data":%q}]}]}`, base64.StdEncoding.EncodeToString(header))
		_, err := PreflightManagedInlineFiles(context.Background(), []byte(body), DefaultManagedInlineLimits())
		if (err == nil) != (width == 8192) {
			t.Fatalf("width %d pixel boundary: %v", width, err)
		}
	}
}

func TestManagedInlineStrictJSONAndPublicSnapshotFailures(t *testing.T) {
	for name, body := range map[string]string{
		"duplicate decoded key":     `{"input":[],"inpu\u0074":[]}`,
		"lone surrogate":            `{"input":"\ud800"}`,
		"trailing JSON":             `{"input":[]} {}`,
		"valid first invalid later": `{"input":[{"role":"user","content":[{"type":"input_file","file_id":"file-public"},{"type":"input_file","filename":"late.txt","file_data":"!invalid"}]}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if plan, err := PreflightManagedInlineFiles(context.Background(), []byte(body), DefaultManagedInlineLimits()); err == nil || plan != nil {
				t.Fatalf("invalid request returned a preparation: %v", err)
			}
		})
	}
	plan, err := PreflightManagedInlineFiles(context.Background(), []byte(`{"input":[{"role":"user","content":[{"type":"input_file","file_id":"file-public"}]}]}`), ManagedInlineLimits{})
	if err != nil {
		t.Fatal(err)
	}
	for _, metadata := range [][]ManagedInlinePublicFile{nil, {{PublicID: "file-other", Bytes: 5}}, {{PublicID: "file-public", Bytes: 0}}, {{PublicID: "file-public", Bytes: 5}, {PublicID: "file-public", Bytes: 5}}, {{PublicID: "file-public", Bytes: 5}, {PublicID: "file-extra", Bytes: 5}}} {
		if checked, err := plan.WithPublicFiles(metadata); err == nil || checked != nil || plan.ReadyForStaging() {
			t.Fatal("inexact public metadata authorized a plan")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := PreflightManagedInlineFiles(ctx, []byte(`{"input":[]}`), DefaultManagedInlineLimits()); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled preflight: %v", err)
	}
}

func TestManagedInlinePreflightTraversesTypedNestingOnceAndPreservesPart(t *testing.T) {
	part := `{"type":"input_text","text":"opaque"}`
	for i := 0; i < 30; i++ {
		part = `{"type":"tool_result","content":[` + part + `]}`
	}
	body := `{"input":[{"role":"user","content":[` + part + `,{"type":"input_file","filename":"hello.txt","file_data":"aGVsbG8="}]}]}`
	plan, err := PreflightManagedInlineFiles(context.Background(), []byte(body), DefaultManagedInlineLimits())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Files()) != 1 || plan.Files()[0].Position.Part != 1 {
		t.Fatalf("typed nesting changed positions: %+v", plan.Files())
	}
}
