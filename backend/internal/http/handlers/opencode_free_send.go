package handlers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/proxy"

	utls "github.com/refraction-networking/utls"

	"github.com/zanonaa/VansGo/backend/internal/providers"
)

func newOpenCodeStreamScanner(reader io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 16<<20)
	return scanner
}

// sendOpenCodeFree executa o contrato do free tier do OpenCode Zen (main do
// 9router): identidade cli/2.0.26 com 4 headers de sessão, fingerprint de
// tools read/shell + bloco <env>, stream forçado upstream, credencial "public"
// quando a conexão não tem chave, TLS uTLS (ClientHello do Bun) no trecho até
// o upstream e fold do stream de volta a um chat.completion quando o cliente
// pediu JSON (com nomes de tools restaurados).
//
// Rejeição 429/403: corpo com "overloaded" é overload upstream — retry no
// mesmo pool após o Retry-After; caso contrário é quota — a session é esquecida
// (mint de nova na próxima tentativa) e o pool marca unfit no escopo
// provider::modelo, re-tentando com o próximo pool (MAX_POOL_RETRIES=2).
func (t *TranslatorSend) sendOpenCodeFree(w http.ResponseWriter, r *http.Request, provider, model string, body map[string]any, connection map[string]any) {
	fail := func(status int, message string) {
		nodeReply(w, status, map[string]any{"success": false, "error": message})
	}
	outbound := map[string]any{}
	for key, value := range body {
		outbound[key] = value
	}
	outbound["model"] = model
	clientWantsStream := outbound["stream"] == true
	outbound["stream"] = true
	renames := openCodeApplyFingerprint(outbound)
	t.TokenSaver.Apply(r.Context(), r, outbound, "openai", model, provider)

	token := ttsString(firstNonEmpty(connection["apiKey"], connection["accessToken"]))
	if token == "" {
		// Executor original: pedido sem credencial cai em "public" — o upstream
		// lê outra coisa como credencial ausente e responde 401.
		token = "public"
	}
	// Upstream do free tier conforme o transport registrado do provider.
	target := strings.TrimRight(providers.TransportFor(provider).BaseURL, "/")
	if !strings.HasSuffix(target, "/chat/completions") {
		target += "/chat/completions"
	}
	connectionID := ttsString(connection["id"])

	encoded, err := json.Marshal(outbound)
	if err != nil {
		fail(500, err.Error())
		return
	}

	metadata, _ := connection["providerSpecificData"].(map[string]any)
	const maxPoolRetries = 2
	const maxOverloadRetries = 2
	excluded := []string{}
	overloadRetries := 0
	var lastStatus int
	var lastDetails string
	var lastRetryAfter string
	for attempt := 0; ; attempt++ {
		attemptMetadata := metadata
		if len(excluded) > 0 {
			copied := map[string]any{}
			for key, value := range metadata {
				copied[key] = value
			}
			// proxyPoolIds vira a lista candidata sem os pools já reprovados;
			// a resolução continua respeitando estratégia/targets/fitness.
			if remaining := mergePoolCandidates(metadata, excluded); len(remaining) > 0 {
				copied["proxyPoolIds"] = remaining
				copied["excludeProxyPoolIds"] = excluded
				if single := strings.TrimSpace(ttsString(copied["proxyPoolId"])); single != "" && single != "__none__" {
					copied["proxyPoolId"] = "__none__"
				}
				copied["proxyPoolScope"] = openCodeIPLimitScope(provider, model)
			}
			attemptMetadata = copied
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
		// Headers por tentativa: uma session rejeitada é esquecida e a próxima
		// iteração minta outra (a quota upstream é contabilizada por session).
		headers := openCodeHeaders(r.Header, token, connectionID)
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(encoded))
		if err != nil {
			cancel()
			fail(500, err.Error())
			return
		}
		for key, value := range headers {
			request.Header.Set(key, value)
		}
		client, relay, poolID, cleanup, err := t.openCodeResolvedClient(ctx, attemptMetadata)
		if err != nil {
			cancel()
			fail(502, err.Error())
			return
		}
		if err := rewriteConnectionRelay(request, relay); err != nil {
			cancel()
			cleanup()
			fail(502, err.Error())
			return
		}
		auditOpenCodeIdentity(r.Header, headers, poolID, model, attempt+1)
		response, err := client.Do(request)
		if err != nil {
			response = nil
			if poolID != "" && attempt < maxPoolRetries && len(mergePoolCandidates(metadata, append(append([]string{}, excluded...), poolID))) > 0 {
				// Falha de transporte no proxy marcado: tenta o próximo pool
				// antes de devolver erro ao cliente.
				excluded = append(excluded, poolID)
				markPoolUnfit(r.Context(), poolID, openCodeIPLimitScope(provider, model), nil, "transport-error")
				cancel()
				cleanup()
				continue
			}
			cancel()
			cleanup()
			fail(502, err.Error())
			return
		}
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			defer cleanup()
			defer cancel()
			t.openCodeRelayResponse(w, response, clientWantsStream, model, renames)
			return
		}
		details, _ := io.ReadAll(io.LimitReader(response.Body, 8<<10))
		response.Body.Close()
		cancel()
		cleanup()
		lastStatus = response.StatusCode
		lastDetails = string(details)
		lastRetryAfter = response.Header.Get("Retry-After")
		if (lastStatus == 429 || lastStatus == 403) && openCodeOverloadedRe.MatchString(lastDetails) {
			// Overload upstream: não é cota do IP nem da session — espera o
			// Retry-After e tenta de novo no mesmo pool.
			if overloadRetries < maxOverloadRetries {
				overloadRetries++
				time.Sleep(openCodeOverloadBackoff(lastRetryAfter))
				continue
			}
			break
		}
		if lastStatus == 429 || lastStatus == 403 {
			// Quota por session rejeitada: esquece o valor para a próxima
			// tentativa mintar outra (#5559 do 9router).
			forgetOpenCodeSession(headers["x-opencode-session"])
		}
		if poolID != "" && openCodeIsIPLimit(lastStatus, lastDetails) {
			until := ipLimitRetryAfter(lastRetryAfter)
			markPoolUnfit(r.Context(), poolID, openCodeIPLimitScope(provider, model), until, "ip-limit")
			excluded = append(excluded, poolID)
			if attempt < maxPoolRetries && len(mergePoolCandidates(metadata, excluded)) > 0 {
				continue
			}
		}
		break
	}
	if lastRetryAfter != "" {
		w.Header().Set("Retry-After", lastRetryAfter)
	}
	nodeReply(w, lastStatus, map[string]any{"success": false, "error": fmt.Sprintf("Provider error: %d", lastStatus), "details": lastDetails})
}

// openCodeOverloadedRe distingue overload upstream de rejeição de quota
// (OPENCODE_OVERLOAD_RE do executor do 9router).
var openCodeOverloadedRe = regexp.MustCompile(`(?i)overloaded`)

// openCodeOverloadBackoff converte o Retry-After do overload em espera, com
// teto de 30s e piso de 2s.
func openCodeOverloadBackoff(retryAfter string) time.Duration {
	if seconds, err := strconv.Atoi(strings.TrimSpace(retryAfter)); err == nil && seconds > 0 {
		if seconds > 30 {
			seconds = 30
		}
		return time.Duration(seconds) * time.Second
	}
	return 2 * time.Second
}

// openCodeRelayResponse escreve a resposta de sucesso: stream dobrado de volta
// em chat.completion para clientes JSON, ou SSE repassado com nomes de tools
// restaurados.
func (t *TranslatorSend) openCodeRelayResponse(w http.ResponseWriter, response *http.Response, clientWantsStream bool, model string, renames map[string]string) {
	defer response.Body.Close()
	if clientWantsStream {
		// Cliente pediu SSE: repassa o stream reescrevendo os nomes das tools
		// renomeadas em cada chunk.
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		flusher, _ := w.(http.Flusher)
		scanner := newOpenCodeStreamScanner(response.Body)
		for scanner.Scan() {
			written := scanner.Bytes()
			if renamed, ok := openCodeRewriteChunk(written, renames); ok {
				written = renamed
			}
			line := append(append([]byte{}, written...), '\n')
			if _, err := w.Write(line); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		return
	}
	completion, err := openCodeFoldStream(response.Body, model, renames)
	if err != nil {
		nodeReply(w, 502, map[string]any{"success": false, "error": err.Error()})
		return
	}
	nodeReply(w, 200, completion)
}

// openCodeResolvedClient resolve o transporte do free tier com TLS uTLS no
// trecho até o upstream (o gate valida o ClientHello). Relay segue no caminho
// normal — o gate vê o egress do relay, não o nosso TLS. Direto, proxy HTTP
// (CONNECT manual + handshake uTLS depois do túnel) e SOCKS (dialer + uTLS)
// instalam DialTLSContext com a spec do Bun.
func (t *TranslatorSend) openCodeResolvedClient(ctx context.Context, data map[string]any) (*http.Client, string, string, func(), error) {
	config := resolveConnectionProxy(ctx, data, t.GetPool)
	if config.relay != "" {
		client, relay, cleanup, err := connectionHTTPResolved(ctx, data, config, t.Client)
		return client, relay, config.poolID, cleanup, err
	}
	client := http.Client{}
	if t.Client != nil {
		client = *t.Client
	}
	cleanup := func() {}
	switch {
	case config.proxyURL != "":
		if err := dialCheck(config.proxyURL); err != nil {
			return nil, "", "", cleanup, err
		}
		var transport *http.Transport
		var err error
		tlsBase := baseTLSConfig(t.Client)
		if isSOCKSProxyURL(config.proxyURL) {
			transport, err = openCodeSocksTransport(config.proxyURL, tlsBase)
		} else {
			transport, err = openCodeProxyConnectTransport(config.proxyURL, config.noProxy, tlsBase)
		}
		if err != nil {
			return nil, "", "", cleanup, err
		}
		if config.strict {
			client.Transport = transport
		} else {
			client.Transport = proxyFallbackTransport{proxy: transport, direct: openCodeDirectTransport(tlsBase)}
		}
	case config.strict && (config.intended || proxyIntended(data)):
		return nil, "", "", cleanup, errProxyRequired
	default:
		client.Transport = openCodeDirectTransport(baseTLSConfig(t.Client))
	}
	client.Transport = auditConnectionTransport(client.Transport)
	if usage, ok := ctx.Value(usageContextKey{}).(*requestUsage); ok {
		client.Transport = usageTransport{base: client.Transport, usage: usage}
	}
	return &client, "", config.poolID, cleanup, nil
}

// baseTLSConfig propaga o ajuste de confiança do client base (InsecureSkipVerify
// e RootCAs customizados, ex.: upstreams de teste) para o handshake uTLS.
func baseTLSConfig(base *http.Client) *utls.Config {
	if base == nil {
		return nil
	}
	transport, ok := base.Transport.(*http.Transport)
	if !ok || transport.TLSClientConfig == nil {
		return nil
	}
	return &utls.Config{
		InsecureSkipVerify: transport.TLSClientConfig.InsecureSkipVerify,
		RootCAs:            transport.TLSClientConfig.RootCAs,
	}
}

// openCodeDirectTransport: conexão direta com TLS uTLS. Proxy nil de propósito:
// com Proxy setado o net/http faz CONNECT e handshake Go TLS próprio,
// ignorando DialTLSContext — e o gate responde 429.
func openCodeDirectTransport(base *utls.Config) *http.Transport {
	transport := sharedDirectTransport().Clone()
	transport.Proxy = nil
	transport.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return openCodeDialTLS(ctx, network, addr, base)
	}
	return transport
}

// openCodeDialTLS troca o handshake Go pelo uTLS (ClientHello do Bun).
func openCodeDialTLS(ctx context.Context, network, addr string, base *utls.Config) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	raw, err := (&net.Dialer{Timeout: 15 * time.Second}).DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	return openCodeTLSHandshake(ctx, raw, host, base)
}

// openCodeSocksTransport: egress via dialer SOCKS (auth da URL; socks5h/4a
// resolvem o host no proxy) com handshake uTLS sobre o túnel.
func openCodeSocksTransport(proxyURL string, base *utls.Config) (*http.Transport, error) {
	parsed, err := url.Parse(strings.TrimSpace(proxyURL))
	if err != nil || parsed.Host == "" {
		return nil, err
	}
	address := parsed.Host
	if parsed.Port() == "" {
		address = net.JoinHostPort(parsed.Hostname(), "1080")
	}
	var auth *proxy.Auth
	if parsed.User != nil {
		password, _ := parsed.User.Password()
		auth = &proxy.Auth{User: parsed.User.Username(), Password: password}
	}
	dialer, err := proxy.SOCKS5("tcp", address, auth, proxy.Direct)
	if err != nil {
		return nil, err
	}
	contextDialer, ok := dialer.(proxy.ContextDialer)
	if !ok {
		return nil, errors.New("socks dialer sem DialContext")
	}
	transport := openCodeDirectTransport(base)
	transport.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		raw, err := contextDialer.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return openCodeTLSHandshake(ctx, raw, host, base)
	}
	return transport, nil
}

// openCodeProxyConnectTransport: CONNECT manual no proxy HTTP e handshake uTLS
// depois do túnel. O transporte vê conexão direta já em TLS (Proxy nil).
func openCodeProxyConnectTransport(proxyURL, noProxy string, base *utls.Config) (*http.Transport, error) {
	parsed, err := url.Parse(strings.TrimSpace(proxyURL))
	if err != nil || parsed.Host == "" {
		return nil, err
	}
	transport := openCodeDirectTransport(base)
	transport.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		if connectionNoProxyMatches(host, noProxy) {
			return openCodeDialTLS(ctx, network, addr, base)
		}
		raw, err := openCodeProxyConnect(ctx, parsed, addr)
		if err != nil {
			return nil, err
		}
		return openCodeTLSHandshake(ctx, raw, host, base)
	}
	return transport, nil
}

// openCodeProxyConnect abre o túnel CONNECT no proxy HTTP e devolve o conn cru
// pronto para o handshake TLS.
func openCodeProxyConnect(ctx context.Context, proxyURL *url.URL, hostPort string) (net.Conn, error) {
	address := proxyURL.Host
	if proxyURL.Port() == "" {
		address = net.JoinHostPort(proxyURL.Hostname(), "80")
	}
	raw, err := (&net.Dialer{Timeout: 15 * time.Second}).DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	var connect strings.Builder
	fmt.Fprintf(&connect, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n", hostPort, hostPort)
	if proxyURL.User != nil {
		password, _ := proxyURL.User.Password()
		credential := base64.StdEncoding.EncodeToString([]byte(proxyURL.User.Username() + ":" + password))
		fmt.Fprintf(&connect, "Proxy-Authorization: Basic %s\r\n", credential)
	}
	connect.WriteString("\r\n")
	if _, err := raw.Write([]byte(connect.String())); err != nil {
		raw.Close()
		return nil, err
	}
	response, err := http.ReadResponse(bufio.NewReader(raw), nil)
	if err != nil {
		raw.Close()
		return nil, err
	}
	// shortcut: resposta CONNECT sem corpo lido além do header não é
	// re-verificada; se o proxy pipelinar bytes após o 200, o handshake falha —
	// trocar por conn de buffer só se isso aparecer na prática.
	if response.StatusCode != http.StatusOK {
		raw.Close()
		return nil, fmt.Errorf("proxy CONNECT: %s", response.Status)
	}
	return raw, nil
}

// openCodeIPLimitScope é o escopo de fitness do free tier (provider::modelo),
// como o proxyScope do chatCore original.
func openCodeIPLimitScope(provider, model string) string {
	return provider + "::" + model
}

// openCodeIsIPLimit reproduz parseError do executor original: 429/403 com
// corpo de limite (limit|rate|quota|exhausted|capacity|too many|retry) é um
// erro escopado ao pool (ip-limit).
func openCodeIsIPLimit(status int, body string) bool {
	if status != 429 && status != 403 {
		return false
	}
	lower := strings.ToLower(body)
	for _, marker := range []string{"limit", "rate", "quota", "exhausted", "capacity", "too many", "retry"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// ipLimitRetryAfter converte o Retry-After do upstream em timestamp de
// expiração do fitness; sem valor usa 24h (a janela do free tier é diária).
func ipLimitRetryAfter(retryAfter string) *int64 {
	if seconds, err := strconv.Atoi(strings.TrimSpace(retryAfter)); err == nil && seconds > 0 {
		until := time.Now().Add(time.Duration(seconds) * time.Second).UnixMilli()
		return &until
	}
	fallback := time.Now().Add(24 * time.Hour).UnixMilli()
	return &fallback
}

// mergePoolCandidates devolve a lista de pools candidatos menos os excluídos,
// preservando a ordem. Inclui o pool único quando não há pool-set — a origem
// re-resolve por resolveProxyConfig com excludePoolIds.
func mergePoolCandidates(metadata map[string]any, excluded []string) []string {
	ids := dedupeStrings(stringSlice(metadata["proxyPoolIds"]))
	if len(ids) == 0 {
		if single := strings.TrimSpace(ttsString(metadata["proxyPoolId"])); single != "" && single != "__none__" {
			ids = []string{single}
		}
	}
	result := make([]string, 0, len(ids))
	skip := map[string]bool{}
	for _, id := range excluded {
		skip[id] = true
	}
	for _, id := range ids {
		if !skip[id] {
			result = append(result, id)
		}
	}
	return result
}

// openCodeRewriteChunk aplica os nomes originais em um chunk SSE de uma linha.
func openCodeRewriteChunk(line []byte, renames map[string]string) ([]byte, bool) {
	if len(renames) == 0 {
		return nil, false
	}
	trimmed := bytes.TrimSpace(line)
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return nil, false
	}
	data := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
	if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
		return nil, false
	}
	var chunk map[string]any
	if json.Unmarshal(data, &chunk) != nil {
		return nil, false
	}
	openCodeRestoreNames(chunk, renames)
	rewritten, err := json.Marshal(chunk)
	if err != nil {
		return nil, false
	}
	return append([]byte("data: "), rewritten...), true
}
