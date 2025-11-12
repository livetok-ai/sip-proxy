package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/joho/godotenv"
)

/*
// Link soxr using pkg-config.
#cgo pkg-config: soxr
#include <stdlib.h>
#include <soxr.h>
*/
import "C"

func main() {
	// Load .env file if it exists
	if err := godotenv.Load(); err != nil {
		slog.Info("No .env file found or error loading it",
			"event", "dotenv_load",
			"error", err.Error())
	}

	port := flag.Int("port", 5060, "SIP server port")
	callbackURL := flag.String("callback-url", "", "HTTP callback URL for INVITE notifications (optional)")
	twilioPort := flag.Int("twilio-port", 8080, "Twilio webhook server port")
	sipUsername := flag.String("sip-username", "", "SIP username for Twilio")
	sipPassword := flag.String("sip-password", "", "SIP password for Twilio")
	publicIP := flag.String("public-ip", "", "Public IP address (defaults to first non-localhost interface)")
	sipURLArg := flag.String("sip-url", "", "SIP URL for Twilio (defaults to sip:PUBLIC_IP:5060)")
	twilioFrom := flag.String("twilio-from", "+1123456789", "Caller ID for Twilio")
	flag.Parse()

	// Determine public IP
	var actualPublicIP string
	if *publicIP != "" {
		actualPublicIP = *publicIP
	} else {
		actualPublicIP = getPublicIP()
	}

	// Determine SIP URL
	var sipURL string
	if *sipURLArg != "" {
		sipURL = *sipURLArg
	} else {
		sipURL = fmt.Sprintf("sip:%s:%d", actualPublicIP, *port)
	}

	config := &Config{
		Port:        *port,
		CallbackURL: *callbackURL,
	}

	// Create media handler factory
	factory := NewMediaHandlerFactory()

	// Initialize SIP server
	server, err := NewSIPServer(config, factory)
	if err != nil {
		slog.Error("Failed to create SIP server",
			"event", "server_create_error",
			"error", err.Error())
		os.Exit(1)
	}

	// Start the server
	if err := server.Start(); err != nil {
		slog.Error("Failed to start SIP server",
			"event", "server_start_error",
			"error", err.Error())
		os.Exit(1)
	}

	// Initialize and start Twilio webhook server
	twilioServer := NewTwilioServer(*twilioPort, *sipUsername, *sipPassword, sipURL, *twilioFrom)
	if err := twilioServer.Start(); err != nil {
		slog.Error("Failed to start Twilio server",
			"event", "twilio_server_start_error",
			"error", err.Error())
		os.Exit(1)
	}

	fmt.Printf("SIP Proxy listening on port %d\n", *port)
	if *callbackURL != "" {
		fmt.Printf("Callback URL: %s\n", *callbackURL)
	} else {
		fmt.Println("Callback URL: none (using default prompt)")
	}
	fmt.Printf("Twilio webhook server listening on port %d\n", *twilioPort)
	fmt.Printf("Public IP: %s\n", actualPublicIP)
	fmt.Printf("SIP URL: %s\n", sipURL)
	fmt.Println("Using Gemini handler")

	// Wait for interrupt signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	<-sigChan

	fmt.Println("\nShutting down server...")
	server.Stop()
}

// Config holds the server configuration
type Config struct {
	Port        int
	CallbackURL string
}

// MediaHandlerFactory creates media handlers
type MediaHandlerFactory struct {
}

// NewMediaHandlerFactory creates a new factory instance
func NewMediaHandlerFactory() *MediaHandlerFactory {
	return &MediaHandlerFactory{}
}

// CreateHandler creates a Gemini handler
func (f *MediaHandlerFactory) CreateHandler(mediaBridge MediaBridge, callID string, sessionConfig *SessionConfig) (MediaHandler, error) {
	// Use Gemini handler
	slog.Info("Creating Gemini handler for session",
		"event", "gemini_handler_create",
		"session", callID)
	geminiHandler, err := NewGeminiHandler(mediaBridge, "gemini-"+callID, sessionConfig)
	if err != nil {
		return nil, err
	}

	// Start Gemini processing
	if err := geminiHandler.Start(); err != nil {
		slog.Error("Failed to start Gemini handler",
			"event", "gemini_handler_start_error",
			"error", err.Error())
	}

	return geminiHandler, nil
}
