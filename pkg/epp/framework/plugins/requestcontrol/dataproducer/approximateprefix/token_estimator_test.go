package approximateprefix

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"

	fwkrh "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/requesthandling"
)

func TestApproximatePrefixCacheTokenEstimator(t *testing.T) {
	tests := []struct {
		name          string
		multimodalCfg *multiModalTokenEstimatorConfig
		block         fwkrh.ContentBlock
		expected      int
	}{
		{
			name:          "EmptyText",
			multimodalCfg: nil,
			block:         fwkrh.ContentBlock{Type: "text", Text: ""},
			expected:      0,
		},
		{
			name:          "Text_4Chars",
			multimodalCfg: nil,
			block:         fwkrh.ContentBlock{Type: "text", Text: "aaaa"},
			expected:      1,
		},
		{
			name:          "Text_5Chars",
			multimodalCfg: nil,
			block:         fwkrh.ContentBlock{Type: "text", Text: "aaaaa"},
			expected:      1,
		},
		{
			name: "Image_Fixed",
			multimodalCfg: &multiModalTokenEstimatorConfig{
				Image: &imageTokenEstimatorConfig{
					Mode: ModeFixed,
					FixedCfg: &fixedTokenEstimatorConfig{
						FixedToken: 10,
					},
				},
			},
			block: fwkrh.ContentBlock{
				Type:     "image_url",
				ImageURL: fwkrh.ImageBlock{URL: "https://example.com/image.jpg"},
			},
			expected: 10,
		},
		{
			name: "Image_Dynamic",
			multimodalCfg: &multiModalTokenEstimatorConfig{
				Image: &imageTokenEstimatorConfig{
					Mode: ModeDynamic,
					DefaultResolution: resolution{
						Width:  1920,
						Height: 1080,
					},
					DynamicCfg: &dynamicTokenEstimatorConfig{
						Factor: 1024,
					},
				},
			},
			block: fwkrh.ContentBlock{
				Type:     "image_url",
				ImageURL: fwkrh.ImageBlock{URL: base64Image180p1},
			},
			expected: 56,
		},
		{
			name:          "Video_DefaultConfig_NoDurationParam",
			multimodalCfg: nil,
			block: fwkrh.ContentBlock{
				Type:     "video_url",
				VideoURL: fwkrh.VideoBlock{URL: "https://example.com/video.mp4"},
			},
			expected: 4500,
		},
		{
			name:          "Video_DefaultConfig_WithDurationParam",
			multimodalCfg: nil,
			block: fwkrh.ContentBlock{
				Type:     "video_url",
				VideoURL: fwkrh.VideoBlock{URL: "https://example.com/video.mp4?duration=1.5"},
			},
			expected: 900,
		},
		{
			name: "Video_Qwen3VL_LongVideo_TimeDilution",
			multimodalCfg: &multiModalTokenEstimatorConfig{
				Video: &videoTokenEstimatorConfig{
					FrameCfg: videoFrameEstimatorConfig{
						Mode: ModeDynamic,
						DynamicCfg: &dynamicFrameEstimatorConfig{
							FPS:                    2.0,
							MinFrames:              4,
							MaxFrames:              64,
							MaxDurationSeconds:     0.0,
							DefaultDurationSeconds: 10.0,
						},
					},
					ImageCfg: &imageTokenEstimatorConfig{
						Mode: ModeDynamic,
						DefaultResolution: resolution{
							Width:  640,
							Height: 360,
						},
						DynamicCfg: &dynamicTokenEstimatorConfig{
							Factor: 1024,
						},
					},
					PatchSize: 1,
				},
			},
			block: fwkrh.ContentBlock{
				Type:     "video_url",
				VideoURL: fwkrh.VideoBlock{URL: "https://example.com/video.mp4?duration=1000.0"},
			},
			expected: 14400,
		},
		{
			name: "Video_Gemma4_LongVideo_TimeTruncation",
			multimodalCfg: &multiModalTokenEstimatorConfig{
				Video: &videoTokenEstimatorConfig{
					FrameCfg: videoFrameEstimatorConfig{
						Mode: ModeDynamic,
						DynamicCfg: &dynamicFrameEstimatorConfig{
							FPS:                    1.0,
							MinFrames:              0,
							MaxFrames:              60,
							MaxDurationSeconds:     60.0,
							DefaultDurationSeconds: 10.0,
						},
					},
					ImageCfg: &imageTokenEstimatorConfig{
						Mode: ModeFixed,
						FixedCfg: &fixedTokenEstimatorConfig{
							FixedToken: 768,
						},
					},
					PatchSize: 1,
				},
			},
			block: fwkrh.ContentBlock{
				Type:     "video_url",
				VideoURL: fwkrh.VideoBlock{URL: "https://example.com/video.mp4?duration=1000.0"},
			},
			expected: 46080,
		},
		{
			name: "Video_CustomConfig_WithPatchSize",
			multimodalCfg: &multiModalTokenEstimatorConfig{
				Video: &videoTokenEstimatorConfig{
					FrameCfg: videoFrameEstimatorConfig{
						Mode: ModeDynamic,
						DynamicCfg: &dynamicFrameEstimatorConfig{
							FPS:                    5.0,
							MinFrames:              2,
							MaxFrames:              20,
							MaxDurationSeconds:     10.0,
							DefaultDurationSeconds: 4.0,
						},
					},
					ImageCfg: &imageTokenEstimatorConfig{
						Mode: ModeFixed,
						FixedCfg: &fixedTokenEstimatorConfig{
							FixedToken: 100,
						},
					},
					PatchSize: 4,
				},
			},
			block: fwkrh.ContentBlock{
				Type:     "video_url",
				VideoURL: fwkrh.VideoBlock{URL: "https://example.com/video.mp4?duration=2.0"},
			},
			expected: 250,
		},
		{
			name: "Video_CustomConfig_WithFixedFrames",
			multimodalCfg: &multiModalTokenEstimatorConfig{
				Video: &videoTokenEstimatorConfig{
					FrameCfg: videoFrameEstimatorConfig{
						Mode: ModeFixed,
						FixedCfg: &fixedFrameEstimatorConfig{
							FixedFrames: 8,
						},
					},
					ImageCfg: &imageTokenEstimatorConfig{
						Mode: ModeFixed,
						FixedCfg: &fixedTokenEstimatorConfig{
							FixedToken: 100,
						},
					},
					PatchSize: 2,
				},
			},
			block: fwkrh.ContentBlock{
				Type:     "video_url",
				VideoURL: fwkrh.VideoBlock{URL: "https://example.com/video.mp4"},
			},
			expected: 400,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			estimator := NewApproximatePrefixCacheTokenEstimator(context.Background(), tt.multimodalCfg)
			assert.Equal(t, tt.expected, estimator.Estimate(tt.block))
		})
	}
}
