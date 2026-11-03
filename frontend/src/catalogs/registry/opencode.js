export default {
  id: "opencode",
  priority: 40,
  hasFree: true,
  alias: "oc",
  uiAlias: "oc",
  display: {
    name: "OpenCode Free",
    icon: "terminal",
    color: "#E87040",
    textIcon: "OC",
  },
  category: "free",
  noAuth: true,
  transport: {
    baseUrl: "https://opencode.ai",
    headers: {
      "x-opencode-client": "desktop",
    },
    noAuth: true,
    // The free tier always answers in SSE; let chatCore aggregate it.
    forceStream: true,
  },
  models: [
    // Lineup de 11/2026: os seeds antigos (space-bunny-free, muse-spark-1.2-
    // contributor-free) saíram do lineup — muse-spark free nem /responses
    // existe mais, e space-bunny responde "Model is unavailable". Novos seeds
    // (big-pickle não tem sufixo -free) + passthrough cobre o resto do lineup.
    { id: "big-pickle", name: "Big Pickle" },
    { id: "glm-5.3-flash-free", name: "GLM 5.3 Flash Free" },
    { id: "deepseek-v4.1-flash-free", name: "DeepSeek 4.1 Flash Free" },
  ],
  modelsFetcher: { url: "https://opencode.ai/zen/v1/models", type: "opencode-free", public: true },
  passthroughModels: true,
};
