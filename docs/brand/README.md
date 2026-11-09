# Atrium brand assets

The logo is a Roman stone arch with a terracotta keystone.

| File | Use it for |
|------|------------|
| `mark.svg`, `mark-dark.svg` | The arch alone, with its inner ring and mortar joints, at 48 px and larger. The root `README.md` uses these. `-dark` is for dark backgrounds. |
| `logo.svg`, `logo-dark.svg` | The arch with the "Atrium" wordmark. The wordmark is Inter SemiBold (SIL Open Font License) converted to outlines, so it renders the same everywhere and needs no font. Nothing references these files yet. |
| `icon.svg` | The arch without inner ring or joints on a dark rounded tile, for small sizes (favicons, avatars). The details turn to noise below about 32 px. |
| `app-badge.svg`, `app-badge.png` | The detailed arch on a square dark tile (`#211E1A`), 512 px, for places that show the image in a circle, such as the badge of the release GitHub App ([setup](../../CONTRIBUTING.md#releases)). The arch is small enough that a circle does not clip it. The PNG is `app-badge.svg` rendered at 512 px, for places that take no SVG. |

The arch is drawn on a 100-unit grid. `mark*.svg` is the arch group of
`logo*.svg` without the `scale(.65)` that fits it to the 64-unit-high lockup.

## Copies to keep in sync

The arch of `icon.svg` is also inlined twice in
`tools/docsite/internal/site/assets/page.html`: as the favicon data URI and as
the header mark (which takes its colours from the theme instead of fixed
fills). A change to the arch has to be made in `logo*.svg`, `mark*.svg`,
`app-badge.svg`, `icon.svg` and both inlined copies, and `app-badge.png`
rendered again from `app-badge.svg`.
