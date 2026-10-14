//go:build darwin

package browser

func candidates() [][]string {
	return [][]string{{"open"}}
}
