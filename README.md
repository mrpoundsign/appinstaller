# appinstaller

Unified pure-Go & Fyne v2 installer, uninstaller, portable self-extracting launcher, and GitHub Release SHA-256 self-updater library for desktop and creator applications (`TownCrier`, `SVGTuber`, `InkAnim`).

## Packages

- `pkg/updater` (`CGO_ENABLED=0`): GitHub Releases version checking, semantic version comparison, `checksums.txt` / `SHA256SUMS.txt` verification, and silent/portable self-update execution.
- `pkg/installer` (`CGO_ENABLED=0`): Declarative `AppSpec` installer & uninstaller engine with `install-manifest.json`, Windows Registry (`HKCU\Software\Microsoft\Windows\CurrentVersion\Uninstall\<AppName>`), `.lnk` shortcut creation, user `PATH` management, and optional post-install component hooks.
- `pkg/launcher` (`CGO_ENABLED=0`): Single-file portable payload extractor with SHA-256 stream verification and automatic pruning of older version directories.
- `pkg/installerui` (Fyne v2): Obsidian Slate (`#0F172A`), Chrome White (`#F8FAFC`), and Electric Sky Blue (`#38BDF8`) GUI Setup Wizard and Uninstaller.
- `pkg/updaterui` (Fyne v2): Non-intrusive Markdown Release Notes & one-click Update dialog.
