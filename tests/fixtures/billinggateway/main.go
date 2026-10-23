// A local integration fixture: real Bifrost HTTP server/config store, fake local
// OpenAI upstream. It never loads production config or calls paid providers.
package main

import (
	"context"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"

	bifrost "github.com/maximhq/bifrost/core"
	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/transports/v2/bifrost-http/handlers"
	"github.com/maximhq/bifrost/transports/v2/bifrost-http/lib"
	bifrostserver "github.com/maximhq/bifrost/transports/v2/bifrost-http/server"
)

func main() {
	port := flag.Int("port", 18989, "loopback gateway port")
	upstreamPort := flag.Int("upstream-port", 18990, "loopback fake provider port")
	flag.Parse()
	root, err := filepath.Abs("tmp/billinggateway")
	if err != nil {
		log.Fatal(err)
	}
	if err = os.MkdirAll(root, 0700); err != nil {
		log.Fatal(err)
	}
	marker := filepath.Join(root, "fixture.marker")
	if _, err = os.Stat(filepath.Join(root, "config.json")); err == nil {
		if _, err = os.Stat(marker); err != nil {
			log.Fatal("refusing an app directory without the owned fixture marker")
		}
	}
	if err = os.WriteFile(marker, []byte("native billing gateway fixture\n"), 0600); err != nil {
		log.Fatal(err)
	}
	var requests atomic.Int64
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{map[string]any{"id": "fixture-model", "object": "model", "owned_by": "fixture"}}})
		case "/pricing.json":
			w.Write([]byte(`{}`))
		case "/__fixture/requests":
			json.NewEncoder(w).Encode(map[string]any{"inference_submits": requests.Load()})
		case "/v1/chat/completions":
			requests.Add(1)
			var body struct {
				Stream bool `json:"stream"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "invalid JSON", 400)
				return
			}
			if body.Stream {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"id\":\"chatcmpl-fixture\",\"object\":\"chat.completion.chunk\",\"model\":\"fixture-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"BILLING_FIXTURE_OK\"},\"finish_reason\":null}]}\n\n")
				fmt.Fprint(w, "data: {\"id\":\"chatcmpl-fixture\",\"object\":\"chat.completion.chunk\",\"model\":\"fixture-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":128,\"completion_tokens\":32,\"total_tokens\":160}}\n\ndata: [DONE]\n\n")
				return
			}
			fmt.Fprint(w, `{"id":"chatcmpl-fixture","object":"chat.completion","model":"fixture-model","choices":[{"index":0,"message":{"role":"assistant","content":"BILLING_FIXTURE_OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":128,"completion_tokens":32,"total_tokens":160}}`)
		case "/v1/responses":
			requests.Add(1)
			fmt.Fprint(w, `{"id":"resp-fixture","object":"response","model":"fixture-model","status":"completed","output":[{"id":"msg-fixture","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"BILLING_FIXTURE_OK","annotations":[]}]}],"usage":{"input_tokens":128,"output_tokens":32,"total_tokens":160}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	upstream.Listener.Close()
	upstream.Listener, err = net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(*upstreamPort))
	if err != nil {
		log.Fatal(err)
	}
	upstream.Start()
	defer upstream.Close()
	config := map[string]any{
		"$schema": "https://www.getbifrost.ai/schema",
		"version": 2, "env_label": "billing-fixture",
		"client":       map[string]any{"initial_pool_size": 16, "enable_logging": true, "oauth2_server_config": map[string]any{"issuer_url": "http://127.0.0.1:" + strconv.Itoa(*port)}},
		"config_store": map[string]any{"enabled": true, "type": "sqlite", "config": map[string]any{"path": filepath.Join(root, "config.db")}},
		"logs_store":   map[string]any{"enabled": true, "type": "sqlite", "config": map[string]any{"path": filepath.Join(root, "logs.db")}},
		"governance":   map[string]any{"auth_config": map[string]any{"is_enabled": true, "admin_username": "fixture-admin", "admin_password": "fixture-only-password"}},
		"framework":    map[string]any{"pricing": map[string]any{"pricing_url": upstream.URL + "/pricing.json", "pricing_sync_interval": 86400}},
		"providers":    map[string]any{"openai": map[string]any{"keys": []any{map[string]any{"name": "fixture-key", "value": "fixture-not-a-real-provider-key", "models": []string{"fixture-model"}, "weight": 1}}, "network_config": map[string]any{"base_url": upstream.URL, "allow_private_network": true, "max_retries": 0}}},
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		log.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "config.json"), data, 0600); err != nil {
		log.Fatal(err)
	}
	if err = os.Setenv("BIFROST_BILLING_RECEIPTS_ENABLED", "true"); err != nil {
		log.Fatal(err)
	}
	server := bifrostserver.NewBifrostHTTPServer("billing-fixture", embed.FS{})
	logger := bifrost.NewDefaultLogger(schemas.LogLevelError)
	lib.SetLogger(logger)
	bifrostserver.SetLogger(logger)
	handlers.SetLogger(logger)
	server.Host = "127.0.0.1"
	server.Port = strconv.Itoa(*port)
	server.AppDir = root
	server.LogLevel = "error"
	if err = server.Bootstrap(context.Background()); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("BILLING_FIXTURE_READY gateway=http://127.0.0.1:%d upstream=%s state=%s\n", *port, upstream.URL, root)
	if err = server.Start(); err != nil {
		log.Fatal(err)
	}
}
