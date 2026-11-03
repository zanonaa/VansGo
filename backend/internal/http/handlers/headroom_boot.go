package handlers

// Boot do Headroom embutido: reconcilia a URL de settings herdada da era
// sidecar (nomes de serviço docker) e, quando o toggle persistido estiver
// ativo, sobe o proxy embutido em loopback sem esperar clique na UI.
// Falhas são logáveis e não impedem o boot do gateway.
import (
	"context"
	"log/slog"
	"net/url"
	"strings"
)

// headroomLegacyHosts são os nomes de serviço docker que o compose antigo
// usava para o sidecar (`http://headroom:8787`). Quantquer outro host externo
// permanece no modo EXTERNAL_PROXY como na origem.
var headroomLegacyHosts = map[string]bool{"headroom": true, "vansrouter": true}

// WriteSettingsPair é a assinatura de gravação de settings usada no boot
// (a mesma injetada em handlers.Overrides).
type WriteSettingsPair func(context.Context, map[string]any) (map[string]any, error)

// BootHeadroom reconcilia a URL legada e faz o autostart. write é a função de
// gravação das settings (pode ser nil — nesse caso não há rewrite persistido).
func BootHeadroom(ctx context.Context, routes *HeadroomRoutes, write WriteSettingsPair) error {
	if routes == nil {
		return nil
	}
	settings := map[string]any{}
	if routes.Read != nil {
		var err error
		settings, err = routes.Read(ctx)
		if err != nil {
			return err
		}
	}
	url, port, codeAware, disableKompress := headroomFlagsFrom(settings)
	if legacy := legacySidecarURL(url); legacy {
		fallback := defaultHeadroomURL()
		if write != nil {
			if merged, err := write(ctx, map[string]any{"headroomUrl": fallback}); err == nil {
				if value, _ := merged["headroomUrl"].(string); value != "" {
					fallback = value
				}
			} else {
				slog.Warn("headroom boot: reconciliação persistida falhou", "erro", err.Error())
			}
		}
		slog.Info("headroom boot: URL herdada do sidecar reconciliada", "de", url, "para", fallback)
		url, port, codeAware, disableKompress = headroomFlagsFrom(map[string]any{
			"headroomUrl": fallback, "headroomCodeAware": settings["headroomCodeAware"], "headroomKompress": settings["headroomKompress"],
		})
	}
	if !tokenSaverTruthy(settings["headroomEnabled"]) {
		return nil
	}
	if !isLoopbackHeadroomURL(url) {
		slog.Info("headroom boot: autostart ignorado (URL externa é gerenciada fora do VansGo)", "url", url)
		return nil
	}
	pid, already, err := routes.H.startHeadroomProxy(port, codeAware, disableKompress)
	if err != nil {
		slog.Warn("headroom boot: autostart falhou", "erro", err.Error())
		return nil
	}
	if already {
		slog.Info("headroom boot: proxy já ativo", "pid", pid, "porta", port)
		return nil
	}
	slog.Info("headroom boot: proxy iniciado", "pid", pid, "porta", port)
	return nil
}

// legacySidecarURL reconhece URLs cujo host é um nome de serviço docker da era
// sidecar (apenas http://<service>:<port> precisa do rewrite; qualquer outro
// host externo é modo EXTERNAL_PROXY da origem).
func legacySidecarURL(value string) bool {
	return headroomLegacyHosts[strings.ToLower(parseHeadroomHost(value))]
}

// parseHeadroomHost devolve o hostname de uma URL http(s) ("" quando inválida).
func parseHeadroomHost(value string) string {
	parsed, err := url.Parse(value)
	if err != nil {
		return ""
	}
	return parsed.Hostname()
}
