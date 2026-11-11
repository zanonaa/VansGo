package handlers

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/net/websocket"
)

// Muse Spark Web (meta.ai) fala o protocolo DGW sobre WebSocket: token RSC
// "ecto1:..." extraído da home autenticada, frames binários [type:1]
// [streamId:2 LE][len:3 LE] e um clippy.ecto.Request protobuf dentro de JSON
// {"req-id","payload"}. A API web não aceita tools nem system; o histórico
// dobra num prompt único e as tools voltam via shim de texto.
const (
	museSparkBaseURL    = "https://www.meta.ai"
	museSparkWSEndpoint = "wss://gateway.meta.ai/ws/clippy"
	museSparkAppID      = "1522763855472543"
	museSparkAppVersion = "1.0.0"
	museSparkAuthType   = "15:0"
	museSparkDGWVersion = "4321"
	museSparkConfigKey  = "5a5b-8d4e-f054-99ef-b2de-db02-0d05-52c7"
	museSparkPersonaID  = "867051314767696"
	museSparkClientName = "Abra Web Main Key"
	museSparkAppName    = "ECTO1"
	museSparkUA         = "Mozilla/5.0 (X11; Linux x86_64; rv:155.0) Gecko/20100101 Firefox/155.0"
)

// DGW frame types (codec 02h3llx9n0qf1.js).
const (
	museFrameEmpty       = 2
	museFrameDrain       = 3
	museFrameDeauth      = 4
	museFramePing        = 9
	museFramePong        = 10
	museFrameAck         = 12
	museFrameData        = 13
	museFrameEndOfData   = 14
	museFrameEstabStream = 15
)

func (t *TranslatorSend) SendMuseSparkWeb(w http.ResponseWriter, r *http.Request, provider, model string, body map[string]any, source string, stream bool) {
	t.sendMuseSparkWeb(w, r, provider, model, body, source, stream)
}

func (t *TranslatorSend) sendMuseSparkWeb(w http.ResponseWriter, r *http.Request, provider, model string, body map[string]any, source string, stream bool) {
	fail := func(status int, message string) {
		nodeReply(w, status, map[string]any{"success": false, "error": message})
	}
	connections, err := t.Connections.List(r.Context(), provider, nil)
	if err != nil {
		fail(500, err.Error())
		return
	}
	var connection map[string]any
	for _, candidate := range connections {
		if candidate["isActive"] != false {
			connection = candidate
			break
		}
	}
	if connection == nil {
		fail(400, "No active connection for provider: "+provider)
		return
	}
	usageSelected(r.Context(), connection)
	cookie := ttsString(firstNonEmpty(connection["apiKey"], connection["accessToken"]))
	if cookie == "" {
		fail(400, "Missing Meta AI cookie — paste the full Cookie header (needs ecto_1_sess) from www.meta.ai")
		return
	}

	intermediate := body
	if source != "openai" {
		if intermediate, err = translateSource(body, source); err != nil {
			fail(400, err.Error())
			return
		}
	}

	requestedTools, _ := intermediate["tools"].([]any)
	var toolNonce string
	var toolContract string
	if len(requestedTools) > 0 {
		toolNonce = newKimiWebNonce()
		toolContract = kimiWebToolContract(toolNonce, kimiWebToolLines(requestedTools))
	}
	prompt, systemPrompt, err := kimiWebFoldMessages(intermediate["messages"], toolContract)
	if err != nil {
		fail(400, err.Error())
		return
	}
	if prompt == "" {
		fail(400, "Meta AI requires a non-empty user message")
		return
	}
	if systemPrompt != "" {
		prompt = systemPrompt + "\n\n" + prompt
	}

	thinking := strings.Contains(model, "thinking") || strings.Contains(model, "contemplating")

	token, viewerID, err := museSparkToken(r.Context(), t.Client, cookie)
	if err != nil {
		fail(401, err.Error())
		return
	}

	reply, err := museSparkTurn(r.Context(), t.Client, cookie, token, viewerID, prompt, thinking)
	if err != nil {
		fail(502, "Meta AI error: "+err.Error())
		return
	}

	content := reply.text
	var toolCalls []any
	finish := "stop"
	if toolNonce != "" {
		var stripped string
		stripped, toolCalls = parseKimiWebToolCalls(content, toolNonce)
		content = strings.TrimSpace(stripped)
		if len(toolCalls) > 0 {
			finish = "tool_calls"
		}
	}
	if usage, ok := r.Context().Value(usageContextKey{}).(*requestUsage); ok && usage != nil {
		usage.addText(content)
		if len(toolCalls) > 0 {
			usage.addToolCalls(toolCalls)
		}
	}

	if !stream {
		message := map[string]any{"role": "assistant", "content": content}
		if reply.reasoning != "" {
			message["reasoning_content"] = reply.reasoning
		}
		if len(toolCalls) > 0 {
			message["tool_calls"] = openAIToolCalls(toolCalls)
		}
		nodeReply(w, 200, map[string]any{
			"id":      fmt.Sprintf("chatcmpl-muse-%d", time.Now().UnixNano()),
			"object":  "chat.completion",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}},
		})
		return
	}
	writeOpenAIDeltas(w, model, reply.reasoning, content, toolCalls, finish)
}

// museSparkReply acumula a resposta do turno.
type museSparkReply struct {
	text      string
	reasoning string
	final     bool
}

var museTokenPattern = regexp.MustCompile(`"accessToken":"(ecto1:[^"]{32,400})"`)

// museSparkToken busca a home autenticada e extrai o accessToken RSC + viewer.
func museSparkToken(ctx context.Context, client *http.Client, cookie string) (string, string, error) {
	if client == nil {
		client = http.DefaultClient
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, museSparkBaseURL+"/", nil)
	if err != nil {
		return "", "", err
	}
	museSparkBrowserHeaders(request, cookie)
	response, err := client.Do(request)
	if err != nil {
		return "", "", fmt.Errorf("meta.ai home fetch failed: %w", err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 6<<20))
	if err != nil {
		return "", "", err
	}
	if response.StatusCode != 200 {
		if response.StatusCode == 403 || response.StatusCode == 401 {
			return "", "", fmt.Errorf("meta.ai rejected the connection cookie (%d) — update it from an authenticated www.meta.ai session", response.StatusCode)
		}
		return "", "", fmt.Errorf("meta.ai home returned %d", response.StatusCode)
	}
	match := museTokenPattern.FindSubmatch(raw)
	if match == nil {
		return "", "", fmt.Errorf("meta.ai home has no accessToken — the ecto_1_sess cookie expired")
	}
	viewer := ""
	if hit := regexp.MustCompile(`"viewerId":"?(\d{4,})"?`).FindSubmatch(raw); hit != nil {
		viewer = string(hit[1])
	}
	return string(match[1]), viewer, nil
}

func museSparkBrowserHeaders(request *http.Request, cookie string) {
	request.Header.Set("User-Agent", museSparkUA)
	request.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	request.Header.Set("Accept-Language", "en-US,en;q=0.5")
	request.Header.Set("Cookie", cookie)
}

// museSparkTurn executa um turno DGW completo: connect, estab, send, leitura.
func museSparkTurn(ctx context.Context, client *http.Client, cookie, token, viewerID, prompt string, thinking bool) (*museSparkReply, error) {
	requestID := museUUID()
	conversationID := museUUID()
	turnID := museUUID()
	sessionID := museUUID()

	request, err := museSparkBuildRequest(requestID, conversationID, turnID, sessionID, viewerID, prompt, thinking)
	if err != nil {
		return nil, err
	}

	config, err := websocket.NewConfig(museSparkConnectURL(token, conversationID), "https://www.meta.ai")
	if err != nil {
		return nil, err
	}
	config.Header.Set("Cookie", cookie)
	config.Header.Set("User-Agent", museSparkUA)
	config.Header.Set("Accept-Language", "en-US,en;q=0.5")
	socket, err := websocket.DialConfig(config)
	if err != nil {
		return nil, fmt.Errorf("dgw websocket dial failed: %w", err)
	}
	defer socket.Close()
	socket.SetDeadline(time.Now().Add(150 * time.Second))

	// EstabStream (15) com headers JSON da conversa.
	estabParams, _ := json.Marshal(map[string]string{
		"x-ecto-conversation-id": conversationID,
		"client-payload-type":    "PROTO_INSIDE_JSON",
	})
	if _, err := socket.Write(dgwHeader(museFrameEstabStream, 1, len(estabParams))); err != nil {
		return nil, err
	}
	if _, err := socket.Write(estabParams); err != nil {
		return nil, err
	}

	// Data (13) com o payload {"req-id","payload":base64(proto)}.
	payload, _ := json.Marshal(map[string]string{"req-id": requestID, "payload": base64.StdEncoding.EncodeToString(request)})
	ackID := uint64(0)
	dataBody := append(dgwAckVarint(uint16(ackID)), payload...)
	if _, err := socket.Write(dgwHeader(museFrameData, 1, len(dataBody))); err != nil {
		return nil, err
	}
	if _, err := socket.Write(dataBody); err != nil {
		return nil, err
	}

	var reply museSparkReply
	acked := false
	established := false
	buffer := &museChunkBuffer{}
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		if err := socket.SetReadDeadline(deadline); err != nil {
			return nil, err
		}
		frameType, streamID, body, err := museReadFrame(socket)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, err
		}
		switch frameType {
		case museFramePing:
			if _, err := socket.Write([]byte{museFramePong, 0, 0, 1, 0, 0, 0}); err != nil {
				return nil, err
			}
		case museFrameEstabStream:
			established = true
		case museFrameAck:
			acked = true
		case museFrameData:
			// Server→client Data traz requiresAck no bit alto do primeiro
			// varint; confirma com Ack (12) no mesmo stream.
			if len(body) >= 1 {
				_, ackValue := museDecodeVarint(body)
				ackFrame := append([]byte{museFrameAck, byte(streamID), byte(streamID >> 8), 2, 0, 0}, dgwAckVarint(uint16(ackValue&0x7fff))...)
				if _, err := socket.Write(ackFrame); err != nil {
					return nil, err
				}
			}
		case museFrameDeauth:
			return nil, fmt.Errorf("dgw rejected the access token (Deauth)")
		case museFrameDrain:
			return nil, fmt.Errorf("dgw draining: retry the request")
		case museFrameEndOfData:
			if err := museCollect(buffer, &reply); err != nil {
				return nil, err
			}
			if reply.final {
				return &reply, nil
			}
		}
		if established && acked && reply.final {
			return &reply, nil
		}
	}
	if err := museCollect(buffer, &reply); err != nil {
		return nil, err
	}
	if reply.text == "" && reply.reasoning == "" {
		return nil, fmt.Errorf("dgw produced no response (acked=%v established=%v)", acked, established)
	}
	return &reply, nil
}

// museReadFrame lê um frame DGW completo do WebSocket.
func museReadFrame(socket *websocket.Conn) (byte, uint16, []byte, error) {
	header := make([]byte, 6)
	if _, err := io.ReadFull(socket, header); err != nil {
		return 0, 0, nil, err
	}
	frameType := header[0]
	streamID := uint16(header[1]) | uint16(header[2])<<8
	length := int(header[3]) | int(header[4])<<8 | int(header[5])<<16
	if length > 8<<20 {
		return 0, 0, nil, fmt.Errorf("dgw frame too large: %d", length)
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(socket, body); err != nil {
		return 0, 0, nil, err
	}
	return frameType, streamID, body, nil
}

func museDecodeVarint(data []byte) (int, uint64) {
	value := uint64(0)
	shift := uint(0)
	for i, b := range data {
		if i >= 10 {
			break
		}
		value |= uint64(b&0x7f) << shift
		if b&0x80 == 0 {
			return i + 1, value
		}
		shift += 7
	}
	return 0, value
}

// museChunkBuffer acumula os chunks JSON concatenados do stream.
type museChunkBuffer struct {
	pending []byte
}

func (b *museChunkBuffer) push(chunk []byte) {
	b.pending = append(b.pending, chunk...)
}

// takeJSON extrai o próximo objeto JSON balanceado (escape-aware).
func (b *museChunkBuffer) takeJSON() []byte {
	start := bytes.IndexByte(b.pending, '{')
	if start < 0 {
		return nil
	}
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(b.pending); i++ {
		c := b.pending[i]
		if inString {
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				object := b.pending[start : i+1]
				b.pending = b.pending[i+1:]
				return object
			}
		}
	}
	return nil
}

// museCollect processa os chunks pendentes e acumula a resposta.
func museCollect(buffer *museChunkBuffer, reply *museSparkReply) error {
	for {
		object := buffer.takeJSON()
		if object == nil {
			if utf8.Valid(buffer.pending) {
				return nil
			}
			// Aguardando mais bytes de um caractere multi-byte.
			return nil
		}
		var chunk struct {
			Piece struct {
				ModelResponse struct {
					RawChunk   string `json:"raw_chunk"`
					PpResponse string `json:"pp_response_so_far"`
					IsFinal    bool   `json:"is_final_chunk"`
				} `json:"model_response"`
				ReasoningResponse struct {
					Delta      string `json:"delta"`
					IsComplete bool   `json:"is_complete"`
				} `json:"reasoning_response"`
				ToolOutput struct {
					Text string `json:"text"`
				} `json:"tool_output"`
			} `json:"piece"`
			Error any  `json:"error"`
			Final bool `json:"is_final_chunk"`
		}
		if err := json.Unmarshal(object, &chunk); err != nil {
			continue
		}
		switch errorValue := chunk.Error.(type) {
		case string:
			if errorValue != "" {
				return fmt.Errorf("meta.ai: %s", errorValue)
			}
		case map[string]any:
			if message, _ := errorValue["message"].(string); message != "" {
				return fmt.Errorf("meta.ai: %s", message)
			}
		}
		if delta := chunk.Piece.ReasoningResponse.Delta; delta != "" {
			reply.reasoning += delta
		}
		if raw := chunk.Piece.ModelResponse.RawChunk; raw != "" {
			reply.text += raw
		}
		if pp := chunk.Piece.ModelResponse.PpResponse; pp != "" {
			reply.text = pp
		}
		if toolText := chunk.Piece.ToolOutput.Text; toolText != "" {
			reply.text += toolText
		}
		if chunk.Piece.ModelResponse.IsFinal || chunk.Final {
			reply.final = true
		}
	}
}

func museUUID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprintf("%08x-%04x-4%03x-9%03x-%012x", time.Now().UnixNano()&0xffffffff, 0, 0, 0, time.Now().UnixNano()&0xffffffffffff)
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16])
}

// museSparkConnectURL monta o URL DGW com todos os parâmetros do cliente web.
func museSparkConnectURL(token, conversationID string) string {
	deviceID := museUUID()
	params := url.Values{}
	params.Set("x-dgw-appid", museSparkAppID)
	params.Set("x-dgw-appversion", museSparkAppVersion)
	params.Set("x-dgw-authtype", museSparkAuthType)
	params.Set("x-dgw-version", museSparkDGWVersion)
	params.Set("x-dgw-uuid", "0")
	params.Set("x-dgw-tier", "prod")
	params.Set("x-dgw-deviceid", deviceID)
	params.Set("x-dgw-app-origin", "meta.ai")
	params.Set("x-dgw-app-clippy-request-id", museUUID())
	params.Set("Authorization", token)
	return museSparkWSEndpoint + "?" + params.Encode()
}

// museSparkBuildRequest codifica o clippy.ecto.Request com os campos do
// cliente web (builder módulo 501697). Schema: ecto_request_client.proto.
func museSparkBuildRequest(requestID, conversationID, turnID, sessionID, viewerID, prompt string, thinking bool) ([]byte, error) {
	userID := uint64(0)
	fmt.Sscanf(viewerID, "%d", &userID)

	now := uint64(time.Now().UnixMilli())
	uniqueMessageID := now

	// prompt.prompt_id.mid (MessageId 1=conv, 2=ts, 3=unique, 4=event)
	mid := pbBytes(2, concat(
		pbString(1, conversationID),
		pbUint(2, now),
		pbUint(3, uniqueMessageID),
	))
	// prompt.prompt_id (PromptId 1=identifier, 2=mid)
	promptID := concat(pbString(1, turnID), mid)
	// prompt (Prompt 1=prompt_id, 2=content)
	promptBytes := pbBytes(1, promptID)
	promptBytes = append(promptBytes, pbString(2, prompt)...)

	// metadata.product_context.*
	persona := pbBytes(8, concat(pbString(1, museSparkPersonaID), pbString(2, museSparkPersonaID)))
	productConfig := pbBytes(12, pbBytes(4, pbUint(1, 1))) // response_config.generate_conversation_title
	bloks := pbBytes(19, concat(pbString(1, "3c92f8af"), pbFloat(2, 2)))
	productContext := concat(
		pbString(1, "abra.chat"),
		pbString(2, museSparkAppID),
		pbString(3, museSparkAppVersion),
		pbString(4, museSparkConfigKey),
		pbBytes(5, pbBytes(5, pbString(1, conversationID))), // product_data.meta_ai_product_data.conversation_id
		pbUint(6, 5), // APP_TYPE_ECTO_1
		pbString(7, "HUMAN_AGENT"),
		persona,
		pbString(10, museSparkAppName),
		pbString(11, museSparkClientName),
		productConfig,
		pbString(13, "desktop"),
		pbString(15, museSparkUA),
		pbString(16, "desktop_web"),
		bloks,
	)

	// metadata.user_ids (UserIdentifiers 1=account, 2=app_account, 3=user_type)
	userIDs := pbBytes(2, concat(
		pbUint(1, userID),
		pbUint(2, userID),
		pbUint(3, 2), // USER_TYPE_EXTERNAL
	))
	traffic := pbBytes(3, pbUint(4, 1)) // is_dgw_request
	// metadata.conversation_context_ids (4=client_thread_id, 1=prompt_session, 3=qpl)
	contextIDs := pbBytes(10, concat(
		pbString(1, sessionID),
		pbString(3, museUUID()),
		pbString(4, conversationID),
	))
	thinkingFlag := byte(0)
	overrides := []byte{}
	if !thinking {
		overrides = pbBytes(7, pbUint(12, 0))
	}
	_ = thinkingFlag

	// capabilities: JSON_PATCH=3, PENDING_IMAGE=6 (+CLIENT_TOOL config)
	metadata := concat(
		pbBytes(1, productContext),
		userIDs,
		traffic,
		pbString(6, requestID),
		overrides,
		contextIDs,
		pbUint(20, 3),
		pbUint(20, 6),
		pbBytes(23, concat(pbUint(1, 5), pbString(3, "blocking_confirmation"))),
	)

	return concat(pbBytes(1, metadata), promptBytes), nil
}

func dgwHeader(frameType byte, streamID uint16, length int) []byte {
	return []byte{frameType, byte(streamID), byte(streamID >> 8), byte(length), byte(length >> 8), byte(length >> 16)}
}

func dgwAckVarint(ackID uint16) []byte {
	return pbVarint(uint64(ackID) | 0x8000)
}

func concat(parts ...[]byte) []byte {
	total := 0
	for _, part := range parts {
		total += len(part)
	}
	out := make([]byte, 0, total)
	for _, part := range parts {
		out = append(out, part...)
	}
	return out
}

func pbTag(field int, wire byte) []byte { return pbVarint(uint64(field)<<3 | uint64(wire)) }
func pbVarint(v uint64) []byte {
	var out []byte
	for {
		b := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			b |= 0x80
		}
		out = append(out, b)
		if v == 0 {
			return out
		}
	}
}
func pbBytes(field int, v []byte) []byte {
	out := pbTag(field, 2)
	out = append(out, pbVarint(uint64(len(v)))...)
	return append(out, v...)
}
func pbString(field int, v string) []byte { return pbBytes(field, []byte(v)) }
func pbUint(field int, v uint64) []byte {
	out := pbTag(field, 0)
	return append(out, pbVarint(v)...)
}
func pbFloat(field int, v float32) []byte {
	out := pbTag(field, 5)
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], math.Float32bits(v))
	return append(out, b[:]...)
}
