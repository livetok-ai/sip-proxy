package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/pion/rtp"
	sdp "github.com/pion/sdp/v3"
)

const (
	// SIP_DEBUG enables detailed logging of all SIP messages (sent and received)
	SIP_DEBUG = true
	// RTP_DEBUG enables detailed logging of all RTP packets (sent and received)
	RTP_DEBUG = false
)

// MediaHandler is an interface for different media handlers (WebRTC, Gemini, etc.)
type MediaHandler interface {
	Close() error
}

// Session represents an active SIP session
type Session struct {
	CallID         string
	From           string
	To             string
	CreatedAt      time.Time
	LastActivity   time.Time
	MediaBridge    MediaBridge
	MediaHandler   MediaHandler // Can be either WebRTCHandler or GeminiHandler
	SIPParticipant *SIPParticipant
	RTPPort        int
	rtpConn        *net.UDPConn
	remoteRTPAddr  *net.UDPAddr
	stopRTP        chan struct{}
	supportsPCMU   bool
	supportsPCMA   bool
	selectedCodec  string // "PCMU" or "PCMA"
	// RTP state for outgoing packets
	rtpSequence     uint16
	rtpTimestamp    uint32
	rtpSSRC         uint32
	rtpLastSendTime time.Time
	rtpStateMux     sync.Mutex
	rtpPacketQueue  chan []byte // Queue for outgoing RTP packets (PCM data)
	stopRTPSender   chan struct{}
}

// SIPParticipant represents a SIP participant in the media bridge
// It wraps a Session to provide QueueFlusher functionality
type SIPParticipant struct {
	id      string
	writer  io.Writer
	session *Session
}

// NewSIPParticipant creates a new SIP participant
func NewSIPParticipant(id string, writer io.Writer, session *Session) *SIPParticipant {
	return &SIPParticipant{
		id:      id,
		writer:  writer,
		session: session,
	}
}

// ID returns the participant's unique identifier
func (p *SIPParticipant) ID() string {
	return p.id
}

// Writer returns the io.Writer for this participant
func (p *SIPParticipant) Writer() io.Writer {
	return p.writer
}

// SetWriter updates the writer for this participant
func (p *SIPParticipant) SetWriter(writer io.Writer) {
	p.writer = writer
}

// FlushQueue implements QueueFlusher interface - delegates to the Session
func (p *SIPParticipant) FlushQueue() {
	if p.session != nil {
		p.session.FlushQueue()
	}
}

// SIPRTPWriter implements io.Writer to send RTP packets back to SIP client
type SIPRTPWriter struct {
	session *Session
}

// Write enqueues PCM audio data to be sent as RTP packets
// Expects p to contain raw PCM audio data (16-bit LPCM)
func (w *SIPRTPWriter) Write(p []byte) (n int, err error) {
	// Split the incoming audio into 20ms chunks for RTP packetization
	// Audio is 16-bit PCM at 8000 Hz
	chunks := splitIntoChunks(p, 8000, 20)

	// Enqueue each chunk separately
	for _, chunk := range chunks {
		// Make a copy of the chunk since it might be reused
		chunkCopy := make([]byte, len(chunk))
		copy(chunkCopy, chunk)

		// Enqueue the PCM chunk for the packet sender goroutine to process
		select {
		case w.session.rtpPacketQueue <- chunkCopy:
			// Successfully enqueued
		default:
			// Queue is full, log and drop chunk
			slog.Warn("RTP queue full, dropping chunk",
				"event", "rtp_queue_full",
				"session", w.session.CallID,
				"chunk_size", len(chunk))
		}
	}

	// Return the total number of bytes processed
	return len(p), nil
}

// FlushQueue drains all pending RTP packets from the queue (called on interruption)
func (s *Session) FlushQueue() {
	flushedCount := 0
	// Drain the queue without blocking
	for {
		select {
		case <-s.rtpPacketQueue:
			flushedCount++
		default:
			// Queue is empty
			if flushedCount > 0 {
				slog.Info("RTP queue flushed",
					"event", "rtp_queue_flush",
					"session", s.CallID,
					"flushed_packets", flushedCount)
			}
			return
		}
	}
}

// rtpPacketSender runs in a separate goroutine to send RTP packets with proper pacing
func (s *Session) rtpPacketSender() {
	slog.Info("Starting RTP packet sender",
		"event", "rtp_sender_start",
		"session", s.CallID)

	for {
		select {
		case <-s.stopRTPSender:
			slog.Info("Stopping RTP packet sender",
				"event", "rtp_sender_stop",
				"session", s.CallID)
			return

		case pcmData := <-s.rtpPacketQueue:
			if s.remoteRTPAddr == nil {
				// Remote address not yet learned, skip
				continue
			}

			// Calculate packet duration from PCM data length
			// PCM is 16-bit (2 bytes per sample) at 8000 Hz
			samplesInChunk := len(pcmData) / 2
			packetDurationMs := float64(samplesInChunk) / 8.0 // 8000 Hz = 8 samples per ms

			// Encode PCM to G.711 using the selected codec
			g711Payload := encodeG711(pcmData, s.selectedCodec)
			if len(g711Payload) == 0 {
				slog.Error("Failed to encode PCM to G.711",
					"event", "encoding_failed",
					"session", s.CallID,
					"codec", s.selectedCodec)
				continue
			}

			// Determine payload type based on codec
			var payloadType uint8
			if s.selectedCodec == "PCMU" {
				payloadType = 0
			} else if s.selectedCodec == "PCMA" {
				payloadType = 8
			} else {
				slog.Error("Unknown codec",
					"event", "unknown_codec",
					"session", s.CallID,
					"codec", s.selectedCodec)
				continue
			}

			// Lock RTP state for thread-safe updates
			s.rtpStateMux.Lock()

			// Check timing - if last packet was sent too recently, wait to maintain proper pacing
			if !s.rtpLastSendTime.IsZero() {
				elapsed := time.Since(s.rtpLastSendTime)
				expectedDuration := time.Duration(packetDurationMs * float64(time.Millisecond))

				if elapsed < expectedDuration {
					waitTime := expectedDuration - elapsed
					s.rtpStateMux.Unlock() // Unlock while sleeping
					time.Sleep(waitTime)
					s.rtpStateMux.Lock() // Re-lock after sleep
				}
			}

			sequenceNumber := s.rtpSequence
			timestamp := s.rtpTimestamp
			ssrc := s.rtpSSRC

			// Increment sequence number for next packet
			s.rtpSequence++

			// Increment timestamp based on sample count (8000 Hz)
			s.rtpTimestamp += uint32(samplesInChunk)

			s.rtpStateMux.Unlock()

			// Create RTP packet
			rtpPacket := &rtp.Packet{
				Header: rtp.Header{
					Version:        2,
					Padding:        false,
					Extension:      false,
					Marker:         false,
					PayloadType:    payloadType,
					SequenceNumber: sequenceNumber,
					Timestamp:      timestamp,
					SSRC:           ssrc,
				},
				Payload: g711Payload,
			}

			// Marshal RTP packet to bytes
			rtpBytes, err := rtpPacket.Marshal()
			if err != nil {
				slog.Error("Failed to marshal RTP packet",
					"event", "marshal_failed",
					"session", s.CallID,
					"error", err.Error())
				continue
			}

			// Send RTP packet
			now := time.Now()
			_, err = s.rtpConn.WriteToUDP(rtpBytes, s.remoteRTPAddr)
			if err != nil {
				slog.Error("RTP send error",
					"event", "rtp_send_error",
					"session", s.CallID,
					"to", s.remoteRTPAddr.String(),
					"error", err.Error())
				continue
			}

			// Update last send time after successful send
			s.rtpStateMux.Lock()
			s.rtpLastSendTime = now
			s.rtpStateMux.Unlock()


			if RTP_DEBUG {
				slog.Info("RTP packet sent",
					"event", "rtp_send",
					"session", s.CallID,
					"to", s.remoteRTPAddr.String(),
					"seq", sequenceNumber,
					"timestamp", timestamp,
					"payload_bytes", len(g711Payload))
			}
		}
	}
}

// SIPServer handles SIP INVITE requests
type SIPServer struct {
	config         *Config
	userAgent      *sipgo.UserAgent
	server         *sipgo.Server
	httpClient     *http.Client
	sessions       map[string]*Session
	sessionsMux    sync.RWMutex
	stopCleanup    chan struct{}
	handlerFactory *MediaHandlerFactory
}

// InvitePayload represents the data sent to the callback URL
type InvitePayload struct {
	URI    string `json:"uri"`
	From   string `json:"from"`
	CallID string `json:"call_id"`
}

// SessionConfig represents the configuration returned from the callback URL
type SessionConfig struct {
	SystemInstructions string `json:"system_instructions"`
	Voice              string `json:"voice,omitempty"`
	Language           string `json:"language,omitempty"`
}

// parseSDP parses the SDP offer to detect supported codecs using pion/sdp
func parseSDP(sdpOffer string) (supportsPCMU bool, supportsPCMA bool) {
	var sd sdp.SessionDescription
	if err := sd.Unmarshal([]byte(sdpOffer)); err != nil {
		slog.Error("Failed to parse SDP offer",
			"event", "sdp_parse_error",
			"error", err.Error())
		return false, false
	}

	// Check all media descriptions for audio codecs
	for _, md := range sd.MediaDescriptions {
		// Only process audio media
		if md.MediaName.Media != "audio" {
			continue
		}

		// Check each payload format in the media description
		for _, format := range md.MediaName.Formats {
			// Parse payload type from format string
			var payloadType uint8
			if _, err := fmt.Sscanf(format, "%d", &payloadType); err != nil {
				continue
			}

			// Get the codec details from the session description
			codecInfo, err := sd.GetCodecForPayloadType(payloadType)
			if err != nil {
				continue
			}

			// Check for PCMU (G.711 μ-law) at 8000 Hz
			if codecInfo.Name == "PCMU" && codecInfo.ClockRate == 8000 {
				supportsPCMU = true
			}
			// Check for PCMA (G.711 A-law) at 8000 Hz
			if codecInfo.Name == "PCMA" && codecInfo.ClockRate == 8000 {
				supportsPCMA = true
			}
		}
	}

	return supportsPCMU, supportsPCMA
}


// NewSIPServer creates a new SIP server instance
func NewSIPServer(config *Config, factory *MediaHandlerFactory) (*SIPServer, error) {
	ua, err := sipgo.NewUA()
	if err != nil {
		return nil, fmt.Errorf("failed to create user agent: %w", err)
	}

	srv, err := sipgo.NewServer(ua)
	if err != nil {
		return nil, fmt.Errorf("failed to create server: %w", err)
	}

	s := &SIPServer{
		config:         config,
		userAgent:      ua,
		server:         srv,
		httpClient:     &http.Client{},
		sessions:       make(map[string]*Session),
		stopCleanup:    make(chan struct{}),
		handlerFactory: factory,
	}

	// Set up SIP message logging if debug is enabled
	if SIP_DEBUG {
		srv.ServeRequest(func(req *sip.Request) {
			slog.Debug("SIP message received",
				"direction", "recv",
				"type", "request",
				"method", string(req.Method),
				"body", req.String())
		})
	}

	return s, nil
}

// Start begins listening for SIP requests
func (s *SIPServer) Start() error {
	// Register INVITE handler
	s.server.OnInvite(s.handleInvite)

	// Register other handlers
	s.server.OnBye(s.handleBye)
	s.server.OnAck(s.handleAck)

	// Start session cleanup goroutine
	go s.cleanupInactiveSessions()

	// Start listening
	addr := fmt.Sprintf("0.0.0.0:%d", s.config.Port)
	if err := s.server.ListenAndServe(context.Background(), "udp", addr); err != nil {
		return fmt.Errorf("failed to start server: %w", err)
	}

	return nil
}

// Stop shuts down the SIP server
func (s *SIPServer) Stop() {
	close(s.stopCleanup)
	if s.server != nil {
		s.server.Close()
	}
	if s.userAgent != nil {
		s.userAgent.Close()
	}
}

// logSentMessage logs a sent SIP message if debug is enabled
func (s *SIPServer) logSentMessage(msg interface{}) {
	if SIP_DEBUG {
		if req, ok := msg.(*sip.Request); ok {
			slog.Debug("SIP message sent",
				"direction", "send",
				"type", "request",
				"method", string(req.Method),
				"body", req.String())
		} else if res, ok := msg.(*sip.Response); ok {
			slog.Debug("SIP message sent",
				"direction", "send",
				"type", "response",
				"status_code", res.StatusCode,
				"reason", res.Reason,
				"body", res.String())
		}
	}
}

// handleInvite processes incoming INVITE requests
func (s *SIPServer) handleInvite(req *sip.Request, tx sip.ServerTransaction) {
	slog.Info("Received INVITE",
		"from", req.From().Address.String(),
		"to", req.To().Address.String())

	// Send 100 Trying immediately
	tryingResp := sip.NewResponseFromRequest(req, sip.StatusTrying, "Trying", nil)
	s.logSentMessage(tryingResp)
	if err := tx.Respond(tryingResp); err != nil {
		slog.Error("Failed to send 100 Trying",
			"event", "sip_response_error",
			"error", err.Error())
	}

	// Extract information from the INVITE
	// Get the full URI from the From header
	fromURI := req.From().Address.String()

	// Get the Request URI
	requestURI := req.Recipient.String()

	// Get the Call-ID
	callID := req.CallID().Value()

	// Parse SDP offer to detect supported codecs
	var supportsPCMU, supportsPCMA bool
	if req.Body() != nil && len(req.Body()) > 0 {
		sdpOffer := string(req.Body())
		supportsPCMU, supportsPCMA = parseSDP(sdpOffer)
		slog.Info("SDP offer codec support",
			"event", "sdp_parsed",
			"pcmu", supportsPCMU,
			"pcma", supportsPCMA)
	} else {
		slog.Info("No SDP offer in INVITE",
			"event", "no_sdp_offer",
			"session", callID)
		// Default to PCMU if no SDP offer
		supportsPCMU = true
	}

	var sessionConfig *SessionConfig
	var err error

	// Make HTTP callback if URL is configured, otherwise use default
	if s.config.CallbackURL != "" {
		payload := InvitePayload{
			URI:    requestURI,
			From:   fromURI,
			CallID: callID,
		}

		sessionConfig, err = s.sendCallback(payload)
		if err != nil {
			slog.Error("Callback failed",
				"event", "callback_failed",
				"error", err.Error())
			// Send error response
			resp := sip.NewResponseFromRequest(req, sip.StatusServiceUnavailable, "Service Unavailable", nil)
			s.logSentMessage(resp)
			if err := tx.Respond(resp); err != nil {
				slog.Error("Failed to send error response",
					"event", "sip_response_error",
					"error", err.Error())
			}
			return
		}

		slog.Info("Callback successful, sending 200 OK",
			"event", "callback_success",
			"call_id", callID)
	} else {
		// Use default session configuration
		sessionConfig = &SessionConfig{
			SystemInstructions: "You are a helpful voice assistant. Be concise, friendly, and natural in your responses. Keep your answers brief and conversational, as this is a phone call.",
			Voice:              "Puck",
			Language:           "en-US",
		}
		slog.Info("Using default session configuration (no callback URL configured)",
			"event", "default_config_used",
			"call_id", callID)
	}

	// Create media bridge
	mediaBridge := NewMediaBridge()
	if err := mediaBridge.Start(); err != nil {
		slog.Error("Failed to start media bridge",
			"event", "media_bridge_start_error",
			"error", err.Error())
		resp := sip.NewResponseFromRequest(req, sip.StatusInternalServerError, "Internal Server Error", nil)
		s.logSentMessage(resp)
		if err := tx.Respond(resp); err != nil {
			slog.Error("Failed to send error response",
				"event", "sip_response_error",
				"error", err.Error())
		}
		return
	}

	// Create appropriate media handler using factory
	mediaHandler, err := s.handlerFactory.CreateHandler(mediaBridge, callID, sessionConfig)
	if err != nil {
		slog.Error("Failed to create media handler",
			"event", "media_handler_create_error",
			"error", err.Error())
		mediaBridge.Stop()
		resp := sip.NewResponseFromRequest(req, sip.StatusInternalServerError, "Internal Server Error", nil)
		s.logSentMessage(resp)
		if err := tx.Respond(resp); err != nil {
			slog.Error("Failed to send error response",
				"event", "sip_response_error",
				"error", err.Error())
		}
		return
	}

	// Create session (without SIPParticipant initially)
	now := time.Now()
	session := &Session{
		CallID:         callID,
		From:           req.From().Address.String(),
		To:             req.To().Address.String(),
		CreatedAt:      now,
		LastActivity:   now,
		MediaBridge:    mediaBridge,
		MediaHandler:   mediaHandler,
		SIPParticipant: nil, // Will be set below
		stopRTP:        make(chan struct{}),
		supportsPCMU:   supportsPCMU,
		supportsPCMA:   supportsPCMA,
		selectedCodec:  "", // Will be set when generating SDP answer
		// Initialize RTP state
		rtpSequence:    uint16(rand.Intn(65536)),      // Random initial sequence number
		rtpTimestamp:   uint32(rand.Intn(1000000000)), // Random initial timestamp
		rtpSSRC:        rand.Uint32(),                 // Random SSRC
		rtpPacketQueue: make(chan []byte, 10000),        // Buffered channel for outgoing packets
		stopRTPSender:  make(chan struct{}),
	}

	// Now create SIP participant that references the session
	sipParticipant := NewSIPParticipant("sip-"+callID, nil, session)
	session.SIPParticipant = sipParticipant

	// Start RTP listener for SIP (this will set session.RTPPort)
	if err := s.startRTPListener(session); err != nil {
		slog.Error("Failed to start RTP listener",
			"event", "rtp_listener_start_error",
			"error", err.Error())
		mediaBridge.Stop()
		mediaHandler.Close()
		resp := sip.NewResponseFromRequest(req, sip.StatusInternalServerError, "Internal Server Error", nil)
		s.logSentMessage(resp)
		if err := tx.Respond(resp); err != nil {
			slog.Error("Failed to send error response",
				"event", "sip_response_error",
				"error", err.Error())
		}
		return
	}

	// Set up the SIP participant writer to send packets back to SIP client
	sipWriter := &SIPRTPWriter{session: session}
	sipParticipant.SetWriter(sipWriter)

	// Add SIP participant to media bridge now that writer is set
	if err := mediaBridge.AddParticipant(sipParticipant); err != nil {
		slog.Error("Failed to add SIP participant",
			"event", "add_participant_error",
			"error", err.Error())
		s.cleanupSession(session)
		resp := sip.NewResponseFromRequest(req, sip.StatusInternalServerError, "Internal Server Error", nil)
		s.logSentMessage(resp)
		if err := tx.Respond(resp); err != nil {
			slog.Error("Failed to send error response",
				"event", "sip_response_error",
				"error", err.Error())
		}
		return
	}

	s.sessionsMux.Lock()
	s.sessions[callID] = session
	s.sessionsMux.Unlock()

	slog.Info("Session created",
		"event", "session_created",
		"call_id", callID,
		"total_sessions", len(s.sessions))

	// Start RTP packet sender goroutine
	go session.rtpPacketSender()

	// Start reading RTP packets in background
	go s.listenRTP(session)

	// Generate SDP answer using pion/sdp
	sessionID := time.Now().Unix()
	sessionVersion := sessionID
	rtpPort := session.RTPPort
	localIP := getPublicIP()

	// Create SDP session description
	sd := &sdp.SessionDescription{
		Version: 0,
		Origin: sdp.Origin{
			Username:       "-",
			SessionID:      uint64(sessionID),
			SessionVersion: uint64(sessionVersion),
			NetworkType:    "IN",
			AddressType:    "IP4",
			UnicastAddress: localIP,
		},
		SessionName: "SIP Proxy Session",
		ConnectionInformation: &sdp.ConnectionInformation{
			NetworkType: "IN",
			AddressType: "IP4",
			Address: &sdp.Address{
				Address: localIP,
			},
		},
		TimeDescriptions: []sdp.TimeDescription{
			{
				Timing: sdp.Timing{
					StartTime: 0,
					StopTime:  0,
				},
			},
		},
	}

	// Build media description with supported codecs
	var formats []string
	mediaDesc := &sdp.MediaDescription{
		MediaName: sdp.MediaName{
			Media:   "audio",
			Port:    sdp.RangedPort{Value: rtpPort},
			Protos:  []string{"RTP", "AVP"},
			Formats: []string{}, // Will be populated below
		},
		Attributes: []sdp.Attribute{
			{Key: "sendrecv"},
		},
	}

	// Prefer PCMU (payload type 0) if supported, otherwise use PCMA (payload type 8)
	if session.supportsPCMU {
		formats = append(formats, "0")
		mediaDesc = mediaDesc.WithCodec(0, "PCMU", 8000, 1, "")
		session.selectedCodec = "PCMU"
	}
	if session.supportsPCMA {
		formats = append(formats, "8")
		mediaDesc = mediaDesc.WithCodec(8, "PCMA", 8000, 1, "")
		if session.selectedCodec == "" {
			session.selectedCodec = "PCMA"
		}
	}

	// Fallback to PCMU if neither was detected
	if len(formats) == 0 {
		formats = append(formats, "0")
		mediaDesc = mediaDesc.WithCodec(0, "PCMU", 8000, 1, "")
		session.selectedCodec = "PCMU"
	}

	mediaDesc.MediaName.Formats = formats
	sd.MediaDescriptions = []*sdp.MediaDescription{mediaDesc}

	// Marshal to SDP string
	sdpBytes, err := sd.Marshal()
	if err != nil {
		slog.Error("Failed to marshal SDP answer",
			"event", "sdp_marshal_error",
			"session", callID,
			"error", err.Error())
		resp := sip.NewResponseFromRequest(req, sip.StatusInternalServerError, "Internal Server Error", nil)
		s.logSentMessage(resp)
		if err := tx.Respond(resp); err != nil {
			slog.Error("Failed to send error response",
				"event", "sip_response_error",
				"error", err.Error())
		}
		return
	}
	sdpAnswer := string(sdpBytes)

	slog.Info("Generated SDP answer",
		"event", "sdp_answer_generated",
		"session", callID,
		"codec", session.selectedCodec,
		"sdp", sdpAnswer)

	// Create 200 OK response with SDP body
	resp := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", []byte(sdpAnswer))

	// Add Content-Type header for SDP
	contentType := sip.ContentTypeHeader("application/sdp")
	resp.AppendHeader(&contentType)

	// Add Contact header using the To header as template
	if contact := req.Contact(); contact != nil {
		resp.AppendHeader(contact)
	}

	// Send response
	s.logSentMessage(resp)
	if err := tx.Respond(resp); err != nil {
		slog.Error("Failed to send 200 OK",
			"event", "sip_response_error",
			"error", err.Error())
	}
}

// handleBye processes BYE requests
func (s *SIPServer) handleBye(req *sip.Request, tx sip.ServerTransaction) {
	slog.Info("Received BYE",
		"event", "bye_received",
		"from", req.From().Address.String())

	// Remove session and cleanup resources
	callID := req.CallID().Value()
	s.sessionsMux.Lock()
	if session, exists := s.sessions[callID]; exists {
		s.cleanupSession(session)
		delete(s.sessions, callID)
		slog.Info("Removed session",
			"event", "session_removed",
			"call_id", callID,
			"total_sessions", len(s.sessions))
	}
	s.sessionsMux.Unlock()

	resp := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
	s.logSentMessage(resp)
	if err := tx.Respond(resp); err != nil {
		slog.Error("Failed to send BYE response",
			"event", "sip_response_error",
			"error", err.Error())
	}
}

// handleAck processes ACK requests
func (s *SIPServer) handleAck(req *sip.Request, tx sip.ServerTransaction) {
	slog.Info("Received ACK",
		"event", "ack_received",
		"from", req.From().Address.String())

	// Update last activity for the session
	callID := req.CallID().Value()
	s.sessionsMux.Lock()
	if session, exists := s.sessions[callID]; exists {
		session.LastActivity = time.Now()
	}
	s.sessionsMux.Unlock()
}

// cleanupInactiveSessions removes sessions that haven't had activity in 5 minutes
func (s *SIPServer) cleanupInactiveSessions() {
	ticker := time.NewTicker(30 * time.Second) // Check every 30 seconds
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			now := time.Now()
			timeout := 5 * time.Minute

			s.sessionsMux.Lock()
			for callID, session := range s.sessions {
				if now.Sub(session.LastActivity) > timeout {
					s.cleanupSession(session)
					delete(s.sessions, callID)
					slog.Info("Cleaned up inactive session",
						"event", "session_cleanup",
						"call_id", callID,
						"inactive_duration", now.Sub(session.LastActivity).String(),
						"total_sessions", len(s.sessions))
				}
			}
			s.sessionsMux.Unlock()

		case <-s.stopCleanup:
			slog.Info("Session cleanup goroutine stopped",
				"event", "cleanup_stopped")
			return
		}
	}
}

// sendCallback sends the INVITE information to the callback URL and returns the session config
func (s *SIPServer) sendCallback(payload InvitePayload) (*SessionConfig, error) {
	jsonData, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal payload: %w", err)
	}

	slog.Info("Sending callback",
		"event", "callback_send",
		"url", s.config.CallbackURL,
		"payload", string(jsonData))

	resp, err := s.httpClient.Post(s.config.CallbackURL, "application/json", bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("callback returned status %d", resp.StatusCode)
	}

	slog.Info("Callback response received",
		"event", "callback_response",
		"status", resp.StatusCode)

	// Read and log response body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	slog.Info("Callback response body",
		"event", "callback_response_body",
		"body", string(body))

	// Parse response into SessionConfig
	var config SessionConfig
	if err := json.Unmarshal(body, &config); err != nil {
		return nil, fmt.Errorf("failed to parse response body: %w", err)
	}

	slog.Info("Parsed session config",
		"event", "session_config_parsed",
		"system_instructions", config.SystemInstructions)

	return &config, nil
}

// startRTPListener creates and binds the UDP listener for RTP
func (s *SIPServer) startRTPListener(session *Session) error {
	// Listen on any available port (OS will assign one)
	addr := &net.UDPAddr{
		IP:   net.IPv4zero,
		Port: 0, // Let OS choose available port
	}

	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return fmt.Errorf("failed to create UDP listener: %w", err)
	}

	// Get the actual port assigned
	localAddr := conn.LocalAddr().(*net.UDPAddr)
	session.RTPPort = localAddr.Port
	session.rtpConn = conn

	slog.Info("RTP listener started",
		"event", "rtp_listener_started",
		"port", session.RTPPort,
		"session", session.CallID)

	return nil
}

// listenRTP reads RTP packets from UDP and broadcasts to other participants
func (s *SIPServer) listenRTP(session *Session) {
	buffer := make([]byte, 1500) // Standard MTU size for RTP packets

	slog.Info("Starting RTP packet reading",
		"event", "rtp_listener_start",
		"session", session.CallID,
		"rtp_port", session.RTPPort)

	for {
		select {
		case <-session.stopRTP:
			slog.Info("Stopping RTP listener",
				"event", "rtp_listener_stop",
				"session", session.CallID)
			return

		default:
			// Set read deadline to allow checking stopRTP channel
			session.rtpConn.SetReadDeadline(time.Now().Add(1 * time.Second))

			n, remoteAddr, err := session.rtpConn.ReadFromUDP(buffer)
			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					// Timeout is expected, continue to check stopRTP
					continue
				}
				slog.Error("Error reading RTP packet",
					"event", "rtp_read_error",
					"session", session.CallID,
					"error", err.Error())
				return
			}

			if n > 0 {
				now := time.Now()

				// Store remote RTP address from first packet (for sending back)
				if session.remoteRTPAddr == nil {
					session.remoteRTPAddr = remoteAddr
					slog.Info("Learned remote RTP address",
						"event", "rtp_remote_addr",
						"session", session.CallID,
						"address", remoteAddr.String())
				}

				// Update last activity timestamp
				session.LastActivity = now

				// Extract RTP payload and determine codec
				payloadType, rtpPayload, sequenceNumber, timestamp, err := extractRTPPayload(buffer[:n])
				if err != nil {
					slog.Error("Error extracting RTP payload",
						"event", "rtp_extract_error",
						"session", session.CallID,
						"error", err.Error())
					continue
				}
				if rtpPayload == nil || len(rtpPayload) == 0 {
					// No payload, skip silently
					continue
				}

				// Determine codec from payload type
				var codec string
				if payloadType == 0 {
					codec = "PCMU"
				} else if payloadType == 8 {
					codec = "PCMA"
				} else {
					slog.Warn("Unsupported RTP payload type",
						"event", "unsupported_payload_type",
						"payload_type", payloadType,
						"session", session.CallID)
					continue
				}

				if RTP_DEBUG {
					slog.Info("RTP packet received",
						"event", "rtp_recv",
						"session", session.CallID,
						"from", remoteAddr.String(),
						"seq", sequenceNumber,
						"timestamp", timestamp,
						"payload_bytes", len(rtpPayload))
				}

				// Decode G.711 to raw PCM
				pcmData := decodeG711(rtpPayload, codec)
				if len(pcmData) == 0 {
					slog.Error("Failed to decode G.711 audio",
						"event", "decode_error",
						"session", session.CallID)
					continue
				}

				// Broadcast PCM data to all other participants (excluding this SIP sender)
				chunk := &MediaChunk{
					Data:     pcmData,
					SenderID: session.SIPParticipant.ID(),
				}

				if err := session.MediaBridge.Broadcast(chunk); err != nil {
					slog.Error("Error broadcasting media chunk",
						"event", "broadcast_error",
						"session", session.CallID,
						"error", err.Error())
				}
			}
		}
	}
}

// cleanupSession closes all resources associated with a session
func (s *SIPServer) cleanupSession(session *Session) {
	// Stop RTP packet sender
	if session.stopRTPSender != nil {
		close(session.stopRTPSender)
	}

	// Stop RTP listener
	if session.stopRTP != nil {
		close(session.stopRTP)
	}

	// Close RTP connection
	if session.rtpConn != nil {
		session.rtpConn.Close()
		slog.Info("Closed RTP connection",
			"event", "rtp_connection_closed",
			"port", session.RTPPort,
			"session", session.CallID)
	}

	if session.MediaBridge != nil {
		session.MediaBridge.Stop()
	}
	if session.MediaHandler != nil {
		session.MediaHandler.Close()
	}
}
