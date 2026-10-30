package updaterui

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/mrpoundsign/appinstaller/pkg/updater"
)

// DialogConfig configures the Fyne Update Available dialog.
type DialogConfig struct {
	Parent         fyne.Window
	AppName        string
	CurrentVersion string
	Release        *updater.ReleaseInfo
	Checker        *updater.Checker
	OnSkipVersion  func(version string)
	OnQuitApp      func()
}

// ShowUpdateDialog displays a modal Fyne dialog showing the new release version, Markdown release notes,
// and one-click SHA-256 verified self-update with a determinate progress bar.
func ShowUpdateDialog(cfg DialogConfig) {
	if cfg.Parent == nil || cfg.Release == nil {
		return
	}

	curVer := updater.NormalizeVersion(cfg.CurrentVersion)
	newVer := updater.NormalizeVersion(cfg.Release.TagName)

	titleLabel := widget.NewLabelWithStyle(
		fmt.Sprintf("%s v%s is available! (Current: v%s)", cfg.AppName, newVer, curVer),
		fyne.TextAlignLeading,
		fyne.TextStyle{Bold: true},
	)

	ghLink := layout.NewSpacer()
	if cfg.Release.HTMLURL != "" {
		if u, err := url.Parse(cfg.Release.HTMLURL); err == nil {
			ghLink = widget.NewHyperlink("View Release on GitHub", u)
		}
	}

	notesText := cfg.Release.Body
	if notesText == "" {
		notesText = "_No release notes provided._"
	}
	notesRich := widget.NewRichTextFromMarkdown(notesText)
	notesRich.Wrapping = fyne.TextWrapWord
	notesScroll := container.NewScroll(notesRich)
	notesScroll.SetMinSize(fyne.NewSize(560, 240))

	progressBar := widget.NewProgressBar()
	progressBar.Hide()
	statusLabel := widget.NewLabelWithStyle("Verified against release SHA-256 checksums before applying.", fyne.TextAlignLeading, fyne.TextStyle{Italic: true})
	statusLabel.Wrapping = fyne.TextWrapWord

	var d dialog.Dialog
	var updateBtn, skipBtn, laterBtn *widget.Button

	laterBtn = widget.NewButton("Remind Me Later", func() {
		if d != nil {
			d.Hide()
		}
	})

	skipBtn = widget.NewButton("Skip This Version", func() {
		if cfg.OnSkipVersion != nil {
			cfg.OnSkipVersion(newVer)
		}
		if d != nil {
			d.Hide()
		}
	})

	updateBtn = widget.NewButtonWithIcon("Update & Restart Now", theme.DownloadIcon(), func() {
		exePath := updater.ResolveTargetExecutable("")
		mode := updater.DetectInstallMode(exePath)
		asset, err := updater.SelectAsset(cfg.Release, mode, runtime.GOOS, runtime.GOARCH)
		if err != nil {
			statusLabel.SetText("❌ Could not find a matching download for " + runtime.GOOS + "/" + runtime.GOARCH + ": " + err.Error())
			return
		}

		updateBtn.Disable()
		skipBtn.Disable()
		laterBtn.Disable()
		progressBar.SetValue(0)
		progressBar.Show()
		statusLabel.SetText("Downloading and verifying " + asset.Name + "...")

		go func() {
			destDir := filepath.Join(os.TempDir(), cfg.AppName+"-update-"+newVer)
			verifiedPath, dlErr := cfg.Checker.DownloadAndVerify(
				context.Background(),
				cfg.Release,
				asset,
				destDir,
				func(cur, total int64) {
					ratio := 0.0
					if total > 0 {
						ratio = min(float64(cur)/float64(total), 1.0)
					}
					curMB := float64(cur) / (1024 * 1024)
					totMB := float64(total) / (1024 * 1024)
					fyne.Do(func() {
						progressBar.SetValue(ratio)
						statusLabel.SetText(fmt.Sprintf("Downloading %s... %.1f / %.1f MB", asset.Name, curMB, totMB))
					})
				},
			)
			if dlErr != nil {
				fyne.Do(func() {
					statusLabel.SetText("❌ Update verification failed: " + dlErr.Error())
					updateBtn.Enable()
					laterBtn.Enable()
				})
				return
			}

			fyne.Do(func() {
				progressBar.SetValue(1.0)
				statusLabel.SetText("✓ SHA-256 checksum verified! Applying update and restarting...")
			})

			applyErr := updater.ApplyAndRestart(verifiedPath, exePath, mode, func() {
				fyne.Do(func() {
					if d != nil {
						d.Hide()
					}
					if cfg.OnQuitApp != nil {
						cfg.OnQuitApp()
					}
				})
			})
			if applyErr != nil {
				fyne.Do(func() {
					statusLabel.SetText("❌ Failed to apply update: " + applyErr.Error())
					updateBtn.Enable()
					laterBtn.Enable()
				})
			}
		}()
	})
	updateBtn.Importance = widget.HighImportance

	header := container.NewVBox(
		container.NewHBox(titleLabel, layout.NewSpacer(), ghLink),
		widget.NewSeparator(),
		widget.NewLabelWithStyle("What's New:", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
	)

	footer := container.NewVBox(
		progressBar,
		statusLabel,
		widget.NewSeparator(),
		container.NewHBox(skipBtn, layout.NewSpacer(), laterBtn, updateBtn),
	)

	content := container.NewBorder(header, footer, nil, nil, notesScroll)
	d = dialog.NewCustomWithoutButtons("Update "+cfg.AppName, content, cfg.Parent)
	d.Resize(fyne.NewSize(620, 440))
	d.Show()
}
