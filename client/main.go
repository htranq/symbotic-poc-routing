package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"time"
)

func main() {
	clientID := "123"
	if len(os.Args) > 1 {
		agentID = os.Args[1]
	}

	target := "http://localhost:10000/join"
	if v := os.Getenv("ENVOY_URL"); v != "" {
		target = v
	}
	q := url.Values{"client_id": []string{clientID}}
	urlStr := target + "?" + q.Encode()

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(urlStr)
	if err != nil {
		log.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	fmt.Printf("status=%d body=%s\n", resp.StatusCode, string(body))
}
