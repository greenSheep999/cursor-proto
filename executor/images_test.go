package executor

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func solidPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, color.RGBA{R: 255, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return buf.Bytes()
}

func decodeSize(t *testing.T, data []byte) (int, int) {
	t.Helper()
	config, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode config: %v", err)
	}
	return config.Width, config.Height
}

// Cursor silently drops sub-4px images about half the time, which shows up as
// flaky vision support, so anything below the floor must be scaled up.
func TestNormalizeImageAttachmentsUpscalesTinyImages(t *testing.T) {
	attachments := NormalizeImageAttachments([]Attachment{
		{Kind: "image", MimeType: "image/png", Data: solidPNG(t, 1, 1)},
	})
	width, height := decodeSize(t, attachments[0].Data)
	if width < minImageEdge || height < minImageEdge {
		t.Fatalf("upscaled to %dx%d, want at least %d on each edge", width, height, minImageEdge)
	}
}

func TestNormalizeImageAttachmentsPreservesColor(t *testing.T) {
	attachments := NormalizeImageAttachments([]Attachment{
		{Kind: "image", MimeType: "image/png", Data: solidPNG(t, 1, 1)},
	})
	decoded, err := png.Decode(bytes.NewReader(attachments[0].Data))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	r, g, b, _ := decoded.At(0, 0).RGBA()
	if r>>8 != 255 || g>>8 != 0 || b>>8 != 0 {
		t.Fatalf("pixel = %d,%d,%d, want pure red", r>>8, g>>8, b>>8)
	}
}

func TestNormalizeImageAttachmentsLeavesLargeImagesUntouched(t *testing.T) {
	original := solidPNG(t, 64, 64)
	attachments := NormalizeImageAttachments([]Attachment{
		{Kind: "image", MimeType: "image/png", Data: original},
	})
	if !bytes.Equal(attachments[0].Data, original) {
		t.Fatal("image at or above the floor must be forwarded byte-for-byte")
	}
}

func TestNormalizeImageAttachmentsIgnoresNonImages(t *testing.T) {
	original := []byte("%PDF-1.4 not an image")
	attachments := NormalizeImageAttachments([]Attachment{
		{Kind: "document", MimeType: "application/pdf", Data: original},
	})
	if !bytes.Equal(attachments[0].Data, original) {
		t.Fatal("documents must not be rewritten by image normalization")
	}
}

// An image we cannot decode is forwarded unchanged rather than dropped.
func TestNormalizeImageAttachmentsKeepsUndecodableImage(t *testing.T) {
	original := []byte("not really a png")
	attachments := NormalizeImageAttachments([]Attachment{
		{Kind: "image", MimeType: "image/png", Data: original},
	})
	if !bytes.Equal(attachments[0].Data, original) {
		t.Fatal("undecodable image must be forwarded as-is")
	}
}
