package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
	"github.com/pion/webrtc/v3/pkg/media/h264reader"
)

type Server struct {
	upgrader    websocket.Upgrader
	videoTrack  *webrtc.TrackLocalStaticSample
	clients     map[string]*Client
	clientsMu   sync.RWMutex
	videoConn   net.Conn
	controlConn net.Conn
	controlMu   sync.Mutex
}

type Client struct {
	id     string
	ws     *websocket.Conn
	pc     *webrtc.PeerConnection
	ctx    context.Context
	cancel context.CancelFunc
}

type TouchEvent struct {
	Type    string  `json:"type"`
	X       float64 `json:"x"`
	Y       float64 `json:"y"`
	Action  int     `json:"action"`
	Pointer int     `json:"pointer"`
}

type KeyEvent struct {
	Type   string `json:"type"`
	Key    int    `json:"key"`
	Action int    `json:"action"`
}

func NewServer() *Server {
	return &Server{
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool { return true },
		},
		clients: make(map[string]*Client),
	}
}

func (s *Server) Start() error {
	var err error
	s.videoTrack, err = webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264},
		"video", "scrcpy-video",
	)
	if err != nil {
		return fmt.Errorf("failed to create video track: %v", err)
	}

	// Setup HTTP routes
	http.HandleFunc("/", s.serveHome)
	http.HandleFunc("/ws", s.handleWebSocket)

	log.Println("Starting Scrcpy WebRTC Server...")
	log.Println("🎯 CORRECT ARCHITECTURE: Go server LISTENS for scrcpy-server connections")
	log.Println("")
	log.Println("📋 Setup Instructions:")
	log.Println("  1. adb forward tcp:27183 localabstract:scrcpy")
	log.Println("  2. adb forward tcp:27184 localabstract:scrcpy")
	log.Println("  3. Start this Go server (listening on localhost:27183, localhost:27184)")
	log.Println("  4. Start scrcpy-server - it will connect TO our listening servers")
	log.Println("")

	// Start listeners for scrcpy-server connections
	go s.startVideoListener()
	go s.startControlListener()

	log.Println("🌐 Web server starting on :8080")
	return http.ListenAndServe(":8080", nil)
}

func (s *Server) startVideoListener() {
	listener, err := net.Listen("tcp", "127.0.0.1:27183")
	if err != nil {
		log.Printf("❌ Failed to start video listener: %v", err)
		return
	}
	defer listener.Close()

	log.Println("🎬 Video listener started on 127.0.0.1:27183")
	log.Println("⏳ Waiting for scrcpy-server video connection...")

	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("❌ Video listener accept error: %v", err)
			continue
		}

		log.Println("✅ scrcpy-server video connected!")
		s.videoConn = conn

		// Start video processing
		go s.processVideoStream()
		break // Handle one connection for now
	}
}

func (s *Server) startControlListener() {
	listener, err := net.Listen("tcp", "127.0.0.1:27184")
	if err != nil {
		log.Printf("❌ Failed to start control listener: %v", err)
		return
	}
	defer listener.Close()

	log.Println("🎮 Control listener started on 127.0.0.1:27184")
	log.Println("⏳ Waiting for scrcpy-server control connection...")

	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("❌ Control listener accept error: %v", err)
			continue
		}

		log.Println("✅ scrcpy-server control connected!")
		s.controlConn = conn
		break // Handle one connection for now
	}
}

func (s *Server) processVideoStream() {
	if s.videoConn == nil {
		log.Printf("❌ No video connection available")
		return
	}

	defer func() {
		if s.videoConn != nil {
			s.videoConn.Close()
			s.videoConn = nil
		}
		log.Println("🔚 Video stream processing stopped")
	}()

	log.Println("🎬 Starting video stream processing...")

	// Set connection optimizations
	if tcpConn, ok := s.videoConn.(*net.TCPConn); ok {
		tcpConn.SetNoDelay(true)
		tcpConn.SetKeepAlive(true)
		tcpConn.SetKeepAlivePeriod(time.Second * 30)
	}

	// Create H264 reader
	h264Reader, err := h264reader.NewReader(s.videoConn)
	if err != nil {
		log.Printf("❌ Failed to create H264 reader: %v", err)
		return
	}

	log.Println("📺 H264 reader created, waiting for video frames...")

	frameCount := 0
	lastFrameTime := time.Now()

	for {
		// Set read timeout
		s.videoConn.SetReadDeadline(time.Now().Add(time.Second * 10))

		nal, err := h264Reader.NextNAL()
		s.videoConn.SetReadDeadline(time.Time{})

		if err != nil {
			if err == io.EOF {
				log.Printf("📺 Video stream ended normally")
				break
			} else if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				log.Printf("⏰ Video stream timeout - checking connection...")
				continue
			} else {
				log.Printf("❌ Error reading NAL: %v", err)
				break
			}
		}

		if nal == nil {
			continue
		}

		// Log first frame
		if frameCount == 0 {
			log.Printf("🎉 First video frame received! NAL type: %d, size: %d bytes", nal.UnitType, len(nal.Data))
		}

		// Add start code prefix and send to WebRTC
		nalWithStartCode := append([]byte{0x00, 0x00, 0x00, 0x01}, nal.Data...)

		sample := media.Sample{
			Data:     nalWithStartCode,
			Duration: time.Millisecond * 33, // ~30 FPS
		}

		if err := s.videoTrack.WriteSample(sample); err != nil {
			log.Printf("❌ Error writing sample: %v", err)
			continue
		}

		frameCount++
		now := time.Now()

		// Log frame rate periodically
		if frameCount%100 == 0 {
			fps := float64(100) / now.Sub(lastFrameTime).Seconds()
			log.Printf("📊 Processed %d video frames (%.1f fps)", frameCount, fps)
			lastFrameTime = now
		}
	}
}

func (s *Server) createPeerConnection() (*webrtc.PeerConnection, error) {
	// Create media engine
	m := &webrtc.MediaEngine{}

	// Register H264 codec
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeH264,
			ClockRate:   90000,
			Channels:    0,
			SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42001f",
		},
		PayloadType: 96,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		return nil, err
	}

	// Create API with media engine
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m))

	// Create peer connection with better ICE configuration
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
			{URLs: []string{"stun:stun1.l.google.com:19302"}},
		},
		ICETransportPolicy: webrtc.ICETransportPolicyAll,
		BundlePolicy:       webrtc.BundlePolicyMaxBundle,
		RTCPMuxPolicy:      webrtc.RTCPMuxPolicyRequire,
	}

	pc, err := api.NewPeerConnection(config)
	if err != nil {
		return nil, err
	}

	// Add video track
	if _, err := pc.AddTrack(s.videoTrack); err != nil {
		pc.Close()
		return nil, err
	}

	return pc, nil
}

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	ws, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade failed: %v", err)
		return
	}

	clientID := generateClientID()
	ctx, cancel := context.WithCancel(context.Background())

	pc, err := s.createPeerConnection()
	if err != nil {
		log.Printf("Failed to create peer connection: %v", err)
		ws.Close()
		return
	}

	client := &Client{
		id:     clientID,
		ws:     ws,
		pc:     pc,
		ctx:    ctx,
		cancel: cancel,
	}

	s.clientsMu.Lock()
	s.clients[clientID] = client
	s.clientsMu.Unlock()

	defer func() {
		s.clientsMu.Lock()
		delete(s.clients, clientID)
		s.clientsMu.Unlock()

		cancel()
		pc.Close()
		ws.Close()
		log.Printf("Client %s disconnected", clientID)
	}()

	log.Printf("Client %s connected", clientID)

	// Handle WebRTC signaling
	pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
		if candidate == nil {
			return
		}

		candidateJSON, _ := json.Marshal(candidate.ToJSON())
		message := map[string]interface{}{
			"type":      "ice-candidate",
			"candidate": json.RawMessage(candidateJSON),
		}

		if err := ws.WriteJSON(message); err != nil {
			log.Printf("Error sending ICE candidate: %v", err)
		}
	})

	// Handle incoming messages
	for {
		var message map[string]interface{}
		if err := ws.ReadJSON(&message); err != nil {
			log.Printf("Error reading message: %v", err)
			break
		}

		if err := s.handleMessage(client, message); err != nil {
			log.Printf("Error handling message: %v", err)
		}
	}
}

func (s *Server) handleMessage(client *Client, message map[string]interface{}) error {
	msgType, ok := message["type"].(string)
	if !ok {
		return fmt.Errorf("invalid message type")
	}

	switch msgType {
	case "offer":
		offer := webrtc.SessionDescription{
			Type: webrtc.SDPTypeOffer,
			SDP:  message["sdp"].(string),
		}

		if err := client.pc.SetRemoteDescription(offer); err != nil {
			return err
		}

		answer, err := client.pc.CreateAnswer(nil)
		if err != nil {
			return err
		}

		if err := client.pc.SetLocalDescription(answer); err != nil {
			return err
		}

		return client.ws.WriteJSON(map[string]interface{}{
			"type": "answer",
			"sdp":  answer.SDP,
		})

	case "ice-candidate":
		candidate := webrtc.ICECandidateInit{}
		candidateData, _ := message["candidate"].(map[string]interface{})
		candidateJSON, _ := json.Marshal(candidateData)
		json.Unmarshal(candidateJSON, &candidate)

		return client.pc.AddICECandidate(candidate)

	case "touch":
		return s.handleTouchEvent(message)

	case "key":
		return s.handleKeyEvent(message)

	default:
		return fmt.Errorf("unknown message type: %s", msgType)
	}
}

func (s *Server) handleTouchEvent(message map[string]interface{}) error {
	s.controlMu.Lock()
	defer s.controlMu.Unlock()

	if s.controlConn == nil {
		return fmt.Errorf("control connection not available")
	}

	x, _ := message["x"].(float64)
	y, _ := message["y"].(float64)
	action, _ := message["action"].(float64)
	pointer, _ := message["pointer"].(float64)

	// Convert to scrcpy control message format
	buf := make([]byte, 28)
	buf[0] = 2 // INJECT_TOUCH_EVENT

	// Pointer ID
	binary.BigEndian.PutUint64(buf[1:9], uint64(pointer))

	// Action (0=down, 1=up, 2=move)
	binary.BigEndian.PutUint32(buf[9:13], uint32(action))

	// Buttons (0 for touch)
	binary.BigEndian.PutUint32(buf[13:17], 0)

	// Position
	binary.BigEndian.PutUint32(buf[17:21], uint32(x))
	binary.BigEndian.PutUint32(buf[21:25], uint32(y))

	// Pressure (1.0 for touch)
	binary.BigEndian.PutUint16(buf[25:27], 0x3f80) // 1.0 in half-float

	// Buttons (0 for touch)
	buf[27] = 0

	// Set write timeout to avoid hanging
	s.controlConn.SetWriteDeadline(time.Now().Add(time.Second * 3))
	_, err := s.controlConn.Write(buf)
	if err != nil {
		log.Printf("🔴 Touch event failed: %v", err)
	}
	return err
}

func (s *Server) handleKeyEvent(message map[string]interface{}) error {
	s.controlMu.Lock()
	defer s.controlMu.Unlock()

	if s.controlConn == nil {
		return fmt.Errorf("control connection not available")
	}

	keyCode, _ := message["key"].(float64)
	action, _ := message["action"].(float64)

	// Convert to scrcpy control message format
	buf := make([]byte, 14)
	buf[0] = 1 // INJECT_KEYCODE

	// Action (0=down, 1=up)
	binary.BigEndian.PutUint32(buf[1:5], uint32(action))

	// Keycode
	binary.BigEndian.PutUint32(buf[5:9], uint32(keyCode))

	// Repeat count
	binary.BigEndian.PutUint32(buf[9:13], 0)

	// Meta state
	buf[13] = 0

	// Set write timeout
	s.controlConn.SetWriteDeadline(time.Now().Add(time.Second * 3))
	_, err := s.controlConn.Write(buf)
	if err != nil {
		log.Printf("🔴 Key event failed: %v", err)
	}
	return err
}

func (s *Server) serveHome(w http.ResponseWriter, r *http.Request) {
	html := `<!DOCTYPE html>
<html>
<head>
    <title>Scrcpy WebRTC</title>
    <style>
        body { 
            margin: 0; 
            padding: 20px; 
            font-family: Arial, sans-serif; 
            background: #f0f0f0;
        }
        .container { 
            max-width: 800px; 
            margin: 0 auto; 
            background: white; 
            padding: 20px; 
            border-radius: 10px; 
            box-shadow: 0 2px 10px rgba(0,0,0,0.1);
        }
        #video { 
            width: 100%; 
            height: auto; 
            border: 2px solid #ddd; 
            border-radius: 8px;
            background: black;
        }
        .controls { 
            margin-top: 20px; 
            text-align: center; 
        }
        button { 
            padding: 10px 20px; 
            margin: 5px; 
            border: none; 
            border-radius: 5px; 
            background: #007bff; 
            color: white; 
            cursor: pointer; 
        }
        button:hover { background: #0056b3; }
        button:disabled { background: #ccc; cursor: not-allowed; }
        .status { 
            margin-top: 10px; 
            padding: 10px; 
            border-radius: 5px; 
            text-align: center; 
        }
        .status.connected { background: #d4edda; color: #155724; }
        .status.disconnected { background: #f8d7da; color: #721c24; }
    </style>
</head>
<body>
    <div class="container">
        <h1>Scrcpy WebRTC Remote Control</h1>
        <video id="video" autoplay muted playsinline></video>
        <div class="controls">
            <button id="connectBtn" onclick="connect()">Connect</button>
            <button id="disconnectBtn" onclick="disconnect()" disabled>Disconnect</button>
        </div>
        <div id="status" class="status disconnected">Disconnected</div>
    </div>

    <script>
        let ws = null;
        let pc = null;
        let video = document.getElementById('video');
        let status = document.getElementById('status');
        let connectBtn = document.getElementById('connectBtn');
        let disconnectBtn = document.getElementById('disconnectBtn');

        function updateStatus(message, connected = false) {
            status.textContent = message;
            status.className = 'status ' + (connected ? 'connected' : 'disconnected');
            connectBtn.disabled = connected;
            disconnectBtn.disabled = !connected;
        }

        async function connect() {
            try {
                updateStatus('Connecting...');
                
                ws = new WebSocket('ws://' + window.location.host + '/ws');
                
                ws.onopen = async () => {
                    updateStatus('Creating WebRTC connection...');
                    await setupWebRTC();
                };
                
                ws.onclose = () => {
                    updateStatus('Disconnected');
                    cleanup();
                };
                
                ws.onerror = (error) => {
                    updateStatus('Connection error');
                    cleanup();
                };
                
                ws.onmessage = handleMessage;
                
            } catch (error) {
                updateStatus('Connection failed: ' + error.message);
                cleanup();
            }
        }

        async function setupWebRTC() {
            pc = new RTCPeerConnection({
                iceServers: [
                    { urls: 'stun:stun.l.google.com:19302' },
                    { urls: 'stun:stun1.l.google.com:19302' }
                ],
                iceCandidatePoolSize: 10
            });

            pc.ontrack = (event) => {
                console.log('Track received:', event);
                video.srcObject = event.streams[0];
                updateStatus('Connected and streaming', true);
            };

            pc.onicecandidate = (event) => {
                if (event.candidate) {
                    console.log('ICE candidate:', event.candidate);
                    ws.send(JSON.stringify({
                        type: 'ice-candidate',
                        candidate: event.candidate
                    }));
                }
            };

            pc.onconnectionstatechange = () => {
                console.log('Connection state:', pc.connectionState);
                if (pc.connectionState === 'failed') {
                    updateStatus('WebRTC connection failed');
                } else if (pc.connectionState === 'connected') {
                    updateStatus('WebRTC connected', true);
                }
            };

            pc.onicegatheringstatechange = () => {
                console.log('ICE gathering state:', pc.iceGatheringState);
            };

            // Create offer with more constraints
            const offer = await pc.createOffer({
                offerToReceiveVideo: true,
                offerToReceiveAudio: false
            });
            await pc.setLocalDescription(offer);
            
            console.log('Sending offer:', offer);
            ws.send(JSON.stringify({
                type: 'offer',
                sdp: offer.sdp
            }));
        }

        async function handleMessage(event) {
            const message = JSON.parse(event.data);
            
            switch (message.type) {
                case 'answer':
                    await pc.setRemoteDescription({
                        type: 'answer',
                        sdp: message.sdp
                    });
                    break;
                    
                case 'ice-candidate':
                    await pc.addIceCandidate(message.candidate);
                    break;
            }
        }

        function disconnect() {
            cleanup();
            updateStatus('Disconnected');
        }

        function cleanup() {
            if (pc) {
                pc.close();
                pc = null;
            }
            if (ws) {
                ws.close();
                ws = null;
            }
            video.srcObject = null;
        }

        // Touch/click handling
        video.addEventListener('click', (e) => {
            if (!ws || ws.readyState !== WebSocket.OPEN) return;
            
            const rect = video.getBoundingClientRect();
            const x = (e.clientX - rect.left) / rect.width * video.videoWidth;
            const y = (e.clientY - rect.top) / rect.height * video.videoHeight;
            
            // Send touch down
            ws.send(JSON.stringify({
                type: 'touch',
                x: x,
                y: y,
                action: 0,
                pointer: 0
            }));
            
            // Send touch up after short delay
            setTimeout(() => {
                ws.send(JSON.stringify({
                    type: 'touch',
                    x: x,
                    y: y,
                    action: 1,
                    pointer: 0
                }));
            }, 50);
        });

        // Keyboard handling
        document.addEventListener('keydown', (e) => {
            if (!ws || ws.readyState !== WebSocket.OPEN) return;
            
            ws.send(JSON.stringify({
                type: 'key',
                key: e.keyCode,
                action: 0
            }));
        });

        document.addEventListener('keyup', (e) => {
            if (!ws || ws.readyState !== WebSocket.OPEN) return;
            
            ws.send(JSON.stringify({
                type: 'key',
                key: e.keyCode,
                action: 1
            }));
        });
    </script>
</body>
</html>`

	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(html))
}

func generateClientID() string {
	return fmt.Sprintf("client_%d", time.Now().UnixNano())
}

func main() {
	server := NewServer()

	log.Println("Starting Scrcpy WebRTC Server...")
	log.Println("Make sure scrcpy-server is running with:")
	log.Println("  adb push scrcpy-server-v2.7.jar /data/local/tmp/scrcpy-server.jar")
	log.Println("  adb forward tcp:27183 localabstract:scrcpy")
	log.Println("  adb forward tcp:27184 localabstract:scrcpy")
	log.Println("  adb shell 'cd /data/local/tmp && CLASSPATH=scrcpy-server.jar app_process / com.genymobile.scrcpy.Server 2.7'")

	if err := server.Start(); err != nil {
		log.Fatal(err)
	}
}
