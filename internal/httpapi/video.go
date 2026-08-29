package httpapi

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/auucoder/gptgrok2api-go/internal/model"
	"github.com/auucoder/gptgrok2api-go/internal/protocol"
)

type videoJob struct {
	ID          string         `json:"id"`
	Object      string         `json:"object"`
	CreatedAt   int64          `json:"created_at"`
	Status      string         `json:"status"`
	Model       string         `json:"model"`
	Progress    int            `json:"progress"`
	Prompt      string         `json:"prompt"`
	Seconds     string         `json:"seconds"`
	Size        string         `json:"size"`
	Quality     string         `json:"quality"`
	CompletedAt int64          `json:"completed_at,omitempty"`
	Error       map[string]any `json:"error,omitempty"`
	ContentPath string         `json:"-"`
	VideoURL    string         `json:"-"`
}

func (s *Server) videosCreate(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "invalid multipart form", "invalid_request_error")
		return
	}
	modelName := strings.TrimSpace(r.FormValue("model"))
	if modelName == "" {
		modelName = "grok-imagine-video"
	}
	prompt := strings.TrimSpace(r.FormValue("prompt"))
	if prompt == "" {
		writeError(w, http.StatusBadRequest, "prompt cannot be empty", "invalid_request_error")
		return
	}
	seconds := positiveInt(r.FormValue("seconds"), 6)
	allowedSeconds := map[int]bool{6: true, 10: true, 12: true, 16: true, 20: true}
	if !allowedSeconds[seconds] {
		writeError(w, http.StatusBadRequest, "seconds must be one of [6, 10, 12, 16, 20]", "invalid_request_error")
		return
	}
	size := strings.TrimSpace(r.FormValue("size"))
	if size == "" {
		size = "720x1280"
	}
	aspect, ok := protocol.AspectRatio(size)
	if !ok || (size != "720x1280" && size != "1280x720" && size != "1024x1024" && size != "1024x1792" && size != "1792x1024") {
		writeError(w, http.StatusBadRequest, "invalid video size", "invalid_request_error")
		return
	}
	resolution := strings.ToLower(strings.TrimSpace(r.FormValue("resolution_name")))
	if resolution == "" {
		resolution = "720p"
	}
	if resolution != "480p" && resolution != "720p" {
		writeError(w, http.StatusBadRequest, "resolution_name must be 480p or 720p", "invalid_request_error")
		return
	}
	preset := strings.ToLower(strings.TrimSpace(r.FormValue("preset")))
	if preset == "" {
		preset = "custom"
	}
	if preset != "fun" && preset != "normal" && preset != "spicy" && preset != "custom" {
		writeError(w, http.StatusBadRequest, "invalid preset", "invalid_request_error")
		return
	}
	if _, ok := model.Find(s.catalog, modelName); !ok || modelName != "grok-imagine-video" {
		writeError(w, http.StatusBadRequest, "model is not a video model", "invalid_request_error")
		return
	}
	job := &videoJob{ID: "video_" + randomID(), Object: "video", CreatedAt: time.Now().Unix(), Status: "queued", Model: modelName, Progress: 0, Prompt: prompt, Seconds: fmt.Sprint(seconds), Size: size, Quality: "standard"}
	s.videoMu.Lock()
	s.videoJobs[job.ID] = job
	s.videoMu.Unlock()
	s.monitor.start(job.ID, "/v1/videos", modelName, prompt)
	var references []string
	for _, header := range multipartFiles(r, "input_reference[]", "input_reference")[:7] {
		file, err := header.Open()
		if err != nil {
			continue
		}
		raw, readErr := io.ReadAll(io.LimitReader(file, 16<<20))
		_ = file.Close()
		if readErr != nil || len(raw) == 0 {
			continue
		}
		mimeType := header.Header.Get("Content-Type")
		if mimeType == "" {
			mimeType = "image/jpeg"
		}
		lease, reserveErr := s.accountPool.Reserve(r.Context(), []string{"super", "heavy"}, nil)
		if reserveErr != nil {
			continue
		}
		fileID, fileURI, uploadErr := s.mediaProvider.Upload(r.Context(), lease.Account, header.Filename, mimeType, base64.StdEncoding.EncodeToString(raw))
		s.accountPool.Release(lease)
		if uploadErr == nil {
			if ref := protocol.ResolveAssetReference(fileID, fileURI, ""); ref != "" {
				references = append(references, ref)
			}
		}
	}
	go s.runVideoJob(job, prompt, aspect, resolution, seconds, preset, references)
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) runVideoJob(job *videoJob, prompt, aspect, resolution string, seconds int, preset string, refs []string) {
	s.setVideoStatus(job, "in_progress", 1, nil)
	lease, err := s.accountPool.Reserve(context.Background(), []string{"super", "heavy"}, nil)
	if err != nil {
		s.setVideoStatus(job, "failed", 0, map[string]any{"code": "no_available_account", "message": err.Error()})
		return
	}
	defer s.accountPool.Release(lease)
	post, err := s.mediaProvider.CreatePost(context.Background(), lease.Account, protocol.VideoPostMediaType, "", prompt)
	if err != nil {
		s.accountPool.Feedback(lease.Account, upstreamStatus(err), err)
		s.setVideoStatus(job, "failed", 0, map[string]any{"code": "create_post_failed", "message": err.Error()})
		return
	}
	postObject, _ := post["post"].(map[string]any)
	parentID := stringValue(postObject["id"])
	if parentID == "" {
		s.setVideoStatus(job, "failed", 0, map[string]any{"code": "invalid_post", "message": "video create-post returned no post id"})
		return
	}
	videoURL := ""
	assetID := ""
	extendPostID := parentID
	elapsedSeconds := 0
	segments, _ := protocol.VideoSegmentLengths(seconds)
	for index, segmentLength := range segments {
		payload := protocol.BuildVideoPayload(prompt, parentID, aspect, resolution, segmentLength, preset, refs)
		if index > 0 {
			payload = protocol.BuildVideoExtendPayload(prompt, parentID, extendPostID, aspect, resolution, segmentLength, preset, float64(elapsedSeconds)+1.0/24.0)
		}
		response, requestErr := s.mediaProvider.StreamChat(context.Background(), lease.Account, payload)
		if requestErr != nil {
			s.accountPool.Feedback(lease.Account, upstreamStatus(requestErr), requestErr)
			s.setVideoStatus(job, "failed", 0, map[string]any{"code": "upstream_failed", "message": requestErr.Error()})
			return
		}
		scanner := bufio.NewScanner(response.Body)
		scanner.Buffer(make([]byte, 4096), 8<<20)
		segmentPostID := ""
		segmentAssetID := ""
		segmentURL := ""
		for scanner.Scan() {
			kind, events := protocol.ParseSSELine(scanner.Text())
			if kind == "done" {
				break
			}
			for _, event := range events {
				if event.Progress > 0 {
					scaled := int((float64(index) + float64(event.Progress)/100.0) / float64(len(segments)) * 100.0)
					s.setVideoStatus(job, "in_progress", scaled, nil)
				}
				if event.Kind == "video" {
					if event.URL != "" {
						segmentURL = event.URL
					}
					if event.AssetID != "" {
						segmentAssetID = event.AssetID
					}
					if event.PostID != "" {
						segmentPostID = event.PostID
					}
					if event.ImageID != "" {
						segmentPostID = event.ImageID
					}
				}
			}
		}
		_ = response.Body.Close()
		if scanner.Err() != nil {
			s.setVideoStatus(job, "failed", 0, map[string]any{"code": "stream_failed", "message": scanner.Err().Error()})
			return
		}
		if segmentURL != "" {
			videoURL = segmentURL
		}
		if segmentAssetID != "" {
			assetID = segmentAssetID
		}
		if segmentPostID != "" {
			extendPostID = segmentPostID
		}
		elapsedSeconds += segmentLength
	}
	if videoURL == "" && assetID != "" {
		videoURL = protocol.ResolveAssetReference(assetID, "", "")
	}
	if videoURL == "" {
		s.setVideoStatus(job, "failed", 0, map[string]any{"code": "no_video", "message": "video generation returned no final video URL"})
		return
	}
	raw, mimeType, err := s.mediaProvider.Fetch(context.Background(), lease.Account, videoURL)
	if err != nil {
		s.setVideoStatus(job, "failed", 0, map[string]any{"code": "download_failed", "message": err.Error()})
		return
	}
	if err := os.MkdirAll(s.cfg.VideoDataDir, 0o755); err != nil {
		s.setVideoStatus(job, "failed", 0, map[string]any{"code": "storage_failed", "message": err.Error()})
		return
	}
	fileID := strings.TrimPrefix(job.ID, "video_")
	ext := ".mp4"
	if strings.Contains(mimeType, "webm") {
		ext = ".webm"
	}
	path := filepath.Join(s.cfg.VideoDataDir, fileID+ext)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		s.setVideoStatus(job, "failed", 0, map[string]any{"code": "storage_failed", "message": err.Error()})
		return
	}
	s.videoMu.Lock()
	job.CompletedAt = time.Now().Unix()
	job.ContentPath = path
	job.VideoURL = videoURL
	s.videoMu.Unlock()
	s.setVideoStatus(job, "completed", 100, nil)
	s.accountPool.Feedback(lease.Account, http.StatusOK, nil)
}

func (s *Server) videoByID(w http.ResponseWriter, r *http.Request) {
	if !s.requireAPI(w, r) {
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1/videos/")
	if strings.HasSuffix(path, "/content") {
		id := strings.TrimSuffix(path, "/content")
		s.videoContent(w, r, id)
		return
	}
	id := path
	s.videoMu.RLock()
	job, ok := s.videoJobs[id]
	var copy videoJob
	if ok {
		copy = *job
	}
	s.videoMu.RUnlock()
	if !ok {
		writeError(w, http.StatusNotFound, "video not found", "not_found")
		return
	}
	writeJSON(w, http.StatusOK, &copy)
}

func (s *Server) videoContent(w http.ResponseWriter, r *http.Request, id string) {
	s.videoMu.RLock()
	job, ok := s.videoJobs[id]
	var path string
	if ok {
		path = job.ContentPath
	}
	s.videoMu.RUnlock()
	if !ok {
		writeError(w, http.StatusNotFound, "video not found", "not_found")
		return
	}
	if path == "" {
		writeError(w, http.StatusConflict, "video content is not ready yet", "video_not_ready")
		return
	}
	if !isWithin(s.cfg.VideoDataDir, path) {
		writeError(w, http.StatusNotFound, "video not found", "not_found")
		return
	}
	http.ServeFile(w, r, path)
}

func (s *Server) setVideoStatus(job *videoJob, status string, progress int, failure map[string]any) {
	s.videoMu.Lock()
	defer s.videoMu.Unlock()
	job.Status = status
	if progress >= 0 {
		job.Progress = progress
	}
	if failure != nil {
		job.Error = failure
	}
	if s.monitor != nil {
		if status == "completed" {
			s.monitor.finish(job.ID, "success", job.Model, job.Prompt, "")
		} else if status == "failed" {
			message := "video job failed"
			if value, ok := failure["message"].(string); ok && value != "" {
				message = value
			}
			s.monitor.finish(job.ID, "failed", job.Model, job.Prompt, message)
		} else {
			s.monitor.update(job.ID, status, progress, "")
		}
	}
}

func multipartFiles(r *http.Request, keys ...string) []*multipart.FileHeader {
	if r.MultipartForm == nil {
		return nil
	}
	for _, key := range keys {
		if files := r.MultipartForm.File[key]; len(files) > 0 {
			return files
		}
	}
	return nil
}

func randomID() string { return fmt.Sprintf("%d", time.Now().UnixNano()) }
