package server

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"time"
)

const (
	requestFieldKVTransferParams = "kv_transfer_params"
)

// Server is the coordination service server.
type Server struct {
	inferenceGatewayURL string
	client              *http.Client
}

// New creates a new Server.
func New(inferenceGatewayURL string) *Server {
	return &Server{
		inferenceGatewayURL: inferenceGatewayURL,
		client: &http.Client{
			Timeout: 300 * time.Second,
		},
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 1. Parse the incoming request body.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", http.StatusInternalServerError)
		return
	}
	defer r.Body.Close()

	var completionRequest map[string]any
	if err := json.Unmarshal(body, &completionRequest); err != nil {
		http.Error(w, "Failed to unmarshal request body", http.StatusBadRequest)
		return
	}

	// 2. Send the request to the prefill worker.
	// Set kv_transfer_params for prefill, leaving other fields as they are.
	completionRequest[requestFieldKVTransferParams] = map[string]any{
		"do_remote_decode":  true,
		"do_remote_prefill": false,
		"remote_engine_id":  nil,
		"remote_block_ids":  nil,
		"remote_host":       nil,
		"remote_port":       nil,
	}

	prefillBody, err := json.Marshal(completionRequest)
	if err != nil {
		http.Error(w, "Failed to marshal prefill request", http.StatusInternalServerError)
		return
	}

	prefillReq, err := http.NewRequest(http.MethodPost, s.inferenceGatewayURL, bytes.NewReader(prefillBody))
	if err != nil {
		http.Error(w, "Failed to create prefill request", http.StatusInternalServerError)
		return
	}
	prefillReq.Header.Set("Content-Type", "application/json")
	prefillReq.Header.Set("X-Inference-Type", "prefill")

	prefillResp, err := s.client.Do(prefillReq)
	if err != nil {
		http.Error(w, "Failed to send prefill request", http.StatusInternalServerError)
		return
	}
	defer prefillResp.Body.Close()

	if prefillResp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(prefillResp.Body)
		log.Printf("Prefill request failed with status %d: %s", prefillResp.StatusCode, string(respBody))
		http.Error(w, "Prefill request failed", prefillResp.StatusCode)
		return
	}

	// 3. Get the prefill response. This should be a single JSON object.
	var prefillResult map[string]interface{}
	if err := json.NewDecoder(prefillResp.Body).Decode(&prefillResult); err != nil {
		http.Error(w, "Failed to decode prefill response", http.StatusInternalServerError)
		return
	}

	pKVTransferParams, ok := prefillResult[requestFieldKVTransferParams]
	if !ok {
		log.Printf("Warning: missing 'kv_transfer_params' field in prefiller response")
	}

	// 4. Send the request to the decode worker.
	// Update the request with the kv_transfer_params from the prefill response.
	completionRequest[requestFieldKVTransferParams] = pKVTransferParams

	decodeBody, err := json.Marshal(completionRequest)
	if err != nil {
		http.Error(w, "Failed to marshal decode request", http.StatusInternalServerError)
		return
	}

	decodeReq, err := http.NewRequest(http.MethodPost, s.inferenceGatewayURL, bytes.NewReader(decodeBody))
	if err != nil {
		http.Error(w, "Failed to create decode request", http.StatusInternalServerError)
		return
	}
	decodeReq.Header.Set("Content-Type", "application/json")
	decodeReq.Header.Set("X-Inference-Type", "decode")

	decodeResp, err := s.client.Do(decodeReq)
	if err != nil {
		http.Error(w, "Failed to send decode request", http.StatusInternalServerError)
		return
	}
	defer decodeResp.Body.Close()

	if decodeResp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(decodeResp.Body)
		log.Printf("Decode request failed with status %d: %s", decodeResp.StatusCode, string(respBody))
		http.Error(w, "Decode request failed", decodeResp.StatusCode)
		return
	}

	// 5. Stream the decode response back to the client.
	w.Header().Set("Content-Type", "application/json")
	if _, err := io.Copy(w, decodeResp.Body); err != nil {
		log.Printf("Failed to stream decode response: %v", err)
	}
}
