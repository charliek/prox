package main

import (
	"os"

	"github.com/charliek/prox/internal/cli"
)

func main() {
	app := cli.NewApp()
	os.Exit(app.Run(os.Args))
}
