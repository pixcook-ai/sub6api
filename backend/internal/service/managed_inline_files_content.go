package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"path"
	"strings"

	_ "golang.org/x/image/webp"
)

const managedInlineImageMaxBytes int64 = 20 << 20

// Keep admission in parity with the gateway's pinned official MIME-or-extension
// registry. This check completes for every inline entry before Stage is exposed.
func managedInlineMediaType(filename, contentType string) (string, error) {
	extension := strings.ToLower(path.Ext(filename))
	if contentType == "" {
		contentType = mime.TypeByExtension(extension)
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil || strings.Count(mediaType, "/") != 1 || strings.Contains(mediaType, "*") {
		return "", inlineReject("invalid_inline_mime", "input")
	}
	mediaType = strings.ToLower(mediaType)
	if !managedInlineFileExtensions[extension] && !managedInlineImageExtensions[extension] && !managedInlineFileMIMEs[mediaType] && !managedInlineImageMIMEs[mediaType] {
		return "", inlineReject("unsupported_inline_type", "input")
	}
	return mediaType, nil
}

func managedInlineImageType(d ManagedInlineFileDescriptor) string {
	if kind := map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp"}[strings.ToLower(path.Ext(d.Filename))]; kind != "" {
		return kind
	}
	if managedInlineImageMIMEs[d.MIME] {
		return d.MIME
	}
	return ""
}

func validateManagedInlineContent(ctx context.Context, d ManagedInlineFileDescriptor, encoded string) error {
	open := func() io.Reader {
		return base64.NewDecoder(base64.StdEncoding.Strict(), managedInlineContextReader{ctx: ctx, reader: strings.NewReader(encoded)})
	}
	return validateManagedFileContent(ctx, d, open)
}

// Both inline data and downloaded bytes pass the same content policy. The
// opener supplies a fresh reader at offset zero for each bounded inspection.
func validateManagedFileContent(ctx context.Context, d ManagedInlineFileDescriptor, open func() io.Reader) error {
	var prefix [16]byte
	n, _ := io.ReadFull(open(), prefix[:])
	if err := ctx.Err(); err != nil {
		return err
	}
	first := prefix[:n]
	extension := strings.ToLower(path.Ext(d.Filename))
	if bytes.HasPrefix(first, []byte("MZ")) {
		return inlineReject("invalid_inline_content", "input")
	}
	if (extension == ".pdf" || d.MIME == "application/pdf") && !bytes.HasPrefix(first, []byte("%PDF")) {
		return inlineReject("invalid_inline_content", "input")
	}
	isZIP := bytes.HasPrefix(first, []byte("PK\x03\x04")) || bytes.HasPrefix(first, []byte("PK\x05\x06")) || bytes.HasPrefix(first, []byte("PK\x07\x08"))
	if managedInlineFileExtensions[extension] && extension != ".pdf" && !managedInlineZIPContainers[extension] && isZIP {
		return inlineReject("invalid_inline_content", "input")
	}
	if imageType := managedInlineImageType(d); imageType != "" {
		config, format, err := image.DecodeConfig(open())
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil || "image/"+format != imageType || (managedInlineImageMIMEs[d.MIME] && d.MIME != imageType) || config.Width <= 0 || config.Height <= 0 || int64(config.Width)*int64(config.Height) > 64*1024*1024 {
			return inlineReject("invalid_inline_content", "input")
		}
	}
	return nil
}
