package handlers

import (
	"bufio"
	"encoding/json"
	"strings"
	"testing"
)

func TestPplxWebScanAskTextFragments(t *testing.T) {
	// Formato atual: intended_usage "ask_text", chunks fragmentados e snapshot
	// final em answer. Deve produzir o texto uma vez, sem duplicar.
	sse := strings.Join([]string{
		`data: {"blocks":[{"intended_usage":"ask_text","markdown_block":{"progress":"IN_PROGRESS","chunks":["PPL"]}}]}`,
		``,
		`data: {"blocks":[{"intended_usage":"ask_text","markdown_block":{"progress":"IN_PROGRESS","chunks":["X_OK"]}}]}`,
		``,
		`data: {"blocks":[{"intended_usage":"ask_text","markdown_block":{"progress":"DONE","chunks":["PPL","X_OK"],"answer":"PPLX_OK"}}]}`,
		`data: {}`,
		``,
	}, "\r\n")
	text, err := pplxWebAccumulate(bufio.NewReader(strings.NewReader(sse)))
	if err != nil || text != "PPLX_OK" {
		t.Fatalf("ask_text fragments: %q %v", text, err)
	}
}

func TestPplxWebScanCumulativeSnapshots(t *testing.T) {
	// Formato antigo: chunks cumulativos por evento; só o excedente é emitido.
	sse := strings.Join([]string{
		`data: {"blocks":[{"intended_usage":"markdown_block","markdown_block":{"progress":"IN_PROGRESS","chunks":["Hel"]}}]}`,
		`data: {"blocks":[{"intended_usage":"markdown_block","markdown_block":{"progress":"IN_PROGRESS","chunks":["Hello"]}}]}`,
		`data: {"blocks":[{"intended_usage":"markdown_block","markdown_block":{"progress":"DONE","chunks":["Hello"]}}]}`,
		``,
	}, "\r\n")
	text, err := pplxWebAccumulate(bufio.NewReader(strings.NewReader(sse)))
	if err != nil || text != "Hello" {
		t.Fatalf("cumulative snapshots: %q %v", text, err)
	}
}

func TestPplxWebCleanStripsCitations(t *testing.T) {
	if got := pplxWebClean("Answer[1][22] here"); got != "Answer here" {
		t.Fatalf("clean: %q", got)
	}
}

func TestPplxWebToolBlockSurvivesClean(t *testing.T) {
	// A limpeza tira citações e tags grok mas preserva o bloco <tool> do shim.
	raw := "vou consultar[1]\n<tool>{\"name\":\"get_weather\",\"arguments\":{\"city\":\"SP\"}}</tool>\n<grok:render/>"
	_, calls := parseKimiWebToolCalls(pplxWebClean(raw), "nonce")
	if len(calls) != 1 {
		t.Fatalf("tool block perdido na limpeza: %v", calls)
	}
}

func TestGrokWebScanTokensAndFinal(t *testing.T) {
	ndjson := strings.Join([]string{
		`{"result":{"response":{"token":"Hel"}}}`,
		`{"result":{"response":{"token":"lo"}}}`,
		`{"result":{"response":{"modelResponse":{"message":"Hello!"}}}}`,
		``,
	}, "\n")
	text, err := grokWebAccumulate(bufio.NewReader(strings.NewReader(ndjson)))
	if err != nil || text != "Hello!" {
		t.Fatalf("grok ndjson: %q %v", text, err)
	}
	broken := `{"error":{"message":"quota exceeded"}}`
	if _, err := grokWebAccumulate(bufio.NewReader(strings.NewReader(broken))); err == nil {
		t.Fatal("grok error event must fail the stream")
	}
}

func TestZaiWebScanPhases(t *testing.T) {
	// phase thinking vira raciocínio, answer vira texto, other/done são ignorados.
	sse := strings.Join([]string{
		`data: {"type":"chat:completion","data":{"delta_content":"pensando","phase":"thinking"}}`,
		`data: {"type":"chat:completion","data":{"phase":"other","usage":{"total_tokens":5}}}`,
		`data: {"type":"chat:completion","data":{"delta_content":"Olá","phase":"answer"}}`,
		`data: {"type":"chat:completion","data":{"delta_content":"!","phase":"answer"}}`,
		`data: {"type":"chat:completion","data":{"phase":"done","done":true}}`,
		`data: [DONE]`,
		``,
	}, "\n")
	text, think, err := zaiWebAccumulate(bufio.NewReader(strings.NewReader(sse)))
	if err != nil || text != "Olá!" || think != "pensando" {
		t.Fatalf("zai phases: %q %q %v", text, think, err)
	}
}

func TestZaiWebScanEmbeddedError(t *testing.T) {
	sse := `data: {"type":"chat:completion","data":{"done":true,"error":{"code":"FRONTEND_CAPTCHA_REQUIRED","detail":"Captcha verification failed."}}}`
	if _, _, err := zaiWebAccumulate(bufio.NewReader(strings.NewReader(sse))); err == nil {
		t.Fatal("zai embedded error must fail the stream")
	}
}

func TestZaiWebGuestTokenParses(t *testing.T) {
	// GET /api/v1/auths/ devolve a sessão guest; só o campo token interessa.
	body := `{"id":"abc","role":"guest","token":"eyJ.novo.token","token_type":"Bearer","expires_at":null}`
	var parsed struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil || parsed.Token != "eyJ.novo.token" {
		t.Fatalf("guest token: %q %v", parsed.Token, err)
	}
}

func TestZaiWebUserID(t *testing.T) {
	// JWT guest do HAR: claim id 71d8d128-...; token malformado cai no uuid estável.
	token := "eyJhbGciOiJFUzI1NiIsInR5cCI6IkpXVCJ9.eyJpZCI6IjcxZDhkMTI4LWE2YzgtNGQ5Ny1hM2I4LWE4ZjA3ZTFiZDg0ZCIsImVtYWlsIjoiZ3Vlc3QifQ.sig"
	if got := zaiWebUserID(token); got != "71d8d128-a6c8-4d97-a3b8-a8f07e1bd84d" {
		t.Fatalf("user id: %q", got)
	}
	if zaiWebUserID("sem-pontos") == "" || zaiWebUserID("sem-pontos") != zaiWebUserID("sem-pontos") {
		t.Fatal("malformed token must yield a stable id")
	}
}
