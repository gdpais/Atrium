# Atrium brand assets

The logo is a Roman stone arch with a terracotta keystone.

| File | Use it for |
|------|------------|
| `mark.svg`, `mark-dark.svg` | The arch alone, with its mortar joints and the shaded inside of the opening, at 48 px and larger. The root `README.md` uses these. `-dark` is for dark backgrounds. |
| `logo.svg`, `logo-dark.svg` | The arch with the "Atrium" wordmark. The wordmark is Helvetica Neue Bold converted to outlines, so it renders the same everywhere and needs no font. On the 64-unit grid it is set at 36 units with -0.8 tracking, on the base line of the piers. The docs site header shows this lockup. |
| `icon.svg` | The detailed arch on a dark rounded tile, filling more of the tile than the badge does, for favicons and avatars. |
| `app-badge.svg`, `app-badge.png` | The detailed arch on a square dark tile (`#1C1B18`), 512 px, for places that show the image in a circle, such as the badge of the release GitHub App ([setup](../../CONTRIBUTING.md#releases)). The arch is small enough that a circle does not clip it. The PNG is `app-badge.svg` rendered at 512 px, for places that take no SVG. |

The arch is drawn on a 64-unit grid, the height of the lockup. `mark*.svg` is
`logo*.svg` without the wordmark. Each pier is three equal courses between a
slim cap at the spring line and one at the base; the caps project outward
only, by less than a joint's width.

## Copies to keep in sync

`tools/docsite/internal/site/assets/page.html` holds two inlined copies:

- The favicon data URI is `icon.svg`.
- The header logo is the paths of `logo.svg`, cropped to the artwork. Its
  fills are classes (`lg-stone`, `lg-key`, ...) instead of fixed colours, and
  `style.css` in the same folder sets them from the `--logo-*` tokens: the
  colours of `logo.svg` in the light theme and of `logo-dark.svg` in the dark
  one.

A change to the arch has to be made in `logo*.svg`, `mark*.svg`,
`app-badge.svg`, `icon.svg`, the inlined favicon and the header logo, and
`app-badge.png` rendered again from `app-badge.svg`. A change to a logo colour
also goes into the `--logo-*` tokens.
