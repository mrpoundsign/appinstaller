package installerui

import (
	"image/color"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/theme"
)

// ObsidianTheme implements the shared Obsidian Slate (#0F172A), Chrome White (#F8FAFC),
// and Electric Sky Blue (#38BDF8) studio palette across TownCrier, SVGTuber, and InkAnim.
type ObsidianTheme struct{}

var _ fyne.Theme = (*ObsidianTheme)(nil)

func NewObsidianTheme() fyne.Theme {
	return &ObsidianTheme{}
}

func (t *ObsidianTheme) Color(name fyne.ThemeColorName, _ fyne.ThemeVariant) color.Color {
	switch name {
	case theme.ColorNameBackground:
		return color.NRGBA{R: 15, G: 23, B: 42, A: 255} // #0F172A Obsidian Slate
	case theme.ColorNameButton, theme.ColorNameInputBackground, theme.ColorNameHeaderBackground, theme.ColorNameMenuBackground, theme.ColorNameOverlayBackground:
		return color.NRGBA{R: 30, G: 41, B: 59, A: 255} // #1E293B Slate Surface
	case theme.ColorNameForeground:
		return color.NRGBA{R: 248, G: 250, B: 252, A: 255} // #F8FAFC Chrome White
	case theme.ColorNamePrimary, theme.ColorNameFocus, theme.ColorNameSelection:
		return color.NRGBA{R: 56, G: 189, B: 248, A: 255} // #38BDF8 Electric Sky Blue
	case theme.ColorNamePlaceHolder, theme.ColorNameDisabled:
		return color.NRGBA{R: 148, G: 163, B: 184, A: 255} // #94A3B8 Muted Slate
	case theme.ColorNameError:
		return color.NRGBA{R: 239, G: 68, B: 68, A: 255}
	case theme.ColorNameSuccess:
		return color.NRGBA{R: 34, G: 197, B: 94, A: 255}
	default:
		return theme.DefaultTheme().Color(name, theme.VariantDark)
	}
}

func (t *ObsidianTheme) Font(style fyne.TextStyle) fyne.Resource {
	return theme.DefaultTheme().Font(style)
}

func (t *ObsidianTheme) Icon(name fyne.ThemeIconName) fyne.Resource {
	return theme.DefaultTheme().Icon(name)
}

func (t *ObsidianTheme) Size(name fyne.ThemeSizeName) float32 {
	return theme.DefaultTheme().Size(name)
}
