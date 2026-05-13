package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/walnuts1018/go-sumtype-accessor/internal/generator"
)

func main() {
	var cfg generator.Config
	flag.StringVar(&cfg.Output, "output", generator.DefaultOutput, "generated file name")
	flag.StringVar(&cfg.Dir, "dir", ".", "target package directory")
	flag.Parse()

	if err := generator.Generate(cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
