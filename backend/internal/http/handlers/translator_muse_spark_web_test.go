package handlers

import (
	"encoding/json"
	"reflect"
	"testing"
)

// O codec DGW (02h3llx9n0qf1.js) monta [type:1][streamId:2 LE][len:3 LE] e no
// Data/Ack acrescenta o varint 0x8000|ackId antes do payload.
func TestMuseSparkFrames(t *testing.T) {
	header := dgwHeader(museFrameData, 1, 300)
	want := []byte{13, 1, 0, 0x2c, 1, 0}
	if !reflect.DeepEqual(header, want) {
		t.Fatalf("dgwHeader = %v, want %v", header, want)
	}
	if got := dgwAckVarint(0); !reflect.DeepEqual(got, []byte{0x80, 0x80, 0x02}) {
		t.Fatalf("dgwAckVarint(0) = %v", got)
	}
	if got := dgwAckVarint(5); !reflect.DeepEqual(got, []byte{0x85, 0x80, 0x02}) {
		t.Fatalf("dgwAckVarint(5) = %v", got)
	}
}

// O request precisa decodificar de volta com os field numbers do schema
// clippy.ecto (ecto_request_client.proto).
func TestMuseSparkBuildRequestShape(t *testing.T) {
	body, err := museSparkBuildRequest("req-1", "conv-1", "turn-1", "sess-1", "123456789", "olá mundo", false)
	if err != nil {
		t.Fatal(err)
	}
	metadata, rest := museTestMessage(body, 1)
	if len(metadata) == 0 || len(rest) == 0 {
		t.Fatal("request sem metadata(1) ou prompt(2)")
	}
	productContext, _ := museTestMessage(metadata, 1)
	if len(productContext) == 0 {
		t.Fatal("metadata sem product_context(1)")
	}
	if got := museTestString(productContext, 2); got != museSparkAppID {
		t.Fatalf("app_id = %q", got)
	}
	if got := museTestString(productContext, 4); got != museSparkConfigKey {
		t.Fatalf("config_key = %q", got)
	}
	if museTestVarint(productContext, 6) != 5 {
		t.Fatal("app_type != APP_TYPE_ECTO_1(5)")
	}
	userIDs, _ := museTestMessage(metadata, 2)
	if museTestVarint(userIDs, 3) != 2 {
		t.Fatal("user_type != USER_TYPE_EXTERNAL(2)")
	}
	if museTestVarint(userIDs, 1) != 123456789 {
		t.Fatal("account_id incorreto")
	}
	if museTestVarint(metadata, 20) != 3 {
		t.Fatal("client_capabilities sem JSON_PATCH(3)")
	}
	toolConfig, _ := museTestMessage(metadata, 23)
	if museTestVarint(toolConfig, 1) != 5 || museTestString(toolConfig, 3) != "blocking_confirmation" {
		t.Fatal("client_capability_configs sem CLIENT_TOOL(5)/blocking_confirmation")
	}
	content, _ := museTestMessage(rest, 2)
	if string(content) != "olá mundo" {
		t.Fatalf("prompt content = %q", content)
	}
	promptID, _ := museTestMessage(rest, 1)
	if museTestString(promptID, 1) != "turn-1" {
		t.Fatal("prompt_id.identifier incorreto")
	}
}

// Chunks chegam concatenados e podem trazer { } dentro de strings com escape.
func TestMuseSparkTakeJSON(t *testing.T) {
	buffer := &museChunkBuffer{}
	buffer.push([]byte(`{"piece":{"model_response":{"raw_chunk":"a{b}\"c"}}}`))
	object := buffer.takeJSON()
	if object == nil {
		t.Fatal("takeJSON não extraiu o objeto completo")
	}
	var parsed map[string]any
	if err := json.Unmarshal(object, &parsed); err != nil {
		t.Fatalf("objeto extraído não é JSON válido: %v", err)
	}
	if len(buffer.pending) != 0 {
		t.Fatalf("buffer residual: %q", buffer.pending)
	}
	if buffer.takeJSON() != nil {
		t.Fatal("takeJSON devolveu objeto além do fim")
	}
	buffer.push([]byte(`{"a":1}{"b":2}`))
	first := buffer.takeJSON()
	if string(first) != `{"a":1}` || string(buffer.takeJSON()) != `{"b":2}` {
		t.Fatal("takeJSON não separa objetos concatenados")
	}
}

// Acúmulo da resposta: raw_chunk soma, pp_response_so_far substitui, final
// marca fim e error (string ou objeto) interrompe.
func TestMuseSparkCollect(t *testing.T) {
	reply := &museSparkReply{}
	buffer := &museChunkBuffer{}
	buffer.push([]byte(`{"piece":{"model_response":{"raw_chunk":"Hel"}}}`))
	buffer.push([]byte(`{"piece":{"model_response":{"raw_chunk":"lo"}}}`))
	if err := museCollect(buffer, reply); err != nil {
		t.Fatal(err)
	}
	if reply.text != "Hello" {
		t.Fatalf("text = %q", reply.text)
	}
	buffer.push([]byte(`{"piece":{"model_response":{"pp_response_so_far":"Hello!","is_final_chunk":true}}}`))
	if err := museCollect(buffer, reply); err != nil {
		t.Fatal(err)
	}
	if reply.text != "Hello!" || !reply.final {
		t.Fatalf("fold final falhou: text=%q final=%v", reply.text, reply.final)
	}
	if err := museCollect(&museChunkBuffer{pending: []byte(`{"error":"boom"}`)}, &museSparkReply{}); err == nil {
		t.Fatal("error string não retornou erro")
	}
	if err := museCollect(&museChunkBuffer{pending: []byte(`{"error":{"message":"kaput"}}`)}, &museSparkReply{}); err == nil {
		t.Fatal("error objeto não retornou erro")
	}
}

// ---- helpers de decode protobuf (suficientes p/ conferir o shape) ----

// museTestMessage devolve o corpo do primeiro campo length-delimited com o
// field number pedido e o resto do buffer.
func museTestMessage(data []byte, field int) ([]byte, []byte) {
	rest := data
	for len(rest) > 0 {
		tag, _, value, next, ok := museTestField(rest)
		if !ok {
			return nil, nil
		}
		if tag.number == field && tag.wire == 2 {
			return value, next
		}
		rest = next
	}
	return nil, nil
}

func museTestString(data []byte, field int) string {
	value, _ := museTestMessage(data, field)
	return string(value)
}

func museTestVarint(data []byte, field int) uint64 {
	rest := data
	for len(rest) > 0 {
		tag, value, _, next, ok := museTestField(rest)
		if !ok {
			return 0
		}
		if tag.number == field && tag.wire == 0 {
			return value
		}
		rest = next
	}
	return 0
}

type museTestTag struct {
	number int
	wire   byte
}

// museTestField decodifica um campo: devolve tag, valor varint (wire 0), bytes
// (wire 2) e o início do próximo campo.
func museTestField(data []byte) (museTestTag, uint64, []byte, []byte, bool) {
	pos := 0
	key, shift := uint64(0), uint(0)
	for pos < len(data) {
		b := data[pos]
		pos++
		key |= uint64(b&0x7f) << shift
		if b&0x80 == 0 {
			break
		}
		shift += 7
	}
	tag := museTestTag{number: int(key >> 3), wire: byte(key & 7)}
	switch tag.wire {
	case 0:
		value, shift := uint64(0), uint(0)
		for pos < len(data) {
			b := data[pos]
			pos++
			value |= uint64(b&0x7f) << shift
			if b&0x80 == 0 {
				return tag, value, nil, data[pos:], true
			}
			shift += 7
		}
	case 2:
		length, shift := 0, uint(0)
		for pos < len(data) {
			b := data[pos]
			pos++
			length |= int(b&0x7f) << shift
			if b&0x80 == 0 {
				break
			}
			shift += 7
		}
		if pos+length > len(data) {
			return tag, 0, nil, nil, false
		}
		return tag, 0, data[pos : pos+length], data[pos+length:], true
	}
	return tag, 0, nil, nil, false
}
