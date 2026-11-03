package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/zanonaa/VansGo/backend/internal/database"
)

// fakeProbe mitiga os custos de rede do teste individual do combo: respostas
// fixas por modelo e registro das chamadas na ORDEM executada.
type fakeProbe struct {
	responses map[string]map[string]any
	calls     []string
}

func (f *fakeProbe) ping(_ context.Context, model, kind string) map[string]any {
	f.calls = append(f.calls, model)
	if v, ok := f.responses[model]; ok {
		return v
	}
	return map[string]any{"ok": false, "status": 500, "error": "HTTP 500: upstream down", "latencyMs": 1.0}
}

func probeOK() map[string]any {
	return map[string]any{"ok": true, "status": 200, "latencyMs": 12.0}
}

func probeFail(status int, message string) map[string]any {
	return map[string]any{"ok": false, "status": status, "error": message, "latencyMs": 7.0}
}

// newComboTestHandle abre um sqlite real com a tabela de combos e o settings.
func newComboTestHandle(t *testing.T, settings string) (*Combos, *fakeProbe) {
	t.Helper()
	db, err := database.Open(context.Background(), t.TempDir(), "NORMAL")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE settings(id INTEGER PRIMARY KEY,data TEXT);` +
		`INSERT INTO settings VALUES(1,'` + settings + `');` +
		`CREATE TABLE combos(id TEXT PRIMARY KEY,name TEXT,kind TEXT,models TEXT,context_length REAL,createdAt TEXT,updatedAt TEXT);`); err != nil {
		t.Fatal(err)
	}
	probe := &fakeProbe{responses: map[string]map[string]any{}}
	handler := &Combos{
		Store: database.Combos{DB: db},
		DB:    db,
		Ping:  probe.ping,
		Read:  func(ctx context.Context) (map[string]any, error) { return database.ReadAuthSettings(ctx, db) },
	}
	return handler, probe
}

func comboTestRequest(t *testing.T, id string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/combos/"+id+"/test", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", id)
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

func decodeComboTest(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	return out
}

func attemptModels(out map[string]any) []string {
	attempts, _ := out["attempts"].([]any)
	models := make([]string, 0, len(attempts))
	for _, raw := range attempts {
		entry, _ := raw.(map[string]any)
		models = append(models, ttsString(entry["model"]))
	}
	return models
}

func TestComboTestFallbackTraversesOn400(t *testing.T) {
	handler, probe := newComboTestHandle(t, `{"requireApiKey":true}`)
	// Decisão de produto (2026-11-03): 400 não para mais a cadeia — o
	// fallback segue para o próximo candidato; erro só ao fim.
	if _, err := handler.DB.Exec(`INSERT INTO combos VALUES('a','zn-glm','llm','["glm/glm-5.3","zn-gpt"]',NULL,'2026','2026');` +
		`INSERT INTO combos VALUES('b','zn-gpt','llm','["cx/gpt-6.1-sol","gcli/grok-4.7"]',NULL,'2026','2026');`); err != nil {
		t.Fatal(err)
	}
	probe.responses = map[string]map[string]any{
		"glm/glm-5.3":   probeFail(400, "HTTP 400: prompt too long"),
		"cx/gpt-6.1-sol": probeOK(),
		"gcli/grok-4.7": probeOK(),
	}
	w := httptest.NewRecorder()
	handler.Test(w, comboTestRequest(t, "a"))
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	out := decodeComboTest(t, w)
	if ok, _ := out["ok"].(bool); !ok {
		t.Fatalf("400 do glm deveria cruzar para o codex: %s", w.Body.String())
	}
	if got := attemptModels(out); strings.Join(got, ",") != "glm/glm-5.3,cx/gpt-6.1-sol" {
		t.Fatalf("ordem %v", got)
	}
}

func TestComboTestFallbackFallsThroughOn429(t *testing.T) {
	handler, probe := newComboTestHandle(t, `{"requireApiKey":true}`)
	if _, err := handler.DB.Exec(`INSERT INTO combos VALUES('a','zn-glm','llm','["glm/glm-5.3","zn-gpt"]',NULL,'2026','2026');` +
		`INSERT INTO combos VALUES('b','zn-gpt','llm','["cx/gpt-6.1-sol","gcli/grok-4.7"]',NULL,'2026','2026');`); err != nil {
		t.Fatal(err)
	}
	probe.responses = map[string]map[string]any{
		"glm/glm-5.3":   probeFail(429, "HTTP 429: 1113 insufficient balance"),
		"cx/gpt-6.1-sol": probeFail(429, "HTTP 429: usage limit"),
		"gcli/grok-4.7": probeOK(),
	}
	w := httptest.NewRecorder()
	handler.Test(w, comboTestRequest(t, "a"))
	out := decodeComboTest(t, w)
	if ok, _ := out["ok"].(bool); !ok {
		t.Fatalf("429 deveria cruzar a cadeia: %s", w.Body.String())
	}
	want := "glm/glm-5.3,cx/gpt-6.1-sol,gcli/grok-4.7"
	if got := strings.Join(attemptModels(out), ","); got != want {
		t.Fatalf("ordem %v, esperado %s", got, want)
	}
}

func TestComboTestGuardAndNotConfigured(t *testing.T) {
	// Sem DB/Ping o endpoint responde 503 NOT_CONFIGURED, nunca 500 mudo.
	handler := &Combos{Store: database.Combos{}}
	w := httptest.NewRecorder()
	handler.Test(w, comboTestRequest(t, "qualquer"))
	if w.Code != 503 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	out := decodeComboTest(t, w)
	if code := ttsString(out["code"]); code != "NOT_CONFIGURED" {
		t.Fatalf("%v", out)
	}
}

func TestComboTest404ForUnknownCombo(t *testing.T) {
	handler, _ := newComboTestHandle(t, `{}`)
	w := httptest.NewRecorder()
	handler.Test(w, comboTestRequest(t, "nao-existe"))
	if w.Code != 404 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestComboTestFusionProbesComboItself(t *testing.T) {
	handler, probe := newComboTestHandle(t, `{"comboStrategies":{"bundle":{"fallbackStrategy":"fusion"}}}`)
	if _, err := handler.DB.Exec(`INSERT INTO combos VALUES('a','bundle','llm','["glm/glm-5.3"]',NULL,'2026','2026');`); err != nil {
		t.Fatal(err)
	}
	probe.responses = map[string]map[string]any{"bundle": probeOK()}
	w := httptest.NewRecorder()
	handler.Test(w, comboTestRequest(t, "a"))
	out := decodeComboTest(t, w)
	if ok, _ := out["ok"].(bool); !ok {
		t.Fatalf("fusion deveria passar pelo ping do combo: %s", w.Body.String())
	}
	if strategy := ttsString(out["strategy"]); strategy != "fusion" {
		t.Fatalf("strategy %v", strategy)
	}
	if got := strings.Join(probe.calls, ","); got != "bundle" {
		t.Fatalf("fusion deveria sondar apenas o combo, não candidatos: %v", got)
	}
}

func TestComboTestRoundRobinPingsAllCandidates(t *testing.T) {
	handler, probe := newComboTestHandle(t, `{"comboStrategies":{"bundle":{"fallbackStrategy":"round-robin"}}}`)
	if _, err := handler.DB.Exec(`INSERT INTO combos VALUES('a','bundle','llm','["glm/glm-5.3","cx/gpt-6.1-sol"]',NULL,'2026','2026');`); err != nil {
		t.Fatal(err)
	}
	probe.responses = map[string]map[string]any{
		"glm/glm-5.3":   probeFail(400, "HTTP 400: bad request"),
		"cx/gpt-6.1-sol": probeOK(),
	}
	w := httptest.NewRecorder()
	handler.Test(w, comboTestRequest(t, "a"))
	out := decodeComboTest(t, w)
	if got := attemptModels(out); len(got) != 2 {
		t.Fatalf("round-robin deveria sondar todos: %v", got)
	}
}

func TestComboTestRejectsCyclesExpansion(t *testing.T) {
	handler, _ := newComboTestHandle(t, `{}`)
	if _, err := handler.DB.Exec(`INSERT INTO combos VALUES('a','zn-a','llm','["zn-b"]',NULL,'2026','2026');` +
		`INSERT INTO combos VALUES('b','zn-b','llm','["zn-a"]',NULL,'2026','2026');`); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	handler.Test(w, comboTestRequest(t, "a"))
	out := decodeComboTest(t, w)
	if w.Code != 400 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(ttsString(out["error"]), "Circular") {
		t.Fatalf("%v", out["error"])
	}
}
