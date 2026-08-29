package protocol

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	ImagePostMediaType = "MEDIA_POST_TYPE_IMAGE"
	VideoPostMediaType = "MEDIA_POST_TYPE_VIDEO"
)

func BuildResetMessage() map[string]any {
	return map[string]any{
		"type":      "conversation.item.create",
		"timestamp": time.Now().UnixMilli(),
		"item":      map[string]any{"type": "message", "content": []any{map[string]any{"type": "reset"}}},
	}
}

func BuildImagineRequest(requestID, prompt, aspectRatio string, enableNSFW, enablePro bool) map[string]any {
	return map[string]any{
		"type":      "conversation.item.create",
		"timestamp": time.Now().UnixMilli(),
		"item": map[string]any{
			"type": "message",
			"content": []any{map[string]any{
				"requestId": requestID,
				"text":      strings.TrimSpace(prompt),
				"type":      "input_text",
				"properties": map[string]any{
					"section_count": 0, "is_kids_mode": false, "enable_nsfw": enableNSFW,
					"skip_upsampler": false, "enable_side_by_side": true, "is_initial": false,
					"aspect_ratio": aspectRatio, "enable_pro": enablePro,
				},
			}},
		},
	}
}

var mediaFileIDPattern = regexp.MustCompile(`^[0-9a-fA-F-]{16,64}$`)

func AspectRatio(size string) (string, bool) {
	switch strings.TrimSpace(strings.ToLower(size)) {
	case "1280x720", "16:9":
		return "16:9", true
	case "720x1280", "9:16":
		return "9:16", true
	case "1792x1024", "3:2":
		return "3:2", true
	case "1024x1792", "2:3":
		return "2:3", true
	case "1024x1024", "1:1":
		return "1:1", true
	default:
		return "", false
	}
}

func BuildImageChatPayload(prompt, mode string, count int) map[string]any {
	payload := BuildGrokPayload("Drawing: "+strings.TrimSpace(prompt), mode, nil, nil, nil)
	payload["imageGenerationCount"] = count
	return payload
}

func BuildVideoPayload(prompt, parentPostID, aspectRatio, resolution string, seconds int, preset string, imageRefs []string) map[string]any {
	config := map[string]any{
		"parentPostId":   parentPostID,
		"aspectRatio":    aspectRatio,
		"videoLength":    seconds,
		"resolutionName": resolution,
	}
	if len(imageRefs) > 0 {
		config["isVideoEdit"] = false
		config["isReferenceToVideo"] = true
		config["imageReferences"] = imageRefs
	}
	mode := "--mode=custom"
	switch strings.ToLower(strings.TrimSpace(preset)) {
	case "fun":
		mode = "--mode=extremely-crazy"
	case "normal":
		mode = "--mode=normal"
	case "spicy":
		mode = "--mode=extremely-spicy-or-crazy"
	}
	return map[string]any{
		"temporary":        true,
		"modelName":        "imagine-video-gen",
		"message":          strings.TrimSpace(prompt) + " " + mode,
		"enableSideBySide": true,
		"responseMetadata": map[string]any{
			"experiments":         []any{},
			"modelConfigOverride": map[string]any{"modelMap": map[string]any{"videoGenModelConfig": config}},
		},
	}
}

func BuildVideoExtendPayload(prompt, parentPostID, extendPostID, aspectRatio, resolution string, seconds int, preset string, startTime float64) map[string]any {
	mode := strings.ToLower(strings.TrimSpace(preset))
	if mode == "" {
		mode = "custom"
	}
	return map[string]any{
		"temporary":        true,
		"modelName":        "imagine-video-gen",
		"message":          strings.TrimSpace(prompt) + " --mode=" + mode,
		"enableSideBySide": true,
		"responseMetadata": map[string]any{
			"experiments": []any{},
			"modelConfigOverride": map[string]any{"modelMap": map[string]any{
				"videoGenModelConfig": map[string]any{
					"isVideoExtension":        true,
					"videoExtensionStartTime": startTime,
					"extendPostId":            extendPostID,
					"stitchWithExtendPostId":  true,
					"originalPrompt":          strings.TrimSpace(prompt),
					"originalPostId":          parentPostID,
					"originalRefType":         "VIDEO",
					"mode":                    mode,
					"aspectRatio":             aspectRatio,
					"videoLength":             seconds,
					"resolutionName":          resolution,
					"parentPostId":            parentPostID,
					"isVideoEdit":             false,
				},
			}},
		},
	}
}

func VideoSegmentLengths(seconds int) ([]int, bool) {
	switch seconds {
	case 6:
		return []int{6}, true
	case 10:
		return []int{10}, true
	case 12:
		return []int{6, 6}, true
	case 16:
		return []int{10, 6}, true
	case 20:
		return []int{10, 10}, true
	default:
		return nil, false
	}
}

func BuildMediaPostPayload(mediaType, mediaURL, prompt string) map[string]any {
	payload := map[string]any{"mediaType": mediaType}
	if strings.TrimSpace(mediaURL) != "" {
		payload["mediaUrl"] = mediaURL
	}
	if strings.TrimSpace(prompt) != "" {
		payload["prompt"] = prompt
	}
	return payload
}

func BuildImageEditPayload(prompt string, refs []string, parentPostID string) map[string]any {
	return map[string]any{
		"temporary":                 true,
		"modelName":                 "imagine-image-edit",
		"message":                   strings.TrimSpace(prompt),
		"enableImageGeneration":     true,
		"returnImageBytes":          false,
		"returnRawGrokInXaiRequest": false,
		"enableImageStreaming":      true,
		"imageGenerationCount":      2,
		"forceConcise":              false,
		"enableSideBySide":          true,
		"sendFinalMetadata":         true,
		"isReasoning":               false,
		"disableTextFollowUps":      true,
		"responseMetadata": map[string]any{
			"modelConfigOverride": map[string]any{
				"modelMap": map[string]any{
					"imageEditModel": "imagine",
					"imageEditModelConfig": map[string]any{
						"imageReferences": refs,
						"parentPostId":    parentPostID,
					},
				},
			},
		},
		"disableMemory":   true,
		"forceSideBySide": false,
	}
}

type MediaEvent struct {
	Kind      string
	URL       string
	Blob      string
	AssetID   string
	ImageID   string
	Progress  int
	Index     int
	PostID    string
	Moderated bool
}

func ParseMediaData(raw []byte) []MediaEvent {
	var envelope map[string]any
	if json.Unmarshal(raw, &envelope) != nil {
		return nil
	}
	return parseMediaObject(envelope)
}

func parseMediaObject(object map[string]any) []MediaEvent {
	result, _ := object["result"].(map[string]any)
	response, _ := result["response"].(map[string]any)
	if response == nil {
		response = object
	}
	events := make([]MediaEvent, 0, 2)
	for _, key := range []string{"streamingImageGenerationResponse", "streamingVideoGenerationResponse"} {
		if stream, ok := response[key].(map[string]any); ok {
			events = append(events, MediaEvent{
				Kind:      map[string]string{"streamingImageGenerationResponse": "image", "streamingVideoGenerationResponse": "video"}[key],
				URL:       stringField(stream, "imageUrl", "videoUrl", "url"),
				AssetID:   stringField(stream, "assetId"),
				ImageID:   stringField(stream, "imageId", "videoId", "videoPostId"),
				PostID:    stringField(stream, "videoPostId", "videoId"),
				Progress:  intField(stream, "progress"),
				Index:     intField(stream, "imageIndex"),
				Moderated: boolField(stream, "moderated"),
			})
		}
	}
	if modelResponse, ok := response["modelResponse"].(map[string]any); ok {
		if values, ok := modelResponse["generatedImageUrls"].([]any); ok {
			for index, value := range values {
				if text, ok := value.(string); ok && text != "" {
					events = append(events, MediaEvent{Kind: "image", URL: text, Progress: 100, Index: index})
				}
			}
		}
		if values, ok := modelResponse["fileAttachments"].([]any); ok {
			for index, value := range values {
				if text, ok := value.(string); ok && text != "" {
					events = append(events, MediaEvent{Kind: "asset", AssetID: text, Progress: 100, Index: index})
				}
			}
		}
	}
	if post, ok := object["post"].(map[string]any); ok {
		events = append(events, MediaEvent{Kind: "post", PostID: stringField(post, "id")})
	}
	if attachment, ok := response["cardAttachment"].(map[string]any); ok {
		if jsonData, ok := attachment["jsonData"].(string); ok {
			var card map[string]any
			if json.Unmarshal([]byte(jsonData), &card) == nil {
				if chunk, ok := card["image_chunk"].(map[string]any); ok {
					urlValue := stringField(chunk, "imageUrl", "url")
					if urlValue != "" {
						events = append(events, MediaEvent{Kind: "image", URL: urlValue, Progress: intField(chunk, "progress"), Moderated: boolField(chunk, "moderated")})
					}
				}
			}
		}
	}
	return events
}

func ParseSSELine(line string) (string, []MediaEvent) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "event:") {
		return "", nil
	}
	if strings.HasPrefix(line, "data:") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	}
	if line == "[DONE]" {
		return "done", nil
	}
	if !strings.HasPrefix(line, "{") {
		return "", nil
	}
	return "data", ParseMediaData([]byte(line))
}

func ParseDataURI(value string) (filename, mime, encoded string, err error) {
	if !strings.HasPrefix(value, "data:") {
		return "", "", "", fmt.Errorf("file input must be a URL or data URI")
	}
	header, body, ok := strings.Cut(value, ",")
	if !ok || !strings.Contains(header, ";base64") {
		return "", "", "", fmt.Errorf("malformed base64 data URI")
	}
	mime = strings.TrimPrefix(strings.SplitN(strings.TrimPrefix(header, "data:"), ";", 2)[0], " ")
	if mime == "" {
		mime = "application/octet-stream"
	}
	encoded = strings.Join(strings.Fields(body), "")
	if _, err := base64.StdEncoding.DecodeString(encoded); err != nil {
		return "", "", "", fmt.Errorf("invalid base64 data URI: %w", err)
	}
	ext := filepath.Ext("." + strings.TrimPrefix(strings.SplitN(mime, "/", 2)[len(strings.SplitN(mime, "/", 2))-1], "."))
	return "file" + ext, mime, encoded, nil
}

func ResolveDownloadURL(value string) (string, string) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err == nil && parsed.Scheme != "" && parsed.Host != "" {
		return parsed.String(), parsed.Scheme + "://" + parsed.Host
	}
	path := "/" + strings.TrimPrefix(strings.TrimSpace(value), "/")
	return "https://assets.grok.com" + path, "https://assets.grok.com"
}

func ResolveAssetReference(fileID, fileURI, userID string) string {
	if fileURI != "" {
		value, _ := ResolveDownloadURL(fileURI)
		return value
	}
	if fileID != "" && userID != "" {
		return "https://assets.grok.com/users/" + url.PathEscape(userID) + "/" + url.PathEscape(fileID) + "/content"
	}
	return ""
}

func ValidMediaFileID(value string) bool { return mediaFileIDPattern.MatchString(value) }

func stringField(object map[string]any, keys ...string) string {
	for _, key := range keys {
		if text, ok := object[key].(string); ok && strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
	}
	return ""
}

func intField(object map[string]any, key string) int {
	switch value := object[key].(type) {
	case float64:
		return int(value)
	case json.Number:
		parsed, _ := value.Int64()
		return int(parsed)
	case int:
		return value
	case string:
		var parsed int
		_, _ = fmt.Sscanf(value, "%d", &parsed)
		return parsed
	default:
		return 0
	}
}

func boolField(object map[string]any, key string) bool { value, _ := object[key].(bool); return value }
