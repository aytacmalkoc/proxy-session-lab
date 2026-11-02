package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestNDJSONReadyAndConcurrentResults(t *testing.T) {
	input := `{"id":"one","url":"https://invalid.test","profile":"bad","timeout_seconds":1}` + "\n" + `{"id":"two","url":"https://invalid.test","profile":"bad","timeout_seconds":1}` + "\n"
	var out bytes.Buffer
	if err := serve(context.Background(), strings.NewReader(input), &out, 2); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("responses %s", out.String())
	}
	var ready map[string]any
	json.Unmarshal([]byte(lines[0]), &ready)
	if ready["protocol"] != float64(1) || ready["event"] != "ready" {
		t.Fatalf("ready %v", ready)
	}
	ids := map[string]bool{}
	for _, line := range lines[1:] {
		var r map[string]any
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		if r["failed_stage"] != "config" {
			t.Fatalf("response %v", r)
		}
		ids[r["id"].(string)] = true
	}
	if !ids["one"] || !ids["two"] {
		t.Fatalf("lost results %v", ids)
	}
}
func TestShutdownAndMalformedProtocolAreBounded(t *testing.T) {
	for _, input := range []string{"{\"id\":\"shutdown\",\"control\":\"shutdown\"}\n", "not json\n", "{}\n"} {
		var out bytes.Buffer
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		serve(ctx, strings.NewReader(input), &out, 1)
		cancel()
		if !strings.Contains(out.String(), "ready") {
			t.Fatal("missing handshake")
		}
	}
}
