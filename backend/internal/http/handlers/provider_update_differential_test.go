package handlers

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

func TestConnectionUpdateCookieAndRefresh(t *testing.T) {
	existing := map[string]any{"id": "conn", "provider": "claude-web", "authType": "cookie", "name": "arthurzanona", "apiKey": "old"}
	var patch map[string]any
	h := Providers{GetConnection: func(_ context.Context, _ string) (map[string]any, error) { return existing, nil },
		UpdateConnection: func(_ context.Context, _ string, p map[string]any) (map[string]any, error) {
			patch = p
			return existing, nil
		}}
	payload := `{"apiKey":"new-cookie","refreshToken":"new-refresh"}`
	r := httptest.NewRequest("PUT", "/api/providers/conn", strings.NewReader(payload))
	route := chi.NewRouteContext()
	route.URLParams.Add("id", "conn")
	r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, route))
	w := httptest.NewRecorder()
	h.Update(w, r)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if patch["apiKey"] != "new-cookie" || patch["refreshToken"] != "new-refresh" {
		t.Fatalf("patch perdido: %v", patch)
	}
}

func TestCookiePair(t *testing.T) {
	cases := []struct {
		raw, name, want string
	}{
		{"sk-ant-sid01-abc", "sessionKey", "sessionKey=sk-ant-sid01-abc"},
		{"sessionKey=sk-ant-sid01-abc", "sessionKey", "sessionKey=sk-ant-sid01-abc"},
		{"sessionKey=abc; cf_clearance=x", "sessionKey", "sessionKey=abc; cf_clearance=x"},
		{"Cookie: sessionKey=abc", "sessionKey", "sessionKey=abc"},
		{"ecto_1_sess=xyz", "ecto_1_sess", "ecto_1_sess=xyz"},
		{"ecto_1_sess=a; datr=b", "ecto_1_sess", "ecto_1_sess=a; datr=b"},
		{"__Secure-next-auth.session-token=jwt", "__Secure-next-auth.session-token", "__Secure-next-auth.session-token=jwt"},
		{"bearer tok", "sso", "sso=tok"},
		{"rawvalue", "sso", "sso=rawvalue"},
	}
	for _, c := range cases {
		if got := cookiePair(c.raw, c.name); got != c.want {
			t.Fatalf("cookiePair(%q, %q) = %q, esperado %q", c.raw, c.name, got, c.want)
		}
	}
}

func TestProviderIDAgainstOriginal(t *testing.T) {
	raw, err := os.ReadFile("testdata/provider-update-original.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Method, ID string
		Input      any
		Status     int
		Body       any
	}
	if err = json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	existing := func() map[string]any {
		return map[string]any{"id": "conn", "provider": "openai", "authType": "apikey", "name": "Existing", "apiKey": "secret", "accessToken": "access", "refreshToken": "refresh", "idToken": "id-secret", "providerSpecificData": map[string]any{"custom": true, "proxyPoolId": "old"}}
	}
	h := Providers{GetConnection: func(_ context.Context, id string) (map[string]any, error) {
		if id == "missing" {
			return nil, nil
		}
		return existing(), nil
	}, GetPool: func(_ context.Context, id string) (map[string]any, error) {
		if id == "pool" {
			return map[string]any{"id": id}, nil
		}
		return nil, nil
	}, UpdateConnection: func(_ context.Context, _ string, patch map[string]any) (map[string]any, error) {
		value := existing()
		for k, v := range patch {
			value[k] = v
		}
		return value, nil
	}, DeleteConnection: func(_ context.Context, id string) (bool, error) { return id != "missing", nil }}
	for i, tt := range cases {
		payload, _ := json.Marshal(tt.Input)
		r := httptest.NewRequest(tt.Method, "/api/providers/"+tt.ID, strings.NewReader(string(payload)))
		route := chi.NewRouteContext()
		route.URLParams.Add("id", tt.ID)
		r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, route))
		w := httptest.NewRecorder()
		switch tt.Method {
		case "GET":
			h.Get(w, r)
		case "PUT":
			h.Update(w, r)
		case "DELETE":
			h.Delete(w, r)
		}
		var got any
		if err = json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if w.Code != tt.Status || !reflect.DeepEqual(got, tt.Body) {
			t.Fatalf("case %d: Go=%d %v original=%d %v", i, w.Code, got, tt.Status, tt.Body)
		}
	}
}
