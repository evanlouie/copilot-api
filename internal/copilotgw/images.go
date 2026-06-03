package copilotgw

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/evanlouie/copilot-api/internal/openai"

	copilot "github.com/github/copilot-sdk/go"
)

const maxImageBytes = 50 << 20

var imageHTTPClient = &http.Client{
	Timeout:       30 * time.Second,
	Transport:     safeImageTransport(),
	CheckRedirect: safeImageRedirect,
}

func safeImageTransport() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = safeImageDialContext
	return transport
}

func safeImageRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return fmt.Errorf("stopped after 10 image_url redirects")
	}
	return validateRemoteImageURL(req.URL)
}

func validateRemoteImageURL(u *url.URL) error {
	if u == nil {
		return fmt.Errorf("image_url is invalid")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("image_url redirect scheme must remain http or https")
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("image_url host is required")
	}
	if ip := net.ParseIP(host); ip != nil {
		if !publicIP(ip) {
			return fmt.Errorf("image_url host resolves to a non-public address")
		}
	}
	return nil
}

func safeImageDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ips, err := resolvePublicIPs(ctx, host)
	if err != nil {
		return nil, err
	}
	var lastErr error
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	for _, ip := range ips {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("image_url host resolves to no public addresses")
}

func resolvePublicIPs(ctx context.Context, host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		if !publicIP(ip) {
			return nil, fmt.Errorf("image_url host resolves to a non-public address")
		}
		return []net.IP{ip}, nil
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, addr := range addrs {
		if publicIP(addr.IP) {
			ips = append(ips, addr.IP)
		}
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("image_url host resolves to no public addresses")
	}
	return ips, nil
}

func publicIP(ip net.IP) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	addr = addr.Unmap()
	return addr.IsGlobalUnicast() && !addr.IsPrivate() && !addr.IsLoopback() && !addr.IsLinkLocalUnicast() && !addr.IsLinkLocalMulticast() && !addr.IsMulticast() && !addr.IsUnspecified()
}

type resolvedPrompt struct {
	Text        string
	Attachments []copilot.Attachment
}

func (g *RealGateway) resolvePrompt(ctx context.Context, model string, prompt openai.PromptContent, param string) (resolvedPrompt, error) {
	if len(prompt.Images) == 0 {
		return resolvedPrompt{Text: prompt.Text}, nil
	}
	modelInfo, err := g.findModel(ctx, model)
	if err != nil {
		return resolvedPrompt{}, err
	}
	if !modelInfo.VisionKnown || !modelInfo.SupportsVision {
		return resolvedPrompt{}, openai.InvalidRequest("model does not support image inputs: "+model, param)
	}
	if modelInfo.Vision != nil && modelInfo.Vision.MaxPromptImages > 0 && int64(len(prompt.Images)) > modelInfo.Vision.MaxPromptImages {
		return resolvedPrompt{}, openai.InvalidRequest(fmt.Sprintf("model supports at most %d image inputs per prompt", modelInfo.Vision.MaxPromptImages), param)
	}
	attachments := make([]copilot.Attachment, 0, len(prompt.Images))
	for i, image := range prompt.Images {
		attachment, err := resolveImageAttachment(ctx, image, i, modelInfo.Vision, param)
		if err != nil {
			return resolvedPrompt{}, err
		}
		attachments = append(attachments, attachment)
	}
	return resolvedPrompt{Text: prompt.Text, Attachments: attachments}, nil
}

func resolveImageAttachment(ctx context.Context, image openai.ImageInput, index int, limits *VisionLimits, param string) (copilot.Attachment, error) {
	raw := strings.TrimSpace(image.URL)
	if raw == "" {
		return nil, openai.InvalidRequest("image_url is required", param)
	}
	if strings.HasPrefix(strings.ToLower(raw), "data:") {
		return dataURLAttachment(raw, index, limits, param)
	}
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() {
		return nil, openai.InvalidRequest("image_url must be an absolute URL or data URL", param)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		if err := validateRemoteImageURL(u); err != nil {
			return nil, openai.InvalidRequest(err.Error(), param)
		}
		return remoteImageAttachment(ctx, u, index, limits, param)
	default:
		return nil, openai.InvalidRequest("image_url scheme must be http, https, or data", param)
	}
}

func dataURLAttachment(raw string, index int, limits *VisionLimits, param string) (copilot.Attachment, error) {
	mediaType, data, err := parseImageDataURL(raw)
	if err != nil {
		return nil, openai.InvalidRequest(err.Error(), param)
	}
	if !mediaTypeAllowed(mediaType, limits) {
		return nil, openai.InvalidRequest("image MIME type is not supported by the selected model: "+mediaType, param)
	}
	displayName := imageDisplayName(index, mediaType, "")
	return copilot.UserMessageAttachmentBlob{Data: data, MIMEType: mediaType, DisplayName: &displayName}, nil
}

func remoteImageAttachment(ctx context.Context, u *url.URL, index int, limits *VisionLimits, param string) (copilot.Attachment, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, openai.InvalidRequest("invalid image_url", param)
	}
	resp, err := imageHTTPClient.Do(req)
	if err != nil {
		return nil, openai.InvalidRequest("failed to fetch image_url: "+err.Error(), param)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, openai.InvalidRequest(fmt.Sprintf("image_url returned HTTP %d", resp.StatusCode), param)
	}
	if resp.ContentLength > maxImageBytes {
		return nil, openai.InvalidRequest("image_url exceeds the 50 MB size limit", param)
	}
	body, err := readLimited(resp.Body, maxImageBytes)
	if err != nil {
		return nil, openai.InvalidRequest(err.Error(), param)
	}
	if len(body) == 0 {
		return nil, openai.InvalidRequest("image_url returned an empty image", param)
	}
	mediaType := imageMediaType(resp.Header.Get("Content-Type"), body)
	if mediaType == "" {
		return nil, openai.InvalidRequest("image_url did not return a supported image MIME type", param)
	}
	if !mediaTypeAllowed(mediaType, limits) {
		return nil, openai.InvalidRequest("image MIME type is not supported by the selected model: "+mediaType, param)
	}
	data := base64.StdEncoding.EncodeToString(body)
	displayName := imageDisplayName(index, mediaType, path.Base(u.Path))
	return copilot.UserMessageAttachmentBlob{Data: data, MIMEType: mediaType, DisplayName: &displayName}, nil
}

func parseImageDataURL(raw string) (string, string, error) {
	comma := strings.IndexByte(raw, ',')
	if comma < 0 {
		return "", "", fmt.Errorf("data URL image inputs must include base64 data")
	}
	meta := raw[len("data:"):comma]
	payload := raw[comma+1:]
	parts := strings.Split(meta, ";")
	mediaType := normalizeMediaType(parts[0])
	base64Encoded := false
	for _, part := range parts[1:] {
		if strings.EqualFold(strings.TrimSpace(part), "base64") {
			base64Encoded = true
			break
		}
	}
	if !base64Encoded {
		return "", "", fmt.Errorf("data URL image inputs must be base64 encoded")
	}
	unescaped, err := url.PathUnescape(payload)
	if err != nil {
		return "", "", fmt.Errorf("data URL image payload is not valid")
	}
	decoded, err := base64.StdEncoding.DecodeString(compactBase64(unescaped))
	if err != nil {
		return "", "", fmt.Errorf("data URL image payload is not valid base64")
	}
	if len(decoded) == 0 {
		return "", "", fmt.Errorf("data URL image payload is empty")
	}
	if len(decoded) > maxImageBytes {
		return "", "", fmt.Errorf("image input exceeds the 50 MB size limit")
	}
	if mediaType == "" || mediaType == "application/octet-stream" {
		mediaType = imageMediaType("", decoded)
	}
	if mediaType == "" || !strings.HasPrefix(mediaType, "image/") {
		return "", "", fmt.Errorf("data URL MIME type must be an image type")
	}
	return mediaType, base64.StdEncoding.EncodeToString(decoded), nil
}

func readLimited(r io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read image_url: %w", err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("image_url exceeds the 50 MB size limit")
	}
	return body, nil
}

func imageMediaType(header string, body []byte) string {
	mediaType := normalizeMediaType(header)
	if mediaType != "" && strings.HasPrefix(mediaType, "image/") {
		return mediaType
	}
	if len(body) == 0 {
		return ""
	}
	detected := normalizeMediaType(http.DetectContentType(body))
	if strings.HasPrefix(detected, "image/") {
		return detected
	}
	return ""
}

func normalizeMediaType(mediaType string) string {
	mediaType = strings.TrimSpace(strings.ToLower(mediaType))
	if mediaType == "" {
		return ""
	}
	if parsed, _, err := mime.ParseMediaType(mediaType); err == nil {
		mediaType = strings.ToLower(parsed)
	}
	if mediaType == "image/jpg" {
		return "image/jpeg"
	}
	return mediaType
}

func mediaTypeAllowed(mediaType string, limits *VisionLimits) bool {
	if limits == nil || len(limits.SupportedMediaTypes) == 0 {
		return true
	}
	mediaType = normalizeMediaType(mediaType)
	for _, candidate := range limits.SupportedMediaTypes {
		if normalizeMediaType(candidate) == mediaType {
			return true
		}
	}
	return false
}

func imageDisplayName(index int, mediaType, fallback string) string {
	fallback = strings.TrimSpace(fallback)
	if fallback != "" && fallback != "." && fallback != "/" {
		return fallback
	}
	return fmt.Sprintf("image_%d%s", index+1, extensionForMediaType(mediaType))
}

func extensionForMediaType(mediaType string) string {
	switch normalizeMediaType(mediaType) {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	default:
		if exts, err := mime.ExtensionsByType(mediaType); err == nil && len(exts) > 0 {
			return exts[0]
		}
		return ".img"
	}
}

func compactBase64(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\n', '\r', '\t':
			return -1
		default:
			return r
		}
	}, s)
}

func (g *RealGateway) findModel(ctx context.Context, id string) (Model, error) {
	if id == "" {
		return Model{}, openai.InvalidRequest("model is required", "model")
	}
	models, err := g.refreshModels(ctx, false)
	if err != nil {
		return Model{}, openai.Upstream(err.Error())
	}
	if m, ok := findModel(models, id); ok {
		return m, nil
	}
	models, err = g.refreshModels(ctx, true)
	if err != nil {
		return Model{}, openai.Upstream(err.Error())
	}
	if m, ok := findModel(models, id); ok {
		return m, nil
	}
	return Model{}, openai.NotFound("model not found: "+id, "model_not_found")
}

func findModel(models []Model, id string) (Model, bool) {
	for _, m := range models {
		if m.ID == id {
			return m, true
		}
	}
	return Model{}, false
}
