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

package kvcacheretention

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	fwkplugin "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/plugin"
	"github.com/llm-d/llm-d-router/pkg/epp/framework/interface/requestcontrol"
	fwkrh "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/requesthandling"
	fwksched "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/scheduling"
)

func newTestPlugin(t *testing.T, mutate func(*Config)) *Plugin {
	t.Helper()
	cfg := DefaultConfig
	if mutate != nil {
		mutate(&cfg)
	}
	p, err := NewPlugin("test", cfg)
	require.NoError(t, err)
	return p
}

func agenticRequest(sessionID string) *fwksched.InferenceRequest {
	return &fwksched.InferenceRequest{
		RequestID: "req-1",
		Headers: map[string]string{
			"x-session-id":   sessionID,
			"x-session-type": "agentic",
		},
		Body: &fwkrh.InferenceRequestBody{
			Payload: fwkrh.PayloadMap{"model": "m", "messages": []any{}},
		},
	}
}

func directive(t *testing.T, request *fwksched.InferenceRequest) map[string]any {
	t.Helper()
	payload, ok := request.Body.Payload.AsMap()
	require.True(t, ok)
	directives, ok := payload[retentionDirectivesField].([]any)
	require.True(t, ok, "retention_directives must be present")
	require.Len(t, directives, 1)
	d, ok := directives[0].(map[string]any)
	require.True(t, ok)
	return d
}

func TestFactory(t *testing.T) {
	t.Parallel()

	plg, err := Factory("kv-cache-retention", fwkplugin.StrictDecoder(json.RawMessage(`{"priority":50,"quantile":0.8}`)), nil)
	require.NoError(t, err)
	assert.Equal(t, PluginType, plg.TypedName().Type)

	_, err = Factory("kv-cache-retention", fwkplugin.StrictDecoder(json.RawMessage(`{"priority":200}`)), nil)
	assert.ErrorContains(t, err, "priority")

	_, err = Factory("kv-cache-retention", fwkplugin.StrictDecoder(json.RawMessage(`{"quantile":1.5}`)), nil)
	assert.ErrorContains(t, err, "quantile")

	_, err = Factory("kv-cache-retention", fwkplugin.StrictDecoder(json.RawMessage(`{"unknown":true}`)), nil)
	assert.ErrorContains(t, err, "failed to parse")
}

func TestPreRequest_InjectsDirective(t *testing.T) {
	t.Parallel()

	p := newTestPlugin(t, nil)
	request := agenticRequest("s1")

	require.NoError(t, p.PreRequest(t.Context(), request, nil))

	assert.True(t, request.Body.Mutated)
	payload, _ := request.Body.Payload.AsMap()
	assert.Equal(t, "s1", payload[retentionScopeField])

	d := directive(t, request)
	assert.Equal(t, 0, d["start"])
	assert.Nil(t, d["end"])
	assert.Equal(t, 70, d["priority"])

	// With no observations the duration comes from the seed parameters:
	// the 0.9 quantile of LogNormal(2.28, 1.34).
	wantSeconds := math.Exp(2.28 + 1.34*math.Sqrt2*math.Erfinv(2*0.9-1))
	assert.InDelta(t, wantSeconds, d["duration"], 1e-6)
}

func TestPreRequest_SkipsNonMatchingRequests(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		headers map[string]string
	}{
		{name: "no session id", headers: map[string]string{"x-session-type": "agentic"}},
		{name: "wrong session type", headers: map[string]string{"x-session-id": "s1", "x-session-type": "chat"}},
		{name: "missing session type", headers: map[string]string{"x-session-id": "s1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := newTestPlugin(t, nil)
			request := agenticRequest("s1")
			request.Headers = tt.headers

			require.NoError(t, p.PreRequest(t.Context(), request, nil))
			assert.False(t, request.Body.Mutated)
		})
	}
}

func TestPreRequest_SessionTypeEmptyMatchesAll(t *testing.T) {
	t.Parallel()

	p := newTestPlugin(t, func(cfg *Config) { cfg.SessionType = "" })
	request := agenticRequest("s1")
	delete(request.Headers, "x-session-type")

	require.NoError(t, p.PreRequest(t.Context(), request, nil))
	assert.True(t, request.Body.Mutated)
}

func TestPreRequest_ClientDirectivesWin(t *testing.T) {
	t.Parallel()

	p := newTestPlugin(t, nil)
	request := agenticRequest("s1")
	clientDirectives := []any{map[string]any{"start": 0, "priority": 99}}
	payload, _ := request.Body.Payload.AsMap()
	payload[retentionDirectivesField] = clientDirectives

	require.NoError(t, p.PreRequest(t.Context(), request, nil))

	assert.False(t, request.Body.Mutated)
	assert.Equal(t, clientDirectives, payload[retentionDirectivesField])
	assert.NotContains(t, payload, retentionScopeField)
}

func TestPreRequest_NonMapPayloadStillObserves(t *testing.T) {
	t.Parallel()

	p := newTestPlugin(t, func(cfg *Config) { cfg.MinSamples = 2 })
	base := time.Unix(1000, 0)
	clock := base
	p.now = func() time.Time { return clock }

	request := agenticRequest("s1")
	request.Body.Payload = fwkrh.RawPayload(`{}`)

	require.NoError(t, p.PreRequest(t.Context(), request, nil))
	clock = base.Add(10 * time.Second)
	require.NoError(t, p.PreRequest(t.Context(), request, nil))

	assert.False(t, request.Body.Mutated)
	_, _, observed := p.estimator.snapshot()
	assert.EqualValues(t, 1, observed)
}

func TestGapObservation_MeasuresIdleFromResponseCompletion(t *testing.T) {
	t.Parallel()

	// EMAFactor 1 and MinSamples 2 make the fitted logMean equal the sample
	// mean of the observed gaps, exposing the measured values directly.
	p := newTestPlugin(t, func(cfg *Config) { cfg.EMAFactor = 1.0; cfg.MinSamples = 2 })
	base := time.Unix(1000, 0)
	clock := base
	p.now = func() time.Time { return clock }

	request := agenticRequest("s1")
	require.NoError(t, p.PreRequest(t.Context(), request, nil))

	// Each turn completes 20s after arrival; the next arrives 30s after
	// completion. The observed gap must be the 30s idle time, not the 50s
	// arrival-to-arrival time.
	clock = base.Add(20 * time.Second)
	p.ResponseBody(t.Context(), request, &requestcontrol.Response{EndOfStream: true}, nil)
	clock = base.Add(50 * time.Second)
	require.NoError(t, p.PreRequest(t.Context(), request, nil))

	clock = base.Add(70 * time.Second)
	p.ResponseBody(t.Context(), request, &requestcontrol.Response{EndOfStream: true}, nil)
	clock = base.Add(100 * time.Second)
	require.NoError(t, p.PreRequest(t.Context(), request, nil))

	logMean, _, observed := p.estimator.snapshot()
	assert.EqualValues(t, 2, observed)
	assert.InDelta(t, math.Log(30), logMean, 1e-9)
}

func TestGapObservation_DiscardsSubMinIntervals(t *testing.T) {
	t.Parallel()

	p := newTestPlugin(t, nil)
	base := time.Unix(1000, 0)
	clock := base
	p.now = func() time.Time { return clock }

	request := agenticRequest("s1")
	require.NoError(t, p.PreRequest(t.Context(), request, nil))

	clock = base.Add(10 * time.Millisecond)
	require.NoError(t, p.PreRequest(t.Context(), request, nil))

	_, _, observed := p.estimator.snapshot()
	assert.EqualValues(t, 0, observed)
}

func TestRetentionDuration_Clamped(t *testing.T) {
	t.Parallel()

	// Seed parameters put the 0.9 quantile near 54s; clamps override it.
	pMin := newTestPlugin(t, func(cfg *Config) { cfg.MinRetention = "2m"; cfg.MaxRetention = "5m" })
	assert.Equal(t, 2*time.Minute, pMin.retentionDuration())

	pMax := newTestPlugin(t, func(cfg *Config) { cfg.MaxRetention = "10s" })
	assert.Equal(t, 10*time.Second, pMax.retentionDuration())
}

func TestResponseBody_IgnoresNonFinalChunks(t *testing.T) {
	t.Parallel()

	p := newTestPlugin(t, nil)
	base := time.Unix(1000, 0)
	clock := base
	p.now = func() time.Time { return clock }

	request := agenticRequest("s1")
	require.NoError(t, p.PreRequest(t.Context(), request, nil))

	clock = base.Add(20 * time.Second)
	p.ResponseBody(t.Context(), request, &requestcontrol.Response{EndOfStream: false}, nil)

	// The gap origin stays at the request arrival because no final chunk was
	// seen.
	clock = base.Add(50 * time.Second)
	gap, ok := p.tracker.observe("s1", clock)
	require.True(t, ok)
	assert.Equal(t, 50*time.Second, gap)
}

func TestDumpState(t *testing.T) {
	t.Parallel()

	p := newTestPlugin(t, nil)
	request := agenticRequest("s1")
	require.NoError(t, p.PreRequest(t.Context(), request, nil))

	raw, err := p.DumpState()
	require.NoError(t, err)

	var state debugState
	require.NoError(t, json.Unmarshal(raw, &state))
	assert.Equal(t, 2.28, state.LogMean)
	assert.Equal(t, 1.34, state.LogStd)
	assert.Equal(t, 1, state.TrackedSessions)
	assert.Positive(t, state.RetentionSeconds)
}
