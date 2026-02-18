// main.go
package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"sync"
	"time"

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
}

func NewWoLExternalProcessor(mac, ip, broadcast string) *WoLExternalProcessor {
	return &WoLExternalProcessor{
		targetMAC:   mac,
		targetIP:    ip,
		broadcastIP: broadcast,
		lastSent:    make(map[string]time.Time),
		cooldown:    60 * time.Second, // Don't send WoL more than once per minute per client
	}
}

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

		var resp *extproc.ProcessingResponse

		switch v := req.Request.(type) {
		case *extproc.ProcessingRequest_RequestHeaders:
			log.Println("Request intercepted - checking if WoL needed")

			// Get client IP for rate limiting
			clientIP := w.getClientIP(v.RequestHeaders)

			// Check if we should send WoL
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

			// Continue processing the request normally
			resp = &extproc.ProcessingResponse{
				Response: &extproc.ProcessingResponse_RequestHeaders{
					RequestHeaders: &extproc.HeadersResponse{},
				},
			}

		case *extproc.ProcessingRequest_ResponseHeaders:
			// Just pass through response headers
			resp = &extproc.ProcessingResponse{
				Response: &extproc.ProcessingResponse_ResponseHeaders{
					ResponseHeaders: &extproc.HeadersResponse{},
				},
			}

		default:
			// Handle other request types if needed
			log.Printf("Unhandled request type: %T", v)
			return status.Error(codes.Unimplemented, "request type not supported")
		}

		if err := stream.Send(resp); err != nil {
			return err
		}
	}
}

func (w *WoLExternalProcessor) getClientIP(headers *extproc.HttpHeaders) string {
	// Look for X-Forwarded-For header
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
	// Parse MAC address
	mac, err := net.ParseMAC(w.targetMAC)
	if err != nil {
		return fmt.Errorf("invalid MAC address: %w", err)
	}

	// Create magic packet
	// 6 bytes of 0xFF followed by MAC address repeated 16 times
	magicPacket := make([]byte, 102)

	// Fill first 6 bytes with 0xFF
	for i := 0; i < 6; i++ {
		magicPacket[i] = 0xFF
	}

	// Repeat MAC address 16 times
	for i := 0; i < 16; i++ {
		copy(magicPacket[6+i*6:], mac)
	}

	// Send via UDP broadcast
	addr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:9", w.broadcastIP))
	if err != nil {
		return fmt.Errorf("failed to resolve broadcast address: %w", err)
	}

	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		return fmt.Errorf("failed to create UDP connection: %w", err)
	}
	defer conn.Close()

	_, err = conn.Write(magicPacket)
	if err != nil {
		return fmt.Errorf("failed to send magic packet: %w", err)
	}

	return nil
}

func main() {
	// Get configuration from environment variables
	targetMAC := os.Getenv("TARGET_MAC")
	if targetMAC == "" {
		targetMAC = "AA:BB:CC:DD:EE:FF" // Default for testing
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

	log.Printf("Starting WoL External Processor")
	log.Printf("Target MAC: %s", targetMAC)
	log.Printf("Target IP: %s", targetIP)
	log.Printf("Broadcast IP: %s", broadcastIP)
	log.Printf("Listening on port: %s", port)

	lis, err := net.Listen("tcp", fmt.Sprintf(":%s", port))
	if err != nil {
		log.Fatalf("Failed to listen: %v", err)
	}

	grpcServer := grpc.NewServer()
	processor := NewWoLExternalProcessor(targetMAC, targetIP, broadcastIP)
	extproc.RegisterExternalProcessorServer(grpcServer, processor)

	log.Printf("Server listening on %s", lis.Addr())
	if err := grpcServer.Serve(lis); err != nil {
		log.Fatalf("Failed to serve: %v", err)
	}
}
