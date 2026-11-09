# Atrium brand assets

The logo is a Roman stone arch with a terracotta keystone.

| File | Use it for |
|------|------------|
| `mark.svg`, `mark-dark.svg` | The arch alone, with mortar joints, at 48 px and larger. The root `README.md` uses these. `-dark` is for dark backgrounds. |
| `logo.svg`, `logo-dark.svg` | The arch with the "Atrium" wordmark. The wordmark is Inter SemiBold (SIL Open Font License) converted to outlines, so it renders the same everywhere and needs no font. In the 64-unit grid it is set at 36 units with -0.8 tracking, on the base line of the piers. Nothing references these files yet. |
| `icon.svg` | The arch without joints on a dark rounded tile, for small sizes (favicons, avatars). The joints turn to noise below about 32 px. |
| `app-badge.png` | The same arch on a square dark tile, 512 px, for places that take no SVG and show the image in a circle, such as the badge of the release GitHub App ([setup](../../CONTRIBUTING.md#releases)). The arch is smaller than in `icon.svg` so a circle does not clip it. |

## Copies to keep in sync

The joint-free arch of `icon.svg` is also inlined twice in
`tools/docsite/internal/site/assets/page.html`: as the favicon data URI and as
the header mark (which takes its colours from the theme instead of fixed
fills). A change to the arch has to be made in all three places, and
`app-badge.png` exported again from `icon.svg` (square corners, arch at 66%).
