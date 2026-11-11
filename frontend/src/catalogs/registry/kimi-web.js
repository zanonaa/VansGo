export default {
  id: "kimi-web",
  priority: 150,
  alias: "kimi-web",
  aliases: [
    "kimi-web",
  ],
  uiAlias: "kimi-web",
  display: {
    name: "Kimi Web (Subscription)",
    icon: "looks_one",
    color: "#161A1D",
    textIcon: "KW",
    website: "https://www.kimi.ai",
  },
  category: "webCookie",
  authType: "apikey",
  authHint: "Paste the access_token value from www.kimi.ai localStorage (refresh_token goes in the connection's refreshToken field)",
  transport: {
    baseUrl: "https://www.kimi.ai/apiv2/kimi.gateway.chat.v1.ChatService/Chat",
    format: "kimi-web",
    authType: "apikey",
  },
  models: [
    { id: "k3", name: "Kimi K3" },
    { id: "k2d6", name: "Kimi K2.6" },
  ],
  passthroughModels: true,
};
