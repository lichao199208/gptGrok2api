package provider

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/accounts"
	"github.com/auucoder/gptgrok2api-go/internal/protocol"
)

type ImageResult struct {
	URL    string
	Base64 string
	MIME   string
}

func (m *Media) GenerateLite(ctx context.Context, account accounts.Account, prompt, mode string, count int) ([]ImageResult, error) {
	if count < 1 {
		count = 1
	}
	if count > 10 {
		count = 10
	}
	response, err := m.StreamChat(ctx, account, protocol.BuildImageChatPayload(prompt, mode, count))
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	results := make([]ImageResult, 0, count)
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 4096), 4<<20)
	seen := map[string]bool{}
	for scanner.Scan() {
		kind, events := protocol.ParseSSELine(scanner.Text())
		if kind == "done" {
			break
		}
		for _, event := range events {
			if event.Kind != "image" || event.URL == "" || seen[event.URL] {
				continue
			}
			seen[event.URL] = true
			results = append(results, ImageResult{URL: absoluteAssetURL(event.URL)})
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("image generation returned no images")
	}
	return results, nil
}

func (m *Media) GenerateImagine(ctx context.Context, account accounts.Account, prompt, aspectRatio string, count int, enableNSFW, enablePro bool, wsURL string) ([]ImageResult, error) {
	socket := NewImagineSocket(wsURL, m.Client, m.RequestTimeout)
	socket.Proxy = m.Proxy
	return socket.Generate(ctx, account, prompt, aspectRatio, count, enableNSFW, enablePro)
}

func (m *Media) ResolveImage(ctx context.Context, account accounts.Account, image ImageResult, responseFormat, imageDir, publicBase string) (map[string]string, error) {
	format := strings.ToLower(strings.TrimSpace(responseFormat))
	if format == "" {
		format = "url"
	}
	if format != "url" && format != "b64_json" {
		return nil, fmt.Errorf("response_format must be url or b64_json")
	}
	if format == "b64_json" {
		if image.Base64 != "" {
			return map[string]string{"b64_json": image.Base64}, nil
		}
		raw, mime, err := m.Fetch(ctx, account, image.URL)
		if err != nil {
			return nil, err
		}
		return map[string]string{"b64_json": base64.StdEncoding.EncodeToString(raw), "mime": mime}, nil
	}
	raw, mime, err := m.Fetch(ctx, account, image.URL)
	if err != nil {
		return nil, err
	}
	fileID := mediaID(image.URL)
	if err := os.MkdirAll(imageDir, 0o755); err != nil {
		return nil, err
	}
	ext := extensionForMIME(mime)
	path := filepath.Join(imageDir, fileID+ext)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return nil, err
	}
	base := strings.TrimRight(publicBase, "/")
	value := "/v1/files/image?id=" + fileID
	if base != "" {
		value = base + value
	}
	return map[string]string{"url": value}, nil
}

func absoluteAssetURL(value string) string {
	if strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "https://") {
		return value
	}
	return "https://assets.grok.com/" + strings.TrimPrefix(value, "/")
}

func mediaID(value string) string {
	hash := fnv.New128a()
	_, _ = io.WriteString(hash, value+time.Now().UTC().Format("20060102150405"))
	return hex.EncodeToString(hash.Sum(nil))[:32]
}

func extensionForMIME(mime string) string {
	switch strings.ToLower(strings.TrimSpace(strings.Split(mime, ";")[0])) {
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	default:
		return ".jpg"
	}
}
