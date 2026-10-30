package installerui

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"fyne.io/fyne/v2"
	fyneapp "fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/mrpoundsign/appinstaller/pkg/installer"
)

// RunMain parses installer/uninstaller CLI flags and launches either the silent runner,
// the Fyne Uninstall wizard, or the Fyne Setup Wizard.
func RunMain(spec installer.AppSpec, appIcon fyne.Resource) int {
	var (
		silentFlag    = flag.Bool("silent", false, "Run installation or uninstallation without GUI")
		uninstallFlag = flag.Bool("uninstall", false, "Uninstall the application")
		purgeFlag     = flag.Bool("purge", false, "Remove user configuration and data during silent uninstall")
		dirFlag       = flag.String("dir", "", "Target installation directory")
		waitPIDFlag   = flag.Int("wait-pid", 0, "Wait for the specified process ID to exit before installing")
		relaunchFlag  = flag.Bool("relaunch", false, "Launch application after installation completes")
	)
	flag.Parse()

	isUninstall := *uninstallFlag
	if exeBase := strings.ToLower(filepath.Base(os.Args[0])); strings.HasPrefix(exeBase, "uninstall") {
		isUninstall = true
	}

	targetDir := strings.TrimSpace(*dirFlag)
	if targetDir == "" {
		if isUninstall {
			if selfExe, err := os.Executable(); err == nil && selfExe != "" {
				targetDir = filepath.Dir(selfExe)
			}
		}
		if targetDir == "" {
			targetDir = installer.DefaultInstallDir(spec.AppName)
		}
	}

	ins := installer.NewInstaller(spec)

	if *silentFlag {
		ctx := context.Background()
		if isUninstall {
			if err := ins.Uninstall(ctx, targetDir, *purgeFlag, nil); err != nil {
				fmt.Fprintf(os.Stderr, "%s uninstall error: %v\n", spec.AppName, err)
				return 1
			}
			return 0
		}

		opts := installer.Options{
			InstallDir:              targetDir,
			CreateStartMenuShortcut: true,
			CreateDesktopShortcut:   false,
			WaitPID:                 *waitPIDFlag,
		}
		_, err := ins.Install(ctx, opts, nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s silent install error: %v\n", spec.AppName, err)
			return 1
		}
		if *relaunchFlag && spec.BinaryName != "" {
			exePath := filepath.Join(targetDir, spec.BinaryName)
			cmd := exec.Command(exePath)
			cmd.Dir = targetDir
			_ = cmd.Start()
		}
		return 0
	}

	fyneApp := fyneapp.NewWithID(spec.AppID + ".installer")
	fyneApp.Settings().SetTheme(NewObsidianTheme())
	if appIcon != nil {
		fyneApp.SetIcon(appIcon)
	}

	if isUninstall {
		showUninstallWindow(fyneApp, ins, targetDir)
	} else {
		showInstallWindow(fyneApp, ins, targetDir, *waitPIDFlag)
	}

	fyneApp.Run()
	return 0
}

func showInstallWindow(a fyne.App, ins *installer.Installer, initialDir string, waitPID int) {
	spec := ins.Spec
	ver := strings.TrimPrefix(spec.Version, "v")
	w := a.NewWindow(fmt.Sprintf("%s v%s — Setup", spec.AppName, ver))
	w.Resize(fyne.NewSize(620, 480))
	w.CenterOnScreen()

	title := widget.NewLabelWithStyle(fmt.Sprintf("Install %s v%s", spec.AppName, ver), fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	desc := widget.NewLabelWithStyle(spec.Description, fyne.TextAlignLeading, fyne.TextStyle{Italic: true})

	dirEntry := widget.NewEntry()
	dirEntry.SetText(initialDir)

	browseBtn := widget.NewButtonWithIcon("Browse...", theme.FolderOpenIcon(), func() {
		d := dialog.NewFolderOpen(func(uri fyne.ListableURI, err error) {
			if err == nil && uri != nil {
				dirEntry.SetText(filepath.Join(uri.Path(), spec.AppName))
			}
		}, w)
		d.Show()
	})

	dirRow := container.NewBorder(nil, nil, nil, browseBtn, dirEntry)
	dirSection := container.NewVBox(
		widget.NewLabelWithStyle("Installation Folder:", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		dirRow,
	)

	startMenuCheck := widget.NewCheck("Create Start Menu shortcut", nil)
	startMenuCheck.SetChecked(true)

	desktopCheck := widget.NewCheck("Create Desktop shortcut", nil)
	desktopCheck.SetChecked(true)

	launchCheck := widget.NewCheck(fmt.Sprintf("Launch %s when installation finishes", spec.AppName), nil)
	launchCheck.SetChecked(true)

	optionsBox := container.NewVBox(
		widget.NewLabelWithStyle("Shortcuts & Options:", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		startMenuCheck,
		desktopCheck,
	)

	selectedComps := make(map[string]bool, len(spec.OptionalComponents))
	var compChecks []*widget.Check
	if len(spec.OptionalComponents) > 0 {
		optionsBox.Add(widget.NewSeparator())
		optionsBox.Add(widget.NewLabelWithStyle("Optional Components & Downloads:", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}))
		for _, comp := range spec.OptionalComponents {
			c := comp
			selectedComps[c.ID] = c.DefaultOn
			label := c.Title
			if c.Description != "" {
				label += " — " + c.Description
			}
			chk := widget.NewCheck(label, func(checked bool) {
				selectedComps[c.ID] = checked
			})
			chk.SetChecked(c.DefaultOn)
			compChecks = append(compChecks, chk)
			optionsBox.Add(chk)
		}
	}

	optionsBox.Add(widget.NewSeparator())
	optionsBox.Add(launchCheck)

	progressBar := widget.NewProgressBar()
	progressBar.Hide()
	statusLabel := widget.NewLabelWithStyle("Ready to install.", fyne.TextAlignLeading, fyne.TextStyle{Italic: true})
	statusLabel.Wrapping = fyne.TextWrapWord

	var installBtn, cancelBtn *widget.Button
	cancelBtn = widget.NewButtonWithIcon("Cancel", theme.CancelIcon(), func() {
		w.Close()
		a.Quit()
	})

	installBtn = widget.NewButtonWithIcon("Install Now", theme.DownloadIcon(), func() {
		targetDir := strings.TrimSpace(dirEntry.Text)
		if targetDir == "" {
			dialog.ShowError(errors.New("please specify a valid installation folder"), w)
			return
		}

		dirEntry.Disable()
		browseBtn.Disable()
		startMenuCheck.Disable()
		desktopCheck.Disable()
		launchCheck.Disable()
		for _, chk := range compChecks {
			chk.Disable()
		}
		installBtn.Disable()
		progressBar.SetValue(0)
		progressBar.Show()

		opts := installer.Options{
			InstallDir:              targetDir,
			CreateStartMenuShortcut: startMenuCheck.Checked,
			CreateDesktopShortcut:   desktopCheck.Checked,
			SelectedComponents:      selectedComps,
			WaitPID:                 waitPID,
		}

		go func() {
			_, err := ins.Install(context.Background(), opts, func(frac float64, status string) {
				fyne.Do(func() {
					progressBar.SetValue(frac)
					statusLabel.SetText(status)
				})
			})
			fyne.Do(func() {
				if err != nil {
					statusLabel.SetText("❌ Installation failed: " + err.Error())
					installBtn.SetText("Retry Install")
					installBtn.Enable()
					dirEntry.Enable()
					browseBtn.Enable()
					dialog.ShowError(err, w)
					return
				}

				progressBar.SetValue(1.0)
				statusLabel.SetText(fmt.Sprintf("✓ %s v%s installed successfully!", spec.AppName, ver))

				if launchCheck.Checked && spec.BinaryName != "" {
					exePath := filepath.Join(targetDir, spec.BinaryName)
					cmd := exec.Command(exePath)
					cmd.Dir = targetDir
					_ = cmd.Start()
				}
				w.Close()
				a.Quit()
			})
		}()
	})
	installBtn.Importance = widget.HighImportance

	footer := container.NewVBox(
		progressBar,
		statusLabel,
		widget.NewSeparator(),
		container.NewHBox(layout.NewSpacer(), cancelBtn, installBtn),
	)

	body := container.NewVScroll(container.NewVBox(
		title,
		desc,
		widget.NewSeparator(),
		dirSection,
		widget.NewSeparator(),
		optionsBox,
	))

	w.SetContent(container.NewPadded(container.NewBorder(nil, footer, nil, nil, body)))
	w.Show()
}

func showUninstallWindow(a fyne.App, ins *installer.Installer, installDir string) {
	spec := ins.Spec
	w := a.NewWindow("Uninstall " + spec.AppName)
	w.Resize(fyne.NewSize(500, 280))
	w.CenterOnScreen()

	title := widget.NewLabelWithStyle("Uninstall "+spec.AppName, fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	msg := widget.NewLabel(fmt.Sprintf("Are you sure you want to remove %s from:\n%s", spec.AppName, installDir))
	msg.Wrapping = fyne.TextWrapWord

	purgeCheck := widget.NewCheck("Also remove my configuration, profiles, logs, and downloaded models/cache", nil)
	purgeCheck.SetChecked(false)

	progressBar := widget.NewProgressBar()
	progressBar.Hide()
	statusLabel := widget.NewLabel("")

	cancelBtn := widget.NewButtonWithIcon("Cancel", theme.CancelIcon(), func() {
		w.Close()
		a.Quit()
	})

	var uninstallBtn *widget.Button
	uninstallBtn = widget.NewButtonWithIcon("Uninstall", theme.DeleteIcon(), func() {
		uninstallBtn.Disable()
		purgeCheck.Disable()
		progressBar.Show()

		go func() {
			err := ins.Uninstall(context.Background(), installDir, purgeCheck.Checked, func(frac float64, status string) {
				fyne.Do(func() {
					progressBar.SetValue(frac)
					statusLabel.SetText(status)
				})
			})
			fyne.Do(func() {
				if err != nil {
					dialog.ShowError(err, w)
					uninstallBtn.Enable()
					return
				}
				w.Close()
				a.Quit()
			})
		}()
	})
	uninstallBtn.Importance = widget.DangerImportance

	content := container.NewVBox(
		title,
		widget.NewSeparator(),
		msg,
		purgeCheck,
		layout.NewSpacer(),
		progressBar,
		statusLabel,
		widget.NewSeparator(),
		container.NewHBox(layout.NewSpacer(), cancelBtn, uninstallBtn),
	)

	w.SetContent(container.NewPadded(content))
	w.Show()
}
