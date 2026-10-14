//go:build darwin

package clipboard

func candidates() [][]string {
	return [][]string{{"pbcopy"}}
}
