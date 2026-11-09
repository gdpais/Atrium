# Atrium brand assets

The logo is a Roman stone arch with a terracotta keystone.

| File | Use it for |
|------|------------|
| `mark.svg`, `mark-dark.svg` | The arch alone, with mortar joints, at 48 px and larger. The root `README.md` uses these. `-dark` is for dark backgrounds. |
| `logo.svg`, `logo-dark.svg` | The arch with the "Atrium" wordmark. The wordmark is live text (Inter, falling back to Helvetica or Arial), so it renders differently per machine; nothing references these files yet. |
| `icon.svg` | The arch without joints on a dark rounded tile, for small sizes (favicons, avatars). The joints turn to noise below about 32 px. |

## Copies to keep in sync

The joint-free arch of `icon.svg` is also inlined twice in
`tools/docsite/internal/site/assets/page.html`: as the favicon data URI and as
the header mark (which takes its colours from the theme instead of fixed
fills). A change to the arch has to be made in all three places.
