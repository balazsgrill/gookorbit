package koreader

import "embed"

//go:embed plugin/*.lua
var pluginFS embed.FS

func mustRead(name string) []byte {
	b, err := pluginFS.ReadFile("plugin/" + name)
	if err != nil {
		panic(err)
	}
	return b
}
