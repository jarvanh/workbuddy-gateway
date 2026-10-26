package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func readHTTPResponse(t *testing.T, reader *bufio.Reader) *http.Response {
	t.Helper()
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	return response
}

func TestGatewayPortsHotSwitchKeepsEstablishedConnection(t *testing.T) {
	isolatedGatewayConfig(t)
	oldCfg := cfg
	cfg.Addr, cfg.WebUI, cfg.PortExplicit = "127.0.0.1", true, false
	cfg.Port, cfg.WebPort = freeLoopbackPort(t), freeLoopbackPort(t)
	oldAPIPort := cfg.Port
	t.Cleanup(func() {
		gatewayServers.shutdown(context.Background())
		cfg = oldCfg
	})
	servers, listeners, err := prepareGatewayServers()
	if err != nil {
		t.Fatal(err)
	}
	gatewayServers.register(servers, listeners)
	errs := make(chan error, 4)
	gatewayServers.start(errs)

	oldConn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.Port)))
	if err != nil {
		t.Fatal(err)
	}
	defer oldConn.Close()
	reader := bufio.NewReader(oldConn)
	_, _ = fmt.Fprintf(oldConn, "GET /health HTTP/1.1\r\nHost: 127.0.0.1\r\nConnection: keep-alive\r\n\r\n")
	if response := readHTTPResponse(t, reader); response.StatusCode != http.StatusOK {
		t.Fatalf("old API health=%d", response.StatusCode)
	}

	nextAPI, nextWeb := freeLoopbackPort(t), freeLoopbackPort(t)
	actualAPI, actualWeb, err := gatewayServers.apply(gatewayFileConfig{APIPort: nextAPI, WebPort: nextWeb}, nextAPI, true)
	if err != nil || actualAPI != nextAPI || actualWeb != nextWeb {
		t.Fatalf("hot switch failed: api=%d web=%d err=%v", actualAPI, actualWeb, err)
	}
	cfg.Port, cfg.WebPort = actualAPI, actualWeb

	_, _ = fmt.Fprintf(oldConn, "GET /health HTTP/1.1\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n")
	if response := readHTTPResponse(t, reader); response.StatusCode != http.StatusOK {
		t.Fatalf("established connection was interrupted: %d", response.StatusCode)
	}
	for _, check := range []struct {
		port int
		path string
	}{{nextAPI, "/health"}, {nextWeb, "/ui/"}} {
		response, err := http.Get("http://127.0.0.1:" + strconv.Itoa(check.port) + check.path)
		if err != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("new listener %d%s failed: %v %v", check.port, check.path, err, response)
		}
		response.Body.Close()
	}
	if conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(oldAPIPort), 150*time.Millisecond); err == nil {
		conn.Close()
		t.Fatal("retired API listener still accepts new connections")
	}
	select {
	case err := <-errs:
		t.Fatal(err)
	default:
	}
}

func TestGatewayPortBindFailureKeepsOldListeners(t *testing.T) {
	isolatedGatewayConfig(t)
	oldCfg := cfg
	cfg.Addr, cfg.WebUI, cfg.PortExplicit = "127.0.0.1", true, false
	cfg.Port, cfg.WebPort = freeLoopbackPort(t), freeLoopbackPort(t)
	t.Cleanup(func() {
		gatewayServers.shutdown(context.Background())
		cfg = oldCfg
	})
	servers, listeners, err := prepareGatewayServers()
	if err != nil {
		t.Fatal(err)
	}
	gatewayServers.register(servers, listeners)
	gatewayServers.start(make(chan error, 2))
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	busyPort := busy.Addr().(*net.TCPAddr).Port
	nextAPI := freeLoopbackPort(t)
	if _, _, err := gatewayServers.apply(gatewayFileConfig{APIPort: nextAPI, WebPort: busyPort}, nextAPI, true); err == nil {
		t.Fatal("occupied target port accepted")
	}
	for _, port := range []int{cfg.Port, cfg.WebPort} {
		conn, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), time.Second)
		if err != nil {
			t.Fatalf("old listener %d was lost after failed switch: %v", port, err)
		}
		conn.Close()
	}
}

func TestGatewayPortSwapRejectedWithoutDroppingListeners(t *testing.T) {
	isolatedGatewayConfig(t)
	oldCfg := cfg
	cfg.Addr, cfg.WebUI, cfg.PortExplicit = "127.0.0.1", true, false
	cfg.Port, cfg.WebPort = freeLoopbackPort(t), freeLoopbackPort(t)
	t.Cleanup(func() {
		gatewayServers.shutdown(context.Background())
		cfg = oldCfg
	})
	servers, listeners, _ := prepareGatewayServers()
	gatewayServers.register(servers, listeners)
	gatewayServers.start(make(chan error, 2))
	if _, _, err := gatewayServers.apply(gatewayFileConfig{APIPort: cfg.WebPort, WebPort: cfg.Port}, cfg.WebPort, true); err == nil ||
		!strings.Contains(err.Error(), "互换") {
		t.Fatalf("port swap should be rejected clearly: %v", err)
	}
}

func TestGatewayHotSwitchPreservesInFlightSSE(t *testing.T) {
	isolatedGatewayConfig(t)
	oldCfg := cfg
	cfg.Addr, cfg.WebUI = "127.0.0.1", false
	t.Cleanup(func() { cfg = oldCfg })
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
			fmt.Fprint(w, "data: second\n\ndata: [DONE]\n\n")
		case <-r.Context().Done():
		}
	})}
	var manager gatewayServerManager
	manager.register([]*http.Server{server}, []net.Listener{listener})
	manager.start(make(chan error, 4))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		manager.shutdown(ctx)
	})
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Get("http://" + listener.Addr().String() + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	if first, err := reader.ReadString('\n'); err != nil || first != "data: first\n" {
		t.Fatalf("first SSE chunk=%q err=%v", first, err)
	}
	if blank, err := reader.ReadString('\n'); err != nil || blank != "\n" {
		t.Fatalf("first SSE frame incomplete: %q %v", blank, err)
	}
	nextAPI := freeLoopbackPort(t)
	if _, _, err := manager.apply(gatewayFileConfig{APIPort: nextAPI}, nextAPI, false); err != nil {
		t.Fatal(err)
	}
	close(release)
	rest, err := io.ReadAll(reader)
	if err != nil || string(rest) != "data: second\n\ndata: [DONE]\n\n" {
		t.Fatalf("hot switch interrupted SSE: %q err=%v", rest, err)
	}
	if conn, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second); err == nil {
		conn.Close()
		t.Fatal("retired listener still accepts new TCP connections")
	}
	health, err := client.Get("http://127.0.0.1:" + strconv.Itoa(nextAPI) + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer health.Body.Close()
	if health.StatusCode != http.StatusOK {
		t.Fatalf("new API health=%d", health.StatusCode)
	}
}
