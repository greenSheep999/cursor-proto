package executor

import (
	"bytes"
	"image"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"image/png"
	"strings"
)

// minImageEdge is the smallest width/height Cursor reliably forwards to the
// model. Measured with interleaved A/B probes against a live account: 1x1 and
// 2x2 solid-colour PNGs are dropped roughly half the time (the model answers
// "I don't see an image attached"), while every trial at 4x4 and above was
// recognized. Conformance suites commonly probe vision with a 1x1 pixel, so
// the failure shows up as flaky multimodal support.
const minImageEdge = 8

// NormalizeImageAttachments upscales images whose smallest edge is below
// minImageEdge, leaving every other attachment untouched. Nearest-neighbour
// replication is used so the visible content is byte-identical in appearance;
// only the pixel dimensions change.
func NormalizeImageAttachments(attachments []Attachment) []Attachment {
	for index, attachment := range attachments {
		if attachment.Kind != "image" || len(attachment.Data) == 0 {
			continue
		}
		scaled, _, _, ok := upscaleTinyImage(attachment.MimeType, attachment.Data)
		if !ok {
			continue
		}
		attachments[index].Data = scaled
		attachments[index].MimeType = "image/png"
	}
	return attachments
}

// upscaleTinyImage reports ok=false when the image needs no change or cannot
// be decoded, so callers forward the original bytes unmodified.
func upscaleTinyImage(mimeType string, data []byte) ([]byte, int, int, bool) {
	decoded, err := decodeImage(mimeType, data)
	if err != nil {
		return nil, 0, 0, false
	}
	bounds := decoded.Bounds()
	sourceWidth, sourceHeight := bounds.Dx(), bounds.Dy()
	if sourceWidth <= 0 || sourceHeight <= 0 {
		return nil, 0, 0, false
	}
	if sourceWidth >= minImageEdge && sourceHeight >= minImageEdge {
		return nil, 0, 0, false
	}

	factor := 1
	for sourceWidth*factor < minImageEdge || sourceHeight*factor < minImageEdge {
		factor++
	}
	targetWidth, targetHeight := sourceWidth*factor, sourceHeight*factor
	target := image.NewRGBA(image.Rect(0, 0, targetWidth, targetHeight))
	for y := 0; y < targetHeight; y++ {
		for x := 0; x < targetWidth; x++ {
			source := decoded.At(bounds.Min.X+x/factor, bounds.Min.Y+y/factor)
			target.Set(x, y, source)
		}
	}
	// Flatten onto the RGBA buffer we already allocated; PNG keeps alpha, so
	// this is only guarding against decoders that hand back a paletted image.
	draw.Draw(target, target.Bounds(), target, image.Point{}, draw.Src)

	var buf bytes.Buffer
	if err := png.Encode(&buf, target); err != nil {
		return nil, 0, 0, false
	}
	return buf.Bytes(), targetWidth, targetHeight, true
}

func decodeImage(mimeType string, data []byte) (image.Image, error) {
	reader := bytes.NewReader(data)
	switch strings.ToLower(strings.TrimSpace(mimeType)) {
	case "image/png":
		return png.Decode(reader)
	case "image/jpeg", "image/jpg":
		return jpeg.Decode(reader)
	case "image/gif":
		return gif.Decode(reader)
	default:
		decoded, _, err := image.Decode(reader)
		return decoded, err
	}
}
