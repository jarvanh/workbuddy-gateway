package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

func reviewLiveGateway(t *testing.T) {
	t.Helper()
	isolateAdminFeatures(t)
	cfg.Addr, cfg.WebUI, cfg.PortExplicit = "127.0.0.1", true, false
	cfg.Port, cfg.WebPort = freeLoopbackPort(t), freeLoopbackPort(t)
	servers, listeners, err := prepareGatewayServers()
	if err != nil {
		t.Fatal(err)
	}
	gatewayServers.register(servers, listeners)
	gatewayServers.start(make(chan error, 16))
	t.Cleanup(func() { gatewayServers.shutdown(context.Background()) })
}

func TestHotReloadRejectedPortKeepsDebugStateAndFile(t *testing.T) {
	reviewLiveGateway(t)
	if err := switchDebugLogging(false); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = switchDebugLogging(false) })
	before, err := os.ReadFile(runtimeConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	var doc map[string]any
	if err := json.Unmarshal(before, &doc); err != nil {
		t.Fatal(err)
	}
	apiPort, _ := currentGatewayPorts()
	doc["gateway"].(map[string]any)["apiPort"] = apiPort
	doc["gateway"].(map[string]any)["webPort"] = busy.Addr().(*net.TCPAddr).Port
	doc["debug"] = map[string]any{"enabled": true, "fullRequests": true}
	content, _ := json.Marshal(doc)
	w := adminRequest(newWebUIHandler(), http.MethodPost, "/admin/api/config-edit", "feature-admin-key",
		editorRequestBody(t, configRevision(before), string(content), false))
	if w.Code == http.StatusOK {
		t.Fatal("occupied port should reject the whole update")
	}
	after, _ := os.ReadFile(runtimeConfigFile)
	if !bytes.Equal(before, after) {
		t.Fatal("rejected update did not restore config.json")
	}
	if debugLoggingEnabled() {
		t.Fatal("rejected config enabled debug logging even though config.json was rolled back")
	}
}

func TestHotReloadDebugOpenFailureKeepsPortsKeysAndFile(t *testing.T) {
	reviewLiveGateway(t)
	if err := switchDebugLogging(false); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = switchDebugLogging(false) })
	before, err := os.ReadFile(runtimeConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	oldAPI, oldWeb := currentGatewayPorts()
	nextAPI, nextWeb := freeLoopbackPort(t), freeLoopbackPort(t)
	// A directory at the debug file path makes opening the sink fail on every OS.
	blocked := filepath.Join(logDir, "debug-"+time.Now().Format("2006-01-02")+".jsonl")
	if err := os.MkdirAll(blocked, 0700); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(before, &doc); err != nil {
		t.Fatal(err)
	}
	g := doc["gateway"].(map[string]any)
	g["apiPort"], g["webPort"] = nextAPI, nextWeb
	g["adminKey"] = "synthetic-rejected-admin-key"
	doc["debug"] = map[string]any{"enabled": true}
	content, _ := json.Marshal(doc)
	w := adminRequest(newWebUIHandler(), http.MethodPost, "/admin/api/config-edit", "feature-admin-key",
		editorRequestBody(t, configRevision(before), string(content), false))
	if w.Code == http.StatusOK {
		t.Fatal("debug sink open failure should reject the whole update")
	}
	after, err := os.ReadFile(runtimeConfigFile)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("failed debug sink creation did not preserve config.json")
	}
	if api, web := currentGatewayPorts(); api != oldAPI || web != oldWeb || debugLoggingEnabled() {
		t.Fatalf("rejected config changed runtime: api=%d web=%d debug=%t", api, web, debugLoggingEnabled())
	}
	for _, port := range []int{nextAPI, nextWeb} {
		listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprint(port)))
		if err != nil {
			t.Fatalf("staged listener %d was not released: %v", port, err)
		}
		listener.Close()
	}
	if w := adminRequest(newWebUIHandler(), http.MethodGet, "/admin/api/settings", "feature-admin-key", ""); w.Code != http.StatusOK {
		t.Fatal("rejected config changed the admin key")
	}
	for _, port := range []int{oldAPI, oldWeb} {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprint(port)), time.Second)
		if err != nil {
			t.Fatalf("old listener %d was lost: %v", port, err)
		}
		conn.Close()
	}
}

func TestHotReloadStatusReadsConcurrentWithPortChanges(t *testing.T) {
	reviewLiveGateway(t)
	if err := writeStatusSnapshot(); err != nil {
		t.Fatal(err)
	}
	handler := newWebUIHandler()
	started, stop := make(chan struct{}), make(chan struct{})
	var wg sync.WaitGroup
	var reads int
	var readErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		close(started)
		for {
			select {
			case <-stop:
				return
			default:
				w := adminRequest(handler, http.MethodGet, "/admin/api/status", "feature-admin-key", "")
				if w.Code != http.StatusOK {
					readErr = fmt.Errorf("concurrent status returned %d", w.Code)
					return
				}
				reads++
			}
		}
	}()
	<-started
	for i := 0; i < 6; i++ {
		apiPort, webPort := freeLoopbackPort(t), freeLoopbackPort(t)
		if err := saveGatewaySettings(nil, "", &apiPort, &webPort, "independent-review-concurrent-ports"); err != nil {
			close(stop)
			wg.Wait()
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
	if readErr != nil || reads == 0 {
		t.Fatalf("status reader did not complete successfully: reads=%d err=%v", reads, readErr)
	}
}

func TestHotReloadFileWatcherMovesListenersAndHonorsExplicitPort(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprintf("explicit_port_%t", explicit), func(t *testing.T) {
			reviewLiveGateway(t)
			cfg.PortExplicit = explicit
			oldAPI, oldWeb := currentGatewayPorts()
			before, err := os.ReadFile(runtimeConfigFile)
			if err != nil {
				t.Fatal(err)
			}
			var doc map[string]any
			if err := json.Unmarshal(before, &doc); err != nil {
				t.Fatal(err)
			}
			nextAPI, nextWeb := freeLoopbackPort(t), freeLoopbackPort(t)
			g := doc["gateway"].(map[string]any)
			g["apiPort"], g["webPort"] = nextAPI, nextWeb
			data, _ := json.Marshal(doc)
			if err := os.WriteFile(runtimeConfigFile, data, 0600); err != nil {
				t.Fatal(err)
			}
			watcher := runtimeConfigWatcher{}
			if !watcher.reload() {
				t.Fatalf("valid file edit was not applied: %v", watcher.lastError)
			}
			wantAPI := nextAPI
			if explicit {
				wantAPI = oldAPI
			}
			if api, web := currentGatewayPorts(); api != wantAPI || web != nextWeb {
				t.Fatalf("wrong listeners after file edit: api=%d web=%d", api, web)
			}
			client := &http.Client{Timeout: 2 * time.Second}
			response, err := client.Get("http://127.0.0.1:" + strconv.Itoa(wantAPI) + "/health")
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("new API health=%d", response.StatusCode)
			}
			if conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(oldWeb)), time.Second); err == nil {
				conn.Close()
				t.Fatal("file hot reload left the old Web listener open")
			}
			if watcher.reload() {
				t.Fatal("identical file should not be reapplied")
			}
		})
	}
}
