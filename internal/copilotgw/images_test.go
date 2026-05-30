package copilotgw

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/evanlouie/copilot-api/internal/openai"

	copilot "github.com/github/copilot-sdk/go"
)

const tinyPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+/p9sAAAAASUVORK5CYII="

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
	attachment := got.Attachments[0]
	if attachment.Type != copilot.AttachmentTypeBlob {
		t.Fatalf("unexpected attachment type %q", attachment.Type)
	}
	if attachment.MIMEType == nil || *attachment.MIMEType != "image/png" {
		t.Fatalf("unexpected MIME type %#v", attachment.MIMEType)
	}
	if attachment.DisplayName == nil || *attachment.DisplayName != "image_1.png" {
		t.Fatalf("unexpected display name %#v", attachment.DisplayName)
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pngBytes)
	}))
	defer srv.Close()
	gw := cachedModelGateway(Model{
		ID:             "vision",
		VisionKnown:    true,
		SupportsVision: true,
		Vision:         &VisionLimits{SupportedMediaTypes: []string{"image/png"}},
	})
	got, err := gw.resolvePrompt(context.Background(), "vision", openai.PromptContent{
		Images: []openai.ImageInput{{URL: srv.URL + "/shot.png"}},
	}, "input")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Attachments) != 1 {
		t.Fatalf("expected one attachment, got %d", len(got.Attachments))
	}
	if got.Attachments[0].DisplayName == nil || *got.Attachments[0].DisplayName != "shot.png" {
		t.Fatalf("unexpected display name %#v", got.Attachments[0].DisplayName)
	}
	if got.Attachments[0].Data == nil || *got.Attachments[0].Data != tinyPNG {
		t.Fatalf("unexpected data %#v", got.Attachments[0].Data)
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
