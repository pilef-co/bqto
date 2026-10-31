package main

import (
	"os"

	"github.com/pilef-co/bqto/app"
)

func main() {
	os.Exit(app.Main(os.Args[1:]))
}
