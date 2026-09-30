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

package agenthints

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	fwkplugin "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/plugin"
	fwkrh "github.com/llm-d/llm-d-router/pkg/epp/framework/interface/requesthandling"
	"github.com/llm-d/llm-d-router/pkg/epp/framework/interface/scheduling"
	"github.com/llm-d/llm-d-router/pkg/epp/metadata"
)

func newPlugin(t *testing.T, rawParams string) *Plugin {
	t.Helper()
	var dec *json.Decoder
	if rawParams != "" {
		dec = json.NewDecoder(strings.NewReader(rawParams))
	}
	pi, err := PluginFactory("test", dec, nil)
	if err != nil {
		t.Fatalf("PluginFactory: %v", err)
	}
	return pi.(*Plugin)
}

// bodyWithHints builds an InferenceRequestBody whose payload is the JSON
// document decoded exactly as the OpenAI parser would decode it (numbers as
// float64), so tests exercise the real type-conversion path.
func bodyWithHints(t *testing.T, jsonBody string) *fwkrh.InferenceRequestBody {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(jsonBody), &m); err != nil {
		t.Fatalf("bad test body: %v", err)
	}
	return &fwkrh.InferenceRequestBody{Payload: fwkrh.PayloadMap(m)}
}

type want struct {
	priority           *int
	strictPriority     *int
	osl                *int
	speculativePrefill *bool
}

func ip(v int) *int   { return &v }
func bp(v bool) *bool { return &v }
func (w want) check(t *testing.T, req *scheduling.InferenceRequest) {
	t.Helper()
	checkInt(t, req, PriorityKey, w.priority)
	checkInt(t, req, StrictPriorityKey, w.strictPriority)
	checkInt(t, req, OSLKey, w.osl)
	got, ok := scheduling.ReadRequestAttribute[bool](req, SpeculativePrefillKey)
	if (w.speculativePrefill != nil) != ok {
		t.Errorf("%s: found=%v, want found=%v", SpeculativePrefillKey, ok, w.speculativePrefill != nil)
	} else if ok && got != *w.speculativePrefill {
		t.Errorf("%s = %v, want %v", SpeculativePrefillKey, got, *w.speculativePrefill)
	}
}

func checkInt(t *testing.T, req *scheduling.InferenceRequest, key fwkplugin.DataKey, wantV *int) {
	t.Helper()
	got, ok := scheduling.ReadRequestAttribute[int](req, key)
	if (wantV != nil) != ok {
		t.Errorf("%s: found=%v, want found=%v", key, ok, wantV != nil)
		return
	}
	if ok && got != *wantV {
		t.Errorf("%s = %d, want %d", key, got, *wantV)
	}
}

func TestRequestHeader(t *testing.T) {
	p := newPlugin(t, "")

	tests := []struct {
		name    string
		headers map[string]string
		body    *fwkrh.InferenceRequestBody
		want    want
	}{
		{
			name: "all hints from body (llmdext body format)",
			body: bodyWithHints(t, `{"model":"m","llmdext":{"agent_hints":{
				"priority":5,"strict_priority":1,"osl":1024,"speculative_prefill":true}}}`),
			want: want{priority: ip(5), strictPriority: ip(1), osl: ip(1024), speculativePrefill: bp(true)},
		},
		{
			name: "all hints from llm-d headers",
			headers: map[string]string{
				metadata.RequestPriorityHeaderKey:       "7",
				metadata.RequestStrictPriorityHeaderKey: "2",
				metadata.ExpectedOutputTokensHeaderKey:  "256",
				metadata.SpeculativePrefillHeaderKey:    "true",
			},
			want: want{priority: ip(7), strictPriority: ip(2), osl: ip(256), speculativePrefill: bp(true)},
		},
		{
			name: "header wins over body per field; other fields still come from body",
			headers: map[string]string{
				metadata.RequestPriorityHeaderKey: "9",
			},
			body: bodyWithHints(t, `{"llmdext":{"agent_hints":{"priority":2,"osl":512}}}`),
			want: want{priority: ip(9), osl: ip(512)},
		},
		{
			name: "malformed header falls back to body",
			headers: map[string]string{
				metadata.RequestPriorityHeaderKey:      "high",
				metadata.ExpectedOutputTokensHeaderKey: "",
			},
			body: bodyWithHints(t, `{"llmdext":{"agent_hints":{"priority":2,"osl":512}}}`),
			want: want{priority: ip(2), osl: ip(512)},
		},
		{
			name: "malformed header with no body fallback leaves no attribute",
			headers: map[string]string{
				metadata.RequestPriorityHeaderKey:    "high",
				metadata.SpeculativePrefillHeaderKey: "maybe",
			},
			want: want{},
		},
		{
			name:    "header whitespace is trimmed",
			headers: map[string]string{metadata.RequestPriorityHeaderKey: "  4 "},
			want:    want{priority: ip(4)},
		},
		{
			name:    "negative priority is preserved (consumers clamp)",
			headers: map[string]string{metadata.RequestPriorityHeaderKey: "-3"},
			want:    want{priority: ip(-3)},
		},
		{
			name:    "negative strict priority is rejected",
			headers: map[string]string{metadata.RequestStrictPriorityHeaderKey: "-1"},
			body:    bodyWithHints(t, `{"llmdext":{"agent_hints":{"strict_priority":-1}}}`),
			want:    want{},
		},
		{
			name: "zero and negative osl are rejected",
			body: bodyWithHints(t, `{"llmdext":{"agent_hints":{"osl":0}}}`),
			want: want{},
		},
		{
			name: "non-integer body number is rejected",
			body: bodyWithHints(t, `{"llmdext":{"agent_hints":{"priority":1.5,"osl":100}}}`),
			want: want{osl: ip(100)},
		},
		{
			name: "wrong body types are rejected field by field",
			body: bodyWithHints(t, `{"llmdext":{"agent_hints":{"priority":"5","speculative_prefill":"yes","osl":8}}}`),
			want: want{osl: ip(8)},
		},
		{
			name:    "header bool accepts strconv forms",
			headers: map[string]string{metadata.SpeculativePrefillHeaderKey: "1"},
			want:    want{speculativePrefill: bp(true)},
		},
		{
			name: "explicit false is published as false",
			body: bodyWithHints(t, `{"llmdext":{"agent_hints":{"speculative_prefill":false}}}`),
			want: want{speculativePrefill: bp(false)},
		},
		{
			name: "llmdext without agent_hints leaves no attribute",
			body: bodyWithHints(t, `{"llmdext":{"backend_instance_id":3}}`),
			want: want{},
		},
		{
			name: "agent_hints that is not an object is ignored",
			body: bodyWithHints(t, `{"llmdext":{"agent_hints":[1,2]}}`),
			want: want{},
		},
		{
			name: "unparsed raw body leaves no attribute",
			body: &fwkrh.InferenceRequestBody{Payload: fwkrh.RawPayload(`{"llmdext":{"agent_hints":{"priority":5}}}`)},
			want: want{},
		},
		{
			name: "nil body does not panic",
			body: nil,
			want: want{},
		},
		{
			name:    "nil headers and empty body do not panic",
			headers: nil,
			body:    &fwkrh.InferenceRequestBody{},
			want:    want{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &scheduling.InferenceRequest{Headers: tt.headers, Body: tt.body}
			if err := p.RequestHeader(context.Background(), req); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			tt.want.check(t, req)
			// The plugin must not touch flow-control identity or objectives.
			if req.FairnessID != "" {
				t.Errorf("FairnessID = %q, want empty", req.FairnessID)
			}
			if req.Objectives.Priority != 0 {
				t.Errorf("Objectives.Priority = %d, want 0 (plugin must not set it)", req.Objectives.Priority)
			}
			if tt.body != nil && tt.body.Mutated {
				t.Errorf("body marked Mutated with stripFromBody disabled")
			}
		})
	}
}

func TestRequestHeader_NilRequest(t *testing.T) {
	p := newPlugin(t, "")
	if err := p.RequestHeader(context.Background(), nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestStripFromBody(t *testing.T) {
	tests := []struct {
		name        string
		jsonBody    string
		wantMutated bool
		wantExt     bool // llmdext key still present after strip
	}{
		{
			name:        "agent_hints removed and empty llmdext dropped",
			jsonBody:    `{"model":"m","llmdext":{"agent_hints":{"priority":5}}}`,
			wantMutated: true,
			wantExt:     false,
		},
		{
			name:        "sibling llmdext fields are preserved",
			jsonBody:    `{"model":"m","llmdext":{"agent_hints":{"priority":5},"backend_instance_id":3}}`,
			wantMutated: true,
			wantExt:     true,
		},
		{
			name:        "no agent_hints means no mutation",
			jsonBody:    `{"model":"m","llmdext":{"backend_instance_id":3}}`,
			wantMutated: false,
			wantExt:     true,
		},
	}

	p := newPlugin(t, `{"stripFromBody": true}`)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := bodyWithHints(t, tt.jsonBody)
			req := &scheduling.InferenceRequest{Body: body}
			if err := p.RequestHeader(context.Background(), req); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if body.Mutated != tt.wantMutated {
				t.Errorf("Mutated = %v, want %v", body.Mutated, tt.wantMutated)
			}
			m, _ := body.Payload.AsMap()
			_, hasExt := m[BodyExtensionKey]
			if hasExt != tt.wantExt {
				t.Errorf("llmdext present = %v, want %v", hasExt, tt.wantExt)
			}
			if hasExt {
				if _, hasHints := m[BodyExtensionKey].(map[string]any)[BodyHintsKey]; hasHints {
					t.Errorf("agent_hints still present after strip")
				}
			}
			if _, ok := m["model"]; !ok {
				t.Errorf("unrelated top-level field was removed")
			}
			// Hints must have been read before stripping.
			if tt.wantMutated {
				checkInt(t, req, PriorityKey, ip(5))
			}
		})
	}
}

func TestPluginFactory(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		wantStrip bool
		wantErr   bool
	}{
		{name: "nil parameters", raw: "", wantStrip: false},
		{name: "empty object", raw: `{}`, wantStrip: false},
		{name: "stripFromBody true", raw: `{"stripFromBody": true}`, wantStrip: true},
		{name: "malformed parameters", raw: `{"stripFromBody": "yes"}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var dec *json.Decoder
			if tt.raw != "" {
				dec = json.NewDecoder(strings.NewReader(tt.raw))
			}
			pi, err := PluginFactory("my-hints", dec, nil)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			p := pi.(*Plugin)
			if p.stripFromBody != tt.wantStrip {
				t.Errorf("stripFromBody = %v, want %v", p.stripFromBody, tt.wantStrip)
			}
			if got := p.TypedName(); got.Type != PluginType || got.Name != "my-hints" {
				t.Errorf("TypedName = %+v", got)
			}
		})
	}
}

func TestProducesTypes(t *testing.T) {
	p := newPlugin(t, "")
	produces := p.Produces()
	if _, ok := produces[PriorityKey].(int); !ok {
		t.Errorf("PriorityKey type = %T, want int", produces[PriorityKey])
	}
	if _, ok := produces[StrictPriorityKey].(int); !ok {
		t.Errorf("StrictPriorityKey type = %T, want int", produces[StrictPriorityKey])
	}
	if _, ok := produces[OSLKey].(int); !ok {
		t.Errorf("OSLKey type = %T, want int", produces[OSLKey])
	}
	if _, ok := produces[SpeculativePrefillKey].(bool); !ok {
		t.Errorf("SpeculativePrefillKey type = %T, want bool", produces[SpeculativePrefillKey])
	}
}

func TestResolve_ReportsInvalidValues(t *testing.T) {
	var reported []string
	Resolve(
		map[string]string{metadata.RequestPriorityHeaderKey: "x"},
		map[string]any{bodyPriorityField: "y", bodyOSLField: -1.0},
		func(source, field, raw string, err error) {
			reported = append(reported, source+":"+field+"="+raw)
		},
	)
	wantReported := []string{
		"header:" + metadata.RequestPriorityHeaderKey + "=x",
		"body:" + bodyPriorityField + "=y",
		"body:" + bodyOSLField + "=-1",
	}
	if strings.Join(reported, ",") != strings.Join(wantReported, ",") {
		t.Errorf("reported = %v, want %v", reported, wantReported)
	}
}
