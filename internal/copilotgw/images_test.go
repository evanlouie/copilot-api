package copilotgw

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
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
	return &RealGateway{
		models:         []Model{model},
		modelsFetched:  time.Now(),
		modelsCacheTTL: time.Hour,
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
	decoded, err := base64.StdEncoding.DecodeString(attachment.Data)
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
	if attachment.Data != tinyPNG {
		t.Fatalf("unexpected data %#v", attachment.Data)
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

func TestResolvePromptRejectsAggregateImageSize(t *testing.T) {
	raw, err := base64.StdEncoding.DecodeString(tinyPNG)
	if err != nil {
		t.Fatal(err)
	}
	original := maxAggregateImageBytes
	// Permit a single image but not two, so the aggregate cap trips on the second
	// image rather than the per-image limit.
	maxAggregateImageBytes = int64(len(raw)) + 1
	defer func() { maxAggregateImageBytes = original }()

	gw := cachedModelGateway(Model{
		ID:             "vision",
		VisionKnown:    true,
		SupportsVision: true,
		Vision:         &VisionLimits{SupportedMediaTypes: []string{"image/png"}},
	})
	img := openai.ImageInput{URL: "data:image/png;base64," + tinyPNG}
	_, err = gw.resolvePrompt(context.Background(), "vision", openai.PromptContent{
		Images: []openai.ImageInput{img, img},
	}, "input")
	if err == nil {
		t.Fatal("expected aggregate image size rejection")
	}
}

func TestResolvePromptRejectsDefaultImageCountWithoutMetadata(t *testing.T) {
	gw := cachedModelGateway(Model{
		ID:             "vision",
		VisionKnown:    true,
		SupportsVision: true,
		// No MaxPromptImages metadata, so the default fallback count applies.
		Vision: &VisionLimits{SupportedMediaTypes: []string{"image/png"}},
	})
	images := make([]openai.ImageInput, defaultMaxPromptImages+1)
	for i := range images {
		images[i] = openai.ImageInput{URL: "data:image/png;base64," + tinyPNG}
	}
	_, err := gw.resolvePrompt(context.Background(), "vision", openai.PromptContent{Images: images}, "input")
	if err == nil {
		t.Fatalf("expected rejection when image count exceeds the default fallback of %d", defaultMaxPromptImages)
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
