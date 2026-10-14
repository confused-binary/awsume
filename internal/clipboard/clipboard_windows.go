//go:build windows

package clipboard

func candidates() [][]string {
	return [][]string{
		{"clip"},
		{"powershell", "-NoProfile", "-NonInteractive", "-Command", "Set-Clipboard -Value ([Console]::In.ReadToEnd())"},
	}
}
