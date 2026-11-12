export default {
  id: "zai-web",
  priority: 150,
  alias: "zai-web",
  aliases: [
    "zai-web",
  ],
  uiAlias: "zai-web",
  display: {
    name: "Z.ai Web (Guest)",
    icon: "chat",
    color: "#4B6BFB",
    textIcon: "ZW",
    website: "https://chat.z.ai",
  },
  category: "webCookie",
  authType: "cookie",
  authHint: "Paste the guest token from chat.z.ai (the token cookie or Authorization Bearer value)",
  transport: {
    baseUrl: "https://chat.z.ai/api/v2/chat/completions",
    format: "zai-web",
    authType: "cookie",
  },
  models: [
    { id: "x-preview-l", name: "GLM-5.3-Flash (x-preview-l)" },
    { id: "x-preview", name: "GLM-5 (x-preview)" },
  ],
  passthroughModels: true,
};
