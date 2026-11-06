package chathistory

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"image"
	_ "image/gif"  // decode GIF screenshots for their dimensions
	_ "image/jpeg" // decode attached JPEGs to re-encode them
	_ "image/png"  // decode attached PNGs to re-encode them
	"strings"

	"github.com/gen2brain/webp"
)

// Images were most of what the archive stored once opaque provider fields were gone,
// and nearly all of them were screenshots tools hand back to the model: browser and
// script captures, rendered pages and files an agent viewed. Those are dropped, keeping
// what they were (media type, size and dimensions) so the conversation still shows an
// image was there. An image a person attached is kept, re-encoded as WebP, which Claude,
// OpenAI and Gemini all accept, so a stored message still reads as a valid request.
//
// A message is hashed before its images change, so the archive keys it exactly as it
// did when images were stored whole: a session that spans the change keeps one chain of
// nodes, and existing rows can be rewritten under their old hashes

// imageQuality is the WebP quality for attached images; at 80 small UI and terminal
// text stays sharp while a screenshot shrinks to about a quarter of its PNG size
const imageQuality = 80

// hasImages is a cheap check for content that may hold an inline image, so most
// messages skip decoding
func hasImages(content []byte) bool {
	return bytes.Contains(content, []byte(`"image/`)) || bytes.Contains(content, []byte(`data:image/`))
}

// shrinkImages rewrites the inline images of a decoded message in place and reports
// whether it changed any. It leaves WebP, dropped and undecodable images alone, so
// running it twice changes nothing more
func shrinkImages(value any) bool {
	return walkImages(value, false)
}

func walkImages(value any, fromTool bool) bool {
	changed := false
	switch value := value.(type) {
	case []any:
		for _, item := range value {
			changed = walkImages(item, fromTool) || changed
		}
	case map[string]any:
		if isToolOutput(value) {
			fromTool = true
		}

		changed = shrinkInline(value, fromTool)
		for key, field := range value {
			if text, ok := field.(string); ok {
				changed = shrinkDataURL(value, key, text, fromTool) || changed
				continue
			}

			changed = walkImages(field, fromTool) || changed
		}
	}

	return changed
}

// isToolOutput reports whether a block or item carries what a tool returned: Claude's
// tool_result and server tool results, and the Responses API's *_call_output items
func isToolOutput(fields map[string]any) bool {
	kind, _ := fields["type"].(string)
	return strings.HasSuffix(kind, "tool_result") || strings.HasSuffix(kind, "_call_output")
}

// shrinkInline handles an image given as base64 beside its media type: Claude's
// image source and Gemini's inline data
func shrinkInline(fields map[string]any, fromTool bool) bool {
	data, ok := fields["data"].(string)
	if !ok || data == "" {
		return false
	}

	for _, key := range []string{"media_type", "mimeType", "mime_type"} {
		media, _ := fields[key].(string)
		if !strings.HasPrefix(media, "image/") {
			continue
		}

		out, ok := shrinkImage(media, data, fromTool)
		if !ok {
			return false
		}

		fields["data"] = out.data
		fields[key] = out.media
		if out.omitted != nil {
			fields["omitted"] = out.omitted
		}

		return true
	}

	return false
}

// shrinkDataURL handles an image given as a data URL, as OpenAI's input_image and
// image_url parts carry it
func shrinkDataURL(fields map[string]any, key, text string, fromTool bool) bool {
	if !strings.HasPrefix(text, "data:image/") {
		return false
	}

	media, data, ok := strings.Cut(strings.TrimPrefix(text, "data:"), ";base64,")
	if !ok || data == "" {
		return false
	}

	out, ok := shrinkImage(media, data, fromTool)
	if !ok {
		return false
	}

	fields[key] = "data:" + out.media + ";base64," + out.data
	if out.omitted != nil {
		fields["omitted"] = out.omitted
	}

	return true
}

type shrunkImage struct {
	media string
	data  string
	// omitted describes a dropped image, nil for a kept one
	omitted map[string]any
}

// shrinkImage drops an image a tool returned, or re-encodes an attached one as WebP
// when that is smaller. It reports false when the image should stay as it is
func shrinkImage(media, data string, fromTool bool) (shrunkImage, bool) {
	raw, errDecode := base64.StdEncoding.DecodeString(data)
	if fromTool {
		omitted := map[string]any{"media_type": media, "bytes": base64.StdEncoding.DecodedLen(len(data))}
		if errDecode == nil {
			omitted["bytes"] = len(raw)
			if config, _, errConfig := image.DecodeConfig(bytes.NewReader(raw)); errConfig == nil {
				omitted["width"] = config.Width
				omitted["height"] = config.Height
			}
		}

		return shrunkImage{media: media, omitted: omitted}, true
	}

	// WebP is already the target, and a GIF may be animated, which a still WebP would
	// flatten to its first frame
	if errDecode != nil || media == "image/webp" || media == "image/gif" {
		return shrunkImage{}, false
	}

	// the EXIF orientation a phone photo relies on does not survive re-encoding, so a
	// rotated JPEG stays as it is rather than being stored on its side
	if jpegOrientation(raw) > 1 {
		return shrunkImage{}, false
	}

	img, _, errImage := image.Decode(bytes.NewReader(raw))
	if errImage != nil {
		return shrunkImage{}, false
	}

	var buf bytes.Buffer
	if errEncode := webp.Encode(&buf, img, webp.Options{Quality: imageQuality, Method: 4}); errEncode != nil || buf.Len() >= len(raw) {
		return shrunkImage{}, false
	}

	return shrunkImage{media: "image/webp", data: base64.StdEncoding.EncodeToString(buf.Bytes())}, true
}

// jpegOrientation reads the EXIF orientation tag of a JPEG: 1 is upright, 2 to 8 flip
// or rotate it, and 0 means the data is not a JPEG or has no tag
func jpegOrientation(raw []byte) int {
	if len(raw) < 4 || raw[0] != 0xFF || raw[1] != 0xD8 {
		return 0
	}

	for at := 2; at+4 <= len(raw) && raw[at] == 0xFF; {
		marker := raw[at+1]
		size := int(binary.BigEndian.Uint16(raw[at+2:]))
		// the image data starts at SOS, after every metadata segment
		if marker == 0xDA || size < 2 || at+2+size > len(raw) {
			return 0
		}

		segment := raw[at+4 : at+2+size]
		if marker == 0xE1 && bytes.HasPrefix(segment, []byte("Exif\x00\x00")) {
			return tiffOrientation(segment[6:])
		}

		at += 2 + size
	}

	return 0
}

// tiffOrientation finds tag 0x0112 in the first IFD of an EXIF TIFF block
func tiffOrientation(tiff []byte) int {
	if len(tiff) < 8 {
		return 0
	}

	var order binary.ByteOrder
	switch string(tiff[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return 0
	}

	ifd := int(order.Uint32(tiff[4:]))
	if ifd+2 > len(tiff) {
		return 0
	}

	count := int(order.Uint16(tiff[ifd:]))
	for i := range count {
		entry := ifd + 2 + i*12
		if entry+12 > len(tiff) {
			return 0
		}

		if order.Uint16(tiff[entry:]) == 0x0112 {
			return int(order.Uint16(tiff[entry+8:]))
		}
	}

	return 0
}

// ShrinkStoredMessage applies the capture's image rules to a message blob stored
// before them, for a migration that rewrites those blobs under their existing hashes.
// It returns the new content and whether any image changed
func ShrinkStoredMessage(content []byte) ([]byte, bool, error) {
	if !hasImages(content) {
		return content, false, nil
	}

	var value any
	if errDecode := decodeJSON(content, &value); errDecode != nil {
		return nil, false, errDecode
	}

	if !shrinkImages(value) {
		return content, false, nil
	}

	return canonical(value), true, nil
}
