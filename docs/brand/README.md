# Atrium brand assets

The logo is a Roman stone arch with one terracotta stone. The stone sits two
places right of the crown and stands a little proud of the ring; the crown
itself is a plain stone.

| File | Use it for |
|------|------------|
| `mark.svg`, `mark-dark.svg` | The arch alone, with its mortar joints and the shaded inside of the opening, at 48 px and larger. The root `README.md` uses these. `-dark` is for dark backgrounds. |
| `logo.svg`, `logo-dark.svg` | The primary logo: the arch followed by "ATRIUM" in drawn capitals. The letters are outlines, not a font, so the lockup renders the same everywhere. They stand on the base line of the piers and are 47 units tall on the 64-unit grid. The docs site header shows this lockup. |
| `logo-secondary.svg`, `logo-secondary-dark.svg` | The secondary logo: the arch stands in for the "A", followed by "TRIUM" in the same capitals. For places where the name is already known or space is tight. Nothing references these files yet. |
| `icon.svg` | The detailed arch on a dark rounded tile, filling more of the tile than the badge does, for favicons and avatars. |
| `app-badge.svg`, `app-badge.png` | The detailed arch on a square dark tile (`#1C1B18`), 512 px, for places that show the image in a circle, such as the badge of the release GitHub App ([setup](../../CONTRIBUTING.md#releases)). The arch is small enough that a circle does not clip it. The PNG is `app-badge.svg` rendered at 512 px, for places that take no SVG. |

The arch is drawn on a 64-unit grid, the height of the lockup. `mark*.svg` is
`logo*.svg` and `logo-secondary*.svg` without their wordmark. Each pier is three equal courses between a
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

A change to the arch has to be made in `logo*.svg`, `logo-secondary*.svg`, `mark*.svg`,
`app-badge.svg`, `icon.svg`, the inlined favicon and the header logo, and
`app-badge.png` rendered again from `app-badge.svg`. A change to a logo colour
also goes into the `--logo-*` tokens.
