package chathistory

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"math/rand/v2"
	"strings"
	"testing"
)

// screenshot is a PNG of gradients with a little noise, like a photo, which PNG
// stores poorly and WebP well
func screenshot(t *testing.T) string {
	t.Helper()
	noise := rand.New(rand.NewPCG(1, 2))
	img := image.NewRGBA(image.Rect(0, 0, 320, 200))
	for y := range 200 {
		for x := range 320 {
			jitter := uint8(noise.IntN(12))
			img.Set(x, y, color.RGBA{uint8(x*255/320) + jitter, uint8(y*255/200) + jitter, 128 + jitter, 255})
		}
	}

	var buf bytes.Buffer
	if errEncode := png.Encode(&buf, img); errEncode != nil {
		t.Fatal(errEncode)
	}

	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// An attached image is kept as WebP and a tool's screenshot is dropped, in both the
// Claude and Responses layouts, while each message keeps the hash of what the client
// sent, so sessions recorded before images were shrunk continue the same chain
func TestImagesAreShrunkUnderTheOriginalHash(t *testing.T) {
	shot := screenshot(t)
	bodies := map[string]string{
		"claude": `{"messages":[
			{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + shot + `"}},{"type":"text","text":"look"}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"u","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + shot + `"}}]}]}]}`,
		"openai-response": `{"input":[
			{"type":"message","role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,` + shot + `"}]},
			{"type":"custom_tool_call_output","call_id":"c","output":[{"type":"input_image","image_url":"data:image/png;base64,` + shot + `"}]}]}`,
	}

	for format, body := range bodies {
		conv, errSplit := splitConversation([]byte(body))
		if errSplit != nil {
			t.Fatal(errSplit)
		}

		recorder := &Recorder{blobs: newSeenSet(10), nodes: newSeenSet(10)}
		rows := recorder.conversationRows(requestRow{Format: format}, conv, "s", "r", "2026-11-06 00:00:00.000", "", 0)
		blobs := rows["blobs"]
		if len(blobs) != 2 {
			t.Fatalf("%s: %d blobs, want 2", format, len(blobs))
		}

		attached, tool := blobs[0].(blobRow), blobs[1].(blobRow)
		for i, blob := range []blobRow{attached, tool} {
			if blob.Hash != hashBytes(conv.messages[i].content) || !strings.Contains(string(conv.messages[i].content), shot) {
				t.Errorf("%s: message %d hash %s does not cover the image the client sent", format, i, blob.Hash)
			}
		}

		if strings.Contains(attached.Content, shot) || !strings.Contains(attached.Content, "image/webp") || len(attached.Content) >= len(shot) {
			t.Errorf("%s: attached image stored as %.200s, want a smaller WebP", format, attached.Content)
		}

		// base64 WebP starts with RIFF, "UklGR"
		webp := attached.Content[strings.Index(attached.Content, "UklGR"):]
		raw, errDecode := base64.StdEncoding.DecodeString(webp[:strings.IndexByte(webp, '"')])
		if errDecode != nil {
			t.Fatal(errDecode)
		}

		if config, kind, errConfig := image.DecodeConfig(bytes.NewReader(raw)); errConfig != nil || kind != "webp" || config.Width != 320 {
			t.Errorf("%s: kept image decodes as %s %dpx (%v), want a 320px WebP", format, kind, config.Width, errConfig)
		}

		if strings.Contains(tool.Content, shot) || !strings.Contains(tool.Content, `"omitted":{"bytes":`) || !strings.Contains(tool.Content, `"height":200`) {
			t.Errorf("%s: tool screenshot stored as %s, want it dropped with its size", format, tool.Content)
		}

		// a decoded message that was already shrunk stays as it is
		var again any
		if errJSON := json.Unmarshal([]byte(tool.Content), &again); errJSON != nil || shrinkImages(again) {
			t.Errorf("%s: shrinking a stored message changed it again (%v)", format, errJSON)
		}
	}
}

// A JPEG that relies on its EXIF orientation is not re-encoded, since WebP would
// store it on its side
func TestJPEGOrientation(t *testing.T) {
	// SOI, then an APP1 Exif segment whose big-endian TIFF block has one IFD entry:
	// orientation 6, rotate 90°
	tiff := []byte{'M', 'M', 0, 42, 0, 0, 0, 8, 0, 1, 0x01, 0x12, 0, 3, 0, 0, 0, 1, 0, 6, 0, 0, 0, 0, 0, 0, 0, 0}
	segment := append([]byte("Exif\x00\x00"), tiff...)
	jpeg := append([]byte{0xFF, 0xD8, 0xFF, 0xE1, 0, byte(len(segment) + 2)}, segment...)
	jpeg = append(jpeg, 0xFF, 0xDA, 0, 2)
	if got := jpegOrientation(jpeg); got != 6 {
		t.Fatalf("orientation %d, want 6", got)
	}

	if _, ok := shrinkImage("image/jpeg", base64.StdEncoding.EncodeToString(jpeg), false); ok {
		t.Fatal("a rotated JPEG was re-encoded")
	}
}
