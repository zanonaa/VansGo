// Compat de imagem: <img> nativo com as props legadas do app.
// Não há otimização/redimensionamento automático.
export default function Image({ src, alt = "", width, height, fill, priority, unoptimized, placeholder, style, ...rest }) {
  const resolvedStyle = fill
    ? { position: "absolute", inset: 0, width: "100%", height: "100%", ...style }
    : { width, height, ...style };
  return (
    <img
      src={typeof src === "object" && src?.src ? src.src : src}
      alt={alt}
      decoding={priority ? "sync" : "async"}
      loading={priority ? "eager" : "lazy"}
      style={resolvedStyle}
      {...rest}
    />
  );
}
