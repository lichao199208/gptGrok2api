package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/protocol"
)

const (
	EditableFileModel          = "gpt-5-5-thinking"
	editableFileThinkingEffort = "extended"
	editableFilePollInterval   = 5 * time.Second
)

const editablePPTPrompt = `我需要你根据用户的需求，来制作一个可以编辑的PPT，你可以使用Agent来做，你不要再继续询问用户问题，内容风格、版式、配色、内容结构和页面信息你可以自行补充并直接执行。整体的流程如下：
1. 用生图的方式，帮我生成一个精美的产品介绍ppt，5-6个页面
2. 帮我把以上涉及到的所有图像和形状素材拆分成单独png，每个素材单独一张图片，不要有遗漏，让我可以直接在ppt里拼接素材还原，不要文字
3. 利用以上所有图片和形状素材，帮我还原你第一次生成的展示ppt，我需要是可编辑的ppt格式，主要部分需要你单独还原插入，文字需要可以编辑
最后只需要给我生成一个PPT文件，以及生成中遇到的各种素材压缩包zip文件就行。`

const editablePSDPrompt = "帮我生成这个图像，把这张海报分成若干图像，包括背景图，每个元素不要改位置，这样子我可以直接在平时里无需拖动，底色为白色，不要伪透明底。再帮我将以上拆分的图像拼合成一个psd文件，去除白色底，不要改变每个图层的相应位置，保留每个元素所在图层的相应位置，保留每个元素的图层，最后只需要给我输出psd文件，以及每个图层的zip文件"

var (
	editableAssetPointer = regexp.MustCompile(`(?:file-service|sediment)://([A-Za-z0-9_-]+)`)
	editableFileID       = regexp.MustCompile(`\b(file[-_](?:[A-Za-z0-9_-]+))\b`)
	editableSandboxPath  = regexp.MustCompile(`(?i)(/mnt/data/[^\s"'\)\]]+\.(?:pptx?|psd|zip))`)
)

type EditableExportFile struct {
	Name string
	MIME string
	Data []byte
}

type EditableExportResult struct {
	ConversationID string
	Primary        EditableExportFile
	Archive        EditableExportFile
}

type editableArtifact struct {
	AttachmentID string
	FileID       string
	Name         string
	MIME         string
	SandboxPath  string
	MessageID    string
}

func (o *OpenAIImage) ExportEditable(ctx context.Context, account accounts.Account, kind, userPrompt string, inputs []OpenAIImageInput) (EditableExportResult, error) {
	kind = strings.ToLower(strings.TrimSpace(kind))
	if kind != "ppt" && kind != "psd" {
		return EditableExportResult{}, fmt.Errorf("editable export kind must be ppt or psd")
	}
	if kind == "psd" && len(inputs) == 0 {
		return EditableExportResult{}, fmt.Errorf("PSD export requires at least one image")
	}
	prompt := editablePPTPrompt
	if kind == "psd" {
		prompt = editablePSDPrompt
	}
	if extra := strings.TrimSpace(userPrompt); extra != "" {
		prompt += "\n\n以下是用户补充需求，请直接结合执行：\n" + extra
	}
	references, err := o.uploadInputs(ctx, account, inputs)
	if err != nil {
		return EditableExportResult{}, err
	}
	scripts, build, err := o.bootstrap(ctx, account)
	if err != nil {
		return EditableExportResult{}, err
	}
	requirements, err := o.chatRequirements(ctx, account, scripts, build)
	if err != nil {
		return EditableExportResult{}, err
	}
	conduit, err := o.prepareEditable(ctx, account, requirements, prompt, references)
	if err != nil {
		return EditableExportResult{}, err
	}
	conversationID, err := o.startEditable(ctx, account, requirements, conduit, prompt, references)
	if err != nil {
		return EditableExportResult{}, err
	}
	artifacts, err := o.pollEditableArtifacts(ctx, account, conversationID, kind)
	if err != nil {
		return EditableExportResult{}, err
	}
	primaryArtifact, archiveArtifact := pickEditableArtifacts(artifacts, kind)
	if primaryArtifact == nil || archiveArtifact == nil {
		return EditableExportResult{}, fmt.Errorf("editable export completed without %s and zip artifacts", kind)
	}
	primary, err := o.downloadEditableArtifact(ctx, account, conversationID, *primaryArtifact, kind)
	if err != nil {
		return EditableExportResult{}, err
	}
	archive, err := o.downloadEditableArtifact(ctx, account, conversationID, *archiveArtifact, "zip")
	if err != nil {
		return EditableExportResult{}, err
	}
	return EditableExportResult{ConversationID: conversationID, Primary: primary, Archive: archive}, nil
}

func (o *OpenAIImage) prepareEditable(ctx context.Context, account accounts.Account, requirements openAIRequirements, prompt string, references []openAIImageReference) (string, error) {
	mimeTypes := make([]any, 0, len(references))
	for _, ref := range references {
		mimeTypes = append(mimeTypes, ref.MIME)
	}
	payload := map[string]any{
		"action": "next", "fork_from_shared_post": false, "parent_message_id": "client-created-root",
		"model": EditableFileModel, "client_prepare_state": "success", "timezone_offset_min": -480,
		"timezone": "Asia/Shanghai", "conversation_mode": map[string]any{"kind": "primary_assistant"},
		"system_hints": []any{}, "partial_query": map[string]any{"id": firstUUID(), "author": map[string]any{"role": "user"}, "content": map[string]any{"content_type": "text", "parts": []any{prompt}}},
		"supports_buffering": true, "supported_encodings": []any{"v1"}, "client_contextual_info": map[string]any{"app_name": "chatgpt.com"},
		"thinking_effort": editableFileThinkingEffort,
	}
	if len(mimeTypes) > 0 {
		payload["attachment_mime_types"] = mimeTypes
	}
	headers := o.requirementHeaders(requirements)
	headers["X-Conduit-Token"] = "no-token"
	response, err := o.do(ctx, http.MethodPost, "/backend-api/f/conversation/prepare", account, payload, headers, false)
	if err != nil {
		return "", err
	}
	var value map[string]any
	if err := decodeOpenAIJSON(response, &value, "editable conversation prepare"); err != nil {
		return "", err
	}
	conduit := stringValue(value["conduit_token"])
	if conduit == "" {
		return "", fmt.Errorf("editable conversation prepare returned no conduit token")
	}
	return conduit, nil
}

func (o *OpenAIImage) startEditable(ctx context.Context, account accounts.Account, requirements openAIRequirements, conduit, prompt string, references []openAIImageReference) (string, error) {
	parts := make([]any, 0, len(references)+1)
	attachments := make([]any, 0, len(references))
	for _, ref := range references {
		parts = append(parts, map[string]any{"content_type": "image_asset_pointer", "asset_pointer": "sediment://" + ref.FileID, "size_bytes": ref.FileSize, "width": ref.Width, "height": ref.Height})
		attachments = append(attachments, map[string]any{"id": ref.FileID, "size": ref.FileSize, "name": ref.FileName, "mime_type": ref.MIME, "width": ref.Width, "height": ref.Height, "source": "library", "is_big_paste": false})
	}
	parts = append(parts, prompt)
	contentType := "text"
	if len(references) > 0 {
		contentType = "multimodal_text"
	}
	metadata := map[string]any{"developer_mode_connector_ids": []any{}, "selected_sources": []any{}, "selected_github_repos": []any{}, "selected_all_github_repos": false, "serialization_metadata": map[string]any{"custom_symbol_offsets": []any{}}}
	if len(attachments) > 0 {
		metadata["attachments"] = attachments
	}
	payload := map[string]any{
		"action": "next", "messages": []any{map[string]any{"id": firstUUID(), "author": map[string]any{"role": "user"}, "create_time": float64(time.Now().UnixNano()) / 1e9, "content": map[string]any{"content_type": contentType, "parts": parts}, "metadata": metadata}},
		"parent_message_id": "client-created-root", "model": EditableFileModel, "client_prepare_state": "sent",
		"timezone_offset_min": -480, "timezone": "Asia/Shanghai", "conversation_mode": map[string]any{"kind": "primary_assistant"},
		"enable_message_followups": true, "system_hints": []any{}, "supports_buffering": true, "supported_encodings": []any{"v1"},
		"client_contextual_info":               map[string]any{"is_dark_mode": false, "time_since_loaded": 401, "page_height": 1138, "page_width": 803, "pixel_ratio": 2, "screen_height": 1440, "screen_width": 2560, "app_name": "chatgpt.com"},
		"paragen_cot_summary_display_override": "allow", "force_parallel_switch": "auto", "thinking_effort": editableFileThinkingEffort,
	}
	headers := o.requirementHeaders(requirements)
	headers["X-Conduit-Token"] = conduit
	headers["Accept"] = "text/event-stream"
	response, err := o.do(ctx, http.MethodPost, "/backend-api/f/conversation", account, payload, headers, true)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	conversationID := ""
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 4096), 8<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var value any
		if json.Unmarshal([]byte(data), &value) == nil && conversationID == "" {
			conversationID = nestedString(value, "conversation_id")
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	if conversationID == "" {
		return "", fmt.Errorf("editable conversation stream returned no conversation id")
	}
	return conversationID, nil
}

func (o *OpenAIImage) pollEditableArtifacts(ctx context.Context, account accounts.Account, conversationID, kind string) ([]editableArtifact, error) {
	timeout := o.RequestTimeout
	if timeout < 20*time.Minute {
		timeout = 20 * time.Minute
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		path := "/backend-api/conversation/" + url.PathEscape(conversationID)
		headers := map[string]string{"Accept": "*/*", "Referer": o.BaseURL + "/c/" + conversationID, "X-OpenAI-Target-Route": "/backend-api/conversation/{conversation_id}"}
		response, err := o.do(ctx, http.MethodGet, path, account, nil, headers, false)
		if err == nil {
			var value any
			if decodeErr := decodeOpenAIJSON(response, &value, "editable conversation"); decodeErr == nil {
				artifacts := collectEditableArtifacts(value)
				primary, archive := pickEditableArtifacts(artifacts, kind)
				if primary != nil && archive != nil {
					return artifacts, nil
				}
			}
		} else if !isRetryableOpenAIError(err) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(editableFilePollInterval):
		}
	}
	return nil, &protocol.UpstreamError{Status: http.StatusGatewayTimeout, Message: "editable file result polling timed out", Body: "editable file result polling timed out"}
}

func collectEditableArtifacts(value any) []editableArtifact {
	items := []editableArtifact{}
	seen := map[string]int{}
	var walk func(any, string)
	walk = func(current any, messageID string) {
		switch typed := current.(type) {
		case []any:
			for _, child := range typed {
				walk(child, messageID)
			}
		case map[string]any:
			if author, ok := typed["author"].(map[string]any); ok && stringValue(author["role"]) != "" {
				messageID = firstNonEmptyProvider(stringValue(typed["id"]), messageID)
			}
			artifact := editableArtifact{MessageID: messageID, AttachmentID: stringValue(typed["attachment_id"]), FileID: stringValue(typed["file_id"]), Name: firstNonEmptyProvider(stringValue(typed["name"]), stringValue(typed["file_name"]), stringValue(typed["filename"]), stringValue(typed["title"])), MIME: cleanMIME(firstNonEmptyProvider(stringValue(typed["mime_type"]), stringValue(typed["mimeType"])))}
			serialized, _ := json.Marshal(typed)
			text := string(serialized)
			if match := editableAssetPointer.FindStringSubmatch(text); len(match) == 2 {
				artifact.AttachmentID = firstNonEmptyProvider(artifact.AttachmentID, match[1])
				artifact.FileID = firstNonEmptyProvider(artifact.FileID, match[1])
			}
			if match := editableFileID.FindStringSubmatch(text); len(match) == 2 {
				artifact.FileID = firstNonEmptyProvider(artifact.FileID, match[1])
			}
			if match := editableSandboxPath.FindStringSubmatch(text); len(match) == 2 {
				artifact.SandboxPath = match[1]
			}
			if artifact.AttachmentID != "" || artifact.FileID != "" || artifact.SandboxPath != "" {
				key := firstNonEmptyProvider(artifact.AttachmentID, artifact.FileID, artifact.SandboxPath)
				if index, ok := seen[key]; ok {
					items[index] = mergeEditableArtifact(items[index], artifact)
				} else {
					seen[key] = len(items)
					items = append(items, artifact)
				}
			}
			for _, child := range typed {
				walk(child, messageID)
			}
		}
	}
	walk(value, "")
	return items
}

func mergeEditableArtifact(current, latest editableArtifact) editableArtifact {
	current.AttachmentID = firstNonEmptyProvider(latest.AttachmentID, current.AttachmentID)
	current.FileID = firstNonEmptyProvider(latest.FileID, current.FileID)
	current.Name = firstNonEmptyProvider(latest.Name, current.Name)
	current.MIME = firstNonEmptyProvider(latest.MIME, current.MIME)
	current.SandboxPath = firstNonEmptyProvider(latest.SandboxPath, current.SandboxPath)
	current.MessageID = firstNonEmptyProvider(latest.MessageID, current.MessageID)
	return current
}

func pickEditableArtifacts(items []editableArtifact, kind string) (*editableArtifact, *editableArtifact) {
	var primary, archive *editableArtifact
	for index := range items {
		item := &items[index]
		text := strings.ToLower(item.Name + " " + item.SandboxPath + " " + item.MIME)
		if strings.Contains(text, ".zip") || strings.Contains(text, "application/zip") || strings.Contains(text, "x-zip") {
			copy := *item
			archive = &copy
		}
		if kind == "ppt" && (strings.Contains(text, ".ppt") || strings.Contains(text, "presentationml") || strings.Contains(text, "ms-powerpoint")) {
			copy := *item
			primary = &copy
		}
		if kind == "psd" && (strings.Contains(text, ".psd") || strings.Contains(text, "photoshop")) {
			copy := *item
			primary = &copy
		}
	}
	return primary, archive
}

func (o *OpenAIImage) downloadEditableArtifact(ctx context.Context, account accounts.Account, conversationID string, artifact editableArtifact, kind string) (EditableExportFile, error) {
	type attempt struct {
		Path  string
		Query url.Values
		Route string
	}
	attempts := []attempt{}
	if artifact.SandboxPath != "" && artifact.MessageID != "" {
		attempts = append(attempts, attempt{Path: "/backend-api/conversation/" + url.PathEscape(conversationID) + "/interpreter/download", Query: url.Values{"message_id": {artifact.MessageID}, "sandbox_path": {artifact.SandboxPath}}, Route: "/backend-api/conversation/{conversation_id}/interpreter/download"})
	}
	ids := uniqueStrings([]string{artifact.AttachmentID, artifact.FileID})
	for _, id := range ids {
		attempts = append(attempts, attempt{Path: "/backend-api/conversation/" + url.PathEscape(conversationID) + "/attachment/" + url.PathEscape(id) + "/download", Route: "/backend-api/conversation/{conversation_id}/attachment/{attachment_id}/download"})
	}
	for _, id := range ids {
		attempts = append(attempts, attempt{Path: "/backend-api/files/download/" + url.PathEscape(id), Query: url.Values{"post_id": {""}, "inline": {"false"}}, Route: "/backend-api/files/download/{file_id}"})
		attempts = append(attempts, attempt{Path: "/backend-api/files/" + url.PathEscape(id) + "/download", Route: "/backend-api/files/download/{file_id}"})
	}
	var lastErr error
	for _, candidate := range attempts {
		path := candidate.Path
		if len(candidate.Query) > 0 {
			path += "?" + candidate.Query.Encode()
		}
		headers := map[string]string{"Accept": "*/*", "Referer": o.BaseURL + "/c/" + conversationID, "X-OpenAI-Target-Route": candidate.Route}
		response, err := o.do(ctx, http.MethodGet, path, account, nil, headers, false)
		if err != nil {
			lastErr = err
			continue
		}
		file, err := o.editableResponseFile(ctx, account, response, artifact, kind)
		if err == nil && len(file.Data) > 0 {
			return file, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("download URL not found")
	}
	return EditableExportFile{}, fmt.Errorf("download editable artifact: %w", lastErr)
}

func (o *OpenAIImage) editableResponseFile(ctx context.Context, account accounts.Account, response *http.Response, artifact editableArtifact, kind string) (EditableExportFile, error) {
	defer response.Body.Close()
	contentType := cleanMIME(response.Header.Get("Content-Type"))
	raw, err := io.ReadAll(io.LimitReader(response.Body, 512<<20))
	if err != nil {
		return EditableExportFile{}, err
	}
	finalURL := ""
	if response.Request != nil && response.Request.URL != nil {
		finalURL = response.Request.URL.String()
	}
	if strings.Contains(contentType, "json") || json.Valid(raw) {
		var value map[string]any
		if json.Unmarshal(raw, &value) == nil {
			downloadURL := firstStringValue(value, "download_url", "url")
			if downloadURL != "" {
				response, err = o.doAbsolute(ctx, http.MethodGet, downloadURL, account, nil, nil, false)
				if err != nil {
					return EditableExportFile{}, err
				}
				defer response.Body.Close()
				contentType = cleanMIME(response.Header.Get("Content-Type"))
				raw, err = io.ReadAll(io.LimitReader(response.Body, 512<<20))
				if err != nil {
					return EditableExportFile{}, err
				}
				if response.Request != nil && response.Request.URL != nil {
					finalURL = response.Request.URL.String()
				}
			}
		}
	}
	if len(raw) == 0 {
		return EditableExportFile{}, fmt.Errorf("editable artifact download returned empty content")
	}
	name := sanitizeEditableName(artifact.Name)
	if name == "" {
		name = sanitizeEditableName(filepath.Base(artifact.SandboxPath))
	}
	if name == "" {
		name = contentDispositionName(response.Header.Get("Content-Disposition"))
	}
	if name == "" {
		if parsed, parseErr := url.Parse(finalURL); parseErr == nil {
			name = sanitizeEditableName(filepath.Base(parsed.Path))
		}
	}
	extension := editableExtension(kind, contentType)
	if name == "" {
		name = "artifact" + extension
	} else if filepath.Ext(name) == "" {
		name += extension
	}
	return EditableExportFile{Name: name, MIME: contentType, Data: raw}, nil
}

func nestedString(value any, key string) string {
	switch typed := value.(type) {
	case map[string]any:
		if result := stringValue(typed[key]); result != "" {
			return result
		}
		for _, child := range typed {
			if result := nestedString(child, key); result != "" {
				return result
			}
		}
	case []any:
		for _, child := range typed {
			if result := nestedString(child, key); result != "" {
				return result
			}
		}
	case string:
		var parsed any
		if json.Unmarshal([]byte(typed), &parsed) == nil {
			return nestedString(parsed, key)
		}
	}
	return ""
}

func cleanMIME(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if index := strings.IndexByte(value, ';'); index >= 0 {
		value = value[:index]
	}
	return value
}

func editableExtension(kind, contentType string) string {
	switch kind {
	case "ppt":
		return ".pptx"
	case "psd":
		return ".psd"
	case "zip":
		return ".zip"
	}
	if extension, _ := mime.ExtensionsByType(contentType); len(extension) > 0 {
		return extension[0]
	}
	return ""
}

func sanitizeEditableName(value string) string {
	return strings.TrimSpace(strings.ReplaceAll(filepath.Base(value), "\x00", ""))
}

func contentDispositionName(value string) string {
	_, params, err := mime.ParseMediaType(value)
	if err != nil {
		return ""
	}
	return sanitizeEditableName(params["filename"])
}

func firstNonEmptyProvider(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func decodeEditableInput(raw string, index int) (OpenAIImageInput, error) {
	value := strings.TrimSpace(raw)
	mimeType := ""
	if comma := strings.IndexByte(value, ','); strings.HasPrefix(strings.ToLower(value), "data:") && comma > 0 {
		header := value[5:comma]
		value = value[comma+1:]
		mimeType = strings.TrimSuffix(strings.Split(header, ";")[0], ";base64")
	}
	data, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return OpenAIImageInput{}, fmt.Errorf("decode image %d: %w", index, err)
	}
	if mimeType == "" {
		mimeType = http.DetectContentType(data)
	}
	extension := ".png"
	if values, _ := mime.ExtensionsByType(mimeType); len(values) > 0 {
		extension = values[0]
	}
	return OpenAIImageInput{Name: fmt.Sprintf("image_%d%s", index, extension), MIME: mimeType, Data: data}, nil
}

func DecodeEditableInputs(values []string) ([]OpenAIImageInput, error) {
	result := make([]OpenAIImageInput, 0, len(values))
	for index, value := range values {
		input, err := decodeEditableInput(value, index+1)
		if err != nil {
			return nil, err
		}
		result = append(result, input)
	}
	return result, nil
}

var _ = bytes.MinRead
