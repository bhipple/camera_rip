# Camera Photo Import & Selector

A web-based application for importing photos from your camera's SD card, reviewing them, selecting the best shots, and exporting both JPEGs and raw files (CR3, ORF). Built with a Go backend and React frontend, distributed as a single self-contained binary.

## Features

- **Import from SD Card**: Detects and imports JPEGs from Canon and Olympus/OM System cameras
- **Photo Review**: Navigate through imported photos with keyboard shortcuts
- **Smart Selection**: Mark photos for export with visual feedback
- **Pin & Compare**: Pin one photo to compare side-by-side with others
- **Batch Export**: Copy selected JPEGs to an export folder
- **Raw File Export**: Copy corresponding raw files (CR3, ORF) from SD card for selected photos

## Screenshot

![Camera Rip Screenshot](doc/screenshot.png)

## Build & Run

Requires [Nix](https://nixos.org/).

```bash
nix-build
./result/bin/backend
```

The app will be available at `http://localhost:5001`.

## Workflow

1. **Import**: Connect your SD card, set an optional "Since" date, click **Import**. Photos land in `~/Pictures/photos/[timestamp]/`.
2. **Review**: Navigate with arrow keys or `j`/`k`. Press `s` to select, `x` to unselect. Press `h` to pin a photo for side-by-side comparison.
3. **Save**: Click **Save selected photos** — selected JPEGs are copied to `selected/`.
4. **Export raw**: Click **Export Raw Files** to copy CR3/ORF files from the SD card to `selected/raw/`.

## Keyboard Shortcuts

| Key | Action |
|-----|--------|
| `←` / `j` | Previous photo |
| `→` / `k` | Next photo |
| `s` | Select current photo |
| `x` | Unselect current photo |
| `h` | Pin/unpin for comparison |
| `Esc` | Clear pinned photo |

## Camera Compatibility

| Brand   | DCIM folders | RAW extension |
|---------|-------------|---------------|
| Canon   | `100CANON`, `101CANON`, … | `.CR3` |
| Olympus | `100OLYMP`, `100OMSYS`, … | `.ORF` |

To add support for another brand, append a row to the `supportedBrands` table in `backend-go/main.go`.

## Directory Structure

```
~/Pictures/photos/
└── 2025-11-01_14-30-45/
    ├── 100_IMG_0001.JPG      # prefixed with DCIM folder number
    ├── 100_IMG_0002.JPG
    └── selected/
        ├── 100_IMG_0001.JPG
        └── raw/
            └── 100_IMG_0001.CR3
```
