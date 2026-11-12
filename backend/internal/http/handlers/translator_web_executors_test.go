package handlers

import (
	"bufio"
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
