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
  models: [],
  modelsFetcher: { url: "https://opencode.ai/zen/v1/models", type: "opencode-free", public: true },
  passthroughModels: true,
};
