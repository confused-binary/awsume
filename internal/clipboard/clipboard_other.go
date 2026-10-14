//go:build !windows && !darwin

package clipboard

func candidates() [][]string {
	return [][]string{
		{"wl-copy"},
		{"xclip", "-selection", "clipboard"},
		{"xsel", "--clipboard", "--input"},
		{"clip.exe"},
		{"termux-clipboard-set"},
	}
}
