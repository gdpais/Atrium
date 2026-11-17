// Builds dist/en.html and dist/pt.html from the shared engine, app and style.
const dir = import.meta.dir;
const read = (f: string) => Bun.file(`${dir}/${f}`).text();
const LOGO = `<svg viewBox="0 0 64 64" role="img" aria-label="Atrium">
      <path class="lg-soffit" d="M17.5 59V33A14.5 14.5 0 0 1 46.5 33V59L44.18 57.08V35.24A12.18 12.18 0 0 0 19.82 35.24V57.08Z"/>
      <path class="lg-stone" d="M56.99 32.5A25 25 0 0 0 55.66 24.92L45.79 28.51A14.5 14.5 0 0 1 46.49 32.5ZM55.32 23.98A25 25 0 0 0 51.47 17.32L43.42 24.07A14.5 14.5 0 0 1 45.45 27.57ZM50.83 16.55A25 25 0 0 0 44.93 11.6L39.68 20.7A14.5 14.5 0 0 1 42.78 23.3ZM44.06 11.1A25 25 0 0 0 36.83 8.47L35.01 18.82A14.5 14.5 0 0 1 38.81 20.2ZM27.17 8.47A25 25 0 0 0 19.94 11.1L25.19 20.2A14.5 14.5 0 0 1 28.99 18.82ZM19.07 11.6A25 25 0 0 0 13.17 16.55L21.22 23.3A14.5 14.5 0 0 1 24.32 20.7ZM12.53 17.32A25 25 0 0 0 8.68 23.98L18.55 27.57A14.5 14.5 0 0 1 20.58 24.07ZM8.34 24.92A25 25 0 0 0 7.01 32.5L17.51 32.5A14.5 14.5 0 0 1 18.21 28.51ZM5.8 33.5H18.7v3H5.8ZM7 37.5H17.5v6.5H7ZM7 45H17.5v6.5H7ZM7 52.5H17.5v6.5H7ZM45.3 33.5H58.2v3H45.3ZM46.5 37.5H57v6.5H46.5ZM46.5 45H57v6.5H46.5ZM46.5 52.5H57v6.5H46.5Z"/>
      <path class="lg-key" d="M36.46 4.85A28.5 28.5 0 0 0 27.54 4.85L30.2 19.92A13.2 13.2 0 0 1 33.8 19.92Z"/>
    </svg>`;
const [css, engine, app] = await Promise.all([read("style.css"), read("engine.js"), read("app.js")]);
for (const lang of ["en", "pt"]) {
  const html = (await read(`${lang}.html`))
    .replace("{{CSS}}", () => css)
    .replace("{{LOGO}}", () => LOGO)
    .replace("{{ENGINE}}", () => engine)
    .replace("{{APP}}", () => app);
  if (/\{\{[A-Z]+\}\}/.test(html)) throw new Error(`${lang}: unreplaced placeholder`);
  await Bun.write(`${dir}/dist/${lang}.html`, html);
  console.log(`dist/${lang}.html`, html.length, "bytes");
}
