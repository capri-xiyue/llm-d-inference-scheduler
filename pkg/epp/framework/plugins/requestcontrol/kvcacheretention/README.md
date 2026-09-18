# KV-Cache Retention Plugin

The `kv-cache-retention` plugin annotates requests from multi-turn sessions
with retention directives for backends implementing the vLLM Context-Aware
KV-Cache Retention API
([vllm#37003](https://github.com/vllm-project/vllm/issues/37003),
[vllm#38514](https://github.com/vllm-project/vllm/pull/38514)). It is the
proof of concept for the timing component of SAECache
([arXiv:2605.18825](https://arxiv.org/pdf/2605.18825)): inter-turn intervals
of a multi-turn workload follow a log-normal distribution whose parameters
vary by deployment, so the retention window is learned online rather than
fixed.

## How it works

1. **Session identity.** The orchestrator labels each request with a session
   identifier and a workload type via headers. The plugin acts only on
   requests whose type matches `sessionType`; everything else passes through
   untouched.
2. **Online estimation.** For each matching session, the plugin measures the
   idle gap between one turn's response completion and the next turn's
   arrival. Gaps feed a log-normal fit: a sliding window over `ln(gap)`
   provides the maximum-likelihood sample estimate (mean and standard
   deviation), blended into the running parameters with an exponential moving
   average. Gaps below `minInterval` (timestamp-precision artifacts) and
   above `maxIdle` (session boundaries) are discarded.
3. **Directive injection.** Each matching request gets a single whole-prompt
   directive in the forwarded body:

   ```json
   {
     "retention_directives": [
       {"start": 0, "end": null, "priority": 70, "duration": 54.5}
     ],
     "retention_scope": "<session id>"
   }
   ```

   `duration` is the configured `quantile` of the fitted distribution,
   clamped to `[minRetention, maxRetention]`: the session's KV blocks stay
   protected while the next turn is likely to arrive and fall back to LRU
   once the session is far into the distribution's tail. Requests that
   already carry `retention_directives` are forwarded unchanged. Bodies that
   are not parsed JSON maps (raw or proto payloads) are forwarded unchanged,
   though their gaps still feed the estimator.

The fitted parameters, observation count, and current retention duration are
visible at `/debug/plugins/state`.

## Configuration

| Parameter | Default | Description |
|---|---|---|
| `sessionIDHeader` | `x-session-id` | Header carrying the session identifier. |
| `sessionTypeHeader` | `x-session-type` | Header carrying the workload type. |
| `sessionType` | `agentic` | Workload type to act on. Empty acts on every request with a session identifier. |
| `priority` | `70` | Eviction priority (0-100) written into the directive. |
| `quantile` | `0.9` | Quantile of the fitted distribution used as the retention duration. |
| `minRetention` | `1s` | Lower clamp on the retention duration. |
| `maxRetention` | `10m` | Upper clamp on the retention duration. |
| `initialLogMean` | `2.28` | Estimator seed, in log-seconds. The default is the CC-Bench agentic-trace fit from the SAECache paper. |
| `initialLogStd` | `1.34` | Estimator seed, in log-seconds. Same source as `initialLogMean`. |
| `emaFactor` | `0.1` | Blend weight of each new sample estimate. |
| `minSamples` | `20` | Observations required before the estimator updates. |
| `windowSize` | `200` | Sliding-window capacity of the sample estimate. |
| `minInterval` | `100ms` | Gaps below this are discarded. |
| `maxIdle` | `1h` | Gaps above this are discarded; idle sessions past this are swept. |
| `maxSessions` | `100000` | Soft cap on tracked sessions. |

## Example

**Location:** Top-level `plugins:` list in the `EndpointPickerConfig`.
**Enabled by default:** No. Add a `- type: kv-cache-retention` entry to
enable; the runner discovers its PreRequest and ResponseBody hooks and wires
them in.

```yaml
apiVersion: inference.networking.x-k8s.io/v1alpha1
kind: EndpointPickerConfig
plugins:
  - type: kv-cache-retention
    parameters:
      sessionIDHeader: x-session-id
      sessionType: agentic
      priority: 70
      quantile: 0.9
```

## Scope

This plugin covers the agentic timing model only. The remaining SAECache
components — per-queue weights, token-type weights, and positional decay for
structural reuse — depend on eviction feedback that the retention API does
not expose and are out of scope.
