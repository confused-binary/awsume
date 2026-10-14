//go:build windows

package browser

func candidates() [][]string {
	// rundll32 passes the URL through unchanged, unlike "cmd /c start" which
	// interprets & and % characters.
	return [][]string{{"rundll32", "url.dll,FileProtocolHandler"}}
}
