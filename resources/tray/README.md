# Tray icons

Placeholder brand-mark icons for the system tray, embedded via `//go:embed` in
`tray.go`. Swap the art but keep the filenames, formats, and sizes:

- `icon.ico` — Windows tray. PNG-in-ICO with 16×16 and 32×32 frames.
- `icon.png` — Linux tray and macOS regular icon. 32×32 RGBA.
- `icon_template.png` — macOS menu-bar template. Monochrome **black shape on a
  transparent background** (alpha is the mask); macOS recolors it for light/dark
  menu bars. Used via `systray.SetTemplateIcon`.
