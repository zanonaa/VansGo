// A página endpoint calculava machineId no SSR no app legado; o client não
// consome a prop. A rota SPA renderiza o client diretamente, eliminando a
// leitura de banco no navegador.
import EndpointPageClient from "@/app/(dashboard)/dashboard/endpoint/EndpointPageClient";

export default function EndpointRoute() {
  return <EndpointPageClient />;
}
