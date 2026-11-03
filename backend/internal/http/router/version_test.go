package router

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"

	"github.com/zanonaa/VansGo/backend/internal/http/handlers"
	"strings"
	"testing"
)

func TestVersionPublicSkip(t *testing.T) {
	t.Setenv("VANSGO_SKIP_UPDATE_CHECK", "1")
	auth, _ := wiredAuth(t)
	r := NewWithAuth(nil, auth)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/api/version", nil))
	var value map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &value)
	if w.Code != 200 || value["currentVersion"] != "2.1.0" || value["latestVersion"] != nil || value["hasUpdate"] != false {
		t.Fatal(w.Code, value)
	}
	for _, method := range []string{"HEAD", "OPTIONS"} {
		w = httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(method, "/api/version", nil))
		if w.Body.Len() != 0 || (w.Code != 200 && w.Code != 204) {
			t.Fatal(method, w.Code)
		}
	}
}

func TestVersionUpdateRouteGate(t *testing.T) {
	auth, _ := wiredAuth(t)
	r := NewWithAuth(nil, auth)
	// ALWAYS_PROTECTED: sem autenticação é 401 para POST e OPTIONS.
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/api/version/update", nil))
	if w.Code != 401 {
		t.Fatalf("anon post %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("OPTIONS", "/api/version/update", nil))
	if w.Code != 401 {
		t.Fatalf("anon options %d", w.Code)
	}
	token, _ := auth.Sessions.Create(nil)
	cookie := &http.Cookie{Name: "auth_token", Value: token}
	req := httptest.NewRequest("POST", "/api/version/update", nil)
	req.AddCookie(cookie)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 403 || !strings.Contains(w.Body.String(), "Update is only available in production build (VansGo)") {
		t.Fatalf("update gate %d %s", w.Code, w.Body.String())
	}
	req = httptest.NewRequest("OPTIONS", "/api/version/update", nil)
	req.AddCookie(cookie)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 200 && w.Code != 204 {
		t.Fatalf("authed options %d", w.Code)
	}
}

// Paridade do modo nativo da origem: o ciclo do headroom pertence à
// dashboard (sessão) em qualquer origem; o guard de Middleware geral basta e
// o 403 local-only foi removido. requireLogin=true → 401 sem sessão; com
// cookie os handlers respondem normalmente.
func TestHeadroomRoutesSessionParity(t *testing.T) {
	t.Setenv("HEADROOM_URL", "")
	auth, _ := wiredAuth(t)
	auth.Settings = func(context.Context) (map[string]any, error) {
		return map[string]any{"requireLogin": true}, nil
	}
	headroom := &handlers.HeadroomRoutes{
		H:    &handlers.Headroom{DataDir: t.TempDir(), FindPython: func() string { return "" }},
		Read: auth.Settings,
	}
	r := NewWithNodes(nil, auth, nil, &handlers.Providers{Headroom: headroom})
	// Sem sessão e requireLogin=true: o Middleware geral nega antes do
	// handler (deny-by-default para /api/*).
	for _, path := range []string{"/api/headroom/status", "/api/headroom/start", "/api/headroom/stop", "/api/headroom/restart", "/api/headroom/extras"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s anon requireLogin=true: %d %s", path, w.Code, w.Body.String())
		}
	}
	// Sessão de dashboard em qualquer origem (era restrita a loopback na era
	// sidecar): handlers respondem com os contratos reais — sem binário do
	// CLI neste ambiente, start/restart → 400 NOT_INSTALLED, stop sem pid →
	// 409, status/extras → 200 e rota desconhecida continua 404.
	token, _ := auth.Sessions.Create(nil)
	cookie := &http.Cookie{Name: "auth_token", Value: token}
	expectations := []struct {
		method string
		path   string
		code   int
		body   string
	}{
		{http.MethodGet, "/api/headroom/status", 200, ""},
		{http.MethodGet, "/api/headroom/extras", 200, ""},
		{http.MethodPost, "/api/headroom/start", 400, "NOT_INSTALLED"},
		{http.MethodPost, "/api/headroom/restart", 400, "NOT_INSTALLED"},
		{http.MethodPost, "/api/headroom/stop", 409, "not_running"},
	}
	for _, expected := range expectations {
		req := httptest.NewRequest(expected.method, expected.path, nil)
		req.AddCookie(cookie)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != expected.code || !strings.Contains(w.Body.String(), expected.body) {
			t.Fatalf("%s %s: %d %s (esperado %d %q)", expected.method, expected.path, w.Code, w.Body.String(), expected.code, expected.body)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/api/headroom/nonexistent", nil)
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 404 {
		t.Fatalf("rota não registrada %d", w.Code)
	}
}

func TestVersionShutdownRouteRegistered(t *testing.T) {
	auth, _ := wiredAuth(t)
	r := NewWithAuth(nil, auth)
	// ALWAYS_PROTECTED exige 401 antes do gate/exit.
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("POST", "/api/version/shutdown", nil))
	if w.Code != 401 {
		t.Fatalf("anon %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("OPTIONS", "/api/version/shutdown", nil))
	if w.Code == 404 {
		t.Fatal("options não registrada")
	}
}

func TestV1BetaModelsRoutePublic(t *testing.T) {
	auth, _ := wiredAuth(t)
	r := NewWithAuth(nil, auth)
	// Public LLM prefix: loopback do peer TCP é confiável; remoto exige key.
	loopback := httptest.NewRequest("GET", "/api/v1beta/models", nil)
	loopback.RemoteAddr = "127.0.0.1:5000"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, loopback)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"models"`) {
		t.Fatalf("v1beta %d", w.Code)
	}
	remote := httptest.NewRequest("GET", "/api/v1beta/models", nil)
	remote.RemoteAddr = "203.0.113.5:5000"
	w = httptest.NewRecorder()
	r.ServeHTTP(w, remote)
	if w.Code != 401 {
		t.Fatalf("remoto %d %s", w.Code, w.Body.String())
	}
	loopbackNoAPI := httptest.NewRequest("GET", "/v1beta/models", nil)
	loopbackNoAPI.RemoteAddr = "127.0.0.1:5000"
	w = httptest.NewRecorder()
	r.ServeHTTP(w, loopbackNoAPI)
	if w.Code != 200 {
		t.Fatalf("prefixo sem api %d", w.Code)
	}
}

func TestPxpipeRoutesRegistered(t *testing.T) {
	auth, _ := wiredAuth(t)
	r := NewWithNodes(nil, auth, nil, &handlers.Providers{Pxpipe: &handlers.Pxpipe{Loader: &handlers.PxpipeLoader{}}})
	// wiredAuth usa requireLogin=false: anônimo acessa rotas comuns do /api/*.
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/api/pxpipe/status", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"mode":"library"`) {
		t.Fatalf("status anon %d %s", w.Code, w.Body.String())
	}
	// Sem sessão autenticada não é possível acionar; testar registro por OPTIONS.
	for _, probe := range []string{"/api/pxpipe/health", "/api/pxpipe/install", "/api/pxpipe/start", "/api/pxpipe/stop", "/api/pxpipe/restart", "/api/pxpipe/logs", "/api/pxpipe/stats"} {
		w = httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("OPTIONS", probe, nil))
		if w.Code == 404 {
			t.Fatalf("%s options não registrada", probe)
		}
	}
	// Sem Pxpipe no wiring: rotas não existem (nil-safe).
	r2 := NewWithNodes(nil, auth, nil, &handlers.Providers{})
	w = httptest.NewRecorder()
	r2.ServeHTTP(w, httptest.NewRequest("GET", "/api/pxpipe/status", nil))
	if w.Code != 404 {
		t.Fatalf("sem wiring %d", w.Code)
	}
}

func TestModelsExtrasRoutesRegistered(t *testing.T) {
	auth, _ := wiredAuth(t)
	r := NewWithNodes(nil, auth, nil, &handlers.Providers{
		ModelAvailability: &handlers.ModelAvailability{},
		CatalogSync:       &handlers.ModelCatalogSync{DataDir: t.TempDir()},
	})
	for _, probe := range []string{"/api/models/availability", "/api/models/catalog-sync"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", probe, nil))
		if w.Code == 404 {
			t.Fatalf("%s GET não registrada", probe)
		}
		w = httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("POST", probe, nil))
		if w.Code == 404 {
			t.Fatalf("%s POST não registrada", probe)
		}
		w = httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("OPTIONS", probe, nil))
		if w.Code == 404 {
			t.Fatalf("%s OPTIONS não registrada", probe)
		}
	}
}

func TestProxyPoolExtrasRoutesRegistered(t *testing.T) {
	auth, _ := wiredAuth(t)
	r := NewWithNodes(nil, auth, nil, &handlers.Providers{
		PoolFitness:  &handlers.PoolFitnessRoutes{Service: &handlers.PoolFitnessService{}},
		ImportPublic: &handlers.ImportPublic{},
	})
	for _, probe := range []string{"/api/proxy-pools/fitness", "/api/proxy-pools/fitness/clear-all", "/api/proxy-pools/p1/fitness/clear", "/api/proxy-pools/import-public"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("POST", probe, nil))
		if w.Code == 404 {
			t.Fatalf("%s POST não registrada", probe)
		}
		w = httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("OPTIONS", probe, nil))
		if w.Code == 404 {
			t.Fatalf("%s OPTIONS não registrada", probe)
		}
	}
	// Sem wiring: rotas ausentes.
	r2 := NewWithNodes(nil, auth, nil, &handlers.Providers{})
	w := httptest.NewRecorder()
	r2.ServeHTTP(w, httptest.NewRequest("GET", "/api/proxy-pools/fitness", nil))
	if w.Code != 404 {
		t.Fatalf("sem wiring %d", w.Code)
	}
}

func TestPoolDeployRoutesRegistered(t *testing.T) {
	auth, _ := wiredAuth(t)
	r := NewWithNodes(nil, auth, nil, &handlers.Providers{Deploys: handlers.NewRelayDeploys(nil)})
	for _, probe := range []string{"/api/proxy-pools/cloudflare-deploy", "/api/proxy-pools/deno-deploy", "/api/proxy-pools/vercel-deploy"} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("POST", probe, nil))
		if w.Code == 404 {
			t.Fatalf("%s não registrada", probe)
		}
		w = httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("OPTIONS", probe, nil))
		if w.Code == 404 {
			t.Fatalf("%s OPTIONS não registrada", probe)
		}
	}
}
