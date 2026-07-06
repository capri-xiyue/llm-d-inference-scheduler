/*
Copyright 2026 The llm-d Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package tokenizer

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"
)

// videoMP4DataURLPrefix is the data-URL scheme prefix for a base64 MP4 payload.
const videoMP4DataURLPrefix = "data:video/mp4;base64,"

// resolutionDims maps a lorem.video resolution label to its 16:9 pixel
// dimensions, standing in for what the x-llm-d-video-resolution header carries.
var resolutionDims = map[string][2]int{
	"360p":  {640, 360},
	"720p":  {1280, 720},
	"1080p": {1920, 1080},
}

// TestVideoEstimator_PlaceholderCount_Live compares placeholderCount against the
// multimodal video token count reported by a live Qwen3-VL vLLM server across a
// matrix of resolutions and durations.
//
// It is skipped unless VIDEO_ESTIMATE_ENDPOINT is set (host:port of the vLLM
// server, e.g. 10.0.0.1:8000). Videos are sourced from lorem.video, then
// base64-encoded into a data URL before both estimation and the API call, so the
// server and the estimator see identical bytes.
//
//	VIDEO_ESTIMATE_ENDPOINT=10.0.0.1:8000 go test ./pkg/.../tokenizer/ \
//	  -run TestVideoEstimator_PlaceholderCount_Live -v
func TestVideoEstimator_PlaceholderCount_Live(t *testing.T) {
	endpoint := os.Getenv("VIDEO_ESTIMATE_ENDPOINT")
	if endpoint == "" {
		endpoint = "34.57.228.181:8000"
	}
	model := os.Getenv("VIDEO_ESTIMATE_MODEL")
	if model == "" {
		model = "Qwen/Qwen3-VL-30B-A3B-Instruct"
	}

	resolutions := []string{"360p", "720p", "1080p"}
	durations := []int{1, 10, 20, 30, 60, 90}

	e := newVideoEstimator(&estimateConfig{Video: &videoEstimateConfig{
		Frames:         &framesConfig{Mode: videoFramesModeSampled, SampleFPS: 2},
		TokensPerFrame: &tokensPerFrameConfig{Mode: videoTPFModeDynamic, Factor: 32 * 32},
		MaxVideoTokens: 12288,
	}})
	client := &http.Client{Timeout: 120 * time.Second}

	t.Logf("%-10s %-6s %10s %10s %8s", "resolution", "dur", "estimate", "actual", "err%")
	for _, res := range resolutions {
		for _, dur := range durations {
			t.Run(fmt.Sprintf("%s/%ds", res, dur), func(t *testing.T) {
				videoURL := fmt.Sprintf("https://lorem.video/%s_h264_%ds", res, dur)
				raw, err := download(context.Background(), client, videoURL)
				if err != nil {
					t.Fatalf("download %s: %v", videoURL, err)
				}
				dataURL := videoMP4DataURLPrefix + base64.StdEncoding.EncodeToString(raw)

				// Feed the metadata a client would pass via x-llm-d-video-* headers.
				dims := resolutionDims[res]
				estimate := e.placeholderCount(videoMetadata{width: dims[0], height: dims[1], duration: float64(dur)})
				actual, err := liveVideoTokens(context.Background(), client, endpoint, model, dataURL)
				if err != nil {
					t.Fatalf("query server: %v", err)
				}

				var errPct float64
				if actual != 0 {
					errPct = float64(estimate-actual) / float64(actual) * 100
				}
				t.Logf("%-10s %-5ds %10d %10d %7.1f%%", res, dur, estimate, actual, errPct)
			})
		}
	}
}

// download fetches url and returns its body.
func download(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// liveVideoTokens posts a single-video chat completion request and returns the
// server-reported multimodal video token count from usage.prompt_tokens_details.
func liveVideoTokens(ctx context.Context, client *http.Client, endpoint, model, videoDataURL string) (int, error) {
	// Build the body from a JSON template so the model name and (untrusted) data
	// URL are properly escaped while the request shape stays a single literal.
	modelJSON, err := json.Marshal(model)
	if err != nil {
		return 0, err
	}
	urlJSON, err := json.Marshal(videoDataURL)
	if err != nil {
		return 0, err
	}
	body := fmt.Appendf(nil, `{"model":%s,"messages":[{"role":"user","content":[{"type":"text","text":"describe the video"},{"type":"video_url","video_url":{"url":%s}}]}],"max_tokens":1,"temperature":0}`, modelJSON, urlJSON)

	url := fmt.Sprintf("http://%s/v1/chat/completions", endpoint)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("status %d: %s", resp.StatusCode, respBody)
	}

	var parsed struct {
		Usage struct {
			PromptTokensDetails struct {
				MultimodalTokens struct {
					Video int `json:"video"`
				} `json:"multimodal_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return 0, fmt.Errorf("decode response: %w", err)
	}
	return parsed.Usage.PromptTokensDetails.MultimodalTokens.Video, nil
}
