package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/MACOS-DO/sub4api/internal/pkg/managedfiles"
)

// ManagedFileURLSource is server-parsed request input, not an authorization
// grant. Callers must authorize its owner and merge the staged bytes into the
// normal managed-file plan before any BPS operation.
type ManagedFileURLSource struct {
	URL, Filename string
	Position      managedfiles.Position
}

type managedFileURLNetwork struct {
	lookup func(context.Context, string) ([]netip.Addr, error)
	dial   func(context.Context, string, string) (net.Conn, error)
}

// StageManagedFileURLs downloads request-local blobs. Existing descriptors must
// already include authorized public-file sizes and decoded inline sizes; they
// participate in the same count, message and request limits. No Files resource
// or remote operation is created here.
func StageManagedFileURLs(ctx context.Context, sources []ManagedFileURLSource, existing []ManagedInlineFileDescriptor, limits ManagedInlineLimits, privateParent string) (*ManagedInlineStaging, error) {
	return stageManagedFileURLs(ctx, sources, existing, limits, privateParent, defaultManagedFileURLNetwork())
}

func stageManagedFileURLs(ctx context.Context, sources []ManagedFileURLSource, existing []ManagedInlineFileDescriptor, limits ManagedInlineLimits, privateParent string, network managedFileURLNetwork) (_ *ManagedInlineStaging, resultErr error) {
	return stageManagedFileURLsWithFilesystem(ctx, sources, existing, limits, privateParent, network, defaultManagedInlineStagingFilesystem())
}

func stageManagedFileURLsWithFilesystem(ctx context.Context, sources []ManagedFileURLSource, existing []ManagedInlineFileDescriptor, limits ManagedInlineLimits, privateParent string, network managedFileURLNetwork, fs managedInlineStagingFilesystem) (_ *ManagedInlineStaging, resultErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	limits, err := normalizeManagedInlineLimits(limits)
	if err != nil {
		return nil, err
	}
	entries := make([]managedInlineEntry, 0, len(existing)+len(sources))
	positions := make(map[managedfiles.Position]bool, len(existing)+len(sources))
	for _, d := range existing {
		if d.Bytes <= 0 || d.Position.Input < 0 || d.Position.Part < 0 || positions[d.Position] {
			return nil, inlineReject("invalid_file_url_budget", "input")
		}
		positions[d.Position] = true
		entries = append(entries, managedInlineEntry{description: d})
	}
	targets := make([]*url.URL, len(sources))
	for i, source := range sources {
		target, err := parseManagedFileURL(source.URL)
		if err != nil {
			return nil, err
		}
		name := source.Filename
		if name == "" {
			name = path.Base(target.Path)
			if name == "." || name == "/" {
				name = "attachment"
			}
		}
		if !validManagedInlineFilename(name) || source.Position.Input < 0 || source.Position.Part < 0 || positions[source.Position] {
			return nil, inlineReject("invalid_file_url", "input")
		}
		positions[source.Position] = true
		targets[i] = target
		entries = append(entries, managedInlineEntry{description: ManagedInlineFileDescriptor{Filename: name, Position: source.Position}})
	}
	if err := validateManagedInlineBudget(entries, limits); err != nil {
		return nil, err
	}
	stage := &ManagedInlineStaging{files: make([]ManagedInlineStagedFile, len(sources)), names: make([]string, len(sources)), readers: make(map[*managedInlineReader]struct{}), removeAll: fs.removeAll}
	keep := false
	defer func() {
		if !keep {
			resultErr = errors.Join(resultErr, stage.Close())
		}
	}()
	operationCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	transport := &http.Transport{
		Proxy: nil, DialContext: managedFileURLDialer(network),
		DisableCompression: true, DisableKeepAlives: true,
		MaxResponseHeaderBytes: 64 << 10, ResponseHeaderTimeout: 10 * time.Second,
		TLSHandshakeTimeout: 5 * time.Second,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for i, target := range targets {
		d := entries[len(existing)+i].description
		remaining := limits.FileBytes
		var total, message int64
		for j, entry := range entries {
			if j == len(existing)+i {
				continue
			}
			total += entry.description.Bytes
			if entry.description.Position.Input == d.Position.Input {
				message += entry.description.Bytes
			}
		}
		if limits.TotalBytes-total < remaining {
			remaining = limits.TotalBytes - total
		}
		if limits.MessageBytes-message < remaining {
			remaining = limits.MessageBytes - message
		}
		if remaining <= 0 {
			return nil, inlineReject("managed_request_limit", "input")
		}
		resp, err := getManagedFileURL(operationCtx, client, target)
		if err != nil {
			return nil, err
		}
		mediaType, mediaErr := managedInlineMediaType(d.Filename, resp.Header.Get("Content-Type"))
		if mediaErr != nil {
			resp.Body.Close()
			return nil, mediaErr
		}
		d.MIME = mediaType
		if managedInlineImageType(d) != "" && remaining > managedInlineImageMaxBytes {
			remaining = managedInlineImageMaxBytes
		}
		if resp.ContentLength > remaining {
			resp.Body.Close()
			return nil, inlineReject("managed_file_limit", "input")
		}
		if stage.root == nil {
			dir, err := fs.mkdirTemp(privateParent, "bps-url-*")
			if err != nil {
				resp.Body.Close()
				return nil, inlineStorageError(err)
			}
			stage.dir = dir
			stage.root, err = fs.openRoot(dir)
			if err != nil {
				resp.Body.Close()
				return nil, inlineStorageError(err)
			}
		}
		name := "blob-" + strconv.Itoa(i)
		file, err := fs.create(stage.root, name)
		if err != nil {
			resp.Body.Close()
			return nil, inlineStorageError(err)
		}
		hash := sha256.New()
		size, copyErr := io.Copy(io.MultiWriter(managedFileURLStorageWriter{file}, hash), io.LimitReader(resp.Body, remaining+1))
		bodyCloseErr, fileCloseErr := resp.Body.Close(), file.Close()
		if operationCtx.Err() != nil {
			return nil, operationCtx.Err()
		}
		// LimitReader deliberately reads one byte beyond the allowed budget. A
		// remote peer may observe the client closing the body and report a read
		// error; the byte count is the authoritative signal in that case.
		if size > remaining {
			return nil, inlineReject("managed_file_limit", "input")
		}
		var storageErr *managedInlineStorageError
		if errors.As(copyErr, &storageErr) {
			return nil, storageErr
		}
		if copyErr != nil || bodyCloseErr != nil {
			return nil, inlineReject("file_url_fetch_failed", "input")
		}
		if fileCloseErr != nil {
			return nil, inlineStorageError(fileCloseErr)
		}
		if size <= 0 {
			return nil, inlineReject("managed_file_limit", "input")
		}
		if resp.ContentLength >= 0 && resp.ContentLength != size {
			return nil, inlineReject("file_url_fetch_failed", "input")
		}
		d.Bytes = size
		content, err := stage.root.Open(name)
		if err != nil {
			return nil, inlineStorageError(err)
		}
		contentErr := validateManagedFileContent(operationCtx, d, func() io.Reader { return io.NewSectionReader(content, 0, size) })
		closeErr := content.Close()
		if contentErr != nil {
			return nil, contentErr
		}
		if closeErr != nil {
			return nil, inlineStorageError(closeErr)
		}
		entries[len(existing)+i].description = d
		stage.files[i] = ManagedInlineStagedFile{ManagedInlineFileDescriptor: d, SHA256: hex.EncodeToString(hash.Sum(nil))}
		stage.names[i] = name
	}
	if err := validateManagedInlineBudget(entries, limits); err != nil {
		return nil, err
	}
	stage.mu.Lock()
	stage.stop = context.AfterFunc(ctx, func() { _ = stage.Close() })
	stage.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	keep = true
	return stage, nil
}

// Keep disk failures distinguishable from response-body read failures without
// exposing the private staging path in the public error text.
type managedFileURLStorageWriter struct{ io.Writer }

func (w managedFileURLStorageWriter) Write(data []byte) (int, error) {
	n, err := w.Writer.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return n, inlineStorageError(err)
	}
	return n, nil
}

func parseManagedFileURL(raw string) (*url.URL, error) {
	invalid := func() (*url.URL, error) { return nil, inlineReject("invalid_file_url", "input") }
	if len(raw) == 0 || len(raw) > 16<<10 || strings.TrimSpace(raw) != raw || strings.ContainsAny(raw, "\\\r\n\t") {
		return invalid()
	}
	target, err := url.Parse(raw)
	if err != nil || target.Opaque != "" || target.User != nil || target.Fragment != "" || (target.Scheme != "https" && target.Scheme != "http") {
		return invalid()
	}
	host := strings.TrimSuffix(strings.ToLower(target.Hostname()), ".")
	if host == "" {
		return invalid()
	}
	for _, r := range host {
		if r > 127 || r <= 32 {
			return invalid()
		}
	}
	if strings.Contains(host, "%") {
		return invalid()
	}
	if port := target.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n <= 0 || n > 65535 {
			return invalid()
		}
	}
	if managedFileURLBlockedHost(host) {
		return nil, inlineReject("unsafe_file_url", "input")
	}
	if ip, err := netip.ParseAddr(host); err == nil && !managedFileURLPublicIP(ip) {
		return nil, inlineReject("unsafe_file_url", "input")
	}
	return target, nil
}

func managedFileURLBlockedHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	return host == "localhost" || strings.HasSuffix(host, ".localhost") || host == "metadata" || host == "metadata.google.internal" || host == "metadata.goog" || host == "instance-data" || strings.HasSuffix(host, ".internal") || host == "localhost.localdomain"
}

var managedFileURLBlockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("192.31.196.0/24"), netip.MustParsePrefix("192.52.193.0/24"), netip.MustParsePrefix("192.175.48.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001::/23"), netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("2002::/16"), netip.MustParsePrefix("3fff::/20"),
	netip.MustParsePrefix("2620:4f:8000::/48"),
}

func managedFileURLPublicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.Zone() != "" {
		return false
	}
	if ip.Is6() && !netip.MustParsePrefix("2000::/3").Contains(ip) {
		return false
	}
	for _, prefix := range managedFileURLBlockedPrefixes {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}

func managedFileURLDialer(network managedFileURLNetwork) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, protocol, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil || managedFileURLBlockedHost(host) {
			return nil, inlineReject("unsafe_file_url", "input")
		}
		var ips []netip.Addr
		if ip, err := netip.ParseAddr(host); err == nil {
			ips = []netip.Addr{ip}
		} else {
			ips, err = network.lookup(ctx, host)
			if err != nil {
				return nil, inlineReject("file_url_fetch_failed", "input")
			}
		}
		if len(ips) == 0 || len(ips) > 64 {
			return nil, inlineReject("unsafe_file_url", "input")
		}
		for _, ip := range ips {
			if !managedFileURLPublicIP(ip) {
				return nil, inlineReject("unsafe_file_url", "input")
			}
		}
		for _, ip := range ips {
			conn, err := network.dial(ctx, protocol, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
		}
		return nil, inlineReject("file_url_fetch_failed", "input")
	}
}

func getManagedFileURL(ctx context.Context, client *http.Client, target *url.URL) (*http.Response, error) {
	for redirects := 0; ; redirects++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
		if err != nil {
			return nil, inlineReject("invalid_file_url", "input")
		}
		req.Header.Set("Accept-Encoding", "identity")
		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			var rejected *managedfiles.Error
			if errors.As(err, &rejected) {
				return nil, rejected
			}
			return nil, inlineReject("file_url_fetch_failed", "input")
		}
		switch resp.StatusCode {
		case 301, 302, 303, 307, 308:
			location := resp.Header.Get("Location")
			resp.Body.Close()
			if len(resp.Header.Values("Location")) != 1 {
				return nil, inlineReject("invalid_file_url", "input")
			}
			if redirects >= 3 {
				return nil, inlineReject("file_url_redirect_limit", "input")
			}
			resolved, err := target.Parse(location)
			if err != nil || location == "" {
				return nil, inlineReject("invalid_file_url", "input")
			}
			if target.Scheme == "https" && resolved.Scheme != "https" {
				return nil, inlineReject("unsafe_file_url", "input")
			}
			target, err = parseManagedFileURL(resolved.String())
			if err != nil {
				return nil, err
			}
		default:
			encoding := resp.Header.Get("Content-Encoding")
			if resp.StatusCode != http.StatusOK || encoding != "" && !strings.EqualFold(encoding, "identity") || len(resp.Header.Values("Content-Type")) > 1 || len(resp.Header.Values("Content-Encoding")) > 1 || len(resp.Header.Values("Content-Range")) != 0 {
				resp.Body.Close()
				return nil, inlineReject("file_url_fetch_failed", "input")
			}
			return resp, nil
		}
	}
}
