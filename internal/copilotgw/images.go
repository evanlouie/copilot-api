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

const (
	maxImageBytes          = 50 << 20
	defaultMaxPromptImages = 20
)

// maxAggregateImageBytes caps the total decoded image bytes accepted per
// request. It is a var (not a const) so tests can exercise the aggregate limit
// without allocating the full default.
var maxAggregateImageBytes int64 = 100 << 20

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
	maxImages := int64(defaultMaxPromptImages)
	if modelInfo.Vision != nil && modelInfo.Vision.MaxPromptImages > 0 {
		maxImages = modelInfo.Vision.MaxPromptImages
	}
	if int64(len(prompt.Images)) > maxImages {
		return resolvedPrompt{}, openai.InvalidRequest(fmt.Sprintf("model supports at most %d image inputs per prompt", maxImages), param)
	}
	attachments := make([]copilot.Attachment, 0, len(prompt.Images))
	var aggregateBytes int64
	for i, image := range prompt.Images {
		resolved, err := resolveImageAttachment(ctx, image, i, modelInfo.Vision, param)
		if err != nil {
			return resolvedPrompt{}, err
		}
		aggregateBytes += resolved.bytes
		if aggregateBytes > maxAggregateImageBytes {
			return resolvedPrompt{}, openai.InvalidRequest(fmt.Sprintf("image inputs exceed the aggregate %d byte size limit", maxAggregateImageBytes), param)
		}
		attachments = append(attachments, resolved.attachment)
	}
	return resolvedPrompt{Text: prompt.Text, Attachments: attachments}, nil
}

type resolvedImageAttachment struct {
	attachment copilot.Attachment
	bytes      int64
}

func resolveImageAttachment(ctx context.Context, image openai.ImageInput, index int, limits *VisionLimits, param string) (resolvedImageAttachment, error) {
	raw := strings.TrimSpace(image.URL)
	if raw == "" {
		return resolvedImageAttachment{}, openai.InvalidRequest("image_url is required", param)
	}
	if strings.HasPrefix(strings.ToLower(raw), "data:") {
		return dataURLAttachment(raw, index, limits, param)
	}
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() {
		return resolvedImageAttachment{}, openai.InvalidRequest("image_url must be an absolute URL or data URL", param)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		if err := validateRemoteImageURL(u); err != nil {
			return resolvedImageAttachment{}, openai.InvalidRequest(err.Error(), param)
		}
		return remoteImageAttachment(ctx, u, index, limits, param)
	default:
		return resolvedImageAttachment{}, openai.InvalidRequest("image_url scheme must be http, https, or data", param)
	}
}

func dataURLAttachment(raw string, index int, limits *VisionLimits, param string) (resolvedImageAttachment, error) {
	mediaType, data, size, err := parseImageDataURL(raw, imageByteLimit(limits))
	if err != nil {
		return resolvedImageAttachment{}, openai.InvalidRequest(err.Error(), param)
	}
	if !mediaTypeAllowed(mediaType, limits) {
		return resolvedImageAttachment{}, openai.InvalidRequest("image MIME type is not supported by the selected model: "+mediaType, param)
	}
	displayName := imageDisplayName(index, mediaType, "")
	return resolvedImageAttachment{attachment: copilot.AttachmentBlob{Data: &data, MIMEType: mediaType, DisplayName: &displayName}, bytes: size}, nil
}

func remoteImageAttachment(ctx context.Context, u *url.URL, index int, limits *VisionLimits, param string) (resolvedImageAttachment, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return resolvedImageAttachment{}, openai.InvalidRequest("invalid image_url", param)
	}
	resp, err := imageHTTPClient.Do(req)
	if err != nil {
		return resolvedImageAttachment{}, openai.InvalidRequest("failed to fetch image_url: "+err.Error(), param)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resolvedImageAttachment{}, openai.InvalidRequest(fmt.Sprintf("image_url returned HTTP %d", resp.StatusCode), param)
	}
	limit := imageByteLimit(limits)
	if resp.ContentLength > limit {
		return resolvedImageAttachment{}, openai.InvalidRequest(imageSizeLimitMessage("image_url", limit), param)
	}
	body, err := readLimited(resp.Body, limit)
	if err != nil {
		return resolvedImageAttachment{}, openai.InvalidRequest(err.Error(), param)
	}
	if len(body) == 0 {
		return resolvedImageAttachment{}, openai.InvalidRequest("image_url returned an empty image", param)
	}
	mediaType := imageMediaType(resp.Header.Get("Content-Type"), body)
	if mediaType == "" {
		return resolvedImageAttachment{}, openai.InvalidRequest("image_url did not return a supported image MIME type", param)
	}
	if !mediaTypeAllowed(mediaType, limits) {
		return resolvedImageAttachment{}, openai.InvalidRequest("image MIME type is not supported by the selected model: "+mediaType, param)
	}
	data := base64.StdEncoding.EncodeToString(body)
	displayName := imageDisplayName(index, mediaType, path.Base(u.Path))
	return resolvedImageAttachment{attachment: copilot.AttachmentBlob{Data: &data, MIMEType: mediaType, DisplayName: &displayName}, bytes: int64(len(body))}, nil
}

func parseImageDataURL(raw string, limit int64) (string, string, int64, error) {
	comma := strings.IndexByte(raw, ',')
	if comma < 0 {
		return "", "", 0, fmt.Errorf("data URL image inputs must include base64 data")
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
		return "", "", 0, fmt.Errorf("data URL image inputs must be base64 encoded")
	}
	unescaped, err := url.PathUnescape(payload)
	if err != nil {
		return "", "", 0, fmt.Errorf("data URL image payload is not valid")
	}
	decoded, err := base64.StdEncoding.DecodeString(compactBase64(unescaped))
	if err != nil {
		return "", "", 0, fmt.Errorf("data URL image payload is not valid base64")
	}
	if len(decoded) == 0 {
		return "", "", 0, fmt.Errorf("data URL image payload is empty")
	}
	if int64(len(decoded)) > limit {
		return "", "", 0, fmt.Errorf("%s", imageSizeLimitMessage("image input", limit))
	}
	if mediaType == "" || mediaType == "application/octet-stream" {
		mediaType = imageMediaType("", decoded)
	}
	if mediaType == "" || !strings.HasPrefix(mediaType, "image/") {
		return "", "", 0, fmt.Errorf("data URL MIME type must be an image type")
	}
	return mediaType, base64.StdEncoding.EncodeToString(decoded), int64(len(decoded)), nil
}

func imageByteLimit(limits *VisionLimits) int64 {
	limit := int64(maxImageBytes)
	if limits != nil && limits.MaxPromptImageSize > 0 && limits.MaxPromptImageSize < limit {
		limit = limits.MaxPromptImageSize
	}
	return limit
}

func imageSizeLimitMessage(subject string, limit int64) string {
	return fmt.Sprintf("%s exceeds the %d byte size limit", subject, limit)
}

func readLimited(r io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read image_url: %w", err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("%s", imageSizeLimitMessage("image_url", limit))
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
