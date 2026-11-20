export async function formatJavaScript(source) {
  if (source.length > 32 * 1024) return source;
  try {
    // Prettier's Babel parser accepts the top-level await and return in
    // Codemode's async function bodies. Load the browser modules only when
    // someone opens a code payload.
    const [{ format }, babel, estree] = await Promise.all([
      import("./prettier/standalone.mjs"),
      import("./prettier/babel.mjs"),
      import("./prettier/estree.mjs"),
    ]);
    return (await format(source, {
      parser: "babel",
      plugins: [babel, estree],
      embeddedLanguageFormatting: "off",
    })).trimEnd();
  } catch {
    return source;
  }
}
