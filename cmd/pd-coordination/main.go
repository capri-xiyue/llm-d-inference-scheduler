package main

import (
	"log"
	"net/http"
	"os"

	"github.com/llm-d/llm-d-inference-scheduler/pkg/pd-coordination/server"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	inferenceGatewayURL := os.Getenv("INFERENCE_GATEWAY_URL")
	if inferenceGatewayURL == "" {
		log.Fatal("INFERENCE_GATEWAY_URL environment variable is not set")
	}

	srv := server.New(inferenceGatewayURL)

	log.Printf("Starting server on port %s", port)
	if err := http.ListenAndServe(":"+port, srv); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}
