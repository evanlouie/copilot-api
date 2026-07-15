package copilotgw

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/evanlouie/copilot-api/internal/openai"

	copilot "github.com/github/copilot-sdk/go"
)

const tinyPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+/p9sAAAAASUVORK5CYII="

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func cachedModelGateway(model Model) *RealGateway {
	return &RealGateway{modelCache: &modelCache{models: []Model{model}, fetched: time.Now(), ttl: time.Hour}}
}

func TestPublicIPRejectsSpecialUseRanges(t *testing.T) {
	for _, address := range []string{
		"0.0.0.1", "100.64.0.1", "100.100.100.200", "192.0.0.1", "192.0.2.1",
		"192.88.99.1", "198.18.0.1", "198.51.100.1", "203.0.113.1", "240.0.0.1",
		"64:ff9b::c0a8:101", "64:ff9b:1::c0a8:101", "100::1", "2001:2::1", "2001:db8::1", "3fff::1",
	} {
		if publicIP(net.ParseIP(address)) {
			t.Errorf("publicIP(%s) = true", address)
		}
	}
	for _, address := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		if !publicIP(net.ParseIP(address)) {
			t.Errorf("publicIP(%s) = false", address)
		}
	}
}

func TestResolvePromptDataURLAttachment(t *testing.T) {
	gw := cachedModelGateway(Model{
		ID:             "vision",
		VisionKnown:    true,
		SupportsVision: true,
		Vision:         &VisionLimits{SupportedMediaTypes: []string{"image/png"}, MaxPromptImages: 1},
	})
	got, err := gw.resolvePrompt(context.Background(), "vision", openai.PromptContent{
		Text:   "describe",
		Images: []openai.ImageInput{{URL: "data:image/png;base64," + tinyPNG}},
	}, "input")
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != "describe" {
		t.Fatalf("unexpected text %q", got.Text)
	}
	if len(got.Attachments) != 1 {
		t.Fatalf("expected one attachment, got %d", len(got.Attachments))
	}
	attachment := requireBlobAttachment(t, got.Attachments[0])
	if attachment.Type() != copilot.AttachmentTypeBlob {
		t.Fatalf("unexpected attachment type %q", attachment.Type())
	}
	if attachment.MIMEType != "image/png" {
		t.Fatalf("unexpected MIME type %#v", attachment.MIMEType)
	}
	if attachment.DisplayName == nil || *attachment.DisplayName != "image_1.png" {
		t.Fatalf("unexpected display name %#v", attachment.DisplayName)
	}
	if attachment.Data == nil {
		t.Fatal("attachment data is nil")
	}
	decoded, err := base64.StdEncoding.DecodeString(*attachment.Data)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded) == 0 {
		t.Fatal("decoded attachment data is empty")
	}
}

func TestResolvePromptRejectsNonVisionModel(t *testing.T) {
	gw := cachedModelGateway(Model{ID: "text", VisionKnown: true, SupportsVision: false})
	_, err := gw.resolvePrompt(context.Background(), "text", openai.PromptContent{
		Text:   "describe",
		Images: []openai.ImageInput{{URL: "data:image/png;base64," + tinyPNG}},
	}, "input")
	if err == nil {
		t.Fatal("expected non-vision model rejection")
	}
}

func TestResolvePromptFetchesRemoteImage(t *testing.T) {
	pngBytes, err := base64.StdEncoding.DecodeString(tinyPNG)
	if err != nil {
		t.Fatal(err)
	}
	oldClient := imageHTTPClient
	imageHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode:    http.StatusOK,
			Header:        http.Header{"Content-Type": []string{"image/png"}},
			Body:          io.NopCloser(bytes.NewReader(pngBytes)),
			ContentLength: int64(len(pngBytes)),
			Request:       req,
		}, nil
	})}
	defer func() { imageHTTPClient = oldClient }()
	gw := cachedModelGateway(Model{
		ID:             "vision",
		VisionKnown:    true,
		SupportsVision: true,
		Vision:         &VisionLimits{SupportedMediaTypes: []string{"image/png"}},
	})
	got, err := gw.resolvePrompt(context.Background(), "vision", openai.PromptContent{
		Images: []openai.ImageInput{{URL: "http://93.184.216.34/shot.png"}},
	}, "input")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Attachments) != 1 {
		t.Fatalf("expected one attachment, got %d", len(got.Attachments))
	}
	attachment := requireBlobAttachment(t, got.Attachments[0])
	if attachment.DisplayName == nil || *attachment.DisplayName != "shot.png" {
		t.Fatalf("unexpected display name %#v", attachment.DisplayName)
	}
	if attachment.Data == nil || *attachment.Data != tinyPNG {
		t.Fatalf("unexpected data %#v", attachment.Data)
	}
}

func TestWarmResolvedImageIsReusedWithoutRefetch(t *testing.T) {
	pngBytes, err := base64.StdEncoding.DecodeString(tinyPNG)
	if err != nil {
		t.Fatal(err)
	}
	fetches := 0
	oldClient := imageHTTPClient
	imageHTTPClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		fetches++
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"image/png"}}, Body: io.NopCloser(bytes.NewReader(pngBytes)), ContentLength: int64(len(pngBytes)), Request: req}, nil
	})}
	defer func() { imageHTTPClient = oldClient }()
	gateway := cachedModelGateway(Model{ID: "vision", VisionKnown: true, SupportsVision: true, Vision: &VisionLimits{SupportedMediaTypes: []string{"image/png"}}})
	budget := newImageRequestBudget()
	warmPrompt, err := gateway.resolvePromptWithImageBudget(context.Background(), "vision", openai.PromptContent{Images: []openai.ImageInput{{URL: "http://93.184.216.34/warm.png"}}}, "input", budget)
	if err != nil {
		t.Fatal(err)
	}
	warm := &WarmResponseSession{responseID: "resp_warm", model: "vision", input: warmPrompt, imageBudget: budget}
	request := ResponseRequest{Model: "vision", PreviousResponseID: "resp_warm", Input: openai.PromptContent{Text: "continue"}}
	used, ok := warm.use(&request)
	if !ok {
		t.Fatal("warm response was not used")
	}
	current, err := gateway.resolvePromptWithImageBudget(context.Background(), "vision", request.Input, "input", used.imageBudget)
	if err != nil {
		t.Fatal(err)
	}
	combined := combineResolvedPrompts(used.prompt, current)
	if fetches != 1 || len(combined.Attachments) != 1 {
		t.Fatalf("fetches=%d attachments=%d", fetches, len(combined.Attachments))
	}
}

func TestResolvePromptRejectsUnknownVisionSupport(t *testing.T) {
	gw := cachedModelGateway(Model{ID: "unknown", VisionKnown: false})
	_, err := gw.resolvePrompt(context.Background(), "unknown", openai.PromptContent{
		Images: []openai.ImageInput{{URL: "data:image/png;base64," + tinyPNG}},
	}, "input")
	if err == nil {
		t.Fatal("expected unknown vision capability rejection")
	}
}

func TestResolvePromptRejectsPrivateRemoteImageHost(t *testing.T) {
	gw := cachedModelGateway(Model{ID: "vision", VisionKnown: true, SupportsVision: true})
	_, err := gw.resolvePrompt(context.Background(), "vision", openai.PromptContent{
		Images: []openai.ImageInput{{URL: "http://127.0.0.1/image.png"}},
	}, "input")
	if err == nil {
		t.Fatal("expected private image_url host rejection")
	}
}

func TestResolvePromptRejectsModelImageSizeLimit(t *testing.T) {
	gw := cachedModelGateway(Model{
		ID:             "vision",
		VisionKnown:    true,
		SupportsVision: true,
		Vision:         &VisionLimits{SupportedMediaTypes: []string{"image/png"}, MaxPromptImageSize: 1},
	})
	_, err := gw.resolvePrompt(context.Background(), "vision", openai.PromptContent{
		Images: []openai.ImageInput{{URL: "data:image/png;base64," + tinyPNG}},
	}, "input")
	if err == nil {
		t.Fatal("expected per-image size limit rejection")
	}
}

func TestResolvePromptEnforcesImageCountAcrossMessages(t *testing.T) {
	gw := cachedModelGateway(Model{
		ID: "vision", VisionKnown: true, SupportsVision: true,
		Vision: &VisionLimits{SupportedMediaTypes: []string{"image/png"}, MaxPromptImages: 1},
	})
	budget := newImageRequestBudget()
	prompt := openai.PromptContent{Images: []openai.ImageInput{{URL: "data:image/png;base64," + tinyPNG}}}
	if _, err := gw.resolvePromptWithImageBudget(context.Background(), "vision", prompt, "messages.0.content", budget); err != nil {
		t.Fatal(err)
	}
	if _, err := gw.resolvePromptWithImageBudget(context.Background(), "vision", prompt, "messages.1.content", budget); err == nil {
		t.Fatal("expected the request-wide image count to reject the second message")
	}
}

func TestResolvePromptDoesNotAddImageCountWithoutModelLimit(t *testing.T) {
	gw := cachedModelGateway(Model{ID: "vision", VisionKnown: true, SupportsVision: true, Vision: &VisionLimits{SupportedMediaTypes: []string{"image/png"}}})
	images := make([]openai.ImageInput, 21)
	for i := range images {
		images[i] = openai.ImageInput{URL: "data:image/png;base64," + tinyPNG}
	}
	if _, err := gw.resolvePrompt(context.Background(), "vision", openai.PromptContent{Images: images}, "input"); err != nil {
		t.Fatalf("proxy added an image-count limit not advertised by the model: %v", err)
	}
}

func TestResolvePromptRejectsUnsupportedMIME(t *testing.T) {
	gw := cachedModelGateway(Model{
		ID:             "vision",
		VisionKnown:    true,
		SupportsVision: true,
		Vision:         &VisionLimits{SupportedMediaTypes: []string{"image/jpeg"}},
	})
	_, err := gw.resolvePrompt(context.Background(), "vision", openai.PromptContent{
		Images: []openai.ImageInput{{URL: "data:image/png;base64," + tinyPNG}},
	}, "input")
	if err == nil {
		t.Fatal("expected unsupported MIME rejection")
	}
}

func requireBlobAttachment(t *testing.T, attachment copilot.Attachment) copilot.AttachmentBlob {
	t.Helper()
	switch a := attachment.(type) {
	case copilot.AttachmentBlob:
		return a
	case *copilot.AttachmentBlob:
		if a == nil {
			t.Fatal("attachment is nil")
		}
		return *a
	default:
		t.Fatalf("unexpected attachment type %T", attachment)
		return copilot.AttachmentBlob{}
	}
}
