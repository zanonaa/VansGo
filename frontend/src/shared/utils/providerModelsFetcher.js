// Fetch suggested models for providers that expose a public models API.
// Fetches via backend proxy to avoid CORS issues. Sem cache: é UI de
// sugestão/import com custo baixo, e o cache escondia lineup novo do
// upstream por 10 min (o clique de import não gerava request no F12).

/**
 * Fetch suggested models for a provider using its modelsFetcher config.
 * @param {{ url: string, type: string }} fetcher
 * @returns {Promise<Array<{ id: string, name: string, contextLength?: number }>>}
 */
export async function fetchSuggestedModels(fetcher) {
  if (!fetcher?.url || !fetcher?.type) return [];
  try {
    const params = new URLSearchParams({ url: fetcher.url, type: fetcher.type });
    const res = await fetch(`/api/providers/suggested-models?${params}`);
    if (!res.ok) return [];
    const json = await res.json();
    return json.data ?? [];
  } catch {
    return [];
  }
}
