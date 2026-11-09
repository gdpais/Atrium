# Atrium brand assets

The logo is a Roman stone arch with a terracotta keystone.

| File | Use it for |
|------|------------|
| `mark.svg`, `mark-dark.svg` | The arch alone, with its inner ring and mortar joints, at 48 px and larger. The root `README.md` uses these. `-dark` is for dark backgrounds. |
| `logo.svg`, `logo-dark.svg` | The arch with the "Atrium" wordmark. The wordmark is Inter SemiBold (SIL Open Font License) converted to outlines, so it renders the same everywhere and needs no font. Nothing references these files yet. |
| `icon.svg` | The detailed arch on a dark rounded tile, filling more of the tile than the badge does, for favicons and avatars. |
| `app-badge.svg`, `app-badge.png` | The detailed arch on a square dark tile (`#211E1A`), 512 px, for places that show the image in a circle, such as the badge of the release GitHub App ([setup](../../CONTRIBUTING.md#releases)). The arch is small enough that a circle does not clip it. The PNG is `app-badge.svg` rendered at 512 px, for places that take no SVG. |

The arch is drawn on a 100-unit grid. `mark*.svg` is the arch group of
`logo*.svg` without the `scale(.65)` that fits it to the 64-unit-high lockup.

## Copies to keep in sync

`icon.svg` is also inlined in
`tools/docsite/internal/site/assets/page.html` as the favicon data URI. The
header mark in the same file is a simpler arch, without inner ring or joints,
that takes its colours from the theme instead of fixed fills. A change to the
arch has to be made in `logo*.svg`, `mark*.svg`, `app-badge.svg`, `icon.svg`,
the inlined favicon and the header mark, and `app-badge.png` rendered again
from `app-badge.svg`.
