# ROI calculator

A single-page calculator for executives: what Atrium saves a client in
developer time, set against what Atrium costs. Not part of the product; it is
a sales tool, published as two private claude.ai pages:

- English: https://claude.ai/artifact/HQhey9FkH3D7RcFz8QYQ2M
- Portuguese: https://claude.ai/artifact/97Bu7NzzUNogbn84RsEPQj

## Files

| File | What it holds |
| --- | --- |
| `engine.js` | The model: `roiCompute(s, f)` and `roiBreakEven(s, f)`. No DOM. |
| `engine.test.js` | Hand-worked checks of the model. |
| `app.js` | Inputs, rendering, chart, saved state and the copy button. |
| `style.css` | Atrium design tokens and layout. |
| `en.html`, `pt.html` | Page markup and the locale object `L` for each language. |
| `build.ts` | Inlines CSS, logo, engine and app into `dist/en.html` and `dist/pt.html`. |

```bash
bun test tools/roi-calculator
```

```bash
bun tools/roi-calculator/build.ts
```

To update a published page, republish the matching `dist/*.html` to its URL.

## The model

Both modes use the same model; they differ only in the hourly cost.

- Simplified: gross salary ÷ productive hours.
- Advanced: gross salary × (1 + employer social security, 23.75%) plus
  benefits, ÷ productive hours.

Yearly savings add up three things:

- **Diagnosis time**: minutes saved per step of a reactive diagnosis, times
  diagnoses per developer per month.
- **Regressions caught earlier**: `releases × share with a regression ×
  (caught with Atrium − caught early today) × f`, each saving
  `late hours − early hours`. The diagnosis hours of the ones that would
  have become incidents are subtracted, so nothing is counted twice.
- **Business impact (optional)**: `min(incidents, diagnoses per year) ×
  hours per diagnosis × impact per hour`. It defaults to 0 because only the
  client's own figures make it credible.

Against Atrium's yearly cost (default €69,000) it shows net value, ROI,
cost covered, payback months and the team size where it breaks even. The
conservative column halves the minutes saved and the extra regressions
caught (`f = 0.5`). Percentages are clamped to 100.

With the defaults, developer time alone covers about 14–19% of a €69k cost;
the case rests on business impact from the client's own incident data.
