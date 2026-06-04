/*
Copyright 2026 The Kubernetes Authors.

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

package approximateprefix

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"net/url"
	"strconv"
	"strings"

	// needed for image dimension parse
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	logutil "github.com/llm-d/llm-d-router/pkg/common/observability/logging"
	fwkrh "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/requesthandling"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// TokenEstimator estimates the number of tokens for different content types.
type TokenEstimator interface {
	Estimate(block fwkrh.ContentBlock) int
}

type approximatePrefixCacheTokenEstimator struct {
	ctx              context.Context
	multimodalConfig *multiModalTokenEstimatorConfig
}

// NewApproximatePrefixCacheTokenEstimator returns a new TokenEstimator.
func NewApproximatePrefixCacheTokenEstimator(ctx context.Context, multimodalConfig *multiModalTokenEstimatorConfig) TokenEstimator {
	return &approximatePrefixCacheTokenEstimator{
		ctx:              ctx,
		multimodalConfig: multimodalConfig,
	}
}

func (e *approximatePrefixCacheTokenEstimator) Estimate(block fwkrh.ContentBlock) int {
	switch block.Type {
	case "text":
		return len(block.Text) / averageCharactersPerToken
	case "image_url":
		return getImagePlaceholders(e.ctx, block.ImageURL.URL, e.multimodalConfig)
	case "video_url":
		return getVideoPlaceholders(e.ctx, block.VideoURL.URL, e.multimodalConfig)
	case "input_audio", "audio_url":
		// Add audio support later
		return 0
	default:
		return 0
	}
}

func getImagePlaceholders(ctx context.Context, url string, multimodalConfig *multiModalTokenEstimatorConfig) int {
	if multimodalConfig == nil || multimodalConfig.Image == nil {
		multimodalConfig = &defaultMultimodalConfig
	}
	logger := log.FromContext(ctx).V(logutil.DEBUG)
	var numPlaceHolders int
	switch multimodalConfig.Image.Mode {
	case ModeFixed:
		numPlaceHolders = multimodalConfig.Image.FixedCfg.FixedToken
		logger.Info("using fixed token placeholders")
	case ModeDynamic:
		if strings.HasPrefix(url, "data:image/") && strings.Contains(url, "base64,") {
			resolution, err := getImageDimensionsFromBase64(url)
			if err != nil {
				logger.Error(err, "failed to get image dimensions from base64 content, using default image resolution")
				numPlaceHolders = multimodalConfig.Image.DefaultResolution.Width * multimodalConfig.Image.DefaultResolution.Height / multimodalConfig.Image.DynamicCfg.Factor
			} else {
				logger.Info(fmt.Sprintf("Using image resolution height %d width %d", resolution.Height, resolution.Width))
				numPlaceHolders = (resolution.Width * resolution.Height) / (multimodalConfig.Image.DynamicCfg.Factor)
			}
		} else {
			logger.Info("Failed to get image dimensions with unsupported type, now we only support base64 encoded image content, using default image resolution")
			numPlaceHolders = multimodalConfig.Image.DefaultResolution.Width * multimodalConfig.Image.DefaultResolution.Height / multimodalConfig.Image.DynamicCfg.Factor
		}
	}
	logger.Info(fmt.Sprintf("Using numPlaceHolders %d", numPlaceHolders))
	return numPlaceHolders
}

func getImageDimensionsFromBase64(url string) (*resolution, error) {
	idx := strings.Index(url, "base64,")
	base64Data := url[idx+7:]
	decoded, err := base64.StdEncoding.DecodeString(base64Data)
	if err != nil {
		return nil, fmt.Errorf("failed to decode base64: %w", err)
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(decoded))
	if err != nil {
		return nil, fmt.Errorf("failed to decode image config: %w", err)
	}
	if config.Width <= 0 || config.Height <= 0 {
		return nil, errors.New("image config width and height must be positive")
	}
	return &resolution{
		Width:  config.Width,
		Height: config.Height,
	}, nil
}

func getVideoPlaceholders(ctx context.Context, url string, multimodalConfig *multiModalTokenEstimatorConfig) int {
	if multimodalConfig == nil || multimodalConfig.Video == nil {
		multimodalConfig = &defaultMultimodalConfig
	}
	logger := log.FromContext(ctx).V(logutil.DEBUG)
	videoCfg := multimodalConfig.Video
	frameCfg := videoCfg.FrameCfg
	var n int
	switch frameCfg.Mode {
	case ModeFixed:
		if frameCfg.FixedCfg != nil {
			n = frameCfg.FixedCfg.FixedFrames
		}
		logger.Info(fmt.Sprintf("using fixed frames for video: %d", n))
	case ModeDynamic:
		if frameCfg.DynamicCfg != nil {
			dynCfg := frameCfg.DynamicCfg
			duration := getVideoDurationFromURL(ctx, url, dynCfg.DefaultDurationSeconds)

			// Formula: N = max(min_frames, min(floor(T_valid * fps), max_frames))
			// where T_valid = min(T, T_max_duration) (if T_max_duration > 0)
			tValid := duration
			if dynCfg.MaxDurationSeconds > 0 && duration > dynCfg.MaxDurationSeconds {
				tValid = dynCfg.MaxDurationSeconds
			}

			n = int(tValid * dynCfg.FPS)
			if n < dynCfg.MinFrames {
				n = dynCfg.MinFrames
			}
			if n > dynCfg.MaxFrames {
				n = dynCfg.MaxFrames
			}
		}
		logger.Info(fmt.Sprintf("using dynamic frames for video: %d", n))
	}

	var tokensPerFrame int
	imgCfg := videoCfg.ImageCfg
	if imgCfg == nil {
		imgCfg = defaultMultimodalConfig.Image
	}
	if imgCfg != nil {
		switch imgCfg.Mode {
		case ModeFixed:
			if imgCfg.FixedCfg != nil {
				tokensPerFrame = imgCfg.FixedCfg.FixedToken
			}
			logger.Info("using fixed tokens per frame for video")
		case ModeDynamic:
			if imgCfg.DynamicCfg != nil && imgCfg.DynamicCfg.Factor > 0 {
				tokensPerFrame = imgCfg.DefaultResolution.Width * imgCfg.DefaultResolution.Height / imgCfg.DynamicCfg.Factor
			}
			logger.Info(fmt.Sprintf("using dynamic tokens per frame for video resolution width %d height %d", imgCfg.DefaultResolution.Width, imgCfg.DefaultResolution.Height))
		}
	}

	patchSize := videoCfg.PatchSize
	if patchSize <= 0 {
		patchSize = 1
	}

	numPlaceholders := (n * tokensPerFrame) / patchSize
	logger.Info(fmt.Sprintf("calculated video placeholders: frames=%d, tokensPerFrame=%d, patchSize=%d, total=%d", n, tokensPerFrame, patchSize, numPlaceholders))
	return numPlaceholders
}

func getVideoDurationFromURL(ctx context.Context, urlStr string, defaultSecs float64) float64 {
	logger := log.FromContext(ctx).V(logutil.DEBUG)
	u, err := url.Parse(urlStr)
	if err != nil {
		logger.Error(err, "failed to parse video URL, using default duration")
		return defaultSecs
	}
	q := u.Query()
	if durationStr := q.Get("duration"); durationStr != "" {
		d, err := strconv.ParseFloat(durationStr, 64)
		if err != nil {
			logger.Error(err, "failed to parse video duration from query param, using default duration", "duration", durationStr)
			return defaultSecs
		}
		if d <= 0 {
			logger.Info("video duration from query param is non-positive, using default duration", "duration", d)
			return defaultSecs
		}
		return d
	}
	return defaultSecs
}
