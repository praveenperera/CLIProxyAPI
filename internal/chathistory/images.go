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

// limits on what is decoded for re-encoding; anything larger is stored as sent
const (
	maxWebPSide    = 16383
	maxImagePixels = 40_000_000
)

// hasImages is a cheap check for content that may hold an inline image, so most
// messages skip decoding
func hasImages(content []byte) bool {
	return bytes.Contains(content, []byte(`"image/`)) || bytes.Contains(content, []byte(`data:image/`))
}

// shrinkImages rewrites the inline images of a decoded message in place and reports
// whether it changed any. It leaves WebP, dropped and undecodable images alone, so
// running it twice changes nothing more
func shrinkImages(value any) bool {
	return walkImages(value, "", false)
}

// walkImages looks for images only where each provider's layout puts them, so text or
// a tool call's arguments that merely look like an image stay as they are. key is the
// field that held value
func walkImages(value any, key string, fromTool bool) bool {
	changed := false
	switch value := value.(type) {
	case []any:
		for _, item := range value {
			changed = walkImages(item, key, fromTool) || changed
		}
	case map[string]any:
		kind, _ := value["type"].(string)
		switch {
		// a tool call holds the model's own arguments: Claude tool_use, Responses
		// *_call items and Gemini functionCall
		case strings.HasSuffix(kind, "tool_use") || strings.HasSuffix(kind, "_call") || key == "functionCall" || key == "function_call":
			return false
		// what a tool returned: Claude tool_result and server tool results, Responses
		// *_call_output items and Gemini functionResponse
		case strings.HasSuffix(kind, "tool_result") || strings.HasSuffix(kind, "_call_output") || key == "functionResponse" || key == "function_response":
			fromTool = true
		}

		switch {
		case key == "source" && kind == "base64":
			// Claude's image source
			changed = shrinkInline(value, "media_type", fromTool)
		case key == "inlineData" || key == "inline_data":
			// Gemini's inline data, in either JSON casing
			changed = shrinkInline(value, "mimeType", fromTool) || shrinkInline(value, "mime_type", fromTool)
		case kind == "input_image":
			// Responses input_image, whose image_url is the data URL itself
			changed = shrinkDataURL(value, "image_url", fromTool)
		case key == "image_url":
			// Chat Completions image_url part, whose url sits one level down
			changed = shrinkDataURL(value, "url", fromTool)
		}

		for field, inner := range value {
			if _, ok := inner.(string); !ok {
				changed = walkImages(inner, field, fromTool) || changed
			}
		}
	}

	return changed
}

// shrinkInline handles an image given as base64 in data beside its media type
func shrinkInline(fields map[string]any, mediaKey string, fromTool bool) bool {
	data, _ := fields["data"].(string)
	media, _ := fields[mediaKey].(string)
	if data == "" || !strings.HasPrefix(media, "image/") {
		return false
	}

	out, ok := shrinkImage(media, data, fromTool)
	if !ok {
		return false
	}

	fields["data"] = out.data
	fields[mediaKey] = out.media
	if out.omitted != nil {
		fields["omitted"] = out.omitted
	}

	return true
}

// shrinkDataURL handles an image given as a base64 data URL in fields[key]
func shrinkDataURL(fields map[string]any, key string, fromTool bool) bool {
	text, _ := fields[key].(string)
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
	if errDecode != nil || media == "image/webp" || media == "image/gif" || animatedPNG(raw) {
		return shrunkImage{}, false
	}

	// the EXIF orientation a phone photo relies on does not survive re-encoding, so a
	// rotated JPEG stays as it is rather than being stored on its side
	if jpegOrientation(raw) > 1 {
		return shrunkImage{}, false
	}

	// a small file can declare a huge canvas; check the header before decoding so one
	// pasted image cannot exhaust memory, and stay within WebP's 16383px limit
	config, _, errConfig := image.DecodeConfig(bytes.NewReader(raw))
	if errConfig != nil || config.Width > maxWebPSide || config.Height > maxWebPSide || config.Width*config.Height > maxImagePixels {
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

// animatedPNG reports whether a PNG is an APNG, which declares its frames in an acTL
// chunk before the image data; Go decodes only the first frame
func animatedPNG(raw []byte) bool {
	if !bytes.HasPrefix(raw, []byte("\x89PNG\r\n\x1a\n")) {
		return false
	}

	for at := 8; at+8 <= len(raw); {
		size := int(binary.BigEndian.Uint32(raw[at:]))
		switch string(raw[at+4 : at+8]) {
		case "acTL":
			return true
		case "IDAT":
			return false
		}

		// length, type, data and CRC
		if size < 0 || size > len(raw) {
			return false
		}

		at += 12 + size
	}

	return false
}

// jpegOrientation reads the EXIF orientation tag of a JPEG: 1 is upright, 2 to 8 flip
// or rotate it, and 0 means the data is not a JPEG or has no tag
func jpegOrientation(raw []byte) int {
	if len(raw) < 4 || raw[0] != 0xFF || raw[1] != 0xD8 {
		return 0
	}

	for at := 2; at+4 <= len(raw) && raw[at] == 0xFF; {
		// any number of 0xFF fill bytes may come before a marker
		if raw[at+1] == 0xFF {
			at++
			continue
		}

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
