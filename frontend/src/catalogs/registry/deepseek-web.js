export default {
  id: "deepseek-web",
  priority: 150,
  alias: "deepseek-web",
  aliases: [
    "ds-web",
  ],
  uiAlias: "ds-web",
  display: {
    name: "DeepSeek Web (Subscription)",
    icon: "whale",
    color: "#4D6BFE",
    textIcon: "DS",
    website: "https://chat.deepseek.com",
  },
  category: "webCookie",
  authType: "cookie",
  authHint: "Paste your session token (64-char Bearer, Authorization header value) from chat.deepseek.com",
  transport: {
    baseUrl: "https://chat.deepseek.com/api/v0/chat/completion",
    format: "deepseek-web",
    authType: "cookie",
  },
  models: [
    { id: "default", name: "DeepSeek Chat" },
  ],
  passthroughModels: true,
};
