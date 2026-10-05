//go:build ignore
package main

import (
	"fmt"
	"os"

	"gookorbit/internal/parser"
)

func main() {
	f, _ := os.Open("/tmp/opencode/test-book.epub")
	defer f.Close()
	fi, _ := f.Stat()
	res, err := parser.Parse(f, fi.Size())
	if err != nil {
		panic(err)
	}
	fmt.Printf("title=%q creators=%q lang=%q desc=%q pub=%q date=%v coverLen=%d\n",
		res.Meta.Title, res.Meta.Creators, res.Meta.Language, res.Meta.Description, res.Meta.Publisher, res.Meta.PublishedDate, len(res.Cover))
	fmt.Printf("coverFirstBytes=% x\n", res.Cover[:min(8, len(res.Cover))])
}
