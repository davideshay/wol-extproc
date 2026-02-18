// main.go (extended)
package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"os/exec"
	"os/signal"

	extproc "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type WoLExternalProcessor struct {
	extproc.UnimplementedExternalProcessorServer

	targetMAC   string
	targetIP    string
	broadcastIP string

	// Rate limiting: track last WoL send time per client
	mu       sync.RWMutex
	lastSent map[string]time.Time
	cooldown time.Duration

	// NEW: Idle shutdown tracking
	muActivity      sync.RWMutex
	lastActivityTS  time.Time // updated on every request
	shutdownTimer   *time.Timer
	shutdownTimeout time.Duration
	inShutdown      bool
}

// NewWoLExternalProcessor configures both WoL and idle shutdown
func NewWoLExternalProcessor(mac, ip, broadcast string, idleTimeout time.Duration) *WoLExternalProcessor {
	p := &WoLExternalProcessor{
		targetMAC:   mac,
		targetIP:    ip,
		broadcastIP: broadcast,
		lastSent:    make(map[string]time.Time),
		cooldown:    60 * time.Second,
		// Use provided idle timeout or default
		shutdownTimeout: idleTimeout,
		inShutdown:      false,
	}
	p.initShutdownTimer()
	return p
}

// NEW: Initialize shutdown timer (runs in background)
func (w *WoLExternalProcessor) initShutdownTimer() {
	w.lastActivityTS = time.Now() // Start active immediately
	w.shutdownTimer = time.AfterFunc(w.shutdownTimeout, func() {
		// This runs in its own goroutine — check activity again to be safe
		w.muActivity.RLock()
		last := w.lastActivityTS
		w.muActivity.RUnlock()
		if time.Since(last) >= w.shutdownTimeout && !w.inShutdown {
			w.triggerShutdown()
		}
	})
}

// NEW: Update activity timestamp — called on every request
func (w *WoLExternalProcessor) updateActivity() {
	w.muActivity.Lock()
	defer w.muActivity.Unlock()
	w.lastActivityTS = time.Now()

	// Reset the shutdown timer to fire again in 30m from now
	if w.shutdownTimer != nil {
		w.shutdownTimer.Reset(w.shutdownTimeout)
	}
}

// NEW: Safely trigger shutdown (idempotent)
func (w *WoLExternalProcessor) triggerShutdown() {
	w.muActivity.Lock()
	defer w.muActivity.Unlock()
	if w.inShutdown {
		return
	}
	w.inShutdown = true

	log.Println("⏳ No activity for", w.shutdownTimeout, "- initiating shutdown in 60 seconds...")

	// Optional: log to syslog/journald for visibility
	exec.Command("logger", "-t", "extproc-wol", "-p", "user.info",
		fmt.Sprintf("Ollama/OpenWebUI idle >%s — shutdown triggered", w.shutdownTimeout)).Run()

	// Grace period: delay shutdown to catch last requests
	go func() {
		time.Sleep(1 * time.Minute)
		log.Println("💥 Executing shutdown now...")
		exec.Command("systemctl", "poweroff").Run() // requires root + systemd
	}()
}

// Process — MAIN REQUEST HANDLER
func (w *WoLExternalProcessor) Process(stream extproc.ExternalProcessor_ProcessServer) error {
	ctx := stream.Context()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		req, err := stream.Recv()
		if err != nil {
			return err
		}

		// ✅ CRITICAL: Record activity on *every* request (including health checks)
		w.updateActivity()

		var resp *extproc.ProcessingResponse
		switch v := req.Request.(type) {
		case *extproc.ProcessingRequest_RequestHeaders:
			log.Println("Request intercepted - checking if WoL needed")

			// Get client IP for rate limiting
			clientIP := w.getClientIP(v.RequestHeaders)

			// Only trigger WoL for *non-healthcheck* requests
			// (prevents accidental cooldown resets from probes)
			if !w.isHealthCheck(v.RequestHeaders) {
				if w.shouldSendWoL(clientIP) {
					if err := w.sendWoL(); err != nil {
						log.Printf("Failed to send WoL: %v", err)
					} else {
						log.Printf("WoL packet sent to %s (triggered by %s)", w.targetMAC, clientIP)
						w.markWoLSent(clientIP)
					}
				} else {
					log.Printf("WoL cooldown active for client %s", clientIP)
				}
			} else {
				log.Println("Health check detected — WoL skipped, activity tracked")
			}

			resp = &extproc.ProcessingResponse{
				Response: &extproc.ProcessingResponse_RequestHeaders{
					RequestHeaders: &extproc.HeadersResponse{},
				},
			}
		case *extproc.ProcessingRequest_ResponseHeaders:
			resp = &extproc.ProcessingResponse{
				Response: &extproc.ProcessingResponse_ResponseHeaders{
					ResponseHeaders: &extproc.HeadersResponse{},
				},
			}
		default:
			log.Printf("Unhandled request type: %T", v)
			return status.Error(codes.Unimplemented, "request type not supported")
		}

		if err := stream.Send(resp); err != nil {
			return err
		}
	}
}

// NEW: Detect common healthcheck paths (customize as needed)
func (w *WoLExternalProcessor) isHealthCheck(headers *extproc.HttpHeaders) bool {
	for _, h := range headers.Headers.Headers {
		if h.Key == ":path" {
			path := string(h.RawValue)
			return path == "/health" ||
				path == "/ready" ||
				path == "/healthz" ||
				path == "/api/health" ||
				path == "/health" // OpenWebUI uses /health
		}
	}
	return false
}

// --- YOUR EXISTING FUNCTIONS (UNMODIFIED) ---

func (w *WoLExternalProcessor) getClientIP(headers *extproc.HttpHeaders) string {
	for _, header := range headers.Headers.Headers {
		if header.Key == "x-forwarded-for" {
			return string(header.RawValue)
		}
	}
	return "unknown"
}

func (w *WoLExternalProcessor) shouldSendWoL(clientIP string) bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	lastSent, exists := w.lastSent[clientIP]
	if !exists {
		return true
	}
	return time.Since(lastSent) > w.cooldown
}

func (w *WoLExternalProcessor) markWoLSent(clientIP string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.lastSent[clientIP] = time.Now()
}

func (w *WoLExternalProcessor) sendWoL() error {
	mac, err := net.ParseMAC(w.targetMAC)
	if err != nil {
		return fmt.Errorf("invalid MAC address: %w", err)
	}
	magicPacket := make([]byte, 102)
	for i := 0; i < 6; i++ {
		magicPacket[i] = 0xFF
	}
	for i := 0; i < 16; i++ {
		copy(magicPacket[6+i*6:], mac)
	}
	addr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:9", w.broadcastIP))
	if err != nil {
		return fmt.Errorf("failed to resolve broadcast: %w", err)
	}
	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		return fmt.Errorf("failed to create UDP: %w", err)
	}
	defer conn.Close()
	_, err = conn.Write(magicPacket)
	if err != nil {
		return fmt.Errorf("failed to send magic packet: %w", err)
	}
	return nil
}

func main() {
	targetMAC := os.Getenv("TARGET_MAC")
	if targetMAC == "" {
		targetMAC = "AA:BB:CC:DD:EE:FF"
	}
	targetIP := os.Getenv("TARGET_IP")
	if targetIP == "" {
		targetIP = "192.168.1.100"
	}
	broadcastIP := os.Getenv("BROADCAST_IP")
	if broadcastIP == "" {
		broadcastIP = "192.168.1.255"
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "9002"
	}

	log.Printf("Starting WoL External Processor with Idle Shutdown")
	log.Printf("Target MAC: %s", targetMAC)
	log.Printf("Target IP: %s", targetIP)
	log.Printf("Broadcast IP: %s", broadcastIP)
	log.Printf("Idle shutdown timeout: %v", 30*time.Minute)
	log.Printf("Listening on port: %s", port)

	lis, err := net.Listen("tcp", ":"+port)
	if err != nil {
		log.Fatalf("Failed to listen: %v", err)
	}

	// Parse IDLE_TIMEOUT env var (e.g., "30m", "1h", "90s")
	idleTimeout := 30 * time.Minute // default
	if val := os.Getenv("IDLE_TIMEOUT"); val != "" {
		if d, err := time.ParseDuration(val); err == nil && d > 0 {
			idleTimeout = d
			log.Printf("Idle shutdown timeout: %v (from IDLE_TIMEOUT)", idleTimeout)
		} else {
			log.Printf("⚠️ Invalid IDLE_TIMEOUT '%s', using default: %v", val, idleTimeout)
		}
	} else {
		log.Printf("Idle shutdown timeout: %v (default)", idleTimeout)
	}

	grpcServer := grpc.NewServer()
	processor := NewWoLExternalProcessor(targetMAC, targetIP, broadcastIP, idleTimeout)
	extproc.RegisterExternalProcessorServer(grpcServer, processor)

	// Set up graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-sigCh
		log.Println("Received shutdown signal — stopping timers")
		if processor.shutdownTimer != nil {
			processor.shutdownTimer.Stop()
		}
		log.Println("Stopping gracefully...")
		grpcServer.GracefulStop()
		os.Exit(0)
	}()

	log.Printf("Server listening on %s", lis.Addr())
	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("Failed to serve: %v", err)
	}
}
